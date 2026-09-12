// Package exec is the single gateway through which flow invokes external
// processes. No other package may import os/exec: routing everything through
// Runner is what makes the rest of the tool testable without a real git
// repository, a real herdr server, or a real Kubernetes cluster.
package exec

import (
	"context"
	"io"
)

// Result is the outcome of a completed external command.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int

	// Skipped reports that --dry-run announced this command instead of
	// running it, so the zero Stdout means "nothing ran" rather than "the
	// command produced no output". Any caller that parses Stdout must check
	// it; a caller that needs real output under --dry-run is asking a
	// read-only question and belongs on the unwrapped runner (App.ReadGit,
	// App.ReadHelm, App.ReadHerdr).
	Skipped bool
}

// Opts describes a single external command invocation.
type Opts struct {
	Name   string
	Args   []string
	Dir    string
	Env    []string // appended to os.Environ()
	Stdin  io.Reader
	Stream bool // when true, pipe stdout/stderr straight to the terminal

	// Secret suppresses verbose logging of this command's output. Set it for
	// anything that prints a credential (the keychain lookup, for example) so
	// that -vv can never leak a token into the log.
	Secret bool
}

// Runner runs external commands.
type Runner interface {
	// Run executes a command to completion.
	Run(ctx context.Context, opts Opts) (Result, error)

	// StartDetached spawns a command in its own session and returns without
	// waiting for it. Used for the editor, which must outlive flow.
	StartDetached(ctx context.Context, opts Opts) error

	// LookPath reports the absolute path of an executable on PATH.
	LookPath(name string) (string, error)
}

// Command renders an invocation the way a shell would accept it, for display
// in --dry-run output and verbose logs.
func Command(opts Opts) string {
	parts := make([]string, 0, len(opts.Args)+1)
	parts = append(parts, opts.Name)
	parts = append(parts, opts.Args...)
	return ShellJoin(parts)
}
