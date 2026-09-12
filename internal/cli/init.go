package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/idosaban-scaleops/flow/internal/editor"
	"github.com/idosaban-scaleops/flow/internal/gitx"
	"github.com/idosaban-scaleops/flow/internal/herdr"
	"github.com/idosaban-scaleops/flow/internal/output"
	"github.com/idosaban-scaleops/flow/internal/registry"
	"github.com/idosaban-scaleops/flow/internal/ticket"
)

type initOptions struct {
	base        string
	branch      string
	noFetch     bool
	noEditor    bool
	noWorkspace bool
	noAssets    bool
	repo        string
	adoptOnly   bool
}

// created records which side effects actually ran, for the summary and the
// --json payload.
type created struct {
	Worktree  bool `json:"worktree"`
	Branch    bool `json:"branch"`
	Assets    bool `json:"assets"`
	Workspace bool `json:"workspace"`
	Editor    bool `json:"editor"`
	Exclude   bool `json:"exclude"`
}

type initResult struct {
	registry.Entry
	Created     created  `json:"created"`
	Adopted     bool     `json:"adopted"`
	AdoptedFrom *string  `json:"adopted_from"`
	Warnings    []string `json:"warnings,omitempty"`
}

func newInitCommand(app *App) *cobra.Command {
	var opts initOptions

	cmd := &cobra.Command{
		Use:   "init <TICKET-ID> <short description...>",
		Short: "Create (or adopt) a worktree, workspace, assets folder and editor window",
		Long: strings.TrimSpace(`
Create everything needed to start work on a ticket: a git worktree on a new
branch, a per-ticket assets folder, a herdr workspace, and an editor window.

flow init is idempotent and adoptive. Run against a ticket that already has a
worktree, branch, registry entry, assets folder or workspace, it converges on a
complete, correctly-registered setup instead of failing, and it never resets or
destroys an existing worktree.`),
		Example: strings.TrimSpace(`
  flow init RD-19471 add new toolbar
  flow init RD-19471 "add new toolbar" --base release-2.4
  flow init RD-19471 add new toolbar --no-editor --no-workspace
  flow init RD-19471 add new toolbar --adopt-only`),
		Args: cobra.MinimumNArgs(2),
	}

	f := cmd.Flags()
	f.StringVar(&opts.base, "base", "", "branch or ref to create the new branch from")
	f.StringVar(&opts.branch, "branch", "", "override the derived branch name")
	f.BoolVar(&opts.noFetch, "no-fetch", false, "skip fetching the remote before branching")
	f.BoolVar(&opts.noEditor, "no-editor", false, "do not launch the editor")
	f.BoolVar(&opts.noWorkspace, "no-workspace", false, "do not create the herdr workspace")
	f.BoolVar(&opts.noAssets, "no-assets", false, "do not create the assets directory")
	f.StringVar(&opts.repo, "repo", "", "run against a repo other than the one containing the cwd")
	f.BoolVar(&opts.adoptOnly, "adopt-only", false,
		"register what already exists and create nothing")

	cmd.RunE = app.runArgs(func(ctx context.Context, args []string) error {
		return app.runInit(ctx, args, opts)
	})
	return cmd
}

