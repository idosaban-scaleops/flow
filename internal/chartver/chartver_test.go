package chartver_test

import (
	"testing"

	"github.com/idosaban-scaleops/flow/internal/chartver"
)

func TestInfix(t *testing.T) {
	tests := []struct {
		branch string
		want   string
	}{
		{"RD-19472-original-req-in-graphs", "-alpha-RD-19472-original-req-in-graphs-"},
		{"main", "-rc-main-"},
		{"feature/nested/name", "-alpha-feature-nested-name-"},
	}
	for _, tt := range tests {
		t.Run(tt.branch, func(t *testing.T) {
			if got := chartver.Infix(tt.branch); got != tt.want {
				t.Errorf("Infix(%q) = %q, want %q", tt.branch, got, tt.want)
			}
		})
	}
}

func TestRunIDOf(t *testing.T) {
	tests := []struct {
		version string
		want    int64
		wantOK  bool
	}{
		{"v1.0.199-alpha-RD-19472-original-req-in-graphs-34468169597", 34468169597, true},
		{"v1.0.200-rc-main-34468169598", 34468169598, true},
		{"v1.0.199", 0, false},
		{"v1.0.199-alpha-RD-1-a-", 0, false},
		{"", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			got, ok := chartver.RunIDOf(tt.version)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("RunIDOf(%q) = %d,%v want %d,%v", tt.version, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// TestNewestOrdersByRunIDNotSemver is the point of the whole ordering rule:
// semver compares the pre-release suffix alphanumerically, which sorts run
// 9999999999 above run 34468169597.
func TestNewestOrdersByRunIDNotSemver(t *testing.T) {
	branch := "RD-19472-original-req-in-graphs"
	versions := []string{
		"v1.0.199-alpha-RD-19472-original-req-in-graphs-9999999999",
		"v1.0.199-alpha-RD-19472-original-req-in-graphs-34468169597",
		"v1.0.199-alpha-RD-19472-original-req-in-graphs-34468101010",
	}

	got, runID, ok := chartver.Newest(versions, branch)
	if !ok {
		t.Fatal("expected a match")
	}
	if runID != 34468169597 {
		t.Errorf("run ID = %d, want the numerically highest 34468169597", runID)
	}
	if got != "v1.0.199-alpha-RD-19472-original-req-in-graphs-34468169597" {
		t.Errorf("version = %q", got)
	}
}

func TestNewestFiltersOtherBranches(t *testing.T) {
	versions := []string{
		"v1.0.199-alpha-RD-99999-other-branch-99999999999",
		"v1.0.199-alpha-RD-1-a-100",
		"v1.0.199-rc-main-200",
		"v1.0.198",
	}

	got, runID, ok := chartver.Newest(versions, "RD-1-a")
	if !ok || runID != 100 || got != "v1.0.199-alpha-RD-1-a-100" {
		t.Errorf("Newest = %q,%d,%v", got, runID, ok)
	}

	got, runID, ok = chartver.Newest(versions, "main")
	if !ok || runID != 200 || got != "v1.0.199-rc-main-200" {
		t.Errorf("main resolution = %q,%d,%v", got, runID, ok)
	}
}

func TestNewestPrefixCollision(t *testing.T) {
	// RD-1-a must not match a version built for RD-1-abc.
	versions := []string{"v1.0.1-alpha-RD-1-abc-500"}
	if _, _, ok := chartver.Newest(versions, "RD-1-a"); ok {
		t.Error("RD-1-a matched a chart built for RD-1-abc")
	}
}

// TestNewestRejectsSiblingBranch is the anchoring rule. Infix("feat/x") is
// "-alpha-feat-x-", which is a plain substring of a chart built for the
// sibling branch "feat/x-2". Ordering by run ID made the sibling's newer build
// the more likely pick, so `flow cluster upgrade` on feat/x installed
// feat/x-2's chart.
func TestNewestRejectsSiblingBranch(t *testing.T) {
	if chartver.Match("1.2.4-alpha-feat-x-2-999", "feat/x") {
		t.Error("feat/x matched a chart built for feat/x-2")
	}
	if !chartver.Match("1.2.4-alpha-feat-x-999", "feat/x") {
		t.Error("feat/x must still match its own chart")
	}

	versions := []string{
		"1.2.4-alpha-feat-x-2-999", // sibling branch, higher run ID
		"1.2.4-alpha-feat-x-100",   // this branch
	}
	got, runID, ok := chartver.Newest(versions, "feat/x")
	if !ok || got != "1.2.4-alpha-feat-x-100" || runID != 100 {
		t.Errorf("Newest = %q,%d,%v; want this branch's own chart", got, runID, ok)
	}

	// The sibling still resolves its own chart correctly.
	got, runID, ok = chartver.Newest(versions, "feat/x-2")
	if !ok || got != "1.2.4-alpha-feat-x-2-999" || runID != 999 {
		t.Errorf("sibling resolution = %q,%d,%v", got, runID, ok)
	}
}

func TestNewestNoMatch(t *testing.T) {
	if _, _, ok := chartver.Newest([]string{"v1.0.198", "v1.0.199"}, "RD-1-a"); ok {
		t.Error("expected no match among release versions")
	}
}

func TestNextTag(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"v1.0.199", "v1.0.200", false},
		{"v1.0.9", "v1.0.10", false},
		{"v2.14.0", "v2.14.1", false},
		{"  v1.0.199  ", "v1.0.200", false},
		{"v1.0.199-rc1", "", true},
		{"v1.0", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := chartver.NextTag(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NextTag(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("NextTag(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDeriveMatchesTheCIFormula(t *testing.T) {
	got, err := chartver.Derive("v1.0.199", "RD-19472-original-req-in-graphs", 34468169597)
	if err != nil {
		t.Fatal(err)
	}
	want := "v1.0.200-alpha-RD-19472-original-req-in-graphs-34468169597"
	if got != want {
		t.Errorf("Derive = %q, want %q", got, want)
	}

	got, err = chartver.Derive("v1.0.199", "main", 34468169598)
	if err != nil {
		t.Fatal(err)
	}
	if want := "v1.0.200-rc-main-34468169598"; got != want {
		t.Errorf("Derive(main) = %q, want %q", got, want)
	}
}

func TestDeriveRoundTripsThroughNewest(t *testing.T) {
	// Whatever Derive produces must be recognizable by the index matcher, or
	// the two strategies would disagree about the same build.
	branch := "feature/some/thing"
	version, err := chartver.Derive("v1.0.199", branch, 42)
	if err != nil {
		t.Fatal(err)
	}
	got, runID, ok := chartver.Newest([]string{version}, branch)
	if !ok || runID != 42 || got != version {
		t.Errorf("derived version %q did not round-trip: %q,%d,%v", version, got, runID, ok)
	}
}
