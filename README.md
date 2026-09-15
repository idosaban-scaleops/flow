# flow

A personal command-line tool for driving the lifecycle of a Jira ticket: create a
git worktree, spin up a terminal workspace, open an editor, manage a per-ticket
assets folder, tear it all down when the PR merges, and upgrade a local
Kubernetes dev cluster with the Helm chart CI built from the ticket's branch.

It is a **toolbox, not a workflow engine**. There is no DAG, no resumable state
machine, no scheduler. Each subcommand does one well-defined job, and a small
local registry remembers what `flow` created so it can find it again later.

> **Charm generation:** this project is built against the **v2 generation** of the
> Charm stack — the `charm.land/` vanity import paths (`charm.land/fang/v2`,
> `charm.land/lipgloss/v2`, `charm.land/bubbletea/v2`, `charm.land/bubbles/v2`,
> `charm.land/huh/v2`, `charm.land/log/v2`). Do not mix in a `github.com/charmbracelet/…`
> v1 package: the `tea.Model` and style types no longer line up and color-profile
> handling differs.

## Install

```sh
go install github.com/idosaban-scaleops/flow/cmd/flow@latest
```

or from the Homebrew tap, which also installs man pages and shell completions:

```sh
brew install idosaban-scaleops/tap/flow
```

Use the fully qualified name to upgrade too — Homebrew core has an unrelated
`flow-desktop` cask, and a bare `brew upgrade flow` matches that instead:

```sh
brew upgrade idosaban-scaleops/tap/flow
```

Requires Go 1.26 or newer to build. `git` is required at runtime; `herdr`,
`cursor`, `helm` and `kubectl` are optional and only needed by the commands that
use them. Run `flow doctor` to see what is missing.

## 60-second quickstart

```sh
# Start work. Creates the worktree, the assets folder, a herdr workspace,
# and opens the editor.
flow init RD-19471 add new toolbar

# Jump into the worktree.
cd $(flow path RD-19471)

# ... do the work, push the branch, open a PR ...

# Install the chart CI built from this branch onto the dev cluster.
# Waits for the gating CI jobs with a live progress display, then runs helm.
flow cluster upgrade

# When the PR merges, tear it down.
flow delete RD-19471
```

## Command reference

| Command | What it does |
|---|---|
| `flow init <ID> <description...>` | Create — or adopt — a worktree, branch, assets folder, workspace and editor window |
| `flow list` | Table of managed tickets, with live status and PR state |
| `flow status [ticket]` | Everything flow knows about one ticket |
| `flow path [ticket]` | The worktree path, one bare line, for `cd $(flow path)` |
| `flow assets [ticket]` | Create and print the assets folder; `--open` reveals it in Finder |
| `flow open [ticket]` | Re-attach: focus or recreate the workspace, relaunch the editor |
| `flow delete [ticket]` | Remove the worktree and branch after checking the PR and local work |
| `flow prune` | Reconcile the registry against reality; adopt orphaned worktrees |
| `flow jira [ticket]` | Open the ticket in Jira |
| `flow pr [ticket]` | Open the PR, or GitHub's compare page when there is none |
| `flow cluster upgrade [ticket]` | Wait for CI, then `helm upgrade` the dev cluster |
| `flow cluster version [ticket]` | Print the chart version CI built for the branch |
| `flow cluster wait [ticket]` | Wait until the branch's chart is installable |
| `flow cluster status` | Deployed release plus a pod summary |
| `flow cluster history` | Release revision history |
| `flow cluster rollback [rev]` | Roll back to a previous revision |
| `flow cluster uninstall` | Uninstall the release |
| `flow cluster values` | The values file, the deployed values, or a diff |
| `flow doctor` | Check every dependency, PASS/WARN/FAIL with a fix |
| `flow config init\|path\|show\|edit` | Manage the config file |
| `flow completions <shell>` | Shell completions (alias of `flow completion`) |
| `flow version` | Version information; exists for `--json` |

### Global flags

