// Package gitx is flow's git plumbing. Every operation is expressed as an
// explicit argv run through exec.Runner, which is what lets the argv assertions
// in the tests guarantee flow runs the command the user expects.
package gitx

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	flowexec "github.com/idosaban-scaleops/flow/internal/exec"
)

// Git runs git commands rooted at a directory.
type Git struct {
	Runner flowexec.Runner
	// Dir is the working directory for every invocation.
	Dir string
}

// New returns a Git bound to dir.
func New(r flowexec.Runner, dir string) *Git { return &Git{Runner: r, Dir: dir} }

// At returns a copy of g rooted at a different directory.
func (g *Git) At(dir string) *Git { return &Git{Runner: g.Runner, Dir: dir} }

func (g *Git) run(ctx context.Context, args ...string) (flowexec.Result, error) {
	return g.Runner.Run(ctx, flowexec.Opts{Name: "git", Args: args, Dir: g.Dir})
}

// out runs git and returns trimmed stdout.
func (g *Git) out(ctx context.Context, args ...string) (string, error) {
	res, err := g.run(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// ok runs git and reports whether it exited zero, discarding the error. Used
// for the "does this ref exist" probes, where a non-zero exit is the answer.
func (g *Git) ok(ctx context.Context, args ...string) bool {
	_, err := g.run(ctx, args...)
	return err == nil
}

// Repo identifies a git repository flow manages worktrees for.
type Repo struct {
	// Root is the main worktree's root, never a linked worktree's.
	Root string
	// Key is "owner/name" from origin, or the absolute root path as a fallback.
	Key string
	// Owner and Name are set only when origin is a GitHub remote.
	Owner string
	Name  string
	// OriginURL is the raw remote URL, for diagnostics.
	OriginURL string
}

// IsGitHub reports whether GitHub-dependent features can work for this repo.
func (r Repo) IsGitHub() bool { return r.Owner != "" && r.Name != "" }

// DiscoverRepo resolves the canonical repository containing dir.
//
// It deliberately does not use `rev-parse --show-toplevel`: inside a linked
// worktree that returns the worktree path, not the repository. The common git
// dir's parent is the main worktree's root in both cases.
func DiscoverRepo(ctx context.Context, r flowexec.Runner, dir, remote string) (Repo, error) {
	g := New(r, dir)

	common, err := g.out(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return Repo{}, fmt.Errorf("%s is not inside a git repository: %w", dir, err)
	}

	root := filepath.Dir(filepath.Clean(common))
	repo := Repo{Root: root, Key: root}

	origin, err := g.At(root).out(ctx, "remote", "get-url", remote)
	if err != nil {
		// A repo with no origin remote is legal. GitHub-dependent features
		// degrade with a clear message rather than failing here.
		return repo, nil //nolint:nilerr // absence of a remote is not an error
	}
	repo.OriginURL = origin

	if owner, name, ok := ParseGitHubRemote(origin); ok {
		repo.Owner, repo.Name = owner, name
		repo.Key = owner + "/" + name
	}
	return repo, nil
}

// GitCommonDir returns the shared .git directory for the repository, which is
// where info/exclude lives even when called from a linked worktree.
func (g *Git) GitCommonDir(ctx context.Context) (string, error) {
	return g.out(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
}

// --- refs ------------------------------------------------------------------

// LocalBranchExists reports whether refs/heads/{branch} exists.
func (g *Git) LocalBranchExists(ctx context.Context, branch string) bool {
	return g.ok(ctx, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
}

// RemoteBranchExists reports whether refs/remotes/{remote}/{branch} exists.
func (g *Git) RemoteBranchExists(ctx context.Context, remote, branch string) bool {
	return g.ok(ctx, "show-ref", "--verify", "--quiet", "refs/remotes/"+remote+"/"+branch)
}

// CurrentBranch returns the checked-out branch, or "" when detached.
func (g *Git) CurrentBranch(ctx context.Context) (string, error) {
	out, err := g.out(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	if out == "HEAD" {
		return "", nil
	}
	return out, nil
}

// Fetch fetches refs from a remote. Callers treat failure as a warning.
func (g *Git) Fetch(ctx context.Context, remote string, refs ...string) error {
	args := append([]string{"fetch", remote}, refs...)
	_, err := g.run(ctx, args...)
	return err
}

// FetchTags fetches tags, which chart-version derivation depends on.
func (g *Git) FetchTags(ctx context.Context, remote string) error {
	_, err := g.run(ctx, "fetch", "--tags", remote)
	return err
}

// LatestTag returns the most recently created tag, which is what the CI's
// chart-version computation starts from.
func (g *Git) LatestTag(ctx context.Context) (string, error) {
	out, err := g.out(ctx, "for-each-ref", "--sort=-creatordate", "--count", "1",
		"--format=%(refname:short)", "refs/tags/*")
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", fmt.Errorf("repository has no tags")
	}
	return out, nil
}

// IsMergedInto reports whether branch is reachable from ref. This is a useful
// cross-check before deleting a branch, but it misses squash merges, so it must
// never be the sole basis for deleting anything.
func (g *Git) IsMergedInto(ctx context.Context, branch, ref string) bool {
	out, err := g.out(ctx, "branch", "--merged", ref, "--format=%(refname:short)")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == branch {
			return true
		}
	}
	return false
}

// DeleteBranch removes a local branch. With force it uses -D, which is only
// safe once GitHub has confirmed the PR merged (a squash merge leaves the
// branch looking unmerged to git).
func (g *Git) DeleteBranch(ctx context.Context, branch string, force bool) error {
	flag := "-d"
	if force {
		flag = "-D"
	}
	_, err := g.run(ctx, "branch", flag, branch)
	return err
}

// --- working tree state ----------------------------------------------------

// Status summarizes a worktree's state relative to its upstream.
type Status struct {
	// Dirty reports uncommitted changes.
	Dirty bool
	// ChangedFiles counts the porcelain entries.
	ChangedFiles int
	// HasUpstream reports whether the branch tracks a remote branch.
	HasUpstream bool
	Ahead       int
	Behind      int
}

// Clean reports a worktree with no local divergence at all.
func (s Status) Clean() bool { return !s.Dirty && s.Ahead == 0 && s.Behind == 0 }

// StatusOf inspects the worktree rooted at g.Dir.
func (g *Git) StatusOf(ctx context.Context) (Status, error) {
	var st Status

	porcelain, err := g.out(ctx, "status", "--porcelain")
	if err != nil {
		return st, err
	}
	if porcelain != "" {
		st.Dirty = true
		st.ChangedFiles = len(strings.Split(porcelain, "\n"))
	}

	counts, err := g.out(ctx, "rev-list", "--left-right", "--count", "@{upstream}...HEAD")
	if err != nil {
		// No upstream configured is a normal state for a branch that has never
		// been pushed; it just means there is no ahead/behind to report.
		return st, nil //nolint:nilerr // no upstream is not an error
	}
	st.HasUpstream = true
	st.Behind, st.Ahead = parseLeftRight(counts)
	return st, nil
}

// parseLeftRight reads the two counts `rev-list --left-right --count` prints.
func parseLeftRight(s string) (left, right int) {
	fields := strings.Fields(s)
	if len(fields) != 2 {
		return 0, 0
	}
	left, _ = strconv.Atoi(fields[0])
	right, _ = strconv.Atoi(fields[1])
	return left, right
}

// UnpushedCommit is one commit present locally but not on the remote.
type UnpushedCommit struct {
	SHA     string
	Subject string
}

// Unpushed lists commits on branch that the remote does not have. A branch
// that was never pushed reports every commit since the base branch.
func (g *Git) Unpushed(ctx context.Context, remote, branch, baseBranch string) ([]UnpushedCommit, error) {
	rangeSpec := remote + "/" + branch + ".." + branch
	if !g.RemoteBranchExists(ctx, remote, branch) {
		base := remote + "/" + baseBranch
		if !g.RemoteBranchExists(ctx, remote, baseBranch) {
			base = baseBranch
		}
		rangeSpec = base + ".." + branch
	}

	out, err := g.out(ctx, "log", "--format=%h %s", rangeSpec)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}

	var commits []UnpushedCommit
	for _, line := range strings.Split(out, "\n") {
		sha, subject, _ := strings.Cut(line, " ")
		commits = append(commits, UnpushedCommit{SHA: sha, Subject: subject})
	}
	return commits, nil
}
