package gitx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Worktree is one entry from `git worktree list --porcelain`.
type Worktree struct {
	Path   string
	Branch string
	Head   string
	// Prunable reports a registered worktree whose directory is gone.
	Prunable bool
	Detached bool
	Bare     bool
}

// ListWorktrees returns every worktree git knows about for this repository.
func (g *Git) ListWorktrees(ctx context.Context) ([]Worktree, error) {
	out, err := g.out(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktreeList(out), nil
}

// parseWorktreeList reads git's porcelain worktree format: blank-line-separated
// records of "key value" lines.
func parseWorktreeList(out string) []Worktree {
	var (
		list    []Worktree
		current *Worktree
	)
	flush := func() {
		if current != nil {
			list = append(list, *current)
			current = nil
		}
	}

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			flush()
			continue
		}
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "worktree":
			flush()
			current = &Worktree{Path: filepath.Clean(value)}
		case "HEAD":
			if current != nil {
				current.Head = value
			}
		case "branch":
			if current != nil {
				current.Branch = strings.TrimPrefix(value, "refs/heads/")
			}
		case "detached":
			if current != nil {
				current.Detached = true
			}
		case "bare":
			if current != nil {
				current.Bare = true
			}
		case "prunable":
			if current != nil {
				current.Prunable = true
			}
		}
	}
	flush()
	return list
}

// FindWorktree returns the worktree registered at path.
func (g *Git) FindWorktree(ctx context.Context, path string) (Worktree, bool, error) {
	list, err := g.ListWorktrees(ctx)
	if err != nil {
		return Worktree{}, false, err
	}
	for _, wt := range list {
		if SamePath(wt.Path, path) {
			return wt, true, nil
		}
	}
	return Worktree{}, false, nil
}

// FindWorktreeForBranch reports where a branch is already checked out. git
// refuses a second checkout of the same branch, so callers must ask first.
func (g *Git) FindWorktreeForBranch(ctx context.Context, branch string) (Worktree, bool, error) {
	list, err := g.ListWorktrees(ctx)
	if err != nil {
		return Worktree{}, false, err
	}
	for _, wt := range list {
		if wt.Branch == branch {
			return wt, true, nil
		}
	}
	return Worktree{}, false, nil
}

// AddWorktreeExisting checks out an existing local branch into a new worktree.
func (g *Git) AddWorktreeExisting(ctx context.Context, path, branch string) error {
	_, err := g.run(ctx, "worktree", "add", path, branch)
	return err
}

// AddWorktreeTracking creates a branch tracking {remote}/{branch}.
func (g *Git) AddWorktreeTracking(ctx context.Context, path, branch, remote string) error {
	_, err := g.run(ctx, "worktree", "add", "--track", "-b", branch, path, remote+"/"+branch)
	return err
}

// AddWorktreeFromBase creates a new branch off an arbitrary start point.
func (g *Git) AddWorktreeFromBase(ctx context.Context, path, branch, startPoint string) error {
	_, err := g.run(ctx, "worktree", "add", "-b", branch, path, startPoint)
	return err
}

// RemoveWorktree removes a worktree, optionally discarding local changes.
func (g *Git) RemoveWorktree(ctx context.Context, path string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	_, err := g.run(ctx, args...)
	return err
}

// PruneWorktrees drops registrations whose directories no longer exist.
func (g *Git) PruneWorktrees(ctx context.Context) error {
	_, err := g.run(ctx, "worktree", "prune")
	return err
}

// ExcludeLine is the entry flow adds to info/exclude so the worktrees
// directory never shows up as an untracked path.
const ExcludeLine = ".worktrees/"

// EnsureExcluded appends line to {git_common_dir}/info/exclude unless it is
// already present as an exact line, creating the file if needed.
func EnsureExcluded(commonDir, line string) (added bool, err error) {
	// 0755 matches the permissions git itself uses inside .git.
	infoDir := filepath.Join(commonDir, "info")
	if err := os.MkdirAll(infoDir, 0o755); err != nil { //nolint:gosec // matches git's own permissions
		return false, fmt.Errorf("create %s: %w", infoDir, err)
	}

	path := filepath.Join(infoDir, "exclude")
	existing, err := os.ReadFile(path) //nolint:gosec // path is inside the repo's own .git
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("read %s: %w", path, err)
	}

	for _, existingLine := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(existingLine) == line {
			return false, nil
		}
	}

	body := string(existing)
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += line + "\n"

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil { //nolint:gosec // matches git
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	return true, nil
}
