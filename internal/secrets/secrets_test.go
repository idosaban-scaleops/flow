package secrets_test

import (
	"context"
	"strings"
	"testing"

	flowexec "github.com/idosaban-scaleops/flow/internal/exec"
	"github.com/idosaban-scaleops/flow/internal/secrets"
)

// env builds a Getenv from a map, so precedence is exercised without touching
// the real environment.
func env(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

func TestTokenResolutionOrder(t *testing.T) {
	keychain := flowexec.NewFake().Respond("security find-generic-password", "from-keychain\n")

	tests := []struct {
		name       string
		environ    map[string]string
		config     string
		runner     flowexec.Runner
		wantToken  string
		wantSource secrets.Source
	}{
		{
			name:       "GITHUB_TOKEN wins over everything",
			environ:    map[string]string{"GITHUB_TOKEN": "from-github-token", "GH_TOKEN": "from-gh-token"},
			config:     "from-config",
			runner:     keychain,
			wantToken:  "from-github-token",
			wantSource: secrets.SourceGitHubToken,
		},
		{
			name:       "GH_TOKEN wins over config and keychain",
			environ:    map[string]string{"GH_TOKEN": "from-gh-token"},
			config:     "from-config",
			runner:     keychain,
			wantToken:  "from-gh-token",
			wantSource: secrets.SourceGHToken,
		},
		{
			name:       "config wins over the keychain",
			config:     "from-config",
			runner:     keychain,
			wantToken:  "from-config",
			wantSource: secrets.SourceConfig,
		},
		{
			name:       "the keychain is the last resort",
			runner:     keychain,
			wantToken:  "from-keychain",
			wantSource: secrets.SourceKeychain,
		},
		{
			name: "nothing configured is not an error",
			runner: func() flowexec.Runner {
				f := flowexec.NewFake()
				f.RespondWith("security", flowexec.Response{ExitCode: 44, Stderr: "not found"})
				return f
			}(),
			wantToken:  "",
			wantSource: secrets.SourceNone,
		},
		{
			name:       "whitespace-only values are ignored",
			environ:    map[string]string{"GITHUB_TOKEN": "   \n"},
			config:     "from-config",
			runner:     keychain,
			wantToken:  "from-config",
			wantSource: secrets.SourceConfig,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &secrets.Resolver{
				Runner:          tt.runner,
				Getenv:          env(tt.environ),
				KeychainService: "flow-github-token",
				ConfigToken:     tt.config,
			}
			token, source := r.Token(context.Background())
			if token != tt.wantToken {
				t.Errorf("token = %q, want %q", token, tt.wantToken)
			}
			if source != tt.wantSource {
				t.Errorf("source = %q, want %q", source, tt.wantSource)
			}
		})
	}
}

func TestKeychainLookupIsMarkedSecret(t *testing.T) {
	// -vv dumps the stdout of every command it runs. The keychain lookup's
	// stdout *is* the token, so the invocation must be marked Secret or the
	// token lands in the log.
	recorder := &secretRecorder{Fake: flowexec.NewFake()}
	recorder.Respond("security find-generic-password", "tok\n")

	r := &secrets.Resolver{
		Runner: recorder, Getenv: env(nil), KeychainService: "flow-github-token",
	}
	if token, _ := r.Token(context.Background()); token != "tok" {
		t.Fatalf("token = %q", token)
	}
	if !recorder.sawSecret {
		t.Error("the keychain lookup must set Opts.Secret so -vv cannot echo the token")
	}
}

type secretRecorder struct {
	*flowexec.Fake
	sawSecret bool
}

func (s *secretRecorder) Run(ctx context.Context, opts flowexec.Opts) (flowexec.Result, error) {
	if opts.Secret {
		s.sawSecret = true
	}
	return s.Fake.Run(ctx, opts)
}

func TestKeychainArgv(t *testing.T) {
	f := flowexec.NewFake().Respond("security find-generic-password", "tok\n")
	r := &secrets.Resolver{Runner: f, Getenv: env(nil), KeychainService: "my-service"}
	if _, source := r.Token(context.Background()); source != secrets.SourceKeychain {
		t.Fatalf("source = %q", source)
	}

	want := "security find-generic-password -s my-service -w"
	if got := f.CommandLines(); len(got) != 1 || got[0] != want {
		t.Errorf("argv = %v, want [%s]", got, want)
	}
}

func TestNoKeychainServiceSkipsTheLookup(t *testing.T) {
	f := flowexec.NewFake()
	r := &secrets.Resolver{Runner: f, Getenv: env(nil)}
	if _, source := r.Token(context.Background()); source != secrets.SourceNone {
		t.Errorf("source = %q, want none", source)
	}
	if len(f.Calls) != 0 {
		t.Errorf("nothing should run without a keychain service: %v", f.CommandLines())
	}
}

func TestNilGetenvIsSafe(t *testing.T) {
	// A Resolver built without Getenv must not panic; it simply finds nothing
	// in the environment.
	r := &secrets.Resolver{}
	if token, source := r.Token(context.Background()); token != "" || source != secrets.SourceNone {
		t.Errorf("Token = %q, %q", token, source)
	}
}

func TestKeychainHintNamesTheService(t *testing.T) {
	hint := secrets.KeychainHint("flow-github-token")
	if !strings.Contains(hint, "flow-github-token") {
		t.Errorf("hint = %q, want it to name the service", hint)
	}
	if !strings.HasPrefix(hint, "security add-generic-password") {
		t.Errorf("hint = %q, want a runnable command", hint)
	}
}