func (a *App) runInit(ctx context.Context, args []string, opts initOptions) error {
	// 1. Normalize the ticket and derive every name.
	tk, err := ticket.New(args[0], strings.Join(args[1:], " "))
	if err != nil {
		return Wrap(ExitUsage, "usage", err, "")
	}

	dir, err := repoDir(opts.repo)
	if err != nil {
		return err
	}

	// 2. Resolve the repository.
	repo, err := gitx.DiscoverRepo(ctx, a.baseRunner, dir, a.cfg.Defaults.Remote)
	if err != nil {
		return Wrap(ExitDependency, "dependency", err, "resolving the git repository")
	}

	n := deriveNames(a.cfg, repo.Root, tk)
	if opts.branch != "" {
		n.Branch = opts.branch
		n.WorktreePath = filepath.Join(repo.Root, a.cfg.Naming.WorktreesSubdir, opts.branch)
	}

	base := opts.base
	if base == "" {
		base = a.cfg.BaseBranchFor(repo.Key)
	}
	remote := a.cfg.Defaults.Remote

	result := initResult{Entry: registry.Entry{
		TicketID:     tk.ID,
		Slug:         tk.Slug,
		RepoKey:      repo.Key,
		RepoRoot:     repo.Root,
		WorktreePath: n.WorktreePath,
		Branch:       n.Branch,
		BaseBranch:   base,
		AssetsPath:   n.AssetsPath,
		LastOpenedAt: time.Now().UTC(),
	}}

	// 3. Reconcile the registry entry, which may rename or adopt.
	file, err := a.Registry.Load()
	if err != nil {
		return Wrap(ExitFailure, "registry", err, "reading the registry")
	}
	existing, hasEntry := file.Find(repo.Key, tk.ID)
	if hasEntry {
		if err := a.reconcileEntry(&result, existing, tk); err != nil {
			return err
		}
	}

	git := a.Git(repo.Root)
	readGit := a.ReadGit(repo.Root)

	// 4-5. The worktrees directory, and git's ignore entry for it.
	if !opts.adoptOnly {
		worktreesDir := filepath.Join(repo.Root, a.cfg.Naming.WorktreesSubdir)
		if err := a.mutate("create "+worktreesDir, func() error {
			return os.MkdirAll(worktreesDir, 0o755) //nolint:gosec // a normal project directory
		}); err != nil {
			return Wrap(ExitFailure, "failure", err, "creating %s", worktreesDir)
		}
		if err := a.ensureExcluded(ctx, readGit, &result); err != nil {
			result.Warnings = append(result.Warnings, err.Error())
			a.Out.Warn("%v", err)
		}
	}

	// 6. Fetch. A failure here is a warning: stale refs still allow branching.
	if !opts.noFetch && a.cfg.Defaults.FetchBeforeBranch && !opts.adoptOnly {
		a.fetch(ctx, git, remote, base, &result)
	}

	// 7. The worktree itself, with adoption.
	adoptedFrom, err := a.ensureWorktree(ctx, git, readGit, &result, remote, base, opts)
	if err != nil {
		return err
	}
	if adoptedFrom != "" {
		result.Adopted = true
		result.AdoptedFrom = &adoptedFrom
	}

	if opts.adoptOnly && !result.Adopted && !hasEntry {
		return NotFound("nothing to adopt for %s: no worktree at %s and no registry entry",
			tk.ID, n.WorktreePath)
	}

	// 8. Assets.
	if !opts.noAssets && a.cfg.Defaults.CreateAssets && !opts.adoptOnly {
		a.ensureAssets(&result, n.AssetsPath)
	}

	// 9. Workspace.
	if !opts.noWorkspace && a.cfg.Defaults.CreateWorkspace {
		a.ensureWorkspace(ctx, &result, n, opts.adoptOnly)
	}

	// 10. Editor.
	if !opts.noEditor && a.cfg.Defaults.OpenEditor && !opts.adoptOnly {
		a.launchEditor(ctx, &result)
	}

	// 11. Record what exists now. A failure after the worktree was created is
	// reported but never rolled back: a half-created workspace is recoverable
	// with `flow open`, a rolled-back worktree loses work.
	if err := a.saveEntry(result.Entry, existing, hasEntry); err != nil {
		a.Out.Failure("%v", err)
		result.Warnings = append(result.Warnings, err.Error())
	}

	return a.reportInit(result, n)
}

