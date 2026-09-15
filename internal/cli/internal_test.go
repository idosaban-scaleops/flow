package cli

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/idosaban-scaleops/flow/internal/config"
	flowexec "github.com/idosaban-scaleops/flow/internal/exec"
	"github.com/idosaban-scaleops/flow/internal/helmx"
	"github.com/idosaban-scaleops/flow/internal/kube"
	"github.com/idosaban-scaleops/flow/internal/output"
	"github.com/idosaban-scaleops/flow/internal/registry"
)

func durationSeconds(n int) time.Duration { return time.Duration(n) * time.Second }

func TestParseGitVersion(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantMajor int
		wantMinor int
		wantOK    bool
	}{
		{"apple git", "git version 2.54.0 (Apple Git-157)", 2, 54, true},
		{"plain", "git version 2.5.0", 2, 5, true},
		{"old", "git version 1.9.3", 1, 9, true},
		{"two components", "git version 2.39", 2, 39, true},
		{"unparseable", "git is not installed", 0, 0, false},
		{"empty", "", 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			major, minor, ok := parseGitVersion(tt.in)
			if ok != tt.wantOK || major != tt.wantMajor || minor != tt.wantMinor {
				t.Errorf("parseGitVersion(%q) = %d,%d,%v want %d,%d,%v",
					tt.in, major, minor, ok, tt.wantMajor, tt.wantMinor, tt.wantOK)
			}
		})
	}
}

func TestSplitRepoKey(t *testing.T) {
	tests := []struct {
		in        string
		wantOwner string
		wantName  string
		wantOK    bool
	}{
		{"scaleops-sh/scaleops", "scaleops-sh", "scaleops", true},
		{"o/r", "o", "r", true},
		{"/Users/ido/Developer/scaleops", "", "", false},
		{"noslash", "", "", false},
		{"a/b/c", "", "", false},
		{"", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			owner, name, ok := splitRepoKey(tt.in)
			if ok != tt.wantOK || owner != tt.wantOwner || name != tt.wantName {
				t.Errorf("splitRepoKey(%q) = %q,%q,%v want %q,%q,%v",
					tt.in, owner, name, ok, tt.wantOwner, tt.wantName, tt.wantOK)
			}
		})
	}
}

func TestInferFromWorktreeLayout(t *testing.T) {
	tests := []struct {
		name     string
		dir      string
		wantID   string
		wantPath string
		wantOK   bool
	}{
		{
			name:     "at the worktree root",
			dir:      "/repo/.worktrees/RD-19471-add-new-toolbar",
			wantID:   "RD-19471",
			wantPath: "/repo/.worktrees/RD-19471-add-new-toolbar",
			wantOK:   true,
		},
		{
			name:     "deep inside",
			dir:      "/repo/.worktrees/RD-19471-add-new-toolbar/pkg/api/v2",
			wantID:   "RD-19471",
			wantPath: "/repo/.worktrees/RD-19471-add-new-toolbar",
			wantOK:   true,
		},
		{
			name:     "nested repos pick the innermost",
			dir:      "/a/.worktrees/RD-1-x/b/.worktrees/RD-2-y/pkg",
			wantID:   "RD-2",
			wantPath: "/a/.worktrees/RD-1-x/b/.worktrees/RD-2-y",
			wantOK:   true,
		},
		{"not under .worktrees", "/repo/pkg/api", "", "", false},
		{"directory is not a ticket", "/repo/.worktrees/scratch", "", "", false},
		{"the .worktrees directory itself", "/repo/.worktrees", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tk, path, ok := inferFromWorktreeLayout(tt.dir, ".worktrees")
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && (tk.ID != tt.wantID || path != tt.wantPath) {
				t.Errorf("= %q at %q, want %q at %q", tk.ID, path, tt.wantID, tt.wantPath)
			}
		})
	}
}

func TestReplaceValuesFlag(t *testing.T) {
	tests := []struct {
		name  string
		extra []string
		want  string
		out   []string
	}{
		{
			name:  "swaps the configured flag",
			extra: []string{"--reset-then-reuse-values"},
			want:  "--reuse-values",
			out:   []string{"--reuse-values"},
		},
		{
			name:  "keeps unrelated extra args",
			extra: []string{"--debug", "--reset-values", "--devel"},
			want:  "--reset-then-reuse-values",
			out:   []string{"--debug", "--devel", "--reset-then-reuse-values"},
		},
		{
			name:  "adds one when none was configured",
			extra: nil,
			want:  "--reset-values",
			out:   []string{"--reset-values"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := replaceValuesFlag(tt.extra, tt.want)
			if len(got) != len(tt.out) {
				t.Fatalf("got %v, want %v", got, tt.out)
			}
			for i := range got {
				if got[i] != tt.out[i] {
					t.Fatalf("got %v, want %v", got, tt.out)
				}
			}
		})
	}
}

