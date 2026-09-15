package output

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

// MinWatchInterval is the floor on a watch refresh. Every tick spends two or
// three subprocesses — `helm get metadata`, sometimes `helm status`, and
// `kubectl get pods` — against a real cluster, and refreshing faster than this
// only adds API-server load without telling anyone anything new.
const MinWatchInterval = time.Second

// DefaultWatchInterval is the refresh rate a watch uses unless asked otherwise.
const DefaultWatchInterval = 5 * time.Second

// watchSpinnerTick drives the footer spinner independently of the refresh, so
// the view stays visibly alive between ticks rather than freezing.
const watchSpinnerTick = 100 * time.Millisecond

// Watch repaints frame's output in place until the user quits or ctx ends.
//
// frame is called on a bubbletea command goroutine, once immediately and then
// every interval, and must return the whole view as a string — the same split
// internal/ciwait uses, where the frame is pure and the model around it is
// thin. It is never called concurrently with itself.
//
// Outside an interactive terminal there is nothing to repaint in place, so the
// frame is written once and Watch returns; callers get one snapshot rather than
// an error. Quitting deliberately — q, esc or ctrl+c — is success.
func (r *Renderer) Watch(ctx context.Context, interval time.Duration, frame func(context.Context) string) error {
	if interval < MinWatchInterval {
		interval = MinWatchInterval
	}
	if !r.interactive || !r.ColorEnabled() {
		fmt.Fprintln(r.out, strings.TrimRight(frame(ctx), "\n"))
		return nil
	}

	m := watchModel{
		theme:    r.Theme,
		interval: interval,
		frame:    frame,
		ctx:      ctx,
		spin:     spinner.New(spinner.WithSpinner(watchSpinner())),
	}

	// Deliberately not the alt screen, for the same reason the CI display is
	// not: the last frame must stay in scrollback after the watch ends.
	p := tea.NewProgram(m,
		tea.WithContext(ctx),
		tea.WithOutput(r.ProgramWriter()),
		tea.WithColorProfile(r.Profile()),
	)
	if _, err := p.Run(); err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		return err
	}
	return nil
}

func watchSpinner() spinner.Spinner {
	s := spinner.Dot
	s.FPS = watchSpinnerTick
	return s
}

type watchFrameMsg string
type watchTickMsg struct{}

// watchModel repaints whatever frame returns, with a footer saying how to get
// out and when the view was last refreshed.
type watchModel struct {
	theme    Theme
	interval time.Duration
	frame    func(context.Context) string
	ctx      context.Context //nolint:containedctx // the refresh command needs it

	spin    spinner.Model
	body    string
	updated time.Time
}

func (m watchModel) Init() tea.Cmd {
	return tea.Batch(m.spin.Tick, m.refresh())
}

// refresh runs frame off the event loop, so a slow cluster stalls the data and
// not the spinner or the keyboard.
func (m watchModel) refresh() tea.Cmd {
	return func() tea.Msg { return watchFrameMsg(m.frame(m.ctx)) }
}

func (m watchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		}
		return m, nil

	case watchFrameMsg:
		m.body = string(msg)
		m.updated = time.Now()
		return m, tea.Tick(m.interval, func(time.Time) tea.Msg { return watchTickMsg{} })

	case watchTickMsg:
		return m, m.refresh()

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m watchModel) View() tea.View {
	return tea.NewView(m.render())
}

func (m watchModel) render() string {
	var b strings.Builder
	if m.body == "" {
		b.WriteString("  " + strings.TrimSpace(m.spin.View()) + " loading…\n")
	} else {
		b.WriteString(strings.TrimRight(m.body, "\n"))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(m.theme.Muted.Render(m.footer()))
	b.WriteString("\n")
	return b.String()
}

func (m watchModel) footer() string {
	out := fmt.Sprintf("%s refreshing every %s", strings.TrimSpace(m.spin.View()), m.interval)
	if !m.updated.IsZero() {
		out += fmt.Sprintf(" · updated %s", m.updated.Format("15:04:05"))
	}
	return out + " · q to quit"
}