// reconcileEntry folds an existing registry entry into the new one, prompting
// when the slug changed — that is a rename, not a second ticket.
func (a *App) reconcileEntry(result *initResult, existing registry.Entry, tk ticket.Ticket) error {
	result.Entry.CreatedAt = existing.CreatedAt
	result.Entry.Workspace = existing.Workspace

	if existing.Slug == tk.Slug {
		return nil
	}

	title := fmt.Sprintf("%s is already registered as %q. Rename to %q?", tk.ID, existing.Slug, tk.Slug)
	err := a.confirm(title, "The worktree, branch and assets folder keep their current names.", true)
	switch {
	case errors.Is(err, output.ErrAborted):
		// Keep the old slug and everything derived from it.
		result.Entry.Slug = existing.Slug
		result.Entry.Branch = existing.Branch
		result.Entry.WorktreePath = existing.WorktreePath
		result.Entry.AssetsPath = existing.AssetsPath
		return nil
	case err != nil:
		return err
	}

	// Accepted: record the new slug, but leave the on-disk names alone —
	// renaming a branch and a worktree mid-flight is riskier than the
	// inconsistency it fixes.
	result.Entry.Branch = existing.Branch
	result.Entry.WorktreePath = existing.WorktreePath
	result.Entry.AssetsPath = existing.AssetsPath
	a.Out.Warn("registry slug updated to %q; the worktree and assets paths still use %q",
		tk.Slug, existing.Slug)
	result.Warnings = append(result.Warnings,
		fmt.Sprintf("on-disk paths still use the old slug %q", existing.Slug))
	return nil
}

func (a *App) ensureExcluded(ctx context.Context, readGit *gitx.Git, result *initResult) error {
	commonDir, err := readGit.GitCommonDir(ctx)
	if err != nil {
		return fmt.Errorf("could not locate the git directory to update info/exclude: %w", err)
	}
	line := a.cfg.Naming.WorktreesSubdir + "/"

	return a.mutate(fmt.Sprintf("add %q to %s/info/exclude", line, commonDir), func() error {
		added, err := gitx.EnsureExcluded(commonDir, line)
		if err != nil {
			return err
		}
		result.Created.Exclude = added
		return nil
	})
}

func (a *App) fetch(ctx context.Context, git *gitx.Git, remote, base string, result *initResult) {
	if err := git.Fetch(ctx, remote, base); err != nil {
		msg := fmt.Sprintf("could not fetch %s/%s: %v — continuing with local refs", remote, base, err)
		a.Out.Warn("%s", msg)
		result.Warnings = append(result.Warnings, msg)
	}
	// Tags matter later, for deriving a chart version without the Helm index.
	if err := git.FetchTags(ctx, remote); err != nil {
		a.Out.Log.Debug("fetching tags failed", "err", err)
	}
}

