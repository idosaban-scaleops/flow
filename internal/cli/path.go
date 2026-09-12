package cli

import (
	"context"

	"github.com/spf13/cobra"
)

func newPathCommand(app *App) *cobra.Command {
	var allowMissing bool

	cmd := &cobra.Command{
		Use:   "path [ticket]",
		Short: "Print a ticket's worktree path",
		Long: "Print a ticket's worktree path, and nothing else, so that\n" +
			"`cd $(flow path RD-19471)` works. The ticket is inferred from the\n" +
			"current directory when omitted.",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: app.completeTickets,
	}
	cmd.Flags().BoolVar(&allowMissing, "allow-missing", false,
		"print the path even if the directory no longer exists")

	cmd.RunE = app.runArgs(func(ctx context.Context, args []string) error {
		got, err := app.resolveTicket(ctx, first(args))
		if err != nil {
			return err
		}

		path := got.Entry.WorktreePath
		if path == "" {
			return NotFound("no worktree path recorded for %s", got.Entry.TicketID)
		}
		if !allowMissing && !worktreeExists(path) {
			return NotFound("the worktree for %s is gone (%s); pass --allow-missing to print it anyway, "+
				"or run `flow prune`", got.Entry.TicketID, path)
		}

		if app.JSON() {
			return app.Out.JSON(map[string]any{
				"ticket": got.Entry.TicketID,
				"path":   path,
				"exists": worktreeExists(path),
			})
		}
		app.Out.Line(path)
		return nil
	})
	return cmd
}

// first returns args[0] or the empty string.
func first(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}
