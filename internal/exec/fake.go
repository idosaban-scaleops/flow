package exec

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// Call is one recorded invocation.
type Call struct {
	Name string
	Args []string
	Dir  string
}

// String renders the call as a shell command line, which is what argv
// assertions compare against.
func (c Call) String() string {
	return ShellJoin(append([]string{c.Name}, c.Args...))
}

// Response is a canned reply for a matched command.
type Response struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error
}

// Fake is a Runner that replays canned responses and records every call.
//
// A response prefix is matched against the plain, space-joined command line
// ("git log --format=%h %s HEAD") as well as the shell-quoted rendering, so
// tests can stub a command by writing it the way it reads. The longest matching
// prefix wins, which lets a test stub "git rev-parse --git-common-dir"
// precisely while leaving a broad "git" entry as a catch-all.
type Fake struct {
	mu sync.Mutex

	// Responses maps a command-line prefix to its canned result.
	Responses map[string]Response
	// Missing lists binaries that LookPath should fail for.
	Missing map[string]bool
	// Default is returned when no prefix matches.
	Default Response

	Calls    []Call
	Detached []Call
}

var _ Runner = (*Fake)(nil)

// NewFake returns a Fake with empty response tables.
func NewFake() *Fake {
	return &Fake{Responses: map[string]Response{}, Missing: map[string]bool{}}
}

// Respond registers a canned stdout for commands matching prefix.
func (f *Fake) Respond(prefix, stdout string) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Responses[prefix] = Response{Stdout: stdout}
	return f
}

// RespondWith registers a full canned response for commands matching prefix.
func (f *Fake) RespondWith(prefix string, r Response) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Responses[prefix] = r
	return f
}

// Run records the call and returns its canned response.
func (f *Fake) Run(ctx context.Context, opts Opts) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	f.mu.Lock()
	call := Call{Name: opts.Name, Args: append([]string(nil), opts.Args...), Dir: opts.Dir}
	f.Calls = append(f.Calls, call)
	resp := f.lookupLocked(call)
	f.mu.Unlock()

	if resp.Err != nil {
		return Result{}, resp.Err
	}
	res := Result{Stdout: resp.Stdout, Stderr: resp.Stderr, ExitCode: resp.ExitCode}
	if res.ExitCode != 0 {
		return res, &ExitError{Cmd: Command(opts), Result: res}
	}
	return res, nil
}

// StartDetached records the call without running anything.
func (f *Fake) StartDetached(_ context.Context, opts Opts) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Detached = append(f.Detached, Call{
		Name: opts.Name, Args: append([]string(nil), opts.Args...), Dir: opts.Dir,
	})
	return nil
}

// LookPath succeeds for every binary not listed in Missing.
func (f *Fake) LookPath(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Missing[name] {
		return "", fmt.Errorf("exec: %q: executable file not found in $PATH", name)
	}
	return "/usr/bin/" + name, nil
}

func (f *Fake) lookupLocked(c Call) Response {
	quoted := c.String()
	plain := strings.Join(append([]string{c.Name}, c.Args...), " ")

	best, bestLen := f.Default, -1
	for prefix, resp := range f.Responses {
		if len(prefix) <= bestLen {
			continue
		}
		if strings.HasPrefix(quoted, prefix) || strings.HasPrefix(plain, prefix) {
			best, bestLen = resp, len(prefix)
		}
	}
	return best
}

// CommandLines returns every recorded call as a shell command line.
func (f *Fake) CommandLines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.Calls))
	for i, c := range f.Calls {
		out[i] = c.String()
	}
	return out
}

// Ran reports whether any recorded call starts with prefix.
func (f *Fake) Ran(prefix string) bool {
	for _, line := range f.CommandLines() {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}
