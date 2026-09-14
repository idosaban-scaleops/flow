package output

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/x/term"
)

// TestProgramWriterIsATerminalFile pins the contract that made every prompt in flow
// look like a hang: bubbletea measures its window only when its output
// satisfies term.File, so handing it the colorprofile writer leaves it at 0x0,
// drawing an empty frame forever. Whatever ProgramWriter returns for a terminal
// stderr must therefore be the file itself.
func TestProgramWriterIsATerminalFile(t *testing.T) {
	f := os.Stderr
	r := &Renderer{err: nil, stderr: f, ttyErr: f}

	got := r.ProgramWriter()
	if got != io.Writer(f) {
		t.Fatalf("ProgramWriter() = %T, want the *os.File stderr", got)
	}
	if _, ok := got.(term.File); !ok {
		t.Fatalf("ProgramWriter() = %T, which bubbletea cannot measure; it must be a term.File", got)
	}
}

// TestProgramWriterFallsBackWhenStderrIsNotATerminal covers the injected-buffer
// case tests use. Prompting is already refused there, so this only has to not
// hand back a nil writer.
func TestProgramWriterFallsBackWhenStderrIsNotATerminal(t *testing.T) {
	var buf bytes.Buffer
	r := New(Options{Stderr: &buf, Stdout: &buf, Stdin: strings.NewReader("")})

	if r.ttyErr != nil {
		t.Fatalf("ttyErr = %v, want nil for a buffer", r.ttyErr)
	}
	if r.ProgramWriter() != io.Writer(r.err) {
		t.Errorf("ProgramWriter() = %T, want the colorprofile writer", r.ProgramWriter())
	}
}
