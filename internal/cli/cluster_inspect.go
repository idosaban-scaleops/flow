package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/idosaban-scaleops/flow/internal/config"
	"github.com/idosaban-scaleops/flow/internal/helmx"
	"github.com/idosaban-scaleops/flow/internal/kube"
)

// kubeContextArg is the value to pass to helm's --kube-context: empty unless
// the user overrode the current context.
func (t clusterTarget) kubeContextArg() string {
	if t.Overridden {
		return t.Context
	}
	return ""
}

func newClusterStatusCommand(app *App) *cobra.Command {
	var flags clusterFlags

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the deployed release and the namespace's pods",
		Args:  cobra.NoArgs,
	}
	flags.register(cmd.Flags())

	cmd.RunE = app.run(func(ctx context.Context) error {
		target, err := app.clusterOnly(ctx, flags)
		if err != nil {
			return err
		}

		release, relErr := app.ReadHelm().Status(ctx,
			target.Settings.ReleaseName, target.Settings.Namespace, target.kubeContextArg())
		pods, podErr := app.Kube().Pods(ctx, target.kubeContextArg(), target.Settings.Namespace)

		if app.JSON() {
			payload := map[string]any{
				"kube_context": target.Context,
				"server":       target.Server,
				"release":      target.Settings.ReleaseName,
				"namespace":    target.Settings.Namespace,
				"pods":         podPayload(pods),
			}
			if relErr == nil {
				payload["deployed"] = release
			} else {
				payload["deployed_error"] = relErr.Error()
			}
			return app.Out.JSON(payload)
		}

		app.printClusterHeader(target, nil, nil)

		if relErr != nil {
			app.Out.Warn("no helm release %q in %s: %v",
				target.Settings.ReleaseName, target.Settings.Namespace, relErr)
		} else {
			app.Out.Field("revision", fmt.Sprint(release.Revision))
			app.Out.Field("status", app.styleReleaseStatus(release.Status))
			app.Out.Field("chart", release.ChartVersion)
			app.Out.Field("app version", release.AppVersion)
			if !release.LastDeployed.IsZero() {
				app.Out.Field("last deployed", release.LastDeployed.Local().Format(time.RFC1123))
			}
			app.Out.Println()
		}

		if podErr != nil {
			return Wrap(ExitDependency, "dependency", podErr, "listing pods in %s", target.Settings.Namespace)
		}
		app.renderPods(pods)
		return nil
	})
	return cmd
}

func podPayload(pods []kube.Pod) []map[string]any {
	out := make([]map[string]any, 0, len(pods))
	for _, p := range pods {
		out = append(out, map[string]any{
			"name": p.Name, "phase": p.Phase, "ready": p.Ready, "total": p.Total,
			"restarts": p.Restarts, "reason": p.Reason, "healthy": p.Healthy(),
		})
	}
	return out
}

func (a *App) renderPods(pods []kube.Pod) {
	if len(pods) == 0 {
		a.Out.Status("no pods in the namespace")
		return
	}

	t := a.Out.Theme
	rows := make([][]string, 0, len(pods))
	var unhealthy int
	for _, p := range pods {
		state := p.Phase
		if p.Reason != "" {
			state = p.Reason
		}
		if !p.Healthy() {
			unhealthy++
			state = t.Error.Render(state)
		} else {
			state = t.Success.Render(state)
		}
		restarts := fmt.Sprint(p.Restarts)
		if p.Restarts > 0 {
			restarts = t.Warn.Render(restarts)
		}
		rows = append(rows, []string{p.Name, fmt.Sprintf("%d/%d", p.Ready, p.Total), state, restarts})
	}
	a.Out.Table([]string{"POD", "READY", "STATE", "RESTARTS"}, rows)

	if unhealthy > 0 {
		a.Out.Warn("%d pod(s) are not Running or Succeeded", unhealthy)
	}
}

func (a *App) styleReleaseStatus(status string) string {
	if status == "deployed" {
		return a.Out.Theme.Success.Render(status)
	}
	return a.Out.Theme.Warn.Render(status)
}

