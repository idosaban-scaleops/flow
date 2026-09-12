package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/idosaban-scaleops/flow/internal/config"
)

func TestDefaultIsValid(t *testing.T) {
	if err := config.Default().Validate(); err != nil {
		t.Fatalf("built-in defaults must validate: %v", err)
	}
}

func TestTemplateMatchesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(config.Template), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("the template flow writes must load cleanly: %v", err)
	}

	want := config.Default()
	if got.Config.Defaults != want.Defaults {
		t.Errorf("defaults drifted: %+v != %+v", got.Config.Defaults, want.Defaults)
	}
	if got.Config.Naming != want.Naming {
		t.Errorf("naming drifted: %+v != %+v", got.Config.Naming, want.Naming)
	}
	if got.Config.Jira != want.Jira {
		t.Errorf("jira drifted: %+v != %+v", got.Config.Jira, want.Jira)
	}
}

func TestLoadPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := strings.Join([]string{
		"defaults:",
		"  base_branch: develop",
		"assets:",
		"  root: /from/file",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("file beats defaults", func(t *testing.T) {
		got, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if got.Config.Defaults.BaseBranch != "develop" {
			t.Errorf("base_branch = %q, want develop", got.Config.Defaults.BaseBranch)
		}
		// Unset keys must still carry their defaults.
		if got.Config.Defaults.Remote != "origin" {
			t.Errorf("remote = %q, want origin", got.Config.Defaults.Remote)
		}
	})

	t.Run("env beats file", func(t *testing.T) {
		t.Setenv("FLOW_ASSETS__ROOT", "/from/env")
		t.Setenv("FLOW_DEFAULTS__REMOTE", "upstream")
		got, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if got.Config.Assets.Root != "/from/env" {
			t.Errorf("assets.root = %q, want /from/env", got.Config.Assets.Root)
		}
		if got.Config.Defaults.Remote != "upstream" {
			t.Errorf("remote = %q, want upstream", got.Config.Defaults.Remote)
		}
	})
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	t.Setenv("FLOW_CONFIG", "")
	got, err := config.Load("")
	if err != nil {
		t.Fatalf("a missing config file must not fail: %v", err)
	}
	if got.Config.Defaults.BaseBranch == "" {
		t.Error("defaults must still be populated")
	}
}

func TestLoadExplicitMissingFileIsAnError(t *testing.T) {
	if _, err := config.Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("an explicitly named missing config file must fail")
	}
}

func TestSetClusterPreservesComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(config.Template), 0o600); err != nil {
		t.Fatal(err)
	}

	cc := config.ClusterConfig{
		ReleaseName: "scaleops",
		Namespace:   "scaleops-system",
		ValuesFile:  "~/Developer/helm-values/ido-saban-dev.yaml",
		HelmRepo:    "scaleops",
		Chart:       "scaleops/scaleops",
		ExtraArgs:   []string{"--reset-then-reuse-values"},
		Repo:        "scaleops-sh/scaleops",
	}
	if err := config.SetCluster(path, "ido-saban-dev", cc); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "# Every key below is optional") {
		t.Error("leading comments were lost by the write-back")
	}
	if !strings.Contains(text, "# that difference is deliberate and load-bearing.") {
		t.Error("inline comments were lost by the write-back")
	}

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("config must still parse after write-back: %v", err)
	}
	back, ok := got.Config.Cluster("ido-saban-dev")
	if !ok {
		t.Fatal("cluster entry was not written")
	}
	if back.Namespace != "scaleops-system" || back.Chart != "scaleops/scaleops" {
		t.Errorf("round-trip mismatch: %+v", back)
	}
	if len(back.ExtraArgs) != 1 || back.ExtraArgs[0] != "--reset-then-reuse-values" {
		t.Errorf("extra_args round-trip mismatch: %+v", back.ExtraArgs)
	}
}

func TestSetClusterUpdatesInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	first := config.ClusterConfig{ReleaseName: "a", Namespace: "ns-a"}
	if err := config.SetCluster(path, "ctx", first); err != nil {
		t.Fatal(err)
	}
	second := config.ClusterConfig{ReleaseName: "b", Namespace: "ns-b"}
	if err := config.SetCluster(path, "ctx", second); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Clusters map[string]config.ClusterConfig `yaml:"clusters"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Clusters) != 1 {
		t.Fatalf("want exactly one cluster entry, got %d", len(doc.Clusters))
	}
	if doc.Clusters["ctx"].ReleaseName != "b" {
		t.Errorf("release_name = %q, want b", doc.Clusters["ctx"].ReleaseName)
	}
}

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty stays empty", "", ""},
		{"tilde alone", "~", home},
		{"tilde prefix", "~/Developer/assets", filepath.Join(home, "Developer", "assets")},
		{"absolute untouched", "/tmp/x", "/tmp/x"},
		{"tilde mid-path is literal", "/a/~/b", "/a/~/b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := config.ExpandPath(tt.in); got != tt.want {
				t.Errorf("ExpandPath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestValidateRejectsBadProvider(t *testing.T) {
	c := config.Default()
	c.Workspace.Provider = "tmux"
	if err := c.Validate(); err == nil {
		t.Fatal("an unsupported workspace provider must be rejected")
	}
}
