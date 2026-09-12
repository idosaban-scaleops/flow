package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/idosaban-scaleops/flow/internal/config"
	"github.com/idosaban-scaleops/flow/internal/gitx"
	"github.com/idosaban-scaleops/flow/internal/output"
)

func newClusterCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Drive Helm against the local dev cluster",
		Long: strings.TrimSpace(`
Commands for installing and inspecting the chart built from a ticket's branch on
the local development cluster.

Settings are keyed by kube context name and learned on first use. The presence
of a clusters entry in the config is the allowlist: flow refuses to upgrade a
context it has never been told about.`),
	}

	cmd.AddCommand(
		newClusterUpgradeCommand(app),
		newClusterVersionCommand(app),
		newClusterWaitCommand(app),
		newClusterStatusCommand(app),
		newClusterHistoryCommand(app),
		newClusterRollbackCommand(app),
		newClusterUninstallCommand(app),
		newClusterValuesCommand(app),
	)
	return cmd
}

// clusterTarget is the fully-resolved description of what a cluster command
// will act on.
type clusterTarget struct {
	Context    string               `json:"kube_context"`
	Server     string               `json:"server,omitempty"`
	Settings   config.ClusterConfig `json:"settings"`
	Overridden bool                 `json:"context_overridden"`
}

// clusterFlags are the per-invocation overrides shared by the cluster commands.
type clusterFlags struct {
	kubeContext string
	release     string
	namespace   string
	helmRepo    string
	chart       string
}

func (c *clusterFlags) register(f *pflag.FlagSet) {
	f.StringVar(&c.kubeContext, "context", "", "kube context to act on")
	f.StringVar(&c.release, "release", "", "helm release name")
	f.StringVar(&c.namespace, "namespace", "", "kubernetes namespace")
	f.StringVar(&c.helmRepo, "helm-repo", "", "local helm repository name")
	f.StringVar(&c.chart, "chart", "", "chart reference, e.g. scaleops/scaleops")
}

// resolveCluster determines the kube context, loads or learns its settings, and
// applies per-invocation overrides. It is the first thing every cluster command
// does, because flow must never act on the wrong cluster.
func (a *App) resolveCluster(ctx context.Context, flags clusterFlags, repoKey string) (clusterTarget, error) {
	k := a.Kube()
	if !k.Available() {
		return clusterTarget{}, Dependency(
			"kubectl is not on PATH, so flow cannot tell which cluster it would be acting on")
	}

	current, err := k.CurrentContext(ctx)
	if err != nil {
		return clusterTarget{}, Wrap(ExitDependency, "dependency", err, "reading the current kube context")
	}

	target := clusterTarget{Context: current}
	if flags.kubeContext != "" && flags.kubeContext != current {
		// Pass --kube-context to helm rather than mutating the kubeconfig.
		target.Context = flags.kubeContext
		target.Overridden = true
	}

	settings, known := a.cfg.Cluster(target.Context)
	if !known {
		settings, err = a.learnCluster(ctx, target.Context, repoKey)
		if err != nil {
			return clusterTarget{}, err
		}
	}

	// Apply per-invocation overrides on top of whatever was configured.
	if flags.release != "" {
		settings.ReleaseName = flags.release
	}
	if flags.namespace != "" {
		settings.Namespace = flags.namespace
	}
	if flags.helmRepo != "" {
		settings.HelmRepo = flags.helmRepo
	}
	if flags.chart != "" {
		settings.Chart = flags.chart
	}
	target.Settings = settings

	if server, err := k.ServerURL(ctx, target.Context); err == nil {
		target.Server = server
	}
	return target, nil
}

