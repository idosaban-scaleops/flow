# flow — code review

**Baseline:** `make lint` → 0 issues. `make test-integration` → all packages green. The four architecture rules in `AGENTS.md` are enforced by `internal/architecture_test.go` and hold. The findings below are the *unenforced* invariants plus reliability and blast-radius concerns.

This is a genuinely well-built codebase — the exec seam, the ciwait state-machine/renderer split, the registry's flock + atomic write, and the `Extra`-field preservation are all better than typical. Everything below is against that bar.

## Confirmed bugs (reproduced)

### 1. `flow prune --json --yes --dry-run` writes the registry — `prune.go:62-73`, `212-239`
The three `apply` closures call `a.Registry.Update(...)` directly, bypassing `app.mutate`. The compensating `DryRun()` guard at `prune.go:86` sits *after* the `if app.JSON()` early return. Reproduced on a scratch registry: **1 entry → 0 entries under `--dry-run`.** This is precisely the escape `AGENTS.md` calls "the easiest rule to forget."

**Fix:** wrap `action.apply()` in `a.mutate(...)` inside `applyPrune`, and delete the ad-hoc check at :86. Leave `app.gitPrune` at `prune.go:72` alone — it goes through `a.Git()`, so it is already correctly announced-and-skipped.

### 2. The wrong branch's chart can be installed — `chartver.go:51-56, 75-77`
`Match` is an unanchored `strings.Contains(version, "-alpha-"+branchTag+"-")`. Verified:

```
Infix("feat/x") = "-alpha-feat-x-"
Match("1.2.4-alpha-feat-x-2-999", "feat/x") = true
Newest([...], "feat/x") -> "1.2.4-alpha-feat-x-2-999"
```

So on branch `feat/x`, `flow cluster upgrade` resolves the chart built from `feat/x-2` and helm-upgrades the cluster with it. `Newest` sorting by run ID makes the sibling's newer build the *more* likely pick.

**Fix:** anchor to the run ID — require `version == prefix + Infix(b) + strconv(runID)`, or match `regexp.QuoteMeta(Infix(b)) + `\d+$``.

### 3. CI gating widens to unrelated jobs — `ciwait/state.go:199-204`
`matchesTarget` is a bare `HasPrefix`. Verified with target `"Test"`:

```
"Test"                 -> true   (intended)
"Test / unit"          -> true   (intended: reusable-workflow nesting)
"Test Coverage Report" -> true   (not intended)
"Tests-e2e"            -> true   (not intended)
```

**Fix:** `name == target || strings.HasPrefix(name, target+" / ")`. `registry.underPath` already gets this exact boundary rule right — worth matching it.

### 4. `flow cluster rollback 3abc` silently rolls back to revision 3 — `cluster_inspect.go:207`
`fmt.Sscanf(args[0], "%d", &revision)` returns `err == nil` on trailing garbage (verified: `"3abc"` → `n=1, r=3, err=<nil>`). A destructive command accepting malformed input as valid. **Fix:** `strconv.Atoi`.

## Safety and correctness

### 5. The printed command and the executed command can disagree — `cluster_inspect.go:18-23`, `cluster_upgrade.go:265-268`
`kubeContextArg()` returns `""` unless `--context` was passed, so helm falls back to its own ambient current-context resolution. Unconditionally, this means the `payload.Command` you print for the user to copy is not reproducible, and the confirmation prompt names `target.Context` while helm resolves the context independently. Separately — and more narrowly — a `kubectl config use-context` between resolution and execution redirects upgrade/rollback/uninstall to a different cluster.

**Fix:** always pass `--kube-context target.Context`. This does not conflict with "never mutate the user's kubeconfig" — that rule is about `use-context`, not about passing the flag.

### 6. A wrong remediation hint in an error message — `cluster.go:91-97, 122-128`
`learnCluster`'s non-interactive error says "…or supply `--release`, `--namespace`, `--helm-repo` and `--chart` explicitly." But `resolveCluster` returns at line 95, *before* the override block at lines 100-111. Supplying those flags does not work. Either apply overrides before the `known` check, or change the message.

