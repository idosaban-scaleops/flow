package ciwait

import (
	"fmt"
	"io"
	"time"

	"github.com/idosaban-scaleops/flow/internal/ghapi"
	"github.com/idosaban-scaleops/flow/internal/output"
)

// Display shows a wait in progress. Three implementations exist: a live
// bubbletea view for terminals, a transition-only line log for pipes and
// --no-color, and a silent one for --json.
type Display interface {
	Update(State)
	Retarget(Retarget)
	Note(string)
	// Close tears the display down, leaving the terminal usable.
	Close()
}

// NewDisplay picks the right implementation for the current output mode.
func NewDisplay(r *output.Renderer, target Target, chart string) Display {
	switch {
	case r.JSONMode():
		return silentDisplay{}
	case r.Interactive() && r.ColorEnabled():
		return newLiveDisplay(r, target, chart)
	default:
		return newPlainDisplay(r.ErrWriter(), target)
	}
}

// silentDisplay emits nothing. In --json mode the wait is reported once, in the
// final payload's ci_wait object.
type silentDisplay struct{}

func (silentDisplay) Update(State)      {}
func (silentDisplay) Retarget(Retarget) {}
func (silentDisplay) Note(string)       {}
func (silentDisplay) Close()            {}

// plainDisplay appends one line per state transition and never repeats a
// heartbeat, so a CI log or a piped run stays readable.
type plainDisplay struct {
	w       io.Writer
	started time.Time
	seen    map[string]string
}

func newPlainDisplay(w io.Writer, target Target) *plainDisplay {
	d := &plainDisplay{w: w, started: time.Now(), seen: map[string]string{}}
	fmt.Fprintf(w, "watching %s run %d (%s)\n", target.Name, target.RunID, target.URL)
	return d
}

func (d *plainDisplay) Update(state State) {
	for _, job := range state.Jobs {
		line := transitionLine(job)
		if d.seen[job.Name] == line {
			continue
		}
		d.seen[job.Name] = line
		fmt.Fprintf(d.w, "[%s] %s: %s\n", clock(time.Since(d.started)), job.Name, line)
	}
}

// transitionLine collapses a row to the text whose change marks a transition.
func transitionLine(job JobView) string {
	if job.Status == ghapi.StatusCompleted {
		conclusion := string(job.Conclusion)
		if conclusion == "" {
			conclusion = "done"
		}
		return fmt.Sprintf("completed (%s)", conclusion)
	}
	return job.Description
}

func (d *plainDisplay) Retarget(r Retarget) {
	fmt.Fprintf(d.w, "[%s] run %d %s — now watching %d\n",
		clock(time.Since(d.started)), r.From.RunID, r.Reason, r.To.RunID)
	d.seen = map[string]string{}
}

func (d *plainDisplay) Note(note string) {
	fmt.Fprintf(d.w, "[%s] %s\n", clock(time.Since(d.started)), note)
}

func (d *plainDisplay) Close() {}

// clock renders elapsed time as mm:ss, the form the transition log uses.
func clock(d time.Duration) string {
	d = d.Round(time.Second)
	return fmt.Sprintf("%02d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}