func newClusterHistoryCommand(app *App) *cobra.Command {
	var flags clusterFlags

	cmd := &cobra.Command{
		Use:   "history",
		Short: "Show the release's revision history",
		Long: "Show the helm release history, which is the quickest way to see which\n" +
			"branch's chart is currently deployed.",
		Args: cobra.NoArgs,
	}
	flags.register(cmd.Flags())

	cmd.RunE = app.run(func(ctx context.Context) error {
		target, err := app.clusterOnly(ctx, flags)
		if err != nil {
			return err
		}

		revisions, err := app.ReadHelm().History(ctx,
			target.Settings.ReleaseName, target.Settings.Namespace, target.kubeContextArg())
		if err != nil {
			return Wrap(ExitDependency, "dependency", err,
				"helm history for %s", target.Settings.ReleaseName)
		}

		if app.JSON() {
			return app.Out.JSON(map[string]any{
				"release": target.Settings.ReleaseName, "revisions": revisions,
			})
		}

		rows := make([][]string, 0, len(revisions))
		for _, r := range revisions {
			updated := ""
			if !r.Updated.IsZero() {
				updated = r.Updated.Local().Format("2006-01-02 15:04")
			}
			rows = append(rows, []string{
				fmt.Sprint(r.Revision), updated,
				app.styleReleaseStatus(r.Status), r.Chart, r.Description,
			})
		}
		app.Out.Table([]string{"REV", "UPDATED", "STATUS", "CHART", "DESCRIPTION"}, rows)
		return nil
	})
	return cmd
}

func newClusterRollbackCommand(app *App) *cobra.Command {
	var flags clusterFlags

	cmd := &cobra.Command{
		Use:   "rollback [revision]",
		Short: "Roll the release back to a previous revision",
		Long:  "Roll back to a revision, defaulting to the previous one. Always confirms.",
		Args:  cobra.MaximumNArgs(1),
	}
	flags.register(cmd.Flags())

	cmd.RunE = app.runArgs(func(ctx context.Context, args []string) error {
		target, err := app.clusterOnly(ctx, flags)
		if err != nil {
			return err
		}

		revision := 0
		if len(args) == 1 {
			if _, err := fmt.Sscanf(args[0], "%d", &revision); err != nil || revision <= 0 {
				return Usage("revision must be a positive integer, got %q", args[0])
			}
		}

		// Name the chart version being rolled back to before asking.
		revisions, err := app.ReadHelm().History(ctx,
			target.Settings.ReleaseName, target.Settings.Namespace, target.kubeContextArg())
		if err != nil {
			return Wrap(ExitDependency, "dependency", err, "reading the release history")
		}
		targetRev, ok := pickRollbackTarget(revisions, revision)
		if !ok {
			return NotFound("no revision to roll back to for %s", target.Settings.ReleaseName)
		}

		app.printClusterHeader(target, map[string]string{
			"rolling back to": fmt.Sprintf("revision %d", targetRev.Revision),
			"chart":           targetRev.Chart,
		}, []string{"rolling back to", "chart"})

		if err := app.confirmDestructive(
			fmt.Sprintf("Roll %s back to revision %d (%s) on %s?",
				target.Settings.ReleaseName, targetRev.Revision, targetRev.Chart, target.Context),
			"",
		); err != nil {
			return err
		}

		if err := app.Helm().Rollback(ctx,
			target.Settings.ReleaseName, target.Settings.Namespace,
			target.kubeContextArg(), targetRev.Revision); err != nil {
			return Wrap(ExitDependency, "helm_failed", err, "helm rollback")
		}

		if app.JSON() {
			return app.Out.JSON(map[string]any{
				"release": target.Settings.ReleaseName, "revision": targetRev.Revision,
				"chart": targetRev.Chart,
			})
		}
		app.Out.Success("rolled %s back to revision %d", target.Settings.ReleaseName, targetRev.Revision)
		return nil
	})
	return cmd
}

