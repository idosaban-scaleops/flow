package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/idosaban-scaleops/flow/internal/gitx"
	"github.com/idosaban-scaleops/flow/internal/herdr"
	"github.com/idosaban-scaleops/flow/internal/registry"
	"github.com/idosaban-scaleops/flow/internal/ticket"
)

// pruneAction is one reconciliation flow proposes.
type pruneAction struct {
	Kind   string `json:"kind"`
	Ticket string `json:"ticket"`
	Detail string `json:"detail"`
	// Prompt is the question asked before applying it.
	Prompt string `json:"-"`
	// mutation describes the registry write in the imperative, for the
	// "would ..." line app.mutate prints under --dry-run.
	mutation string
	apply    func() error
}

func newPruneCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Reconcile the registry against reality",
		Long: strings.TrimSpace(`
Bring the registry back in line with what is actually on disk, without deleting
any real work:

  - drop entries whose worktree directory is gone
  - clear workspace IDs herdr no longer knows about
  - adopt worktrees under .worktrees that have no registry entry
  - run git worktree prune

Nothing here removes a worktree, a branch, or an assets directory.`),
		Args: cobra.NoArgs,
	}

	cmd.RunE = app.run(func(ctx context.Context) error {
		dir, err := cwd()
		if err != nil {
			return err
		}
		repo, err := gitx.DiscoverRepo(ctx, app.baseRunner, dir, app.cfg.Defaults.Remote)
		if err != nil {
			return Wrap(ExitDependency, "dependency", err, "prune operates on the current repository")
		}

		file, err := app.Registry.Load()
		if err != nil {
			return Wrap(ExitFailure, "registry", err, "reading the registry")
		}

		actions := app.planPrune(ctx, repo, file)

		if app.JSON() {
			payload := map[string]any{"actions": actions}
			switch {
			case app.DryRun():
				// --dry-run writes nothing, --yes or not. Say so in the
				// payload rather than reporting "applied": 0, which reads
				// like every action failed.
				payload["dry_run"] = true
			case app.Yes():
				applied, failures := app.applyPrune(actions, true)
				payload["applied"] = applied
				payload["failed"] = failures
			}
			if err := app.Out.JSON(payload); err != nil {
				return err
			}
			return app.gitPrune(ctx, repo)
		}

		if len(actions) == 0 {
			app.Out.Success("registry is already in sync with %s", repo.Key)
			return app.gitPrune(ctx, repo)
		}

		app.Out.Heading(fmt.Sprintf("%d thing(s) to reconcile", len(actions)))
		for _, action := range actions {
			app.Out.Status("  %s  %s", app.Out.Theme.Ticket.Render(action.Ticket), action.Detail)
		}
		app.Out.Println()

		if app.DryRun() {
			app.Out.Status("--dry-run: nothing was changed")
			return nil
		}

		applied, failures := app.applyPrune(actions, app.Yes())
		app.Out.Status("%d applied, %d failed", applied, failures)
		return app.gitPrune(ctx, repo)
	})
	return cmd
}

func (a *App) gitPrune(ctx context.Context, repo gitx.Repo) error {
	if err := a.Git(repo.Root).PruneWorktrees(ctx); err != nil {
		return Wrap(ExitDependency, "dependency", err, "git worktree prune")
	}
	return nil
}

func (a *App) planPrune(ctx context.Context, repo gitx.Repo, file *registry.File) []pruneAction {
	var actions []pruneAction

	knownWorkspaces := a.listWorkspaces(ctx)
	var registered []string

	for _, entry := range file.ForRepo(repo.Key) {
		registered = append(registered, entry.WorktreePath)

		if !worktreeExists(entry.WorktreePath) {
			actions = append(actions, pruneAction{
				Kind:     "drop-entry",
				Ticket:   entry.TicketID,
				Detail:   fmt.Sprintf("worktree %s is gone — drop the registry entry", entry.WorktreePath),
				Prompt:   fmt.Sprintf("Drop the registry entry for %s?", entry.TicketID),
				mutation: fmt.Sprintf("drop the registry entry for %s", entry.TicketID),
				apply:    a.dropEntry(entry),
			})
			continue
		}

		if entry.Workspace.Empty() || knownWorkspaces == nil {
			continue
		}
		_, byID := herdr.FindByID(knownWorkspaces, entry.Workspace.ID)
		_, byLabel := herdr.FindByLabel(knownWorkspaces, entry.Workspace.Label)
		if !byID && !byLabel {
			actions = append(actions, pruneAction{
				Kind:   "clear-workspace",
				Ticket: entry.TicketID,
				Detail: fmt.Sprintf("herdr no longer knows workspace %q — clear it",
					orDash(entry.Workspace.Label, entry.Workspace.ID)),
				Prompt:   fmt.Sprintf("Clear the recorded workspace for %s?", entry.TicketID),
				mutation: fmt.Sprintf("clear the recorded workspace for %s", entry.TicketID),
				apply:    a.clearWorkspace(entry),
			})
		}
	}

	actions = append(actions, a.planAdoptions(repo, file, registered)...)
	return actions
}

