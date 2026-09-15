package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/idosaban-scaleops/flow/internal/config"
	"github.com/idosaban-scaleops/flow/internal/gitx"
	"github.com/idosaban-scaleops/flow/internal/registry"
	"github.com/idosaban-scaleops/flow/internal/ticket"
)

// names holds every path and label derived from a ticket, so the underscore /
// hyphen convention is computed in exactly one place.
type names struct {
	Ticket         ticket.Ticket
	Branch         string
	WorktreePath   string
	AssetsPath     string
	WorkspaceLabel string
	// LabelExplicit reports that WorkspaceLabel came from --label rather than
	// from the naming template, which decides whether a mismatch is worth a
	// warning.
	LabelExplicit bool
}

// deriveNames applies the configured naming templates.
func deriveNames(cfg config.Config, repoRoot string, tk ticket.Ticket) names {
	dirName := tk.Expand(cfg.Naming.Branch)
	return names{
		Ticket:         tk,
		Branch:         dirName,
		WorktreePath:   filepath.Join(repoRoot, cfg.Naming.WorktreesSubdir, dirName),
		AssetsPath:     filepath.Join(cfg.AssetsRoot(), tk.Expand(cfg.Naming.AssetsDir)),
		WorkspaceLabel: tk.Expand(cfg.Naming.WorkspaceLabel),
	}
}

// resolved is a ticket plus everything flow knows about where it lives.
type resolved struct {
	Entry registry.Entry
	Repo  gitx.Repo
	// Registered reports whether the entry came from the registry rather than
	// being reconstructed from the directory layout.
	Registered bool
}

// resolveTicket finds the ticket a command should act on: the one named on the
// command line, or the one the current directory belongs to.
func (a *App) resolveTicket(ctx context.Context, arg string) (resolved, error) {
	dir, err := cwd()
	if err != nil {
		return resolved{}, err
	}

	// Repo discovery is best-effort: `flow status RD-1` should work from
	// anywhere, using whatever the registry recorded.
	repo, repoErr := gitx.DiscoverRepo(ctx, a.baseRunner, dir, a.cfg.Defaults.Remote)

	file, err := a.Registry.Load()
	if err != nil {
		return resolved{}, Wrap(ExitFailure, "registry", err, "reading the registry")
	}

	if arg != "" {
		return a.resolveByID(file, repo, repoErr == nil, arg)
	}
	return a.inferFromDir(ctx, file, repo, repoErr == nil, dir)
}

func (a *App) resolveByID(file *registry.File, repo gitx.Repo, haveRepo bool, arg string) (resolved, error) {
	id, err := ticket.NormalizeID(arg)
	if err != nil {
		return resolved{}, Wrap(ExitUsage, "usage", err, "")
	}

	if haveRepo {
		if entry, ok := file.Find(repo.Key, id); ok {
			return resolved{Entry: entry, Repo: repo, Registered: true}, nil
		}
	}

	entry, ok, ambiguous := file.FindAnyRepo(id)
	if !ok {
		return resolved{}, NotFound("ticket %s is not registered; run `flow init %s <description>` first", id, id)
	}
	if ambiguous && haveRepo {
		a.Out.Warn("%s exists in more than one repository; using %s", id, entry.RepoKey)
	}
	return resolved{Entry: entry, Repo: repo, Registered: true}, nil
}

// inferFromDir implements the cwd inference ladder: registry first, then the
// .worktrees directory layout.
func (a *App) inferFromDir(ctx context.Context, file *registry.File, repo gitx.Repo, haveRepo bool, dir string) (resolved, error) {
	if entry, ok, ambiguous := file.MatchByPath(dir); ok {
		if ambiguous {
			a.Out.Log.Debug("multiple registry entries match the current directory; using the deepest",
				"ticket", entry.TicketID)
		}
		return resolved{Entry: entry, Repo: repo, Registered: true}, nil
	}

	if tk, wtPath, ok := inferFromWorktreeLayout(dir, a.cfg.Naming.WorktreesSubdir); ok {
		entry := registry.Entry{
			TicketID:     tk.ID,
			Slug:         tk.Slug,
			WorktreePath: wtPath,
			Branch:       tk.Expand(a.cfg.Naming.Branch),
			BaseBranch:   a.cfg.Defaults.BaseBranch,
			AssetsPath:   filepath.Join(a.cfg.AssetsRoot(), tk.Expand(a.cfg.Naming.AssetsDir)),
		}
		if haveRepo {
			entry.RepoKey, entry.RepoRoot = repo.Key, repo.Root
			entry.BaseBranch = a.cfg.BaseBranchFor(repo.Key)
			// The directory name is only a guess at the branch; ask git.
			if branch, err := a.ReadGit(wtPath).CurrentBranch(ctx); err == nil && branch != "" {
				entry.Branch = branch
			}
		}
		return resolved{Entry: entry, Repo: repo, Registered: false}, nil
	}

	return resolved{}, NotFound(
		"could not tell which ticket this directory belongs to; name one explicitly, " +
			"for example `flow status RD-19471`")
}

// inferFromWorktreeLayout walks up looking for a directory literally named
// .worktrees, and parses the ticket out of its immediate child.
func inferFromWorktreeLayout(dir, subdir string) (ticket.Ticket, string, bool) {
	parts := strings.Split(filepath.Clean(dir), string(filepath.Separator))
	for i := len(parts) - 2; i >= 0; i-- {
		if parts[i] != subdir {
			continue
		}
		name := parts[i+1]
		tk, ok := ticket.ParseDirName(name)
		if !ok {
			continue
		}
		path := string(filepath.Separator) + filepath.Join(parts[:i+2]...)
		return tk, path, true
	}
	return ticket.Ticket{}, "", false
}

// tk rebuilds the ticket identity from a registry entry.
func tk(e registry.Entry) ticket.Ticket {
	return ticket.Ticket{ID: e.TicketID, Slug: e.Slug}
}

// worktreeExists reports whether a recorded worktree path is still there.
func worktreeExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// completeTickets offers ticket IDs from the registry, current repository
// first. It must never prompt, hit the network, or fail loudly — on any error
// it simply offers nothing.
func (a *App) completeTickets(cmd *cobra.Command, args []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	// PersistentPreRunE also runs for __complete, but a broken config must not
	// make completion noisy.
	if a.Registry == nil || a.Out == nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	file, err := a.Registry.Load()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	var current []cobra.Completion
	var others []cobra.Completion

	repoKey := ""
	if dir, err := cwd(); err == nil {
		if repo, err := gitx.DiscoverRepo(cmd.Context(), a.baseRunner, dir, a.cfg.Defaults.Remote); err == nil {
			repoKey = repo.Key
		}
	}

	for _, e := range file.All() {
		completion := cobra.CompletionWithDesc(e.TicketID, e.Slug)
		if repoKey != "" && e.RepoKey == repoKey {
			current = append(current, completion)
		} else {
			others = append(others, completion)
		}
	}
	return append(current, others...), cobra.ShellCompDirectiveNoFileComp
}
