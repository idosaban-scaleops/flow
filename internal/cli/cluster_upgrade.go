package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/idosaban-scaleops/flow/internal/config"
	flowexec "github.com/idosaban-scaleops/flow/internal/exec"
	"github.com/idosaban-scaleops/flow/internal/gitx"
	"github.com/idosaban-scaleops/flow/internal/helmx"
	"github.com/idosaban-scaleops/flow/internal/output"
)

type upgradeOptions struct {
	cluster clusterFlags
	wait    waitFlags

	branch         string
	version        string
	values         []string
	sets           []string
	reuseValues    bool
	resetThenReuse bool
	resetValues    bool
	helmWait       bool
	timeout        string
	atomic         bool
	createNS       bool
	install        bool
	noRepoUpdate   bool
}

type upgradePayload struct {
	Ticket      string        `json:"ticket,omitempty"`
	Branch      string        `json:"branch"`
	KubeContext string        `json:"kube_context"`
	Server      string        `json:"server,omitempty"`
	Release     string        `json:"release"`
	Namespace   string        `json:"namespace"`
	Chart       string        `json:"chart"`
	Version     versionResult `json:"chart_version"`
	Command     string        `json:"command"`
	Executed    bool          `json:"executed"`
}

func newClusterUpgradeCommand(app *App) *cobra.Command {
	var opts upgradeOptions

	cmd := &cobra.Command{
		Use:   "upgrade [ticket]",
		Short: "Upgrade the dev cluster with the chart built from a ticket's branch",
		Long: strings.TrimSpace(`
Wait for CI to publish the chart for a ticket's branch, then run helm upgrade
against the local dev cluster.

Run with no ticket argument from inside a worktree, it upgrades the cluster with
that branch's chart.

Note the two waits: --helm-wait is helm's own rollout wait, while the CI wait is
on by default and is turned off with --no-wait.

The baseline command is helm upgrade, not helm upgrade --install; pass --install
to opt in.`),
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: app.completeTickets,
	}

	f := cmd.Flags()
	opts.cluster.register(f)
	opts.wait.register(cmd, true)

	f.StringVar(&opts.branch, "branch", "", "use this branch directly, bypassing ticket resolution")
	f.StringVar(&opts.version, "version", "", "install this exact chart version")
	f.StringArrayVar(&opts.values, "values", nil, "values file (repeatable)")
	f.StringArrayVar(&opts.sets, "set", nil, "helm --set value (repeatable)")
	f.BoolVar(&opts.reuseValues, "reuse-values", false, "pass --reuse-values to helm")
	f.BoolVar(&opts.resetThenReuse, "reset-then-reuse-values", false, "pass --reset-then-reuse-values to helm")
	f.BoolVar(&opts.resetValues, "reset-values", false, "pass --reset-values to helm")
	f.BoolVar(&opts.helmWait, "helm-wait", false, "pass --wait to helm (this is helm's rollout wait)")
	f.StringVar(&opts.timeout, "timeout", "", "pass --timeout to helm")
	f.BoolVar(&opts.atomic, "atomic", false, "pass --atomic to helm")
	f.BoolVar(&opts.createNS, "create-namespace", false, "pass --create-namespace to helm")
	f.BoolVar(&opts.install, "install", false, "use helm upgrade --install")
	f.BoolVar(&opts.noRepoUpdate, "no-repo-update", false, "skip helm repo update, but still verify the index")

	cmd.MarkFlagsMutuallyExclusive("reuse-values", "reset-then-reuse-values", "reset-values")

	cmd.RunE = app.runArgs(func(ctx context.Context, args []string) error {
		return app.runUpgrade(ctx, args, opts)
	})
	return cmd
}

