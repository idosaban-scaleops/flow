// Package editor launches the user's editor on a worktree and immediately
// forgets about it. flow never closes an editor window — that is the user's
// job, and an explicit non-goal.
package editor

import (
	"context"
	"fmt"
	"strings"

	"github.com/idosaban-scaleops/flow/internal/config"
	flowexec "github.com/idosaban-scaleops/flow/internal/exec"
)

// Launcher spawns the configured editor.
type Launcher struct {
	Runner flowexec.Runner
	Cfg    config.Editor
}

// New returns a Launcher for the configured editor.
func New(r flowexec.Runner, cfg config.Editor) *Launcher {
	return &Launcher{Runner: r, Cfg: cfg}
}

// ErrNotOnPath reports a missing editor binary. Callers print a warning and
// carry on: a worktree without an editor window is still useful.
type ErrNotOnPath struct {
	Command string
	Err     error
}

func (e *ErrNotOnPath) Error() string {
	return fmt.Sprintf("editor %q is not on PATH", e.Command)
}

func (e *ErrNotOnPath) Unwrap() error { return e.Err }

// Args renders the configured argument list with {path} substituted.
func (l *Launcher) Args(path string) []string {
	args := l.Cfg.Args
	if len(args) == 0 {
		args = []string{"{path}"}
	}
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = strings.ReplaceAll(a, "{path}", path)
	}
	return out
}

// Open launches the editor with its working directory set to path.
//
// The spawn is detached — its own session, all three standard streams on
// /dev/null — so the editor survives flow exiting, and flow never blocks on it
// or leaves a zombie behind.
func (l *Launcher) Open(ctx context.Context, path string) error {
	command := l.Cfg.Command
	if command == "" {
		return &ErrNotOnPath{Command: command}
	}
	if _, err := l.Runner.LookPath(command); err != nil {
		return &ErrNotOnPath{Command: command, Err: err}
	}

	opts := flowexec.Opts{Name: command, Args: l.Args(path), Dir: path}
	if !l.Cfg.Detach {
		_, err := l.Runner.Run(ctx, opts)
		return err
	}
	return l.Runner.StartDetached(ctx, opts)
}
