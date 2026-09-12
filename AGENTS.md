# AGENTS.md

Working notes for coding agents in this repository. The [README](README.md)
explains what `flow` does for its users; this file covers what you need to know
to change it without breaking something. Read both.

## Commands

```sh
make build              # build into ./bin
make test               # unit tests, -race -short
make test-integration   # everything, including tests that drive a real git
make lint               # golangci-lint over all build tags
make fmt                # format and organize imports
make golden             # regenerate golden files after an intentional change
```

Run `make lint test-integration` before declaring any change complete. Both must
be clean; `make test` alone skips the build-tagged tests that exercise real git.

Toolchain versions are pinned in `.tool-versions`, which asdf, CI and the
GitHub Actions all read: Go 1.26.3 (1.26+ is required by go-github v91),
`golangci-lint` 2.12.2 and `goreleaser` 2.18.1. `go.mod` keeps its own
`go 1.26.0` line — that is the language floor for anyone installing flow as a
module, not the toolchain this repo builds with. `.golangci.yaml` uses the **v2
config schema** (`version: "2"`); the v1 schema fails at the config-parse stage
with a confusing error.

## Four rules that are enforced, not conventions

`internal/architecture_test.go` parses every file in the module and fails the
build if any of these is violated. If you find yourself wanting to add an entry
to one of its allowlists, that is the signal to reconsider the design instead.

1. **No package outside `internal/exec` may import `os/exec`.** Every external
   process goes through `exec.Runner`. This is what lets the tests assert the
   exact argv `flow` produces without a real git, herdr, helm, or cluster.
2. **No package outside `internal/ghapi` may import `go-github`.** The import
   path carries the major version, so a bump would otherwise ripple through
   every file naming one of its types. Above `ghapi`, mock the `ghapi.API`
   interface — never go-github itself.
3. **Only `internal/output`, `internal/ciwait` and `cmd/flow` may import a Charm
   library.** Commands ask the renderer for output; they never construct a
   style. This keeps a theme change, or a future TUI, a single-file change.
4. **All Charm dependencies stay on the v2 generation** — the `charm.land/`
   vanity paths. Never add a `github.com/charmbracelet/…` v1 package alongside
   them: the `tea.Model` and style types do not line up and color-profile
   handling differs. Two `github.com/charmbracelet/` packages are legitimate
   because they have no `charm.land/` path and the v2 line itself depends on
   them — `colorprofile` and `x/term`, both used only in `internal/output`.
   Rule 3 still applies to them.

## Layout

| Package | Responsibility |
|---|---|
| `cmd/flow` | Stamps build info, decides the color scheme, hands off to fang, maps errors to exit codes. Stays thin. |
| `internal/cli` | One file per command; `root.go` holds globals and the shared `App`. |
| `internal/exec` | The only gateway to `os/exec`. Real runner, fake, dry-run wrapper. |
| `internal/output` | Every byte that reaches a terminal. Theme, renderer, prompts, tables. |
| `internal/ciwait` | CI wait: a **pure state machine** plus a **separate renderer**. |
| `internal/ghapi` | go-github behind flow's own types. |
| `internal/config`, `internal/registry` | Configuration and the local state store. |
| `internal/gitx`, `internal/herdr`, `internal/helmx`, `internal/kube`, `internal/editor` | External tool wrappers, all built on `exec.Runner`. |
| `internal/ticket`, `internal/chartver`, `internal/secrets` | Pure logic. |

## Writing a command

Add one file to `internal/cli`, register it in `NewRootCommand`, and use the
`App` helpers rather than reaching for the filesystem or a prompt directly:

- `app.run(fn)` / `app.runArgs(fn)` wrap the body so errors are rendered once,
  in the right format — a JSON envelope on stdout under `--json`, fang's styled
  error otherwise.
- **Never call `os.Exit` from a handler.** Return one of `Usage`, `NotFound`,
  `Precondition`, `Dependency`, `Failure`, or `Wrap`; `main` maps it through
  `ExitCodeFor`.
- **`app.mutate(description, fn)` for every filesystem write that does not go
  through `exec.Runner`** — `MkdirAll`, the registry, the config file. The
  dry-run wrapper only covers subprocesses, so a write that skips `mutate`
  silently escapes `--dry-run`. This is the easiest rule to forget.
