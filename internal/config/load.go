package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/adrg/xdg"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
)

// EnvPrefix is the prefix for environment overrides; "__" separates nesting
// levels, so FLOW_ASSETS__ROOT sets assets.root.
const EnvPrefix = "FLOW_"

// Loaded is a Config plus the provenance a user needs to debug it.
type Loaded struct {
	Config Config
	// Path is the config file flow resolved, whether or not it exists.
	Path string
	// Exists reports whether that file was actually read.
	Exists bool
}

// DefaultPath returns the config path, honoring --config and FLOW_CONFIG.
func DefaultPath(override string) string {
	if override != "" {
		return ExpandPath(override)
	}
	if p := os.Getenv("FLOW_CONFIG"); p != "" {
		return ExpandPath(p)
	}
	return filepath.Join(xdg.ConfigHome, "flow", "config.yaml")
}

// Load merges defaults, the config file, and FLOW_-prefixed environment
// variables, in that order of increasing precedence. Flags are applied by the
// caller, on top. A missing file is not an error.
func Load(override string) (Loaded, error) {
	path := DefaultPath(override)
	out := Loaded{Path: path}

	k := koanf.New(".")
	if err := k.Load(structs.Provider(Default(), "koanf"), nil); err != nil {
		return out, fmt.Errorf("load defaults: %w", err)
	}

	switch _, err := os.Stat(path); {
	case err == nil:
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
			return out, fmt.Errorf("read %s: %w", path, err)
		}
		out.Exists = true
	case errors.Is(err, fs.ErrNotExist):
		if override != "" || os.Getenv("FLOW_CONFIG") != "" {
			return out, fmt.Errorf("config file %s does not exist", path)
		}
	default:
		return out, fmt.Errorf("stat %s: %w", path, err)
	}

	envProvider := env.Provider(EnvPrefix, ".", func(k string) string {
		key := strings.TrimPrefix(k, EnvPrefix)
		return strings.ReplaceAll(strings.ToLower(key), "__", ".")
	})
	if err := k.Load(envProvider, nil); err != nil {
		return out, fmt.Errorf("load environment: %w", err)
	}

	cfg := Default()
	if err := k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return out, fmt.Errorf("parse config: %w", err)
	}
	normalize(&cfg)

	if err := cfg.Validate(); err != nil {
		return out, fmt.Errorf("invalid config (%s): %w", path, err)
	}
	out.Config = cfg
	return out, nil
}

// normalize fills in blanks that a partial config file would otherwise leave
// zeroed, since koanf overwrites whole sub-structs rather than merging fields.
func normalize(c *Config) {
	d := Default()
	if c.Defaults.BaseBranch == "" {
		c.Defaults.BaseBranch = d.Defaults.BaseBranch
	}
	if c.Defaults.Remote == "" {
		c.Defaults.Remote = d.Defaults.Remote
	}
	if c.Naming.Branch == "" {
		c.Naming.Branch = d.Naming.Branch
	}
	if c.Naming.AssetsDir == "" {
		c.Naming.AssetsDir = d.Naming.AssetsDir
	}
	if c.Naming.WorkspaceLabel == "" {
		c.Naming.WorkspaceLabel = d.Naming.WorkspaceLabel
	}
	if c.Naming.WorktreesSubdir == "" {
		c.Naming.WorktreesSubdir = d.Naming.WorktreesSubdir
	}
	if c.Assets.Root == "" {
		c.Assets.Root = d.Assets.Root
	}
	if c.Editor.Command == "" {
		c.Editor.Command = d.Editor.Command
	}
	if len(c.Editor.Args) == 0 {
		c.Editor.Args = d.Editor.Args
	}
	if c.Workspace.Provider == "" {
		c.Workspace.Provider = d.Workspace.Provider
	}
	if c.Workspace.Command == "" {
		c.Workspace.Command = d.Workspace.Command
	}
	if c.Jira.BrowseURL == "" {
		c.Jira.BrowseURL = d.Jira.BrowseURL
	}
	if c.GitHub.APIBase == "" {
		c.GitHub.APIBase = d.GitHub.APIBase
	}
	if c.GitHub.TokenKeychainService == "" {
		c.GitHub.TokenKeychainService = d.GitHub.TokenKeychainService
	}
	if c.Repos == nil {
		c.Repos = map[string]RepoConfig{}
	}
	if c.Clusters == nil {
		c.Clusters = map[string]ClusterConfig{}
	}
}
