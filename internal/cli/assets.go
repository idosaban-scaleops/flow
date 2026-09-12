package cli

import (
	"context"
	"os"

	"github.com/spf13/cobra"

	flowexec "github.com/idosaban-scaleops/flow/internal/exec"
	"github.com/idosaban-scaleops/flow/internal/gitx"
)

func newAssetsCommand(app *App) *cobra.Command {
	var (
		noCreate bool
		reveal   bool
	)

	cmd := &cobra.Command{
		Use:   "assets [ticket]",
		Short: "Print (and create) a ticket's assets directory",
		Long: "Create the ticket's assets directory if it does not exist, then print\n" +
			"its absolute path and nothing else.\n\n" +
			"Note that the assets directory uses an underscore between the ticket ID\n" +
			"and the slug (RD-19471_add-new-toolbar), where the branch and worktree\n" +
			"use a hyphen. When the ticket is inferred from the current directory the\n" +
			"slug comes from the registry, so the convention is reproduced exactly.",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: app.completeTickets,
	}

	f := cmd.Flags()
	f.BoolVar(&noCreate, "no-create", false, "print the path without creating the directory")
	f.BoolVar(&reveal, "open", false, "also reveal the directory in Finder")

	cmd.RunE = app.runArgs(func(ctx context.Context, args []string) error {
		got, err := app.resolveTicket(ctx, first(args))
		if err != nil {
			return err
		}

		path := got.Entry.AssetsPath
		if path == "" {
			return NotFound("no assets path recorded for %s", got.Entry.TicketID)
		}

		if !noCreate && !gitx.DirExists(path) {
			if err := app.mutate("create "+path, func() error {
				return os.MkdirAll(path, 0o755) //nolint:gosec // a normal project directory
			}); err != nil {
				return Wrap(ExitFailure, "failure", err, "creating %s", path)
			}
		}

		if reveal {
			if _, err := app.Runner.Run(ctx, flowexec.Opts{Name: "open", Args: []string{path}}); err != nil {
				app.Out.Warn("could not reveal %s: %v", path, err)
			}
		}

		if app.JSON() {
			return app.Out.JSON(map[string]any{
				"ticket": got.Entry.TicketID,
				"path":   path,
				"exists": gitx.DirExists(path),
			})
		}
		app.Out.Line(path)
		return nil
	})
	return cmd
}
