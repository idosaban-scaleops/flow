package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/idosaban-scaleops/flow/internal/ghapi"
	"github.com/idosaban-scaleops/flow/internal/gitx"
	"github.com/idosaban-scaleops/flow/internal/herdr"
	"github.com/idosaban-scaleops/flow/internal/output"
)

type statusPayload struct {
	Ticket         string       `json:"ticket"`
	Slug           string       `json:"slug"`
	RepoKey        string       `json:"repo_key"`
	RepoRoot       string       `json:"repo_root"`
	Branch         string       `json:"branch"`
	BaseBranch     string       `json:"base_branch"`
	WorktreePath   string       `json:"worktree_path"`
	WorktreeExists bool         `json:"worktree_exists"`
	AssetsPath     string       `json:"assets_path"`
	AssetsExists   bool         `json:"assets_exists"`
	Workspace      *workspaceRO `json:"workspace"`
	Dirty          bool         `json:"dirty"`
	ChangedFiles   int          `json:"changed_files"`
	HasUpstream    bool         `json:"has_upstream"`
	Ahead          int          `json:"ahead"`
	Behind         int          `json:"behind"`
	PR             ghapi.PRInfo `json:"pull_request"`
	CI             *ciSummary   `json:"latest_ci_run"`
	CreatedAt      time.Time    `json:"created_at"`
	LastOpenedAt   time.Time    `json:"last_opened_at,omitempty"`
}

type workspaceRO struct {
	ID    string `json:"id,omitempty"`
	Label string `json:"label,omitempty"`
	Known bool   `json:"known_to_herdr"`
}

type ciSummary struct {
	RunID      int64  `json:"run_id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion,omitempty"`
	URL        string `json:"url,omitempty"`
}

func newStatusCommand(app *App) *cobra.Command {
	var noRemote bool

	cmd := &cobra.Command{
		Use:   "status [ticket]",
		Short: "Show everything flow knows about one ticket",
		Args:  cobra.MaximumNArgs(1),
		Long: "Show one ticket's repository, branch, worktree, assets folder, workspace,\n" +
			"working-tree cleanliness, ahead/behind counts, pull request and latest CI run.\n\n" +
			"A ticket with no pull request, or a branch with no CI runs yet, is a normal\n" +
			"state and is reported as such.",
		ValidArgsFunction: app.completeTickets,
	}
	cmd.Flags().BoolVar(&noRemote, "no-remote", false, "skip all GitHub calls")

	cmd.RunE = app.runArgs(func(ctx context.Context, args []string) error {
		got, err := app.resolveTicket(ctx, first(args))
		if err != nil {
			return err
		}
		payload := app.buildStatus(ctx, got, noRemote)

		if app.JSON() {
			return app.Out.JSON(payload)
		}
		app.renderStatus(ctx, got, payload)
		return nil
	})
	return cmd
}

func (a *App) buildStatus(ctx context.Context, got resolved, noRemote bool) statusPayload {
	e := got.Entry
	p := statusPayload{
		Ticket:         e.TicketID,
		Slug:           e.Slug,
		RepoKey:        e.RepoKey,
		RepoRoot:       e.RepoRoot,
		Branch:         e.Branch,
		BaseBranch:     e.BaseBranch,
		WorktreePath:   e.WorktreePath,
		WorktreeExists: worktreeExists(e.WorktreePath),
		AssetsPath:     e.AssetsPath,
		AssetsExists:   gitx.DirExists(e.AssetsPath),
		CreatedAt:      e.CreatedAt,
		LastOpenedAt:   e.LastOpenedAt,
		PR:             ghapi.PRInfo{State: ghapi.PRUnknown, Reason: "not checked"},
	}

	if p.WorktreeExists {
		if st, err := a.ReadGit(e.WorktreePath).StatusOf(ctx); err == nil {
			p.Dirty, p.ChangedFiles = st.Dirty, st.ChangedFiles
			p.HasUpstream, p.Ahead, p.Behind = st.HasUpstream, st.Ahead, st.Behind
		}
	}

	if !e.Workspace.Empty() {
		ws := &workspaceRO{ID: e.Workspace.ID, Label: e.Workspace.Label}
		if client := a.ReadHerdr(); client != nil {
			if list, err := client.ListWorkspaces(ctx); err == nil {
				_, byID := herdr.FindByID(list, e.Workspace.ID)
				_, byLabel := herdr.FindByLabel(list, e.Workspace.Label)
				ws.Known = byID || byLabel
			}
		}
		p.Workspace = ws
	}

	if noRemote {
		return p
	}
	owner, name, ok := splitRepoKey(e.RepoKey)
	if !ok {
		p.PR.Reason = "no GitHub remote"
		return p
	}

	gh := a.GitHub(ctx)
	info, err := gh.FindPR(ctx, owner, name, e.Branch)
	if err != nil && info.Reason == "" {
		info.Reason = ghapi.Describe(err)
	}
	p.PR = info

	workflow := a.cfg.Repo(e.RepoKey).BuildWorkflow
	if workflow == "" {
		return p
	}
	runs, err := gh.ListWorkflowRuns(ctx, owner, name, workflow, e.Branch, 1)
	if err == nil && len(runs) > 0 {
		p.CI = &ciSummary{
			RunID:      runs[0].ID,
			Status:     string(runs[0].Status),
			Conclusion: string(runs[0].Conclusion),
			URL:        runs[0].HTMLURL,
		}
	}
	return p
}

