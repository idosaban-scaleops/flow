// Package ciwait waits for the CI jobs that make a chart installable, and shows
// live progress while it does.
//
// The package is split deliberately: Machine is a pure state machine over
// job payloads, with no network and no terminal, and the renderer is separate.
// That is what makes the percentage-monotonicity and job-matching rules
// testable as table-driven cases.
package ciwait

import (
	"fmt"
	"strings"
	"time"

	"github.com/idosaban-scaleops/flow/internal/ghapi"
)

// Phase is the overall state of a wait.
type Phase int

// Wait phases.
const (
	// PhaseWaiting means gating jobs are still running or queued.
	PhaseWaiting Phase = iota
	// PhaseSucceeded means every gating job finished satisfactorily.
	PhaseSucceeded
	// PhaseFailed means a gating job failed.
	PhaseFailed
	// PhaseCancelled means the watched run was cancelled, usually by a newer push.
	PhaseCancelled
)

// String returns the phase name used in output.
func (p Phase) String() string {
	switch p {
	case PhaseSucceeded:
		return "succeeded"
	case PhaseFailed:
		return "failed"
	case PhaseCancelled:
		return "cancelled"
	case PhaseWaiting:
		return "waiting"
	default:
		return "waiting"
	}
}

// JobView is one row of the progress display.
type JobView struct {
	Name string `json:"name"`
	// Description is never just a state: it names the step currently running,
	// or how the job finished.
	Description string           `json:"description"`
	Status      ghapi.RunStatus  `json:"status"`
	Conclusion  ghapi.Conclusion `json:"conclusion,omitempty"`
	Duration    time.Duration    `json:"-"`
	DurationStr string           `json:"duration,omitempty"`
	HTMLURL     string           `json:"html_url,omitempty"`
	// Gating reports whether this job is one the wait depends on.
	Gating bool `json:"gating"`
}

// State is the machine's output after one poll.
type State struct {
	Phase Phase `json:"phase"`
	// Jobs holds the gating jobs, in the configured order.
	Jobs []JobView `json:"jobs"`
	// Percent is monotonic across a run: it never decreases except on a
	// deliberate retarget, which resets it to zero.
	Percent float64 `json:"percent"`

	// FailedJob, FailedStep and FailedURL are set when Phase is PhaseFailed.
	FailedJob  string `json:"failed_job,omitempty"`
	FailedStep string `json:"failed_step,omitempty"`
	FailedURL  string `json:"failed_url,omitempty"`

	// Matched reports how many jobs matched the gating patterns. Zero means CI
	// has not created them yet, which is a normal early state.
	Matched int `json:"matched"`
}

// Machine turns successive job listings into display state. It holds exactly
// one piece of history — the highest percentage seen — because the percentage
// must never go backwards while the user watches it.
type Machine struct {
	// Targets are job-name prefixes that gate the wait. Empty means every job
	// in the run gates, which is the fallback when a repo configures none.
	Targets []string
	// IgnoreFailures keeps waiting when a gating job fails, for the case where
	// a non-essential matrix leg failed but the chart published anyway.
	IgnoreFailures bool

	maxPercent float64
}

// NewMachine returns a Machine for a set of gating job-name prefixes.
func NewMachine(targets []string) *Machine {
	return &Machine{Targets: append([]string(nil), targets...)}
}

// Retarget resets the progress history. This is the one sanctioned way for the
// percentage to decrease: the run being watched was replaced.
func (m *Machine) Retarget() { m.maxPercent = 0 }

// Update folds a job listing into display state.
func (m *Machine) Update(jobs []ghapi.Job) State {
	matched := m.match(jobs)

	state := State{Matched: len(matched)}
	if len(matched) == 0 {
		// CI has not created the gating jobs yet. Report no progress rather
		// than claiming completion of an empty set.
		state.Percent = m.advanceTo(0)
		return state
	}

	share := 1.0 / float64(len(matched))
	var progress float64
	allDone := true
	anyCancelled := false

	for _, job := range matched {
		view := describe(job)
		view.Gating = true
		state.Jobs = append(state.Jobs, view)

		switch {
		case job.Done():
			progress += share
		case job.Status == ghapi.StatusInProgress:
			progress += share * stepFraction(job)
			allDone = false
		default:
			allDone = false
		}

		if job.Failed() && !m.IgnoreFailures && state.Phase != PhaseFailed {
			state.Phase = PhaseFailed
			state.FailedJob = job.Name
			state.FailedStep = failedStepName(job)
			state.FailedURL = job.HTMLURL
		}
		if job.Conclusion == ghapi.ConclusionCancelled {
			anyCancelled = true
		}
	}

	state.Percent = m.advanceTo(progress)

	// Cancellation is decided only once the whole gating set has stopped.
	// A single cancelled matrix leg is not a cancelled run: reacting to it
	// immediately triggered a retarget, which ended the wait with ErrCancelled
	// whenever no newer run existed — while the run's other legs were still
	// happily building. When cancel-in-progress really does cancel the run,
	// every unfinished job is cancelled with it, so this still fires.
	if state.Phase == PhaseWaiting && allDone {
		if anyCancelled {
			state.Phase = PhaseCancelled
		} else {
			state.Phase = PhaseSucceeded
			// A finished set is 100% regardless of step accounting.
			state.Percent = m.advanceTo(1)
		}
	}
	return state
}

