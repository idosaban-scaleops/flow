// Package config defines flow's configuration schema, its defaults, and the
// precedence rules that merge them: flags beat environment, environment beats
// the file, the file beats built-in defaults. flow works with no config at all.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the whole configuration tree.
type Config struct {
	Defaults  Defaults                 `koanf:"defaults" yaml:"defaults"`
	Naming    Naming                   `koanf:"naming" yaml:"naming"`
	Assets    Assets                   `koanf:"assets" yaml:"assets"`
	Editor    Editor                   `koanf:"editor" yaml:"editor"`
	Workspace Workspace                `koanf:"workspace" yaml:"workspace"`
	Jira      Jira                     `koanf:"jira" yaml:"jira"`
	GitHub    GitHub                   `koanf:"github" yaml:"github"`
	Repos     map[string]RepoConfig    `koanf:"repos" yaml:"repos,omitempty"`
	Clusters  map[string]ClusterConfig `koanf:"clusters" yaml:"clusters,omitempty"`
}

// Defaults control branch creation and which side effects flow init performs.
type Defaults struct {
	BaseBranch        string `koanf:"base_branch" yaml:"base_branch"`
	Remote            string `koanf:"remote" yaml:"remote"`
	FetchBeforeBranch bool   `koanf:"fetch_before_branch" yaml:"fetch_before_branch"`
	OpenEditor        bool   `koanf:"open_editor" yaml:"open_editor"`
	CreateWorkspace   bool   `koanf:"create_workspace" yaml:"create_workspace"`
	CreateAssets      bool   `koanf:"create_assets" yaml:"create_assets"`
}

// Naming holds the templates that derive every path and label from a ticket.
type Naming struct {
	Branch          string `koanf:"branch" yaml:"branch"`
	AssetsDir       string `koanf:"assets_dir" yaml:"assets_dir"`
	WorkspaceLabel  string `koanf:"workspace_label" yaml:"workspace_label"`
	WorktreesSubdir string `koanf:"worktrees_subdir" yaml:"worktrees_subdir"`
}

// Assets locates the per-ticket asset folders.
type Assets struct {
	Root string `koanf:"root" yaml:"root"`
}

// Editor describes how to launch the editor for a worktree.
type Editor struct {
	Command string   `koanf:"command" yaml:"command"`
	Args    []string `koanf:"args" yaml:"args"`
	Detach  bool     `koanf:"detach" yaml:"detach"`
}

// Workspace selects the terminal workspace provider.
type Workspace struct {
	Provider string `koanf:"provider" yaml:"provider"`
	Command  string `koanf:"command" yaml:"command"`
}

// Jira holds the browse URL template. flow never calls the Jira API.
type Jira struct {
	BrowseURL string `koanf:"browse_url" yaml:"browse_url"`
}

// GitHub holds API and token-resolution settings.
type GitHub struct {
	APIBase              string `koanf:"api_base" yaml:"api_base"`
	Token                string `koanf:"token" yaml:"token,omitempty"`
	TokenKeychainService string `koanf:"token_keychain_service" yaml:"token_keychain_service"`
}

// RepoConfig holds per-repository overrides, keyed by "owner/name" or by
// absolute repo path when the origin remote cannot be parsed.
type RepoConfig struct {
	BaseBranch          string   `koanf:"base_branch" yaml:"base_branch,omitempty"`
	HelmRepo            string   `koanf:"helm_repo" yaml:"helm_repo,omitempty"`
	ChartName           string   `koanf:"chart_name" yaml:"chart_name,omitempty"`
	BuildWorkflow       string   `koanf:"build_workflow" yaml:"build_workflow,omitempty"`
	ChartVersionPattern string   `koanf:"chart_version_pattern" yaml:"chart_version_pattern,omitempty"`
	WaitForJobs         []string `koanf:"wait_for_jobs" yaml:"wait_for_jobs,omitempty"`
}