func (a *App) renderStatus(_ context.Context, got resolved, p statusPayload) {
	t := a.Out.Theme
	out := a.Out

	out.Println()
	out.Field("ticket", t.Ticket.Render(p.Ticket))
	out.Field("slug", p.Slug)
	if p.RepoKey != "" {
		out.Field("repo", p.RepoKey)
	}
	out.Field("branch", p.Branch)
	if p.BaseBranch != "" {
		out.Field("base branch", p.BaseBranch)
	}
	if !got.Registered {
		out.Field("registered", t.Warn.Render("no — inferred from the directory layout"))
	}

	out.Field("worktree", pathWithExistence(t, p.WorktreePath, p.WorktreeExists))
	out.Field("assets", pathWithExistence(t, p.AssetsPath, p.AssetsExists))

	switch {
	case p.Workspace == nil:
		out.Field("workspace", t.Muted.Render("none"))
	case p.Workspace.Known:
		out.Field("workspace", fmt.Sprintf("%s (%s)", p.Workspace.ID, p.Workspace.Label))
	default:
		out.Field("workspace", t.Warn.Render(fmt.Sprintf("%s — herdr no longer knows about it",
			orDash(p.Workspace.Label, p.Workspace.ID))))
	}

	if p.WorktreeExists {
		out.Field("working tree", a.styleStatus(cleanliness(p)))
		switch {
		case !p.HasUpstream:
			out.Field("upstream", t.Muted.Render("none — the branch has never been pushed"))
		default:
			out.Field("upstream", fmt.Sprintf("ahead %d, behind %d", p.Ahead, p.Behind))
		}
	}

	out.Field("pull request", a.describePR(p, got))
	out.Field("latest CI run", a.describeCI(p))
	out.Println()
}

func cleanliness(p statusPayload) string {
	if !p.Dirty {
		return "clean"
	}
	return fmt.Sprintf("dirty (%d changed)", p.ChangedFiles)
}

// describePR spells out all three states distinctly: a missing pull request and
// an undeterminable one must never read the same.
func (a *App) describePR(p statusPayload, got resolved) string {
	t := a.Out.Theme
	switch p.PR.State {
	case ghapi.PRNone:
		owner, name, ok := splitRepoKey(p.RepoKey)
		if !ok {
			return t.Muted.Render("none")
		}
		base := p.BaseBranch
		if base == "" {
			base = a.cfg.BaseBranchFor(got.Entry.RepoKey)
		}
		return t.Muted.Render("none") + "  " + t.Path.Render(fmt.Sprintf(
			"https://github.com/%s/%s/compare/%s...%s?expand=1", owner, name, base, p.Branch))
	case ghapi.PRUnknown:
		return t.Warn.Render(fmt.Sprintf("unknown (%s)", orDash(p.PR.Reason, "no reason given")))
	case ghapi.PRMerged:
		merged := ""
		if p.PR.MergedAt != nil {
			merged = ", merged " + p.PR.MergedAt.Local().Format("2006-01-02 15:04")
		}
		return t.Success.Render(fmt.Sprintf("#%d merged%s", p.PR.Number, merged)) + "  " +
			t.Path.Render(p.PR.URL)
	case ghapi.PROpen, ghapi.PRClosed:
		return fmt.Sprintf("#%d %s", p.PR.Number, p.PR.State) + "  " + t.Path.Render(p.PR.URL)
	default:
		// A state this build does not know about renders like PRUnknown rather
		// than like "none".
		return t.Warn.Render(fmt.Sprintf("unknown (%s)", p.PR.State))
	}
}

func (a *App) describeCI(p statusPayload) string {
	t := a.Out.Theme
	if p.CI == nil {
		return t.Muted.Render("no CI runs for this branch")
	}
	state := p.CI.Status
	if p.CI.Conclusion != "" {
		state += " (" + p.CI.Conclusion + ")"
	}
	return fmt.Sprintf("%s  run %d  %s", state, p.CI.RunID, t.Path.Render(p.CI.URL))
}

func pathWithExistence(t output.Theme, path string, exists bool) string {
	if path == "" {
		return t.Muted.Render("none")
	}
	if exists {
		return t.Path.Render(path)
	}
	return t.Path.Render(path) + t.Warn.Render("  (missing)")
}

func orDash(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return "—"
}
