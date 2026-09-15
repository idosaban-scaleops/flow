package cli

import (
	"context"
	"time"

	"github.com/spf13/cobra"

	"github.com/idosaban-scaleops/flow/internal/chartver"
	"github.com/idosaban-scaleops/flow/internal/ciwait"
	"github.com/idosaban-scaleops/flow/internal/config"
	"github.com/idosaban-scaleops/flow/internal/ghapi"
	"github.com/idosaban-scaleops/flow/internal/gitx"
	"github.com/idosaban-scaleops/flow/internal/helmx"
)

// propagation bounds the post-wait check that the chart actually reached the
// index. CI reporting success and the index not carrying the chart is almost
// always a misconfigured helm repo, not slow propagation.
const (
	propagationAttempts = 6
	propagationGap      = 10 * time.Second
)

// versionRequest is everything needed to resolve a chart version.
type versionRequest struct {
	Repo       gitx.Repo
	Branch     string
	HelmRepo   string
	Chart      string
	Workflow   string
	Override   string
	RepoUpdate bool
	// Wait enables the CI wait; it is on by default for upgrade.
	Wait     bool
	WaitOpts ciwait.Options
}

// versionResult carries the resolution plus any CI wait that produced it.
type versionResult struct {
	chartver.Resolution
	CIWait *ciWaitPayload `json:"ci_wait,omitempty"`
}

type ciWaitPayload struct {
	RunID   int64            `json:"run_id"`
	RunURL  string           `json:"run_url,omitempty"`
	Jobs    []ciwait.JobView `json:"jobs"`
	Total   string           `json:"total_wait,omitempty"`
	Skipped bool             `json:"skipped,omitempty"`
}

// helmIndex adapts helmx to the narrow interface chartver needs.
type helmIndex struct{ h *helmx.Helm }

func (i helmIndex) UpdateRepo(ctx context.Context, repo string) error {
	return i.h.UpdateRepo(ctx, repo)
}

func (i helmIndex) SearchVersions(ctx context.Context, repo, chart string) ([]chartver.VersionEntry, error) {
	versions, err := i.h.SearchVersions(ctx, repo, chart)
	if err != nil {
		return nil, err
	}
	out := make([]chartver.VersionEntry, 0, len(versions))
	for _, v := range versions {
		out = append(out, chartver.VersionEntry{Version: v.Version})
	}
	return out, nil
}

// resolveVersion runs the strategies in order and reports which one won.
func (a *App) resolveVersion(ctx context.Context, req versionRequest) (versionResult, error) {
	// Strategy C: an explicit version short-circuits everything.
	if req.Override != "" {
		res := versionResult{Resolution: chartver.Resolution{
			Version: req.Override, Strategy: chartver.StrategyExplicit,
		}}
		if runID, ok := chartver.RunIDOf(req.Override); ok {
			res.RunID = runID
		}
		return res, nil
	}

	if req.Wait {
		return a.resolveByWaiting(ctx, req)
	}
	return a.resolveWithoutWaiting(ctx, req)
}

// resolveByWaiting waits for CI, which pins the run ID, which pins the exact
// chart version — so the resolution is verified by construction rather than by
// heuristic.
func (a *App) resolveByWaiting(ctx context.Context, req versionRequest) (versionResult, error) {
	result, err := a.waitForBuild(ctx, req)
	if err != nil {
		return versionResult{}, err
	}

	helm := a.ReadHelm()
	idx := helmIndex{helm}

	version, runID, found, searchErr := chartver.Search(ctx, idx,
		req.HelmRepo, req.Chart, req.Branch, req.RepoUpdate)

	if found && runID == result.Target.RunID {
		return versionResult{
			Resolution: chartver.Resolution{
				Version: version, Strategy: chartver.StrategyRun,
				RunID: runID, RunURL: result.Target.URL, Verified: true,
			},
			CIWait: waitPayload(result),
		}, nil
	}

	// The run succeeded but the index does not carry it yet. Retry a few times,
	// then say plainly what the combination usually means.
	version, err = a.awaitPropagation(ctx, idx, req, result.Target.RunID)
	if err != nil {
		if searchErr != nil {
			a.Out.Log.Debug("chart index search failed", "err", searchErr)
		}
		return versionResult{}, err
	}
	return versionResult{
		Resolution: chartver.Resolution{
			Version: version, Strategy: chartver.StrategyRun,
			RunID: result.Target.RunID, RunURL: result.Target.URL, Verified: true,
		},
		CIWait: waitPayload(result),
	}, nil
}

