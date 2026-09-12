package gitx_test

import (
	"context"
	"strings"
	"testing"

	flowexec "github.com/idosaban-scaleops/flow/internal/exec"
	"github.com/idosaban-scaleops/flow/internal/gitx"
)

// wantArgv is the core assertion of this package: flow must run exactly the
// command the user would have typed.
func wantArgv(t *testing.T, f *flowexec.Fake, want string) {
	t.Helper()
	for _, got := range f.CommandLines() {
		if got == want {
			return
		}
	}
	t.Errorf("expected command\n  %s\ngot\n  %s", want, strings.Join(f.CommandLines(), "\n  "))
}

func TestDiscoverRepoUsesCommonDirNotToplevel(t *testing.T) {
	// A linked worktree: --show-toplevel would return the worktree path, which
	// is the bug this test exists to prevent.
	f := flowexec.NewFake().
		Respond("git rev-parse --path-format=absolute --git-common-dir", "/repo/.git\n").
		Respond("git remote get-url origin", "git@github.com:scaleops-sh/scaleops.git\n")

	repo, err := gitx.DiscoverRepo(context.Background(), f,
		"/repo/.worktrees/RD-1-toolbar", "origin")
	if err != nil {
		t.Fatal(err)
	}

	if repo.Root != "/repo" {
		t.Errorf("Root = %q, want /repo (the main worktree, not the linked one)", repo.Root)
	}
	if repo.Key != "scaleops-sh/scaleops" {
		t.Errorf("Key = %q", repo.Key)
	}
	if repo.Owner != "scaleops-sh" || repo.Name != "scaleops" {
		t.Errorf("owner/name = %q/%q", repo.Owner, repo.Name)
	}
	wantArgv(t, f, "git rev-parse --path-format=absolute --git-common-dir")
}

func TestDiscoverRepoMainCheckout(t *testing.T) {
	f := flowexec.NewFake().
		Respond("git rev-parse --path-format=absolute --git-common-dir", "/repo/.git\n").
		Respond("git remote get-url origin", "https://github.com/o/r\n")

	repo, err := gitx.DiscoverRepo(context.Background(), f, "/repo/pkg/api", "origin")
	if err != nil {
		t.Fatal(err)
	}
	if repo.Root != "/repo" {
		t.Errorf("Root = %q, want /repo", repo.Root)
	}
	if !repo.IsGitHub() {
		t.Error("repo should be recognized as GitHub-backed")
	}
}

func TestDiscoverRepoWithoutOriginFallsBackToPath(t *testing.T) {
	f := flowexec.NewFake().
		Respond("git rev-parse --path-format=absolute --git-common-dir", "/repo/.git\n").
		RespondWith("git remote get-url origin", flowexec.Response{ExitCode: 2, Stderr: "error: No such remote"})

	repo, err := gitx.DiscoverRepo(context.Background(), f, "/repo", "origin")
	if err != nil {
		t.Fatalf("a repo without an origin remote is legal: %v", err)
	}
	if repo.Key != "/repo" {
		t.Errorf("Key = %q, want the repo root as a fallback", repo.Key)
	}
	if repo.IsGitHub() {
		t.Error("a repo without origin must not claim to be GitHub-backed")
	}
}

func TestDiscoverRepoNonGitHubRemote(t *testing.T) {
	f := flowexec.NewFake().
		Respond("git rev-parse --path-format=absolute --git-common-dir", "/repo/.git\n").
		Respond("git remote get-url origin", "git@gitlab.com:o/r.git\n")

	repo, err := gitx.DiscoverRepo(context.Background(), f, "/repo", "origin")
	if err != nil {
		t.Fatal(err)
	}
	if repo.IsGitHub() {
		t.Error("a GitLab remote must not be treated as GitHub")
	}
	if repo.Key != "/repo" {
		t.Errorf("Key = %q, want the repo root for a non-GitHub remote", repo.Key)
	}
}

func TestDiscoverRepoOutsideRepository(t *testing.T) {
	f := flowexec.NewFake()
	f.RespondWith("git rev-parse", flowexec.Response{
		ExitCode: 128,
		Stderr:   "fatal: not a git repository (or any of the parent directories): .git",
	})
	if _, err := gitx.DiscoverRepo(context.Background(), f, "/tmp", "origin"); err == nil {
		t.Fatal("running outside a git repository must fail")
	}
}

