package cli

import (
	"context"
	"time"

	"github.com/spf13/cobra"

	"github.com/idosaban-scaleops/flow/internal/registry"
)

func newOpenCommand(app *App) *cobra.Command {
	var (
		noEditor      bool
		noWorkspace   bool
		workspaceOnly bool
	)

	cmd := &cobra.Command{
		Use:   "open [ticket]",
		Short: "Re-attach to a ticket: focus or recreate its workspace, relaunch the editor",
		Long: "Re-attach to an existing ticket. If the herdr workspace is gone it is\n" +
			"recreated; if it exists it is focused. The editor is relaunched unless\n" +
			"--no-editor is given.",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: app.completeTickets,
	}

	f := cmd.Flags()
	f.BoolVar(&noEditor, "no-editor", false, "do not launch the editor")
	f.BoolVar(&noWorkspace, "no-workspace", false, "do not touch the herdr workspace")
	f.BoolVar(&workspaceOnly, "workspace-only", false, "only focus or recreate the workspace")

	cmd.RunE = app.runArgs(func(ctx context.Context, args []string) error {
		got, err := app.resolveTicket(ctx, first(args))
		if err != nil {
			return err
		}
		if !worktreeExists(got.Entry.WorktreePath) {
			return NotFound("the worktree for %s is gone (%s); run `flow init %s <description>` "+
				"to recreate it, or `flow prune` to drop the entry",
				got.Entry.TicketID, got.Entry.WorktreePath, got.Entry.TicketID)
		}

		result := initResult{Entry: got.Entry}
		result.Entry.LastOpenedAt = time.Now().UTC()

		n := deriveNames(app.cfg, got.Entry.RepoRoot, tk(got.Entry))
		if result.Entry.Workspace.Label != "" {
			n.WorkspaceLabel = result.Entry.Workspace.Label
		}

		if !noWorkspace {
			app.ensureWorkspace(ctx, &result, n, false)
		}
		if !noEditor && !workspaceOnly {
			app.launchEditor(ctx, &result)
		}

		if err := app.mutate("update "+got.Entry.TicketID+" in the registry", func() error {
			return app.Registry.Update(func(f *registry.File) error {
				f.Upsert(result.Entry)
				return nil
			})
		}); err != nil {
			app.Out.Warn("could not update the registry: %v", err)
		}

		if app.JSON() {
			return app.Out.JSON(result)
		}
		app.Out.Success("%s is open at %s", result.Entry.TicketID, result.Entry.WorktreePath)
		return nil
	})
	return cmd
}
