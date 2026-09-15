//go:build integration

// End-to-end tests that drive the real command tree against a real git
// repository. They are behind a build tag so `go test -short ./...` stays fast.
package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/idosaban-scaleops/flow/internal/cli"
	"github.com/idosaban-scaleops/flow/internal/registry"
)

// harness is one isolated flow installation: its own repo, config and registry.
type harness struct {
	t        *testing.T
	repoRoot string
	config   string
	registry string
	assets   string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	base := t.TempDir()
	h := &harness{
		t:        t,
		repoRoot: filepath.Join(base, "repo"),
		config:   filepath.Join(base, "config.yaml"),
		registry: filepath.Join(base, "registry.json"),
		assets:   filepath.Join(base, "assets"),
	}

	origin := filepath.Join(base, "origin.git")
	h.git(base, "init", "--bare", "--initial-branch=main", origin)
	h.git(base, "init", "--initial-branch=main", h.repoRoot)
	if err := os.WriteFile(filepath.Join(h.repoRoot, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.git(h.repoRoot, "add", ".")
	h.git(h.repoRoot, "commit", "-m", "initial")
	h.git(h.repoRoot, "tag", "v1.0.199")
	h.git(h.repoRoot, "remote", "add", "origin", "git@github.com:scaleops-sh/scaleops.git")

	body := strings.Join([]string{
		"assets:",
		"  root: " + h.assets,
		"workspace:",
		"  provider: none",
		"defaults:",
		"  open_editor: false",
		"  fetch_before_branch: false",
	}, "\n")
	if err := os.WriteFile(h.config, []byte(body+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("FLOW_CONFIG", h.config)
	t.Setenv("FLOW_REGISTRY", h.registry)
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	return h
}

func (h *harness) git(dir string, args ...string) string {
	h.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=flow", "GIT_AUTHOR_EMAIL=flow@example.com",
		"GIT_COMMITTER_NAME=flow", "GIT_COMMITTER_EMAIL=flow@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// run executes the command tree from inside dir and returns its streams.
func (h *harness) run(dir string, args ...string) (stdout, stderr string, err error) {
	h.t.Helper()

	previous, wdErr := os.Getwd()
	if wdErr != nil {
		h.t.Fatal(wdErr)
	}
	if err := os.Chdir(dir); err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = os.Chdir(previous) }()

	var out, errBuf bytes.Buffer
	root := cli.NewRootCommand()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetIn(strings.NewReader(""))
	root.SetArgs(args)

	err = root.Execute()
	return out.String(), errBuf.String(), err
}

func (h *harness) mustRun(dir string, args ...string) (stdout, stderr string) {
	h.t.Helper()
	out, errOut, err := h.run(dir, args...)
	if err != nil {
		h.t.Fatalf("flow %s failed: %v\nstdout:\n%s\nstderr:\n%s",
			strings.Join(args, " "), err, out, errOut)
	}
	return out, errOut
}

func (h *harness) entries() []registry.Entry {
	h.t.Helper()
	file, err := registry.Open(h.registry).Load()
	if err != nil {
		h.t.Fatal(err)
	}
	return file.Entries
}

// TestInitIsIdempotent is the contract from the spec: running init twice
// produces no errors, no duplicates, and an identical entry apart from
// last_opened_at.
func TestInitIsIdempotent(t *testing.T) {
	h := newHarness(t)

	h.mustRun(h.repoRoot, "init", "RD-19471", "add", "new", "toolbar")
	first := h.entries()
	if len(first) != 1 {
		t.Fatalf("want one entry after the first init, got %d", len(first))
	}

	_, stderr := h.mustRun(h.repoRoot, "init", "RD-19471", "add", "new", "toolbar")
	second := h.entries()

	if len(second) != 1 {
		t.Fatalf("a second init created %d entries; it must adopt, not duplicate", len(second))
	}
	if !strings.Contains(stderr, "adopting existing worktree") {
		t.Errorf("the second run should say it adopted the worktree, got:\n%s", stderr)
	}

	a, b := first[0], second[0]
	a.LastOpenedAt, b.LastOpenedAt = a.CreatedAt, a.CreatedAt
	if !reflect.DeepEqual(a, b) {
		t.Errorf("the entry changed between identical runs:\n%+v\n%+v", a, b)
	}

	// No duplicate worktrees and no duplicate branches.
	worktrees := h.git(h.repoRoot, "worktree", "list")
	if n := linesContaining(worktrees, "RD-19471-add-new-toolbar"); n != 1 {
		t.Errorf("%d worktrees for the ticket, want 1:\n%s", n, worktrees)
	}
	branches := h.git(h.repoRoot, "branch", "--list")
	if n := linesContaining(branches, "RD-19471-add-new-toolbar"); n != 1 {
		t.Errorf("%d branches for the ticket, want 1:\n%s", n, branches)
	}
}

func TestInitCreatesTheDocumentedLayout(t *testing.T) {
	h := newHarness(t)
	h.mustRun(h.repoRoot, "init", "RD-19471", "Add New Toolbar!")

	entry := h.entries()[0]

	wantWorktree := filepath.Join(h.repoRoot, ".worktrees", "RD-19471-add-new-toolbar")
	if !sameFile(t, entry.WorktreePath, wantWorktree) {
		t.Errorf("worktree = %q, want %q", entry.WorktreePath, wantWorktree)
	}
	if entry.Branch != "RD-19471-add-new-toolbar" {
		t.Errorf("branch = %q — branches use a hyphen", entry.Branch)
	}

	// The assets directory uses an underscore; that difference is deliberate.
	wantAssets := filepath.Join(h.assets, "RD-19471_add-new-toolbar")
	if entry.AssetsPath != wantAssets {
		t.Errorf("assets = %q, want %q", entry.AssetsPath, wantAssets)
	}
	if _, err := os.Stat(wantAssets); err != nil {
		t.Errorf("the assets directory was not created: %v", err)
	}

	// .worktrees/ must be excluded, exactly once.
	exclude, err := os.ReadFile(filepath.Join(h.repoRoot, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	if n := countLines(string(exclude), ".worktrees/"); n != 1 {
		t.Errorf(".worktrees/ appears %d times in info/exclude, want 1", n)
	}
}

func TestPathPrintsExactlyOneLine(t *testing.T) {
	h := newHarness(t)
	h.mustRun(h.repoRoot, "init", "RD-19471", "add", "new", "toolbar")

	stdout, _ := h.mustRun(h.repoRoot, "path", "RD-19471")
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout has %d lines; `cd $(flow path)` needs exactly 1:\n%q", len(lines), stdout)
	}
	if !strings.HasSuffix(lines[0], "RD-19471-add-new-toolbar") {
		t.Errorf("path = %q", lines[0])
	}
}

func TestTicketInferredFromTheWorktree(t *testing.T) {
	h := newHarness(t)
	h.mustRun(h.repoRoot, "init", "RD-19471", "add", "new", "toolbar")

	nested := filepath.Join(h.repoRoot, ".worktrees", "RD-19471-add-new-toolbar", "pkg", "api")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	stdout, _ := h.mustRun(nested, "path")
	if !strings.Contains(stdout, "RD-19471-add-new-toolbar") {
		t.Errorf("the ticket was not inferred from the cwd: %q", stdout)
	}
}

// TestInitAdoptsAWorktreeOnADifferentBranch covers the adoption rule that the
// actual branch wins over the derived one.
func TestInitAdoptsAWorktreeOnADifferentBranch(t *testing.T) {
	h := newHarness(t)

	path := filepath.Join(h.repoRoot, ".worktrees", "RD-19471-add-new-toolbar")
	h.git(h.repoRoot, "worktree", "add", "-b", "something-else", path, "main")

	_, stderr := h.mustRun(h.repoRoot, "init", "RD-19471", "add", "new", "toolbar")

	entry := h.entries()[0]
	if entry.Branch != "something-else" {
		t.Errorf("branch = %q, want the actual checked-out branch", entry.Branch)
	}
	if !strings.Contains(stderr, "something-else") {
		t.Errorf("the mismatch should be warned about, got:\n%s", stderr)
	}
}

// TestInitRefusesToClobberANonEmptyDirectory is the one case where refusing is
// the right answer.
func TestInitRefusesToClobberANonEmptyDirectory(t *testing.T) {
	h := newHarness(t)

	path := filepath.Join(h.repoRoot, ".worktrees", "RD-19471-add-new-toolbar")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "important.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, err := h.run(h.repoRoot, "init", "RD-19471", "add", "new", "toolbar")
	if err == nil {
		t.Fatal("flow must refuse rather than clobber unknown files")
	}
	if code := cli.ExitCodeFor(err); code != cli.ExitPrecondition {
		t.Errorf("exit code = %d, want %d", code, cli.ExitPrecondition)
	}
	if _, statErr := os.Stat(filepath.Join(path, "important.txt")); statErr != nil {
		t.Error("the existing file was destroyed")
	}
}

func TestInitAdoptsAnEmptyDirectory(t *testing.T) {
	h := newHarness(t)

	path := filepath.Join(h.repoRoot, ".worktrees", "RD-19471-add-new-toolbar")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}

	h.mustRun(h.repoRoot, "init", "RD-19471", "add", "new", "toolbar")
	if len(h.entries()) != 1 {
		t.Error("an empty directory should have been replaced by a real worktree")
	}
}

func TestAdoptOnlyRefusesToCreate(t *testing.T) {
	h := newHarness(t)

	_, _, err := h.run(h.repoRoot, "init", "RD-19471", "add", "new", "toolbar", "--adopt-only")
	if err == nil {
		t.Fatal("--adopt-only must fail when there is nothing to adopt")
	}
	if code := cli.ExitCodeFor(err); code != cli.ExitNotFound {
		t.Errorf("exit code = %d, want %d", code, cli.ExitNotFound)
	}
	if _, statErr := os.Stat(filepath.Join(h.repoRoot, ".worktrees")); statErr == nil {
		t.Error("--adopt-only created a directory")
	}

	// Now create one by hand and adopt it.
	path := filepath.Join(h.repoRoot, ".worktrees", "RD-19471-add-new-toolbar")
	h.git(h.repoRoot, "worktree", "add", "-b", "RD-19471-add-new-toolbar", path, "main")

	h.mustRun(h.repoRoot, "init", "RD-19471", "add", "new", "toolbar", "--adopt-only")
	if len(h.entries()) != 1 {
		t.Error("--adopt-only did not register the pre-existing worktree")
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	h := newHarness(t)

	h.mustRun(h.repoRoot, "init", "RD-19471", "add", "new", "toolbar", "--dry-run")

	if _, err := os.Stat(h.registry); err == nil {
		t.Error("--dry-run wrote the registry")
	}
	if _, err := os.Stat(filepath.Join(h.repoRoot, ".worktrees")); err == nil {
		t.Error("--dry-run created the worktrees directory")
	}
	if _, err := os.Stat(h.assets); err == nil {
		t.Error("--dry-run created the assets directory")
	}
	exclude, err := os.ReadFile(filepath.Join(h.repoRoot, ".git", "info", "exclude"))
	if err == nil && strings.Contains(string(exclude), ".worktrees/") {
		t.Error("--dry-run edited .git/info/exclude")
	}
}

func TestDeleteTearsDownAndKeepsAssets(t *testing.T) {
	h := newHarness(t)
	h.mustRun(h.repoRoot, "init", "RD-19471", "add", "new", "toolbar")
	entry := h.entries()[0]

	_, stderr := h.mustRun(h.repoRoot, "delete", "RD-19471", "--yes")

	if len(h.entries()) != 0 {
		t.Error("the registry entry survived delete")
	}
	if _, err := os.Stat(entry.WorktreePath); err == nil {
		t.Error("the worktree survived delete")
	}
	if !strings.Contains(h.git(h.repoRoot, "branch", "--list"), "RD-19471") {
		// Deleted, as expected.
	} else {
		t.Error("the branch survived delete")
	}

	// Assets are never deleted, and the user is reminded where they are.
	if _, err := os.Stat(entry.AssetsPath); err != nil {
		t.Errorf("flow deleted the assets directory: %v", err)
	}
	if !strings.Contains(stderr, "assets kept") {
		t.Errorf("the reminder about kept assets is missing:\n%s", stderr)
	}
}

func TestListJSONPayload(t *testing.T) {
	h := newHarness(t)
	h.mustRun(h.repoRoot, "init", "RD-19471", "add", "new", "toolbar")

	stdout, _ := h.mustRun(h.repoRoot, "list", "--no-remote", "--json")

	var payload struct {
		Tickets []struct {
			Ticket string `json:"ticket"`
			Status string `json:"status"`
			PR     struct {
				State string `json:"state"`
			} `json:"pr"`
		} `json:"tickets"`
	}
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatalf("--json did not emit a single JSON object: %v\n%s", err, stdout)
	}
	if len(payload.Tickets) != 1 {
		t.Fatalf("want one ticket, got %d", len(payload.Tickets))
	}
	if payload.Tickets[0].Ticket != "RD-19471" {
		t.Errorf("ticket = %q", payload.Tickets[0].Ticket)
	}
	if payload.Tickets[0].Status != "clean" {
		t.Errorf("status = %q", payload.Tickets[0].Status)
	}
	// PR state must serialize by name, never as an integer.
	if payload.Tickets[0].PR.State != "unknown" {
		t.Errorf("pr state = %q, want unknown when GitHub was not consulted", payload.Tickets[0].PR.State)
	}
}

func TestJSONErrorsGoToStdoutWithANonZeroExit(t *testing.T) {
	h := newHarness(t)

	stdout, _, err := h.run(h.repoRoot, "path", "RD-99999", "--json")
	if err == nil {
		t.Fatal("an unknown ticket must fail")
	}
	if code := cli.ExitCodeFor(err); code != cli.ExitNotFound {
		t.Errorf("exit code = %d, want %d", code, cli.ExitNotFound)
	}

	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if jsonErr := json.Unmarshal([]byte(stdout), &envelope); jsonErr != nil {
		t.Fatalf("the error envelope was not on stdout: %v\n%s", jsonErr, stdout)
	}
	if envelope.Error.Code != "not_found" {
		t.Errorf("code = %q", envelope.Error.Code)
	}
}

func TestPruneAdoptsAndDrops(t *testing.T) {
	h := newHarness(t)
	h.mustRun(h.repoRoot, "init", "RD-19471", "add", "new", "toolbar")

	// An unregistered worktree that matches the naming pattern.
	orphan := filepath.Join(h.repoRoot, ".worktrees", "RD-19472-orphaned-work")
	h.git(h.repoRoot, "worktree", "add", "-b", "RD-19472-orphaned-work", orphan, "main")

	// And a registered one whose directory the user deleted by hand.
	gone := h.entries()[0].WorktreePath
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}

	h.mustRun(h.repoRoot, "prune", "--yes")

	entries := h.entries()
	if len(entries) != 1 {
		t.Fatalf("want exactly the adopted entry after prune, got %d: %+v", len(entries), entries)
	}
	if entries[0].TicketID != "RD-19472" {
		t.Errorf("prune kept %q; it should have dropped RD-19471 and adopted RD-19472",
			entries[0].TicketID)
	}
}

// TestPruneDryRunNeverWritesTheRegistry is the escape AGENTS.md warns about:
// prune's apply closures call Registry.Update directly rather than going
// through exec.Runner, so the dry-run wrapper never saw them. The --json
// branch applied before the --dry-run guard was even reached, so
// `flow prune --json --yes --dry-run` reconciled the registry for real.
func TestPruneDryRunNeverWritesTheRegistry(t *testing.T) {
	h := newHarness(t)
	h.mustRun(h.repoRoot, "init", "RD-19471", "add", "new", "toolbar")

	// Make there be something to prune: delete the worktree by hand.
	gone := h.entries()[0].WorktreePath
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	before := h.entries()
	if len(before) != 1 {
		t.Fatalf("setup: want one entry, got %d", len(before))
	}

	for _, args := range [][]string{
		{"prune", "--json", "--yes", "--dry-run"},
		{"prune", "--yes", "--dry-run"},
		{"prune", "--dry-run"},
	} {
		h.mustRun(h.repoRoot, args...)
		if got := h.entries(); len(got) != 1 {
			t.Fatalf("flow %s changed the registry: %d entries, want 1",
				strings.Join(args, " "), len(got))
		}
	}

	// Without --dry-run it still does the work.
	h.mustRun(h.repoRoot, "prune", "--json", "--yes")
	if got := h.entries(); len(got) != 0 {
		t.Errorf("prune --json --yes left %d entries, want 0", len(got))
	}
}

// TestDeleteAllOnGoneWorktrees pins the prompt load of the shape most likely to
// surprise: --all over worktrees the user already removed by hand. With --yes
// it must still complete without hanging on a prompt.
func TestDeleteAllOnGoneWorktrees(t *testing.T) {
	h := newHarness(t)
	h.mustRun(h.repoRoot, "init", "RD-19471", "add", "new", "toolbar")
	h.mustRun(h.repoRoot, "init", "RD-19472", "second", "ticket")

	for _, e := range h.entries() {
		if err := os.RemoveAll(e.WorktreePath); err != nil {
			t.Fatal(err)
		}
	}

	h.mustRun(h.repoRoot, "delete", "--all", "--yes")
	if got := h.entries(); len(got) != 0 {
		t.Errorf("delete --all --yes left %d entries: %+v", len(got), got)
	}
}

// TestDeleteAllJSONRequiresExplicitConsent is the --json contract: it never
// implies consent for a destructive action.
func TestDeleteAllJSONRequiresExplicitConsent(t *testing.T) {
	h := newHarness(t)
	h.mustRun(h.repoRoot, "init", "RD-19471", "add", "new", "toolbar")

	_, _, err := h.run(h.repoRoot, "delete", "--all", "--json")
	if err == nil {
		t.Fatal("delete --all --json without --yes must not proceed")
	}
	if code := cli.ExitCodeFor(err); code != cli.ExitPrecondition {
		t.Errorf("exit code = %d, want %d", code, cli.ExitPrecondition)
	}
	if got := h.entries(); len(got) != 1 {
		t.Errorf("the entry was deleted without consent: %+v", got)
	}
}

func TestVersionJSON(t *testing.T) {
	h := newHarness(t)
	stdout, _ := h.mustRun(h.repoRoot, "version", "--json")

	var payload map[string]string
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["version"] == "" {
		t.Error("version is empty")
	}
	if payload["version"] != cli.Version {
		t.Errorf("flow version reports %q but the package says %q — the two must agree",
			payload["version"], cli.Version)
	}
}

func TestConfigShowReflectsTheFile(t *testing.T) {
	h := newHarness(t)
	stdout, _ := h.mustRun(h.repoRoot, "config", "show", "--json")

	var cfg struct {
		Assets struct {
			Root string `json:"Root"`
		} `json:"Assets"`
		Workspace struct {
			Provider string `json:"Provider"`
		} `json:"Workspace"`
	}
	if err := json.Unmarshal([]byte(stdout), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Assets.Root != h.assets {
		t.Errorf("assets root = %q, want %q", cfg.Assets.Root, h.assets)
	}
	if cfg.Workspace.Provider != "none" {
		t.Errorf("workspace provider = %q", cfg.Workspace.Provider)
	}
}

func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return a == b
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		return a == b
	}
	return ra == rb
}

// linesContaining counts the lines mentioning want, which is what "how many
// worktrees" actually means: git prints the path and the branch on one line.
func linesContaining(body, want string) int {
	var n int
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, want) {
			n++
		}
	}
	return n
}

