// Package helmx wraps the helm CLI: chart index queries, upgrades, and the
// release introspection that `flow cluster status` and `history` render.
package helmx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	flowexec "github.com/idosaban-scaleops/flow/internal/exec"
)

// Helm wraps the helm binary.
type Helm struct {
	Runner flowexec.Runner
	Bin    string
}

// New returns a Helm for the helm binary.
func New(r flowexec.Runner) *Helm { return &Helm{Runner: r, Bin: "helm"} }

// Available reports whether helm is on PATH.
func (h *Helm) Available() bool {
	_, err := h.Runner.LookPath(h.Bin)
	return err == nil
}

func (h *Helm) run(ctx context.Context, args ...string) (string, error) {
	res, err := h.Runner.Run(ctx, flowexec.Opts{Name: h.Bin, Args: args})
	if err != nil {
		return "", err
	}
	return res.Stdout, nil
}

// Version returns helm's version string.
func (h *Helm) Version(ctx context.Context) (string, error) {
	out, err := h.run(ctx, "version", "--short")
	return strings.TrimSpace(out), err
}

// Repo is one entry from `helm repo list`.
type Repo struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// ListRepos returns the configured chart repositories.
func (h *Helm) ListRepos(ctx context.Context) ([]Repo, error) {
	out, err := h.run(ctx, "repo", "list", "-o", "json")
	if err != nil {
		// helm exits non-zero when no repositories are configured, which is a
		// legitimate empty state rather than a failure.
		if strings.Contains(err.Error(), "no repositories") {
			return nil, nil
		}
		return nil, err
	}
	var repos []Repo
	if err := json.Unmarshal([]byte(out), &repos); err != nil {
		return nil, fmt.Errorf("parse helm repo list: %w", err)
	}
	return repos, nil
}

// UpdateRepo refreshes one chart repository's index.
func (h *Helm) UpdateRepo(ctx context.Context, repo string) error {
	args := []string{"repo", "update"}
	if repo != "" {
		args = append(args, repo)
	}
	_, err := h.Runner.Run(ctx, flowexec.Opts{Name: h.Bin, Args: args})
	return err
}

// ChartVersion is one row from `helm search repo --versions`.
type ChartVersion struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	AppVersion  string `json:"app_version"`
	Description string `json:"description"`
}

// SearchVersions lists every published version of a chart, including
// pre-releases: the alpha chart versions CI publishes are all pre-releases, so
// --devel is mandatory here.
func (h *Helm) SearchVersions(ctx context.Context, repo, chart string) ([]ChartVersion, error) {
	out, err := h.run(ctx, "search", "repo", repo+"/"+chart, "--versions", "--devel", "-o", "json")
	if err != nil {
		return nil, err
	}
	var versions []ChartVersion
	if err := json.Unmarshal([]byte(out), &versions); err != nil {
		return nil, fmt.Errorf("parse helm search output: %w", err)
	}
	return versions, nil
}

// HasVersion reports whether a specific version is present in the index.
func (h *Helm) HasVersion(ctx context.Context, repo, chart, version string) (bool, error) {
	versions, err := h.SearchVersions(ctx, repo, chart)
	if err != nil {
		return false, err
	}
	for _, v := range versions {
		if v.Version == version {
			return true, nil
		}
	}
	return false, nil
}

// UpgradeOptions describes a `helm upgrade` invocation.
type UpgradeOptions struct {
	Release     string
	Chart       string
	Namespace   string
	Version     string
	KubeContext string
	ValuesFiles []string
	SetValues   []string
	ExtraArgs   []string
	Install     bool
	Wait        bool
	Atomic      bool
	CreateNS    bool
	Timeout     string
	DryRun      bool
}

// UpgradeArgs renders the full argv. It is exported so the command can print
// the exact invocation before running it — the printed line and the executed
// command can then never disagree.
func (o UpgradeOptions) UpgradeArgs() []string {
	args := []string{"upgrade"}
	if o.Install {
		args = append(args, "--install")
	}
	args = append(args, o.Release, o.Chart)

	if o.Namespace != "" {
		args = append(args, "--namespace", o.Namespace)
	}
	if o.KubeContext != "" {
		args = append(args, "--kube-context", o.KubeContext)
	}
	args = append(args, o.ExtraArgs...)
	for _, f := range o.ValuesFiles {
		args = append(args, "--values", f)
	}
	for _, s := range o.SetValues {
		args = append(args, "--set", s)
	}
	if o.Version != "" {
		args = append(args, "--version", o.Version)
	}
	if o.CreateNS {
		args = append(args, "--create-namespace")
	}
	if o.Wait {
		args = append(args, "--wait")
	}
	if o.Atomic {
		args = append(args, "--atomic")
	}
	if o.Timeout != "" {
		args = append(args, "--timeout", o.Timeout)
	}
	if o.DryRun {
		args = append(args, "--dry-run")
	}
	return args
}

// CommandLine renders the upgrade as a copy-pasteable shell command.
func (o UpgradeOptions) CommandLine(bin string) string {
	if bin == "" {
		bin = "helm"
	}
	return flowexec.ShellJoin(append([]string{bin}, o.UpgradeArgs()...))
}

