package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/idosaban-scaleops/flow/internal/config"
	"github.com/idosaban-scaleops/flow/internal/helmx"
	"github.com/idosaban-scaleops/flow/internal/kube"
	"github.com/idosaban-scaleops/flow/internal/output"
)

// kubeContextArg is the value to pass to helm's --kube-context: always the
// context flow resolved, whether or not --context overrode it.
//
// Sending it only for an override left helm to resolve the context itself,
// which has two consequences. The payload.Command printed for the user to copy
// omits the flag, so it is not reproducible from another shell; and the
// confirmation prompt names target.Context while helm independently re-reads
// the kubeconfig, so a `kubectl config use-context` between the two redirects
// the upgrade, rollback or uninstall to a different cluster than the one the
// user just agreed to.
//
// This does not conflict with "never mutate the user's kubeconfig" — that rule
// is about `use-context`, and AGENTS.md names passing --kube-context as the
// sanctioned way to target a cluster.
func (t clusterTarget) kubeContextArg() string { return t.Context }

func newClusterStatusCommand(app *App) *cobra.Command {
	var (
		flags    clusterFlags
		watch    bool
		interval time.Duration
	)

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the deployed release and the namespace's pods",
		Long: "Show the release helm has deployed and the state of every pod in its\n" +
			"namespace.\n\n" +
			"--watch repaints the whole view — release fields and pod table alike — in\n" +
			"place until you quit with q, which is the shape to leave running beside a\n" +
			"`flow cluster upgrade` while the new pods come up.",
		Args: cobra.NoArgs,
	}
	flags.register(cmd.Flags())
	f := cmd.Flags()
	f.BoolVarP(&watch, "watch", "w", false, "repaint the status in place until you quit")
	f.DurationVar(&interval, "interval", output.DefaultWatchInterval,
		"how often --watch re-reads the cluster (never faster than 1s)")

	cmd.RunE = app.run(func(ctx context.Context) error {
		target, err := app.clusterOnly(ctx, flags)
		if err != nil {
			return err
		}

		// --json reports one snapshot: a document that has to be a single
		// object on stdout is not something to repaint.
		if watch && !app.JSON() {
			return app.Out.Watch(ctx, interval, func(ctx context.Context) string {
				return app.statusFrame(target, app.readClusterStatus(ctx, target))
			})
		}

		snap := app.readClusterStatus(ctx, target)

		if app.JSON() {
			payload := map[string]any{
				"kube_context": target.Context,
				"server":       target.Server,
				"release":      target.Settings.ReleaseName,
				"namespace":    target.Settings.Namespace,
				"pods":         podPayload(snap.pods),
			}
			if snap.relErr == nil {
				payload["deployed"] = snap.release
			} else {
				payload["deployed_error"] = snap.relErr.Error()
			}
			return app.Out.JSON(payload)
		}

		app.printClusterHeader(target, nil, nil)

		if snap.relErr != nil {
			app.Out.Warn("no helm release %q in %s: %v",
				target.Settings.ReleaseName, target.Settings.Namespace, snap.relErr)
		} else {
			for _, field := range app.releaseFields(snap.release) {
				app.Out.Field(field[0], field[1])
			}
			app.Out.Println()
		}

		if snap.podErr != nil {
			return Wrap(ExitDependency, "dependency", snap.podErr,
				"listing pods in %s", target.Settings.Namespace)
		}
		app.renderPods(snap.pods)
		return nil
	})
	return cmd
}

// clusterStatus is one reading of the cluster. Both reads are taken together so
// that a watch frame shows a single consistent moment, and each carries its own
// error: a missing release says nothing about whether the pods are listable.
type clusterStatus struct {
	release helmx.Release
	relErr  error
	pods    []kube.Pod
	podErr  error
}

func (a *App) readClusterStatus(ctx context.Context, target clusterTarget) clusterStatus {
	var snap clusterStatus
	snap.release, snap.relErr = a.ReadHelm().Status(ctx,
		target.Settings.ReleaseName, target.Settings.Namespace, target.kubeContextArg())
	snap.pods, snap.podErr = a.Kube().Pods(ctx, target.kubeContextArg(), target.Settings.Namespace)
	return snap
}

