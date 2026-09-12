package ghapi

import (
	"bytes"
	"context"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxRetries bounds retry attempts for 5xx and transport errors.
const maxRetries = 3

// maxRetryWait caps how long a server-supplied Retry-After or X-RateLimit-Reset
// is honored. Past this, handing the 429 back so the caller can report a
// rate-limit error beats blocking a CLI for minutes.
const maxRetryWait = 60 * time.Second

// RetryTransport retries transport errors, 5xx responses and 429s, preferring
// the server's own Retry-After guidance over exponential backoff plus jitter.
// Implementing it as a RoundTripper means the policy applies uniformly instead
// of being repeated at every call site.
type RetryTransport struct {
	Base http.RoundTripper
	// Sleep overrides the wait between attempts; tests substitute a no-op.
	// Left nil, the wait is a cancellable timer, so Ctrl-C during a backoff
	// aborts instead of running it out.
	Sleep func(time.Duration)
}

// NewRetryTransport wraps base with the retry policy.
func NewRetryTransport(base http.RoundTripper) *RetryTransport {
	return &RetryTransport{Base: base}
}

// RoundTrip implements http.RoundTripper.
func (t *RetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
	}

	var resp *http.Response
	var err error
	for attempt := range maxRetries {
		// Clone per attempt. A RoundTripper must not modify the request it is
		// given, and the original's Body was consumed above; CachingTransport
		// already follows the same rule.
		attemptReq := req.Clone(req.Context())
		if body != nil {
			attemptReq.Body = io.NopCloser(bytes.NewReader(body))
		}

		resp, err = t.base().RoundTrip(attemptReq)
		if !shouldRetry(attemptReq, resp, err) || attempt == maxRetries-1 {
			return resp, err
		}

		// Computed before draining, so the response is still intact if the
		// wait turns out to be too long to sit through.
		wait := retryDelay(resp, attempt, time.Now())
		if wait > maxRetryWait {
			return resp, err
		}

		if resp != nil {
			// Drain so the connection can be reused.
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}

		if sleepErr := t.sleep(req.Context(), wait); sleepErr != nil {
			return nil, sleepErr
		}
	}
	return resp, err
}

// shouldRetry retries transport errors, 5xx and 429. A 4xx other than 429 is a
// client mistake and retrying it just burns rate limit.
//
// Only idempotent methods are replayed. flow issues nothing but GETs today, so
// this changes no current behaviour; it stops a future POST from being silently
// submitted twice because its response happened to be a 502.
func shouldRetry(req *http.Request, resp *http.Response, err error) bool {
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		return false
	}
	if err != nil {
		return true
	}
	if resp == nil {
		return false
	}
	return resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
}

func (t *RetryTransport) base() http.RoundTripper {
	if t.Base != nil {
		return t.Base
	}
	return http.DefaultTransport
}

// sleep waits between attempts, returning early if the request's context is
// cancelled. Checking the context only before the sleep left a Ctrl-C sitting
// through the whole backoff.
func (t *RetryTransport) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.Sleep != nil {
		t.Sleep(d)
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// retryDelay prefers the server's own guidance to a guess. GitHub sends
// Retry-After on a secondary rate limit and X-RateLimit-Reset once the primary
// quota is spent; ignoring both and coming back 500ms later is exactly what
// earns a longer block.
func retryDelay(resp *http.Response, attempt int, now time.Time) time.Duration {
	if resp != nil {
		if d, ok := retryAfter(resp.Header, now); ok {
			return d
		}
	}
	return backoff(attempt)
}

// retryAfter reads Retry-After (delta-seconds or an HTTP-date), then
// X-RateLimit-Reset when the remaining quota is actually zero.
func retryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			return atLeastZero(time.Duration(secs) * time.Second), true
		}
		if when, err := http.ParseTime(v); err == nil {
			return atLeastZero(when.Sub(now)), true
		}
	}
	if h.Get("X-RateLimit-Remaining") == "0" {
		if v := strings.TrimSpace(h.Get("X-RateLimit-Reset")); v != "" {
			if epoch, err := strconv.ParseInt(v, 10, 64); err == nil {
				return atLeastZero(time.Unix(epoch, 0).Sub(now)), true
			}
		}
	}
	return 0, false
}

func atLeastZero(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}

func backoff(attempt int) time.Duration {
	base := time.Duration(1<<attempt) * 500 * time.Millisecond
	jitter := time.Duration(rand.Int64N(int64(base / 2))) //nolint:gosec // jitter, not cryptography
	return base + jitter
}

// CachingTransport implements conditional requests with ETags. A 45-minute CI
// wait polling every 5s is roughly 540 requests; without this, each one costs a
// rate-limit unit even when nothing changed.
//
// It is deliberately a small in-memory map rather than a dependency on an
// unmaintained HTTP cache library: flow's polling loop is the only consumer,
// the responses are small, and the cache lives no longer than one command.
type CachingTransport struct {
	Base http.RoundTripper

	mu      sync.Mutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	etag string
	body []byte
	// header is the stored response header, so rate-limit fields survive a 304.
	header http.Header
	status int
}

// NewCachingTransport wraps base with an ETag cache.
func NewCachingTransport(base http.RoundTripper) *CachingTransport {
	return &CachingTransport{Base: base, entries: map[string]cacheEntry{}}
}

// RoundTrip implements http.RoundTripper.
func (t *CachingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return t.base().RoundTrip(req)
	}
	key := req.URL.String()

	t.mu.Lock()
	entry, cached := t.entries[key]
	t.mu.Unlock()

	if cached && entry.etag != "" {
		// Clone so the caller's request is not mutated across retries.
		req = req.Clone(req.Context())
		req.Header.Set("If-None-Match", entry.etag)
	}

	resp, err := t.base().RoundTrip(req)
	if err != nil {
		return resp, err
	}

	if resp.StatusCode == http.StatusNotModified && cached {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return replayed(resp, entry), nil
	}

	if resp.StatusCode == http.StatusOK {
		if etag := resp.Header.Get("ETag"); etag != "" {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				return nil, readErr
			}
			t.mu.Lock()
			t.entries[key] = cacheEntry{
				etag: etag, body: body, header: resp.Header.Clone(), status: resp.StatusCode,
			}
			t.mu.Unlock()
			resp.Body = io.NopCloser(bytes.NewReader(body))
		}
	}
	return resp, nil
}

// replayed rebuilds a 200 response from the cache, carrying over the fresh
// rate-limit headers from the 304 so callers still see accurate quota.
func replayed(resp *http.Response, entry cacheEntry) *http.Response {
	header := entry.header.Clone()
	for _, name := range []string{
		"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "X-RateLimit-Used",
	} {
		if v := resp.Header.Get(name); v != "" {
			header.Set(name, v)
		}
	}
	header.Set("Content-Length", strconv.Itoa(len(entry.body)))

	return &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Proto:         resp.Proto,
		ProtoMajor:    resp.ProtoMajor,
		ProtoMinor:    resp.ProtoMinor,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(entry.body)),
		ContentLength: int64(len(entry.body)),
		Request:       resp.Request,
	}
}

func (t *CachingTransport) base() http.RoundTripper {
	if t.Base != nil {
		return t.Base
	}
	return http.DefaultTransport
}
