package ciwait

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/idosaban-scaleops/flow/internal/ghapi"
)

// Polling intervals. The floor is deliberate: polling faster than 5s burns
// rate limit without telling the user anything new.
const (
	MinPollInterval     = 5 * time.Second
	DefaultPollInterval = 10 * time.Second
	QueuedPollInterval  = 15 * time.Second
	MaxPollInterval     = 30 * time.Second
	DefaultWaitTimeout  = 45 * time.Minute
)

// RateLimitFloor is the remaining-request budget below which the wait widens
// its poll interval rather than spending what is left of the quota.
const RateLimitFloor = 100

// Options configure a wait.
type Options struct {
	Owner        string
	Repo         string
	WorkflowFile string
	Branch       string
	// RunID pins the run to watch. Zero means "find the newest run".
	RunID int64
	// WaitForJobs are the gating job-name prefixes.
	WaitForJobs []string

	PollInterval   time.Duration
	Timeout        time.Duration
	IgnoreFailures bool
}

// Target identifies the run currently being watched.
type Target struct {
	RunID   int64  `json:"run_id"`
	Attempt int    `json:"run_attempt,omitempty"`
	URL     string `json:"url,omitempty"`
	Name    string `json:"workflow,omitempty"`
	Branch  string `json:"branch,omitempty"`
}

// Event is one update from the wait loop. Exactly one of the fields is set.
type Event struct {
	// State is a new progress snapshot.
	State *State
	// Retargeted reports that the watched run was replaced.
	Retargeted *Retarget
	// Note carries a one-off message, such as a rate-limit warning.
	Note string
	// Err is a terminal error.
	Err error
}

// Retarget records a switch to a different run or attempt.
type Retarget struct {
	From Target
	To   Target
	// Reason explains the switch, for display.
	Reason string
}

// Result is the outcome of a completed wait.
type Result struct {
	Target  Target        `json:"target"`
	Jobs    []JobView     `json:"jobs"`
	Elapsed time.Duration `json:"-"`
	Total   string        `json:"total_wait,omitempty"`
}

// ErrTimeout reports that the wait exceeded its budget.
type ErrTimeout struct {
	Target  Target
	Elapsed time.Duration
}

func (e *ErrTimeout) Error() string {
	return fmt.Sprintf("timed out after %s waiting for run %d (%s)",
		FormatDuration(e.Elapsed), e.Target.RunID, e.Target.URL)
}

// ErrJobFailed reports a gating job that failed.
type ErrJobFailed struct {
	Job  string
	Step string
	URL  string
}

func (e *ErrJobFailed) Error() string {
	if e.Step != "" {
		return fmt.Sprintf("CI job %q failed at step %q (%s)", e.Job, e.Step, e.URL)
	}
	return fmt.Sprintf("CI job %q failed (%s)", e.Job, e.URL)
}

// ErrNoRun reports a branch with no workflow runs. This is a normal state for
// an unpushed branch, and the message says exactly what to do about it.
type ErrNoRun struct {
	Branch   string
	Workflow string
}

func (e *ErrNoRun) Error() string {
	return fmt.Sprintf("no %s runs found for branch %q; push the branch and wait for a build to start",
		e.Workflow, e.Branch)
}

// ErrCancelled reports a cancelled run with no successor to retarget onto.
type ErrCancelled struct{ Target Target }

func (e *ErrCancelled) Error() string {
	return fmt.Sprintf("run %d was cancelled and no newer run exists (%s)",
		e.Target.RunID, e.Target.URL)
}

// Waiter polls GitHub and emits progress events.
type Waiter struct {
	API  ghapi.API
	Opts Options

	// now and sleep are injected so the loop is testable without wall time.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error

	machine *Machine
	target  Target
	started time.Time

	// warnedRateLimit keeps the low-budget notice to one line, since the
	// condition persists across every subsequent poll.
	warnedRateLimit bool
}

