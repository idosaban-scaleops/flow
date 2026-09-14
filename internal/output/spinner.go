package output

import (
	"context"

	"charm.land/huh/v2/spinner"
)

// Spin runs fn while showing a one-line indeterminate spinner on stderr. In
// JSON or non-interactive mode it just runs fn, so scripted output stays clean.
func (r *Renderer) Spin(ctx context.Context, title string, fn func() error) error {
	if !r.interactive {
		return fn()
	}
	var runErr error
	err := spinner.New().
		Title(" " + title).
		WithOutput(r.ProgramWriter()).
		Context(ctx).
		ActionWithErr(func(context.Context) error {
			runErr = fn()
			return nil
		}).
		Run()
	if err != nil {
		return err
	}
	return runErr
}
