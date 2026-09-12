package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/idosaban-scaleops/flow/internal/ghapi"
	"github.com/idosaban-scaleops/flow/internal/gitx"
	"github.com/idosaban-scaleops/flow/internal/output"
	"github.com/idosaban-scaleops/flow/internal/registry"
)

type deleteOptions struct {
	all        bool
	force      bool
	keepBranch bool
}

type deleteOutcome struct {
	Ticket        string `json:"ticket"`
	Deleted       bool   `json:"deleted"`
	Skipped       bool   `json:"skipped"`
	Reason        string `json:"reason,omitempty"`
	BranchDeleted bool   `json:"branch_deleted"`
	BranchKept    string `json:"branch_kept,omitempty"`
	AssetsPath    string `json:"assets_path_kept,omitempty"`
	Workspace     string `json:"workspace_left_open,omitempty"`
}

func newDeleteCommand(app *App) *cobra.Command {
	var opts deleteOptions

	cmd := &cobra.Command{
		Use:   "delete [ticket]",
		Short: "Tear down a ticket's worktree and branch",
		Long: strings.TrimSpace(`
Remove a ticket's worktree and local branch, and drop its registry entry.

flow deliberately does not close the herdr workspace, does not close the editor
window, and never deletes the assets directory. It prints reminders for each
instead.

Before removing anything it checks the pull request, uncommitted changes, and
unpushed commits, and prompts for each. A merged pull request with a clean
worktree deletes without prompting.`),
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: app.completeTickets,
	}

	f := cmd.Flags()
	f.BoolVar(&opts.all, "all", false, "operate on every ticket in the current repository")
	f.BoolVar(&opts.force, "force", false, "skip every safety prompt and force removal")
	f.BoolVar(&opts.keepBranch, "keep-branch", false, "keep the local branch")

	cmd.RunE = app.runArgs(func(ctx context.Context, args []string) error {
		if opts.all {
			if len(args) > 0 {
				return Usage("--all takes no ticket argument")
			}
			return app.deleteAll(ctx, opts)
		}
		got, err := app.resolveTicket(ctx, first(args))
		if err != nil {
			return err
		}

		outcome, err := app.deleteOne(ctx, got, opts)
		if err != nil {
			return err
		}
		if app.JSON() {
			return app.Out.JSON(outcome)
		}
		return nil
	})
	return cmd
}

func (a *App) deleteAll(ctx context.Context, opts deleteOptions) error {
	dir, err := cwd()
	if err != nil {
		return err
	}
	repo, err := gitx.DiscoverRepo(ctx, a.baseRunner, dir, a.cfg.Defaults.Remote)
	if err != nil {
		return Wrap(ExitDependency, "dependency", err, "--all operates on the current repository")
	}

	file, err := a.Registry.Load()
	if err != nil {
		return Wrap(ExitFailure, "registry", err, "reading the registry")
	}
	entries := file.ForRepo(repo.Key)
	if len(entries) == 0 {
		a.Out.Status("no tickets registered for %s", repo.Key)
		return nil
	}

	// One summary confirmation for the whole repository, naming the count.
	// --all alone must never auto-confirm, and this is the only thing standing
	// in front of `--all --force`, which skips every per-ticket prompt and
	// force-removes each worktree along with any uncommitted work in it.
	// confirmDestructive defaults to No and makes --json demand an explicit
	// --yes.
	summary := fmt.Sprintf("Delete all %d ticket(s) registered for %s?", len(entries), repo.Key)
	if opts.force {
		summary = fmt.Sprintf(
			"Force-delete all %d ticket(s) registered for %s, discarding uncommitted work?",
			len(entries), repo.Key)
	}
	if err := a.confirmDestructive(summary, strings.Join(ticketIDs(entries), ", ")); err != nil {
		if errors.Is(err, output.ErrAborted) {
			a.Out.Status("keeping every ticket in %s", repo.Key)
			return nil
		}
		return err
	}

	var outcomes []deleteOutcome
	var deleted, skipped, failed int

	for _, entry := range entries {
		got := resolved{Entry: entry, Repo: repo, Registered: true}
		outcome, err := a.deleteOne(ctx, got, opts)
		switch {
		case err != nil:
			failed++
			outcome.Reason = err.Error()
			a.Out.Failure("%s: %v", entry.TicketID, err)
		case outcome.Deleted:
			deleted++
		default:
			skipped++
		}
		outcomes = append(outcomes, outcome)
	}

	if a.JSON() {
		return a.Out.JSON(map[string]any{
			"deleted": deleted, "skipped": skipped, "failed": failed, "tickets": outcomes,
		})
	}
	a.Out.Println()
	a.Out.Status("%d deleted, %d skipped, %d failed", deleted, skipped, failed)
	if failed > 0 {
		return Failure("%d ticket(s) could not be deleted", failed)
	}
	return nil
}

