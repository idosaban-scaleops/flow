package output

import (
	"errors"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
)

// ErrAborted is returned when the user declines a confirmation or presses
// ctrl-c at a prompt. Callers map it to exit code 6.
var ErrAborted = errors.New("aborted by user")

// ErrNotInteractive is returned when a prompt is required but stdin is not a
// terminal. The message always names the flag that would have avoided it.
type ErrNotInteractive struct {
	// Flag is the escape hatch, e.g. "--yes".
	Flag string
	// What describes the decision that could not be made.
	What string
}

func (e *ErrNotInteractive) Error() string {
	return fmt.Sprintf("cannot prompt for %s: stdin is not a terminal; pass %s", e.What, e.Flag)
}

// Confirm asks a yes/no question. assumeYes short-circuits it (--yes), and a
// non-interactive session fails with the flag that would have answered it.
func (r *Renderer) Confirm(title, description string, defaultYes, assumeYes bool) error {
	if assumeYes {
		return nil
	}
	if !r.interactive {
		return &ErrNotInteractive{Flag: "--yes", What: title}
	}

	value := defaultYes
	field := huh.NewConfirm().Title(title).Value(&value).Affirmative("Yes").Negative("No")
	if description != "" {
		field = field.Description(description)
	}

	if err := r.runForm(huh.NewForm(huh.NewGroup(field))); err != nil {
		return err
	}
	if !value {
		return ErrAborted
	}
	return nil
}

// SelectOption is one choice in a Select prompt.
type SelectOption struct {
	Label string
	Value string
}

// Select asks the user to choose one of options.
func (r *Renderer) Select(title string, options []SelectOption) (string, error) {
	if !r.interactive {
		return "", &ErrNotInteractive{Flag: "--yes", What: title}
	}
	if len(options) == 0 {
		return "", fmt.Errorf("no options to choose from for %q", title)
	}

	opts := make([]huh.Option[string], len(options))
	for i, o := range options {
		opts[i] = huh.NewOption(o.Label, o.Value)
	}

	var value string
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title(title).Options(opts...).Value(&value),
	))
	if err := r.runForm(form); err != nil {
		return "", err
	}
	return value, nil
}

// Input asks for a free-text value, pre-filled with def.
func (r *Renderer) Input(title, def string) (string, error) {
	if !r.interactive {
		return "", &ErrNotInteractive{Flag: "--yes", What: title}
	}

	value := def
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().Title(title).Value(&value).Placeholder(def),
	))
	if err := r.runForm(form); err != nil {
		return "", err
	}
	if value == "" {
		value = def
	}
	return value, nil
}

// runForm renders a form on stderr with the same color profile as every other
// byte flow emits, and normalizes ctrl-c into ErrAborted.
//
// The output is ProgramWriter (the terminal file) rather than r.err: handed the
// colorprofile writer, bubbletea sits at a 0x0 window and draws an empty frame
// forever, which is what made every prompt in flow look like a hang. The
// profile r.err would have applied is passed to bubbletea directly instead.
// WithProgramOptions replaces the option slice rather than appending to it, so
// it has to come before WithOutput and WithInput.
func (r *Renderer) runForm(form *huh.Form) error {
	err := form.
		WithProgramOptions(tea.WithColorProfile(r.Profile())).
		WithOutput(r.ProgramWriter()).
		WithInput(r.stdin).
		WithShowHelp(false).
		Run()
	switch {
	case err == nil:
		return nil
	case errors.Is(err, huh.ErrUserAborted):
		return ErrAborted
	default:
		return err
	}
}
