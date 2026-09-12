package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	flowexec "github.com/idosaban-scaleops/flow/internal/exec"
	"github.com/idosaban-scaleops/flow/internal/ghapi"
)

func newPRCommand(app *App) *cobra.Command {
	var (
		printOnly bool
		web       bool
		create    bool
		strict    bool
	)

	cmd := &cobra.Command{
		Use:   "pr [ticket]",
		Short: "Open a ticket's pull request, or the page that creates one",
		Long: "Open the pull request for the ticket's branch.\n\n" +
			"A missing pull request is not an error: flow offers GitHub's compare page\n" +
			"instead and exits 0. Pass --strict to fail with exit 3 when no pull\n" +
			"request exists, which is the useful behaviour in scripts.",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: app.completeTickets,
	}

	f := cmd.Flags()
	f.BoolVar(&printOnly, "print", false, "print the URL instead of opening it")
	f.BoolVar(&web, "web", false, "open in a browser (the default)")
	f.BoolVar(&create, "create", false, "go straight to the compare page when no PR exists")
	f.BoolVar(&strict, "strict", false, "exit 3 when no pull request exists")

	// --web names the default, so the only thing it can mean is "not --print".
	// Asking for both is a contradiction rather than something to resolve
	// silently; it used to be accepted and ignored.
	cmd.MarkFlagsMutuallyExclusive("print", "web")

	cmd.RunE = app.runArgs(func(ctx context.Context, args []string) error {
		openInBrowser := web || !printOnly
		got, err := app.resolveTicket(ctx, first(args))
		if err != nil {
			return err
		}

		repo, err := app.repoFor(got)
		if err != nil {
			return err
		}
		if !repo.IsGitHub() {
			return Precondition("repository %s has no GitHub origin remote, so flow cannot find a pull request",
				repo.Key)
		}

		branch := got.Entry.Branch
		info, prErr := app.GitHub(ctx).FindPR(ctx, repo.Owner, repo.Name, branch)

		if info.State == ghapi.PRUnknown {
			return Wrap(ExitDependency, "dependency", prErr,
				"could not determine the pull request for %s", branch)
		}

		if info.Exists() {
			if app.JSON() {
				return app.Out.JSON(map[string]any{
					"ticket": got.Entry.TicketID, "branch": branch, "pr": info,
				})
			}
			if !openInBrowser {
				app.Out.Line(info.URL)
				return nil
			}
			return app.openURL(ctx, info.URL)
		}

		// No pull request. Report what can be done rather than failing.
		base := got.Entry.BaseBranch
		if base == "" {
			base = app.cfg.BaseBranchFor(repo.Key)
		}
		compare := fmt.Sprintf("https://github.com/%s/%s/compare/%s...%s?expand=1",
			repo.Owner, repo.Name, base, branch)

		pushed := app.ReadGit(repo.Root).RemoteBranchExists(ctx, app.cfg.Defaults.Remote, branch)

		if app.JSON() {
			payload := map[string]any{
				"ticket": got.Entry.TicketID, "branch": branch,
				"pr": info, "compare_url": compare, "pushed": pushed,
			}
			if strict {
				return NotFound("no pull request for branch %s", branch)
			}
			return app.Out.JSON(payload)
		}

		if strict {
			return NotFound("no pull request for branch %s", branch)
		}

		app.Out.Status("no pull request for branch %s", branch)
		if !pushed {
			app.Out.Warn("branch %s has never been pushed; run `git push -u %s %s` first",
				branch, app.cfg.Defaults.Remote, branch)
		}
		if !openInBrowser {
			app.Out.Line(compare)
			return nil
		}
		if create || app.Yes() {
			return app.openURL(ctx, compare)
		}
		if confirmErr := app.confirm("Open GitHub's compare page to create one?", compare, true); confirmErr != nil {
			// Declining is a legitimate answer; print the link and stop.
			app.Out.Status("compare page: %s", compare)
			return nil //nolint:nilerr // the user declined, which is not a failure
		}
		return app.openURL(ctx, compare)
	})
	return cmd
}

func (a *App) openURL(ctx context.Context, url string) error {
	if _, err := a.Runner.Run(ctx, flowexec.Opts{Name: "open", Args: []string{url}}); err != nil {
		return Wrap(ExitDependency, "dependency", err, "opening %s", url)
	}
	a.Out.Status("opened %s", url)
	return nil
}