func countLines(body, want string) int {
	var n int
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == want {
			n++
		}
	}
	return n
}

// A worktree whose directory name does not match its branch is still the
// worktree for that branch. --adopt-only exists to register what is already
// there, so it must find this one rather than reporting nothing to adopt.
func TestAdoptOnlyAdoptsAWorktreeAtANonMatchingPath(t *testing.T) {
	h := newHarness(t)

	path := filepath.Join(h.repoRoot, ".worktrees", "totally-different-dir")
	h.git(h.repoRoot, "worktree", "add", "-b", "legacy-feature", path, "main")

	h.mustRun(h.repoRoot, "init", "RD-19471", "brand", "new", "description",
		"--branch", "legacy-feature", "--adopt-only", "--yes")

	entries := h.entries()
	if len(entries) != 1 {
		t.Fatalf("want the pre-existing worktree adopted, got %d entries", len(entries))
	}
	// macOS resolves the temp dir through /private, so compare the tail.
	if !strings.HasSuffix(entries[0].WorktreePath, filepath.Join(".worktrees", "totally-different-dir")) {
		t.Errorf("worktree = %q, want the existing %q", entries[0].WorktreePath, path)
	}
	if entries[0].Branch != "legacy-feature" {
		t.Errorf("branch = %q, want legacy-feature", entries[0].Branch)
	}
	// Adoption registers; it must never create a second worktree.
	if got := strings.Count(h.git(h.repoRoot, "worktree", "list"), ".worktrees"); got != 1 {
		t.Errorf("worktree list has %d entries under .worktrees, want 1:\n%s",
			got, h.git(h.repoRoot, "worktree", "list"))
	}
}
