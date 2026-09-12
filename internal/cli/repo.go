package cli

import (
	"context"

	"github.com/idosaban-scaleops/flow/internal/gitx"
)

// repoFor returns the repository a resolved ticket belongs to. The resolution
// may have happened outside any repository (`flow pr RD-1` from home), in which
// case the repo is re-derived from the registry entry's recorded root.
func (a *App) repoFor(got resolved) (gitx.Repo, error) {
	if got.Repo.Root != "" && (got.Entry.RepoRoot == "" || gitx.SamePath(got.Repo.Root, got.Entry.RepoRoot)) {
		return got.Repo, nil
	}
	if got.Entry.RepoRoot == "" {
		return gitx.Repo{}, NotFound("no repository recorded for %s", got.Entry.TicketID)
	}

	repo := gitx.Repo{Root: got.Entry.RepoRoot, Key: got.Entry.RepoKey}
	if owner, name, ok := splitRepoKey(got.Entry.RepoKey); ok {
		repo.Owner, repo.Name = owner, name
	}
	return repo, nil
}

// discoverRepoFor re-reads the repository from disk, for the commands that need
// the live origin URL rather than the recorded key.
func (a *App) discoverRepoFor(ctx context.Context, got resolved) (gitx.Repo, error) {
	root := got.Entry.RepoRoot
	if root == "" {
		root = got.Repo.Root
	}
	if root == "" {
		return a.repoFor(got)
	}
	repo, err := gitx.DiscoverRepo(ctx, a.baseRunner, root, a.cfg.Defaults.Remote)
	if err != nil {
		return a.repoFor(got)
	}
	return repo, nil
}

// splitRepoKey parses an "owner/name" registry key.
func splitRepoKey(key string) (owner, name string, ok bool) {
	for i := range key {
		if key[i] != '/' {
			continue
		}
		owner, name = key[:i], key[i+1:]
		// An absolute path key also contains slashes; owner/name never starts
		// with one and never contains a second.
		if owner == "" || name == "" {
			return "", "", false
		}
		for j := range name {
			if name[j] == '/' {
				return "", "", false
			}
		}
		return owner, name, true
	}
	return "", "", false
}
