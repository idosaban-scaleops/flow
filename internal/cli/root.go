// Package cli defines flow's command tree. One file per command; this file
// holds the global flags and the per-invocation state every command shares.
package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/idosaban-scaleops/flow/internal/config"
	"github.com/idosaban-scaleops/flow/internal/editor"
	flowexec "github.com/idosaban-scaleops/flow/internal/exec"
	"github.com/idosaban-scaleops/flow/internal/ghapi"
	"github.com/idosaban-scaleops/flow/internal/gitx"
	"github.com/idosaban-scaleops/flow/internal/helmx"
	"github.com/idosaban-scaleops/flow/internal/herdr"
	"github.com/idosaban-scaleops/flow/internal/kube"
	"github.com/idosaban-scaleops/flow/internal/output"
	"github.com/idosaban-scaleops/flow/internal/registry"
	"github.com/idosaban-scaleops/flow/internal/secrets"
)

// annotationAllowMissingConfig marks the commands whose whole job is to create
// or locate a config file that does not exist yet.
const annotationAllowMissingConfig = "flow/allow-missing-config"

// globals holds the flags registered on the root command and inherited by
// every subcommand.
type globals struct {
	json       bool
	noColor    bool
	verbose    int
	configPath string
	yes        bool
	dryRun     bool
}

// App is the state one flow invocation shares across its command. It is built
// lazily, so that `flow --help` never reads a config file or takes a lock.
type App struct {
	g globals

	cfg      config.Config
	cfgPath  string
	cfgFound bool

	Out      *output.Renderer
	Runner   flowexec.Runner
	Registry *registry.Store

	// baseRunner is the unwrapped runner, for read-only work that must still
	// happen under --dry-run.
	baseRunner flowexec.Runner

	// The token and the GitHub client are resolved once per invocation.
	// `flow delete --all` and `flow list` ask for them once per ticket, and
	// each miss used to mean another `security find-generic-password` spawn
	// and a fresh HTTP client — which also threw away the ETag cache that
	// makes the polling loop cheap.
	tokenOnce   sync.Once
	tokenValue  string
	tokenSource secrets.Source
	githubOnce  sync.Once
	githubValue *ghapi.Client
}

// Config returns the effective configuration.
func (a *App) Config() config.Config { return a.cfg }

// ConfigPath returns the resolved config file path, whether or not it exists.
func (a *App) ConfigPath() string { return a.cfgPath }

// ConfigExists reports whether a config file was actually read.
func (a *App) ConfigExists() bool { return a.cfgFound }

// JSON reports whether --json was given.
func (a *App) JSON() bool { return a.g.json }

// Yes reports whether confirmations are pre-answered.
func (a *App) Yes() bool { return a.g.yes }

// DryRun reports whether mutating actions are suppressed.
func (a *App) DryRun() bool { return a.g.dryRun }

// Git returns a git client rooted at dir.
func (a *App) Git(dir string) *gitx.Git { return gitx.New(a.Runner, dir) }

// ReadGit returns a git client that runs even under --dry-run, for inspection.
func (a *App) ReadGit(dir string) *gitx.Git { return gitx.New(a.baseRunner, dir) }

// GitHub builds the GitHub client, resolving a token from the environment, the
// config, or the macOS keychain. A missing token is not an error: the returned
// client reports Unknown for everything it cannot determine.
func (a *App) GitHub(ctx context.Context) *ghapi.Client {
	a.githubOnce.Do(func() {
		token, _ := a.Token(ctx)
		// ghapi.New does not retain ctx — every request carries its own — so
		// one client is reusable for the whole invocation.
		a.githubValue = ghapi.New(ctx, ghapi.Options{
			APIBase:   a.cfg.GitHub.APIBase,
			Token:     token,
			UserAgent: "flow/" + Version,
		})
	})
	return a.githubValue
}

