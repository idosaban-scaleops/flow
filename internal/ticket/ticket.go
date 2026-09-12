// Package ticket turns what a user types into the exact names flow uses on
// disk. The naming rules here are load-bearing: the branch and worktree use a
// hyphen between ID and slug, the assets directory uses an underscore, and both
// must be reproduced identically on every later invocation.
package ticket

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxSlugLen bounds the slug so that worktree paths stay manageable.
const MaxSlugLen = 60

var (
	idPattern     = regexp.MustCompile(`^[A-Z][A-Z0-9]*-\d+$`)
	dirPattern    = regexp.MustCompile(`^([A-Z][A-Z0-9]*-\d+)-(.+)$`)
	separatorRun  = regexp.MustCompile(`[\s_/.]+`)
	notSlugChar   = regexp.MustCompile(`[^a-z0-9-]`)
	hyphenRun     = regexp.MustCompile(`-{2,}`)
	looseIDSearch = regexp.MustCompile(`(?i)\b([a-z][a-z0-9]*-\d+)\b`)
)

// Ticket is a normalized ticket identity.
type Ticket struct {
	ID   string
	Slug string
}

// ErrInvalid reports input flow cannot turn into a ticket. Callers map it to
// exit code 2.
type ErrInvalid struct {
	What  string
	Input string
	Why   string
}

func (e *ErrInvalid) Error() string {
	return fmt.Sprintf("invalid %s %q: %s", e.What, e.Input, e.Why)
}

// NormalizeID uppercases and validates a ticket ID: "rd-19471" becomes
// "RD-19471", and anything that is not PROJECT-NUMBER is rejected.
func NormalizeID(raw string) (string, error) {
	id := strings.ToUpper(strings.TrimSpace(raw))
	if id == "" {
		return "", &ErrInvalid{What: "ticket ID", Input: raw, Why: "it is empty"}
	}
	if !idPattern.MatchString(id) {
		return "", &ErrInvalid{
			What:  "ticket ID",
			Input: raw,
			Why:   "expected a project key and a number, like RD-19471",
		}
	}
	return id, nil
}

// Slugify normalizes a free-text description into the slug used in branch,
// worktree and assets names.
func Slugify(raw string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = separatorRun.ReplaceAllString(s, "-")
	s = notSlugChar.ReplaceAllString(s, "")
	s = hyphenRun.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")

	if len(s) > MaxSlugLen {
		s = s[:MaxSlugLen]
	}
	s = strings.Trim(s, "-")

	if s == "" {
		return "", &ErrInvalid{
			What:  "description",
			Input: raw,
			Why:   "it contains no letters or digits to build a slug from",
		}
	}
	return s, nil
}

// New normalizes an ID and a description into a Ticket.
func New(id, description string) (Ticket, error) {
	normID, err := NormalizeID(id)
	if err != nil {
		return Ticket{}, err
	}
	slug, err := Slugify(description)
	if err != nil {
		return Ticket{}, err
	}
	return Ticket{ID: normID, Slug: slug}, nil
}

// Expand substitutes {id} and {slug} in a naming template.
func (t Ticket) Expand(template string) string {
	r := strings.NewReplacer("{id}", t.ID, "{slug}", t.Slug)
	return r.Replace(template)
}

// ParseDirName recovers a ticket from a worktree directory name such as
// "RD-19471-add-new-toolbar".
func ParseDirName(name string) (Ticket, bool) {
	m := dirPattern.FindStringSubmatch(name)
	if m == nil {
		return Ticket{}, false
	}
	return Ticket{ID: m[1], Slug: m[2]}, true
}

// LooksLikeID reports whether s could be a ticket ID, so that commands taking
// an optional [ticket] argument can tell one from a flag value.
func LooksLikeID(s string) bool {
	_, err := NormalizeID(s)
	return err == nil
}

// IDFromBranch extracts a ticket ID from a branch name, for branches flow did
// not create itself.
func IDFromBranch(branch string) (string, bool) {
	m := looseIDSearch.FindStringSubmatch(branch)
	if m == nil {
		return "", false
	}
	id, err := NormalizeID(m[1])
	if err != nil {
		return "", false
	}
	return id, true
}