// NewWaiter builds a Waiter.
func NewWaiter(api ghapi.API, opts Options) *Waiter {
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.PollInterval < MinPollInterval {
		opts.PollInterval = MinPollInterval
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultWaitTimeout
	}

	m := NewMachine(opts.WaitForJobs)
	m.IgnoreFailures = opts.IgnoreFailures

	return &Waiter{
		API:     api,
		Opts:    opts,
		machine: m,
		now:     time.Now,
		sleep:   sleepCtx,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Target reports the run currently being watched.
func (w *Waiter) Target() Target { return w.target }

// ETA estimates the remaining time from the median of recent successful runs.
// It returns false rather than a wild guess when there is no history.
func (w *Waiter) ETA(ctx context.Context, elapsed time.Duration) (time.Duration, bool) {
	durations, err := w.API.SuccessfulRunDurations(ctx,
		w.Opts.Owner, w.Opts.Repo, w.Opts.WorkflowFile, w.Opts.Branch, 10)
	if err != nil || len(durations) == 0 {
		return 0, false
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	median := durations[len(durations)/2]
	if remaining := median - elapsed; remaining > 0 {
		return remaining, true
	}
	return 0, false
}

// Resolve finds the run to watch: the pinned one, or the newest on the branch.
func (w *Waiter) Resolve(ctx context.Context) (Target, error) {
	runs, err := w.API.ListWorkflowRuns(ctx,
		w.Opts.Owner, w.Opts.Repo, w.Opts.WorkflowFile, w.Opts.Branch, 20)
	if err != nil {
		return Target{}, err
	}
	if len(runs) == 0 {
		return Target{}, &ErrNoRun{Branch: w.Opts.Branch, Workflow: w.Opts.WorkflowFile}
	}

	if w.Opts.RunID != 0 {
		for _, r := range runs {
			if r.ID == w.Opts.RunID {
				return targetOf(r), nil
			}
		}
		return Target{}, fmt.Errorf("run %d not found on branch %q", w.Opts.RunID, w.Opts.Branch)
	}
	return targetOf(newest(runs)), nil
}

func targetOf(r ghapi.Run) Target {
	return Target{RunID: r.ID, Attempt: r.Attempt, URL: r.HTMLURL, Name: r.Name, Branch: r.Branch}
}

func newest(runs []ghapi.Run) ghapi.Run {
	best := runs[0]
	for _, r := range runs[1:] {
		if r.CreatedAt.After(best.CreatedAt) {
			best = r
		}
	}
	return best
}

// Run polls until the gating jobs finish, emitting an Event on every poll. The
// channel is closed when the wait ends; the final event carries either Err or
// a terminal State.
//
// SIGINT is handled by the caller cancelling ctx: the loop returns
// context.Canceled promptly, so the renderer can restore the terminal.
func (w *Waiter) Run(ctx context.Context, events chan<- Event) (Result, error) {
	defer close(events)

	w.started = w.now()
	deadline := w.started.Add(w.Opts.Timeout)

	target, err := w.Resolve(ctx)
	if err != nil {
		return Result{}, err
	}
	w.target = target

	for {
		if w.now().After(deadline) {
			return Result{}, &ErrTimeout{Target: w.target, Elapsed: w.now().Sub(w.started)}
		}

		jobs, err := w.API.ListRunJobs(ctx, w.Opts.Owner, w.Opts.Repo, w.target.RunID)
		if err != nil {
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			var rate *ghapi.RateLimitError
			if errors.As(err, &rate) {
				emit(ctx, events, Event{Note: rate.Error() + "; widening the poll interval"})
				if sleepErr := w.sleep(ctx, MaxPollInterval); sleepErr != nil {
					return Result{}, sleepErr
				}
				continue
			}
			return Result{}, err
		}

		state := w.machine.Update(jobs)
		emit(ctx, events, Event{State: &state})
		w.noteRateLimit(ctx, events)

		switch state.Phase {
		case PhaseSucceeded:
			return Result{
				Target:  w.target,
				Jobs:    state.Jobs,
				Elapsed: w.now().Sub(w.started),
				Total:   FormatDuration(w.now().Sub(w.started)),
			}, nil

		case PhaseFailed:
			return Result{}, &ErrJobFailed{
				Job: state.FailedJob, Step: state.FailedStep, URL: state.FailedURL,
			}

		case PhaseCancelled:
			// cancel-in-progress is set for non-main refs, so pushing again
			// cancels the run being watched. Follow the newer run rather than
			// reporting a failure the user already fixed.
			if err := w.retarget(ctx, events, "was cancelled by a newer push"); err != nil {
				return Result{}, err
			}
			continue
		}

		w.checkAttempt(ctx, events)

		if err := w.sleep(ctx, w.interval(state)); err != nil {
			return Result{}, err
		}
	}
}

// checkAttempt follows a re-run of the same workflow. A lookup failure here is
// deliberately silent: the next poll retries, and ending the wait over it would
// be worse than watching a stale attempt for one more interval.
func (w *Waiter) checkAttempt(ctx context.Context, events chan<- Event) {
	runs, err := w.API.ListWorkflowRuns(ctx,
		w.Opts.Owner, w.Opts.Repo, w.Opts.WorkflowFile, w.Opts.Branch, 5)
	if err != nil {
		return
	}
	for _, r := range runs {
		if r.ID != w.target.RunID || r.Attempt <= w.target.Attempt {
			continue
		}
		from := w.target
		w.target = targetOf(r)
		w.machine.Retarget()
		emit(ctx, events, Event{Retargeted: &Retarget{
			From: from, To: w.target,
			Reason: fmt.Sprintf("was re-run (attempt %d)", r.Attempt),
		}})
		return
	}
}

// retarget switches to the newest run on the branch, or fails if there is none.
func (w *Waiter) retarget(ctx context.Context, events chan<- Event, reason string) error {
	runs, err := w.API.ListWorkflowRuns(ctx,
		w.Opts.Owner, w.Opts.Repo, w.Opts.WorkflowFile, w.Opts.Branch, 20)
	if err != nil {
		return err
	}

	var candidate *ghapi.Run
	for i := range runs {
		if runs[i].ID == w.target.RunID {
			continue
		}
		if candidate == nil || runs[i].CreatedAt.After(candidate.CreatedAt) {
			candidate = &runs[i]
		}
	}
	if candidate == nil || candidate.ID < w.target.RunID {
		return &ErrCancelled{Target: w.target}
	}

	from := w.target
	w.target = targetOf(*candidate)
	w.machine.Retarget()
	emit(ctx, events, Event{Retargeted: &Retarget{From: from, To: w.target, Reason: reason}})
	return nil
}

// noteRateLimit reports a dwindling request budget once, so the status line
// explains why the display slowed down.
func (w *Waiter) noteRateLimit(ctx context.Context, events chan<- Event) {
	if w.warnedRateLimit || !w.rateLimitLow() {
		return
	}
	w.warnedRateLimit = true

	remaining, reset := w.API.LastRate()
	note := fmt.Sprintf("GitHub rate limit is low (%d requests left); polling every %s",
		remaining, MaxPollInterval)
	if !reset.IsZero() {
		note += ", resets at " + reset.Local().Format("15:04")
	}
	emit(ctx, events, Event{Note: note})
}

// rateLimitLow reports a budget worth slowing down for. A budget of -1 means
// nothing has been observed yet.
func (w *Waiter) rateLimitLow() bool {
	remaining, _ := w.API.LastRate()
	return remaining >= 0 && remaining < RateLimitFloor
}

// interval adapts the poll rate to what the run is doing: faster while work is
// visibly happening, slower while everything is still queued. A low rate-limit
// budget overrides all of that and pins the interval to the maximum.
func (w *Waiter) interval(state State) time.Duration {
	if w.rateLimitLow() {
		return MaxPollInterval
	}

	base := w.Opts.PollInterval

	var running, queued int
	for _, j := range state.Jobs {
		switch j.Status {
		case ghapi.StatusInProgress:
			running++
		case ghapi.StatusQueued, ghapi.StatusWaiting, ghapi.StatusPending:
			queued++
		}
	}

	switch {
	case running > 0:
		base = MinPollInterval
	case queued > 0 && running == 0:
		base = QueuedPollInterval
	}

	if base < MinPollInterval {
		base = MinPollInterval
	}
	if base > MaxPollInterval {
		base = MaxPollInterval
	}
	return base
}

// emit sends an event without blocking a cancelled wait.
func emit(ctx context.Context, events chan<- Event, e Event) {
	select {
	case events <- e:
	case <-ctx.Done():
	}
}