// ensureWorktree creates or adopts the worktree, and reports what it adopted.
func (a *App) ensureWorktree(
	ctx context.Context, git, readGit *gitx.Git, result *initResult,
	remote, base string, opts initOptions,
) (adoptedFrom string, err error) {
	path := result.Entry.WorktreePath

	// Already a registered worktree of this repo? Adopt it untouched.
	if wt, ok, listErr := readGit.FindWorktree(ctx, path); listErr == nil && ok {
		if wt.Prunable {
			a.Out.Status("worktree registration at %s is stale; pruning", path)
			if err := git.PruneWorktrees(ctx); err != nil {
				a.Out.Log.Debug("worktree prune failed", "err", err)
			}
		} else {
			a.Out.Status("adopting existing worktree at %s", path)
			if wt.Branch != "" && wt.Branch != result.Entry.Branch {
				a.Out.Warn("worktree at %s is on branch %s, expected %s — recording %s",
					path, wt.Branch, result.Entry.Branch, wt.Branch)
				result.Entry.Branch = wt.Branch
			}
			return "worktree", nil
		}
	}

	// A directory that git does not know about.
	if gitx.DirExists(path) {
		if !gitx.DirIsEmpty(path) {
			return "", Precondition(
				"the directory %s already exists and is not a git worktree. flow will not touch it, "+
					"because the alternative is clobbering unknown files. "+
					"Move it aside, or run `flow prune`, or repair it with `git worktree repair %s`",
				path, path)
		}
		if err := a.mutate("remove the empty directory "+path, func() error {
			return os.Remove(path)
		}); err != nil {
			return "", Wrap(ExitFailure, "failure", err, "removing the empty directory %s", path)
		}
	}

	if opts.adoptOnly {
		return "", nil
	}

	// The branch may already be checked out somewhere else; git refuses a
	// second checkout, so offer to adopt that location instead.
	if other, ok, listErr := readGit.FindWorktreeForBranch(ctx, result.Entry.Branch); listErr == nil && ok {
		a.Out.Warn("branch %s is already checked out at %s", result.Entry.Branch, other.Path)
		promptErr := a.confirm(
			fmt.Sprintf("Adopt %s as the worktree for %s?", other.Path, result.Entry.TicketID),
			"flow will record that path instead of creating a second worktree.", true)
		if promptErr == nil {
			result.Entry.WorktreePath = other.Path
			return "worktree", nil
		}
		if !errors.Is(promptErr, output.ErrAborted) {
			return "", promptErr
		}
		return "", Precondition("branch %s is already checked out at %s; "+
			"git cannot check it out twice", result.Entry.Branch, other.Path)
	}

	if err := a.createWorktree(ctx, git, readGit, result, remote, base); err != nil {
		return "", err
	}
	result.Created.Worktree = true
	return "", nil
}

// createWorktree mirrors git-wt's branch-resolution ladder.
func (a *App) createWorktree(
	ctx context.Context, git, readGit *gitx.Git, result *initResult,
	remote, base string,
) error {
	branch, path := result.Entry.Branch, result.Entry.WorktreePath

	switch {
	case readGit.LocalBranchExists(ctx, branch):
		if err := git.AddWorktreeExisting(ctx, path, branch); err != nil {
			return Wrap(ExitDependency, "dependency", err, "creating the worktree from local branch %s", branch)
		}

	case readGit.RemoteBranchExists(ctx, remote, branch):
		if err := git.AddWorktreeTracking(ctx, path, branch, remote); err != nil {
			return Wrap(ExitDependency, "dependency", err,
				"creating the worktree tracking %s/%s", remote, branch)
		}
		result.Created.Branch = true

	default:
		startPoint := remote + "/" + base
		if !readGit.RemoteBranchExists(ctx, remote, base) {
			if readGit.LocalBranchExists(ctx, base) {
				startPoint = base
			} else {
				a.Out.Warn("neither %s/%s nor a local %s exists; branching from HEAD",
					remote, base, base)
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("branched from HEAD because %s was not found", base))
				startPoint = "HEAD"
			}
		}
		if err := git.AddWorktreeFromBase(ctx, path, branch, startPoint); err != nil {
			return Wrap(ExitDependency, "dependency", err,
				"creating the worktree for %s from %s", branch, startPoint)
		}
		result.Created.Branch = true
	}
	return nil
}

func (a *App) ensureAssets(result *initResult, path string) {
	if gitx.DirExists(path) {
		return
	}
	err := a.mutate("create "+path, func() error {
		return os.MkdirAll(path, 0o755) //nolint:gosec // a normal project directory
	})
	if err != nil {
		a.Out.Warn("could not create the assets directory %s: %v", path, err)
		result.Warnings = append(result.Warnings, err.Error())
		return
	}
	result.Created.Assets = true
}

