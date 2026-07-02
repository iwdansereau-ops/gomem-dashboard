#!/usr/bin/env bash
# staging-capture.sh — capture heap profiles from a staging Go processor,
# diff consecutive pairs, and emit SVG + Markdown + JSON reports.
#
# Usage: staging-capture.sh <pprof-base-url> <interval-seconds> <snapshot-count>
# Example: staging-capture.sh http://staging-processor.internal:6060 300 12
set -euo pipefail

URL="${1:-}"
INTERVAL="${2:-30}"
COUNT="${3:-6}"
PROFILE_DIR="${PROFILE_DIR:-$(pwd)/profiles}"
REPORT_DIR="${REPORT_DIR:-$(pwd)/reports}"
GOMEM="${GOMEM:-$(dirname "$0")/../bin/gomem}"

if [[ -z "$URL" ]]; then
  echo "usage: $0 <pprof-base-url> [interval-seconds=30] [snapshot-count=6]" >&2
  echo "example: $0 http://staging-processor.internal:6060 300 12" >&2
  exit 2
fi

if [[ ! -x "$GOMEM" ]]; then
  echo "gomem binary not found at $GOMEM — run: go build -o bin/gomem ./cmd/gomem" >&2
  exit 3
fi

echo "==> pre-flight: verifying pprof endpoint"
if ! curl -fsS --max-time 10 "$URL/debug/pprof/" -o /dev/null; then
  echo "!! could not reach $URL/debug/pprof/ — is the service running with net/http/pprof enabled?" >&2
  exit 4
fi

mkdir -p "$PROFILE_DIR" "$REPORT_DIR"

echo "==> capturing $COUNT heap snapshots from $URL every ${INTERVAL}s"
"$GOMEM" capture --url "$URL" --dir "$PROFILE_DIR" \
  --interval "${INTERVAL}s" --count "$COUNT"

echo "==> generating diff reports for every consecutive pair"
"$GOMEM" report --dir "$PROFILE_DIR" --out "$REPORT_DIR" --top 5

echo "==> done."
echo "    profiles: $PROFILE_DIR"
echo "    reports : $REPORT_DIR"
echo "    view    : $GOMEM serve --dir $PROFILE_DIR --reports $REPORT_DIR"