| Flag | Meaning |
|---|---|
| `--json` | Machine-readable JSON on stdout. Implies yes for ordinary prompts, but **not** for destructive ones |
| `--no-color` | Disable colors (`NO_COLOR` is honored too) |
| `-v`, `-vv` | Log each external command; `-vv` adds its output |
| `--config` | An alternate config file |
| `-y`, `--yes` | Assume yes for all confirmations |
| `--dry-run` | Print every mutating action and perform none of them |

### Exit codes

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | Generic failure |
| 2 | Usage error |
| 3 | Not found (unknown ticket, no worktree, no PR under `--strict`) |
| 4 | Precondition failed (dirty worktree, wrong kube context, CI timeout) |
| 5 | External dependency missing or failed |
| 6 | Aborted at a confirmation prompt |

## Naming conventions, and why assets use an underscore

Given `flow init RD-19471 add new toolbar`:

| Thing | Pattern | Example |
|---|---|---|
| Branch | `{id}-{slug}` | `RD-19471-add-new-toolbar` |
| Worktree | `{repo}/.worktrees/{id}-{slug}` | `~/Developer/scaleops/.worktrees/RD-19471-add-new-toolbar` |
| Assets | `{assets_root}/{id}_{slug}` | `~/Developer/assets/RD-19471_add-new-toolbar` |
| Workspace label | `{id}` | `RD-19471` |

The assets directory uses an **underscore** between the ID and the slug; every
other name uses a hyphen. That is deliberate and preserved exactly: the assets
folders predate `flow` and are sorted and referenced by that shape. Both patterns
are configurable as templates under `naming:`.

The slug is derived from the description: lowercased, runs of whitespace `_`, `/`
and `.` collapsed to a single `-`, everything outside `[a-z0-9-]` dropped, and
truncated to 60 characters.

## `flow init` is idempotent and adopts what already exists

Running `flow init` against a ticket that already has a worktree, a branch, a
registry entry, an assets folder or a workspace converges on a complete,
correctly-registered setup instead of failing. It never resets, stashes, or
destroys an existing worktree.

- An existing worktree is **adopted** untouched. If it is checked out on a
  different branch than the derived one, flow records the *actual* branch and
  warns.
- An existing herdr workspace with a matching label is **focused**, not
  duplicated — so `flow init` on an existing ticket behaves like `flow open`.
- A different slug for a known ticket is treated as a **rename**: flow asks, and
  on yes updates only the registry, leaving the on-disk paths alone (renaming a
  branch and worktree mid-flight is riskier than the inconsistency it fixes).
- The one case flow refuses is a **non-empty directory that is not a git
  worktree**, because the alternative is clobbering unknown files.

To bring pre-`flow` worktrees under management one at a time, without creating
anything:

```sh
flow init RD-19471 add new toolbar --adopt-only
```

`flow prune` does the same in bulk: it offers to adopt every worktree under
`.worktrees/` that matches the naming pattern and has no registry entry.

## No command requires a pull request to exist

A ticket is frequently worked on before a PR is opened, may never get one, and
GitHub may be unreachable or untokened. PR state is modelled as three values —
and "there is no PR" never reads the same as "I could not find out":

| | No PR | Could not determine |
|---|---|---|
| `flow list` | `—` | `?` |
| `flow status` | `pull request: none` plus the compare URL | `pull request: unknown (reason)` |
| `flow delete` | Prompts, saying plainly that no PR was found | Prompts, saying why the state is unknown |
| `flow pr` | Offers the compare/create URL, exits 0 | Reports the error, exits 5 |
| `flow cluster upgrade` | Unaffected — chart versions key off the branch | Unaffected |

The same applies to CI: a branch with no workflow runs yet is a normal state.

## Chart version resolution

The ScaleOps CI builds a chart version from the newest git tag, the branch name
and the run ID:

```
v1.0.199-alpha-RD-19472-original-req-in-graphs-34468169597
└─next tag┘      └─────── branch name ────────┘ └─run id─┘
```

`flow` recovers it by trying, in order:

1. **Explicit** — `--version` short-circuits everything.
2. **CI wait** (the default for `cluster upgrade` and `cluster wait`) — watch the
   run until the gating jobs finish. The wait pins the run ID, the run ID pins
   the chart version, so the result is *verified by construction*.
