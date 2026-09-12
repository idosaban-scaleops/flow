package registry_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/idosaban-scaleops/flow/internal/registry"
)

func entry(id, slug string) registry.Entry {
	return registry.Entry{
		TicketID:     id,
		Slug:         slug,
		RepoKey:      "scaleops-sh/scaleops",
		RepoRoot:     "/repo",
		WorktreePath: "/repo/.worktrees/" + id + "-" + slug,
		Branch:       id + "-" + slug,
		BaseBranch:   "main",
		CreatedAt:    time.Date(2026, 9, 12, 9, 14, 22, 0, time.UTC),
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	s := registry.Open(filepath.Join(t.TempDir(), "registry.json"))
	f, err := s.Load()
	if err != nil {
		t.Fatalf("a first run must not fail: %v", err)
	}
	if len(f.Entries) != 0 {
		t.Errorf("want no entries, got %d", len(f.Entries))
	}
}

func TestUpsertRoundTrip(t *testing.T) {
	s := registry.Open(filepath.Join(t.TempDir(), "registry.json"))
	if err := s.Update(func(f *registry.File) error {
		f.Upsert(entry("RD-19471", "add-new-toolbar"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	f, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := f.Find("scaleops-sh/scaleops", "RD-19471")
	if !ok {
		t.Fatal("entry not found after write")
	}
	if got.Slug != "add-new-toolbar" {
		t.Errorf("slug = %q", got.Slug)
	}
}

func TestUpsertPreservesCreatedAt(t *testing.T) {
	f := &registry.File{Version: registry.Version}
	original := entry("RD-1", "a")
	f.Upsert(original)

	updated := original
	updated.Slug = "b"
	updated.CreatedAt = time.Time{}
	f.Upsert(updated)

	if len(f.Entries) != 1 {
		t.Fatalf("want one entry, got %d", len(f.Entries))
	}
	if !f.Entries[0].CreatedAt.Equal(original.CreatedAt) {
		t.Errorf("created_at = %v, want %v", f.Entries[0].CreatedAt, original.CreatedAt)
	}
	if f.Entries[0].Slug != "b" {
		t.Errorf("slug was not updated: %q", f.Entries[0].Slug)
	}
}

func TestUpsertMatchesTicketIDCaseInsensitively(t *testing.T) {
	// Find and Remove have always compared ticket IDs with EqualFold. Upsert
	// used to compare exactly, so an "RD-1" upsert against a stored "rd-1"
	// appended a duplicate instead of updating it.
	f := &registry.File{Version: registry.Version}
	f.Upsert(entry("rd-1", "lower"))

	updated := entry("RD-1", "upper")
	updated.CreatedAt = time.Time{}
	f.Upsert(updated)

	if len(f.Entries) != 1 {
		t.Fatalf("want one entry, got %d: %+v", len(f.Entries), f.Entries)
	}
	if f.Entries[0].Slug != "upper" {
		t.Errorf("slug = %q, want the upserted value", f.Entries[0].Slug)
	}
}

func TestSameTicketInTwoRepos(t *testing.T) {
	f := &registry.File{Version: registry.Version}
	a := entry("RD-1", "a")
	b := entry("RD-1", "b")
	b.RepoKey = "other/repo"
	f.Upsert(a)
	f.Upsert(b)

	if len(f.Entries) != 2 {
		t.Fatalf("the repo key is part of the identity: want 2 entries, got %d", len(f.Entries))
	}
	_, ok, ambiguous := f.FindAnyRepo("RD-1")
	if !ok || !ambiguous {
		t.Errorf("FindAnyRepo should report the cross-repo ambiguity (ok=%v ambiguous=%v)", ok, ambiguous)
	}
}

func TestMatchByPathUsesSegmentBoundaries(t *testing.T) {
	f := &registry.File{Version: registry.Version}
	f.Upsert(entry("RD-1", "toolbar"))

	tests := []struct {
		name string
		dir  string
		want bool
	}{
		{"exact", "/repo/.worktrees/RD-1-toolbar", true},
		{"child", "/repo/.worktrees/RD-1-toolbar/pkg/api", true},
		{"sibling with shared prefix", "/repo/.worktrees/RD-1-toolbar-extra", false},
		{"parent", "/repo/.worktrees", false},
		{"unrelated", "/elsewhere", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok, _ := f.MatchByPath(tt.dir)
			if ok != tt.want {
				t.Errorf("MatchByPath(%q) = %v, want %v", tt.dir, ok, tt.want)
			}
		})
	}
}

func TestMatchByPathPrefersDeepest(t *testing.T) {
	f := &registry.File{Version: registry.Version}
	outer := entry("RD-1", "a")
	outer.WorktreePath = "/repo/.worktrees/RD-1-a"
	inner := entry("RD-2", "b")
	inner.WorktreePath = "/repo/.worktrees/RD-1-a/nested"
	f.Upsert(outer)
	f.Upsert(inner)

	got, ok, _ := f.MatchByPath("/repo/.worktrees/RD-1-a/nested/pkg")
	if !ok || got.TicketID != "RD-2" {
		t.Errorf("want the deepest match RD-2, got %q (ok=%v)", got.TicketID, ok)
	}
}

func TestUnknownVersionIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	if err := os.WriteFile(path, []byte(`{"version":99,"entries":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Open(path).Load(); err == nil {
		t.Fatal("an unknown registry version must fail loudly, not silently drop data")
	}
}

func TestUnknownFieldsSurviveRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	body := `{"version":1,"entries":[{"ticket_id":"RD-1","slug":"a","repo_key":"r","future_field":{"x":1}}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	s := registry.Open(path)
	if err := s.Update(func(f *registry.File) error {
		f.Entries[0].Slug = "b"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Entries []map[string]json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Entries[0]["future_field"]; !ok {
		t.Error("a field written by a newer flow was dropped on rewrite")
	}
}

func TestConcurrentUpdatesDoNotLoseEntries(t *testing.T) {
	s := registry.Open(filepath.Join(t.TempDir(), "registry.json"))

	const n = 8
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.Update(func(f *registry.File) error {
				f.Upsert(entry("RD-"+string(rune('1'+i)), "slug"))
				return nil
			})
		}()
	}
	wg.Wait()

	f, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Entries) != n {
		t.Errorf("want %d entries after concurrent writes, got %d", n, len(f.Entries))
	}
}

func TestRemove(t *testing.T) {
	f := &registry.File{Version: registry.Version}
	f.Upsert(entry("RD-1", "a"))
	if !f.Remove("scaleops-sh/scaleops", "rd-1") {
		t.Error("Remove must be case-insensitive on the ticket ID")
	}
	if f.Remove("scaleops-sh/scaleops", "RD-1") {
		t.Error("removing twice must report false")
	}
}
