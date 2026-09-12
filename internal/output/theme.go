// Package output owns every byte flow writes to a terminal. It is the only
// package outside internal/ciwait permitted to import a Charm library: commands
// ask the renderer for output, they never construct a style themselves. That is
// what keeps a later theme change, or a full TUI, a single-file change.
package output

import (
	"fmt"
	"image/color"
	"io"

	"charm.land/fang/v2"
	"charm.land/lipgloss/v2"
)

// Theme holds every style flow uses. Construct it once, from a LightDarkFunc,
// and pass it around: lipgloss v2 dropped AdaptiveColor in favour of resolving
// light/dark up front, so styles cannot be package-level values any more.
type Theme struct {
	Success lipgloss.Style
	Warn    lipgloss.Style
	Error   lipgloss.Style
	Muted   lipgloss.Style
	Heading lipgloss.Style
	Ticket  lipgloss.Style
	Path    lipgloss.Style
	Bold    lipgloss.Style

	// Raw colors, for the widgets that take a color rather than a style.
	AccentColor  color.Color
	SuccessColor color.Color
	WarnColor    color.Color
	ErrorColor   color.Color
	MutedColor   color.Color
}

// NewTheme builds the theme for a terminal with the given background.
func NewTheme(darkBackground bool) Theme {
	return NewThemeFrom(lipgloss.LightDark(darkBackground))
}

// NewThemeFrom builds the theme from an existing resolver. fang hands its
// color-scheme callback one it has already derived, and reusing it is what
// keeps fang's help pages on the same light/dark branch as everything else.
func NewThemeFrom(ld lipgloss.LightDarkFunc) Theme {
	var (
		green  = ld(lipgloss.Color("#0F7B3F"), lipgloss.Color("#5FD787"))
		yellow = ld(lipgloss.Color("#8A6300"), lipgloss.Color("#F5C542"))
		red    = ld(lipgloss.Color("#B3261E"), lipgloss.Color("#FF6B6B"))
		grey   = ld(lipgloss.Color("#6C6C6C"), lipgloss.Color("#9A9A9A"))
		purple = ld(lipgloss.Color("#5A3FBF"), lipgloss.Color("#B39DFF"))
		blue   = ld(lipgloss.Color("#1F5FAF"), lipgloss.Color("#7FB2F0"))
	)

	return Theme{
		Success:      lipgloss.NewStyle().Foreground(green),
		Warn:         lipgloss.NewStyle().Foreground(yellow),
		Error:        lipgloss.NewStyle().Foreground(red).Bold(true),
		Muted:        lipgloss.NewStyle().Foreground(grey),
		Heading:      lipgloss.NewStyle().Bold(true),
		Ticket:       lipgloss.NewStyle().Foreground(purple).Bold(true),
		Path:         lipgloss.NewStyle().Foreground(blue),
		Bold:         lipgloss.NewStyle().Bold(true),
		AccentColor:  purple,
		SuccessColor: green,
		WarnColor:    yellow,
		ErrorColor:   red,
		MutedColor:   grey,
	}
}

// Symbols used across the CLI and the CI progress display.
const (
	SymOK      = "✔"
	SymFail    = "✘"
	SymWarn    = "!"
	SymSkip    = "·"
	SymPending = "◦"
)

// FangColorScheme derives fang's help and error styling from flow's own
// palette, so help pages and command output never disagree about color.
//
// plain is decided by main before fang parses flags, because fang renders
// before any command's pre-run hook has looked at --no-color or --json.
func FangColorScheme(plain bool) func(lipgloss.LightDarkFunc) fang.ColorScheme {
	return func(ld lipgloss.LightDarkFunc) fang.ColorScheme {
		base := fang.DefaultColorScheme(ld)
		if plain {
			return plainScheme(base)
		}

		theme := NewThemeFrom(ld)
		base.Title = theme.AccentColor
		base.Command = theme.AccentColor
		base.Flag = theme.SuccessColor
		base.Argument = theme.SuccessColor
		base.DimmedArgument = theme.MutedColor
		base.Comment = theme.MutedColor
		return base
	}
}

// plainScheme collapses every color to the terminal's default, which is what
// --no-color, --json and NO_COLOR all mean.
func plainScheme(base fang.ColorScheme) fang.ColorScheme {
	none := lipgloss.NoColor{}
	base.Base = none
	base.Title = none
	base.Description = none
	base.Codeblock = none
	base.Program = none
	base.QuotedString = none
	base.Command = none
	base.DimmedArgument = none
	base.Comment = none
	base.Flag = none
	base.FlagDefault = none
	base.Argument = none
	base.Help = none
	base.Dash = none
	base.ErrorHeader = [2]color.Color{none, none}
	base.ErrorDetails = none
	return base
}

// FangErrorHandler builds fang's error handler. It lives here rather than in
// internal/cli because its signature names fang's own types, and internal/cli
// must not import a Charm library.
//
// aborted and alreadyReported are injected so this file needs no knowledge of
// flow's error taxonomy.
func FangErrorHandler(aborted, alreadyReported func(error) bool) fang.ErrorHandler {
	return func(w io.Writer, styles fang.Styles, err error) {
		switch {
		case aborted != nil && aborted(err):
			fmt.Fprintln(w, "  aborted")
		case alreadyReported != nil && alreadyReported(err):
			// The JSON envelope is already on stdout; printing again would
			// produce two reports of one failure.
		default:
			fang.DefaultErrorHandler(w, styles, err)
		}
	}
}
