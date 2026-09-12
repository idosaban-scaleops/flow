// Command flow is a personal workflow CLI for ticket-driven development.
//
// This file stays thin on purpose: it stamps build information, decides the
// color scheme before fang parses anything, hands the command tree to fang, and
// maps the resulting error onto flow's exit-code scheme. No command handler
// ever calls os.Exit.
package main

import (
	"context"
	"os"
	"strings"

	"charm.land/fang/v2"

	"github.com/idosaban-scaleops/flow/internal/cli"
	"github.com/idosaban-scaleops/flow/internal/output"
)

// Stamped by the linker via -ldflags; see the Makefile and .goreleaser.yaml.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

func main() {
	cli.SetBuildInfo(version, commit, date)

	root := cli.NewRootCommand()

	options := []fang.Option{
		fang.WithVersion(cli.Version),
		fang.WithCommit(cli.Commit),
		fang.WithErrorHandler(output.FangErrorHandler(cli.Aborted, cli.ErrorAlreadyReported)),
		fang.WithColorSchemeFunc(output.FangColorScheme(plainOutput())),
	}

	if err := fang.Execute(context.Background(), root, options...); err != nil {
		os.Exit(cli.ExitCodeFor(err))
	}
}

// plainOutput decides whether fang should style its help and error pages.
//
// fang owns flag parsing, so the root command's PersistentPreRunE has not run
// when fang decides how to render. Scanning argv here is what keeps fang, the
// command output, huh and the logger agreeing about whether color is on.
func plainOutput() bool {
	if os.Getenv("NO_COLOR") != "" {
		return true
	}
	for _, arg := range os.Args[1:] {
		if arg == "--" {
			return false
		}
		name, _, _ := strings.Cut(arg, "=")
		switch name {
		case "--no-color", "--json":
			return true
		}
	}
	return false
}
