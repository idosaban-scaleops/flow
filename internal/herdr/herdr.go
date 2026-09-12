// Package herdr drives the herdr terminal multiplexer through its JSON CLI.
//
// Two rules matter here. Never run bare `herdr` — that launches the TUI, even
// as a capability probe. And never fail flow because herdr is unavailable: a
// missing workspace is a warning, since the worktree and the editor are the
// parts that carry the work.
package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	flowexec "github.com/idosaban-scaleops/flow/internal/exec"
)

// Workspace is a herdr workspace as flow records it.
type Workspace struct {
	ID         string `json:"id,omitempty"`
	Label      string `json:"label,omitempty"`
	TabID      string `json:"tab_id,omitempty"`
	RootPaneID string `json:"root_pane_id,omitempty"`
	CWD        string `json:"cwd,omitempty"`
}

// Identified reports whether flow learned the workspace's ID. A workspace can
// be created successfully yet unidentified if herdr's JSON shape has changed.
func (w Workspace) Identified() bool { return w.ID != "" }

// Client is the herdr surface flow uses.
type Client interface {
	Available(ctx context.Context) bool
	CreateWorkspace(ctx context.Context, cwd, label string) (Workspace, error)
	ListWorkspaces(ctx context.Context) ([]Workspace, error)
	FocusWorkspace(ctx context.Context, id string) error
	CloseWorkspace(ctx context.Context, id string) error
}

// Logger records parse fallbacks at -v. The signature matches
// charmbracelet/log, whose message parameter is an any.
type Logger interface {
	Debug(msg any, keyvals ...any)
}

// CLI is the herdr binary wrapper.
type CLI struct {
	Runner flowexec.Runner
	// Bin is the herdr executable name.
	Bin string
	Log Logger
}

var _ Client = (*CLI)(nil)

// New returns a CLI for the configured herdr binary.
func New(r flowexec.Runner, bin string, log Logger) *CLI {
	if bin == "" {
		bin = "herdr"
	}
	return &CLI{Runner: r, Bin: bin, Log: log}
}

// Available probes herdr without launching its TUI. `herdr workspace list`
// succeeds only when the local server is reachable, which is exactly the
// question callers are asking.
func (c *CLI) Available(ctx context.Context) bool {
	if _, err := c.Runner.LookPath(c.Bin); err != nil {
		return false
	}
	_, err := c.run(ctx, "workspace", "list")
	return err == nil
}

// Version reports herdr's version string, for `flow doctor`.
func (c *CLI) Version(ctx context.Context) (string, error) {
	res, err := c.Runner.Run(ctx, flowexec.Opts{Name: c.Bin, Args: []string{"--version"}})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout + res.Stderr), nil
}

// CreateWorkspace opens a plain empty shell at cwd. It deliberately starts no
// agent and splits no panes.
func (c *CLI) CreateWorkspace(ctx context.Context, cwd, label string) (Workspace, error) {
	out, err := c.run(ctx, "workspace", "create", "--cwd", cwd, "--label", label)
	if err != nil {
		return Workspace{}, err
	}

	ws, ok := parseWorkspace(out)
	if !ok {
		// Created, but the JSON shape was not what this version of flow knows.
		// Record the label so `flow open` can still find it by name.
		c.debug("herdr workspace create returned an unrecognized payload", "output", out)
		return Workspace{Label: label, CWD: cwd}, nil
	}
	if ws.Label == "" {
		ws.Label = label
	}
	if ws.CWD == "" {
		ws.CWD = cwd
	}
	return ws, nil
}

// ListWorkspaces returns every workspace herdr knows about.
func (c *CLI) ListWorkspaces(ctx context.Context) ([]Workspace, error) {
	out, err := c.run(ctx, "workspace", "list")
	if err != nil {
		return nil, err
	}
	return parseWorkspaceList(out), nil
}

// FocusWorkspace brings an existing workspace to the front.
func (c *CLI) FocusWorkspace(ctx context.Context, id string) error {
	_, err := c.run(ctx, "workspace", "focus", id)
	return err
}

// CloseWorkspace closes a workspace. flow never calls this during delete — that
// is an explicit non-goal — but `flow prune` and future commands may.
func (c *CLI) CloseWorkspace(ctx context.Context, id string) error {
	_, err := c.run(ctx, "workspace", "close", id)
	return err
}

// FindByLabel returns the workspace carrying label.
func FindByLabel(list []Workspace, label string) (Workspace, bool) {
	for _, w := range list {
		if w.Label == label {
			return w, true
		}
	}
	return Workspace{}, false
}

