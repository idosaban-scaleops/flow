package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/idosaban-scaleops/flow/internal/config"
	flowexec "github.com/idosaban-scaleops/flow/internal/exec"
	"github.com/idosaban-scaleops/flow/internal/secrets"
)

// checkLevel is a doctor result.
type checkLevel string

// Doctor result levels.
const (
	levelPass checkLevel = "PASS"
	levelWarn checkLevel = "WARN"
	levelFail checkLevel = "FAIL"
)

type check struct {
	Name   string     `json:"name"`
	Level  checkLevel `json:"level"`
	Detail string     `json:"detail,omitempty"`
	// Hint is what to do about it.
	Hint string `json:"hint,omitempty"`
}

func newDoctorCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that everything flow depends on is working",
		Long: "Check git, herdr, the editor, helm, kubectl, the GitHub token, the config\n" +
			"file and the registry, reporting each as PASS, WARN or FAIL with a\n" +
			"remediation hint. Exits 0 unless something FAILs.",
		Args: cobra.NoArgs,
	}

	cmd.RunE = app.run(func(ctx context.Context) error {
		checks := app.runChecks(ctx)

		if app.JSON() {
			if err := app.Out.JSON(map[string]any{
				"checks": checks, "ok": !anyFailed(checks),
			}); err != nil {
				return err
			}
		} else {
			app.renderChecks(checks)
		}

		if anyFailed(checks) {
			return &Error{Code: ExitDependency, Kind: "doctor", Msg: "one or more checks failed"}
		}
		return nil
	})
	return cmd
}

func anyFailed(checks []check) bool {
	for _, c := range checks {
		if c.Level == levelFail {
			return true
		}
	}
	return false
}

func (a *App) runChecks(ctx context.Context) []check {
	var checks []check
	add := func(c check) { checks = append(checks, c) }

	add(a.checkGit(ctx))
	add(a.checkHerdr(ctx))
	add(a.checkEditor())
	checks = append(checks, a.checkHelm(ctx)...)
	checks = append(checks, a.checkKube(ctx)...)
	add(a.checkGitHubToken(ctx))
	checks = append(checks, a.checkConfig()...)
	add(a.checkRegistry())
	return checks
}

func (a *App) checkGit(ctx context.Context) check {
	res, err := a.baseRunner.Run(ctx, flowexec.Opts{Name: "git", Args: []string{"--version"}})
	if err != nil {
		return check{Name: "git", Level: levelFail, Detail: err.Error(),
			Hint: "install git, or install the Xcode command line tools"}
	}
	version := strings.TrimSpace(res.Stdout)

	// Worktrees arrived in git 2.5. Probing by running the subcommand is
	// unreliable — `git worktree list -h` exits non-zero by design, and outside
	// a repository it fails for an unrelated reason — so compare the version.
	if major, minor, ok := parseGitVersion(version); ok && (major < 2 || (major == 2 && minor < 5)) {
		return check{Name: "git", Level: levelWarn, Detail: version,
			Hint: "flow needs git 2.5 or newer for worktree support"}
	}
	return check{Name: "git", Level: levelPass, Detail: version}
}

// parseGitVersion pulls the major and minor out of "git version 2.54.0 (...)".
func parseGitVersion(s string) (major, minor int, ok bool) {
	fields := strings.Fields(s)
	for _, f := range fields {
		parts := strings.SplitN(f, ".", 3)
		if len(parts) < 2 {
			continue
		}
		major, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		minor, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		return major, minor, true
	}
	return 0, 0, false
}

