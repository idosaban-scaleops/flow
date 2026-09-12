package ciwait_test

import (
	"testing"
	"time"

	"github.com/idosaban-scaleops/flow/internal/ciwait"
	"github.com/idosaban-scaleops/flow/internal/ghapi"
)

// gatingJobs is the ScaleOps configuration: the two prefixes that actually gate
// an installable chart.
var gatingJobs = []string{"Pre Release Helm", "Pre Release Images"}

// nextJobID hands out distinct IDs; real jobs always have distinct ones, and
// the machine relies on that to keep same-named matrix legs separate.
var nextJobID int64

func job(name string, status ghapi.RunStatus, conclusion ghapi.Conclusion, steps ...ghapi.Step) ghapi.Job {
	nextJobID++
	return ghapi.Job{
		ID:         nextJobID,
		Name:       name,
		Status:     status,
		Conclusion: conclusion,
		Steps:      steps,
	}
}

func step(name string, status ghapi.RunStatus, conclusion ghapi.Conclusion) ghapi.Step {
	return ghapi.Step{Name: name, Status: status, Conclusion: conclusion}
}

func TestMatchIgnoresNonGatingJobs(t *testing.T) {
	// Waiting on Release Helm would hang forever: it only runs when
	// is_release == 'true', which a dev branch never is.
	jobs := []ghapi.Job{
		job("Setup", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
		job("Helm Build", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
		job("Pre Release Helm / Release", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
		job("Pre Release Images / Build And Push", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
		job("Wiz Scan", ghapi.StatusInProgress, ""),
		job("E2E", ghapi.StatusQueued, ""),
		job("Release Helm", ghapi.StatusQueued, ""),
		job("Release Images", ghapi.StatusQueued, ""),
	}

	got := ciwait.NewMachine(gatingJobs).Update(jobs)
	if got.Matched != 2 {
		t.Fatalf("matched %d jobs, want 2 (the two gating ones)", got.Matched)
	}
	if got.Phase != ciwait.PhaseSucceeded {
		t.Errorf("phase = %v, want succeeded — non-gating jobs must not hold the wait open", got.Phase)
	}
	if got.Percent != 1 {
		t.Errorf("percent = %v, want 1", got.Percent)
	}
}

func TestNestedReusableWorkflowNamesMatch(t *testing.T) {
	// Reusable workflows report as "{caller job} / {inner job}"; the YAML job
	// key is never exposed by the API.
	jobs := []ghapi.Job{
		job("Pre Release Helm / Release", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
		job("Pre Release Images / Build And Push", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
	}
	got := ciwait.NewMachine(gatingJobs).Update(jobs)
	if got.Matched != 2 || got.Phase != ciwait.PhaseSucceeded {
		t.Errorf("nested names did not match: %+v", got)
	}
}

func TestCollidingJobNamesAllGate(t *testing.T) {
	// Four jobs share the display name "Pre Release Images"; all of them must
	// finish, so one straggler keeps the wait open.
	jobs := []ghapi.Job{
		job("Pre Release Helm", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
		job("Pre Release Images / Build And Push", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
		job("Pre Release Images / Build And Push (2)", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
		job("Pre Release Images / Build And Push (3)", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
		job("Pre Release Images / Build And Push (4)", ghapi.StatusInProgress, ""),
	}

	got := ciwait.NewMachine(gatingJobs).Update(jobs)
	if got.Matched != 5 {
		t.Fatalf("matched %d, want all 5", got.Matched)
	}
	if got.Phase != ciwait.PhaseWaiting {
		t.Errorf("phase = %v, want waiting while one matrix leg is still running", got.Phase)
	}
}

func TestSkippedCountsAsSatisfied(t *testing.T) {
	// On a default dev build SKIP_GPU=true and build_all_images=false, so the
	// GPU and FIPS legs are skipped. Waiting for them would never finish.
	jobs := []ghapi.Job{
		job("Pre Release Helm", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
		job("Pre Release Images / Build And Push", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
		job("Pre Release Images / Build And Push GPU", ghapi.StatusCompleted, ghapi.ConclusionSkipped),
	}

	got := ciwait.NewMachine(gatingJobs).Update(jobs)
	if got.Phase != ciwait.PhaseSucceeded {
		t.Errorf("phase = %v, want succeeded — a skipped job is satisfied", got.Phase)
	}
	if got.Percent != 1 {
		t.Errorf("percent = %v, want 1", got.Percent)
	}

	var sawSkipped bool
	for _, view := range got.Jobs {
		if view.Description == "skipped" {
			sawSkipped = true
		}
	}
	if !sawSkipped {
		t.Error("the skipped job should be shown as skipped, not as done")
	}
}

func TestEveryRowCarriesADescription(t *testing.T) {
	started := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	finished := started.Add(2*time.Minute + 3*time.Second)

	done := job("Pre Release Helm", ghapi.StatusCompleted, ghapi.ConclusionSuccess)
	done.StartedAt, done.CompletedAt = &started, &finished

	jobs := []ghapi.Job{
		done,
		job("Pre Release Images / running", ghapi.StatusInProgress, "",
			step("Set up job", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
			step("Building and pushing scaleops image", ghapi.StatusInProgress, ""),
			step("Cleanup", ghapi.StatusQueued, "")),
		job("Pre Release Images / between-steps", ghapi.StatusInProgress, "",
			step("Checkout", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
			step("Login", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
			step("Push", ghapi.StatusQueued, "")),
		job("Pre Release Images / queued", ghapi.StatusQueued, ""),
		job("Pre Release Images / skipped", ghapi.StatusCompleted, ghapi.ConclusionSkipped),
	}

	got := ciwait.NewMachine(gatingJobs).Update(jobs)
	want := []string{
		"done in 2m03s",
		"Building and pushing scaleops image",
		"after Login",
		"queued",
		"skipped",
	}
	if len(got.Jobs) != len(want) {
		t.Fatalf("want %d rows, got %d", len(want), len(got.Jobs))
	}
	for i, w := range want {
		if got.Jobs[i].Description != w {
			t.Errorf("row %d description = %q, want %q", i, got.Jobs[i].Description, w)
		}
	}
}

func TestFailureStopsTheWait(t *testing.T) {
	failing := job("Pre Release Images / Build And Push", ghapi.StatusCompleted, ghapi.ConclusionFailure,
		step("Set up job", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
		step("Push image", ghapi.StatusCompleted, ghapi.ConclusionFailure))
	failing.HTMLURL = "https://github.com/o/r/actions/runs/1/job/2"

	jobs := []ghapi.Job{
		job("Pre Release Helm", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
		failing,
	}

	got := ciwait.NewMachine(gatingJobs).Update(jobs)
	if got.Phase != ciwait.PhaseFailed {
		t.Fatalf("phase = %v, want failed", got.Phase)
	}
	if got.FailedStep != "Push image" {
		t.Errorf("FailedStep = %q", got.FailedStep)
	}
	if got.FailedURL == "" {
		t.Error("the failing job's URL must be surfaced so the user can open it")
	}
}

func TestIgnoreFailuresKeepsWaiting(t *testing.T) {
	m := ciwait.NewMachine(gatingJobs)
	m.IgnoreFailures = true

	got := m.Update([]ghapi.Job{
		job("Pre Release Helm", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
		job("Pre Release Images / gpu", ghapi.StatusCompleted, ghapi.ConclusionFailure),
	})
	if got.Phase == ciwait.PhaseFailed {
		t.Error("--ignore-failures must not stop the wait on a non-essential leg")
	}
}

func TestCancelledRunIsReported(t *testing.T) {
	got := ciwait.NewMachine(gatingJobs).Update([]ghapi.Job{
		job("Pre Release Helm", ghapi.StatusCompleted, ghapi.ConclusionCancelled),
		job("Pre Release Images", ghapi.StatusCompleted, ghapi.ConclusionCancelled),
	})
	if got.Phase != ciwait.PhaseCancelled {
		t.Errorf("phase = %v, want cancelled so the caller can retarget", got.Phase)
	}
}

func TestNoGatingJobsYetIsNotSuccess(t *testing.T) {
	// Early in a run the gating jobs do not exist yet. Reporting success for an
	// empty set would run helm against a chart that was never published.
	got := ciwait.NewMachine(gatingJobs).Update([]ghapi.Job{
		job("Setup", ghapi.StatusInProgress, ""),
	})
	if got.Phase != ciwait.PhaseWaiting {
		t.Errorf("phase = %v, want waiting", got.Phase)
	}
	if got.Percent != 0 {
		t.Errorf("percent = %v, want 0", got.Percent)
	}
}

func TestEmptyTargetsFallBackToEveryJob(t *testing.T) {
	m := ciwait.NewMachine(nil)
	got := m.Update([]ghapi.Job{
		job("A", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
		job("B", ghapi.StatusInProgress, ""),
	})
	if got.Matched != 2 {
		t.Errorf("matched %d, want every job when no gating set is configured", got.Matched)
	}
	if got.Phase != ciwait.PhaseWaiting {
		t.Errorf("phase = %v", got.Phase)
	}
}

// TestPercentIsMonotonic walks realistic poll sequences and asserts the
// percentage never decreases — a bar that goes backwards is the single most
// distracting thing a progress display can do.
func TestPercentIsMonotonic(t *testing.T) {
	sequences := map[string][][]ghapi.Job{
		"all queued then progress": {
			{
				job("Pre Release Helm", ghapi.StatusQueued, ""),
				job("Pre Release Images", ghapi.StatusQueued, ""),
			},
			{
				job("Pre Release Helm", ghapi.StatusInProgress, "",
					step("a", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
					step("b", ghapi.StatusInProgress, "")),
				job("Pre Release Images", ghapi.StatusQueued, ""),
			},
			{
				job("Pre Release Helm", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
				job("Pre Release Images", ghapi.StatusInProgress, "",
					step("a", ghapi.StatusInProgress, "")),
			},
			{
				job("Pre Release Helm", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
				job("Pre Release Images", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
			},
		},
		"a job list that grows mid-run": {
			{job("Pre Release Helm", ghapi.StatusCompleted, ghapi.ConclusionSuccess)},
			{
				job("Pre Release Helm", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
				job("Pre Release Images / 1", ghapi.StatusQueued, ""),
				job("Pre Release Images / 2", ghapi.StatusQueued, ""),
				job("Pre Release Images / 3", ghapi.StatusQueued, ""),
			},
			{
				job("Pre Release Helm", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
				job("Pre Release Images / 1", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
				job("Pre Release Images / 2", ghapi.StatusCompleted, ghapi.ConclusionSkipped),
				job("Pre Release Images / 3", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
			},
		},
		"steps regress between polls": {
			{
				job("Pre Release Helm", ghapi.StatusInProgress, "",
					step("a", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
					step("b", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
					step("c", ghapi.StatusInProgress, "")),
			},
			// A re-run resets the step list; the bar must not rewind.
			{
				job("Pre Release Helm", ghapi.StatusInProgress, "",
					step("a", ghapi.StatusInProgress, ""),
					step("b", ghapi.StatusQueued, ""),
					step("c", ghapi.StatusQueued, "")),
			},
			{
				job("Pre Release Helm", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
			},
		},
		"gating jobs appear only after setup": {
			{job("Setup", ghapi.StatusInProgress, "")},
			{
				job("Setup", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
				job("Pre Release Helm", ghapi.StatusQueued, ""),
			},
			{
				job("Setup", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
				job("Pre Release Helm", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
			},
		},
	}

	for name, seq := range sequences {
		t.Run(name, func(t *testing.T) {
			m := ciwait.NewMachine(gatingJobs)
			var prev float64
			for i, jobs := range seq {
				got := m.Update(jobs)
				if got.Percent < prev {
					t.Errorf("poll %d: percent fell from %v to %v", i, prev, got.Percent)
				}
				if got.Percent < 0 || got.Percent > 1 {
					t.Errorf("poll %d: percent %v out of range", i, got.Percent)
				}
				prev = got.Percent
			}
		})
	}
}

func TestRetargetIsTheOnlyWayBackToZero(t *testing.T) {
	m := ciwait.NewMachine(gatingJobs)

	first := m.Update([]ghapi.Job{
		job("Pre Release Helm", ghapi.StatusCompleted, ghapi.ConclusionSuccess),
		job("Pre Release Images", ghapi.StatusInProgress, ""),
	})
	if first.Percent <= 0 {
		t.Fatalf("expected progress, got %v", first.Percent)
	}

	// Without a retarget, an empty listing must not rewind the bar.
	held := m.Update(nil)
	if held.Percent < first.Percent {
		t.Errorf("percent fell from %v to %v without a retarget", first.Percent, held.Percent)
	}

	m.Retarget()
	after := m.Update([]ghapi.Job{job("Pre Release Helm", ghapi.StatusQueued, "")})
	if after.Percent != 0 {
		t.Errorf("percent = %v after retarget, want 0", after.Percent)
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{0, ""},
		{-time.Second, ""},
		{41 * time.Second, "41s"},
		{2*time.Minute + 3*time.Second, "2m03s"},
		{10 * time.Minute, "10m00s"},
		{time.Hour + 4*time.Minute, "1h04m"},
	}
	for _, tt := range tests {
		if got := ciwait.FormatDuration(tt.in); got != tt.want {
			t.Errorf("FormatDuration(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
