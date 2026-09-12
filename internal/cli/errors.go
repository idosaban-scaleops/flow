package cli

import (
	"context"
	"errors"
	"fmt"

	flowexec "github.com/idosaban-scaleops/flow/internal/exec"
	"github.com/idosaban-scaleops/flow/internal/ghapi"
	"github.com/idosaban-scaleops/flow/internal/output"
	"github.com/idosaban-scaleops/flow/internal/ticket"
)

// Exit codes. fang owns the error path, so command handlers never call
// os.Exit: they return an error, and main maps it through ExitCodeFor.
const (
	ExitOK           = 0
	ExitFailure      = 1
	ExitUsage        = 2
	ExitNotFound     = 3
	ExitPrecondition = 4
	ExitDependency   = 5
	ExitAborted      = 6
)

// Error is a flow error carrying the exit code it should produce.
type Error struct {
	Code int
	// Kind is the machine-readable code emitted in --json mode.
	Kind string
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	if e.Msg != "" && e.Err != nil {
		return e.Msg + ": " + e.Err.Error()
	}
	if e.Msg != "" {
		return e.Msg
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "unknown error"
}

func (e *Error) Unwrap() error { return e.Err }

func newError(code int, kind, format string, a ...any) *Error {
	return &Error{Code: code, Kind: kind, Msg: fmt.Sprintf(format, a...)}
}

// Usage reports a bad flag or argument.
func Usage(format string, a ...any) *Error {
	return newError(ExitUsage, "usage", format, a...)
}

// NotFound reports an unknown ticket, missing worktree, or absent PR.
func NotFound(format string, a ...any) *Error {
	return newError(ExitNotFound, "not_found", format, a...)
}

// Precondition reports a state flow refuses to act on: a dirty worktree, an
// unmerged PR, the wrong kube context.
func Precondition(format string, a ...any) *Error {
	return newError(ExitPrecondition, "precondition_failed", format, a...)
}

// Dependency reports a missing or failing external tool.
func Dependency(format string, a ...any) *Error {
	return newError(ExitDependency, "dependency", format, a...)
}

// Failure reports a generic error.
func Failure(format string, a ...any) *Error {
	return newError(ExitFailure, "failure", format, a...)
}

// Wrap attaches an exit code to an existing error.
func Wrap(code int, kind string, err error, format string, a ...any) *Error {
	return &Error{Code: code, Kind: kind, Msg: fmt.Sprintf(format, a...), Err: err}
}

// ExitCodeFor maps any error to flow's exit-code scheme.
func ExitCodeFor(err error) int {
	if err == nil {
		return ExitOK
	}

	// Cancellation is checked before *Error. A Ctrl-C during `helm upgrade`
	// surfaces as context.Canceled wrapped in Wrap(ExitDependency,
	// "helm_failed", …), and the outer code would otherwise win and report a
	// dependency failure for what the user did deliberately. Aborted() already
	// looks through the whole chain, so the two must agree.
	if Aborted(err) {
		return ExitAborted
	}

	var flowErr *Error
	if errors.As(err, &flowErr) {
		return flowErr.Code
	}

	var notInteractive *output.ErrNotInteractive
	if errors.As(err, &notInteractive) {
		return ExitPrecondition
	}

	var invalid *ticket.ErrInvalid
	if errors.As(err, &invalid) {
		return ExitUsage
	}

	// ghapi's sentinels can reach here unwrapped, notably from the CI wait,
	// which passes ListRunJobs errors straight through.
	var rateLimited *ghapi.RateLimitError
	switch {
	case errors.Is(err, ghapi.ErrNotFound):
		return ExitNotFound
	case errors.Is(err, ghapi.ErrUnauthorized), errors.As(err, &rateLimited):
		return ExitDependency
	}

	var exitErr *flowexec.ExitError
	if errors.As(err, &exitErr) {
		return ExitDependency
	}

	return ExitFailure
}

// KindFor maps an error to the machine-readable code used in JSON output.
func KindFor(err error) string {
	// Same ordering as ExitCodeFor: an aborted action must not report the
	// wrapping layer's kind ("helm_failed") alongside exit code 6.
	if Aborted(err) {
		return "aborted"
	}

	var flowErr *Error
	if errors.As(err, &flowErr) && flowErr.Kind != "" {
		return flowErr.Kind
	}
	switch ExitCodeFor(err) {
	case ExitUsage:
		return "usage"
	case ExitNotFound:
		return "not_found"
	case ExitPrecondition:
		return "precondition_failed"
	case ExitDependency:
		return "dependency"
	case ExitAborted:
		return "aborted"
	default:
		return "failure"
	}
}

// ErrorAlreadyReported reports an error whose JSON envelope has already been
// written to stdout, so the terminal error handler stays silent about it.
func ErrorAlreadyReported(err error) bool { return jsonErrorEmitted(err) }

// Aborted reports an error that means the user stopped the command themselves,
// which deserves a one-line acknowledgement rather than a styled error page.
func Aborted(err error) bool {
	return errors.Is(err, output.ErrAborted) || errors.Is(err, context.Canceled)
}
