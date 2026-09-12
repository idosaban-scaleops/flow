package ciwait_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/idosaban-scaleops/flow/internal/ciwait"
	"github.com/idosaban-scaleops/flow/internal/ghapi"
)

// fakeAPI replays scripted responses so the wait loop is testable without a
// network, and without wall-clock time.
type fakeAPI struct {
	runs [][]ghapi.Run
	jobs [][]ghapi.Job

	runCall int
	jobCall int

	jobErr    error
	durations []time.Duration

	remaining int
	resetAt   time.Time
}

func (f *fakeAPI) FindPR(context.Context, string, string, string) (ghapi.PRInfo, error) {
	return ghapi.PRInfo{State: ghapi.PRNone}, nil
}

func (f *fakeAPI) ListWorkflowRuns(_ context.Context, _, _, _, _ string, _ int) ([]ghapi.Run, error) {
	if len(f.runs) == 0 {
		return nil, nil
	}
	i := min(f.runCall, len(f.runs)-1)
	f.runCall++
	return f.runs[i], nil
}

func (f *fakeAPI) ListRunJobs(context.Context, string, string, int64) ([]ghapi.Job, error) {
	if f.jobErr != nil {
		err := f.jobErr
		f.jobErr = nil
		return nil, err
	}
	if len(f.jobs) == 0 {
		return nil, nil
	}
	i := min(f.jobCall, len(f.jobs)-1)
	f.jobCall++
	return f.jobs[i], nil
}

func (f *fakeAPI) SuccessfulRunDurations(context.Context, string, string, string, string, int) ([]time.Duration, error) {
	return f.durations, nil
}

func (f *fakeAPI) AuthenticatedLogin(context.Context) (string, error) { return "test", nil }
func (f *fakeAPI) HasToken() bool                                     { return true }

// LastRate reports -1 when a test does not care about the budget, which is what
// "nothing observed yet" means.
func (f *fakeAPI) LastRate() (int, time.Time) {
	if f.remaining == 0 {
		return -1, time.Time{}
	}
	return f.remaining, f.resetAt
}

func run(id int64, created time.Time, attempt int) ghapi.Run {
	return ghapi.Run{
		ID: id, Name: "Build And Release", Branch: "RD-1-a", Attempt: attempt,
		CreatedAt: created, HTMLURL: "https://github.com/o/r/actions/runs/1",
	}
}

// drain collects every event a wait emits.
func drain(t *testing.T, w *ciwait.Waiter) (ciwait.Result, []ciwait.Event, error) {
	t.Helper()
	events := make(chan ciwait.Event, 64)

	type outcome struct {
		res ciwait.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := w.Run(context.Background(), events)
		done <- outcome{res, err}
	}()

	var collected []ciwait.Event
	for e := range events {
		collected = append(collected, e)
	}
	got := <-done
	return got.res, collected, got.err
}

// waiter builds a Waiter whose sleeps are instant, so the tests exercise the
// loop's logic rather than its clock. The adaptive interval itself is asserted
// directly in TestAdaptivePollInterval.
func waiter(api ghapi.API) *ciwait.Waiter {
	w := ciwait.NewWaiter(api, ciwait.Options{
		Owner: "o", Repo: "r", WorkflowFile: "go.yaml", Branch: "RD-1-a",
		WaitForJobs: gatingJobs,
	})
	ciwait.SetSleepForTest(w, func(ctx context.Context, _ time.Duration) error {
		return ctx.Err()
	})
	return w
}

