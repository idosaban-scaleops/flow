package ciwait_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/charmbracelet/colorprofile"

	"github.com/idosaban-scaleops/flow/internal/ciwait"
	"github.com/idosaban-scaleops/flow/internal/ghapi"
	"github.com/idosaban-scaleops/flow/internal/output"
)

var update = flag.Bool("update", false, "rewrite golden files")

// elapsedPattern normalizes the elapsed-time field, which moves with the clock.
var elapsedPattern = regexp.MustCompile(`\d+m\d+s elapsed`)

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)

	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden file %s (run: go test ./internal/ciwait -update): %v", path, err)
	}
	if got != string(want) {
		t.Errorf("output does not match %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

// ascii strips color the same way flow does at runtime: lipgloss v2 keeps
// styles profile-agnostic and the colorprofile writer performs the downgrade,
// so forcing the writer to ASCII is the v2 equivalent of a plain renderer.
func ascii(frame string) string {
	var buf bytes.Buffer
	w := colorprofile.NewWriter(&buf, []string{"TERM=dumb"})
	w.Profile = colorprofile.Ascii
	_, _ = w.WriteString(frame)
	return buf.String()
}

func sampleState() ciwait.State {
	return ciwait.State{
		Phase:   ciwait.PhaseWaiting,
		Matched: 5,
		Percent: 0.67,
		Jobs: []ciwait.JobView{
			{Name: "Setup", Description: "done in 41s",
				Status: ghapi.StatusCompleted, Conclusion: ghapi.ConclusionSuccess},
			{Name: "Helm Build", Description: "done in 2m03s",
				Status: ghapi.StatusCompleted, Conclusion: ghapi.ConclusionSuccess},
			{Name: "Pre Release Helm", Description: "Publishing chart to Helm repository",
				Status: ghapi.StatusInProgress},
			{Name: "Pre Release Images / Build And Push",
				Description: "Building and pushing scaleops:v1.0.199-alpha-RD-19472-original-req-in-graphs",
				Status:      ghapi.StatusInProgress},
			{Name: "Pre Release Images / GPU", Description: "skipped",
				Status: ghapi.StatusCompleted, Conclusion: ghapi.ConclusionSkipped},
		},
	}
}

func TestLiveFrameLayout(t *testing.T) {
	target := ciwait.Target{
		RunID: 34468169597, Name: "Build And Release", Branch: "RD-19472-original-req-in-graphs",
	}
	chart := "v1.0.199-alpha-RD-19472-original-req-in-graphs-34468169597"

	for _, width := range []int{60, 100, 140} {
		t.Run(widthName(width), func(t *testing.T) {
			got := ascii(ciwait.RenderFrameForTest(
				output.NewTheme(true), target, chart, sampleState(), width))
			got = elapsedPattern.ReplaceAllString(got, "4m12s elapsed")

			// No row may exceed the terminal width, or the in-place render
			// corrupts itself on the next frame.
			for _, line := range splitLines(got) {
				if w := visibleWidth(line); w > width {
					t.Errorf("line is %d columns wide, exceeding the %d-column terminal:\n%q",
						w, width, line)
				}
			}
			golden(t, "frame-"+widthName(width)+".txt", got)
		})
	}
}

func TestPlainDisplayEmitsOnlyTransitions(t *testing.T) {
	var buf bytes.Buffer
	d := ciwait.NewPlainDisplayForTest(&buf, ciwait.Target{
		RunID: 34468169597, Name: "Build And Release",
		URL: "https://github.com/o/r/actions/runs/34468169597",
	})

	first := ciwait.State{Jobs: []ciwait.JobView{
		{Name: "Setup", Description: "Set up job", Status: ghapi.StatusInProgress},
	}}
	// Polling repeatedly with no change must not produce a heartbeat.
	d.Update(first)
	d.Update(first)
	d.Update(first)

	d.Update(ciwait.State{Jobs: []ciwait.JobView{
		{Name: "Setup", Description: "done in 41s",
			Status: ghapi.StatusCompleted, Conclusion: ghapi.ConclusionSuccess},
		{Name: "Helm Build", Description: "Linting chart", Status: ghapi.StatusInProgress},
	}})
	d.Close()

	lines := nonEmptyLines(buf.String())
	if len(lines) != 4 {
		t.Fatalf("want 4 lines (header, one transition each), got %d:\n%s", len(lines), buf.String())
	}

	got := timestampPattern.ReplaceAllString(buf.String(), "[00:00]")
	golden(t, "plain-transitions.txt", got)
}

var timestampPattern = regexp.MustCompile(`\[\d\d:\d\d\]`)

func TestPlainDisplayReportsRetarget(t *testing.T) {
	var buf bytes.Buffer
	d := ciwait.NewPlainDisplayForTest(&buf, ciwait.Target{RunID: 100, Name: "Build And Release"})

	state := ciwait.State{Jobs: []ciwait.JobView{
		{Name: "Setup", Description: "running", Status: ghapi.StatusInProgress},
	}}
	d.Update(state)
	d.Retarget(ciwait.Retarget{
		From:   ciwait.Target{RunID: 100},
		To:     ciwait.Target{RunID: 200},
		Reason: "was cancelled by a newer push",
	})
	// After a retarget the same row must be reported again for the new run.
	d.Update(state)

	got := buf.String()
	if n := countOccurrences(got, "Setup: running"); n != 2 {
		t.Errorf("Setup reported %d times, want 2 (once per run)", n)
	}
	if !contains(got, "now watching 200") {
		t.Errorf("the retarget was not announced:\n%s", got)
	}
}

func widthName(w int) string {
	switch w {
	case 60:
		return "narrow"
	case 100:
		return "default"
	default:
		return "wide"
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range splitLines(s) {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// visibleWidth counts printable columns, ignoring ANSI escape sequences.
func visibleWidth(s string) int {
	stripped := ansiPattern.ReplaceAllString(s, "")
	return len([]rune(stripped))
}

var ansiPattern = regexp.MustCompile("\x1b\\[[0-9;]*[a-zA-Z]")

func countOccurrences(s, substr string) int {
	var n int
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			n++
		}
	}
	return n
}