### 7. The `clusters` allowlist is documented as stricter than it is — `cluster.go:91-97`
`AGENTS.md` and `flow cluster --help` both say flow "refuses to act on a kube context with no entry." It actually runs `learnCluster`, which prompts and writes the entry — any context is adopted in an interactive session. It's a learn-on-first-use, not an allowlist. Worth aligning the docs (or the code) since this invariant is load-bearing for cluster safety.

### 8. `--all` composes with `--force` into a repo-wide destructive flag pair — `delete.go:57, 87-88, 154`
`--force` is documented as "skip every safety prompt and force removal", so skipping `confirmDelete` is what the user asked for. The real issue: the comment at `delete.go:87-88` claims "`--all` alone must never auto-confirm; the combination with `--yes` has to be explicit" — but no such guard exists in the code. `--all --force` force-removes every worktree in the repo, discarding uncommitted work, with no `--yes` and no summary prompt. Suggest requiring `--yes` for that pair, or one summary `confirmDestructive` naming the ticket count.

### 9. Inspection failure fails open to "clean" — `delete.go:217-229`
`StatusOf`'s error leaves `clean = true`; `Unpushed`'s error is dropped (`unpushed, _ :=`). With `PRMerged` + apparent-clean, delete proceeds with **no prompt at all**. git's own non-force `worktree remove` is then the only remaining guard. Treat an inspection failure as "not clean" and prompt.

### 10. Dry-run classification is a denylist, so *new* verbs default to executing — `root.go:298-351`
To be clear: this is correct for the current tree. I checked every call site — `DeleteBranch` emits `-d`/`-D` (which `hasAny` catches), `IsMergedInto` uses `branch --merged` (correctly read-only), `kube` only issues `config view|current-context|get-contexts`, and `secrets` only `find-generic-password`. No live escape.

The risk is structural: `mutatingGitVerbs` is a denylist, so `stash`, `restore`, `rm`, `mv`, `pull`, `gc`, `config`, `submodule` would all silently execute under `--dry-run` the day someone adds them. `case "security": return true` and the wholesale `kubectl config` allowance are the same shape. Inverting to an allowlist of read verbs makes the failure mode "dry-run is over-cautious" instead of "dry-run mutated something."

### 11. `DryRun.Run` returns `Result{}, nil` — `dryrun.go:28`
Indistinguishable from "succeeded with empty output." Any caller on `a.Runner` that parses stdout gets a wrong dry-run description. `herdr --version` is the concrete shape (`isReadOnlyCommand` requires `len(Args) > 1`, so it classifies as mutating); `doctor` dodges it only because it happens to use `ReadHerdr`. A sentinel or `ErrDryRun` would let parsers tell the difference.

### 12. `flow init` duplicates a workspace when herdr is flaky — `init.go:412-439`
On `ListWorkspaces` error it warns, returns only if `adoptOnly`, then falls through with `list == nil`. Verified `herdr.FindByID` and `FindByLabel` both return plain `false` on a nil slice — no sentinel — so both lookups miss and `CreateWorkspace` runs, creating a second workspace for a ticket that already has one. That breaks the "focuses rather than duplicates" invariant. `prune.listWorkspaces` gets this right (returns nil specifically to *suppress* actions); init should bail out the same way.

### 13. 429 handling ignores `Retry-After` — `ghapi/transport.go:71-99`
GitHub sends `Retry-After` / `X-RateLimit-Reset` on secondary rate limits. Backing off 500ms–2s and giving up after 3 attempts is the pattern that earns a longer block — relevant for a 45-minute poll. The sleep is also not cancellable (ctx is checked *before* sleeping, not during). Two latent items in the same function: `RoundTrip` mutates the caller's `req.Body`, against the `RoundTripper` contract, and retries any method — harmless while flow only issues GETs.

### 14. `--poll-interval` is silently overridden — `ciwait/wait.go:404-409`
Once any job is in progress, `base = MinPollInterval` (5s) unconditionally replaces the user's value. A user who passes `--poll-interval 30s` to conserve quota gets 5s. Widening should be honored: `base = max(opts.PollInterval, …)`.

