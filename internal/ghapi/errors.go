package ghapi

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/go-github/v91/github"
)

// ErrNotFound reports a 404 from GitHub. Callers map it to exit code 3.
var ErrNotFound = errors.New("not found on GitHub")

// ErrUnauthorized reports a 401 or 403 that is not a rate limit. Its message
// names the token-resolution order, because that is almost always the fix.
var ErrUnauthorized = errors.New("not authorized by GitHub")

// RateLimitError reports an exhausted rate limit and when it resets.
type RateLimitError struct {
	ResetAt time.Time
	Message string
}

func (e *RateLimitError) Error() string {
	if e.ResetAt.IsZero() {
		return "GitHub rate limit exceeded"
	}
	return fmt.Sprintf("GitHub rate limit exceeded, resets at %s", e.ResetAt.Local().Format("15:04"))
}

// translate maps go-github errors onto flow's own sentinel errors, so no other
// package has to know what a *github.ErrorResponse is.
func translate(err error) error {
	if err == nil {
		return nil
	}

	var rate *github.RateLimitError
	if errors.As(err, &rate) {
		return &RateLimitError{ResetAt: rate.Rate.Reset.Time, Message: rate.Message}
	}
	var abuse *github.AbuseRateLimitError
	if errors.As(err, &abuse) {
		reset := time.Time{}
		if abuse.RetryAfter != nil {
			reset = time.Now().Add(*abuse.RetryAfter)
		}
		return &RateLimitError{ResetAt: reset, Message: abuse.Message}
	}

	var resp *github.ErrorResponse
	if errors.As(err, &resp) && resp.Response != nil {
		switch resp.Response.StatusCode {
		case http.StatusNotFound:
			return fmt.Errorf("%w: %s", ErrNotFound, resp.Message)
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("%w: %s (flow looks for a token in $GITHUB_TOKEN, "+
				"$GH_TOKEN, github.token in the config file, then the macOS keychain)",
				ErrUnauthorized, resp.Message)
		}
	}
	return err
}

// describeError renders a short, user-facing reason for an Unknown PR state.
func describeError(err error) string {
	if err == nil {
		return ""
	}

	var rate *github.RateLimitError
	if errors.As(err, &rate) {
		if rate.Rate.Reset.IsZero() {
			return "rate limited"
		}
		return "rate limited until " + rate.Rate.Reset.Time.Local().Format("15:04")
	}

	var resp *github.ErrorResponse
	if errors.As(err, &resp) && resp.Response != nil {
		switch resp.Response.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return "no usable GitHub token configured"
		case http.StatusNotFound:
			return "repository not found on GitHub"
		}
	}
	return "api.github.com unreachable"
}

// Describe renders any error as a short reason suitable for a prompt.
func Describe(err error) string { return describeError(err) }
