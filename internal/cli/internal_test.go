package cli

import (
	"strings"
	"testing"
	"time"

	flowexec "github.com/idosaban-scaleops/flow/internal/exec"
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