3. **Helm repository index** — `helm search repo --versions --devel`, filtered to
   versions carrying `-alpha-{branch}-` (or `-rc-main-` on `main`), taking the
   highest **trailing run ID**. Ordering is by run ID, never by semver: semver
   compares the pre-release suffix alphanumerically and sorts run `…-9` above
   run `…-10`.
4. **Local derivation** — bump the third component of the newest git tag and
   assemble the version from the newest successful run's ID. This result is
   marked **not verified** and flow says so.

`flow cluster version --json` reports which strategy won, the run it came from,
and whether it was verified.

### Waiting for CI

A chart is only installable once CI has both published it to the Helm repository
and pushed the images it references. `flow cluster upgrade` waits for exactly the
jobs that gate that.

`build_workflow` is learned on first use: the first cluster command that needs
it lists the repository's workflows and asks which one publishes the chart, then
records the answer. Non-interactively it refuses and names the key, the same way
an unknown kube context is refused. The full block:

```yaml
repos:
  scaleops-sh/scaleops:
    build_workflow: go.yaml
    wait_for_jobs:
      - "Pre Release Helm"
      - "Pre Release Images"
```

Jobs are matched by **name prefix**, because reusable workflows report as
`{caller} / {inner}` and several jobs share a display name — all matches must
finish collectively. A `skipped` job counts as satisfied (the GPU and FIPS legs
are always skipped on a default dev build). If the watched run is cancelled by a
newer push, flow retargets to the newer run and says so.

Progress renders live to **stderr**, so stdout stays clean for `--json` and for
`helm upgrade --version $(flow cluster wait)`. In a pipe, under `--no-color`, or
in `--json` mode it degrades to one appended line per state transition, never a
repeated heartbeat.

## Configuration

Optional, at `~/.config/flow/config.yaml`. `flow` works with zero config; run
`flow config init` to write a fully-commented file, `flow config show` to see
what is actually in effect.

Precedence, highest first: **command-line flags → `FLOW_` environment variables →
config file → built-in defaults**. Environment variables use `__` for nesting, so
`FLOW_ASSETS__ROOT` sets `assets.root`.

```yaml
defaults:
  base_branch: main            # branch to create new ticket branches from
  remote: origin
  fetch_before_branch: true
  open_editor: true
  create_workspace: true
  create_assets: true

naming:
  branch: "{id}-{slug}"        # also the worktree directory name
  assets_dir: "{id}_{slug}"    # note the underscore
  workspace_label: "{id}"
  worktrees_subdir: ".worktrees"

assets:
  root: "~/Developer/assets"

editor:
  command: "cursor"
  args: ["{path}"]             # {path} becomes the worktree path
  detach: true                 # spawned in its own session; flow never blocks

workspace:
  provider: "herdr"            # "herdr" or "none"
  command: "herdr"

jira:
  browse_url: "https://scaleopscom.atlassian.net/browse/{id}"

github:
  api_base: "https://api.github.com"
  token_keychain_service: "flow-github-token"

# Per-repository overrides. Keys are "owner/name" or an absolute repo path.
repos:
  scaleops-sh/scaleops:
    base_branch: main
    helm_repo: scaleops
    chart_name: scaleops
    build_workflow: go.yaml
    chart_version_pattern: "alpha"
    wait_for_jobs: ["Pre Release Helm", "Pre Release Images"]

# Per-kube-context settings, learned on first use and written back here.
clusters:
  ido-saban-dev:
    release_name: scaleops
    namespace: scaleops-system
    values_file: "~/Developer/helm-values/ido-saban-dev.yaml"
    helm_repo: scaleops
    chart: scaleops/scaleops
    extra_args: ["--reset-then-reuse-values"]
    repo: scaleops-sh/scaleops
```

`~` is expanded in every path-valued field.

**The `clusters` map is the allowlist.** `flow` refuses to upgrade a kube context
it has never been told about. The first time you run a `flow cluster` command
against an unknown context it asks a handful of questions and writes the answers
back. That write is a surgical YAML-node edit, so the comments `flow config init`
produced survive it.

### GitHub token