// advanceTo enforces monotonicity and keeps the value in [0, 1]. It is named
// for the fact that it advances state: it records the new high-water mark in
// m.maxPercent, so it is not the pure helper a name like "clamp" suggests.
func (m *Machine) advanceTo(p float64) float64 {
	switch {
	case p < 0:
		p = 0
	case p > 1:
		p = 1
	}
	if p < m.maxPercent {
		return m.maxPercent
	}
	m.maxPercent = p
	return p
}

// match selects the gating jobs.
//
// Three details make this less obvious than it looks. Job display names
// collide — four jobs share the name "Pre Release Images" — so every match is
// kept, as a group that must collectively finish. Reusable workflows nest
// names as "{caller} / {inner}", so matching is on prefix. And the YAML job key
// is never used, because the Actions API does not expose it.
func (m *Machine) match(jobs []ghapi.Job) []ghapi.Job {
	if len(m.Targets) == 0 {
		return jobs
	}

	var out []ghapi.Job
	seen := make(map[int64]bool, len(jobs))
	for _, target := range m.Targets {
		for _, job := range jobs {
			if seen[job.ID] || !matchesTarget(job.Name, target) {
				continue
			}
			seen[job.ID] = true
			out = append(out, job)
		}
	}
	return out
}

// matchesTarget reports whether a job name matches a gating pattern, by exact
// match or by the "{target} / {inner job}" nesting a reusable workflow produces.
//
// The separator is required. A bare HasPrefix widened the gate to every job
// whose name merely starts with the target — target "Test" also matched "Test
// Coverage Report" and "Tests-e2e" — so the wait blocked on jobs it was never
// meant to watch. This is the same boundary rule registry.underPath applies to
// path prefixes.
func matchesTarget(name, target string) bool {
	if name == target {
		return true
	}
	return strings.HasPrefix(name, target+" / ")
}

// stepFraction is how far through its own step list a running job is. Total
// step counts only exist once a job starts, so a started-but-stepless job
// counts as zero rather than as complete.
func stepFraction(job ghapi.Job) float64 {
	if len(job.Steps) == 0 {
		return 0
	}
	var done int
	for _, s := range job.Steps {
		if s.Status == ghapi.StatusCompleted {
			done++
		}
	}
	return float64(done) / float64(len(job.Steps))
}

// describe builds a row. Every row carries a description, not just a state:
// for a running job that is the step currently executing, which is the only
// thing that tells a watching user whether anything is actually happening.
func describe(job ghapi.Job) JobView {
	view := JobView{
		Name:       job.Name,
		Status:     job.Status,
		Conclusion: job.Conclusion,
		HTMLURL:    job.HTMLURL,
		Duration:   job.Duration(),
	}
	if view.Duration > 0 {
		view.DurationStr = FormatDuration(view.Duration)
	}

	switch {
	case job.Skipped():
		view.Description = "skipped"
	case job.Failed():
		if step := failedStepName(job); step != "" {
			view.Description = fmt.Sprintf("failed at step %q", step)
		} else {
			view.Description = "failed"
		}
	case job.Conclusion == ghapi.ConclusionCancelled:
		view.Description = "cancelled"
	case job.Done():
		if view.Duration > 0 {
			view.Description = "done in " + view.DurationStr
		} else {
			view.Description = "done"
		}
	case job.Status == ghapi.StatusInProgress:
		view.Description = runningStepName(job)
	default:
		view.Description = "queued"
	}
	return view
}

// runningStepName names the step a job is on, falling back to "after {step}"
// when GitHub reports no step as in progress — which happens between steps.
func runningStepName(job ghapi.Job) string {
	for i := len(job.Steps) - 1; i >= 0; i-- {
		if job.Steps[i].Status == ghapi.StatusInProgress {
			return job.Steps[i].Name
		}
	}
	for i := len(job.Steps) - 1; i >= 0; i-- {
		if job.Steps[i].Status == ghapi.StatusCompleted {
			return "after " + job.Steps[i].Name
		}
	}
	return "running"
}

func failedStepName(job ghapi.Job) string {
	for _, s := range job.Steps {
		switch s.Conclusion {
		case ghapi.ConclusionFailure, ghapi.ConclusionTimedOut:
			return s.Name
		case ghapi.ConclusionSuccess, ghapi.ConclusionSkipped,
			ghapi.ConclusionCancelled, ghapi.ConclusionNeutral, ghapi.ConclusionNone:
			// Not what broke the job.
		default:
		}
	}
	return ""
}

// FormatDuration renders a duration the way the progress display shows it:
// "41s", "2m03s", "1h04m".
func FormatDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
