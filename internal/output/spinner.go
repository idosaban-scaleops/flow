package output

import (
	"context"

	"charm.land/huh/v2/spinner"
)

// Spin runs fn while showing a one-line indeterminate spinner on stderr. In
// JSON or non-interactive mode it just runs fn, so scripted output stays clean.
//
// fn owns a context of its own, and Spin always waits for fn to return before
// it does. Both matter because of what ctrl+c does here: the spinner puts the
// terminal in raw mode, so the key arrives as a key press rather than as SIGINT
// to the process group — it never reaches a subprocess fn started — and huh
// answers it by tearing the program down while the action is still running.
// Cancelling fn's context is therefore the only thing that makes the interrupt
// mean anything, and waiting for fn to notice is what lets the caller read
// whatever fn wrote without racing it.
func (r *Renderer) Spin(ctx context.Context, title string, fn func(context.Context) error) error {
	if !r.interactive {
		return fn(ctx)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// fn runs on a goroutine this function owns rather than on huh's, so its
	// lifetime does not end with the spinner's.
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()

	spinErr := spinner.New().
		Title(" " + title).
		WithOutput(r.ProgramWriter()).
		Context(ctx).
		ActionWithErr(func(actionCtx context.Context) error {
			select {
			case err := <-done:
				// Hand the result straight back for the wait below; the
				// channel is buffered, so this cannot block.
				done <- err
			case <-actionCtx.Done():
			}
			return nil
		}).
		Run()

	if spinErr != nil {
		cancel()
	}
	// fn's own error wins: "helm upgrade failed like this" says more than
	// "the spinner was interrupted", and an interrupt reaches the caller as
	// fn's context.Canceled anyway.
	if err := <-done; err != nil {
		return err
	}
	return spinErr
}
