#!/usr/bin/env bash
# simulate.sh — dry-run the staging-memory-check workflow's evaluator step
# against whatever reports live in ./reports/. Prints the PR comment body
# the workflow would post.
#
# Usage: simulate.sh [threshold-bytes=512000]
set -euo pipefail

THRESHOLD="${1:-512000}"
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$REPO_ROOT"

if ! ls reports/diff_*.json >/dev/null 2>&1; then
  echo "No reports/diff_*.json found. Generate some first, e.g.:"
  echo "  ./scripts/staging-capture.sh http://localhost:6060 3 4"
  exit 1
fi

TMP="$(mktemp -d)"
python3 scripts/ci/evaluate_leak.py \
  --reports-dir ./reports \
  --profiles-dir ./profiles \
  --threshold-bytes "$THRESHOLD" \
  --sha        "0000000000000000000000000000000000000000" \
  --short-sha  "sim0000" \
  --run-url    "https://example.invalid/actions/runs/0" \
  --out-comment "$TMP/comment.md" \
  --out-summary "$TMP/summary.md"

echo "─────────────── PR COMMENT (threshold=${THRESHOLD} bytes) ───────────────"
cat "$TMP/comment.md"
echo "─────────────────────────────────────────────────────────────────────"
