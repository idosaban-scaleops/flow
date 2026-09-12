package herdr_test

import (
	"context"
	"testing"

	flowexec "github.com/idosaban-scaleops/flow/internal/exec"
	"github.com/idosaban-scaleops/flow/internal/herdr"
)

func TestCreateWorkspaceArgv(t *testing.T) {
	f := flowexec.NewFake().Respond("herdr workspace create",
		`{"result":{"workspace":{"id":"3"},"tab":{"id":"3:1"},"root_pane":{"id":"3:1.1"}}}`)

	ws, err := herdr.New(f, "herdr", nil).
		CreateWorkspace(context.Background(), "/repo/.worktrees/RD-1-a", "RD-1")
	if err != nil {
		t.Fatal(err)
	}

	want := "herdr workspace create --cwd /repo/.worktrees/RD-1-a --label RD-1"
	if got := f.CommandLines(); len(got) != 1 || got[0] != want {
		t.Errorf("argv = %v, want [%s]", got, want)
	}
	if ws.ID != "3" || ws.TabID != "3:1" || ws.RootPaneID != "3:1.1" {
		t.Errorf("workspace = %+v", ws)
	}
	if ws.Label != "RD-1" {
		t.Errorf("label = %q, want the label flow asked for", ws.Label)
	}
}

func TestCreateWorkspaceToleratesUnknownShapes(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		wantID string
	}{
		{
			name:   "identifiers as numbers",
			stdout: `{"result":{"workspace":{"id":3},"tab":{"id":"3:1"}}}`,
			wantID: "3",
		},
		{
			name:   "no result envelope",
			stdout: `{"workspace":{"id":"7"}}`,
			wantID: "7",
		},
		{
			name:   "flat shape",
			stdout: `{"id":"9","label":"RD-1"}`,
			wantID: "9",
		},
		{
			name:   "camelCase root pane",
			stdout: `{"result":{"workspace":{"id":"4"},"rootPane":{"id":"4:1.1"}}}`,
			wantID: "4",
		},
		{
			// Created but unidentified: flow must record the label, not fail.
			name:   "completely unknown payload",
			stdout: `{"result":{"totally":"different"}}`,
			wantID: "",
		},
		{
			name:   "human text instead of JSON",
			stdout: "created workspace 3\n",
			wantID: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := flowexec.NewFake().Respond("herdr workspace create", tt.stdout)
			ws, err := herdr.New(f, "herdr", nil).
				CreateWorkspace(context.Background(), "/wt", "RD-1")
			if err != nil {
				t.Fatalf("an unrecognized payload must not fail the command: %v", err)
			}
			if ws.ID != tt.wantID {
				t.Errorf("ID = %q, want %q", ws.ID, tt.wantID)
			}
			if ws.Label != "RD-1" {
				t.Errorf("Label = %q, want RD-1 to remain recoverable", ws.Label)
			}
			if tt.wantID == "" && ws.Identified() {
				t.Error("a workspace with no ID must report Identified() == false")
			}
		})
	}
}

func TestListWorkspaces(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
	}{
		{"array in result", `{"result":[{"id":"1","label":"RD-1"},{"id":"2","label":"RD-2"}]}`},
		{"bare array", `[{"id":"1","label":"RD-1"},{"id":"2","label":"RD-2"}]`},
		{"object with workspaces", `{"result":{"workspaces":[{"id":"1","label":"RD-1"},{"id":"2","label":"RD-2"}]}}`},
		{"numeric ids", `{"result":[{"id":1,"label":"RD-1"},{"id":2,"label":"RD-2"}]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := flowexec.NewFake().Respond("herdr workspace list", tt.stdout)
			list, err := herdr.New(f, "herdr", nil).ListWorkspaces(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(list) != 2 {
				t.Fatalf("want 2 workspaces, got %d: %+v", len(list), list)
			}
			got, ok := herdr.FindByLabel(list, "RD-2")
			if !ok || got.ID != "2" {
				t.Errorf("FindByLabel(RD-2) = %+v, ok=%v", got, ok)
			}
		})
	}
}

func TestAvailableNeverRunsBareHerdr(t *testing.T) {
	f := flowexec.NewFake().Respond("herdr workspace list", `{"result":[]}`)

	if !herdr.New(f, "herdr", nil).Available(context.Background()) {
		t.Error("herdr should be reported available")
	}
	for _, line := range f.CommandLines() {
		if line == "herdr" {
			t.Fatal("running bare herdr launches its TUI and must never happen")
		}
	}
}

func TestAvailableFalseWhenBinaryMissing(t *testing.T) {
	f := flowexec.NewFake()
	f.Missing["herdr"] = true
	if herdr.New(f, "herdr", nil).Available(context.Background()) {
		t.Error("herdr must not be reported available when it is not on PATH")
	}
	if len(f.Calls) != 0 {
		t.Errorf("no command should run when the binary is missing, got %v", f.CommandLines())
	}
}

func TestAvailableFalseWhenServerUnreachable(t *testing.T) {
	f := flowexec.NewFake()
	f.RespondWith("herdr workspace list", flowexec.Response{
		ExitCode: 1, Stderr: "error: could not connect to herdr server",
	})
	if herdr.New(f, "herdr", nil).Available(context.Background()) {
		t.Error("an unreachable herdr server must report unavailable")
	}
}

func TestFocusAndCloseArgv(t *testing.T) {
	f := flowexec.NewFake().Respond("herdr workspace", `{"result":{}}`)
	c := herdr.New(f, "herdr", nil)

	if err := c.FocusWorkspace(context.Background(), "3"); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWorkspace(context.Background(), "3"); err != nil {
		t.Fatal(err)
	}

	want := []string{"herdr workspace focus 3", "herdr workspace close 3"}
	got := f.CommandLines()
	for i, w := range want {
		if i >= len(got) || got[i] != w {
			t.Errorf("argv[%d] = %q, want %q", i, got, w)
		}
	}
}

func TestCustomBinaryName(t *testing.T) {
	f := flowexec.NewFake().Respond("/opt/herdr workspace list", `{"result":[]}`)
	if _, err := herdr.New(f, "/opt/herdr", nil).ListWorkspaces(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !f.Ran("/opt/herdr workspace list") {
		t.Errorf("configured binary was not used: %v", f.CommandLines())
	}
}