// learnCluster runs the first-use interactive setup and writes the result back
// to the config file.
func (a *App) learnCluster(ctx context.Context, contextName, repoKey string) (config.ClusterConfig, error) {
	if !a.Out.Interactive() {
		return config.ClusterConfig{}, Precondition(
			"kube context %q is not configured, and flow cannot prompt here.\n"+
				"Either add a clusters.%s block to %s, or re-run interactively, or supply "+
				"--release, --namespace, --helm-repo and --chart explicitly.",
			contextName, contextName, a.cfgPath)
	}

	a.Out.Heading(fmt.Sprintf("Setting up kube context %q", contextName))

	repos, err := a.ReadHelm().ListRepos(ctx)
	if err != nil {
		return config.ClusterConfig{}, Wrap(ExitDependency, "dependency", err, "listing helm repositories")
	}
	if len(repos) == 0 {
		return config.ClusterConfig{}, Dependency(
			"no helm repositories are configured; add one with `helm repo add <name> <url>` first")
	}

	options := make([]output.SelectOption, 0, len(repos))
	for _, r := range repos {
		options = append(options, output.SelectOption{Label: r.Name + "  " + r.URL, Value: r.Name})
	}
	helmRepo, err := a.Out.Select(
		fmt.Sprintf("Which helm repo hosts the charts for %s?", orDash(repoKey, "this repository")),
		options)
	if err != nil {
		return config.ClusterConfig{}, err
	}

	chartName, err := a.Out.Input("Chart name within that repo", helmRepo)
	if err != nil {
		return config.ClusterConfig{}, err
	}
	release, err := a.Out.Input("Release name", "scaleops")
	if err != nil {
		return config.ClusterConfig{}, err
	}
	namespace, err := a.Out.Input("Namespace", "scaleops-system")
	if err != nil {
		return config.ClusterConfig{}, err
	}
	valuesFile, err := a.Out.Input("Values file",
		fmt.Sprintf("~/Developer/helm-values/%s.yaml", contextName))
	if err != nil {
		return config.ClusterConfig{}, err
	}
	sourceRepo, err := a.Out.Input("Source repo this cluster tracks", repoKey)
	if err != nil {
		return config.ClusterConfig{}, err
	}

	if expanded := config.ExpandPath(valuesFile); expanded != "" && !fileExists(expanded) {
		a.Out.Warn("%s does not exist yet", expanded)
	}

	settings := config.ClusterConfig{
		ReleaseName: release,
		Namespace:   namespace,
		ValuesFile:  valuesFile,
		HelmRepo:    helmRepo,
		Chart:       helmRepo + "/" + chartName,
		ExtraArgs:   []string{"--reset-then-reuse-values"},
		Repo:        sourceRepo,
	}

	if err := a.mutate("write clusters."+contextName+" to "+a.cfgPath, func() error {
		return config.SetCluster(a.cfgPath, contextName, settings)
	}); err != nil {
		return config.ClusterConfig{}, Wrap(ExitFailure, "failure", err, "saving cluster settings")
	}
	a.cfg.Clusters[contextName] = settings
	a.Out.Success("saved clusters.%s to %s", contextName, a.cfgPath)
	return settings, nil
}

// printClusterHeader names exactly which cluster is about to be changed.
func (a *App) printClusterHeader(target clusterTarget, extra map[string]string, order []string) {
	t := a.Out.Theme
	a.Out.Println()
	a.Out.Field("kube context", t.Bold.Render(target.Context))
	if target.Server != "" {
		a.Out.Field("server", target.Server)
	}
	a.Out.Field("release", target.Settings.ReleaseName)
	a.Out.Field("namespace", target.Settings.Namespace)
	for _, key := range order {
		if value := extra[key]; value != "" {
			a.Out.Field(key, value)
		}
	}
	a.Out.Println()
}

// repoForCluster figures out which source repository a cluster command should
// use: the cwd's repository, or the one the cluster's config records.
func (a *App) repoForCluster(ctx context.Context) (gitx.Repo, error) {
	dir, err := cwd()
	if err != nil {
		return gitx.Repo{}, err
	}
	repo, err := gitx.DiscoverRepo(ctx, a.baseRunner, dir, a.cfg.Defaults.Remote)
	if err != nil {
		return gitx.Repo{}, Wrap(ExitDependency, "dependency", err,
			"run this from inside the repository, or pass a ticket argument")
	}
	return repo, nil
}

// helmFor returns the chart coordinates for a cluster, splitting the configured
// "repo/chart" reference.
func helmFor(settings config.ClusterConfig) (helmRepo, chart string) {
	helmRepo = settings.HelmRepo
	chart = settings.Chart
	if repo, name, ok := strings.Cut(chart, "/"); ok {
		if helmRepo == "" {
			helmRepo = repo
		}
		return helmRepo, name
	}
	if chart == "" {
		chart = helmRepo
	}
	return helmRepo, chart
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