// ensureWorkspace focuses an existing workspace rather than creating a second
// one, which is what makes `flow init` on an existing ticket behave like
// `flow open`.
func (a *App) ensureWorkspace(ctx context.Context, result *initResult, n names, adoptOnly bool) {
	client := a.Herdr()
	if client == nil {
		return
	}
	readClient := a.ReadHerdr()

	list, err := readClient.ListWorkspaces(ctx)
	if err != nil {
		// herdr not running is not a reason to fail init — but it is a reason
		// not to create. Falling through left list nil, and FindByID and
		// FindByLabel both report a plain false on a nil slice rather than a
		// sentinel, so both lookups missed and a ticket that already had a
		// workspace got a second one. prune.listWorkspaces suppresses its
		// actions on the same failure; init suppresses the create.
		a.Out.Warn("could not list herdr workspaces, so not creating one: %v", err)
		result.Warnings = append(result.Warnings, err.Error())
		return
	}

	if ws, ok := herdr.FindByID(list, result.Entry.Workspace.ID); ok {
		a.focusWorkspace(ctx, client, ws, result)
		return
	}
	if ws, ok := herdr.FindByLabel(list, n.WorkspaceLabel); ok {
		a.focusWorkspace(ctx, client, ws, result)
		return
	}
	if adoptOnly {
		return
	}

	ws, err := client.CreateWorkspace(ctx, result.Entry.WorktreePath, n.WorkspaceLabel)
	if err != nil {
		a.Out.Warn("could not create the herdr workspace: %v", err)
		result.Warnings = append(result.Warnings, err.Error())
		return
	}
	result.Entry.Workspace = registry.Workspace{
		Provider:   a.cfg.Workspace.Provider,
		ID:         ws.ID,
		Label:      ws.Label,
		TabID:      ws.TabID,
		RootPaneID: ws.RootPaneID,
	}
	result.Created.Workspace = true
}

func (a *App) focusWorkspace(ctx context.Context, client *herdr.CLI, ws herdr.Workspace, result *initResult) {
	if err := client.FocusWorkspace(ctx, ws.ID); err != nil {
		a.Out.Log.Debug("focusing the workspace failed", "err", err)
	}
	result.Entry.Workspace = registry.Workspace{
		Provider: a.cfg.Workspace.Provider,
		ID:       ws.ID,
		Label:    ws.Label,
	}
}

func (a *App) launchEditor(ctx context.Context, result *initResult) {
	err := a.Editor().Open(ctx, result.Entry.WorktreePath)
	var notOnPath *editor.ErrNotOnPath
	switch {
	case err == nil:
		result.Created.Editor = true
	case errors.As(err, &notOnPath):
		a.Out.Warn("%v — skipping the editor", err)
		result.Warnings = append(result.Warnings, err.Error())
	default:
		a.Out.Warn("could not launch the editor: %v", err)
		result.Warnings = append(result.Warnings, err.Error())
	}
}

func (a *App) saveEntry(entry, existing registry.Entry, hasExisting bool) error {
	if hasExisting && entry.CreatedAt.IsZero() {
		entry.CreatedAt = existing.CreatedAt
	}
	return a.mutate("record "+entry.TicketID+" in the registry", func() error {
		return a.Registry.Update(func(f *registry.File) error {
			f.Upsert(entry)
			return nil
		})
	})
}

func (a *App) reportInit(result initResult, n names) error {
	if a.JSON() {
		return a.Out.JSON(result)
	}

	e := result.Entry
	a.Out.Println()
	a.Out.Field("ticket", a.Out.Theme.Ticket.Render(e.TicketID))
	a.Out.Field("branch", e.Branch)
	a.Out.Field("worktree", a.Out.Theme.Path.Render(e.WorktreePath))
	if e.AssetsPath != "" {
		a.Out.Field("assets", a.Out.Theme.Path.Render(e.AssetsPath))
	}
	switch {
	case e.Workspace.ID != "":
		a.Out.Field("workspace", fmt.Sprintf("%s (%s)", e.Workspace.ID, n.WorkspaceLabel))
	case e.Workspace.Label != "":
		a.Out.Field("workspace", n.WorkspaceLabel+" (id unknown)")
	}
	a.Out.Println()
	a.Out.Println("  cd " + e.WorktreePath)
	a.Out.Println()
	return nil
}
