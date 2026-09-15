package cli

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/idosaban-scaleops/flow/internal/ciwait"
)

// waitFlags are the CI-wait knobs, shared by `cluster wait` and
// `cluster upgrade`.
type waitFlags struct {
	noWait         bool
	timeout        time.Duration
	pollInterval   time.Duration
	ignoreFailures bool

	// flags is the set these were registered on, so apply can tell an
	// explicitly passed --poll-interval from the default.
	flags *pflag.FlagSet
}

func (w *waitFlags) register(cmd *cobra.Command, includeNoWait bool) {
	w.flags = cmd.Flags()
	f := w.flags
	if includeNoWait {
		f.BoolVar(&w.noWait, "no-wait", false, "do not wait for CI to finish building the chart")
	}
	f.DurationVar(&w.timeout, "wait-timeout", ciwait.DefaultWaitTimeout, "give up waiting after this long")
	f.DurationVar(&w.pollInterval, "poll-interval", ciwait.DefaultPollInterval,
		"how often to poll GitHub (never faster than 5s)")
	f.BoolVar(&w.ignoreFailures, "ignore-failures", false,
		"keep waiting when a non-essential gating job fails")
}

func (w waitFlags) apply(req *versionRequest) {
	req.Wait = !w.noWait
	req.WaitOpts.Timeout = w.timeout
	req.WaitOpts.PollInterval = w.pollInterval
	req.WaitOpts.PollIntervalSet = w.flags != nil && w.flags.Changed("poll-interval")
	req.WaitOpts.IgnoreFailures = w.ignoreFailures
}

// waitForBuild watches the CI run that will publish the chart, showing live
// progress, and returns once the gating jobs are satisfied.
func (a *App) waitForBuild(ctx context.Context, req versionRequest) (ciwait.Result, error) {
	if !req.Repo.IsGitHub() {
		return ciwait.Result{}, Precondition(
			"repository %s has no GitHub origin remote, so flow cannot watch CI", req.Repo.Key)
	}
	if req.Workflow == "" {
		workflow, err := a.learnBuildWorkflow(ctx, req.Repo)
		if err != nil {
			return ciwait.Result{}, err
		}
		req.Workflow = workflow
	}

	opts := req.WaitOpts
	opts.Owner, opts.Repo = req.Repo.Owner, req.Repo.Name
	opts.WorkflowFile, opts.Branch = req.Workflow, req.Branch
	opts.WaitForJobs = a.cfg.Repo(req.Repo.Key).WaitForJobs

	waiter := ciwait.NewWaiter(a.GitHub(ctx), opts)

	// SIGINT must leave the terminal usable and print the run URL, so the user
	// can pick up in a browser.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	target, err := waiter.Resolve(ctx)
	if err != nil {
		return ciwait.Result{}, mapWaitError(err)
	}

	display := ciwait.NewDisplay(a.Out, target, "")
	defer display.Close()

	// pumped is closed once every event has reached the display. Waiting for
	// it before tearing the display down is what lets the final frame — the
	// 100% one the state machine emits on success — actually get painted:
	// events is buffered, so Run returns as soon as it has handed the last
	// state over, well before the pump has forwarded it.
	events := make(chan ciwait.Event, 16)
	pumped := make(chan struct{})
	go func() {
		defer close(pumped)
		a.pumpEvents(ctx, waiter, display, events)
	}()

	result, err := waiter.Run(ctx, events)
	<-pumped
	display.Close()

	if err != nil {
		if errors.Is(err, context.Canceled) {
			a.Out.Status("wait interrupted; the run is still going: %s", waiter.Target().URL)
			return ciwait.Result{}, Wrap(ExitAborted, "aborted", err, "interrupted")
		}
		return ciwait.Result{}, mapWaitError(err)
	}
	return result, nil
}

// pumpEvents forwards wait events to the display and fetches the ETA once
// there is something to estimate against.
func (a *App) pumpEvents(ctx context.Context, waiter *ciwait.Waiter, display ciwait.Display, events <-chan ciwait.Event) {
	var askedForETA bool
	started := time.Now()

	for event := range events {
		switch {
		case event.State != nil:
			display.Update(*event.State)
			if !askedForETA && event.State.Percent > 0 {
				askedForETA = true
				if live, ok := display.(interface{ SetETA(time.Duration) }); ok {
					if eta, ok2 := waiter.ETA(ctx, time.Since(started)); ok2 {
						live.SetETA(eta)
					}
				}
			}
		case event.Retargeted != nil:
			display.Retarget(*event.Retargeted)
		case event.Note != "":
			display.Note(event.Note)
		}
	}
}

// mapWaitError gives each wait failure the exit code the spec assigns it.
func mapWaitError(err error) error {
	var noRun *ciwait.ErrNoRun
	var failed *ciwait.ErrJobFailed
	var timeout *ciwait.ErrTimeout
	var cancelled *ciwait.ErrCancelled

	switch {
	case errors.As(err, &noRun):
		return Wrap(ExitNotFound, "not_found", err, "")
	case errors.As(err, &failed):
		return Wrap(ExitDependency, "ci_failed", err, "")
	case errors.As(err, &timeout):
		return Wrap(ExitPrecondition, "timeout", err, "")
	case errors.As(err, &cancelled):
		return Wrap(ExitPrecondition, "cancelled", err, "")
	default:
		return Wrap(ExitFailure, "failure", err, "waiting for CI")
	}
}

func newClusterWaitCommand(app *App) *cobra.Command {
	var (
		flags  clusterFlags
		wait   waitFlags
		branch string
	)

	cmd := &cobra.Command{
		Use:   "wait [ticket]",
		Short: "Wait until a branch's chart is installable",
		Long: "Watch the CI jobs that publish the chart and push the images it references,\n" +
			"showing live progress. On success the resolved chart version is printed as\n" +
			"the last line on stdout, so `helm upgrade --version $(flow cluster wait)` works.\n\n" +
			"This does not touch the cluster.",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: app.completeTickets,
	}

	flags.register(cmd.Flags())
	wait.register(cmd, false)
	cmd.Flags().StringVar(&branch, "branch", "", "wait for this branch instead of a ticket's")

	cmd.RunE = app.runArgs(func(ctx context.Context, args []string) error {
		req, err := app.buildVersionRequest(ctx, args, flags, branch, "")
		if err != nil {
			return err
		}
		wait.apply(&req)
		req.Wait = true

		res, err := app.resolveVersion(ctx, req)
		if err != nil {
			return err
		}

		if app.JSON() {
			return app.Out.JSON(res)
		}
		app.Out.Line(res.Version)
		return nil
	})
	return cmd
}
