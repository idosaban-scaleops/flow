package herdr_test

import (
	"context"
	"strings"
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

// The payloads below are verbatim herdr output. herdr names the identifier
// `workspace_id`, not `id`, and puts a command identifier under `id` at the
// envelope's top level — so parsing that reads `id` records no workspace ID at
// all, and a workspace renamed in herdr can then only be found by its stale
// label, which is to say not at all.
func TestListWorkspacesParsesRealHerdrOutput(t *testing.T) {
	const stdout = `{"id":"cli:workspace:list","result":{"type":"workspace_list","workspaces":[` +
		`{"active_tab_id":"w13:tB","agent_status":"working","focused":true,"label":"Flow","number":1,"pane_count":4,"tab_count":4,"workspace_id":"w13"},` +
		`{"active_tab_id":"w12:t5","agent_status":"idle","focused":false,"label":"ROT Auto Timeline","number":3,"pane_count":11,"tab_count":11,"workspace_id":"w12"}]}}`

	f := flowexec.NewFake().Respond("herdr workspace list", stdout)
	list, err := herdr.New(f, "herdr", nil).ListWorkspaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 workspaces, got %d: %+v", len(list), list)
	}

	// A workspace renamed in herdr is still found by the ID flow recorded.
	got, ok := herdr.FindByID(list, "w12")
	if !ok {
		t.Fatalf("FindByID(w12) missed: %+v", list)
	}
	if got.Label != "ROT Auto Timeline" {
		t.Errorf("label = %q, want the renamed label", got.Label)
	}
}

func TestCreateWorkspaceParsesRealHerdrOutput(t *testing.T) {
	const stdout = `{"id":"cli:workspace:create","result":{` +
		`"root_pane":{"agent_status":"unknown","cwd":"/private/tmp","focused":false,"foreground_cwd":"/private/tmp","pane_id":"w17:p1","revision":0,"tab_id":"w17:t1","terminal_id":"term_65b8614be62b950","workspace_id":"w17"},` +
		`"tab":{"agent_status":"unknown","focused":false,"label":"1","number":1,"pane_count":1,"tab_id":"w17:t1","workspace_id":"w17"},` +
		`"type":"workspace_created",` +
		`"workspace":{"active_tab_id":"w17:t1","agent_status":"unknown","focused":false,"label":"RD-1","number":9,"pane_count":1,"tab_count":1,"workspace_id":"w17"}}}`

	f := flowexec.NewFake().Respond("herdr workspace create", stdout)
	ws, err := herdr.New(f, "herdr", nil).CreateWorkspace(context.Background(), "/wt", "RD-1")
	if err != nil {
		t.Fatal(err)
	}
	if ws.ID != "w17" {
		t.Errorf("ID = %q, want w17 — without it a rename in herdr loses the workspace", ws.ID)
	}
	if !ws.Identified() {
		t.Error("a workspace herdr identified must report Identified() == true")
	}
	if ws.TabID != "w17:t1" || ws.RootPaneID != "w17:p1" {
		t.Errorf("tab/pane = %q/%q, want w17:t1/w17:p1", ws.TabID, ws.RootPaneID)
	}
}

// The envelope's own `id` is a command name, never a workspace.
func TestCommandIDIsNeverTakenForAWorkspaceID(t *testing.T) {
	f := flowexec.NewFake().Respond("herdr workspace create",
		`{"id":"cli:workspace:create","type":"workspace_info","label":"RD-1"}`)
	ws, err := herdr.New(f, "herdr", nil).CreateWorkspace(context.Background(), "/wt", "RD-1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(ws.ID, "cli:") {
		t.Errorf("ID = %q, want the command identifier ignored", ws.ID)
	}
}