func TestWorktreeArgv(t *testing.T) {
	tests := []struct {
		name string
		call func(*gitx.Git) error
		want string
	}{
		{
			name: "existing local branch",
			call: func(g *gitx.Git) error {
				return g.AddWorktreeExisting(context.Background(), "/repo/.worktrees/RD-1-a", "RD-1-a")
			},
			want: "git worktree add /repo/.worktrees/RD-1-a RD-1-a",
		},
		{
			name: "tracking a remote branch",
			call: func(g *gitx.Git) error {
				return g.AddWorktreeTracking(context.Background(), "/repo/.worktrees/RD-1-a", "RD-1-a", "origin")
			},
			want: "git worktree add --track -b RD-1-a /repo/.worktrees/RD-1-a origin/RD-1-a",
		},
		{
			name: "branching from a base",
			call: func(g *gitx.Git) error {
				return g.AddWorktreeFromBase(context.Background(), "/repo/.worktrees/RD-1-a", "RD-1-a", "origin/main")
			},
			want: "git worktree add -b RD-1-a /repo/.worktrees/RD-1-a origin/main",
		},
		{
			name: "remove",
			call: func(g *gitx.Git) error {
				return g.RemoveWorktree(context.Background(), "/repo/.worktrees/RD-1-a", false)
			},
			want: "git worktree remove /repo/.worktrees/RD-1-a",
		},
		{
			name: "force remove",
			call: func(g *gitx.Git) error {
				return g.RemoveWorktree(context.Background(), "/repo/.worktrees/RD-1-a", true)
			},
			want: "git worktree remove --force /repo/.worktrees/RD-1-a",
		},
		{
			name: "prune",
			call: func(g *gitx.Git) error { return g.PruneWorktrees(context.Background()) },
			want: "git worktree prune",
		},
		{
			name: "delete branch safely",
			call: func(g *gitx.Git) error {
				return g.DeleteBranch(context.Background(), "RD-1-a", false)
			},
			want: "git branch -d RD-1-a",
		},
		{
			name: "delete branch forcibly",
			call: func(g *gitx.Git) error {
				return g.DeleteBranch(context.Background(), "RD-1-a", true)
			},
			want: "git branch -D RD-1-a",
		},
		{
			name: "fetch tags",
			call: func(g *gitx.Git) error { return g.FetchTags(context.Background(), "origin") },
			want: "git fetch --tags origin",
		},
		{
			name: "fetch base branch",
			call: func(g *gitx.Git) error { return g.Fetch(context.Background(), "origin", "main") },
			want: "git fetch origin main",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := flowexec.NewFake()
			if err := tt.call(gitx.New(f, "/repo")); err != nil {
				t.Fatal(err)
			}
			wantArgv(t, f, tt.want)
		})
	}
}

