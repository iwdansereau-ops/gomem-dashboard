# CI: staging-memory-check

Automated staging memory regression gate for Go services that use
`gomem-dashboard`. On every successful deployment to the `staging`
environment it:

1. Checks out the exact commit that was deployed.
2. Captures **5 heap profiles over 15 minutes** from the deployed
   processor's `/debug/pprof/heap` endpoint, **interleaved with 5
   `runtime.MemStats` snapshots** from `/debug/memstats`.
3. Runs `gomem report` to produce diff JSON, Markdown, and SVG for every
   consecutive pair of snapshots.
4. Aggregates the per-pair diffs into a single **"first snapshot → last
   snapshot"** view and diffs the first/last `gcstats_*.json` to get
   `TotalAlloc`, `NumGC`, and pause deltas.
5. **Classifies** the regression as `RETENTION_LEAK`, `ALLOC_CHURN`,
   `MIXED`, or `CLEAN` and ranks the top 5 functions by retained
   `inuse_space` bytes, mapping each back to its source `file:line`.
6. Posts (or updates in place) a sticky PR comment with a verdict badge,
   the GC & allocation metrics table, and verdict-specific remediation.
7. **Fails the workflow** — turning the PR check red — when the verdict is
   anything other than `CLEAN`. A retention leak trips on any function
   whose flat delta exceeds **500 KB**; a churn regression trips on churn
   ratio ≥ 20× combined with sustained GC or alloc pressure.

## One-time setup

1. **Enable pprof on your service.** Add `import _ "net/http/pprof"` and
   expose it on a reachable port. Recommended: bind to an internal-only
   listener + require a bearer token.

2. **Create the `staging` deployment environment** (Settings → Environments)
   if it doesn't already exist. Any tool that marks a deployment `success`
   for this environment will trigger the workflow — GitHub Environments,
   Argo CD's GitHub notifications controller, Flux's notification-controller,
   Spinnaker, or a bespoke webhook.

3. **Add secrets** (Settings → Secrets and variables → Actions):

   | Name | Required? | Purpose |
   |---|---|---|
   | `STAGING_PPROF_URL`   | **yes** | Base URL of the pprof endpoint, e.g. `https://staging-processor.internal:6060` |
   | `STAGING_PPROF_TOKEN` | optional | Bearer token; if set, the workflow injects it into every profile request via a local auth proxy |

4. (Optional) **Restrict runners** if staging is only reachable from a
   private network. Replace `runs-on: ubuntu-latest` with your self-hosted
   runner label (e.g. `runs-on: [self-hosted, staging-network]`).

## Trigger surface

- `deployment_status` — the default. Fires for **every** deployment event,
  and the workflow gates on `state == 'success' && environment == 'staging'`.
- `workflow_dispatch` — manual re-run. Optional `sha` and `pprof_url`
  inputs let you rerun a check against an older commit or an ad-hoc host.
- `workflow_run` — alternative for repos with their own "Deploy to staging"
  workflow. See the commented block at the bottom of `staging-memory-check.yml`.

## Threshold configuration

Both knobs live in the workflow's `env:` block:

```yaml
env:
  SNAPSHOT_COUNT: "5"
  SNAPSHOT_INTERVAL_SECONDS: "180"   # 5 × 180s = 15 min
  LEAK_THRESHOLD_BYTES: "512000"     # 500 KB
```

Change `LEAK_THRESHOLD_BYTES` to tune sensitivity per service. The evaluator
compares against the **flat delta** (bytes allocated *by that function*,
not through it) — so `runProcessor` won't trip the alarm just because it
calls a leaky child.

## Dry-run locally

The `scripts/ci/simulate.sh` script feeds the workflow's evaluator step
real profile data from `./profiles/` without needing to actually run in
GitHub. Useful when tweaking the PR-comment format:

```bash
./scripts/ci/simulate.sh 512000    # 500 KB threshold → regression case
./scripts/ci/simulate.sh 10485760  # 10 MB threshold  → clean case
```

## What the PR sees

The sticky comment carries a verdict badge, a "Why this verdict" bullet
list, a **GC & allocation metrics** table, and verdict-specific
remediation. Three shapes:

**Retention leak** — real leak, check fails:

> ### 🚨 Staging memory regression detected on `abc1234`
>
> **Retention leak — live memory is not being freed.**
>
> | # | Function | Flat Δ | Cum Δ | Source |
> |--:|---|--:|--:|---|
> | 1 🚨 | `main.processBatch` | 4.26 MB | 4.26 MB | `cmd/sample-processor/main.go:27` |

