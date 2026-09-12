package gitx_test

import (
	"testing"

	"github.com/idosaban-scaleops/flow/internal/gitx"
)

func TestParseGitHubRemote(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantOwner string
		wantName  string
		wantOK    bool
	}{
		{"scp with .git", "git@github.com:scaleops-sh/scaleops.git", "scaleops-sh", "scaleops", true},
		{"scp without .git", "git@github.com:scaleops-sh/scaleops", "scaleops-sh", "scaleops", true},
		{"ssh url with .git", "ssh://git@github.com/scaleops-sh/scaleops.git", "scaleops-sh", "scaleops", true},
		{"ssh url without .git", "ssh://git@github.com/scaleops-sh/scaleops", "scaleops-sh", "scaleops", true},
		{"https with .git", "https://github.com/scaleops-sh/scaleops.git", "scaleops-sh", "scaleops", true},
		{"https without .git", "https://github.com/scaleops-sh/scaleops", "scaleops-sh", "scaleops", true},
		{"https with trailing slash", "https://github.com/scaleops-sh/scaleops/", "scaleops-sh", "scaleops", true},
		{"https with credentials", "https://user:tok@github.com/o/r.git", "o", "r", true},
		{"ssh url with port", "ssh://git@github.com:22/o/r.git", "o", "r", true},
		{"surrounding whitespace", "  git@github.com:o/r.git\n", "o", "r", true},
		{"repo name containing dots", "git@github.com:o/my.repo.git", "o", "my.repo", true},

		{"gitlab is not github", "git@gitlab.com:o/r.git", "", "", false},
		{"self-hosted enterprise host", "git@github.example.com:o/r.git", "", "", false},
		{"local path", "/Users/ido/Developer/scaleops", "", "", false},
		{"empty", "", "", "", false},
		{"too few path segments", "https://github.com/onlyowner", "", "", false},
		{"too many path segments", "https://github.com/a/b/c", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner, name, ok := gitx.ParseGitHubRemote(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("ParseGitHubRemote(%q) ok = %v, want %v", tt.in, ok, tt.wantOK)
			}
			if owner != tt.wantOwner || name != tt.wantName {
				t.Errorf("ParseGitHubRemote(%q) = %q/%q, want %q/%q",
					tt.in, owner, name, tt.wantOwner, tt.wantName)
			}
		})
	}
}
