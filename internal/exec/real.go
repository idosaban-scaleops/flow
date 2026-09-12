package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"syscall"
)

// Logger receives a line per command when verbose logging is enabled. The
// signature matches charmbracelet/log, whose message parameter is an any.
type Logger interface {
	Debug(msg any, keyvals ...any)
	Info(msg any, keyvals ...any)
}

// Real runs commands with os/exec. It is the only place in the tree that may
// import os/exec.
type Real struct {
	Log Logger
	// DebugOutput mirrors -vv: log the full stdout/stderr of every command.
	DebugOutput bool
	// Stdout and Stderr receive streamed output; nil means the process's own.
	Stdout io.Writer
	Stderr io.Writer
}

var _ Runner = (*Real)(nil)

// Run executes opts to completion.
func (r *Real) Run(ctx context.Context, opts Opts) (Result, error) {
	r.logStart(opts)

	// The command and its arguments come from flow's own callers, which is the
	// entire purpose of this package: it is the single audited gateway to
	// os/exec, and nothing else in the tree may reach it.
	cmd := osexec.CommandContext(ctx, opts.Name, opts.Args...) //nolint:gosec // see above
	cmd.Dir = opts.Dir
	if len(opts.Env) > 0 {
		cmd.Env = append(os.Environ(), opts.Env...)
	}
	cmd.Stdin = opts.Stdin

	var stdout, stderr bytes.Buffer
	if opts.Stream {
		cmd.Stdout = orDefault(r.Stdout, os.Stdout)
		cmd.Stderr = orDefault(r.Stderr, os.Stderr)
	} else {
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
	}

	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}

	var exitErr *osexec.ExitError
	switch {
	case err == nil:
		res.ExitCode = 0
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		// Binary missing, permission denied, context cancelled.
		return res, fmt.Errorf("%s: %w", opts.Name, err)
	}

	r.logResult(opts, res)
	if res.ExitCode != 0 {
		return res, &ExitError{Cmd: Command(opts), Result: res}
	}
	return res, nil
}

// StartDetached spawns a process in its own session and does not wait for it.
func (r *Real) StartDetached(_ context.Context, opts Opts) error {
	r.logStart(opts)

	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	defer func() { _ = devNull.Close() }()

	// Deliberately not CommandContext: the child must outlive flow.
	cmd := osexec.Command(opts.Name, opts.Args...) //nolint:gosec,noctx // detached by design; see Run
	cmd.Dir = opts.Dir
	if len(opts.Env) > 0 {
		cmd.Env = append(os.Environ(), opts.Env...)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, devNull, devNull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", opts.Name, err)
	}
	// Reap the child without blocking, so flow leaves no zombie behind.
	go func() { _ = cmd.Wait() }()
	return nil
}

// LookPath reports the absolute path of name on PATH.
func (r *Real) LookPath(name string) (string, error) {
	return osexec.LookPath(name)
}

func (r *Real) logStart(opts Opts) {
	if r.Log == nil {
		return
	}
	// For a Secret invocation, log only the binary name. The keychain lookup
	// keeps its token in stdout rather than argv, but redacting both is free
	// and makes the guarantee unconditional.
	cmdline := Command(opts)
	if opts.Secret {
		cmdline = opts.Name + " <redacted>"
	}
	if opts.Dir != "" {
		r.Log.Info("run", "cmd", cmdline, "dir", opts.Dir)
		return
	}
	r.Log.Info("run", "cmd", cmdline)
}

func (r *Real) logResult(opts Opts, res Result) {
	if r.Log == nil || !r.DebugOutput {
		return
	}
	if opts.Secret {
		r.Log.Debug("done", "cmd", opts.Name, "exit", res.ExitCode, "output", "<redacted>")
		return
	}
	r.Log.Debug("done", "cmd", opts.Name, "exit", res.ExitCode,
		"stdout", res.Stdout, "stderr", res.Stderr)
}

func orDefault(w, def io.Writer) io.Writer {
	if w == nil {
		return def
	}
	return w
}

// ExitError reports a command that ran but returned a non-zero status.
type ExitError struct {
	Cmd    string
	Result Result
}

func (e *ExitError) Error() string {
	msg := firstLine(e.Result.Stderr)
	if msg == "" {
		msg = firstLine(e.Result.Stdout)
	}
	if msg == "" {
		return fmt.Sprintf("%s: exit status %d", e.Cmd, e.Result.ExitCode)
	}
	return fmt.Sprintf("%s: exit status %d: %s", e.Cmd, e.Result.ExitCode, msg)
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}
