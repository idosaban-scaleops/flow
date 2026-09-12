package ghapi

import (
	"bytes"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// maxRetries bounds retry attempts for 5xx and transport errors.
const maxRetries = 3

// RetryTransport retries transport errors, 5xx responses and 429s with
// exponential backoff plus jitter. Implementing it as a RoundTripper means the
// policy applies uniformly instead of being repeated at every call site.
type RetryTransport struct {
	Base http.RoundTripper
	// Sleep is time.Sleep in production; tests substitute a no-op.
	Sleep func(time.Duration)
}

// NewRetryTransport wraps base with the retry policy.
func NewRetryTransport(base http.RoundTripper) *RetryTransport {
	return &RetryTransport{Base: base, Sleep: time.Sleep}
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
		if body != nil {
			req.Body = io.NopCloser(bytes.NewReader(body))
		}

		resp, err = t.base().RoundTrip(req)
		if !t.shouldRetry(resp, err) || attempt == maxRetries-1 {
			return resp, err
		}
		if resp != nil {
			// Drain so the connection can be reused.
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}

		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		default:
		}
		t.sleep(backoff(attempt))
	}
	return resp, err
}

// shouldRetry retries transport errors, 5xx and 429. A 4xx other than 429 is a
// client mistake and retrying it just burns rate limit.
func (t *RetryTransport) shouldRetry(resp *http.Response, err error) bool {
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

func (t *RetryTransport) sleep(d time.Duration) {
	if t.Sleep != nil {
		t.Sleep(d)
		return
	}
	time.Sleep(d)
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
