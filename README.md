# gomem-dashboard

Automated memory analysis dashboard for Go processors. It:

1. **Captures** heap profiles from any Go binary exposing `net/http/pprof` at a fixed interval.
2. **Diffs** two snapshots to compute the `inuse_space` delta per function (bytes retained between snapshots).
3. **Ranks** the top 5 functions responsible for the largest memory growth and maps each hotspot back to the exact source file + line.
4. **Renders** an SVG call graph in which nodes and edges are shaded proportional to their share of the leaked bytes (darkest red = worst offender).
5. **Serves** a lightweight web dashboard that lists every captured snapshot, every diff, and the ranked remediation report.

The full pipeline runs against staging with a single command:

```
scripts/staging-capture.sh http://staging-host:6060 300 12
#                          ^ pprof endpoint        ^ interval(s) ^ snapshots
```

## Components

| Path | Purpose |
| --- | --- |
| `cmd/gomem` | Single binary: `capture`, `diff`, `report`, `serve`, `gcstats`, `gcdiff` sub-commands |
| `cmd/sample-processor` | Reference Go "processor" with an intentional leak, exposes `/debug/pprof` + `/debug/memstats`. `MODE=churn` simulates GC thrashing instead of a leak |
| `internal/capture` | Fetches `/debug/pprof/heap` and writes timestamped `.pb.gz` files |
| `internal/gcstats` | Fetches `/debug/memstats` and writes timestamped `gcstats_*.json` files (TotalAlloc, NumGC, HeapInuse, pause histograms, …). Interleaved with heap capture so every snapshot has a paired GC-metrics file. |
| `internal/diff` | Parses two pprof profiles, computes per-function `inuse_space` delta |
| `internal/graph` | Builds a call graph from the diff and renders it to SVG |
| `internal/report` | Produces a Markdown + JSON summary ranking the top-N leaking functions with source lines |
| `internal/server` | HTTP dashboard on `:8080` |
| `scripts/staging-capture.sh` | Ops-friendly wrapper: captures N snapshots at a fixed interval, diffs consecutive pairs, generates SVG + report |
| `scripts/ci/evaluate_leak.py` | CI classifier — parses reports + gcstats and returns one of `CLEAN` / `RETENTION_LEAK` / `ALLOC_CHURN` / `MIXED`, then renders the PR comment. |

## Quickstart

```bash
# 1. Build
go build -o bin/gomem ./cmd/gomem
go build -o bin/sample-processor ./cmd/sample-processor

# 2. Start the sample leaky processor (listens on :6060)
./bin/sample-processor &

# 3. Capture 6 heap snapshots, 10s apart, from staging
./scripts/staging-capture.sh http://localhost:6060 10 6

# 4. Open the dashboard
./bin/gomem serve --dir ./profiles --reports ./reports
open http://localhost:8080
```

## Output artefacts

For every consecutive pair of snapshots `(t_n, t_{n+1})` the tool emits:

- `reports/diff_<n>_<n+1>.json`  — full ranked function list with source refs
- `reports/diff_<n>_<n+1>.md`    — human-readable top-5 remediation report
- `reports/diff_<n>_<n+1>.svg`   — call graph, red intensity ∝ leaked bytes

## Automated regression gate (GitHub Actions)

See [`.github/workflows/README.md`](.github/workflows/README.md) for a
ready-to-drop-in workflow that:

- Fires on every successful `staging` deployment (via `deployment_status`).
- Captures **5 snapshots over 15 minutes** from the deployed processor.
- Aggregates the diffs into a first→last window and ranks the top 5 leakers.
- Posts a **sticky PR comment** listing each offender with its
  `file:line` source reference.
- **Fails the check** — turning the PR check red — when any function's
  flat `inuse_space` delta exceeds **500 KB** over the window.
- Uploads all profiles + reports as a workflow artifact so anyone can
  reproduce the analysis locally.

Dry-run the comment renderer against your own snapshots with
`./scripts/ci/simulate.sh [threshold-bytes]`.

## Why `inuse_space`?

`alloc_space` counts every byte ever allocated — useful for GC pressure analysis but noisy for leak hunting. `inuse_space` reports live bytes at the moment the profile was taken, so the delta between two snapshots is exactly *"bytes retained that were not freed"* — the definition of a leak candidate.

## Retention leak vs. GC thrashing / allocation churn

A heap regression is not always a leak. A hot path allocating multi-MB slices per
request and immediately dropping them will not grow `inuse_space` — but it *will*
blow up GC frequency and pause time. To tell the two apart, the workflow now:

1. Polls `/debug/memstats` on the target process alongside every heap profile.
   The sample processor exposes it out of the box; production services need the
   4-line handler shown below.
2. Diffs the first and last `gcstats_*.json` to compute `TotalAlloc` delta,
   sustained alloc rate (bytes/s), `NumGC` delta, GC frequency (cycles/s), avg
   pause, `HeapInuse` delta, and a **churn ratio** = `TotalAlloc Δ` / `HeapInuse Δ`.
3. Classifies the regression:

   | Verdict | Meaning | Triggers |
   | --- | --- | --- |
   | `RETENTION_LEAK` | Live memory kept growing — real leak | any function `flat_delta` ≥ 500 KB |
   | `ALLOC_CHURN` | GC thrash — high alloc rate, `HeapInuse` flat | churn ratio ≥ 20× **and** (GC ≥ 1 cycle/s **or** alloc ≥ 5 MB/s) |
   | `MIXED` | Both signals present | both above true |
   | `CLEAN` | Neither — ship it | otherwise |

All non-`CLEAN` verdicts fail the workflow. The PR comment carries a verdict
badge, a **GC & allocation metrics** table, and verdict-specific remediation
hints (e.g. `sync.Pool` / `GOGC` tuning for churn, top-N + source lines for a
leak).

### Exposing `/debug/memstats` on your service

```go
import (
    "encoding/json"
    "net/http"
    "runtime"
)

http.HandleFunc("/debug/memstats", func(w http.ResponseWriter, r *http.Request) {
    var ms runtime.MemStats
    runtime.ReadMemStats(&ms)
    w.Header().Set("Content-Type", "application/json")
    _ = json.NewEncoder(w).Encode(&ms)
})
```

Override the path with `gomem capture --memstats-path /internal/memstats` if
you already expose it elsewhere.

### Extra CLI

```
gomem gcstats --base http://host:6060 [--out gcstats_now.json]
gomem gcdiff  --dir  ./profiles       [--out gcdiff.json]
```