// releaseFields is the label/value list the deployed release contributes, in
// display order, so the printed status and the watch frame cannot drift apart.
func (a *App) releaseFields(release helmx.Release) [][2]string {
	fields := [][2]string{
		{"revision", fmt.Sprint(release.Revision)},
		{"status", a.styleReleaseStatus(release.Status)},
	}
	// Only present when the release is not deployed, where it carries helm's
	// reason for that.
	if release.Description != "" {
		fields = append(fields, [2]string{"description", release.Description})
	}
	if release.ChartVersion != "" {
		fields = append(fields, [2]string{"chart", release.ChartVersion})
	}
	if release.AppVersion != "" {
		fields = append(fields, [2]string{"app version", release.AppVersion})
	}
	if !release.LastDeployed.IsZero() {
		fields = append(fields,
			[2]string{"last deployed", release.LastDeployed.Local().Format(time.RFC1123)})
	}
	return fields
}

// statusFrame renders the whole status as one string, for --watch to repaint.
// It builds text and nothing else — the same split internal/ciwait uses, where
// the frame is pure and the program around it is thin.
func (a *App) statusFrame(target clusterTarget, snap clusterStatus) string {
	t := a.Out.Theme
	var b strings.Builder

	b.WriteString(a.Out.RenderField("kube context", t.Bold.Render(target.Context)) + "\n")
	if target.Server != "" {
		b.WriteString(a.Out.RenderField("server", target.Server) + "\n")
	}
	b.WriteString(a.Out.RenderField("release", target.Settings.ReleaseName) + "\n")
	b.WriteString(a.Out.RenderField("namespace", target.Settings.Namespace) + "\n\n")

	if snap.relErr != nil {
		b.WriteString(t.Warn.Render(fmt.Sprintf("%s no helm release %q in %s",
			output.SymWarn, target.Settings.ReleaseName, target.Settings.Namespace)) + "\n\n")
	} else {
		for _, field := range a.releaseFields(snap.release) {
			b.WriteString(a.Out.RenderField(field[0], field[1]) + "\n")
		}
		b.WriteString("\n")
	}

	switch {
	case snap.podErr != nil:
		b.WriteString(t.Error.Render(fmt.Sprintf("%s could not list pods in %s",
			output.SymFail, target.Settings.Namespace)) + "\n")
	case len(snap.pods) == 0:
		b.WriteString(t.Muted.Render("no pods in the namespace") + "\n")
	default:
		rows, unhealthy := a.podRows(snap.pods)
		b.WriteString(a.Out.RenderTable(podHeaders, rows) + "\n")
		if unhealthy > 0 {
			b.WriteString(t.Warn.Render(fmt.Sprintf("%s %d pod(s) are not Running or Succeeded",
				output.SymWarn, unhealthy)) + "\n")
		}
	}
	return b.String()
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

// podHeaders is the pod table's header row, shared by the printed table and
// the watch frame.
var podHeaders = []string{"POD", "READY", "STATE", "RESTARTS"}

func (a *App) renderPods(pods []kube.Pod) {
	if len(pods) == 0 {
		a.Out.Status("no pods in the namespace")
		return
	}

	rows, unhealthy := a.podRows(pods)
	a.Out.Table(podHeaders, rows)

	if unhealthy > 0 {
		a.Out.Warn("%d pod(s) are not Running or Succeeded", unhealthy)
	}
}

// podRows builds the styled table rows and counts the pods worth worrying
// about.
func (a *App) podRows(pods []kube.Pod) (rows [][]string, unhealthy int) {
	t := a.Out.Theme
	rows = make([][]string, 0, len(pods))
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
	return rows, unhealthy
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
			// strconv.Atoi, not fmt.Sscanf: Sscanf stops at the first
			// non-digit and reports success, so "3abc" would roll back to
			// revision 3.
			parsed, err := strconv.Atoi(args[0])
			if err != nil || parsed <= 0 {
				return Usage("revision must be a positive integer, got %q", args[0])
			}
			revision = parsed
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