// Upgrade runs helm upgrade, streaming helm's output straight to the terminal
// so the user sees progress and errors as helm produces them.
func (h *Helm) Upgrade(ctx context.Context, o UpgradeOptions) error {
	_, err := h.Runner.Run(ctx, flowexec.Opts{
		Name: h.Bin, Args: o.UpgradeArgs(), Stream: true,
	})
	return err
}

// Release is the deployed release's identity and state.
type Release struct {
	Name         string    `json:"name"`
	Namespace    string    `json:"namespace"`
	Revision     int       `json:"revision"`
	Status       string    `json:"status"`
	LastDeployed time.Time `json:"last_deployed"`
	ChartName    string    `json:"chart_name"`
	ChartVersion string    `json:"chart_version"`
	AppVersion   string    `json:"app_version"`
}

// Status returns the deployed release's summary.
//
// It reads `helm get metadata`, not `helm status`. Status once carried the
// chart under a nested chart.metadata object, but that was helm 3: helm 4 emits
// only name, namespace, version, info, config and manifest, so every chart and
// app version flow read from it came back empty. get metadata is the command
// built for this question — it answers in one call, and it keeps the chart name
// and its version apart rather than in helm's joined "<name>-<version>"
// rendering, which cannot be split back (both halves may contain hyphens).
//
// The floor this sets is helm 3.10, where get metadata landed.
func (h *Helm) Status(ctx context.Context, release, namespace, kubeContext string) (Release, error) {
	args := []string{"get", "metadata", release, "-n", namespace, "-o", "json"}
	if kubeContext != "" {
		args = append(args, "--kube-context", kubeContext)
	}
	out, err := h.run(ctx, args...)
	if err != nil {
		return Release{}, err
	}

	var raw struct {
		Name       string `json:"name"`
		Namespace  string `json:"namespace"`
		Revision   int    `json:"revision"`
		Status     string `json:"status"`
		Chart      string `json:"chart"`
		Version    string `json:"version"`
		AppVersion string `json:"appVersion"`
		DeployedAt string `json:"deployedAt"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return Release{}, fmt.Errorf("parse helm get metadata: %w", err)
	}

	return Release{
		Name:         raw.Name,
		Namespace:    raw.Namespace,
		Revision:     raw.Revision,
		Status:       raw.Status,
		LastDeployed: parseHelmTime(raw.DeployedAt),
		ChartName:    raw.Chart,
		ChartVersion: raw.Version,
		AppVersion:   raw.AppVersion,
	}, nil
}

// Revision is one row of `helm history`.
type Revision struct {
	Revision    int       `json:"revision"`
	Updated     time.Time `json:"updated"`
	Status      string    `json:"status"`
	Chart       string    `json:"chart"`
	AppVersion  string    `json:"app_version"`
	Description string    `json:"description"`
}

// History returns the release's revision history, newest last.
func (h *Helm) History(ctx context.Context, release, namespace, kubeContext string) ([]Revision, error) {
	args := []string{"history", release, "-n", namespace, "-o", "json"}
	if kubeContext != "" {
		args = append(args, "--kube-context", kubeContext)
	}
	out, err := h.run(ctx, args...)
	if err != nil {
		return nil, err
	}

	var raw []struct {
		Revision    int    `json:"revision"`
		Updated     string `json:"updated"`
		Status      string `json:"status"`
		Chart       string `json:"chart"`
		AppVersion  string `json:"app_version"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse helm history: %w", err)
	}

	revisions := make([]Revision, 0, len(raw))
	for _, r := range raw {
		revisions = append(revisions, Revision{
			Revision:    r.Revision,
			Updated:     parseHelmTime(r.Updated),
			Status:      r.Status,
			Chart:       r.Chart,
			AppVersion:  r.AppVersion,
			Description: r.Description,
		})
	}
	return revisions, nil
}

// Rollback reverts a release to a previous revision. Revision 0 means the
// immediately previous one, which is helm's own default.
func (h *Helm) Rollback(ctx context.Context, release, namespace, kubeContext string, revision int) error {
	args := []string{"rollback", release}
	if revision > 0 {
		args = append(args, fmt.Sprint(revision))
	}
	args = append(args, "-n", namespace)
	if kubeContext != "" {
		args = append(args, "--kube-context", kubeContext)
	}
	_, err := h.Runner.Run(ctx, flowexec.Opts{Name: h.Bin, Args: args, Stream: true})
	return err
}

// Uninstall removes a release.
func (h *Helm) Uninstall(ctx context.Context, release, namespace, kubeContext string, keepHistory bool) error {
	args := []string{"uninstall", release, "-n", namespace}
	if keepHistory {
		args = append(args, "--keep-history")
	}
	if kubeContext != "" {
		args = append(args, "--kube-context", kubeContext)
	}
	_, err := h.Runner.Run(ctx, flowexec.Opts{Name: h.Bin, Args: args, Stream: true})
	return err
}

// GetValues returns the values actually deployed for a release.
func (h *Helm) GetValues(ctx context.Context, release, namespace, kubeContext string) (string, error) {
	args := []string{"get", "values", release, "-n", namespace, "-o", "yaml"}
	if kubeContext != "" {
		args = append(args, "--kube-context", kubeContext)
	}
	return h.run(ctx, args...)
}

// parseHelmTime reads the several timestamp formats helm emits, returning the
// zero time rather than an error: a missing timestamp is cosmetic.
func parseHelmTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"Mon Jan  2 15:04:05 2006",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