// Token resolves the GitHub token and reports where it came from. The value is
// never logged; only the source is ever displayed.
func (a *App) Token(ctx context.Context) (string, secrets.Source) {
	a.tokenOnce.Do(func() {
		r := &secrets.Resolver{
			Runner:          a.baseRunner,
			Getenv:          os.Getenv,
			KeychainService: a.cfg.GitHub.TokenKeychainService,
			ConfigToken:     a.cfg.GitHub.Token,
		}
		a.tokenValue, a.tokenSource = r.Token(ctx)
	})
	return a.tokenValue, a.tokenSource
}

// Herdr returns the workspace client, or nil when the provider is "none".
func (a *App) Herdr() *herdr.CLI {
	if a.cfg.Workspace.Provider == "none" {
		return nil
	}
	return herdr.New(a.Runner, a.cfg.Workspace.Command, a.Out.Log)
}

// ReadHerdr returns a workspace client that runs even under --dry-run.
func (a *App) ReadHerdr() *herdr.CLI {
	if a.cfg.Workspace.Provider == "none" {
		return nil
	}
	return herdr.New(a.baseRunner, a.cfg.Workspace.Command, a.Out.Log)
}

// Editor returns the editor launcher.
func (a *App) Editor() *editor.Launcher { return editor.New(a.Runner, a.cfg.Editor) }

// Helm returns the helm wrapper used for mutating operations.
func (a *App) Helm() *helmx.Helm { return helmx.New(a.Runner) }

// ReadHelm returns a helm wrapper for queries, which run even under --dry-run.
func (a *App) ReadHelm() *helmx.Helm { return helmx.New(a.baseRunner) }

// Kube returns the kubectl wrapper. Everything flow asks kubectl is read-only,
// so it always uses the unwrapped runner.
func (a *App) Kube() *kube.Kube { return kube.New(a.baseRunner) }

// mutate performs a filesystem change that does not go through exec.Runner —
// creating a directory, writing the registry, editing the config file. Under
// --dry-run it is announced and skipped, which is what stops a dry run from
// quietly editing .git/info/exclude or the registry.
func (a *App) mutate(description string, fn func() error) error {
	if a.g.dryRun {
		a.Out.Status("%s %s", a.Out.Theme.Muted.Render("would"), description)
		return nil
	}
	return fn()
}

// confirm asks a non-destructive question. --json implies yes here, per the
// global flag contract.
func (a *App) confirm(title, description string, defaultYes bool) error {
	return a.Out.Confirm(title, description, defaultYes, a.g.yes || a.g.json)
}

// confirmDestructive asks a question whose "yes" destroys something. It always
// defaults to No, and --json does not imply consent: the user must pass --yes
// explicitly.
func (a *App) confirmDestructive(title, description string) error {
	if a.g.json && !a.g.yes {
		return Precondition(
			"%s: --json does not imply consent for a destructive action; pass --yes", title)
	}
	return a.Out.Confirm(title, description, false, a.g.yes)
}

// NewRootCommand builds the command tree.
func NewRootCommand() *cobra.Command {
	app := &App{}

	root := &cobra.Command{
		Use:   "flow",
		Short: "A personal workflow CLI for ticket-driven development",
		Long: strings.TrimSpace(`
flow automates the lifecycle of working on a ticket: creating a git worktree,
spinning up a terminal workspace, opening an editor, managing per-ticket asset
folders, tearing everything down when the PR merges, and upgrading a local
Kubernetes dev cluster with the Helm chart built from the ticket's branch.

It is a toolbox, not a workflow engine. Each subcommand does one job, and a
small local registry remembers what flow created so it can find it again.`),
		SilenceUsage:      true,
		SilenceErrors:     true,
		PersistentPreRunE: app.setup,
	}

	f := root.PersistentFlags()
	f.BoolVar(&app.g.json, "json", false, "emit machine-readable JSON on stdout")
	f.BoolVar(&app.g.noColor, "no-color", false, "disable ANSI colors (NO_COLOR is also honored)")
	f.CountVarP(&app.g.verbose, "verbose", "v", "log external commands (-vv adds their output)")
	f.StringVar(&app.g.configPath, "config", "", "path to an alternate config file")
	f.BoolVarP(&app.g.yes, "yes", "y", false, "assume yes for all confirmations")
	f.BoolVar(&app.g.dryRun, "dry-run", false, "print mutating actions instead of performing them")

	root.AddCommand(
		newInitCommand(app),
		newListCommand(app),
		newStatusCommand(app),
		newOpenCommand(app),
		newPathCommand(app),
		newAssetsCommand(app),
		newDeleteCommand(app),
		newPruneCommand(app),
		newJiraCommand(app),
		newPRCommand(app),
		newClusterCommand(app),
		newDoctorCommand(app),
		newConfigCommand(app),
		newVersionCommand(app),
	)

	// fang registers a `completion` command (and a hidden `man` command).
	// Register the plural spelling the user expects as an alias on it rather
	// than writing a second generator. This must run after AddCommand: cobra
	// refuses to create the command while the root has no subcommands.
	root.InitDefaultCompletionCmd()
	if c := findChild(root, "completion"); c != nil {
		c.Aliases = append(c.Aliases, "completions")
		c.Long = completionsHelp
	}

	return root
}

