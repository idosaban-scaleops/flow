//go:build integration

// These tests drive a real git binary against real temporary repositories.
// They are behind a build tag (and skip under -short) so `go test -short ./...`
// stays fast and works on a runner with no git identity configured.
package gitx_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	flowexec "github.com/idosaban-scaleops/flow/internal/exec"
	"github.com/idosaban-scaleops/flow/internal/gitx"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=flow", "GIT_AUTHOR_EMAIL=flow@example.com",
		"GIT_COMMITTER_NAME=flow", "GIT_COMMITTER_EMAIL=flow@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// newRepo builds a repository with one commit, one tag, and a local "origin"
// pointing at a second bare repository.
func newRepo(t *testing.T) (root string) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	root = filepath.Join(base, "work")

	git(t, base, "init", "--bare", "--initial-branch=main", origin)
	git(t, base, "init", "--initial-branch=main", root)

	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", ".")
	git(t, root, "commit", "-m", "initial")
	git(t, root, "tag", "v1.0.199")
	git(t, root, "remote", "add", "origin", origin)
	git(t, root, "push", "-u", "origin", "main")
	git(t, root, "push", "--tags", "origin")
	return root
}

func TestIntegrationDiscoverRepoFromLinkedWorktree(t *testing.T) {
	root := newRepo(t)
	ctx := context.Background()
	runner := &flowexec.Real{}

	wt := filepath.Join(root, ".worktrees", "RD-1-toolbar")
	git(t, root, "worktree", "add", "-b", "RD-1-toolbar", wt, "main")

	// From the main checkout.
	fromMain, err := gitx.DiscoverRepo(ctx, runner, root, "origin")
	if err != nil {
		t.Fatal(err)
	}
	// From inside the linked worktree: this is where --show-toplevel lies.
	fromWorktree, err := gitx.DiscoverRepo(ctx, runner, wt, "origin")
	if err != nil {
		t.Fatal(err)
	}

	wantRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]gitx.Repo{"main": fromMain, "worktree": fromWorktree} {
		gotRoot, err := filepath.EvalSymlinks(got.Root)
		if err != nil {
			t.Fatal(err)
		}
		if gotRoot != wantRoot {
			t.Errorf("from %s: Root = %q, want %q", name, gotRoot, wantRoot)
		}
	}
}

func TestIntegrationWorktreeLifecycle(t *testing.T) {
	root := newRepo(t)
	ctx := context.Background()
	g := gitx.New(&flowexec.Real{}, root)

	wt := filepath.Join(root, ".worktrees", "RD-1-toolbar")
	if err := g.AddWorktreeFromBase(ctx, wt, "RD-1-toolbar", "main"); err != nil {
		t.Fatal(err)
	}

	found, ok, err := g.FindWorktree(ctx, wt)
	if err != nil || !ok {
		t.Fatalf("worktree not listed after creation (ok=%v err=%v)", ok, err)
	}
	if found.Branch != "RD-1-toolbar" {
		t.Errorf("branch = %q", found.Branch)
	}

	byBranch, ok, err := g.FindWorktreeForBranch(ctx, "RD-1-toolbar")
	if err != nil || !ok {
		t.Fatalf("FindWorktreeForBranch failed (ok=%v err=%v)", ok, err)
	}
	if !gitx.SamePath(byBranch.Path, found.Path) {
		t.Errorf("FindWorktreeForBranch = %q, want %q", byBranch.Path, found.Path)
	}

	if !g.LocalBranchExists(ctx, "RD-1-toolbar") {
		t.Error("branch should exist locally")
	}

	if err := g.RemoveWorktree(ctx, wt, false); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := g.FindWorktree(ctx, wt); ok {
		t.Error("worktree still listed after removal")
	}
	if err := g.DeleteBranch(ctx, "RD-1-toolbar", true); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationPruneAfterManualDeletion(t *testing.T) {
	root := newRepo(t)
	ctx := context.Background()
	g := gitx.New(&flowexec.Real{}, root)

	wt := filepath.Join(root, ".worktrees", "RD-2-gone")
	if err := g.AddWorktreeFromBase(ctx, wt, "RD-2-gone", "main"); err != nil {
		t.Fatal(err)
	}
	// Simulate the user deleting the directory by hand, which is exactly the
	// drift the registry and `flow prune` must tolerate.
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}

	list, err := g.ListWorktrees(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var sawPrunable bool
	for _, w := range list {
		if gitx.SamePath(w.Path, wt) && w.Prunable {
			sawPrunable = true
		}
	}
	if !sawPrunable {
		t.Error("a worktree whose directory was deleted should be reported prunable")
	}

	if err := g.PruneWorktrees(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := g.FindWorktree(ctx, wt); ok {
		t.Error("prune should have dropped the stale registration")
	}
}

func TestIntegrationStatusAndUnpushed(t *testing.T) {
	root := newRepo(t)
	ctx := context.Background()
	runner := &flowexec.Real{}
	g := gitx.New(runner, root)

	wt := filepath.Join(root, ".worktrees", "RD-3-work")
	if err := g.AddWorktreeFromBase(ctx, wt, "RD-3-work", "main"); err != nil {
		t.Fatal(err)
	}
	wg := gitx.New(runner, wt)

	st, err := wg.StatusOf(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Dirty {
		t.Error("a fresh worktree should be clean")
	}

	if err := os.WriteFile(filepath.Join(wt, "new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err = wg.StatusOf(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Dirty || st.ChangedFiles != 1 {
		t.Errorf("expected one untracked file, got %+v", st)
	}

	git(t, wt, "add", ".")
	git(t, wt, "commit", "-m", "add new.txt")

	unpushed, err := wg.Unpushed(ctx, "origin", "RD-3-work", "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(unpushed) != 1 || unpushed[0].Subject != "add new.txt" {
		t.Errorf("Unpushed = %+v, want the single unpushed commit", unpushed)
	}
}

func TestIntegrationLatestTagAndMergeCheck(t *testing.T) {
	root := newRepo(t)
	ctx := context.Background()
	g := gitx.New(&flowexec.Real{}, root)

	tag, err := g.LatestTag(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tag != "v1.0.199" {
		t.Errorf("LatestTag = %q, want v1.0.199", tag)
	}

	git(t, root, "branch", "RD-4-merged", "main")
	if !g.IsMergedInto(ctx, "RD-4-merged", "main") {
		t.Error("a branch pointing at main must read as merged")
	}
}

func TestIntegrationEnsureExcluded(t *testing.T) {
	root := newRepo(t)
	ctx := context.Background()
	g := gitx.New(&flowexec.Real{}, root)

	commonDir, err := g.GitCommonDir(ctx)
	if err != nil {
		t.Fatal(err)
	}

	added, err := gitx.EnsureExcluded(commonDir, gitx.ExcludeLine)
	if err != nil {
		t.Fatal(err)
	}
	if !added {
		t.Error("first call should add the exclude line")
	}

	added, err = gitx.EnsureExcluded(commonDir, gitx.ExcludeLine)
	if err != nil {
		t.Fatal(err)
	}
	if added {
		t.Error("second call must be a no-op, not a duplicate line")
	}

	body, err := os.ReadFile(filepath.Join(commonDir, "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for _, line := range splitLines(string(body)) {
		if line == gitx.ExcludeLine {
			count++
		}
	}
	if count != 1 {
		t.Errorf("exclude line appears %d times, want exactly 1", count)
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
