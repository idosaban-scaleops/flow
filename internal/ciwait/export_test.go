package ciwait

import (
	"context"
	"io"
	"time"

	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"

	"github.com/idosaban-scaleops/flow/internal/output"
)

// SetSleepForTest replaces the wait loop's sleep so tests run without wall time.
func SetSleepForTest(w *Waiter, fn func(context.Context, time.Duration) error) {
	w.sleep = fn
}

// IntervalForTest exposes the adaptive poll interval for direct assertions.
func IntervalForTest(w *Waiter, s State) time.Duration { return w.interval(s) }

// RenderFrameForTest renders one progress frame at a fixed width, so the layout
// can be compared against a golden file without a terminal.
func RenderFrameForTest(theme output.Theme, target Target, chart string, state State, width int) string {
	m := waitModel{
		theme:   theme,
		target:  target,
		chart:   chart,
		width:   width,
		started: time.Now().Add(-4*time.Minute - 12*time.Second),
		spin:    spinner.New(spinner.WithSpinner(flowSpinner())),
		bar: progress.New(
			progress.WithoutPercentage(),
			progress.WithWidth(36),
			progress.WithFillCharacters('█', '░'),
		),
		state: state,
	}
	return m.render()
}

// NewPlainDisplayForTest builds the non-TTY display against an arbitrary writer.
func NewPlainDisplayForTest(w io.Writer, target Target) Display {
	return newPlainDisplay(w, target)
}