func (a *App) checkHerdr(ctx context.Context) check {
	if a.cfg.Workspace.Provider == "none" {
		return check{Name: "herdr", Level: levelPass, Detail: "workspace provider is \"none\""}
	}
	client := a.ReadHerdr()

	if _, err := a.baseRunner.LookPath(a.cfg.Workspace.Command); err != nil {
		return check{Name: "herdr", Level: levelWarn, Detail: "not on PATH",
			Hint: "install herdr, or set workspace.provider: none — flow works without it"}
	}
	version, err := client.Version(ctx)
	if err != nil {
		version = "unknown version"
	}
	if !client.Available(ctx) {
		return check{Name: "herdr", Level: levelWarn, Detail: version + ", server unreachable",
			Hint: "start herdr; flow init will still create the worktree without a workspace"}
	}
	return check{Name: "herdr", Level: levelPass, Detail: version}
}

func (a *App) checkEditor() check {
	command := a.cfg.Editor.Command
	if command == "" {
		return check{Name: "editor", Level: levelWarn, Detail: "no editor configured",
			Hint: "set editor.command in " + a.cfgPath}
	}
	path, err := a.baseRunner.LookPath(command)
	if err != nil {
		return check{Name: "editor", Level: levelWarn, Detail: command + " is not on PATH",
			Hint: "install it, or change editor.command in " + a.cfgPath}
	}
	return check{Name: "editor", Level: levelPass, Detail: path}
}

func (a *App) checkHelm(ctx context.Context) []check {
	helm := a.ReadHelm()
	if !helm.Available() {
		return []check{{Name: "helm", Level: levelWarn, Detail: "not on PATH",
			Hint: "install helm; only the flow cluster commands need it"}}
	}

	version, err := helm.Version(ctx)
	if err != nil {
		return []check{{Name: "helm", Level: levelWarn, Detail: err.Error()}}
	}
	checks := []check{{Name: "helm", Level: levelPass, Detail: version}}

	repos, err := helm.ListRepos(ctx)
	switch {
	case err != nil:
		checks = append(checks, check{Name: "helm repositories", Level: levelWarn, Detail: err.Error()})
	case len(repos) == 0:
		checks = append(checks, check{Name: "helm repositories", Level: levelWarn,
			Detail: "none configured",
			Hint:   "add one with `helm repo add <name> <url>` before using flow cluster"})
	default:
		names := make([]string, 0, len(repos))
		for _, r := range repos {
			names = append(names, r.Name)
		}
		checks = append(checks, check{Name: "helm repositories", Level: levelPass,
			Detail: strings.Join(names, ", ")})
	}
	return checks
}

func (a *App) checkKube(ctx context.Context) []check {
	k := a.Kube()
	if !k.Available() {
		return []check{{Name: "kubectl", Level: levelWarn, Detail: "not on PATH",
			Hint: "install kubectl; a shell alias does not count, flow needs the real binary"}}
	}

	current, err := k.CurrentContext(ctx)
	if err != nil {
		return []check{{Name: "kubectl", Level: levelWarn, Detail: "no current context",
			Hint: "select one with `kubectl config use-context <name>`"}}
	}
	checks := []check{{Name: "kube context", Level: levelPass, Detail: current}}

	if err := k.Reachable(ctx, ""); err != nil {
		checks = append(checks, check{Name: "cluster reachable", Level: levelWarn,
			Detail: "could not reach the API server for " + current,
			Hint:   "start the cluster, or switch context"})
	} else {
		checks = append(checks, check{Name: "cluster reachable", Level: levelPass, Detail: current})
	}

	if _, known := a.cfg.Cluster(current); !known {
		checks = append(checks, check{Name: "cluster configured", Level: levelWarn,
			Detail: fmt.Sprintf("kube context %q has no clusters entry", current),
			Hint:   "run `flow cluster status` once to set it up interactively"})
	} else {
		checks = append(checks, check{Name: "cluster configured", Level: levelPass, Detail: current})
	}
	return checks
}

