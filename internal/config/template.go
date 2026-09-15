package config

// Template is the fully-commented default config file written by
// `flow config init`. It documents every key, and every value in it matches
// Default(), so writing it changes nothing about flow's behaviour.
const Template = `# ~/.config/flow/config.yaml
#
# Every key below is optional: flow runs with no config file at all. Values
# shown are the built-in defaults. Precedence, highest first:
#   command-line flags > FLOW_ environment variables > this file > defaults
# Environment variables use "__" for nesting, e.g. FLOW_ASSETS__ROOT.

defaults:
  base_branch: main            # branch to create new ticket branches from
  remote: origin
  fetch_before_branch: true    # fetch the remote before branching
  open_editor: true
  create_workspace: true
  create_assets: true

naming:
  # {id} is the normalized ticket ID (RD-19471), {slug} the normalized
  # description. Note the underscore in assets_dir and the hyphen elsewhere:
  # that difference is deliberate and load-bearing.
  branch: "{id}-{slug}"        # also used for the worktree directory name
  assets_dir: "{id}_{slug}"
  workspace_label: "{id}"
  worktrees_subdir: ".worktrees"

assets:
  root: "~/Developer/assets"

editor:
  command: "cursor"
  args: ["{path}"]             # {path} is substituted with the worktree path
  detach: true                 # spawn detached; flow must not block on it

workspace:
  provider: "herdr"            # only "herdr" and "none" are supported in v1
  command: "herdr"

jira:
  browse_url: "https://scaleopscom.atlassian.net/browse/{id}"

github:
  api_base: "https://api.github.com"
  # Token resolution order: $GITHUB_TOKEN, $GH_TOKEN, github.token below,
  # then the macOS keychain service named here.
  token_keychain_service: "flow-github-token"

# Per-repository overrides. Keys are "owner/name" or an absolute repo path.
# build_workflow is learned on first use — the cluster commands ask which
# workflow publishes the chart and record the answer here — so this block only
# needs filling in by hand to override something or to work non-interactively.
repos: {}
#  scaleops-sh/scaleops:
#    base_branch: main
#    helm_repo: scaleops                     # local ` + "`helm repo`" + ` name for this project's charts
#    chart_name: scaleops                    # chart within that repo
#    build_workflow: go.yaml                 # workflow file that produces the chart
#    chart_version_pattern: "alpha"          # "alpha" for feature branches, "rc-main" for main
#    # Jobs that must finish before the chart is installable. Matched by name
#    # prefix; a "skipped" job counts as satisfied.
#    wait_for_jobs:
#      - "Pre Release Helm"
#      - "Pre Release Images"

# Per-kube-context cluster settings, learned on first use and written back here.
# A context absent from this map is one flow will refuse to upgrade.
clusters: {}
#  ido-saban-dev:                            # the kube context name
#    release_name: scaleops
#    namespace: scaleops-system
#    values_file: "~/Developer/helm-values/ido-saban-dev.yaml"
#    helm_repo: scaleops
#    chart: scaleops/scaleops
#    extra_args: ["--reset-then-reuse-values"]
#    repo: scaleops-sh/scaleops              # which source repo this cluster tracks
`
