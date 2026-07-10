#!/usr/bin/env bash
# scripts/ci/avpt-preserve.sh
#
# AVPT "Preserve" phase for gomem-dashboard.
#
# Captures a *safe*, read-only snapshot of benchmark + config metadata so a
# later phase (or a human) can compare "what we shipped" against a known-good
# baseline. It writes nothing outside the output directory, mutates no repo
# state, and records no secrets — only public build/config metadata.
#
# Snapshot contents (best-effort; missing inputs are recorded as absent):
#   - meta.json         : git SHA/branch/describe, timestamp, go version
#   - go.mod / go.sum   : module + dependency pins (verbatim copy)
#   - go-env.txt        : `go env` (module/toolchain config, no secrets)
#   - workflows.txt     : inventory of .github/workflows/*.yml
#   - bench.txt         : `go test -run=^$ -bench=.` output when benchmarks
#                         exist (skipped otherwise / when go is absent)
#
# Usage: scripts/ci/avpt-preserve.sh [output-dir]
#        default output-dir: ./avpt-snapshot
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"

OUT_DIR="${1:-$REPO_ROOT/avpt-snapshot}"
mkdir -p "$OUT_DIR"

log() { printf '[avpt-preserve] %s\n' "$*"; }

git_sha="$(git rev-parse HEAD 2>/dev/null || echo unknown)"
git_branch="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)"
git_describe="$(git describe --tags --always --dirty 2>/dev/null || echo unknown)"
go_version="$(command -v go >/dev/null 2>&1 && go version || echo 'go: not installed')"
ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

log "Writing metadata snapshot to $OUT_DIR"

cat > "$OUT_DIR/meta.json" <<JSON
{
  "phase": "preserve",
  "dry_run": true,
  "generated_at": "$ts",
  "git": {
    "sha": "$git_sha",
    "branch": "$git_branch",
    "describe": "$git_describe"
  },
  "go_version": "$go_version"
}
JSON

# Verbatim config/dependency pins — the "config metadata" half of the snapshot.
for f in go.mod go.sum; do
  if [[ -f "$f" ]]; then
    cp "$f" "$OUT_DIR/$f"
    log "captured $f"
  fi
done

if command -v go >/dev/null 2>&1; then
  go env > "$OUT_DIR/go-env.txt" 2>/dev/null || true
fi

# Workflow inventory so we can see which CI gates were active at snapshot time.
{
  echo "# .github/workflows inventory @ $git_sha"
  ls -1 .github/workflows/*.yml 2>/dev/null || echo "(none)"
} > "$OUT_DIR/workflows.txt"

# Benchmark metadata — the "benchmark" half of the snapshot. Only run when the
# module actually declares benchmarks so this stays fast and side-effect free.
if command -v go >/dev/null 2>&1 && grep -rql '^func Benchmark' --include='*_test.go' . 2>/dev/null; then
  log "Benchmarks detected — capturing baseline (bench only, no unit tests)"
  go test -run='^$' -bench=. -benchmem ./... > "$OUT_DIR/bench.txt" 2>&1 || \
    log "benchmark run reported non-zero; output retained in bench.txt"
else
  echo "no benchmarks found (or go unavailable) at $ts" > "$OUT_DIR/bench.txt"
  log "No benchmarks to capture."
fi

log "Preserve snapshot complete:"
ls -la "$OUT_DIR"