// isCompletionRequest reports whether cobra is answering a shell's
// __complete request rather than running a real command.
func isCompletionRequest(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		switch c.Name() {
		case cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
			return true
		}
	}
	return false
}

// findChild returns a direct subcommand by name.
func findChild(parent *cobra.Command, name string) *cobra.Command {
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

// setup builds the shared state before any command runs.
func (a *App) setup(cmd *cobra.Command, _ []string) error {
	a.Out = output.New(output.Options{
		JSON:    a.g.json,
		NoColor: a.g.noColor,
		Verbose: a.g.verbose,
		Stdout:  cmd.OutOrStdout(),
		Stderr:  cmd.ErrOrStderr(),
		Stdin:   cmd.InOrStdin(),
	})

	loaded, err := config.Load(a.g.configPath)
	if err != nil {
		// Two cases must survive an unreadable config. `flow config
		// init|path|edit` exists precisely to create or locate one, and shell
		// completion must never fail loudly — cobra runs this hook for
		// __complete too, and an error there reaches the user's shell as noise.
		// Everything else treats a bad explicitly-named config as the typo it
		// usually is.
		if cmd.Annotations[annotationAllowMissingConfig] != "true" && !isCompletionRequest(cmd) {
			return Wrap(ExitUsage, "bad_config", err, "configuration")
		}
		a.cfg = config.Default()
		a.cfgPath, a.cfgFound = config.DefaultPath(a.g.configPath), false
	} else {
		a.cfg, a.cfgPath, a.cfgFound = loaded.Config, loaded.Path, loaded.Exists
	}

	runner := &flowexec.Real{
		Log:         a.Out.Log,
		DebugOutput: a.g.verbose >= 2,
		Stdout:      cmd.OutOrStdout(),
		Stderr:      cmd.ErrOrStderr(),
	}
	a.baseRunner = runner

	if a.g.dryRun {
		a.Runner = &flowexec.DryRun{
			Inner:  runner,
			IsRead: isReadOnlyCommand,
			Announce: func(cmdline string) {
				a.Out.Status("%s %s", a.Out.Theme.Muted.Render("would run:"), cmdline)
			},
		}
	} else {
		a.Runner = runner
	}

	a.Registry = registry.Open("")
	return nil
}

// mutatingGitVerbs are the git subcommands that change the repository. Under
// --dry-run everything else still runs, because a dry run that cannot inspect
// the repository cannot describe what it would do.
//
// This is a denylist, which is the wrong shape for a safety check: a verb added
// later that is not named here executes under --dry-run instead of being
// announced. It stays a denylist anyway, because the obvious inversion is
// worse. Many git verbs are read-only in one form and mutating in another —
// `remote get-url`, `config --get` and `stash list` are all reads flow either
// makes today or plausibly would — so an allowlist of read verbs turns every
// omission into a dry run that cannot inspect the repository, and a dry run
// that cannot inspect cannot describe what it would do.
//
// What keeps it honest is auditing the call sites, which are all in
// internal/gitx: today they are rev-parse, remote get-url, show-ref,
// for-each-ref, branch, status, rev-list, log, fetch and worktree. When adding
// a git call, decide which side of this list it is on.
var mutatingGitVerbs = []string{
	"worktree", "branch", "fetch", "push",
	"checkout", "switch", "commit", "add",
	"reset", "merge", "rebase", "tag", "clean",
}

// readOnlyGitWorktreeVerbs are the `git worktree` subcommands that only read.
var readOnlyGitWorktreeVerbs = []string{"list"}

func isReadOnlyCommand(opts flowexec.Opts) bool {
	if len(opts.Args) == 0 {
		return true
	}
	verb := opts.Args[0]

	// A bare capability probe reads nothing and mutates nothing. Without this
	// `herdr --version` classifies as mutating, and doctor would report an
	// empty version string under --dry-run.
	if len(opts.Args) == 1 && (verb == "--version" || verb == "-version") {
		return true
	}

	switch opts.Name {
	case "git":
		if verb == "worktree" && len(opts.Args) > 1 && slices.Contains(readOnlyGitWorktreeVerbs, opts.Args[1]) {
			return true
		}
		// `git branch` with no mutating flag only lists.
		if verb == "branch" && !containsAny(opts.Args, "-d", "-D", "-m", "-M", "-c", "-C") {
			return true
		}
		return !slices.Contains(mutatingGitVerbs, verb)
	case "helm":
		switch verb {
		case "list", "status", "history", "get", "search", "version", "show":
			return true
		case "repo":
			return len(opts.Args) > 1 && opts.Args[1] == "list"
		default:
			return false
		}
	case "kubectl":
		switch verb {
		case "get", "version", "describe":
			return true
		case "config":
			// `kubectl config` is not read-only as a family: use-context,
			// set-context and friends rewrite the user's kubeconfig, which
			// flow must never do. Name the subcommands flow actually
			// issues instead of waving the whole verb through.
			return len(opts.Args) > 1 &&
				slices.Contains([]string{"view", "current-context", "get-contexts"}, opts.Args[1])
		default:
			return false
		}
	case "herdr":
		return len(opts.Args) > 1 && opts.Args[1] == "list"
	case "security":
		// flow only ever runs `security find-generic-password`, a read. Anything
		// else reaching here would be a new call site that needs classifying.
		return len(opts.Args) > 0 && opts.Args[0] == "find-generic-password"
	case "open":
		return false
	default:
		return false
	}
}

func containsAny(args []string, wanted ...string) bool {
	return slices.ContainsFunc(args, func(a string) bool { return slices.Contains(wanted, a) })
}

// run wraps a command body so that errors are rendered once, in the right
// format: a JSON envelope on stdout in --json mode, fang's styled error
// otherwise.
func (a *App) run(fn func(ctx context.Context) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		err := fn(cmd.Context())
		return a.report(err)
	}
}

