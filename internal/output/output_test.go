package output_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/idosaban-scaleops/flow/internal/output"
)

var update = flag.Bool("update", false, "rewrite golden files")

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
		t.Fatalf("missing golden file %s (run: go test ./internal/output -update): %v", path, err)
	}
	if got != string(want) {
		t.Errorf("output does not match %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

// newRenderer builds a Renderer writing into buffers. Buffers are not
// terminals, so the color profile resolves to plain text automatically — which
// is exactly the behaviour a piped run must have.
func newRenderer(t *testing.T, jsonMode bool) (r *output.Renderer, stdout, stderr *bytes.Buffer) {
	t.Helper()
	var out, errBuf bytes.Buffer
	renderer := output.New(output.Options{
		JSON:   jsonMode,
		Stdout: &out,
		Stderr: &errBuf,
		Stdin:  strings.NewReader(""),
	})
	return renderer, &out, &errBuf
}

func TestNonTTYOutputCarriesNoEscapes(t *testing.T) {
	r, stdout, stderr := newRenderer(t, false)

	r.Println(r.Theme.Ticket.Render("RD-19471"))
	r.Success("created %s", "the worktree")
	r.Warn("herdr is not running")
	r.Failure("something broke")

	for name, buf := range map[string]*bytes.Buffer{"stdout": stdout, "stderr": stderr} {
		if strings.Contains(buf.String(), "\x1b[") {
			t.Errorf("%s contains ANSI escapes when writing to a pipe:\n%q", name, buf.String())
		}
	}
}

func TestLineIsExactlyOneBareLine(t *testing.T) {
	// `cd $(flow path)` depends on this: one line, no styling, nothing else.
	r, stdout, stderr := newRenderer(t, false)
	r.Status("this progress line must not reach stdout")
	r.Line("/repo/.worktrees/RD-19471-add-new-toolbar")

	if got := stdout.String(); got != "/repo/.worktrees/RD-19471-add-new-toolbar\n" {
		t.Errorf("stdout = %q", got)
	}
	if stderr.Len() == 0 {
		t.Error("the progress line should have gone to stderr")
	}
}

func TestJSONModeSuppressesHumanOutput(t *testing.T) {
	r, stdout, stderr := newRenderer(t, true)

	r.Status("progress")
	r.Success("done")
	r.Warn("careful")
	r.Heading("heading")
	if stderr.Len() != 0 {
		t.Errorf("--json must not emit human progress output, got %q", stderr.String())
	}

	if err := r.JSON(map[string]string{"path": "/wt"}); err != nil {
		t.Fatal(err)
	}
	var back map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &back); err != nil {
		t.Fatalf("stdout is not a single JSON object: %v\n%s", err, stdout.String())
	}
	if back["path"] != "/wt" {
		t.Errorf("payload = %v", back)
	}
}

func TestJSONErrorEnvelope(t *testing.T) {
	r, stdout, _ := newRenderer(t, true)
	r.JSONError("not_found", "ticket RD-1 is not registered")

	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "not_found" {
		t.Errorf("code = %q", envelope.Error.Code)
	}
	golden(t, "json-error.json", stdout.String())
}

func TestTableAlignsCellsCarryingStyles(t *testing.T) {
	// The reason for lipgloss/table over text/tabwriter: tabwriter measures
	// byte length, so any cell carrying an escape sequence mis-aligns.
	r, stdout, _ := newRenderer(t, false)

	r.Table(
		[]string{"TICKET", "SLUG", "BRANCH", "STATUS", "PR", "AGE"},
		[][]string{
			{
				r.Theme.Ticket.Render("RD-19471"), "add-new-toolbar",
				"RD-19471-add-new-toolbar", r.Theme.Success.Render("clean"),
				r.Theme.Muted.Render("—"), "2d",
			},
			{
				r.Theme.Ticket.Render("RD-19472"), "original-req-in-graphs",
				"RD-19472-original-req-in-graphs", r.Theme.Warn.Render("dirty ahead 2"),
				r.Theme.Success.Render("#123 merged"), "5h",
			},
			{
				r.Theme.Ticket.Render("RD-19999"), "gone-by-hand",
				"RD-19999-gone-by-hand", r.Theme.Error.Render("gone"),
				r.Theme.Muted.Render("?"), "3w",
			},
		})

	got := stdout.String()
	golden(t, "list-table.txt", got)

	// Every row must be the same rendered width, which is the property
	// tabwriter gets wrong.
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	widths := make([]int, 0, len(lines))
	for _, line := range lines {
		widths = append(widths, len([]rune(line)))
	}
	for i, w := range widths {
		if w != widths[0] {
			t.Errorf("row %d is %d columns wide, but row 0 is %d:\n%s", i, w, widths[0], got)
		}
	}
}

func TestFieldAlignment(t *testing.T) {
	r, stdout, _ := newRenderer(t, false)
	r.Field("ticket", "RD-19471")
	r.Field("branch", "RD-19471-add-new-toolbar")
	r.Field("pull request", "none")
	golden(t, "fields.txt", stdout.String())
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		width int
		want  string
	}{
		{"fits", "hello", 10, "hello"},
		{"exact", "hello", 5, "hello"},
		{"truncates", "hello world", 8, "hello w…"},
		{"width one", "hello", 1, "…"},
		{"width zero", "hello", 0, ""},
		{"negative", "hello", -1, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := output.Truncate(tt.in, tt.width); got != tt.want {
				t.Errorf("Truncate(%q, %d) = %q, want %q", tt.in, tt.width, got, tt.want)
			}
		})
	}
}

func TestPromptsFailWithTheFlagThatWouldAvoidThem(t *testing.T) {
	r, _, _ := newRenderer(t, false)

	err := r.Confirm("Delete anyway?", "", false, false)
	var notInteractive *output.ErrNotInteractive
	if !errors.As(err, &notInteractive) {
		t.Fatalf("err = %v, want *output.ErrNotInteractive", err)
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("the error must name the escape hatch, got %q", err)
	}
	if !strings.Contains(err.Error(), "not a terminal") {
		t.Errorf("the error must explain why, got %q", err)
	}
}

func TestAssumeYesSkipsThePrompt(t *testing.T) {
	r, _, _ := newRenderer(t, false)
	if err := r.Confirm("Delete anyway?", "", false, true); err != nil {
		t.Errorf("--yes must satisfy a confirmation without prompting: %v", err)
	}
}

func TestNoColorStripsColorButKeepsAttributes(t *testing.T) {
	// --no-color forces the ASCII profile, which removes color while leaving
	// bold and underline — the NO_COLOR convention is about color specifically.
	// A non-terminal writer goes further and strips everything, which is what
	// TestNonTTYOutputCarriesNoEscapes covers.
	var stdout bytes.Buffer
	r := output.New(output.Options{NoColor: true, Stdout: &stdout, Stderr: &stdout})

	if r.ColorEnabled() {
		t.Error("--no-color must report color as disabled")
	}

	r.Println(r.Theme.Error.Render("boom"))
	if colorEscape.MatchString(stdout.String()) {
		t.Errorf("--no-color output still contains a color escape: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "boom") {
		t.Errorf("the text itself was lost: %q", stdout.String())
	}
}

// colorEscape matches SGR sequences that set a foreground or background color.
var colorEscape = regexp.MustCompile(`\x1b\[(?:[349][0-7]|10[0-7]|[34]8;)`)