### 15. One cancelled job retargets the whole run — `ciwait/state.go:141-143`
A single cancelled matrix leg sets `PhaseCancelled`, which triggers `retarget`, which ends the wait with `ErrCancelled` when no newer run exists. Should key off the *run's* conclusion, not an individual job's.

### 16. Fork PRs read as "no PR" — `ghapi/client.go:147`
`Head: owner + ":" + branch` only finds same-repo PRs. A fork PR yields `PRNone`, and `flow delete` then asks "No pull request found for branch X. Delete anyway?" — the most dangerous of the four prompts. Fine if the team never uses forks, but worth a comment saying so.

### 17. Registry lock has no timeout — `registry/store.go:88`
`flock.Lock()` blocks forever. A hung `flow` wedges every other invocation with no message. `TryLockContext` plus a clear error is better. The rest of the store is solid — flock around the whole read-modify-write, temp+fsync+rename, `Extra` preservation, version guard.

### 18. `Upsert` vs `Find`/`Remove` case sensitivity — `registry.go:71, 128, 149`
`Find` and `Remove` use `EqualFold`; `Upsert` compares `Key()` exactly. A registry containing `rd-1` gets a duplicate `RD-1` appended rather than updated. Upstream normalization hides it today.

### 19. Non-atomic `info/exclude` rewrite — `gitx/worktree.go:177`
Read-whole/write-whole via `os.WriteFile`. `config.WriteFileAtomic` already exists in the tree. (That helper itself doesn't fsync the parent directory after rename — fine for a CLI, worth a comment.)

### 20. `flow doctor --dry-run` writes a probe file — `doctor.go:299-305`
`writable()` creates and removes a temp file in the assets root regardless of `--dry-run`. Self-cleaning, so harmless, but it's the one mutation outside `mutate`. Also `doctor.go:290` recurses on `filepath.Dir` with no depth guard.

### 21. Ctrl-C during helm exits 5, not 6 — `errors.go:91-102`
`ExitCodeFor` checks `*Error` first, so `Wrap(ExitDependency, "helm_failed", …)` around a `context.Canceled` wins over the `ExitAborted` mapping.

## Smaller items and modern Go

- **Dead code:** `init.go:350` `_ = n`, `pr.go:39` `_ = web`. `--web` is registered but never read; `--keep-assets` (`delete.go:59`) is documented as a no-op. Drop or wire them.
- **`sort` → `slices`:** `sort.Slice` at `chartver.go:105` and `ciwait/wait.go:189`, `sort.SliceStable` at `registry.go:117`. `chartver.Newest` and `Waiter.ETA` don't need a sort at all — one pass for max, and `slices.Sort` + index for the median.
- `hasAny` (`root.go:353`) → `slices.Contains` / `slices.ContainsFunc`.
- `map[string]bool` sets (`root.go:301, 308`) → `map[string]struct{}`, or just `slices.Contains` on a small slice.
- `firstLine` (`exec/real.go:164`) → `strings.Cut(s, "\n")`.
- `syscall.SysProcAttr{Setsid: true}` (`real.go:95`) is Unix-only. `.goreleaser.yaml` targets only darwin/linux, so it compiles today — an unguarded landmine if Windows is ever added.
- `prune.go:112, 161` compares paths via `filepath.Clean` map keys rather than `gitx.SamePath`, against the repo's own rule. Protected today by the `file.Find(repo.Key, tk.ID)` fallback, so cosmetic.
- `ciwait.Machine.clamp` mutates `maxPercent` but reads like a pure helper.
- `ErrTimeout` discards the partial job state — the user loses the view exactly when it matters most.
- `App.Token` / `App.GitHub` are uncached: `flow delete --all` and `flow list` spawn one `security find-generic-password` and build a fresh HTTP client per ticket.
- **`.golangci.yaml`** is already strong (errorlint, gosec, noctx, nilerr, bodyclose, gocritic w/ performance, staticcheck all). The one high-value addition for *this* codebase is **`exhaustive`** — it's exactly the linter that would mechanically enforce "`PRNone` and `PRUnknown` must never collapse" on every `switch` over `PRState`, `Phase`, and `Conclusion`.

---

**If you fix four things, make them #1, #2, #3 and #5** — those are the ones where a user gets a wrong outcome without any signal that something went wrong.
