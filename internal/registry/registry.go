// Package registry remembers what flow created, so that open, delete, list and
// status can find it again. It is a cache of facts about the world, not the
// source of truth: every reader must tolerate drift — a worktree deleted by
// hand, a workspace closed, a branch renamed — and never crash because of it.
package registry

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Version is the on-disk schema version. An unknown version is a hard error
// rather than a silent partial read, so a newer flow's file is never truncated
// by an older one.
const Version = 1

// Workspace records the terminal workspace flow created for a ticket.
type Workspace struct {
	Provider   string `json:"provider,omitempty"`
	ID         string `json:"id,omitempty"`
	Label      string `json:"label,omitempty"`
	TabID      string `json:"tab_id,omitempty"`
	RootPaneID string `json:"root_pane_id,omitempty"`
}

// Empty reports whether no workspace is recorded.
func (w Workspace) Empty() bool { return w.ID == "" && w.Label == "" }

// Entry is everything flow knows about one ticket in one repository.
type Entry struct {
	TicketID     string    `json:"ticket_id"`
	Slug         string    `json:"slug"`
	RepoKey      string    `json:"repo_key"`
	RepoRoot     string    `json:"repo_root"`
	WorktreePath string    `json:"worktree_path"`
	Branch       string    `json:"branch"`
	BaseBranch   string    `json:"base_branch"`
	AssetsPath   string    `json:"assets_path"`
	Workspace    Workspace `json:"workspace"`
	CreatedAt    time.Time `json:"created_at"`
	LastOpenedAt time.Time `json:"last_opened_at"`

	// Extra preserves fields written by a future version of flow so that an
	// older binary rewriting the file does not discard them.
	Extra map[string]json.RawMessage `json:"-"`
}

// Key identifies an entry. The same ticket ID may exist in two repositories,
// so the repo key is part of the identity.
type Key struct {
	RepoKey  string
	TicketID string
}

// Key returns the entry's identity.
func (e Entry) Key() Key { return Key{RepoKey: e.RepoKey, TicketID: e.TicketID} }

// File is the whole registry document.
type File struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

// Find returns the entry for a repo and ticket.
func (f *File) Find(repoKey, ticketID string) (Entry, bool) {
	for _, e := range f.Entries {
		if e.RepoKey == repoKey && strings.EqualFold(e.TicketID, ticketID) {
			return e, true
		}
	}
	return Entry{}, false
}

// FindAnyRepo returns the entry for a ticket in any repo, and reports whether
// the match was ambiguous across repositories.
func (f *File) FindAnyRepo(ticketID string) (Entry, bool, bool) {
	var found []Entry
	for _, e := range f.Entries {
		if strings.EqualFold(e.TicketID, ticketID) {
			found = append(found, e)
		}
	}
	switch len(found) {
	case 0:
		return Entry{}, false, false
	case 1:
		return found[0], true, false
	default:
		return found[0], true, true
	}
}

// ForRepo returns every entry for one repository, newest first.
func (f *File) ForRepo(repoKey string) []Entry {
	var out []Entry
	for _, e := range f.Entries {
		if e.RepoKey == repoKey {
			out = append(out, e)
		}
	}
	sortEntries(out)
	return out
}

// All returns every entry, newest first.
func (f *File) All() []Entry {
	out := append([]Entry(nil), f.Entries...)
	sortEntries(out)
	return out
}

func sortEntries(es []Entry) {
	sort.SliceStable(es, func(i, j int) bool {
		if es[i].CreatedAt.Equal(es[j].CreatedAt) {
			return es[i].TicketID < es[j].TicketID
		}
		return es[i].CreatedAt.After(es[j].CreatedAt)
	})
}

// Upsert inserts or replaces an entry, preserving the original CreatedAt when
// one already existed.
func (f *File) Upsert(e Entry) {
	for i, existing := range f.Entries {
		if existing.Key() == e.Key() {
			if e.CreatedAt.IsZero() {
				e.CreatedAt = existing.CreatedAt
			}
			if e.Extra == nil {
				e.Extra = existing.Extra
			}
			f.Entries[i] = e
			return
		}
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	f.Entries = append(f.Entries, e)
}

// Remove deletes an entry and reports whether it was present.
func (f *File) Remove(repoKey, ticketID string) bool {
	for i, e := range f.Entries {
		if e.RepoKey == repoKey && strings.EqualFold(e.TicketID, ticketID) {
			f.Entries = append(f.Entries[:i], f.Entries[i+1:]...)
			return true
		}
	}
	return false
}

// MatchByPath finds the entry whose worktree path is dir or an ancestor of it.
// Matching is on path-segment boundaries, so /a/b never matches /a/bc.
func (f *File) MatchByPath(dir string) (Entry, bool, bool) {
	dir = filepath.Clean(dir)
	var best Entry
	var bestLen int
	var matches int
	for _, e := range f.Entries {
		if e.WorktreePath == "" || !underPath(dir, e.WorktreePath) {
			continue
		}
		matches++
		if len(e.WorktreePath) > bestLen {
			best, bestLen = e, len(e.WorktreePath)
		}
	}
	return best, matches > 0, matches > 1
}

func underPath(dir, root string) bool {
	root = filepath.Clean(root)
	if dir == root {
		return true
	}
	return strings.HasPrefix(dir, root+string(filepath.Separator))
}

// UnmarshalJSON decodes an entry while retaining any fields this version of
// flow does not know about.
func (e *Entry) UnmarshalJSON(data []byte) error {
	type alias Entry
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*e = Entry(a)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for _, known := range knownFields {
		delete(raw, known)
	}
	if len(raw) > 0 {
		e.Extra = raw
	}
	return nil
}

// MarshalJSON writes the known fields plus anything preserved from a newer
// schema.
func (e Entry) MarshalJSON() ([]byte, error) {
	type alias Entry
	data, err := json.Marshal(alias(e))
	if err != nil {
		return nil, err
	}
	if len(e.Extra) == 0 {
		return data, nil
	}

	var merged map[string]json.RawMessage
	if err := json.Unmarshal(data, &merged); err != nil {
		return nil, err
	}
	for k, v := range e.Extra {
		if _, clash := merged[k]; !clash {
			merged[k] = v
		}
	}
	return json.Marshal(merged)
}

var knownFields = []string{
	"ticket_id", "slug", "repo_key", "repo_root", "worktree_path", "branch",
	"base_branch", "assets_path", "workspace", "created_at", "last_opened_at",
}

// ErrUnknownVersion reports a registry written by a newer flow.
type ErrUnknownVersion struct {
	Path string
	Got  int
}

func (e *ErrUnknownVersion) Error() string {
	return fmt.Sprintf("registry %s has version %d, but this flow understands version %d; "+
		"upgrade flow rather than letting it rewrite the file", e.Path, e.Got, Version)
}