Resolved in order, first hit wins: `$GITHUB_TOKEN`, `$GH_TOKEN`, `github.token`
in the config file, then the macOS keychain. To store one:

```sh
security add-generic-password -a "$USER" -s flow-github-token -w
```

Without a token, GitHub-dependent features degrade rather than fail: PR state
reads `?`, `flow delete` prompts because merge status is unknown, and chart
version resolution falls back to the Helm index. The token is never logged —
even under `-vv`, the keychain lookup is marked secret and its argv and output
are redacted.

## zsh completions

`flow completions` is an alias for `flow completion`, so both spellings work.
The Homebrew cask installs them for you. Otherwise:

```sh
# Once, if completion is not already enabled:
echo "autoload -U compinit; compinit" >> ~/.zshrc

# Then:
flow completions zsh > "${fpath[1]}/_flow"
```

Ticket arguments complete from the local registry, current repository first,
with the slug as the description.

## What flow deliberately does not do

These are explicit non-goals, not gaps:

- **It does not close the editor window** on `flow delete`. You do that.
- **It does not close the herdr workspace** on `flow delete`. It prints a
  reminder naming the workspace so it is easy to close by hand.
- **It never deletes the assets directory.** Ever. It prints the path so you
  know it was kept.
- **It does not run bootstrap commands** in a new worktree — no `go mod download`,
  no `make setup`, no copying `.env`.
- **It does not start agents or split panes** in the herdr workspace; the
  workspace is a plain empty shell at the worktree path.
- **It has no Jira API integration.** The Jira URL only builds a browse link.
- **`helm upgrade --install` is not the default.** `--install` is opt-in.

## Design notes

- **Every external process goes through `internal/exec.Runner`**, which has a
  fake implementation. No package calls `os/exec` directly. That is what lets the
  tests assert the exact argv `flow` produces without a real git, herdr, or
  cluster.
- **`internal/ghapi` wraps go-github behind a narrow interface** returning flow's
  own types. No other package imports go-github, so a major bump — which changes
  the import path — stays confined to one file.
- **The registry is a cache of facts about the world, not the source of truth.**
  Every reader tolerates drift: a worktree deleted by hand, a closed workspace, a
  renamed branch. Writes are atomic and taken under a file lock.
- **All styling lives in `internal/output/theme.go`.** Only `output` and `ciwait`
  import a Charm library; commands ask the renderer for output and never style.
  `--no-color`, `NO_COLOR`, `--json` and a non-TTY writer each settle the color
  profile once, centrally, and that decision is shared with `huh`,
  `charmbracelet/log` and `fang` so all four agree.
- **`ciwait` is split into a pure state machine and a renderer**, so the
  job-matching rules and the monotonic-percentage invariant are table-testable
  with no network and no terminal.

### Choices made where the spec left room

- **Go 1.26, not 1.25.** The current major of `go-github` (v91) requires 1.26.
- **`lipgloss.Renderer` does not exist in the v2 generation.** v2 dropped
  renderers and `AdaptiveColor` in favour of `colorprofile.Writer` plus
  `lipgloss.LightDark`. `internal/output` implements the intent: one profile
  decision point, one writer bound to stdout for results and one to stderr for
  progress, and a `Theme` built from a resolved light/dark function.
- **A small in-process ETag cache instead of `gregjones/httpcache`.** That module
  has no semver tags and is unmaintained; flow's polling loop is its only
  consumer, so `internal/ghapi/transport.go` carries a ~90-line `RoundTripper`
  instead of taking a pseudo-version dependency.
- **The values diff is computed in-process** with `go-udiff` rather than shelling
  out to `diff`, so it behaves the same everywhere.
- **The CI progress bar is static rather than spring-animated.** The percentage
  changes at most once per poll, and a deterministic bar is golden-testable; the
  spinner still ticks at ~100 ms so the display stays alive between polls.

## Development

```sh
make build              # build into ./bin
make test               # fast unit tests, with -race
make test-integration   # everything, including tests that drive a real git
make lint               # golangci-lint over all build tags
make golden             # regenerate golden files
make snapshot           # local GoReleaser build
```

## License

MIT. See [LICENSE](LICENSE).