func TestListWorktreesParsesPorcelain(t *testing.T) {
	out := strings.Join([]string{
		"worktree /repo",
		"HEAD abc123",
		"branch refs/heads/main",
		"",
		"worktree /repo/.worktrees/RD-1-a",
		"HEAD def456",
		"branch refs/heads/RD-1-a",
		"",
		"worktree /repo/.worktrees/RD-2-b",
		"HEAD 000000",
		"detached",
		"",
		"worktree /repo/.worktrees/RD-3-gone",
		"HEAD 111111",
		"branch refs/heads/RD-3-gone",
		"prunable gitdir file points to non-existent location",
		"",
	}, "\n")

	f := flowexec.NewFake().Respond("git worktree list --porcelain", out)
	list, err := gitx.New(f, "/repo").ListWorktrees(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if len(list) != 4 {
		t.Fatalf("want 4 worktrees, got %d: %+v", len(list), list)
	}
	if list[1].Path != "/repo/.worktrees/RD-1-a" || list[1].Branch != "RD-1-a" {
		t.Errorf("linked worktree parsed as %+v", list[1])
	}
	if !list[2].Detached {
		t.Error("detached worktree not flagged")
	}
	if !list[3].Prunable {
		t.Error("prunable worktree not flagged")
	}
}

func TestStatusOfParsesAheadBehind(t *testing.T) {
	tests := []struct {
		name        string
		porcelain   string
		revList     string
		revListFail bool
		want        gitx.Status
	}{
		{
			name: "clean and in sync",
			want: gitx.Status{HasUpstream: true},
		},
		{
			name:      "dirty",
			porcelain: " M pkg/a.go\n?? new.go",
			want:      gitx.Status{Dirty: true, ChangedFiles: 2, HasUpstream: true},
		},
		{
			name:    "ahead two",
			revList: "0\t2",
			want:    gitx.Status{Ahead: 2, HasUpstream: true},
		},
		{
			name:    "behind three",
			revList: "3\t0",
			want:    gitx.Status{Behind: 3, HasUpstream: true},
		},
		{
			name:      "diverged and dirty",
			porcelain: " M a",
			revList:   "1\t4",
			want:      gitx.Status{Dirty: true, ChangedFiles: 1, Behind: 1, Ahead: 4, HasUpstream: true},
		},
		{
			name:        "no upstream",
			revListFail: true,
			want:        gitx.Status{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := flowexec.NewFake().Respond("git status --porcelain", tt.porcelain)
			if tt.revListFail {
				f.RespondWith("git rev-list", flowexec.Response{
					ExitCode: 128, Stderr: "fatal: no upstream configured",
				})
			} else {
				f.Respond("git rev-list", tt.revList)
			}

			got, err := gitx.New(f, "/wt").StatusOf(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("StatusOf = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestUnpushedUsesRemoteRangeWhenPushed(t *testing.T) {
	f := flowexec.NewFake().
		Respond("git show-ref --verify --quiet refs/remotes/origin/RD-1-a", "").
		Respond("git log --format=%h %s origin/RD-1-a..RD-1-a", "abc123 first\ndef456 second")

	got, err := gitx.New(f, "/wt").Unpushed(context.Background(), "origin", "RD-1-a", "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Subject != "first" {
		t.Errorf("Unpushed = %+v", got)
	}
	wantArgv(t, f, "git log '--format=%h %s' origin/RD-1-a..RD-1-a")
}

func TestUnpushedFallsBackToBaseWhenNeverPushed(t *testing.T) {
	f := flowexec.NewFake()
	f.RespondWith("git show-ref --verify --quiet refs/remotes/origin/RD-1-a",
		flowexec.Response{ExitCode: 1})
	f.Respond("git show-ref --verify --quiet refs/remotes/origin/main", "")
	f.Respond("git log", "abc123 only commit")

	got, err := gitx.New(f, "/wt").Unpushed(context.Background(), "origin", "RD-1-a", "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 unpushed commit, got %d", len(got))
	}
	wantArgv(t, f, "git log '--format=%h %s' origin/main..RD-1-a")
}

func TestIsMergedInto(t *testing.T) {
	f := flowexec.NewFake().
		Respond("git branch --merged origin/main", "main\nRD-1-a\nRD-2-b")

	g := gitx.New(f, "/repo")
	if !g.IsMergedInto(context.Background(), "RD-1-a", "origin/main") {
		t.Error("RD-1-a should be reported as merged")
	}
	if g.IsMergedInto(context.Background(), "RD-9-z", "origin/main") {
		t.Error("RD-9-z should not be reported as merged")
	}
}

func TestLatestTag(t *testing.T) {
	f := flowexec.NewFake().Respond("git for-each-ref", "v1.0.199\n")
	got, err := gitx.New(f, "/repo").LatestTag(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "v1.0.199" {
		t.Errorf("LatestTag = %q", got)
	}
	wantArgv(t, f, "git for-each-ref --sort=-creatordate --count 1 '--format=%(refname:short)' 'refs/tags/*'")
}

func TestLatestTagWithNoTags(t *testing.T) {
	f := flowexec.NewFake().Respond("git for-each-ref", "")
	if _, err := gitx.New(f, "/repo").LatestTag(context.Background()); err == nil {
		t.Fatal("a repository with no tags must report an error, not an empty tag")
	}
}
