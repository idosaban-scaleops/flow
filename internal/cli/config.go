package cli

import (
	"context"
	"os"

	"github.com/spf13/cobra"

	"github.com/idosaban-scaleops/flow/internal/config"
	flowexec "github.com/idosaban-scaleops/flow/internal/exec"
)

func newConfigCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect and edit flow's configuration",
		Long: "flow works with no configuration at all. These commands write and show\n" +
			"the optional config file.",
	}
	cmd.AddCommand(
		newConfigInitCommand(app),
		newConfigPathCommand(app),
		newConfigShowCommand(app),
		newConfigEditCommand(app),
	)
	return cmd
}

func newConfigInitCommand(app *App) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:         "init",
		Annotations: map[string]string{annotationAllowMissingConfig: "true"},
		Short:       "Write a fully-commented default config file",
		Args:        cobra.NoArgs,
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing config file")

	cmd.RunE = app.run(func(_ context.Context) error {
		path := app.cfgPath

		if fileExists(path) && !force {
			return Precondition("the config file %s already exists; pass --force to overwrite it", path)
		}
		if err := app.mutate("write "+path, func() error {
			return config.WriteFileAtomic(path, []byte(config.Template), 0o600)
		}); err != nil {
			return Wrap(ExitFailure, "failure", err, "writing %s", path)
		}

		if app.JSON() {
			return app.Out.JSON(map[string]string{"path": path})
		}
		app.Out.Success("wrote %s", path)
		return nil
	})
	return cmd
}

func newConfigPathCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "path",
		Annotations: map[string]string{annotationAllowMissingConfig: "true"},
		Short:       "Print the resolved config file path",
		Args:        cobra.NoArgs,
	}
	cmd.RunE = app.run(func(_ context.Context) error {
		if app.JSON() {
			return app.Out.JSON(map[string]any{"path": app.cfgPath, "exists": app.cfgFound})
		}
		app.Out.Line(app.cfgPath)
		return nil
	})
	return cmd
}

func newConfigShowCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the effective merged configuration",
		Long: "Print the configuration flow is actually using, after merging defaults,\n" +
			"the config file and FLOW_ environment variables.",
		Args: cobra.NoArgs,
	}
	cmd.RunE = app.run(func(_ context.Context) error {
		if app.JSON() {
			return app.Out.JSON(app.cfg)
		}
		rendered, err := config.Marshal(app.cfg)
		if err != nil {
			return Wrap(ExitFailure, "failure", err, "rendering the configuration")
		}
		app.Out.Println(rendered)
		return nil
	})
	return cmd
}

func newConfigEditCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "edit",
		Annotations: map[string]string{annotationAllowMissingConfig: "true"},
		Short:       "Open the config file in $EDITOR",
		Args:        cobra.NoArgs,
	}
	cmd.RunE = app.run(func(ctx context.Context) error {
		path := app.cfgPath
		if !fileExists(path) {
			if err := app.mutate("write "+path, func() error {
				return config.WriteFileAtomic(path, []byte(config.Template), 0o600)
			}); err != nil {
				return Wrap(ExitFailure, "failure", err, "creating %s", path)
			}
			app.Out.Status("created %s", path)
		}

		edit := os.Getenv("EDITOR")
		if edit == "" {
			edit = os.Getenv("VISUAL")
		}
		if edit == "" {
			return Precondition("$EDITOR is not set; the config file is at %s", path)
		}

		// Stream so an interactive editor owns the terminal.
		if _, err := app.Runner.Run(ctx, flowexec.Opts{
			Name: edit, Args: []string{path}, Stream: true, Stdin: os.Stdin,
		}); err != nil {
			return Wrap(ExitDependency, "dependency", err, "running %s", edit)
		}
		return nil
	})
	return cmd
}