func waitPayload(result ciwait.Result) *ciWaitPayload {
	return &ciWaitPayload{
		RunID: result.Target.RunID, RunURL: result.Target.URL,
		Jobs: result.Jobs, Total: result.Total,
	}
}

// awaitPropagation re-checks the chart index until the pinned run's version
// appears.
func (a *App) awaitPropagation(ctx context.Context, idx helmIndex, req versionRequest, runID int64) (string, error) {
	for attempt := range propagationAttempts {
		if attempt > 0 {
			a.Out.Status("waiting for chart index to propagate… (%d/%d)", attempt+1, propagationAttempts)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(propagationGap):
			}
		}

		version, gotRunID, found, err := chartver.Search(ctx, idx, req.HelmRepo, req.Chart, req.Branch, true)
		if err != nil {
			a.Out.Log.Debug("chart index search failed", "err", err)
			continue
		}
		if found && gotRunID == runID {
			return version, nil
		}
	}

	return "", Precondition(
		"CI reported success for run %d, but chart %s/%s has no version for branch %s in the index.\n"+
			"That combination almost always means the wrong helm repo is configured for this "+
			"repository — flow is looking in %q.",
		runID, req.HelmRepo, req.Chart, req.Branch, req.HelmRepo)
}

// resolveWithoutWaiting is --no-wait: Strategy A, then Strategy B's local
// derivation, failing fast rather than waiting.
func (a *App) resolveWithoutWaiting(ctx context.Context, req versionRequest) (versionResult, error) {
	idx := helmIndex{a.ReadHelm()}

	version, runID, found, err := chartver.Search(ctx, idx, req.HelmRepo, req.Chart, req.Branch, req.RepoUpdate)
	if err != nil {
		a.Out.Log.Debug("chart index search failed", "err", err)
	}

	// Strategy B: cross-check against the newest successful CI run.
	newest, runErr := a.newestSuccessfulRun(ctx, req)

	switch {
	case found && runErr == nil && newest.ID == runID:
		return versionResult{Resolution: chartver.Resolution{
			Version: version, Strategy: chartver.StrategyIndex,
			RunID: runID, RunURL: newest.HTMLURL, Verified: true,
		}}, nil

	case found:
		return versionResult{Resolution: chartver.Resolution{
			Version: version, Strategy: chartver.StrategyIndex, RunID: runID, Verified: true,
		}}, nil

	case runErr == nil:
		derived, deriveErr := a.deriveVersion(ctx, req, newest.ID)
		if deriveErr != nil {
			return versionResult{}, deriveErr
		}
		a.Out.Warn("derived %s from local tags; it was not verified against %s",
			derived, req.HelmRepo)
		return versionResult{Resolution: chartver.Resolution{
			Version: derived, Strategy: chartver.StrategyDerived,
			RunID: newest.ID, RunURL: newest.HTMLURL, Verified: false,
		}}, nil

	default:
		return versionResult{}, runErr
	}
}

// newestSuccessfulRun finds the newest completed, successful run, and explains
// clearly when the newest run is still in flight.
func (a *App) newestSuccessfulRun(ctx context.Context, req versionRequest) (ghapi.Run, error) {
	if !req.Repo.IsGitHub() {
		return ghapi.Run{}, Precondition(
			"repository %s has no GitHub origin remote, so flow cannot look up CI runs", req.Repo.Key)
	}
	if req.Workflow == "" {
		workflow, err := a.learnBuildWorkflow(ctx, req.Repo)
		if err != nil {
			return ghapi.Run{}, err
		}
		req.Workflow = workflow
	}

	runs, err := a.GitHub(ctx).ListWorkflowRuns(ctx,
		req.Repo.Owner, req.Repo.Name, req.Workflow, req.Branch, 20)
	if err != nil {
		return ghapi.Run{}, Wrap(ExitDependency, "dependency", err, "listing CI runs for %s", req.Branch)
	}
	if len(runs) == 0 {
		return ghapi.Run{}, NotFound(
			"no %s runs found for branch %s; push the branch and wait for a build",
			req.Workflow, req.Branch)
	}

	for _, r := range runs {
		if r.Succeeded() {
			return r, nil
		}
	}
	return ghapi.Run{}, Precondition(
		"latest run %d is %s; re-run without --no-wait to wait for it, or pass --version",
		runs[0].ID, runs[0].Status)
}

