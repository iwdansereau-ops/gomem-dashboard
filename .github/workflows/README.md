# CI: staging-memory-check

Automated staging memory regression gate for Go services. The logic is
published as a **reusable workflow** in this repo
(`staging-memory-check-reusable.yml`), so downstream services adopt it
with a 6-line caller and never fork the pipeline.

## Adopt in a downstream Go repo (6 lines)

Drop this file at `.github/workflows/staging-memory-check.yml` in your
service repo:

```yaml
name: staging-memory-check
on:
  deployment_status:
jobs:
  check:
    uses: iwdansereau-ops/gomem-dashboard/.github/workflows/staging-memory-check-reusable.yml@v1
    secrets:
      pprof_url:   ${{ secrets.STAGING_PPROF_URL }}
      pprof_token: ${{ secrets.STAGING_PPROF_TOKEN }}   # optional; delete if unauthenticated
```

That's it. Then in your service:

1. Enable pprof: `import _ "net/http/pprof"` on a reachable port.
2. Expose `/debug/memstats` (4-line handler in the top-level README).
3. Add repo secrets `STAGING_PPROF_URL` and
   `STAGING_PPROF_TOKEN` (optional bearer token). If `STAGING_PPROF_URL`
   is not set, the memory check skips gracefully (no red runs) until you
   configure it — set it to activate the gate.
4. Configure branch protection to require the
   `staging-memory-check/verdict` context (see
   [`scripts/ci/require-branch-protection.sh`](../../scripts/ci/require-branch-protection.sh)).

### Versioning

Pin the reusable workflow to a **released tag** (`@v1`, `@v1.2.3`), not
`@main`, so breaking changes never appear silently on your critical path.
The `v1` tag is moved forward for backwards-compatible additions; a
`v2` tag will exist if we ever need to change inputs or verdict semantics.

### Customising per-service

All knobs are optional inputs. Override any of them in the `with:` block:

```yaml
jobs:
  check:
    uses: iwdansereau-ops/gomem-dashboard/.github/workflows/staging-memory-check-reusable.yml@v1
    with:
      gomem_ref:                v1.3.0     # pin the tooling version explicitly
      snapshot_count:           7
      snapshot_interval_seconds: 300        # 7 × 300s = 35 min
      leak_threshold_bytes:     1048576    # 1 MB per function
      environment:              staging-eu # match your deployment env name
      status_context:           mem/eu     # gate branch protection on this name
      runs_on:                  "self-hosted,staging-eu"
      go_version:               "1.22"
    secrets:
      pprof_url:   ${{ secrets.STAGING_PPROF_URL_EU }}
      pprof_token: ${{ secrets.STAGING_PPROF_TOKEN_EU }}
```

### Reading outputs from downstream jobs

The caller can wire follow-up jobs (Slack notification, PagerDuty alert,
dashboard refresh, …) to the verdict:

```yaml
jobs:
  check:
    uses: iwdansereau-ops/gomem-dashboard/.github/workflows/staging-memory-check-reusable.yml@v1
    secrets: { pprof_url: ${{ secrets.STAGING_PPROF_URL }} }

  notify:
    needs: check
    if: needs.check.outputs.has_regression == 'true'
    runs-on: ubuntu-latest
    steps:
      - run: |
          echo "Regression verdict: ${{ needs.check.outputs.verdict }}"
          echo "Worst offender:     ${{ needs.check.outputs.worst_function }}"
          # curl slack / pagerduty / …
```

Available outputs: `verdict`, `has_regression`, `worst_function`,
`worst_bytes`.

---

## What the workflow does

On every successful deployment to the `staging`
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

The reusable workflow accepts whatever event the caller declares. The
most common shapes:

- `deployment_status` — the default. The reusable workflow gates on
  `state == 'success' && environment == inputs.environment`.
- `workflow_dispatch` — manual re-run. Forward `sha` / `pprof_url` from
  the caller's inputs into the reusable workflow's `override_sha` /
  `override_pprof_url` inputs.
- `workflow_run` — fire after your own "Deploy to staging" workflow
  succeeds. Set `environment: <yours>` on the caller and gate the
  caller's `jobs.check.if` on `github.event.workflow_run.conclusion == 'success'`.

## Threshold configuration

Pass these as `with:` inputs on the caller (defaults shown):

