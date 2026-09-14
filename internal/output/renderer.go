package output

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"charm.land/log/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/term"
)

// Options describe how a Renderer should behave. They come straight from the
// global flags on the root command.
type Options struct {
	JSON    bool
	NoColor bool
	Verbose int

	// Writers default to the process's own when nil. Tests inject buffers.
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
}

// Renderer is the single sink for user-facing output. The primary result goes
// to stdout; progress, warnings and logs go to stderr, so that
// `cd $(flow path)` keeps working.
type Renderer struct {
	out *colorprofile.Writer
	err *colorprofile.Writer

	stdout io.Writer
	stderr io.Writer
	stdin  io.Reader

	// ttyErr is stderr as a terminal file, or nil when stderr is not a
	// terminal. Bubbletea programs must be handed this rather than the
	// colorprofile writer: see ProgramWriter.
	ttyErr *os.File

	Theme Theme
	Log   *log.Logger

	json        bool
	interactive bool
	width       int
}

// New builds a Renderer, deciding the color profile exactly once. --no-color,
// NO_COLOR, --json and a non-TTY writer each force plain ASCII; because the
// decision is made here and shared with huh, charmbracelet/log and fang, all
// four can never disagree about whether color is on.
func New(opts Options) *Renderer {
	stdout := writerOr(opts.Stdout, os.Stdout)
	stderr := writerOr(opts.Stderr, os.Stderr)
	var stdin io.Reader = os.Stdin
	if opts.Stdin != nil {
		stdin = opts.Stdin
	}

	env := os.Environ()
	outW := colorprofile.NewWriter(stdout, env)
	errW := colorprofile.NewWriter(stderr, env)

	plain := opts.NoColor || opts.JSON || os.Getenv("NO_COLOR") != ""
	if plain {
		outW.Profile = colorprofile.Ascii
		errW.Profile = colorprofile.Ascii
	}

	dark := true
	width := 100
	var ttyErr *os.File
	if f, ok := stderr.(*os.File); ok && term.IsTerminal(f.Fd()) {
		ttyErr = f
		if w, _, err := term.GetSize(f.Fd()); err == nil && w > 0 {
			width = w
		}
		if in, ok := stdin.(*os.File); ok {
			dark = lipgloss.HasDarkBackground(in, f)
		}
	}

	r := &Renderer{
		out: outW, err: errW,
		stdout: stdout, stderr: stderr, stdin: stdin,
		ttyErr:      ttyErr,
		Theme:       NewTheme(dark),
		json:        opts.JSON,
		interactive: isTTY(stdin) && isTTY(stderr) && !opts.JSON,
		width:       width,
	}

	logger := log.New(errW)
	logger.SetLevel(levelFor(opts.Verbose))
	logger.SetReportTimestamp(false)
	r.Log = logger
	return r
}

func levelFor(verbose int) log.Level {
	switch {
	case verbose >= 2:
		return log.DebugLevel
	case verbose == 1:
		return log.InfoLevel
	default:
		return log.WarnLevel
	}
}

func writerOr(w io.Writer, def *os.File) io.Writer {
	if w == nil {
		return def
	}
	return w
}

func isTTY(v any) bool {
	f, ok := v.(*os.File)
	return ok && term.IsTerminal(f.Fd())
}

// JSONMode reports whether --json was given.
func (r *Renderer) JSONMode() bool { return r.json }

// Interactive reports whether prompting the user is possible.
func (r *Renderer) Interactive() bool { return r.interactive }

// Width is the usable terminal width, or a sane default when not a terminal.
func (r *Renderer) Width() int { return r.width }

// ErrWriter exposes the profile-aware stderr writer, for the plain CI progress
// display that appends lines rather than driving a bubbletea program. Anything
// that does drive one wants ProgramWriter instead.
func (r *Renderer) ErrWriter() io.Writer { return r.err }

// Profile reports the resolved color profile, so nested programs (bubbletea,
// huh) render with the same capabilities as plain output.
func (r *Renderer) Profile() colorprofile.Profile { return r.err.Profile }