// deriveVersion reproduces the CI's formula locally, from the newest git tag.
func (a *App) deriveVersion(ctx context.Context, req versionRequest, runID int64) (string, error) {
	git := a.ReadGit(req.Repo.Root)
	if err := a.Git(req.Repo.Root).FetchTags(ctx, a.cfg.Defaults.Remote); err != nil {
		a.Out.Log.Debug("fetching tags failed", "err", err)
	}

	tag, err := git.LatestTag(ctx)
	if err != nil {
		return "", Wrap(ExitPrecondition, "precondition_failed", err,
			"deriving a chart version needs at least one git tag")
	}
	version, err := chartver.Derive(tag, req.Branch, runID)
	if err != nil {
		return "", Wrap(ExitPrecondition, "precondition_failed", err, "deriving a chart version")
	}
	return version, nil
}

// versionRequestFor assembles a request from config, flags, and a branch.
func (a *App) versionRequestFor(repo gitx.Repo, settings config.ClusterConfig, branch string) versionRequest {
	helmRepo, chart := helmFor(settings)
	repoCfg := a.cfg.Repo(repo.Key)

	if helmRepo == "" {
		helmRepo = repoCfg.HelmRepo
	}
	if chart == "" {
		chart = repoCfg.ChartName
	}
	return versionRequest{
		Repo: repo, Branch: branch,
		HelmRepo: helmRepo, Chart: chart,
		Workflow:   repoCfg.BuildWorkflow,
		RepoUpdate: true,
	}
}

func newClusterVersionCommand(app *App) *cobra.Command {
	var (
		flags    clusterFlags
		branch   string
		wait     bool
		override string
	)

	cmd := &cobra.Command{
		Use:   "version [ticket]",
		Short: "Print the chart version CI built for a branch",
		Long: "Resolve and print the chart version for a ticket's branch, one bare line\n" +
			"on stdout. --json reports the full resolution: which strategy won, the CI\n" +
			"run it came from, and whether it was verified against the chart index.\n\n" +
			"This command does not wait for CI. Pass --wait, or use `flow cluster wait`,\n" +
			"to block until the branch's chart is installable.",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: app.completeTickets,
	}

	f := cmd.Flags()
	flags.register(f)
	f.StringVar(&branch, "branch", "", "resolve for this branch instead of a ticket's")
	f.BoolVar(&wait, "wait", false, "wait for CI to finish before resolving (see `flow cluster wait`)")
	f.StringVar(&override, "version", "", "short-circuit resolution with an explicit version")

	cmd.RunE = app.runArgs(func(ctx context.Context, args []string) error {
		req, err := app.buildVersionRequest(ctx, args, flags, branch, override)
		if err != nil {
			return err
		}
		req.Wait = wait

		res, err := app.resolveVersion(ctx, req)
		if err != nil {
			return err
		}

		if app.JSON() {
			return app.Out.JSON(res)
		}
		app.Out.Log.Info("resolved chart version", "strategy", res.Strategy, "verified", res.Verified)
		app.Out.Line(res.Version)
		return nil
	})
	return cmd
}

// buildVersionRequest resolves the branch (from a ticket, or --branch) and the
// chart coordinates.
func (a *App) buildVersionRequest(
	ctx context.Context, args []string, flags clusterFlags, branchOverride, override string,
) (versionRequest, error) {
	var (
		repo   gitx.Repo
		branch = branchOverride
		err    error
	)

	if branch == "" {
		got, resolveErr := a.resolveTicket(ctx, first(args))
		if resolveErr != nil {
			return versionRequest{}, resolveErr
		}
		branch = got.Entry.Branch
		repo, err = a.discoverRepoFor(ctx, got)
		if err != nil {
			return versionRequest{}, err
		}
	} else {
		repo, err = a.repoForCluster(ctx)
		if err != nil {
			return versionRequest{}, err
		}
	}

	target, err := a.resolveCluster(ctx, flags, repo.Key)
	if err != nil {
		return versionRequest{}, err
	}

	req := a.versionRequestFor(repo, target.Settings, branch)
	req.Override = override
	if req.HelmRepo == "" || req.Chart == "" {
		return versionRequest{}, Precondition(
			"no helm repo or chart is configured for %s; set clusters.%s.helm_repo and "+
				"clusters.%s.chart in %s, or pass --helm-repo and --chart",
			repo.Key, target.Context, target.Context, a.cfgPath)
	}
	return req, nil
}
