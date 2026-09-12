package editor_test

import (
	"context"
	"errors"
	"testing"

	"github.com/idosaban-scaleops/flow/internal/config"
	"github.com/idosaban-scaleops/flow/internal/editor"
	flowexec "github.com/idosaban-scaleops/flow/internal/exec"
)

func TestOpenSpawnsDetachedAtWorktree(t *testing.T) {
	f := flowexec.NewFake()
	l := editor.New(f, config.Editor{Command: "cursor", Args: []string{"{path}"}, Detach: true})

	if err := l.Open(context.Background(), "/repo/.worktrees/RD-1-a"); err != nil {
		t.Fatal(err)
	}

	if len(f.Calls) != 0 {
		t.Errorf("a detached editor must not be run synchronously: %v", f.CommandLines())
	}
	if len(f.Detached) != 1 {
		t.Fatalf("want one detached spawn, got %d", len(f.Detached))
	}
	got := f.Detached[0]
	if got.String() != "cursor /repo/.worktrees/RD-1-a" {
		t.Errorf("argv = %q", got.String())
	}
	if got.Dir != "/repo/.worktrees/RD-1-a" {
		t.Errorf("working directory = %q", got.Dir)
	}
}

func TestOpenSubstitutesPathInEveryArg(t *testing.T) {
	f := flowexec.NewFake()
	l := editor.New(f, config.Editor{
		Command: "code",
		Args:    []string{"--new-window", "{path}", "--goto", "{path}/README.md"},
		Detach:  true,
	})

	if err := l.Open(context.Background(), "/wt"); err != nil {
		t.Fatal(err)
	}
	want := "code --new-window /wt --goto /wt/README.md"
	if got := f.Detached[0].String(); got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

func TestOpenDefaultsToPathArgument(t *testing.T) {
	f := flowexec.NewFake()
	l := editor.New(f, config.Editor{Command: "vim", Detach: true})

	if err := l.Open(context.Background(), "/wt"); err != nil {
		t.Fatal(err)
	}
	if got := f.Detached[0].String(); got != "vim /wt" {
		t.Errorf("argv = %q, want vim /wt", got)
	}
}

func TestOpenWithoutDetachRunsSynchronously(t *testing.T) {
	f := flowexec.NewFake()
	l := editor.New(f, config.Editor{Command: "vim", Args: []string{"{path}"}, Detach: false})

	if err := l.Open(context.Background(), "/wt"); err != nil {
		t.Fatal(err)
	}
	if len(f.Detached) != 0 {
		t.Error("detach: false must not spawn detached")
	}
	if len(f.Calls) != 1 {
		t.Fatalf("want one synchronous call, got %d", len(f.Calls))
	}
}

func TestOpenMissingBinaryIsRecoverable(t *testing.T) {
	f := flowexec.NewFake()
	f.Missing["cursor"] = true
	l := editor.New(f, config.Editor{Command: "cursor", Detach: true})

	err := l.Open(context.Background(), "/wt")
	var notOnPath *editor.ErrNotOnPath
	if !errors.As(err, &notOnPath) {
		t.Fatalf("err = %v, want *editor.ErrNotOnPath so callers can warn and continue", err)
	}
	if len(f.Detached) != 0 {
		t.Error("nothing should have been spawned")
	}
}