func TestAdaptivePollInterval(t *testing.T) {
	base := ciwait.NewWaiter(&fakeAPI{}, ciwait.Options{PollInterval: 10 * time.Second})

	tests := []struct {
		name  string
		state ciwait.State
		want  time.Duration
	}{
		{
			name:  "everything queued polls slowly",
			state: ciwait.State{Jobs: []ciwait.JobView{{Status: ghapi.StatusQueued}}},
			want:  ciwait.QueuedPollInterval,
		},
		{
			name: "work in progress polls at the floor",
			state: ciwait.State{Jobs: []ciwait.JobView{
				{Status: ghapi.StatusQueued}, {Status: ghapi.StatusInProgress},
			}},
			want: ciwait.MinPollInterval,
		},
		{
			name:  "nothing pending uses the configured interval",
			state: ciwait.State{Jobs: []ciwait.JobView{{Status: ghapi.StatusCompleted}}},
			want:  10 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ciwait.IntervalForTest(base, tt.state); got != tt.want {
				t.Errorf("interval = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPollIntervalNeverGoesBelowTheFloor(t *testing.T) {
	w := ciwait.NewWaiter(&fakeAPI{}, ciwait.Options{PollInterval: time.Millisecond})
	if got := ciwait.IntervalForTest(w, ciwait.State{}); got < ciwait.MinPollInterval {
		t.Errorf("interval = %v, want at least %v", got, ciwait.MinPollInterval)
	}
}

func TestWaitSucceeds(t *testing.T) {
	now := time.Now()
	api := &fakeAPI{
		runs: [][]ghapi.Run{{run(100, now, 1)}},
		jobs: [][]ghapi.Job{
			{
				job("Pre Release Helm", ghapi.StatusInProgress, ""),
				job("Pre Release Images", ghapi.StatusQueued, ""),
			},
			{
				job("Pre Release Helm", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
				job("Pre Release Images", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
			},
		},
	}

	res, events, err := drain(t, waiter(api))
	if err != nil {
		t.Fatal(err)
	}
	if res.Target.RunID != 100 {
		t.Errorf("target run = %d", res.Target.RunID)
	}
	if len(res.Jobs) != 2 {
		t.Errorf("want 2 job views in the result, got %d", len(res.Jobs))
	}
	if len(events) < 2 {
		t.Errorf("want a progress event per poll, got %d", len(events))
	}
}

func TestWaitWithNoRunsReportsHowToFix(t *testing.T) {
	_, _, err := drain(t, waiter(&fakeAPI{}))

	var noRun *ciwait.ErrNoRun
	if !errors.As(err, &noRun) {
		t.Fatalf("err = %v, want *ciwait.ErrNoRun", err)
	}
	if got := err.Error(); !contains(got, "push the branch") {
		t.Errorf("the message must say what to do, got %q", got)
	}
}

func TestWaitFailsOnGatingJobFailure(t *testing.T) {
	now := time.Now()
	api := &fakeAPI{
		runs: [][]ghapi.Run{{run(100, now, 1)}},
		jobs: [][]ghapi.Job{{
			job("Pre Release Helm", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
			job("Pre Release Images", ghapi.StatusCompleted, ghapi.ConclusionFailure,
				step("Push image", ghapi.StatusCompleted, ghapi.ConclusionFailure)),
		}},
	}

	_, _, err := drain(t, waiter(api))
	var failed *ciwait.ErrJobFailed
	if !errors.As(err, &failed) {
		t.Fatalf("err = %v, want *ciwait.ErrJobFailed", err)
	}
	if failed.Step != "Push image" {
		t.Errorf("failed step = %q", failed.Step)
	}
}

func TestWaitRetargetsAfterCancellation(t *testing.T) {
	now := time.Now()
	older := run(100, now, 1)
	newer := run(200, now.Add(time.Minute), 1)

	api := &fakeAPI{
		// First resolve sees only the old run; the retarget lookup sees both.
		runs: [][]ghapi.Run{{older}, {older, newer}},
		jobs: [][]ghapi.Job{
			{
				job("Pre Release Helm", ghapi.StatusCompleted, ghapi.ConclusionCancelled),
				job("Pre Release Images", ghapi.StatusCompleted, ghapi.ConclusionCancelled),
			},
			{
				job("Pre Release Helm", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
				job("Pre Release Images", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
			},
		},
	}

	res, events, err := drain(t, waiter(api))
	if err != nil {
		t.Fatal(err)
	}
	if res.Target.RunID != 200 {
		t.Errorf("final target = %d, want the newer run 200", res.Target.RunID)
	}

	var retargeted bool
	for _, e := range events {
		if e.Retargeted != nil {
			retargeted = true
			if e.Retargeted.From.RunID != 100 || e.Retargeted.To.RunID != 200 {
				t.Errorf("retarget = %+v", e.Retargeted)
			}
		}
	}
	if !retargeted {
		t.Error("a cancelled run must produce a visible retarget notice")
	}
}

func TestWaitFailsWhenCancelledWithNoSuccessor(t *testing.T) {
	now := time.Now()
	api := &fakeAPI{
		runs: [][]ghapi.Run{{run(100, now, 1)}},
		jobs: [][]ghapi.Job{{
			job("Pre Release Helm", ghapi.StatusCompleted, ghapi.ConclusionCancelled),
		}},
	}

	_, _, err := drain(t, waiter(api))
	var cancelled *ciwait.ErrCancelled
	if !errors.As(err, &cancelled) {
		t.Fatalf("err = %v, want *ciwait.ErrCancelled", err)
	}
}

func TestWaitFollowsARerun(t *testing.T) {
	now := time.Now()
	api := &fakeAPI{
		runs: [][]ghapi.Run{
			{run(100, now, 1)},
			{run(100, now, 2)}, // attempt incremented mid-wait
		},
		jobs: [][]ghapi.Job{
			{job("Pre Release Helm", ghapi.StatusInProgress, "")},
			{job("Pre Release Helm", ghapi.StatusCompleted, ghapi.ConclusionSuccess)},
		},
	}

	res, events, err := drain(t, waiter(api))
	if err != nil {
		t.Fatal(err)
	}
	if res.Target.Attempt != 2 {
		t.Errorf("attempt = %d, want 2", res.Target.Attempt)
	}

	var sawRetarget bool
	for _, e := range events {
		if e.Retargeted != nil && contains(e.Retargeted.Reason, "re-run") {
			sawRetarget = true
		}
	}
	if !sawRetarget {
		t.Error("a new run attempt must be announced")
	}
}

func TestWaitWidensIntervalOnRateLimit(t *testing.T) {
	now := time.Now()
	api := &fakeAPI{
		runs:   [][]ghapi.Run{{run(100, now, 1)}},
		jobErr: &ghapi.RateLimitError{ResetAt: now.Add(time.Minute)},
		jobs: [][]ghapi.Job{{
			job("Pre Release Helm", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
		}},
	}

	w := ciwait.NewWaiter(api, ciwait.Options{
		Owner: "o", Repo: "r", WorkflowFile: "go.yaml", Branch: "RD-1-a",
		WaitForJobs: gatingJobs,
	})
	ciwait.SetSleepForTest(w, func(context.Context, time.Duration) error { return nil })

	_, events, err := drain(t, w)
	if err != nil {
		t.Fatalf("a rate limit must widen the interval, not end the wait: %v", err)
	}

	var noted bool
	for _, e := range events {
		if contains(e.Note, "rate limit") {
			noted = true
		}
	}
	if !noted {
		t.Error("the rate limit should be reported in the status line")
	}
}

func TestWaitTimesOut(t *testing.T) {
	now := time.Now()
	api := &fakeAPI{
		runs: [][]ghapi.Run{{run(100, now, 1)}},
		jobs: [][]ghapi.Job{{job("Pre Release Helm", ghapi.StatusInProgress, "")}},
	}

	w := ciwait.NewWaiter(api, ciwait.Options{
		Owner: "o", Repo: "r", WorkflowFile: "go.yaml", Branch: "RD-1-a",
		WaitForJobs: gatingJobs, Timeout: time.Nanosecond,
	})
	ciwait.SetSleepForTest(w, func(context.Context, time.Duration) error { return nil })

	_, _, err := drain(t, w)
	var timeout *ciwait.ErrTimeout
	if !errors.As(err, &timeout) {
		t.Fatalf("err = %v, want *ciwait.ErrTimeout", err)
	}
	if !contains(err.Error(), "actions/runs") {
		t.Errorf("the timeout message must carry the run URL, got %q", err)
	}
}

func TestWaitCancellationIsPrompt(t *testing.T) {
	now := time.Now()
	api := &fakeAPI{
		runs: [][]ghapi.Run{{run(100, now, 1)}},
		jobs: [][]ghapi.Job{{job("Pre Release Helm", ghapi.StatusInProgress, "")}},
	}

	w := ciwait.NewWaiter(api, ciwait.Options{
		Owner: "o", Repo: "r", WorkflowFile: "go.yaml", Branch: "RD-1-a",
		WaitForJobs: gatingJobs, PollInterval: time.Hour,
	})

	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan ciwait.Event, 16)
	go func() {
		for range events {
			cancel()
		}
	}()

	_, err := w.Run(ctx, events)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled so SIGINT exits cleanly", err)
	}
}

func TestETAFromMedianOfPastRuns(t *testing.T) {
	api := &fakeAPI{durations: []time.Duration{
		5 * time.Minute, 9 * time.Minute, 7 * time.Minute,
	}}
	w := waiter(api)

	got, ok := w.ETA(context.Background(), 2*time.Minute)
	if !ok {
		t.Fatal("an ETA should be available with three past runs")
	}
	if got != 5*time.Minute {
		t.Errorf("ETA = %v, want 5m (median 7m minus 2m elapsed)", got)
	}
}

func TestETAOmittedWithoutHistory(t *testing.T) {
	if _, ok := waiter(&fakeAPI{}).ETA(context.Background(), time.Minute); ok {
		t.Error("with no history, omit the estimate rather than guessing")
	}
}

func contains(s, substr string) bool {
	return substr != "" && len(s) >= len(substr) &&
		(func() bool {
			for i := 0; i+len(substr) <= len(s); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
			return false
		})()
}

// TestLowRateLimitWidensTheInterval covers the requirement that a dwindling
// budget slows the loop down rather than spending what is left of the quota.
func TestLowRateLimitWidensTheInterval(t *testing.T) {
	running := ciwait.State{Jobs: []ciwait.JobView{{Status: ghapi.StatusInProgress}}}

	plenty := ciwait.NewWaiter(&fakeAPI{remaining: 4900}, ciwait.Options{PollInterval: 10 * time.Second})
	if got := ciwait.IntervalForTest(plenty, running); got != ciwait.MinPollInterval {
		t.Errorf("with quota to spare the interval is %v, want %v", got, ciwait.MinPollInterval)
	}

	scarce := ciwait.NewWaiter(&fakeAPI{remaining: 42}, ciwait.Options{PollInterval: 10 * time.Second})
	if got := ciwait.IntervalForTest(scarce, running); got != ciwait.MaxPollInterval {
		t.Errorf("with 42 requests left the interval is %v, want %v", got, ciwait.MaxPollInterval)
	}
}

func TestLowRateLimitIsReportedOnce(t *testing.T) {
	now := time.Now()
	api := &fakeAPI{
		runs:      [][]ghapi.Run{{run(100, now, 1)}},
		remaining: 12,
		resetAt:   now.Add(30 * time.Minute),
		jobs: [][]ghapi.Job{
			{job("Pre Release Helm", ghapi.StatusInProgress, "")},
			{job("Pre Release Helm", ghapi.StatusInProgress, "")},
			{job("Pre Release Helm", ghapi.StatusCompleted, ghapi.ConclusionSuccess)},
		},
	}

	_, events, err := drain(t, waiter(api))
	if err != nil {
		t.Fatal(err)
	}

	var notes int
	for _, e := range events {
		if contains(e.Note, "rate limit is low") {
			notes++
		}
	}
	if notes != 1 {
		t.Errorf("the low-budget notice appeared %d times, want exactly 1", notes)
	}
}
