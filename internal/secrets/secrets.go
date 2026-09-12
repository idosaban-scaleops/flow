// Package secrets resolves the GitHub token. A missing token is never fatal:
// GitHub-dependent features degrade to "unknown" rather than failing.
package secrets

import (
	"context"
	"strings"

	flowexec "github.com/idosaban-scaleops/flow/internal/exec"
)

// Source names where a token came from, for `flow doctor`. The token value
// itself is never logged or displayed.
type Source string

// The token resolution order, first hit wins. These are the names of the
// sources, never the values found in them.
//
//nolint:gosec // G101 false positive: these are source labels, not credentials
const (
	SourceNone        Source = "none"
	SourceGitHubToken Source = "$GITHUB_TOKEN"
	SourceGHToken     Source = "$GH_TOKEN"
	SourceConfig      Source = "config github.token"
	SourceKeychain    Source = "macOS keychain"
)

// Resolver finds a GitHub token.
type Resolver struct {
	Runner flowexec.Runner
	// Getenv is os.Getenv in production; tests substitute their own.
	Getenv func(string) string
	// KeychainService is the macOS keychain service name to query.
	KeychainService string
	// ConfigToken is github.token from the config file.
	ConfigToken string
}

// Token returns the first token found and where it came from.
func (r *Resolver) Token(ctx context.Context) (string, Source) {
	getenv := r.Getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}

	for _, candidate := range []struct {
		value  string
		source Source
	}{
		{getenv("GITHUB_TOKEN"), SourceGitHubToken},
		{getenv("GH_TOKEN"), SourceGHToken},
		{r.ConfigToken, SourceConfig},
	} {
		if v := strings.TrimSpace(candidate.value); v != "" {
			return v, candidate.source
		}
	}

	if token := r.fromKeychain(ctx); token != "" {
		return token, SourceKeychain
	}
	return "", SourceNone
}

// fromKeychain reads the token from the macOS keychain. The invocation is
// marked Secret so that -vv cannot echo the token into the log.
func (r *Resolver) fromKeychain(ctx context.Context) string {
	if r.Runner == nil || r.KeychainService == "" {
		return ""
	}
	res, err := r.Runner.Run(ctx, flowexec.Opts{
		Name:   "security",
		Args:   []string{"find-generic-password", "-s", r.KeychainService, "-w"},
		Secret: true,
	})
	if err != nil {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

// KeychainHint returns the command that stores a token, for error messages and
// `flow doctor`.
func KeychainHint(service string) string {
	return "security add-generic-password -a \"$USER\" -s " + service + " -w"
}