**Allocation churn** — GC thrash, check fails:

> ### ⚠️ Allocation churn on `deadbee`
>
> **Allocation churn / GC thrash — not a leak.**
> `HeapInuse` Δ 48 KB, `TotalAlloc` Δ 7.33 GB, churn ratio 160201×,
> GC 139/s. Look for temporary slices in hot loops — pool with `sync.Pool`.

**Clean:** check passes, sticky comment updates to
`✅ Staging memory check passed on <sha>` with GC metrics still shown for
reference — reviewers always see the *latest* deploy result rather than a
stale warning from a previous push.

## Artifacts

Every run uploads `gomem-staging-<short-sha>` containing:

- `profiles/heap_*.pb.gz` — the raw pprof snapshots (open with
  `go tool pprof profiles/heap_*.pb.gz`).
- `profiles/gcstats_*.json` — the paired `runtime.MemStats` snapshots.
- `reports/diff_*.svg|md|json` — per-pair diff reports.
- `reports/pr-comment.md` — the exact comment body that was posted.

Retention: 14 days (change `retention-days` in the workflow).

## Permissions

The workflow requests the minimum required scopes:

```yaml
permissions:
  contents: read
  pull-requests: write   # sticky PR comment
  deployments: read      # enrich the deployment event
  actions: read
  statuses: write        # publish the blocking `staging-memory-check/verdict` status
```

`GITHUB_TOKEN` in this scope **cannot** push code, create branches, merge
PRs, or approve reviews. Mutating actions are limited to `POST/PATCH` on
the PR's issue comments and `POST` to the commit-statuses endpoint of the
PR's head SHA.

## Blocking merges with branch protection

The workflow publishes a named commit status on the **PR head SHA** (not
the deployed merge commit) so it shows up next to every other check on
the PR:

| Verdict | Commit status | PR check |
|---|---|---|
| `CLEAN` | `success` | ✅ |
| `RETENTION_LEAK` | `failure` | ❌ |
| `ALLOC_CHURN`    | `failure` | ❌ |
| `MIXED`          | `failure` | ❌ |
| evaluator crashed / no PR head | `error` | ⚠️ |
| workflow still running | `pending` | 🟡 |

**Context name:** `staging-memory-check/verdict`

Add that context to the branch's required-status-checks list to gate
merging. A helper script is included:

```bash
# One-time setup, run from a machine with a gh-authenticated admin token:
./scripts/ci/require-branch-protection.sh \
    --repo my-org/my-service \
    --branch main

# Preserve other checks you already require, and enforce for admins too:
./scripts/ci/require-branch-protection.sh \
    --repo my-org/my-service \
    --branch main \
    --extra-check "ci/build" \
    --extra-check "ci/test" \
    --required-reviewers 1 \
    --enforce-admins

# See exactly what would be sent to the API without applying it:
./scripts/ci/require-branch-protection.sh --repo my-org/my-service --dry-run

# Roll back:
./scripts/ci/require-branch-protection.sh --repo my-org/my-service --remove
```

The script is **idempotent** and preserves any other required contexts,
review counts, linear-history / force-push / conversation-resolution
settings you already had on the branch — it only appends
`staging-memory-check/verdict` to `required_status_checks.contexts`.

### Overriding a failed check

Options from least to most disruptive:

1. **Fix the regression** and push a new commit. The deploy fires again,
   the workflow reruns, and the status flips to `success`. This is the
   only path a normal contributor has.
2. **Admin merge-anyway.** If you left `enforce_admins` off (the default
   of this helper), a repo admin sees a "Merge without waiting for
   requirements to be met" button on the PR. GitHub records the override
   in the audit log.
3. **Temporarily remove the requirement.** Run
   `./scripts/ci/require-branch-protection.sh --repo … --remove`,
   merge, then re-run without `--remove` to restore the gate. Also
   recorded in the audit log.

### Why publish to the PR head SHA, not the merge commit?

`deployment_status` events fire on whatever SHA was actually deployed —
often the merge commit for squash-merged PRs, or an ephemeral SHA for
rebase-and-merge flows. Branch protection evaluates checks on the PR's
*head* commit, so if we posted the status on the deployed SHA it would
never gate the merge. The workflow resolves the PR from the deployed SHA
via `/repos/{owner}/{repo}/commits/{sha}/pulls` and publishes the status
back to that PR's `head.sha`, closing the loop.
