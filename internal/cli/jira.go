package cli

import (
	"context"
	"strings"

	"github.com/spf13/cobra"

	flowexec "github.com/idosaban-scaleops/flow/internal/exec"
)

func newJiraCommand(app *App) *cobra.Command {
	var printOnly bool

	cmd := &cobra.Command{
		Use:   "jira [ticket]",
		Short: "Open a ticket in Jira",
		Long: "Open the ticket's Jira page in a browser. The URL is built from the\n" +
			"jira.browse_url template; flow never calls the Jira API.",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: app.completeTickets,
	}
	cmd.Flags().BoolVar(&printOnly, "print", false, "print the URL instead of opening it")

	cmd.RunE = app.runArgs(func(ctx context.Context, args []string) error {
		got, err := app.resolveTicket(ctx, first(args))
		if err != nil {
			return err
		}

		url := strings.ReplaceAll(app.cfg.Jira.BrowseURL, "{id}", got.Entry.TicketID)

		if app.JSON() {
			return app.Out.JSON(map[string]string{"ticket": got.Entry.TicketID, "url": url})
		}
		if printOnly {
			app.Out.Line(url)
			return nil
		}
		if _, err := app.Runner.Run(ctx, flowexec.Opts{Name: "open", Args: []string{url}}); err != nil {
			return Wrap(ExitDependency, "dependency", err, "opening %s", url)
		}
		app.Out.Status("opened %s", url)
		return nil
	})
	return cmd
}
