package ticket_test

import (
	"strings"
	"testing"

	"github.com/idosaban-scaleops/flow/internal/ticket"
)

func TestNormalizeID(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"already normal", "RD-19471", "RD-19471", false},
		{"lowercase", "rd-19471", "RD-19471", false},
		{"mixed case", "Rd-19471", "RD-19471", false},
		{"surrounding space", "  rd-19471  ", "RD-19471", false},
		{"digits in key", "AB2-7", "AB2-7", false},
		{"empty", "", "", true},
		{"no number", "RD-", "", true},
		{"no key", "-19471", "", true},
		{"leading digit in key", "1RD-5", "", true},
		{"trailing text", "RD-19471-toolbar", "", true},
		{"underscore separator", "RD_19471", "", true},
		{"spaces inside", "RD 19471", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ticket.NormalizeID(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NormalizeID(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("NormalizeID(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSlugify(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"simple words", "add new toolbar", "add-new-toolbar", false},
		{"uppercase", "Add New Toolbar", "add-new-toolbar", false},
		{"underscores", "add_new_toolbar", "add-new-toolbar", false},
		{"slashes", "feat/add/toolbar", "feat-add-toolbar", false},
		{"dots", "fix.the.bug", "fix-the-bug", false},
		{"mixed separators", "add _ new / tool.bar", "add-new-tool-bar", false},
		{"punctuation dropped", "fix: the (broken) thing!", "fix-the-broken-thing", false},
		{"collapse hyphens", "a---b", "a-b", false},
		{"trim hyphens", "--a b--", "a-b", false},
		{"digits kept", "bump v2 to v3", "bump-v2-to-v3", false},
		{"non-ascii dropped", "café naïve", "caf-nave", false},
		{"empty", "", "", true},
		{"only punctuation", "!!! ???", "", true},
		{"only separators", "___ ///", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ticket.Slugify(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Slugify(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("Slugify(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSlugifyTruncates(t *testing.T) {
	long := strings.Repeat("abcde ", 30)
	got, err := ticket.Slugify(long)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > ticket.MaxSlugLen {
		t.Errorf("slug is %d chars, want at most %d", len(got), ticket.MaxSlugLen)
	}
	if strings.HasSuffix(got, "-") {
		t.Errorf("truncation left a trailing hyphen: %q", got)
	}
}

func TestSlugifyTruncationTrimsTrailingHyphen(t *testing.T) {
	// 60 chars of "a" then a separator, so the cut lands exactly on a hyphen.
	in := strings.Repeat("a", ticket.MaxSlugLen) + " tail"
	got, err := ticket.Slugify(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(got, "-") {
		t.Errorf("got %q, which ends in a hyphen", got)
	}
}

func TestExpandPreservesSeparatorConvention(t *testing.T) {
	tk := ticket.Ticket{ID: "RD-19471", Slug: "add-new-toolbar"}
	tests := []struct {
		name     string
		template string
		want     string
	}{
		{"branch uses a hyphen", "{id}-{slug}", "RD-19471-add-new-toolbar"},
		{"assets use an underscore", "{id}_{slug}", "RD-19471_add-new-toolbar"},
		{"workspace label is the ID", "{id}", "RD-19471"},
		{"literal text passes through", "wt/{id}", "wt/RD-19471"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tk.Expand(tt.template); got != tt.want {
				t.Errorf("Expand(%q) = %q, want %q", tt.template, got, tt.want)
			}
		})
	}
}

func TestParseDirName(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantID   string
		wantSlug string
		wantOK   bool
	}{
		{"standard", "RD-19471-add-new-toolbar", "RD-19471", "add-new-toolbar", true},
		{"single word slug", "RD-1-toolbar", "RD-1", "toolbar", true},
		{"slug containing a ticket-like run", "RD-1-fixes-RD-2-too", "RD-1", "fixes-RD-2-too", true},
		{"no slug", "RD-19471", "", "", false},
		{"lowercase key", "rd-19471-toolbar", "", "", false},
		{"not a ticket", "some-branch", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ticket.ParseDirName(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("ParseDirName(%q) ok = %v, want %v", tt.in, ok, tt.wantOK)
			}
			if ok && (got.ID != tt.wantID || got.Slug != tt.wantSlug) {
				t.Errorf("ParseDirName(%q) = %+v, want {%s %s}", tt.in, got, tt.wantID, tt.wantSlug)
			}
		})
	}
}

func TestIDFromBranch(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"RD-19471-add-new-toolbar", "RD-19471", true},
		{"feature/rd-123-thing", "RD-123", true},
		{"main", "", false},
		{"no-ticket-here", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, ok := ticket.IDFromBranch(tt.in)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("IDFromBranch(%q) = %q,%v want %q,%v", tt.in, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