- **`app.confirm` vs `app.confirmDestructive`.** `--json` implies yes for
  ordinary prompts but never for destructive ones, where the user must pass
  `--yes` explicitly. Use the destructive variant in anything that deletes,
  uninstalls, or rolls back.
- `app.Git(dir)` mutates and is gated by `--dry-run`; `app.ReadGit(dir)` is for
  inspection and always runs. The same split exists for helm (`Helm` /
  `ReadHelm`). A dry run that cannot inspect the repository cannot describe what
  it would do.
- Commands taking an optional `[ticket]` should set
  `ValidArgsFunction: app.completeTickets` and resolve via
  `app.resolveTicket(ctx, first(args))`, which handles cwd inference.

### Error message wording

fang capitalizes the first character of an error. A message starting with an
identifier comes out mangled — `RD-99999 is not registered` renders as
`Rd-99999 …`. Lead with a word: `ticket %s is not registered`.

## Domain invariants

- **Pull request state is three-valued.** `PRNone` ("there is no PR") and
  `PRUnknown` ("I could not find out") must never collapse into the same branch
  of a conditional, because the wording a user sees and the right action differ.
  No command may *require* a PR to exist.
- **The registry is a cache of facts about the world, not the source of truth.**
  Every reader must tolerate drift — a worktree deleted by hand, a closed
  workspace, a renamed branch — and never crash because of it.
- **`flow init` is idempotent and adoptive.** It adopts an existing worktree
  untouched, focuses rather than duplicates a workspace, and refuses in exactly
  one case: a non-empty directory that is not a git worktree.
- **The `clusters` config map is learn-on-first-use, not a closed allowlist.**
  An unknown kube context is never acted on silently, but interactively
  `learnCluster` prompts for the settings and writes the entry, so any context
  can be adopted. Only a non-interactive session refuses outright. Per-invocation
  overrides (`--release`, `--namespace`, `--helm-repo`, `--chart`) are applied
  *after* the entry is resolved, so they cannot stand in for one. To target a
  different cluster, pass `--kube-context` to helm; never mutate the user's
  kubeconfig.
- **Chart versions order by trailing run ID, never by semver.** Semver compares
  the pre-release suffix alphanumerically and sorts `…-9` above `…-10`.
- **A `skipped` CI job counts as satisfied.** The GPU and FIPS legs are always
  skipped on a default dev build; waiting for them would hang forever.
- **The CI progress percentage is monotonic.** The only sanctioned way back to
  zero is an explicit retarget to a different run.
- **Never log the token.** `exec.Opts.Secret` redacts both argv and output under
  `-vv`; the keychain lookup sets it. `doctor` reports the authenticated login,
  never the credential.

## Testing

- **The argv assertions are the highest-value surface in the project.** When you
  change how a wrapper invokes a tool, assert the exact command line with
  `exec.Fake`. Its response prefixes match both the plain and the shell-quoted
  rendering, longest prefix first, so `f.Respond("git log --format=%h %s", …)`
  reads naturally.
- `ciwait`'s state machine is table-tested with no network and no terminal; add
  cases there rather than to the renderer.
- Golden files live in `internal/{ciwait,output}/testdata`. Regenerate with
  `make golden`, and **read the diff** — a golden test that is updated without
  being looked at is worse than no test.
- Tests needing a real `git` go behind `//go:build integration` and skip under
  `-short`.
- Per the spec, **do not write tests for cobra wiring.** `App` has no runner
  seam by design; that surface is covered by the integration tests in
  `internal/cli/cli_integration_test.go`.

## Do not

- **Do not run `flow cluster upgrade`, `wait`, or `uninstall` against a real
  cluster while iterating**, and do not create real herdr workspaces. This
  machine has a live kube context, a real `helm`, and a real `herdr`. Exercise
  those paths through `exec.Fake`.
- Do not run bare `herdr` — it launches the TUI, even as a capability probe. Use
  `herdr --version` or `herdr workspace list`.
- Do not add behaviour from the non-goals list in the README (closing editor
  windows or workspaces, deleting assets, bootstrap commands in new worktrees,
  a Jira API integration, `helm upgrade --install` by default).
- Do not rely on `git rev-parse --show-toplevel`: inside a linked worktree it
  returns the worktree, not the repository. Use `gitx.DiscoverRepo`.
- Do not compare paths with `==`. macOS resolves `/tmp` and `/var` through
  symlinks and git always reports the resolved form; use `gitx.SamePath`.