```yaml
with:
  snapshot_count:           5
  snapshot_interval_seconds: 180   # 5 × 180s = 15 min
  leak_threshold_bytes:     512000 # 500 KB per-function flat_delta
```

The evaluator compares against the **flat delta** (bytes allocated *by
that function*, not through it) — so `runProcessor` won't trip the alarm
just because it calls a leaky child.

Separately, the churn classifier has fixed thresholds (churn ratio 20×,
GC ≥ 1/s, alloc ≥ 5 MB/s) hard-coded in `evaluate_leak.py`. Fork if you
need to change those — they're not part of the workflow's input surface
because the values above are what actually distinguish thrash from
normal Go behaviour, and per-service overrides tend to hide problems.

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

## AVPT rollout

`.github/workflows/avpt-shared-rollout.yml` integrates this repo with the
shared, self-contained **AVPT** workflow (Assess → Validate → Preserve →
Transition) hosted by the platform repo
`iwdansereau-ops/avpt-cicd-dashboard`. The caller is intentionally thin: it
`uses:` the shared reusable workflow **pinned by commit SHA** and maps the two
repo-specific phases onto real tooling already in this repo.

```
uses: iwdansereau-ops/avpt-cicd-dashboard/.github/workflows/avpt.yml@4e033c739fe50c9cd470132c8005e23297877b19
```

### Phase mapping

| AVPT phase | gomem-dashboard mapping |
| --- | --- |
| **Assess** | Platform-provided context probe. No repo hook. |
| **Validate** | `scripts/ci/avpt-validate.sh` — real Go `build` / `vet` / `test` plus a **dry-run** of the memory-regression evaluator (the same classifier behind the blocking `staging-memory-check/verdict` gate). No live staging endpoint, no secrets. |
| **Preserve** | `scripts/ci/avpt-preserve.sh` — a safe, read-only snapshot of benchmark + config metadata (`go.mod`/`go.sum`, `go env`, workflow inventory, benchmark baseline, git SHA). Uploaded as the AVPT artifact. Mutates nothing. |
| **Transition** (deploy / rollback) | **Inert.** `dry_run: true`, `enable_deploy: false`, `enable_rollback: false`, and no-op commands. Nothing is ever deployed or rolled back during the rollout. |

### Safety envelope

The rollout runs entirely offline and cannot touch live infrastructure:

- `dry_run: true` — plan-only.
- No `secrets:` block — the shared workflow receives zero credentials, so it
  cannot reach any staging/production environment.
- Deploy **and** rollback are disabled and no-op.
- **Branch protection is untouched.** This workflow neither adds nor removes a
  required status check. The existing `staging-memory-check/verdict` gate (and
  any extras configured via `scripts/ci/require-branch-protection.sh`) keeps
  working exactly as before. Promoting AVPT to a *required* check is a
  deliberate, separate follow-up — see below.

### Upgrading the pin

The `@<SHA>` on the `uses:` line is the single source of truth for which
version of the shared AVPT workflow runs. Never point it at a mutable ref
(`@main`, `@v1`) — a SHA is the only pin that can't shift under you.

To bump it:

1. Read the diff on `iwdansereau-ops/avpt-cicd-dashboard` between the current
   pinned SHA and the target SHA, paying attention to the `workflow_call`
   input contract this caller depends on: `dry_run`, `environment`,
   `checkout_caller`, `assess_command`, `validate_command`,
   `preserve_command`, `preserve_artifact_path`, `enable_deploy`,
   `enable_rollback`, `deploy_command`, `rollback_command`. If any were
   renamed or removed, update this caller in the same PR.
2. Update the SHA in **both** the `uses:` line and the "Upgrading the pin"
   reference above so docs and code never drift.
3. Keep `dry_run: true` and the inert deploy/rollback until the rollout is
   explicitly signed off — a pin bump is not a promotion to live deploy.
4. Let the `pull_request` self-test on this caller run green before merging.

### Promoting Validate to a required check (later)

When the team is ready to make AVPT blocking, append its status context to
branch protection with the existing helper — this is additive and preserves
`staging-memory-check/verdict`:

```bash
./scripts/ci/require-branch-protection.sh \
    --repo iwdansereau-ops/gomem-dashboard \
    --extra-check "avpt/validate"
```

Until then, AVPT runs in report-only mode alongside the memory-check gate.