func (a *App) runUpgrade(ctx context.Context, args []string, opts upgradeOptions) error {
	// 1. Verify the cluster before anything else.
	repo, branch, ticketID, err := a.upgradeTargetBranch(ctx, args, opts)
	if err != nil {
		return err
	}

	target, err := a.resolveCluster(ctx, opts.cluster, repo.Key)
	if err != nil {
		return err
	}
	if err := a.confirmContext(target); err != nil {
		return err
	}

	// 2-3. Resolve the chart version, waiting for CI unless told not to.
	req := a.versionRequestFor(repo, target.Settings, branch)
	req.Override = opts.version
	req.RepoUpdate = !opts.noRepoUpdate
	opts.wait.apply(&req)
	if opts.version != "" {
		// An explicit version means the user already knows the build exists.
		req.Wait = false
	}
	if req.HelmRepo == "" || req.Chart == "" {
		return Precondition(
			"no helm repo or chart is configured for kube context %q; set clusters.%s.helm_repo "+
				"and clusters.%s.chart in %s, or pass --helm-repo and --chart",
			target.Context, target.Context, target.Context, a.cfgPath)
	}

	version, err := a.resolveVersion(ctx, req)
	if err != nil {
		return err
	}

	// 4. Verify the chart is actually in the index.
	if err := a.verifyChartPresent(ctx, req, version); err != nil {
		return err
	}

	// 5. Render the exact command.
	upgrade := a.buildUpgrade(target, version.Version, opts)
	payload := upgradePayload{
		Ticket: ticketID, Branch: branch,
		KubeContext: target.Context, Server: target.Server,
		Release: target.Settings.ReleaseName, Namespace: target.Settings.Namespace,
		Chart: upgrade.Chart, Version: version,
		Command: upgrade.CommandLine("helm"),
	}

	if !a.JSON() {
		a.printClusterHeader(target, map[string]string{
			"ticket":        ticketID,
			"branch":        branch,
			"chart":         upgrade.Chart,
			"chart version": version.Version,
			"resolved by":   describeResolution(version),
		}, []string{"ticket", "branch", "chart", "chart version", "resolved by"})
		a.Out.Println(payload.Command)
		a.Out.Println()
	}

	// 6. Confirm, unless --yes. --dry-run stops here.
	if a.DryRun() {
		if err := a.offerServerSideDryRun(ctx, upgrade); err != nil {
			return err
		}
		if a.JSON() {
			return a.Out.JSON(payload)
		}
		return nil
	}

	if err := a.confirmDestructive(
		fmt.Sprintf("Upgrade %s in %s on %s?",
			target.Settings.ReleaseName, target.Settings.Namespace, target.Context),
		"",
	); err != nil {
		return err
	}

	// 7. Execute.
	if err := a.runHelmUpgrade(ctx, upgrade); err != nil {
		return Wrap(ExitDependency, "helm_failed", err, "helm upgrade")
	}
	payload.Executed = true

	// 8. Report the new revision.
	if a.JSON() {
		return a.Out.JSON(payload)
	}
	a.reportRevision(ctx, target)
	return nil
}

// runHelmUpgrade runs the upgrade with something on screen while it works.
//
// helm prints nothing until it has finished — and under --helm-wait or
// --atomic that is minutes of blank terminal, which is indistinguishable from
// a hang. Interactively, helm's output is therefore captured and replayed
// under a spinner. Everywhere else it still streams, so piped runs and CI logs
// are byte-for-byte what they were.
func (a *App) runHelmUpgrade(ctx context.Context, upgrade helmx.UpgradeOptions) error {
	helm := a.Helm()
	if !a.Out.Interactive() {
		return helm.Upgrade(ctx, upgrade)
	}

	var res flowexec.Result
	err := a.Out.Spin(ctx, fmt.Sprintf("upgrading %s in %s…", upgrade.Release, upgrade.Namespace),
		func(ctx context.Context) error {
			var runErr error
			res, runErr = helm.UpgradeCaptured(ctx, upgrade)
			return runErr
		})

	// Replay helm's own output either way: NOTES.txt on the way through, and
	// the whole failure on the way out, since the error carries one line of it.
	if out := strings.TrimSpace(res.Stdout); out != "" {
		a.Out.Println(out)
	}
	if err != nil {
		if stderr := strings.TrimSpace(res.Stderr); stderr != "" {
			a.Out.Failure("%s", stderr)
		}
		return err
	}
	return nil
}

// upgradeTargetBranch resolves which branch to install, from a ticket argument,
// the current directory, or --branch.
func (a *App) upgradeTargetBranch(ctx context.Context, args []string, opts upgradeOptions) (repo gitx.Repo, branch, ticketID string, err error) {
	if opts.branch != "" {
		r, err := a.repoForCluster(ctx)
		return r, opts.branch, "", err
	}

	got, err := a.resolveTicket(ctx, first(args))
	if err != nil {
		return gitx.Repo{}, "", "", err
	}
	r, err := a.discoverRepoFor(ctx, got)
	if err != nil {
		return gitx.Repo{}, "", "", err
	}
	return r, got.Entry.Branch, got.Entry.TicketID, nil
}

// confirmContext gates an explicit --context override behind a destructive
// confirmation: acting on a cluster other than the ambient current one is
// never something flow does without being asked twice.
func (a *App) confirmContext(target clusterTarget) error {
	if !target.Overridden {
		return nil
	}
	return a.confirmDestructive(
		fmt.Sprintf("--context overrides the current kube context; act on %q?", target.Context),
		"helm will be passed --kube-context; your kubeconfig is not modified.")
}

