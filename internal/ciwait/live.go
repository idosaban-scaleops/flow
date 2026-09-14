package ciwait

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/idosaban-scaleops/flow/internal/ghapi"
	"github.com/idosaban-scaleops/flow/internal/output"
)

// spinnerTick drives the spinner independently of the poll interval, so the
// display stays alive between polls rather than freezing for ten seconds at a
// time.
const spinnerTick = 100 * time.Millisecond

// flowSpinner is the dot spinner ticking at spinnerTick.
func flowSpinner() spinner.Spinner {
	s := spinner.Dot
	s.FPS = spinnerTick
	return s
}

// liveDisplay drives a bubbletea program on stderr. bubbletea is used rather
// than hand-rolled cursor math because it handles resize, cursor restoration
// and wrapped lines correctly, all of which break the hand-rolled version.
type liveDisplay struct {
	program *tea.Program
	done    chan struct{}
}

type stateMsg State
type retargetMsg Retarget
type noteMsg string
type etaMsg time.Duration

func newLiveDisplay(r *output.Renderer, target Target, chart string) *liveDisplay {
	m := waitModel{
		theme:   r.Theme,
		target:  target,
		chart:   chart,
		started: time.Now(),
		width:   r.Width(),
		spin:    spinner.New(spinner.WithSpinner(flowSpinner())),
		bar: progress.New(
			progress.WithColors(r.Theme.AccentColor, r.Theme.SuccessColor),
			progress.WithoutPercentage(),
			progress.WithWidth(36),
		),
	}

	// Deliberately not the alt screen: the final frame must stay in scrollback
	// after the command finishes.
	p := tea.NewProgram(m,
		tea.WithOutput(r.ProgramWriter()),
		tea.WithColorProfile(r.Profile()),
	)

	d := &liveDisplay{program: p, done: make(chan struct{})}
	go func() {
		defer close(d.done)
		_, _ = p.Run()
	}()
	return d
}

func (d *liveDisplay) Update(s State)      { d.program.Send(stateMsg(s)) }
func (d *liveDisplay) Retarget(r Retarget) { d.program.Send(retargetMsg(r)) }
func (d *liveDisplay) Note(note string)    { d.program.Send(noteMsg(note)) }

// SetETA supplies an estimate derived from past runs.
func (d *liveDisplay) SetETA(eta time.Duration) { d.program.Send(etaMsg(eta)) }

func (d *liveDisplay) Close() {
	d.program.Quit()
	<-d.done
}

// waitModel renders the progress frame.
type waitModel struct {
	theme   output.Theme
	target  Target
	chart   string
	started time.Time
	width   int

	spin spinner.Model
	bar  progress.Model

	state State
	notes []string
	eta   time.Duration
}

func (m waitModel) Init() tea.Cmd { return m.spin.Tick }

func (m waitModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		barWidth := msg.Width / 3
		if barWidth < 12 {
			barWidth = 12
		}
		if barWidth > 40 {
			barWidth = 40
		}
		m.bar.SetWidth(barWidth)
		return m, nil

	case tea.KeyPressMsg:
		// Interrupt is handled by the caller cancelling the context; quitting
		// here too keeps the terminal responsive.
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		return m, nil

	case stateMsg:
		m.state = State(msg)
		return m, nil

	case retargetMsg:
		m.notes = append(m.notes, fmt.Sprintf("run %d %s — now watching %d",
			msg.From.RunID, msg.Reason, msg.To.RunID))
		m.target = msg.To
		m.state = State{}
		return m, nil

	case noteMsg:
		m.notes = append(m.notes, string(msg))
		return m, nil

	case etaMsg:
		m.eta = time.Duration(msg)
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m waitModel) View() tea.View {
	return tea.NewView(m.render())
}

func (m waitModel) render() string {
	var b strings.Builder
	t := m.theme

	b.WriteString("\n  ")
	b.WriteString(t.Heading.Render(m.headline()))
	b.WriteString("\n\n  ")
	b.WriteString(m.bar.ViewAs(m.state.Percent))
	fmt.Fprintf(&b, "  %3.0f%%   ", m.state.Percent*100)
	b.WriteString(t.Muted.Render(m.timing()))
	b.WriteString("\n\n")

	for _, job := range m.state.Jobs {
		b.WriteString("  ")
		b.WriteString(m.row(job))
		b.WriteString("\n")
	}
	if len(m.state.Jobs) == 0 {
		b.WriteString("  ")
		b.WriteString(t.Muted.Render(strings.TrimSpace(m.spin.View()) + " waiting for jobs to appear"))
		b.WriteString("\n")
	}

	for _, note := range m.notes {
		b.WriteString("\n  ")
		b.WriteString(t.Warn.Render(note))
	}
	if len(m.notes) > 0 {
		b.WriteString("\n")
	}

	if m.chart != "" {
		chart := output.Truncate(m.chart, m.usableWidth()-len("chart")-4)
		b.WriteString("\n  ")
		b.WriteString(t.Muted.Render("chart") + "  " + t.Path.Render(chart))
		b.WriteString("\n")
	}
	return b.String()
}

func (m waitModel) headline() string {
	name := m.target.Name
	if name == "" {
		name = "workflow"
	}
	parts := []string{name}
	if m.target.Branch != "" {
		parts = append(parts, m.target.Branch)
	}
	parts = append(parts, fmt.Sprintf("run %d", m.target.RunID))
	return output.Truncate(strings.Join(parts, " · "), m.usableWidth())
}

func (m waitModel) timing() string {
	out := FormatDuration(time.Since(m.started)) + " elapsed"
	if m.eta > 0 {
		out += fmt.Sprintf("   ~%s remaining (estimate)", FormatDuration(m.eta))
	}
	return out
}

// row renders one job line, truncated to the terminal width so a long step
// name never wraps and corrupts the in-place render.
func (m waitModel) row(job JobView) string {
	t := m.theme

	var symbol string
	switch {
	case job.Skipped():
		symbol = t.Muted.Render(output.SymSkip)
	case job.Status == ghapi.StatusCompleted && job.Conclusion == ghapi.ConclusionSuccess:
		symbol = t.Success.Render(output.SymOK)
	case job.Status == ghapi.StatusCompleted && job.Conclusion != ghapi.ConclusionSuccess:
		symbol = t.Error.Render(output.SymFail)
	case job.Status == ghapi.StatusInProgress:
		symbol = t.Warn.Render(strings.TrimSpace(m.spin.View()))
	default:
		symbol = t.Muted.Render(output.SymPending)
	}

	// Reserve room for the marker, the gap, and the name column.
	nameWidth := 28
	available := m.usableWidth() - nameWidth - 6
	if available < 10 {
		available = 10
	}

	name := output.Truncate(job.Name, nameWidth)
	description := output.Truncate(job.Description, available)

	style := t.Muted
	if job.Status == ghapi.StatusInProgress {
		style = lipgloss.NewStyle()
	}
	return fmt.Sprintf("%s  %-*s  %s", symbol, nameWidth, name, style.Render(description))
}

func (m waitModel) usableWidth() int {
	if m.width <= 0 {
		return 100
	}
	if m.width < 40 {
		return 40
	}
	return m.width - 4
}

// Skipped reports a job GitHub chose not to run.
func (j JobView) Skipped() bool { return j.Conclusion == ghapi.ConclusionSkipped }
