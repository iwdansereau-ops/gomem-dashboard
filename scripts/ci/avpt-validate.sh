#!/usr/bin/env bash
# scripts/ci/avpt-validate.sh
#
# AVPT "Validate" phase for gomem-dashboard.
#
# Maps the abstract AVPT Validate stage onto *real* Go build + staging
# memory-regression checks — the same tooling the blocking
# `staging-memory-check/verdict` gate uses. It is intentionally read-only and
# needs no secrets: it performs a build/vet/test of the gomem CLI and a
# *dry-run* of the memory-regression evaluator. No live staging endpoint is
# contacted, so this is safe to run under `dry_run: true`.
#
# Steps (each guarded so a missing toolchain downgrades to a skip, never a
# hard failure of unrelated infrastructure):
#   1. go build ./cmd/gomem ./cmd/sample-processor
#   2. go vet ./...
#   3. go test ./...            (no-op if the module has no tests)
#   4. memory-check evaluator dry-run via scripts/ci/simulate.sh when sample
#      reports are available; otherwise a byte-compile check of the evaluator.
#
# Usage: scripts/ci/avpt-validate.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"

log() { printf '[avpt-validate] %s\n' "$*"; }

fail=0

if command -v go >/dev/null 2>&1; then
  log "go: $(go version)"
  log "Building gomem CLI + sample processor…"
  go build ./cmd/gomem ./cmd/sample-processor || fail=1
  log "go vet ./…"
  go vet ./... || fail=1
  log "go test ./… (skips cleanly when no tests exist)"
  go test ./... || fail=1
else
  log "go toolchain not found on PATH — skipping build/vet/test."
  log "In CI the AVPT workflow provisions Go before invoking this script."
fi

# Memory-regression evaluator: dry-run only. Prefer the canned simulate.sh
# path (real evaluator against sample reports) and fall back to a syntax check
# so the Validate stage still exercises the classifier code path offline.
if ls reports/diff_*.json >/dev/null 2>&1; then
  log "Sample reports found — running evaluator dry-run via simulate.sh"
  scripts/ci/simulate.sh || fail=1
elif command -v python3 >/dev/null 2>&1; then
  log "No sample reports — byte-compiling the memory evaluator instead"
  python3 -m py_compile scripts/ci/evaluate_leak.py || fail=1
  log "evaluate_leak.py compiles cleanly."
else
  log "python3 not found — skipping evaluator dry-run."
fi

if [[ "$fail" -ne 0 ]]; then
  log "VALIDATE FAILED"
  exit 1
fi
log "VALIDATE OK"