func (a *App) checkGitHubToken(ctx context.Context) check {
	token, source := a.Token(ctx)
	if token == "" {
		return check{Name: "GitHub token", Level: levelWarn, Detail: "not found",
			Hint: "store one with: " + secrets.KeychainHint(a.cfg.GitHub.TokenKeychainService) +
				"  (flow degrades gracefully without it: PR state shows as \"?\")"}
	}

	login, err := a.GitHub(ctx).AuthenticatedLogin(ctx)
	if err != nil {
		return check{Name: "GitHub token", Level: levelWarn,
			Detail: fmt.Sprintf("found in %s, but the API rejected it: %v", source, err),
			Hint:   "replace it with: " + secrets.KeychainHint(a.cfg.GitHub.TokenKeychainService)}
	}
	// The login, never the token.
	return check{Name: "GitHub token", Level: levelPass,
		Detail: fmt.Sprintf("%s, authenticated as %s", source, login)}
}

func (a *App) checkConfig() []check {
	detail := a.cfgPath
	if !a.cfgFound {
		detail += " (not present; built-in defaults are in use)"
	}
	checks := []check{{Name: "config file", Level: levelPass, Detail: detail}}

	assetsRoot := a.cfg.AssetsRoot()
	if err := writable(assetsRoot); err != nil {
		checks = append(checks, check{Name: "assets root", Level: levelWarn,
			Detail: fmt.Sprintf("%s is not writable: %v", assetsRoot, err),
			Hint:   "create it, or change assets.root in " + a.cfgPath})
	} else {
		checks = append(checks, check{Name: "assets root", Level: levelPass, Detail: assetsRoot})
	}

	for ctxName, cc := range a.cfg.Clusters {
		if cc.ValuesFile == "" {
			continue
		}
		expanded := config.ExpandPath(cc.ValuesFile)
		if fileExists(expanded) {
			continue
		}
		checks = append(checks, check{
			Name:   "values file for " + ctxName,
			Level:  levelWarn,
			Detail: expanded + " does not exist",
			Hint:   "create it, or update clusters." + ctxName + ".values_file",
		})
	}
	return checks
}

// writable reports whether a directory exists and can be written to, creating
// nothing.
func writable(dir string) error {
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		// An absent assets root is fine as long as its parent is writable;
		// flow creates it on demand.
		return writable(filepath.Dir(dir))
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory")
	}

	probe, err := os.CreateTemp(dir, ".flow-doctor-*")
	if err != nil {
		return err
	}
	name := probe.Name()
	_ = probe.Close()
	return os.Remove(name)
}

func (a *App) checkRegistry() check {
	path := a.Registry.Path()
	file, err := a.Registry.Load()
	if err != nil {
		return check{Name: "registry", Level: levelFail, Detail: err.Error(), Hint: "inspect " + path}
	}

	var drifted int
	for _, e := range file.Entries {
		if !worktreeExists(e.WorktreePath) {
			drifted++
		}
	}
	if drifted > 0 {
		return check{Name: "registry", Level: levelWarn,
			Detail: fmt.Sprintf("%d of %d entries point at a missing worktree",
				drifted, len(file.Entries)),
			Hint: "run `flow prune`"}
	}
	return check{Name: "registry", Level: levelPass,
		Detail: fmt.Sprintf("%s, %d entr%s", path, len(file.Entries), plural(len(file.Entries)))}
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

func (a *App) renderChecks(checks []check) {
	t := a.Out.Theme
	rows := make([][]string, 0, len(checks))
	for _, c := range checks {
		var level string
		switch c.Level {
		case levelPass:
			level = t.Success.Render(string(c.Level))
		case levelWarn:
			level = t.Warn.Render(string(c.Level))
		default:
			level = t.Error.Render(string(c.Level))
		}
		rows = append(rows, []string{level, c.Name, c.Detail})
	}
	a.Out.Table([]string{"", "CHECK", "DETAIL"}, rows)

	for _, c := range checks {
		if c.Level != levelPass && c.Hint != "" {
			a.Out.Status("  %s %s: %s", t.Muted.Render("→"), c.Name, c.Hint)
		}
	}
}
