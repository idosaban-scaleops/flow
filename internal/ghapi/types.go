// Package ghapi wraps go-github behind a narrow domain interface. No other
// package may import go-github: a major-version bump changes the import path,
// and confining it here keeps that from rippling through every file that names
// a pull request.
package ghapi

import (
	"encoding/json"
	"fmt"
	"time"
)

// PRState models pull-request existence as three values, because "there is no
// PR" and "I could not find out" must never collapse into the same branch of a
// conditional — the wording a user sees differs, and so does the right action.
type PRState int

// Pull request states.
const (
	// PRUnknown means the state could not be determined.
	PRUnknown PRState = iota
	// PRNone means it was determined that no pull request exists.
	PRNone
	PROpen
	PRMerged
	PRClosed
)

// String returns the lowercase name used in output and JSON payloads.
func (s PRState) String() string {
	switch s {
	case PRNone:
		return "none"
	case PROpen:
		return "open"
	case PRMerged:
		return "merged"
	case PRClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// MarshalJSON emits the name rather than the ordinal, so golden payloads stay
// readable and stable.
func (s PRState) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// UnmarshalJSON parses the name form.
func (s *PRState) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return err
	}
	switch name {
	case "none":
		*s = PRNone
	case "open":
		*s = PROpen
	case "merged":
		*s = PRMerged
	case "closed":
		*s = PRClosed
	case "unknown":
		*s = PRUnknown
	default:
		return fmt.Errorf("unknown pull request state %q", name)
	}
	return nil
}

// PRInfo is everything flow needs to know about a branch's pull request.
type PRInfo struct {
	State    PRState    `json:"state"`
	Number   int        `json:"number,omitempty"` // 0 when None or Unknown
	URL      string     `json:"url,omitempty"`
	MergedAt *time.Time `json:"merged_at,omitempty"`
	// Reason explains why State is Unknown, for display.
	Reason string `json:"reason,omitempty"`
}

// Known reports whether the state was actually determined.
func (p PRInfo) Known() bool { return p.State != PRUnknown }

// Exists reports a pull request that was found.
func (p PRInfo) Exists() bool {
	return p.State == PROpen || p.State == PRMerged || p.State == PRClosed
}

// Unknown builds an Unknown PRInfo with a displayable reason.
func Unknown(reason string) PRInfo { return PRInfo{State: PRUnknown, Reason: reason} }

// RunStatus is a workflow run or job lifecycle state.
type RunStatus string

// Workflow run and job statuses as GitHub reports them.
const (
	StatusQueued     RunStatus = "queued"
	StatusInProgress RunStatus = "in_progress"
	StatusCompleted  RunStatus = "completed"
	StatusWaiting    RunStatus = "waiting"
	StatusPending    RunStatus = "pending"
)

// Conclusion is how a completed run or job finished.
type Conclusion string

// Workflow run and job conclusions.
const (
	ConclusionSuccess   Conclusion = "success"
	ConclusionFailure   Conclusion = "failure"
	ConclusionCancelled Conclusion = "cancelled"
	ConclusionSkipped   Conclusion = "skipped"
	ConclusionTimedOut  Conclusion = "timed_out"
	ConclusionNeutral   Conclusion = "neutral"
	ConclusionNone      Conclusion = ""
)

// Run is one workflow run, flattened to the fields flow uses.
type Run struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name,omitempty"`
	Status     RunStatus  `json:"status"`
	Conclusion Conclusion `json:"conclusion,omitempty"`
	Branch     string     `json:"branch,omitempty"`
	HTMLURL    string     `json:"html_url,omitempty"`
	Attempt    int        `json:"run_attempt,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at,omitempty"`
	HeadSHA    string     `json:"head_sha,omitempty"`
}

// Succeeded reports a run that completed successfully.
func (r Run) Succeeded() bool {
	return r.Status == StatusCompleted && r.Conclusion == ConclusionSuccess
}

// Step is one step within a job. The step list is what supplies the
// human-readable description in the progress display.
type Step struct {
	Name       string     `json:"name"`
	Number     int        `json:"number"`
	Status     RunStatus  `json:"status"`
	Conclusion Conclusion `json:"conclusion,omitempty"`
}

// Job is one job within a run.
type Job struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Status      RunStatus  `json:"status"`
	Conclusion  Conclusion `json:"conclusion,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	HTMLURL     string     `json:"html_url,omitempty"`
	Steps       []Step     `json:"steps,omitempty"`
}

// Duration returns how long the job ran, or zero if it has not finished.
func (j Job) Duration() time.Duration {
	if j.StartedAt == nil || j.CompletedAt == nil {
		return 0
	}
	return j.CompletedAt.Sub(*j.StartedAt)
}

// Done reports a job that will not change state again. A skipped job counts as
// satisfied: on a default dev build, the GPU and FIPS legs are always skipped,
// and waiting for them would hang forever.
func (j Job) Done() bool { return j.Status == StatusCompleted }

// Failed reports a job that finished unsuccessfully. Skipped and cancelled are
// deliberately not failures here; cancellation is handled by retargeting.
func (j Job) Failed() bool {
	if j.Status != StatusCompleted {
		return false
	}
	switch j.Conclusion {
	case ConclusionFailure, ConclusionTimedOut:
		return true
	default:
		return false
	}
}

// Skipped reports a job GitHub chose not to run.
func (j Job) Skipped() bool { return j.Conclusion == ConclusionSkipped }