// planAdoptions finds worktrees matching the ticket naming pattern that flow
// has never been told about.
func (a *App) planAdoptions(repo gitx.Repo, file *registry.File, registered []string) []pruneAction {
	root := filepath.Join(repo.Root, a.cfg.Naming.WorktreesSubdir)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	var actions []pruneAction
	for _, dirEntry := range entries {
		if !dirEntry.IsDir() {
			continue
		}
		path := filepath.Join(root, dirEntry.Name())
		// gitx.SamePath, not a filepath.Clean map key: macOS resolves /tmp and
		// /var through symlinks and git always reports the resolved form, so
		// two spellings of the same directory would otherwise look distinct
		// and prune would propose adopting an already-registered worktree.
		if slices.ContainsFunc(registered, func(r string) bool { return gitx.SamePath(r, path) }) {
			continue
		}
		tk, ok := ticket.ParseDirName(dirEntry.Name())
		if !ok {
			continue
		}
		if _, exists := file.Find(repo.Key, tk.ID); exists {
			continue
		}

		n := deriveNames(a.cfg, repo.Root, tk)
		newEntry := registry.Entry{
			TicketID:     tk.ID,
			Slug:         tk.Slug,
			RepoKey:      repo.Key,
			RepoRoot:     repo.Root,
			WorktreePath: path,
			Branch:       n.Branch,
			BaseBranch:   a.cfg.BaseBranchFor(repo.Key),
			AssetsPath:   n.AssetsPath,
		}
		actions = append(actions, pruneAction{
			Kind:     "adopt",
			Ticket:   tk.ID,
			Detail:   fmt.Sprintf("unregistered worktree at %s — adopt it", path),
			Prompt:   fmt.Sprintf("Adopt the worktree at %s as %s?", path, tk.ID),
			mutation: fmt.Sprintf("adopt the worktree at %s as %s", path, tk.ID),
			apply:    a.adoptEntry(newEntry),
		})
	}
	return actions
}

func (a *App) listWorkspaces(ctx context.Context) []herdr.Workspace {
	client := a.ReadHerdr()
	if client == nil {
		return nil
	}
	list, err := client.ListWorkspaces(ctx)
	if err != nil {
		// herdr being unavailable must not make prune claim every workspace is
		// stale; returning nil suppresses those actions entirely.
		a.Out.Log.Debug("could not list herdr workspaces", "err", err)
		return nil
	}
	if list == nil {
		return []herdr.Workspace{}
	}
	return list
}

func (a *App) dropEntry(entry registry.Entry) func() error {
	return func() error {
		return a.Registry.Update(func(f *registry.File) error {
			f.Remove(entry.RepoKey, entry.TicketID)
			return nil
		})
	}
}

func (a *App) clearWorkspace(entry registry.Entry) func() error {
	return func() error {
		return a.Registry.Update(func(f *registry.File) error {
			updated := entry
			updated.Workspace = registry.Workspace{}
			f.Upsert(updated)
			return nil
		})
	}
}

func (a *App) adoptEntry(entry registry.Entry) func() error {
	return func() error {
		return a.Registry.Update(func(f *registry.File) error {
			f.Upsert(entry)
			return nil
		})
	}
}

func (a *App) applyPrune(actions []pruneAction, assumeYes bool) (applied, failed int) {
	for _, action := range actions {
		if !assumeYes {
			if err := a.confirm(action.Prompt, action.Detail, true); err != nil {
				continue
			}
		}
		// Through mutate: these closures call Registry.Update directly rather
		// than going through exec.Runner, so the dry-run wrapper does not
		// otherwise reach them. AGENTS.md calls this the easiest rule to
		// forget, and prune forgot it.
		if err := a.mutate(action.mutation, action.apply); err != nil {
			failed++
			a.Out.Failure("%s: %v", action.Ticket, err)
			continue
		}
		applied++
	}
	return applied, failed
}