// ClusterConfig holds per-kube-context settings. Its presence in the config is
// the allowlist: flow refuses to upgrade a context it has never been told about.
type ClusterConfig struct {
	ReleaseName string   `koanf:"release_name" yaml:"release_name"`
	Namespace   string   `koanf:"namespace" yaml:"namespace"`
	ValuesFile  string   `koanf:"values_file" yaml:"values_file,omitempty"`
	HelmRepo    string   `koanf:"helm_repo" yaml:"helm_repo,omitempty"`
	Chart       string   `koanf:"chart" yaml:"chart,omitempty"`
	ExtraArgs   []string `koanf:"extra_args" yaml:"extra_args,omitempty"`
	Repo        string   `koanf:"repo" yaml:"repo,omitempty"`
}

// Default returns the built-in configuration, which is a complete, usable
// configuration on its own.
func Default() Config {
	return Config{
		Defaults: Defaults{
			BaseBranch:        "main",
			Remote:            "origin",
			FetchBeforeBranch: true,
			OpenEditor:        true,
			CreateWorkspace:   true,
			CreateAssets:      true,
		},
		Naming: Naming{
			Branch:          "{id}-{slug}",
			AssetsDir:       "{id}_{slug}",
			WorkspaceLabel:  "{id}",
			WorktreesSubdir: ".worktrees",
		},
		Assets:    Assets{Root: "~/Developer/assets"},
		Editor:    Editor{Command: "cursor", Args: []string{"{path}"}, Detach: true},
		Workspace: Workspace{Provider: "herdr", Command: "herdr"},
		Jira:      Jira{BrowseURL: "https://scaleopscom.atlassian.net/browse/{id}"},
		// token_keychain_service names a keychain entry; it is not itself a
		// credential.
		GitHub: GitHub{ //nolint:gosec // G101 false positive on the service name
			APIBase:              "https://api.github.com",
			TokenKeychainService: "flow-github-token",
		},
		Repos:    map[string]RepoConfig{},
		Clusters: map[string]ClusterConfig{},
	}
}

// Repo returns the overrides for a repo key, or the zero value if there are
// none. It never returns nil, so callers can read fields unconditionally.
func (c Config) Repo(key string) RepoConfig {
	if rc, ok := c.Repos[key]; ok {
		return rc
	}
	return RepoConfig{}
}

// Cluster returns the settings for a kube context and whether they exist. The
// boolean matters: absence is the signal to run first-use setup, or to refuse.
func (c Config) Cluster(ctx string) (ClusterConfig, bool) {
	cc, ok := c.Clusters[ctx]
	return cc, ok
}

// BaseBranchFor resolves the base branch for a repo, falling back to the global
// default.
func (c Config) BaseBranchFor(repoKey string) string {
	if b := c.Repo(repoKey).BaseBranch; b != "" {
		return b
	}
	return c.Defaults.BaseBranch
}

// AssetsRoot returns the assets root with ~ expanded.
func (c Config) AssetsRoot() string { return ExpandPath(c.Assets.Root) }

// Validate reports configuration that flow cannot act on.
func (c Config) Validate() error {
	if c.Defaults.Remote == "" {
		return fmt.Errorf("defaults.remote must not be empty")
	}
	if c.Defaults.BaseBranch == "" {
		return fmt.Errorf("defaults.base_branch must not be empty")
	}
	for _, f := range []struct{ name, tmpl string }{
		{"naming.branch", c.Naming.Branch},
		{"naming.assets_dir", c.Naming.AssetsDir},
		{"naming.workspace_label", c.Naming.WorkspaceLabel},
	} {
		if f.tmpl == "" {
			return fmt.Errorf("%s must not be empty", f.name)
		}
	}
	if c.Naming.WorktreesSubdir == "" {
		return fmt.Errorf("naming.worktrees_subdir must not be empty")
	}
	switch c.Workspace.Provider {
	case "herdr", "none", "":
	default:
		return fmt.Errorf("workspace.provider %q is not supported (want \"herdr\" or \"none\")", c.Workspace.Provider)
	}
	if !strings.Contains(c.Jira.BrowseURL, "{id}") {
		return fmt.Errorf("jira.browse_url must contain {id}")
	}
	return nil
}

// ExpandPath expands a leading ~ and returns an absolute path where possible.
// Every path-valued config field passes through here.
func ExpandPath(p string) string {
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			p = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// Marshal renders the effective configuration as YAML, for `flow config show`.
func Marshal(c Config) (string, error) {
	data, err := yaml.Marshal(c)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(data), "\n"), nil
}