// ticketIDs lists the ticket IDs an --all run would act on, for the summary
// prompt's detail line.
func ticketIDs(entries []registry.Entry) []string {
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.TicketID)
	}
	return ids
}

func (a *App) deleteOne(ctx context.Context, got resolved, opts deleteOptions) (deleteOutcome, error) {
	e := got.Entry
	outcome := deleteOutcome{Ticket: e.TicketID, AssetsPath: e.AssetsPath}
	if !e.Workspace.Empty() {
		outcome.Workspace = orDash(e.Workspace.Label, e.Workspace.ID)
	}

	repo, err := a.repoFor(got)
	if err != nil {
		return outcome, err
	}
	git := a.Git(repo.Root)
	readGit := a.ReadGit(repo.Root)

	if !opts.force {
		if err := a.confirmDelete(ctx, e, repo, readGit); err != nil {
			if errors.Is(err, output.ErrAborted) {
				outcome.Skipped, outcome.Reason = true, "declined at the prompt"
				a.Out.Status("keeping %s", e.TicketID)
				return outcome, nil
			}
			return outcome, err
		}
	}

	// 5. Remove the worktree. A directory that is already gone is not a
	// failure; prune reconciles git's view.
	if worktreeExists(e.WorktreePath) || registered(ctx, readGit, e.WorktreePath) {
		if err := git.RemoveWorktree(ctx, e.WorktreePath, opts.force); err != nil {
			a.Out.Warn("could not remove the worktree (%v); pruning instead", err)
			if pruneErr := git.PruneWorktrees(ctx); pruneErr != nil {
				return outcome, Wrap(ExitDependency, "dependency", err,
					"removing the worktree %s", e.WorktreePath)
			}
		}
	}

	// 6. The branch.
	a.deleteBranch(ctx, git, readGit, repo, e, opts, &outcome)

	// 7. Reconcile git's worktree list.
	if err := git.PruneWorktrees(ctx); err != nil {
		a.Out.Log.Debug("worktree prune failed", "err", err)
	}

	// 10. Drop the registry entry.
	if err := a.mutate("remove "+e.TicketID+" from the registry", func() error {
		return a.Registry.Update(func(f *registry.File) error {
			f.Remove(e.RepoKey, e.TicketID)
			return nil
		})
	}); err != nil {
		a.Out.Warn("could not update the registry: %v", err)
	}

	outcome.Deleted = true
	a.reportDelete(e, outcome)
	return outcome, nil
}

func registered(ctx context.Context, git *gitx.Git, path string) bool {
	_, ok, err := git.FindWorktree(ctx, path)
	return err == nil && ok
}

// confirmDelete runs the three safety checks in order, each with its own prompt.
func (a *App) confirmDelete(ctx context.Context, e registry.Entry, repo gitx.Repo, readGit *gitx.Git) error {
	info := a.prState(ctx, repo, e.Branch)

	// Local cross-check: useful context for the prompt, never sufficient on its
	// own, because it misses squash merges.
	mergedLocally := readGit.IsMergedInto(ctx, e.Branch,
		a.cfg.Defaults.Remote+"/"+baseOf(a.cfg, e, repo))

	var details []string

	// An inspection that could not run must never read as "there is nothing to
	// lose here". Both of these used to swallow their error and leave clean
	// true, so a merged PR over an unreadable worktree deleted with no prompt
	// at all — git's own non-force `worktree remove` was the only thing left
	// standing between the user and discarded work.
	clean := true
	if worktreeExists(e.WorktreePath) {
		st, err := a.ReadGit(e.WorktreePath).StatusOf(ctx)
		switch {
		case err != nil:
			clean = false
			details = append(details, fmt.Sprintf("could not inspect the worktree: %v", err))
		case st.Dirty:
			clean = false
			details = append(details, fmt.Sprintf("%d uncommitted change(s)", st.ChangedFiles))
		}
	}

	// Unpushed commits belong to the branch, not the directory, so this still
	// matters once the worktree is gone; ask git from the repository instead.
	unpushedDir := e.WorktreePath
	if !worktreeExists(e.WorktreePath) {
		unpushedDir = repo.Root
	}
	unpushed, err := a.ReadGit(unpushedDir).Unpushed(ctx,
		a.cfg.Defaults.Remote, e.Branch, baseOf(a.cfg, e, repo))
	switch {
	case err != nil:
		clean = false
		details = append(details, fmt.Sprintf("could not check for unpushed commits: %v", err))
	case len(unpushed) > 0:
		clean = false
		details = append(details, describeUnpushed(unpushed))
	}
	if mergedLocally {
		details = append(details, fmt.Sprintf("branch is already merged into %s locally",
			baseOf(a.cfg, e, repo)))
	}

	// Shared by PRUnknown and by any enum value this build does not know
	// about: both mean flow could not determine the state, which must never
	// read like PRNone ("there is no PR").
	undeterminable := func() error {
		return a.confirmDestructive(
			fmt.Sprintf("Could not determine whether %s is merged. Delete anyway?", e.Branch),
			joinDetails(append(details, "reason: "+orDash(info.Reason, "unknown")), ""))
	}

	switch info.State {
	case ghapi.PRMerged:
		if clean {
			return nil
		}
		return a.confirmDestructive(
			fmt.Sprintf("PR #%d for %s is merged, but the worktree has local work. Delete anyway?",
				info.Number, e.TicketID),
			strings.Join(details, "\n"))

	case ghapi.PROpen, ghapi.PRClosed:
		return a.confirmDestructive(
			fmt.Sprintf("PR #%d for %s is %s (not merged). Delete anyway?",
				info.Number, e.TicketID, info.State),
			joinDetails(details, info.URL))

	case ghapi.PRUnknown:
		// Named explicitly, not left to the default: PRNone and PRUnknown
		// collapsing into one branch is the thing the domain rules forbid, and
		// naming every state is what lets the linter enforce it.
		return undeterminable()

	case ghapi.PRNone:
		summary := "worktree is clean, nothing unpushed"
		if !clean {
			summary = strings.Join(details, "\n")
		} else if len(details) > 0 {
			summary = summary + "\n" + strings.Join(details, "\n")
		}
		return a.confirmDestructive(
			fmt.Sprintf("No pull request found for branch %s. Delete anyway?", e.Branch),
			summary)

	default:
		return undeterminable()
	}
}

