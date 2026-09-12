package cli

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/idosaban-scaleops/flow/internal/ghapi"
	"github.com/idosaban-scaleops/flow/internal/gitx"
	"github.com/idosaban-scaleops/flow/internal/registry"
)

// listWorkers bounds the concurrency used to gather live status, so a dozen
// tickets do not mean a dozen simultaneous git and GitHub calls.
const listWorkers = 8

// listTimeout caps the whole gathering phase; a slow row degrades to "?" rather
// than hanging the command.
const listTimeout = 20 * time.Second

type listRow struct {
	Ticket   string        `json:"ticket"`
	Slug     string        `json:"slug"`
	Branch   string        `json:"branch"`
	RepoKey  string        `json:"repo_key"`
	Status   string        `json:"status"`
	PR       ghapi.PRInfo  `json:"pr"`
	Age      string        `json:"age"`
	AgeSince time.Duration `json:"-"`
	Path     string        `json:"worktree_path"`
	Exists   bool          `json:"worktree_exists"`
}

func newListCommand(app *App) *cobra.Command {
	var (
		all      bool
		noRemote bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List tickets flow is managing",
		Long: "List the tickets in the registry, scoped to the current repository.\n\n" +
			"Status and pull-request state are gathered live and concurrently; a row\n" +
			"that cannot be determined degrades to \"?\" rather than failing the command.",
		Aliases: []string{"ls"},
		Args:    cobra.NoArgs,
	}

	f := cmd.Flags()
	f.BoolVar(&all, "all", false, "list tickets from every repository")
	f.BoolVar(&noRemote, "no-remote", false, "skip all GitHub calls")

	cmd.RunE = app.run(func(ctx context.Context) error {
		file, err := app.Registry.Load()
		if err != nil {
			return Wrap(ExitFailure, "registry", err, "reading the registry")
		}

		entries := file.All()
		var repo gitx.Repo
		if !all {
			dir, err := cwd()
			if err != nil {
				return err
			}
			repo, err = gitx.DiscoverRepo(ctx, app.baseRunner, dir, app.cfg.Defaults.Remote)
			if err != nil {
				return Wrap(ExitDependency, "dependency", err,
					"listing is scoped to the current repository; pass --all to list every repo")
			}
			entries = file.ForRepo(repo.Key)
		}

		if len(entries) == 0 {
			if app.JSON() {
				return app.Out.JSON(map[string]any{"tickets": []listRow{}})
			}
			app.Out.Status("no tickets registered%s", scopeSuffix(all, repo.Key))
			return nil
		}

		rows := app.gatherRows(ctx, entries, noRemote)

		if app.JSON() {
			return app.Out.JSON(map[string]any{"tickets": rows})
		}
		app.renderList(rows, all)
		return nil
	})
	return cmd
}

func scopeSuffix(all bool, repoKey string) string {
	if all || repoKey == "" {
		return ""
	}
	return " for " + repoKey
}

// gatherRows computes live status for every entry with a bounded worker pool.
func (a *App) gatherRows(ctx context.Context, entries []registry.Entry, noRemote bool) []listRow {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()

	gh := a.GitHub(ctx)
	rows := make([]listRow, len(entries))

	sem := make(chan struct{}, listWorkers)
	var wg sync.WaitGroup

	for i, entry := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rows[i] = a.buildRow(ctx, entry, gh, noRemote)
		}()
	}
	wg.Wait()
	return rows
}

func (a *App) buildRow(ctx context.Context, entry registry.Entry, gh ghapi.API, noRemote bool) listRow {
	row := listRow{
		Ticket:  entry.TicketID,
		Slug:    entry.Slug,
		Branch:  entry.Branch,
		RepoKey: entry.RepoKey,
		Path:    entry.WorktreePath,
		Exists:  worktreeExists(entry.WorktreePath),
		PR:      ghapi.PRInfo{State: ghapi.PRUnknown, Reason: "not checked"},
	}
	if !entry.CreatedAt.IsZero() {
		row.AgeSince = time.Since(entry.CreatedAt)
		row.Age = humanAge(row.AgeSince)
	}

	row.Status = a.worktreeStatus(ctx, entry, row.Exists)

	if noRemote {
		return row
	}
	owner, name, ok := splitRepoKey(entry.RepoKey)
	if !ok {
		row.PR.Reason = "no GitHub remote"
		return row
	}
	info, err := gh.FindPR(ctx, owner, name, entry.Branch)
	if err != nil && info.State == ghapi.PRUnknown && info.Reason == "" {
		info.Reason = ghapi.Describe(err)
	}
	row.PR = info
	return row
}

func (a *App) worktreeStatus(ctx context.Context, entry registry.Entry, exists bool) string {
	if !exists {
		return "gone"
	}
	st, err := a.ReadGit(entry.WorktreePath).StatusOf(ctx)
	if err != nil {
		return "?"
	}

	var parts []string
	if st.Dirty {
		parts = append(parts, "dirty")
	}
	if st.Ahead > 0 {
		parts = append(parts, fmt.Sprintf("ahead %d", st.Ahead))
	}
	if st.Behind > 0 {
		parts = append(parts, fmt.Sprintf("behind %d", st.Behind))
	}
	if len(parts) == 0 {
		return "clean"
	}
	return strings.Join(parts, " ")
}

func (a *App) renderList(rows []listRow, all bool) {
	headers := []string{"TICKET", "SLUG", "BRANCH", "STATUS", "PR", "AGE"}
	if all {
		headers = append([]string{"REPO"}, headers...)
	}

	t := a.Out.Theme
	data := make([][]string, 0, len(rows))
	for _, row := range rows {
		cells := []string{
			t.Ticket.Render(row.Ticket),
			row.Slug,
			row.Branch,
			a.styleStatus(row.Status),
			a.stylePR(row.PR),
			t.Muted.Render(row.Age),
		}
		if all {
			cells = append([]string{t.Muted.Render(row.RepoKey)}, cells...)
		}
		data = append(data, cells)
	}
	a.Out.Table(headers, data)
}

func (a *App) styleStatus(status string) string {
	t := a.Out.Theme
	switch status {
	case "clean":
		return t.Success.Render(status)
	case "gone":
		return t.Error.Render(status)
	case "?":
		return t.Muted.Render(status)
	default:
		return t.Warn.Render(status)
	}
}

// stylePR renders the three-valued pull-request state. "—" means there is no
// pull request; "?" means flow could not find out. They are never the same cell.
func (a *App) stylePR(info ghapi.PRInfo) string {
	t := a.Out.Theme
	switch info.State {
	case ghapi.PRNone:
		return t.Muted.Render("—")
	case ghapi.PRUnknown:
		return t.Muted.Render("?")
	case ghapi.PRMerged:
		return t.Success.Render(fmt.Sprintf("#%d merged", info.Number))
	case ghapi.PROpen:
		return t.Warn.Render(fmt.Sprintf("#%d open", info.Number))
	case ghapi.PRClosed:
		return t.Muted.Render(fmt.Sprintf("#%d closed", info.Number))
	default:
		return t.Muted.Render("?")
	}
}

// humanAge renders a duration the way a list column wants it.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 14*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dw", int(d.Hours()/24/7))
	}
}