// pickRollbackTarget resolves an explicit revision, or the one before the
// currently deployed revision.
func pickRollbackTarget(revisions []helmx.Revision, want int) (helmx.Revision, bool) {
	if want > 0 {
		for _, r := range revisions {
			if r.Revision == want {
				return r, true
			}
		}
		return helmx.Revision{}, false
	}
	if len(revisions) < 2 {
		return helmx.Revision{}, false
	}
	return revisions[len(revisions)-2], true
}

func newClusterUninstallCommand(app *App) *cobra.Command {
	var (
		flags       clusterFlags
		keepHistory bool
	)

	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Uninstall the release from the cluster",
		Args:  cobra.NoArgs,
	}
	flags.register(cmd.Flags())
	cmd.Flags().BoolVar(&keepHistory, "keep-history", false, "pass --keep-history to helm")

	cmd.RunE = app.run(func(ctx context.Context) error {
		target, err := app.clusterOnly(ctx, flags)
		if err != nil {
			return err
		}

		app.printClusterHeader(target, nil, nil)
		if err := app.confirmDestructive(
			fmt.Sprintf("Uninstall release %q from namespace %q on kube context %q?",
				target.Settings.ReleaseName, target.Settings.Namespace, target.Context),
			"This removes the deployed workloads. It cannot be undone from here.",
		); err != nil {
			return err
		}

		if err := app.Helm().Uninstall(ctx,
			target.Settings.ReleaseName, target.Settings.Namespace,
			target.kubeContextArg(), keepHistory); err != nil {
			return Wrap(ExitDependency, "helm_failed", err, "helm uninstall")
		}

		if app.JSON() {
			return app.Out.JSON(map[string]any{
				"release": target.Settings.ReleaseName, "uninstalled": true,
			})
		}
		app.Out.Success("uninstalled %s from %s", target.Settings.ReleaseName, target.Context)
		return nil
	})
	return cmd
}

func newClusterValuesCommand(app *App) *cobra.Command {
	var (
		flags   clusterFlags
		current bool
		diff    bool
	)

	cmd := &cobra.Command{
		Use:   "values",
		Short: "Show the values file, or the values actually deployed",
		Args:  cobra.NoArgs,
	}
	flags.register(cmd.Flags())
	cmd.Flags().BoolVar(&current, "current", false, "fetch the values deployed on the cluster")
	cmd.Flags().BoolVar(&diff, "diff", false, "diff the local values file against the deployed values")

	cmd.RunE = app.run(func(ctx context.Context) error {
		target, err := app.clusterOnly(ctx, flags)
		if err != nil {
			return err
		}
		localPath := config.ExpandPath(target.Settings.ValuesFile)

		if !current && !diff {
			if app.JSON() {
				return app.Out.JSON(map[string]any{
					"values_file": localPath, "exists": fileExists(localPath),
				})
			}
			app.Out.Line(localPath)
			return nil
		}

		deployed, err := app.ReadHelm().GetValues(ctx,
			target.Settings.ReleaseName, target.Settings.Namespace, target.kubeContextArg())
		if err != nil {
			return Wrap(ExitDependency, "dependency", err, "helm get values")
		}

		if !diff {
			if app.JSON() {
				return app.Out.JSON(map[string]any{
					"values_file": localPath, "deployed_values": deployed,
				})
			}
			app.Out.Println(strings.TrimRight(deployed, "\n"))
			return nil
		}
		return app.diffValues(ctx, localPath, deployed)
	})
	return cmd
}

// clusterOnly resolves the cluster for commands that take no ticket, using the
// current repository only to answer "which repo does this cluster track?"
// during first-use setup.
func (a *App) clusterOnly(ctx context.Context, flags clusterFlags) (clusterTarget, error) {
	repoKey := ""
	if repo, err := a.repoForCluster(ctx); err == nil {
		repoKey = repo.Key
	}
	return a.resolveCluster(ctx, flags, repoKey)
}