func joinDetails(details []string, extra string) string {
	if extra != "" {
		details = append(details, extra)
	}
	return strings.Join(details, "\n")
}

func describeUnpushed(commits []gitx.UnpushedCommit) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d unpushed commit(s):", len(commits))
	for i, c := range commits {
		if i == 5 {
			fmt.Fprintf(&b, "\n  … and %d more", len(commits)-5)
			break
		}
		fmt.Fprintf(&b, "\n  %s %s", c.SHA, c.Subject)
	}
	return b.String()
}

func baseOf(cfg configLike, e registry.Entry, repo gitx.Repo) string {
	if e.BaseBranch != "" {
		return e.BaseBranch
	}
	return cfg.BaseBranchFor(repo.Key)
}

func (a *App) prState(ctx context.Context, repo gitx.Repo, branch string) ghapi.PRInfo {
	if !repo.IsGitHub() {
		return ghapi.Unknown("no GitHub origin remote")
	}
	info, err := a.GitHub(ctx).FindPR(ctx, repo.Owner, repo.Name, branch)
	if err != nil && info.Reason == "" {
		info.Reason = ghapi.Describe(err)
	}
	return info
}

func (a *App) deleteBranch(
	ctx context.Context, git, readGit *gitx.Git, repo gitx.Repo,
	e registry.Entry, opts deleteOptions, outcome *deleteOutcome,
) {
	if opts.keepBranch {
		outcome.BranchKept = "--keep-branch was given"
		return
	}
	if !readGit.LocalBranchExists(ctx, e.Branch) {
		return
	}

	if err := git.DeleteBranch(ctx, e.Branch, false); err == nil {
		outcome.BranchDeleted = true
		return
	}

	// git refuses -d for a squash-merged branch, which looks unmerged to it.
	// Force only on GitHub's word, or an explicit --force.
	merged := a.prState(ctx, repo, e.Branch).State == ghapi.PRMerged
	if !merged && !opts.force {
		outcome.BranchKept = fmt.Sprintf(
			"git refused to delete %s as unmerged, and GitHub did not report the PR as merged; "+
				"delete it with `git branch -D %s` if that is wrong", e.Branch, e.Branch)
		a.Out.Warn("%s", outcome.BranchKept)
		return
	}
	if err := git.DeleteBranch(ctx, e.Branch, true); err != nil {
		outcome.BranchKept = err.Error()
		a.Out.Warn("could not delete branch %s: %v", e.Branch, err)
		return
	}
	outcome.BranchDeleted = true
}

func (a *App) reportDelete(e registry.Entry, outcome deleteOutcome) {
	if a.JSON() {
		return
	}
	a.Out.Success("deleted %s", e.TicketID)
	if outcome.BranchDeleted {
		a.Out.Status("  branch %s deleted", e.Branch)
	}
	if outcome.AssetsPath != "" {
		a.Out.Status("  assets kept at %s", outcome.AssetsPath)
	}
	if outcome.Workspace != "" {
		a.Out.Status("  herdr workspace %q and the editor window are still open — close them by hand",
			outcome.Workspace)
	}
}

// configLike is the slice of config the delete helpers need, kept narrow so
// baseOf stays a pure function.
type configLike interface {
	BaseBranchFor(repoKey string) string
}