func (a *App) verifyChartPresent(ctx context.Context, req versionRequest, version versionResult) error {
	// A version the wait already verified against the index needs no re-check.
	if version.Verified {
		return nil
	}
	if req.RepoUpdate {
		if err := a.ReadHelm().UpdateRepo(ctx, req.HelmRepo); err != nil {
			a.Out.Warn("helm repo update %s failed: %v", req.HelmRepo, err)
		}
	}

	present, err := a.ReadHelm().HasVersion(ctx, req.HelmRepo, req.Chart, version.Version)
	if err != nil {
		return Wrap(ExitDependency, "dependency", err, "searching %s/%s", req.HelmRepo, req.Chart)
	}
	if !present {
		return Precondition(
			"chart %s/%s has no version %s in the index; flow is looking in helm repo %q",
			req.HelmRepo, req.Chart, version.Version, req.HelmRepo)
	}
	return nil
}

func (a *App) buildUpgrade(target clusterTarget, version string, opts upgradeOptions) helmx.UpgradeOptions {
	settings := target.Settings

	valuesFiles := opts.values
	if len(valuesFiles) == 0 && settings.ValuesFile != "" {
		valuesFiles = []string{config.ExpandPath(settings.ValuesFile)}
	}

	extra := settings.ExtraArgs
	switch {
	case opts.reuseValues:
		extra = replaceValuesFlag(extra, "--reuse-values")
	case opts.resetThenReuse:
		extra = replaceValuesFlag(extra, "--reset-then-reuse-values")
	case opts.resetValues:
		extra = replaceValuesFlag(extra, "--reset-values")
	}

	return helmx.UpgradeOptions{
		Release:     settings.ReleaseName,
		Chart:       settings.Chart,
		Namespace:   settings.Namespace,
		Version:     version,
		KubeContext: target.kubeContextArg(),
		ValuesFiles: valuesFiles,
		SetValues:   opts.sets,
		ExtraArgs:   extra,
		Install:     opts.install,
		Wait:        opts.helmWait,
		Atomic:      opts.atomic,
		CreateNS:    opts.createNS,
		Timeout:     opts.timeout,
	}
}

// replaceValuesFlag swaps whichever values-handling flag extra_args carries for
// the one the user asked for, so the three stay mutually exclusive in practice
// as well as on the command line.
func replaceValuesFlag(extra []string, want string) []string {
	out := make([]string, 0, len(extra)+1)
	for _, arg := range extra {
		switch arg {
		case "--reuse-values", "--reset-then-reuse-values", "--reset-values":
			continue
		}
		out = append(out, arg)
	}
	return append(out, want)
}

func (a *App) offerServerSideDryRun(ctx context.Context, upgrade helmx.UpgradeOptions) error {
	a.Out.Status("--dry-run: the command above was not run")
	if a.JSON() || !a.Out.Interactive() {
		return nil
	}

	err := a.confirm("Run it with helm's own --dry-run for a server-side render?", "", false)
	switch {
	case errors.Is(err, output.ErrAborted):
		return nil
	case err != nil:
		return err
	}

	upgrade.DryRun = true
	// Deliberately the base runner: the whole point is to actually run helm.
	if _, err := a.baseRunner.Run(ctx, flowexec.Opts{
		Name: "helm", Args: upgrade.UpgradeArgs(), Stream: true,
	}); err != nil {
		return Wrap(ExitDependency, "helm_failed", err, "helm upgrade --dry-run")
	}
	return nil
}

func (a *App) reportRevision(ctx context.Context, target clusterTarget) {
	rel, err := a.ReadHelm().Status(ctx,
		target.Settings.ReleaseName, target.Settings.Namespace, target.kubeContextArg())
	if err != nil {
		a.Out.Success("helm upgrade completed")
		return
	}
	if rel.ChartVersion == "" {
		a.Out.Success("%s is now at revision %d (%s)", rel.Name, rel.Revision, rel.Status)
		return
	}
	a.Out.Success("%s is now at revision %d (%s), chart %s",
		rel.Name, rel.Revision, rel.Status, rel.ChartVersion)
}

func describeResolution(v versionResult) string {
	out := string(v.Strategy)
	if v.RunID != 0 {
		out += fmt.Sprintf(", run %d", v.RunID)
	}
	if v.Verified {
		out += ", verified against the chart index"
	} else {
		out += ", NOT verified against the chart index"
	}
	return out
}
