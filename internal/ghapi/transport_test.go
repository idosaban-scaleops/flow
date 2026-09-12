package ghapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func response(status int, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       http.NoBody,
	}
}

func TestRetryAfterIsHonored(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	tests := []struct {
		name   string
		header http.Header
		want   time.Duration
		wantOK bool
	}{
		{
			name:   "delta seconds",
			header: http.Header{"Retry-After": {"42"}},
			want:   42 * time.Second,
			wantOK: true,
		},
		{
			name:   "http date",
			header: http.Header{"Retry-After": {now.Add(30 * time.Second).UTC().Format(http.TimeFormat)}},
			want:   30 * time.Second,
			wantOK: true,
		},
		{
			name:   "a date already past is not a negative wait",
			header: http.Header{"Retry-After": {now.Add(-time.Hour).UTC().Format(http.TimeFormat)}},
			want:   0,
			wantOK: true,
		},
		{
			name: "rate limit reset once the quota is spent",
			header: http.Header{
				"X-Ratelimit-Remaining": {"0"},
				"X-Ratelimit-Reset":     {"1700000015"},
			},
			want:   15 * time.Second,
			wantOK: true,
		},
		{
			name: "reset is ignored while quota remains",
			header: http.Header{
				"X-Ratelimit-Remaining": {"57"},
				"X-Ratelimit-Reset":     {"1700000015"},
			},
			wantOK: false,
		},
		{
			name:   "nothing to go on",
			header: http.Header{},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := retryAfter(tt.header, now)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("delay = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRetryWaitsAsLongAsTheServerAsks is the point of the change: a 429 with
// Retry-After must not be retried after the 500ms exponential backoff, which is
// what earns a longer block.
func TestRetryWaitsAsLongAsTheServerAsks(t *testing.T) {
	var attempts int
	var slept []time.Duration

	rt := &RetryTransport{
		Base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			attempts++
			if attempts == 1 {
				return response(http.StatusTooManyRequests, http.Header{"Retry-After": {"7"}}), nil
			}
			return response(http.StatusOK, nil), nil
		}),
		Sleep: func(d time.Duration) { slept = append(slept, d) },
	}

	resp, err := rt.RoundTrip(httptest.NewRequest(http.MethodGet, "https://api.github.com/x", http.NoBody))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if len(slept) != 1 || slept[0] != 7*time.Second {
		t.Errorf("slept = %v, want one 7s wait", slept)
	}
}

// TestRetryGivesUpRatherThanBlockingForMinutes keeps a CLI responsive when the
// server asks for a wait no interactive command should sit through.
func TestRetryGivesUpRatherThanBlockingForMinutes(t *testing.T) {
	var attempts int
	rt := &RetryTransport{
		Base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			attempts++
			return response(http.StatusTooManyRequests, http.Header{"Retry-After": {"3600"}}), nil
		}),
		Sleep: func(time.Duration) { t.Error("should not have slept") },
	}

	resp, err := rt.RoundTrip(httptest.NewRequest(http.MethodGet, "https://api.github.com/x", http.NoBody))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want the 429 handed back", resp.StatusCode)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
}

// TestRetryStopsOnCancellation covers the 45-minute poll: Ctrl-C during a
// backoff must abort rather than run the wait out.
func TestRetryStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	rt := &RetryTransport{
		Base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			cancel() // the user hits Ctrl-C while the request is in flight
			return response(http.StatusInternalServerError, nil), nil
		}),
	}

	req := httptest.NewRequest(http.MethodGet, "https://api.github.com/x", http.NoBody).WithContext(ctx)
	start := time.Now()
	resp, err := rt.RoundTrip(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("want a context error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v; the backoff was not cancellable", elapsed)
	}
}

// TestRetryOnlyReplaysIdempotentRequests stops a future POST being submitted
// twice because its response happened to be a 502.
func TestRetryOnlyReplaysIdempotentRequests(t *testing.T) {
	var attempts int
	rt := &RetryTransport{
		Base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			attempts++
			return response(http.StatusBadGateway, nil), nil
		}),
		Sleep: func(time.Duration) {},
	}

	req := httptest.NewRequest(http.MethodPost, "https://api.github.com/x", strings.NewReader("{}"))
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if attempts != 1 {
		t.Errorf("a POST was retried %d times; only idempotent methods may replay", attempts)
	}
}

// TestRetryDoesNotMutateTheCallersRequest is the RoundTripper contract.
func TestRetryDoesNotMutateTheCallersRequest(t *testing.T) {
	var attempts int
	rt := &RetryTransport{
		Base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			attempts++
			r.Header.Set("X-Attempt-Scribble", "mutated")
			if attempts == 1 {
				return response(http.StatusServiceUnavailable, nil), nil
			}
			return response(http.StatusOK, nil), nil
		}),
		Sleep: func(time.Duration) {},
	}

	req := httptest.NewRequest(http.MethodGet, "https://api.github.com/x", http.NoBody)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if got := req.Header.Get("X-Attempt-Scribble"); got != "" {
		t.Errorf("the caller's request was mutated: %q", got)
	}
}
