// Package chartver resolves the Helm chart version CI produced for a branch.
//
// The CI computes it as "{nextTag}-alpha-{branch}-{run_id}" for feature
// branches and "{nextTag}-rc-main-{run_id}" for main, where nextTag is the most
// recent git tag with its third component bumped. The public REST API does not
// expose the step summary that records it, so it is recovered here instead.
package chartver

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Strategy names how a version was resolved, so -v and --json can report it.
type Strategy string

// Resolution strategies, in the order they are attempted.
const (
	// StrategyExplicit means --version short-circuited everything.
	StrategyExplicit Strategy = "explicit"
	// StrategyIndex means the version was read from the Helm repository index.
	StrategyIndex Strategy = "helm-index"
	// StrategyRun means a CI run pinned the run ID and the index confirmed it.
	StrategyRun Strategy = "ci-run"
	// StrategyDerived means the version was computed locally and not verified
	// against the chart repository.
	StrategyDerived Strategy = "derived"
)

// Resolution is a resolved chart version and how it was arrived at.
type Resolution struct {
	Version  string   `json:"version"`
	Strategy Strategy `json:"strategy"`
	RunID    int64    `json:"run_id,omitempty"`
	RunURL   string   `json:"run_url,omitempty"`
	// Verified reports that the version was seen in the chart index, rather
	// than computed from local tags and hope.
	Verified bool `json:"verified"`
}

// BranchTag renders a branch name the way CI does, replacing slashes with
// dashes (`github.ref_name | tr '/' '-'`).
func BranchTag(branch string) string { return strings.ReplaceAll(branch, "/", "-") }

// Infix is the marker that identifies a branch's chart versions: "-alpha-{branch}-"
// for a feature branch, "-rc-main-" for main.
func Infix(branch string) string {
	if branch == "main" {
		return "-rc-main-"
	}
	return "-alpha-" + BranchTag(branch) + "-"
}

// runIDPattern captures the trailing numeric run ID.
var runIDPattern = regexp.MustCompile(`-(\d+)$`)

// RunIDOf extracts the trailing run ID from a chart version.
func RunIDOf(version string) (int64, bool) {
	m := runIDPattern.FindStringSubmatch(version)
	if m == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// Match reports whether a chart version belongs to a branch.
func Match(version, branch string) bool {
	return strings.Contains(version, Infix(branch))
}

// Newest picks the version with the highest run ID.
//
// Ordering is by run ID, never by semver: these versions carry a pre-release
// suffix containing the branch name, and semver ordering compares that
// alphanumerically, which puts "…-9" after "…-10".
func Newest(versions []string, branch string) (version string, runID int64, found bool) {
	type candidate struct {
		version string
		runID   int64
	}

	var candidates []candidate
	for _, v := range versions {
		if !Match(v, branch) {
			continue
		}
		id, parsed := RunIDOf(v)
		if !parsed {
			continue
		}
		candidates = append(candidates, candidate{v, id})
	}
	if len(candidates) == 0 {
		return "", 0, false
	}

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].runID > candidates[j].runID })
	return candidates[0].version, candidates[0].runID, true
}

// NextTag bumps the third dot-separated component of a git tag, mirroring the
// CI's `awk -F"." '{printf "%s.%s.%s", $1, $2, ++$3}'`.
func NextTag(latestTag string) (string, error) {
	parts := strings.Split(strings.TrimSpace(latestTag), ".")
	if len(parts) < 3 {
		return "", fmt.Errorf("tag %q does not have three dot-separated components", latestTag)
	}

	patch, err := strconv.Atoi(parts[2])
	if err != nil {
		return "", fmt.Errorf("tag %q has a non-numeric patch component %q", latestTag, parts[2])
	}
	parts[2] = strconv.Itoa(patch + 1)
	return strings.Join(parts[:3], "."), nil
}

// Derive assembles the version CI would have produced, without consulting the
// chart index. The caller must mark the result unverified.
func Derive(latestTag, branch string, runID int64) (string, error) {
	next, err := NextTag(latestTag)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%s%d", next, Infix(branch), runID), nil
}

// Index is the chart-repository surface the resolver needs.
type Index interface {
	UpdateRepo(ctx context.Context, repo string) error
	SearchVersions(ctx context.Context, repo, chart string) ([]VersionEntry, error)
}

// VersionEntry is one published chart version.
type VersionEntry struct {
	Version string
}

// Search lists the versions published for a branch, newest first.
func Search(ctx context.Context, idx Index, helmRepo, chart, branch string, update bool) (version string, runID int64, found bool, err error) {
	if update {
		if err := idx.UpdateRepo(ctx, helmRepo); err != nil {
			return "", 0, false, fmt.Errorf("helm repo update %s: %w", helmRepo, err)
		}
	}

	entries, err := idx.SearchVersions(ctx, helmRepo, chart)
	if err != nil {
		return "", 0, false, err
	}

	versions := make([]string, 0, len(entries))
	for _, e := range entries {
		versions = append(versions, e.Version)
	}
	version, runID, ok := Newest(versions, branch)
	return version, runID, ok, nil
}