// ColorEnabled reports whether the terminal can render color. It is false for
// --no-color, NO_COLOR, --json and every non-terminal writer, which together
// are exactly the cases where a live progress display must fall back to plain
// appended lines.
func (r *Renderer) ColorEnabled() bool { return r.err.Profile >= colorprofile.ANSI }

// ProgramWriter is the writer every bubbletea program must be given: huh's
// forms and spinner here, and the live CI display in internal/ciwait. It is the
// terminal file itself, not r.err, because bubbletea measures its window only
// when the output satisfies term.File. A *colorprofile.Writer does not, so the
// program sits at a 0x0 window and paints nothing at all — an empty frame that
// looks exactly like a hang at a prompt. Pass tea.WithColorProfile(r.Profile())
// alongside it and bypassing r.err costs nothing.
func (r *Renderer) ProgramWriter() io.Writer {
	if r.ttyErr != nil {
		return r.ttyErr
	}
	return r.err
}

// Stdin exposes the input stream for prompts.
func (r *Renderer) Stdin() io.Reader { return r.stdin }

// Line writes one bare line to stdout with no styling. This is the contract
// for `flow path` and `flow assets`: exactly one line, nothing else.
func (r *Renderer) Line(s string) {
	fmt.Fprintln(r.stdout, s)
}

// Printf writes formatted, styled text to stdout.
func (r *Renderer) Printf(format string, a ...any) {
	fmt.Fprintf(r.out, format, a...)
}

// Println writes a styled line to stdout.
func (r *Renderer) Println(a ...any) {
	fmt.Fprintln(r.out, a...)
}

// Status writes a progress line to stderr, keeping stdout pipe-clean.
func (r *Renderer) Status(format string, a ...any) {
	if r.json {
		return
	}
	fmt.Fprintf(r.err, format+"\n", a...)
}

// Success writes a success line to stderr.
func (r *Renderer) Success(format string, a ...any) {
	r.marked(r.Theme.Success, SymOK, format, a...)
}

// Warn writes a warning line to stderr.
func (r *Renderer) Warn(format string, a ...any) {
	r.marked(r.Theme.Warn, SymWarn, format, a...)
}

// Failure writes an error line to stderr without terminating the command.
func (r *Renderer) Failure(format string, a ...any) {
	r.marked(r.Theme.Error, SymFail, format, a...)
}

func (r *Renderer) marked(style lipgloss.Style, sym, format string, a ...any) {
	if r.json {
		return
	}
	fmt.Fprintf(r.err, "%s %s\n", style.Render(sym), fmt.Sprintf(format, a...))
}

// Heading writes a section heading to stderr.
func (r *Renderer) Heading(s string) {
	if r.json {
		return
	}
	fmt.Fprintln(r.err, r.Theme.Heading.Render(s))
}

// Field writes an aligned "label: value" line to stdout.
func (r *Renderer) Field(label, value string) {
	fmt.Fprintf(r.out, "%s %s\n", r.Theme.Muted.Render(pad(label+":", 18)), value)
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

// Table renders rows with lipgloss/table, which measures rendered width and so
// aligns correctly even when cells carry ANSI escapes.
func (r *Renderer) Table(headers []string, rows [][]string) {
	t := table.New().
		Border(lipgloss.NormalBorder()).
		BorderTop(false).BorderBottom(false).BorderLeft(false).
		BorderRight(false).BorderColumn(false).BorderRow(false).
		BorderStyle(r.Theme.Muted).
		Headers(headers...).
		Rows(rows...).
		StyleFunc(func(row, _ int) lipgloss.Style {
			if row == table.HeaderRow {
				return r.Theme.Heading.PaddingRight(2)
			}
			return lipgloss.NewStyle().PaddingRight(2)
		})
	fmt.Fprintln(r.out, t.Render())
}

// JSON writes v as the command's single JSON object on stdout.
func (r *Renderer) JSON(v any) error {
	enc := json.NewEncoder(r.stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// JSONError writes the error envelope required in --json mode.
func (r *Renderer) JSONError(code, message string) {
	_ = r.JSON(map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}

// Truncate shortens s to width runes, ending with an ellipsis.
func Truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	runes := []rune(s)
	if width == 1 {
		return "…"
	}
	for len(runes) > 0 && lipgloss.Width(string(runes))+1 > width {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}