func TestHumanAge(t *testing.T) {
	tests := []struct {
		seconds int
		want    string
	}{
		{30, "just now"},
		{60 * 5, "5m"},
		{60 * 60 * 3, "3h"},
		{60 * 60 * 24 * 2, "2d"},
		{60 * 60 * 24 * 21, "3w"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := humanAge(durationSeconds(tt.seconds)); got != tt.want {
				t.Errorf("humanAge(%ds) = %q, want %q", tt.seconds, got, tt.want)
			}
		})
	}
}

// TestKubeContextIsAlwaysPassedToHelm pins the rule that the command flow
// prints and the command flow runs name the same cluster. Sending
// --kube-context only for an override left helm to re-resolve the context
// itself, so a `kubectl config use-context` between the confirmation prompt
// and the upgrade redirected it to a different cluster.
func TestKubeContextIsAlwaysPassedToHelm(t *testing.T) {
	tests := []struct {
		name   string
		target clusterTarget
		want   string
	}{
		{
			name:   "ambient current context",
			target: clusterTarget{Context: "dev", Overridden: false},
			want:   "dev",
		},
		{
			name:   "explicit --context override",
			target: clusterTarget{Context: "other", Overridden: true},
			want:   "other",
		},
		{
			name:   "no context resolved at all",
			target: clusterTarget{},
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.target.kubeContextArg(); got != tt.want {
				t.Errorf("kubeContextArg() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDryRunClassifiesEveryGitCallSite audits isReadOnlyCommand against the
// argv internal/gitx actually produces. mutatingGitVerbs is a denylist, so the
// risk is a read being misclassified as a write (dry-run then cannot inspect)
// or a write slipping through as a read (dry-run then mutates).
func TestDryRunClassifiesEveryGitCallSite(t *testing.T) {
	tests := []struct {
		args     []string
		wantRead bool
	}{
		// Reads: a dry run must still be able to make these.
		{[]string{"rev-parse", "--path-format=absolute", "--git-common-dir"}, true},
		{[]string{"rev-parse", "--abbrev-ref", "HEAD"}, true},
		{[]string{"remote", "get-url", "origin"}, true},
		{[]string{"show-ref", "--verify", "--quiet", "refs/heads/RD-1"}, true},
		{[]string{"for-each-ref", "--sort=-creatordate", "--count", "1"}, true},
		{[]string{"branch", "--merged", "origin/main", "--format=%(refname:short)"}, true},
		{[]string{"status", "--porcelain"}, true},
		{[]string{"rev-list", "--left-right", "--count", "@{upstream}...HEAD"}, true},
		{[]string{"log", "--format=%h %s", "origin/main..RD-1"}, true},
		{[]string{"worktree", "list", "--porcelain"}, true},

		// Writes: a dry run must announce and skip these.
		{[]string{"fetch", "--tags", "origin"}, false},
		{[]string{"branch", "-d", "RD-1"}, false},
		{[]string{"branch", "-D", "RD-1"}, false},
		{[]string{"worktree", "add", "/wt", "RD-1"}, false},
		{[]string{"worktree", "add", "--track", "-b", "RD-1", "/wt", "origin/RD-1"}, false},
		{[]string{"worktree", "prune"}, false},
		{[]string{"worktree", "remove", "/wt"}, false},
	}

	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			got := isReadOnlyCommand(flowexec.Opts{Name: "git", Args: tt.args})
			if got != tt.wantRead {
				verb := "mutating"
				if tt.wantRead {
					verb = "read-only"
				}
				t.Errorf("isReadOnlyCommand = %v, want %v (this call is %v)", got, tt.wantRead, verb)
			}
		})
	}
}

// TestDryRunClassifiesNonGitTools covers the other binaries flow drives.
func TestDryRunClassifiesNonGitTools(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantRead bool
	}{
		{"kubectl", []string{"config", "current-context"}, true},
		{"kubectl", []string{"config", "view", "-o", "json"}, true},
		{"kubectl", []string{"config", "get-contexts", "-o", "name"}, true},
		// flow never issues this, and it would rewrite the user's kubeconfig.
		{"kubectl", []string{"config", "use-context", "prod"}, false},
		{"kubectl", []string{"get", "pods"}, true},
		{"helm", []string{"status", "scaleops"}, true},
		{"helm", []string{"repo", "list"}, true},
		{"helm", []string{"repo", "update"}, false},
		{"helm", []string{"upgrade", "scaleops", "scaleops/scaleops"}, false},
		{"helm", []string{"uninstall", "scaleops"}, false},
		{"herdr", []string{"workspace", "list"}, true},
		{"herdr", []string{"workspace", "create"}, false},
		{"herdr", []string{"--version"}, true},
		{"security", []string{"find-generic-password", "-s", "svc", "-w"}, true},
		{"open", []string{"https://example.com"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name+" "+strings.Join(tt.args, " "), func(t *testing.T) {
			got := isReadOnlyCommand(flowexec.Opts{Name: tt.name, Args: tt.args})
			if got != tt.wantRead {
				t.Errorf("isReadOnlyCommand = %v, want %v", got, tt.wantRead)
			}
		})
	}
}

// TestStatusFrame pins the one part of `cluster status --watch` worth pinning:
// the frame is a pure string, so every state the watch can be in is checkable
// without a terminal or a cluster.
func TestStatusFrame(t *testing.T) {
	app := &App{Out: output.New(output.Options{
		NoColor: true, Stdout: io.Discard, Stderr: io.Discard,
	})}
	target := clusterTarget{
		Context: "dev", Server: "https://dev.example",
		Settings: config.ClusterConfig{ReleaseName: "scaleops", Namespace: "scaleops-system"},
	}

	tests := []struct {
		name     string
		snap     clusterStatus
		want     []string
		wantGone []string
	}{
		{
			name: "deployed release with pods",
			snap: clusterStatus{
				release: helmx.Release{Revision: 7, Status: "deployed", ChartVersion: "1.0.1-alpha"},
				pods: []kube.Pod{
					{Name: "scaleops-agent", Phase: "Running", Ready: 1, Total: 1},
				},
			},
			want:     []string{"kube context", "dev", "revision", "7", "deployed", "1.0.1-alpha", "scaleops-agent", "1/1"},
			wantGone: []string{"not Running"},
		},
		{
			name: "unhealthy pod is counted",
			snap: clusterStatus{
				release: helmx.Release{Revision: 1, Status: "failed", Description: "upgrade failed"},
				pods: []kube.Pod{
					{Name: "broken", Phase: "Pending", Reason: "ImagePullBackOff", Ready: 0, Total: 1},
				},
			},
			want: []string{"failed", "upgrade failed", "ImagePullBackOff", "1 pod(s) are not Running"},
		},
		{
			name:     "no release yet",
			snap:     clusterStatus{relErr: errors.New("release: not found")},
			want:     []string{"no helm release", "no pods in the namespace"},
			wantGone: []string{"revision"},
		},
		{
			name: "pods cannot be listed",
			snap: clusterStatus{
				release: helmx.Release{Revision: 2, Status: "deployed"},
				podErr:  errors.New("connection refused"),
			},
			want:     []string{"could not list pods in scaleops-system"},
			wantGone: []string{"no pods in the namespace"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frame := app.statusFrame(target, tt.snap)
			for _, want := range tt.want {
				if !strings.Contains(frame, want) {
					t.Errorf("frame is missing %q:\n%s", want, frame)
				}
			}
			for _, gone := range tt.wantGone {
				if strings.Contains(frame, gone) {
					t.Errorf("frame should not mention %q:\n%s", gone, frame)
				}
			}
		})
	}
}

// appWithHerdr builds an App whose herdr client is the fake runner, with a
// buffer standing in for stderr so warnings can be asserted.
func appWithHerdr(f flowexec.Runner, errOut *strings.Builder) *App {
	cfg := config.Default()
	return &App{
		cfg:        cfg,
		Runner:     f,
		baseRunner: f,
		Out: output.New(output.Options{
			NoColor: true, Stdout: io.Discard, Stderr: errOut,
		}),
	}
}

// --label names the workspace flow creates. The flag is the only way to give a
// workspace a readable name and still have flow find it again, since the
// derived label is mechanically {id}.
func TestExplicitLabelReachesHerdr(t *testing.T) {
	f := flowexec.NewFake().
		Respond("herdr workspace list", `{"result":{"workspaces":[]}}`).
		Respond("herdr workspace create",
			`{"result":{"workspace":{"workspace_id":"w9","label":"New Toolbar"}}}`)

	var errOut strings.Builder
	app := appWithHerdr(f, &errOut)
	result := initResult{Entry: registry.Entry{
		TicketID: "RD-1", WorktreePath: "/repo/.worktrees/RD-1-toolbar",
	}}
	app.ensureWorkspace(t.Context(), &result,
		names{WorkspaceLabel: "New Toolbar", LabelExplicit: true}, false)

	// CommandLines quotes arguments containing spaces, which a readable label has.
	want := "herdr workspace create --cwd /repo/.worktrees/RD-1-toolbar --label 'New Toolbar'"
	if !f.Ran(want) {
		t.Errorf("argv = %v, want %q", f.CommandLines(), want)
	}
	if result.Entry.Workspace.ID != "w9" || result.Entry.Workspace.Label != "New Toolbar" {
		t.Errorf("recorded workspace = %+v", result.Entry.Workspace)
	}
}

// A workspace found by its recorded ID is focused, never retitled: the user may
// have renamed it deliberately. An explicit --label that cannot be honoured is
// reported rather than silently dropped.
func TestExplicitLabelDoesNotRenameAnExistingWorkspace(t *testing.T) {
	f := flowexec.NewFake().
		Respond("herdr workspace list",
			`{"result":{"workspaces":[{"workspace_id":"w12","label":"ROT Auto Timeline"}]}}`).
		Respond("herdr workspace focus", `{"result":{"type":"ok"}}`)

	var errOut strings.Builder
	app := appWithHerdr(f, &errOut)
	result := initResult{Entry: registry.Entry{
		TicketID:  "RD-1",
		Workspace: registry.Workspace{Provider: "herdr", ID: "w12", Label: "RD-1"},
	}}
	app.ensureWorkspace(t.Context(), &result,
		names{WorkspaceLabel: "Something Else", LabelExplicit: true}, false)

	for _, line := range f.CommandLines() {
		if strings.Contains(line, "workspace create") || strings.Contains(line, "workspace rename") {
			t.Errorf("an existing workspace must be focused, not %q", line)
		}
	}
	if !f.Ran("herdr workspace focus w12") {
		t.Errorf("argv = %v, want a focus of w12", f.CommandLines())
	}
	if got := errOut.String(); !strings.Contains(got, "herdr workspace rename w12") {
		t.Errorf("stderr = %q, want a warning naming the rename command", got)
	}
	// The recorded label tracks herdr, so the next run finds it by label too.
	if result.Entry.Workspace.Label != "ROT Auto Timeline" {
		t.Errorf("recorded label = %q, want herdr's current label", result.Entry.Workspace.Label)
	}
}

// The derived label is only a fallback: a workspace whose label matches is
// adopted rather than duplicated, which is what makes a lost ID recoverable.
func TestDerivedLabelAdoptsAMatchingWorkspace(t *testing.T) {
	f := flowexec.NewFake().
		Respond("herdr workspace list",
			`{"result":{"workspaces":[{"workspace_id":"w7","label":"RD-1"}]}}`).
		Respond("herdr workspace focus", `{"result":{"type":"ok"}}`)

	var errOut strings.Builder
	app := appWithHerdr(f, &errOut)
	result := initResult{Entry: registry.Entry{
		TicketID:  "RD-1",
		Workspace: registry.Workspace{Provider: "herdr", Label: "RD-1"},
	}}
	app.ensureWorkspace(t.Context(), &result, names{WorkspaceLabel: "RD-1"}, false)

	if f.Ran("herdr workspace create") {
		t.Errorf("a label match must adopt, not create: %v", f.CommandLines())
	}
	// Adoption is what heals an entry that never recorded an ID.
	if result.Entry.Workspace.ID != "w7" {
		t.Errorf("recorded ID = %q, want w7", result.Entry.Workspace.ID)
	}
	if errOut.Len() != 0 {
		t.Errorf("a derived label must not warn: %q", errOut.String())
	}
}