// FindByID returns the workspace with the given ID.
func FindByID(list []Workspace, id string) (Workspace, bool) {
	if id == "" {
		return Workspace{}, false
	}
	for _, w := range list {
		if w.ID == id {
			return w, true
		}
	}
	return Workspace{}, false
}

func (c *CLI) run(ctx context.Context, args ...string) (string, error) {
	res, err := c.Runner.Run(ctx, flowexec.Opts{Name: c.Bin, Args: args})
	if err != nil {
		return "", fmt.Errorf("herdr %s: %w", strings.Join(args, " "), err)
	}
	return res.Stdout, nil
}

func (c *CLI) debug(msg string, keyvals ...any) {
	if c.Log != nil {
		c.Log.Debug(msg, keyvals...)
	}
}

// --- permissive parsing ----------------------------------------------------
//
// herdr's JSON shape differs across versions, so decoding is deliberately
// tolerant: identifiers may be strings or numbers, and may live at the top
// level or under `result`.

type envelope struct {
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

type rawWorkspace struct {
	Workspace  *rawNode    `json:"workspace"`
	Tab        *rawNode    `json:"tab"`
	RootPane   *rawNode    `json:"root_pane"`
	RootPaneV2 *rawNode    `json:"rootPane"`
	ID         flexID      `json:"id"`
	Label      string      `json:"label"`
	Name       string      `json:"name"`
	CWD        string      `json:"cwd"`
	Workspaces []*rawEntry `json:"workspaces"`
}

type rawEntry struct {
	ID    flexID `json:"id"`
	Label string `json:"label"`
	Name  string `json:"name"`
	CWD   string `json:"cwd"`
}

type rawNode struct {
	ID    flexID `json:"id"`
	Label string `json:"label"`
	Name  string `json:"name"`
	CWD   string `json:"cwd"`
}

// flexID accepts an identifier given either as a JSON string or a number.
type flexID string

func (f *flexID) UnmarshalJSON(data []byte) error {
	data = []byte(strings.TrimSpace(string(data)))
	if string(data) == "null" {
		*f = ""
		return nil
	}
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*f = flexID(s)
		return nil
	}
	*f = flexID(strings.Trim(string(data), `"`))
	return nil
}

// unwrap returns the payload to decode, preferring `result` when present.
func unwrap(out string) []byte {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return nil
	}
	var env envelope
	if err := json.Unmarshal([]byte(trimmed), &env); err == nil && len(env.Result) > 0 {
		return env.Result
	}
	return []byte(trimmed)
}

func parseWorkspace(out string) (Workspace, bool) {
	payload := unwrap(out)
	if len(payload) == 0 {
		return Workspace{}, false
	}

	var raw rawWorkspace
	if err := json.Unmarshal(payload, &raw); err != nil {
		return Workspace{}, false
	}

	ws := Workspace{ID: string(raw.ID), Label: firstNonEmpty(raw.Label, raw.Name), CWD: raw.CWD}
	if raw.Workspace != nil {
		ws.ID = firstNonEmpty(string(raw.Workspace.ID), ws.ID)
		ws.Label = firstNonEmpty(raw.Workspace.Label, raw.Workspace.Name, ws.Label)
		ws.CWD = firstNonEmpty(raw.Workspace.CWD, ws.CWD)
	}
	if raw.Tab != nil {
		ws.TabID = string(raw.Tab.ID)
	}
	if pane := firstNonNil(raw.RootPane, raw.RootPaneV2); pane != nil {
		ws.RootPaneID = string(pane.ID)
	}

	if ws.ID == "" && ws.TabID == "" && ws.Label == "" {
		return Workspace{}, false
	}
	return ws, true
}

func parseWorkspaceList(out string) []Workspace {
	payload := unwrap(out)
	if len(payload) == 0 {
		return nil
	}

	// The list may be a bare array or an object with a `workspaces` key.
	var entries []*rawEntry
	if err := json.Unmarshal(payload, &entries); err != nil {
		var wrapped rawWorkspace
		if err := json.Unmarshal(payload, &wrapped); err != nil {
			return nil
		}
		entries = wrapped.Workspaces
	}

	var out2 []Workspace
	for _, e := range entries {
		if e == nil {
			continue
		}
		out2 = append(out2, Workspace{
			ID:    string(e.ID),
			Label: firstNonEmpty(e.Label, e.Name),
			CWD:   e.CWD,
		})
	}
	return out2
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstNonNil(nodes ...*rawNode) *rawNode {
	for _, n := range nodes {
		if n != nil {
			return n
		}
	}
	return nil
}
