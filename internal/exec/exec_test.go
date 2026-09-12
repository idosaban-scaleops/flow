package exec_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	flowexec "github.com/idosaban-scaleops/flow/internal/exec"
)

func TestShellQuote(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain word", "helm", "helm"},
		{"flag", "--reset-then-reuse-values", "--reset-then-reuse-values"},
		{"path", "/Users/ido/helm-values/dev.yaml", "/Users/ido/helm-values/dev.yaml"},
		{"key=value", "image.tag=v1", "image.tag=v1"},
		{"empty", "", "''"},
		{"space", "add new toolbar", "'add new toolbar'"},
		{"git format string", "%h %s", "'%h %s'"},
		{"glob", "refs/tags/*", "'refs/tags/*'"},
		{"single quote", "it's", `'it'\''s'`},
		{"dollar", "$HOME", "'$HOME'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := flowexec.ShellQuote(tt.in); got != tt.want {
				t.Errorf("ShellQuote(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCommandRendersCopyPasteableLine(t *testing.T) {
	got := flowexec.Command(flowexec.Opts{
		Name: "helm",
		Args: []string{"upgrade", "scaleops", "scaleops/scaleops", "--set", "a b=c"},
	})
	want := "helm upgrade scaleops scaleops/scaleops --set 'a b=c'"
	if got != want {
		t.Errorf("Command = %q, want %q", got, want)
	}
}

func TestRealRunCapturesOutputAndExitCode(t *testing.T) {
	r := &flowexec.Real{}
	ctx := context.Background()

	res, err := r.Run(ctx, flowexec.Opts{Name: "echo", Args: []string{"hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Stdout) != "hello" {
		t.Errorf("stdout = %q", res.Stdout)
	}

	res, err = r.Run(ctx, flowexec.Opts{Name: "sh", Args: []string{"-c", "echo oops >&2; exit 3"}})
	var exitErr *flowexec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("err = %v, want *exec.ExitError", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
	if !strings.Contains(exitErr.Error(), "oops") {
		t.Errorf("the error should carry the command's stderr, got %q", exitErr)
	}
}

func TestRealRunMissingBinary(t *testing.T) {
	_, err := (&flowexec.Real{}).Run(context.Background(),
		flowexec.Opts{Name: "definitely-not-a-real-binary-xyz"})
	if err == nil {
		t.Fatal("a missing binary must produce an error")
	}
	var exitErr *flowexec.ExitError
	if errors.As(err, &exitErr) {
		t.Error("a missing binary is not a non-zero exit; it must be distinguishable")
	}
}

func TestRealRunRespectsDir(t *testing.T) {
	dir := t.TempDir()
	res, err := (&flowexec.Real{}).Run(context.Background(),
		flowexec.Opts{Name: "pwd", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	// macOS resolves the temp directory through a symlink, so compare the
	// resolved forms.
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(strings.TrimSpace(res.Stdout))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("pwd = %q, want %q", got, want)
	}
}

func TestRealRunCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&flowexec.Real{}).Run(ctx, flowexec.Opts{Name: "sleep", Args: []string{"5"}}); err == nil {
		t.Fatal("a cancelled context must abort the command")
	}
}

func TestFakeMatchesQuotedAndPlainForms(t *testing.T) {
	f := flowexec.NewFake().Respond("git log --format=%h %s HEAD", "abc subject")

	res, err := f.Run(context.Background(), flowexec.Opts{
		Name: "git", Args: []string{"log", "--format=%h %s", "HEAD"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "abc subject" {
		t.Errorf("stdout = %q; the plain form of the command line should match", res.Stdout)
	}
}

func TestFakeLongestPrefixWins(t *testing.T) {
	f := flowexec.NewFake().
		Respond("git", "generic").
		Respond("git rev-parse", "specific")

	res, _ := f.Run(context.Background(), flowexec.Opts{Name: "git", Args: []string{"rev-parse", "HEAD"}})
	if res.Stdout != "specific" {
		t.Errorf("stdout = %q, want the longer prefix to win", res.Stdout)
	}
	res, _ = f.Run(context.Background(), flowexec.Opts{Name: "git", Args: []string{"status"}})
	if res.Stdout != "generic" {
		t.Errorf("stdout = %q, want the catch-all", res.Stdout)
	}
}

func TestFakeRecordsDetachedSeparately(t *testing.T) {
	f := flowexec.NewFake()
	if err := f.StartDetached(context.Background(),
		flowexec.Opts{Name: "cursor", Args: []string{"/wt"}, Dir: "/wt"}); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 0 {
		t.Error("a detached spawn must not appear as a synchronous call")
	}
	if len(f.Detached) != 1 || f.Detached[0].Dir != "/wt" {
		t.Errorf("detached = %+v", f.Detached)
	}
}

// TestDryRunRunsReadsAndSkipsWrites is the contract that makes --dry-run
// useful: a dry run that cannot inspect the repository cannot describe what it
// would do, so read-only commands must still execute.
func TestDryRunRunsReadsAndSkipsWrites(t *testing.T) {
	inner := flowexec.NewFake().Respond("git rev-parse", "/repo/.git")
	var announced []string

	d := &flowexec.DryRun{
		Inner: inner,
		IsRead: func(o flowexec.Opts) bool {
			return len(o.Args) > 0 && o.Args[0] == "rev-parse"
		},
		Announce: func(cmdline string) { announced = append(announced, cmdline) },
	}

	res, err := d.Run(context.Background(), flowexec.Opts{Name: "git", Args: []string{"rev-parse", "HEAD"}})
	if err != nil || res.Stdout != "/repo/.git" {
		t.Fatalf("a read-only command must actually run: %q %v", res.Stdout, err)
	}

	if _, err := d.Run(context.Background(), flowexec.Opts{
		Name: "git", Args: []string{"worktree", "add", "/wt"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(inner.Calls) != 1 {
		t.Errorf("the mutating command should not have reached the runner: %v", inner.CommandLines())
	}
	if len(announced) != 1 || announced[0] != "git worktree add /wt" {
		t.Errorf("announced = %v", announced)
	}
}

func TestDryRunNeverSpawnsDetached(t *testing.T) {
	inner := flowexec.NewFake()
	var announced []string
	d := &flowexec.DryRun{
		Inner:    inner,
		IsRead:   func(flowexec.Opts) bool { return false },
		Announce: func(c string) { announced = append(announced, c) },
	}

	if err := d.StartDetached(context.Background(),
		flowexec.Opts{Name: "cursor", Args: []string{"/wt"}}); err != nil {
		t.Fatal(err)
	}
	if len(inner.Detached) != 0 {
		t.Error("--dry-run must not launch the editor")
	}
	if len(announced) != 1 {
		t.Errorf("the spawn should still be announced, got %v", announced)
	}
}

func TestVerboseLoggingRedactsSecretCommands(t *testing.T) {
	log := &recordingLogger{}
	r := &flowexec.Real{Log: log, DebugOutput: true}

	if _, err := r.Run(context.Background(), flowexec.Opts{
		Name: "echo", Args: []string{"super-secret-token"}, Secret: true,
	}); err != nil {
		t.Fatal(err)
	}

	for _, line := range log.lines {
		if strings.Contains(line, "super-secret-token") {
			t.Fatalf("-vv leaked the output of a Secret command: %q", line)
		}
	}
	if len(log.lines) == 0 {
		t.Error("the command itself should still be logged")
	}
}

func TestVerboseLoggingShowsNonSecretOutput(t *testing.T) {
	log := &recordingLogger{}
	r := &flowexec.Real{Log: log, DebugOutput: true}

	if _, err := r.Run(context.Background(), flowexec.Opts{
		Name: "echo", Args: []string{"ordinary-output"},
	}); err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, line := range log.lines {
		if strings.Contains(line, "ordinary-output") {
			found = true
		}
	}
	if !found {
		t.Error("-vv should show the output of ordinary commands")
	}
}

type recordingLogger struct{ lines []string }

func (r *recordingLogger) Debug(msg any, keyvals ...any) { r.record(msg, keyvals) }
func (r *recordingLogger) Info(msg any, keyvals ...any)  { r.record(msg, keyvals) }

func (r *recordingLogger) record(msg any, keyvals []any) {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(strings.Join([]string{toString(msg)}, "")))
	for _, kv := range keyvals {
		b.WriteString(" ")
		b.WriteString(toString(kv))
	}
	r.lines = append(r.lines, b.String())
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