// runArgs is run for commands that take positional arguments.
func (a *App) runArgs(fn func(ctx context.Context, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		err := fn(cmd.Context(), args)
		return a.report(err)
	}
}

func (a *App) report(err error) error {
	if err == nil {
		return nil
	}
	if a.g.json {
		a.Out.JSONError(KindFor(err), err.Error())
		return &jsonReported{err: err}
	}
	return err
}

// jsonReported marks an error whose JSON envelope has already been written, so
// fang does not print it a second time.
type jsonReported struct{ err error }

func (j *jsonReported) Error() string { return j.err.Error() }
func (j *jsonReported) Unwrap() error { return j.err }

func jsonErrorEmitted(err error) bool {
	var j *jsonReported
	return errors.As(err, &j)
}

// cwd returns the current directory, resolved through symlinks so that
// comparisons against git's output and against registry paths both work.
func cwd() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", Failure("cannot determine the current directory: %v", err)
	}
	return gitx.ResolvePath(dir), nil
}

// repoDir resolves the directory a repo-scoped command should operate from.
func repoDir(override string) (string, error) {
	if override != "" {
		abs, err := filepath.Abs(config.ExpandPath(override))
		if err != nil {
			return "", Usage("invalid --repo path %q: %v", override, err)
		}
		return gitx.ResolvePath(abs), nil
	}
	return cwd()
}
