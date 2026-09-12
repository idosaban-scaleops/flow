package cli

import (
	"context"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Build information, stamped by the linker via -ldflags and copied in from
// main before fang runs, so that `flow --version` and `flow version --json`
// always report the same values.
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// SetBuildInfo records the linker-stamped values. main calls this before
// fang.Execute so fang's --version output and flow version agree.
func SetBuildInfo(version, commit, date string) {
	if version != "" {
		Version = version
	}
	if commit != "" {
		Commit = commit
	}
	if date != "" {
		Date = date
	}
	fillFromBuildInfo()
}

// fillFromBuildInfo backfills from the module's embedded VCS stamps, which is
// what a plain `go install` produces when no ldflags were given.
func fillFromBuildInfo() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if Version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		Version = info.Main.Version
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if Commit == "" {
				Commit = setting.Value
			}
		case "vcs.time":
			if Date == "" {
				Date = setting.Value
			}
		}
	}
}

func newVersionCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Long: "Print version information.\n\n" +
			"`flow --version` prints the same values; this command exists to provide " +
			"--json output for scripts.",
		Args: cobra.NoArgs,
	}
	cmd.RunE = app.run(func(_ context.Context) error {
		payload := map[string]string{
			"version":    Version,
			"commit":     Commit,
			"date":       Date,
			"go_version": runtime.Version(),
			"platform":   runtime.GOOS + "/" + runtime.GOARCH,
		}
		if app.JSON() {
			return app.Out.JSON(payload)
		}

		app.Out.Line("flow " + Version)
		if Commit != "" {
			app.Out.Field("commit", Commit)
		}
		if Date != "" {
			app.Out.Field("built", Date)
		}
		app.Out.Field("go", runtime.Version())
		app.Out.Field("platform", payload["platform"])
		return nil
	})
	return cmd
}

// completionsHelp is attached to cobra's completion command; the user is on
// zsh, so those instructions come first.
const completionsHelp = `Generate a shell completion script.

flow registers "completions" as an alias, so both spellings work.

zsh:

    # Once, if completion is not already enabled:
    echo "autoload -U compinit; compinit" >> ~/.zshrc

    # Then, to install flow's completions:
    flow completions zsh > "${fpath[1]}/_flow"

    # Or, for a Homebrew-installed flow, this is done for you by the formula.

bash:

    flow completions bash > /usr/local/etc/bash_completion.d/flow

fish:

    flow completions fish > ~/.config/fish/completions/flow.fish

Ticket arguments complete from the local registry, current repository first.`
