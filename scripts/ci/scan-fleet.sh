#!/usr/bin/env bash
# scripts/ci/scan-fleet.sh
#
# Enumerate every repo in a GitHub organization (or a user account) and
# report which Go services are already gomem-ready and which still need
# the /debug/memstats handler + workflow.
#
# For each repo we:
#   1. Shallow-clone the default branch (fast; no history).
#   2. Locate go.mod files (root and sub-modules).
#   3. Grep the Go source for the three signals a service needs:
#         - `import _ "net/http/pprof"` (or aliased variants)
#         - `runtime.ReadMemStats(`
#         - a handler wired to the literal `/debug/memstats` route
#      (or `/internal/memstats` — configurable via --route-regex).
#   4. Check for an existing `staging-memory-check` workflow file.
#
# Output:
#   * fleet-scan.json  — structured scan results (one object per repo)
#   * fleet-scan.md    — human-readable onboarding checklist grouped by state
#
# Usage:
#   scripts/ci/scan-fleet.sh --org my-org           # scans a GitHub org
#   scripts/ci/scan-fleet.sh --user my-handle       # scans a user account
#   scripts/ci/scan-fleet.sh --org my-org --include-archived
#   scripts/ci/scan-fleet.sh --org my-org --route-regex '/(debug|internal)/memstats'
#   scripts/ci/scan-fleet.sh --org my-org --workdir /tmp/scan --keep
#
# Requires: gh (authenticated), git, jq. Read scope on the target org's
# private repos is required to include them in the scan.

set -euo pipefail

TARGET=""
TARGET_KIND=""      # org | user
INCLUDE_ARCHIVED="false"
ROUTE_REGEX='/debug/memstats'
WORKDIR="$(mktemp -d -t gomem-fleet-scan.XXXXXXXX)"
KEEP_WORKDIR="false"
OUT_JSON="fleet-scan.json"
OUT_MD="fleet-scan.md"

usage() {
  sed -n '2,30p' "$0"
  exit 2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --org)                TARGET="$2"; TARGET_KIND="org";  shift 2 ;;
    --user)               TARGET="$2"; TARGET_KIND="user"; shift 2 ;;
    --include-archived)   INCLUDE_ARCHIVED="true";         shift ;;
    --route-regex)        ROUTE_REGEX="$2";                shift 2 ;;
    --workdir)            WORKDIR="$2";                    shift 2 ;;
    --keep)               KEEP_WORKDIR="true";             shift ;;
    --out-json)           OUT_JSON="$2";                   shift 2 ;;
    --out-md)             OUT_MD="$2";                     shift 2 ;;
    -h|--help)            usage ;;
    *) echo "unknown flag: $1" >&2; usage ;;
  esac
done

[[ -z "$TARGET" ]] && { echo "--org OR --user is required" >&2; usage; }
for c in gh git jq; do
  command -v "$c" >/dev/null 2>&1 || { echo "$c not on PATH" >&2; exit 3; }
done

mkdir -p "$WORKDIR"
trap '[[ "$KEEP_WORKDIR" == "true" ]] || rm -rf "$WORKDIR"' EXIT

# ─────────────────────────────────────────────────────────────────────
# 1. List repos in the target scope
# ─────────────────────────────────────────────────────────────────────
echo ">> Enumerating repos under ${TARGET_KIND}/${TARGET}…" >&2
if [[ "$TARGET_KIND" == "user" ]]; then
  # For a user scope, also include repos where the authenticated caller is
  # a collaborator or org member; /users/:user/repos alone hides those.
  {
    gh api --paginate "/users/${TARGET}/repos?per_page=100&type=all" \
      --jq '.[] | {full_name, default_branch, language, archived, private, size}' 2>/dev/null || true
    ME=$(gh api /user --jq .login 2>/dev/null || echo '')
    if [[ "$ME" == "$TARGET" ]]; then
      gh api --paginate "/user/repos?per_page=100&affiliation=owner,collaborator,organization_member" \
        --jq '.[] | {full_name, default_branch, language, archived, private, size}' 2>/dev/null || true
    fi
  } | jq -c 'select(.full_name != null)' | sort -u > "$WORKDIR/repos.jsonl"
else
  gh api --paginate "/orgs/${TARGET}/repos?per_page=100&type=all" \
    --jq '.[] | {full_name, default_branch, language, archived, private, size}' \
    > "$WORKDIR/repos.jsonl"
fi

TOTAL=$(wc -l < "$WORKDIR/repos.jsonl" | tr -d ' ')
echo ">> Found ${TOTAL} repos." >&2

# ─────────────────────────────────────────────────────────────────────
# 2. Scan each repo
# ─────────────────────────────────────────────────────────────────────
: > "$WORKDIR/results.jsonl"

while IFS= read -r repo; do
  full=$(echo "$repo" | jq -r .full_name)
  branch=$(echo "$repo" | jq -r '.default_branch // "main"')
  archived=$(echo "$repo" | jq -r '.archived')
  language=$(echo "$repo" | jq -r '.language // ""')

  if [[ "$archived" == "true" && "$INCLUDE_ARCHIVED" != "true" ]]; then
    echo "  skip (archived): $full" >&2
    continue
  fi

  # Cheap prefilter: only clone if either the language is Go or the repo
  # has any file with `.go` extension via the search API. Most orgs have a
  # long tail of non-Go repos and cloning all of them wastes bandwidth.
  if [[ "$language" != "Go" ]]; then
    # Check contents API for a root go.mod — very cheap.
    if ! gh api "/repos/$full/contents/go.mod?ref=$branch" >/dev/null 2>&1; then
      # Fall back: search the repo's code index for filename:go.mod.
      # (May return 0 hits for very new repos; that's fine — we skip.)
      hits=$(gh api "/search/code?q=filename:go.mod+repo:$full" --jq '.total_count // 0' 2>/dev/null || echo 0)
      if [[ "$hits" == "0" ]]; then
        jq -nc \
          --arg full "$full" --arg branch "$branch" --arg lang "$language" \
          --argjson archived "$archived" \
          '{full_name:$full, default_branch:$branch, language:$lang, archived:$archived,
            status:"not-go", has_go_mod:false, implements_memstats_handler:false,
            go_mod_paths:[], main_go_paths:[], readmemstats_paths:[],
            pprof_import_paths:[], memstats_route_paths:[], has_staging_workflow:false}' \
          >> "$WORKDIR/results.jsonl"
        continue
      fi
    fi
  fi

  echo "  scan: $full ($branch)" >&2
  dest="$WORKDIR/clones/$(echo "$full" | tr / _)"
  rm -rf "$dest"
  if ! git clone -q --depth 1 --branch "$branch" \
        "https://github.com/$full.git" "$dest" 2>/dev/null; then
    echo "    !! clone failed, skipping" >&2
    jq -nc --arg full "$full" --arg branch "$branch" \
      '{full_name:$full, default_branch:$branch, status:"clone-failed",
        has_go_mod:false, implements_memstats_handler:false,
        go_mod_paths:[], main_go_paths:[], readmemstats_paths:[],
        pprof_import_paths:[], memstats_route_paths:[], has_staging_workflow:false}' \
      >> "$WORKDIR/results.jsonl"
    continue
  fi

  # ── grep for signals ─────────────────────────────────────────────
  gomods=$(find "$dest" -name go.mod \
              -not -path '*/vendor/*' -not -path '*/.git/*' 2>/dev/null \
            | sed "s#^$dest/##" | jq -R . | jq -sc . 2>/dev/null || echo '[]')
  mains=$(find "$dest" -name main.go \
              -not -path '*/vendor/*' -not -path '*/.git/*' 2>/dev/null \
            | sed "s#^$dest/##" | jq -R . | jq -sc . 2>/dev/null || echo '[]')

  # Grep helpers: capture "path:line" hits.
  gg() {
    grep -rnI --include='*.go' \
         --exclude-dir=vendor --exclude-dir=.git \
         -E "$1" "$dest" 2>/dev/null \
      | sed "s#^$dest/##" \
      | head -50 \
      | jq -R . | jq -sc .
  }
  readms=$(gg 'runtime\.ReadMemStats\(')
  pprof=$(gg '"net/http/pprof"')
  route=$(gg "$ROUTE_REGEX")

  workflow="false"
  if compgen -G "$dest/.github/workflows/*staging-memory-check*" >/dev/null 2>&1; then
    workflow="true"
  fi

  has_go_mod=$([[ "$(echo "$gomods" | jq 'length')" -gt 0 ]] && echo true || echo false)
  # A repo is "ready" iff it has: go.mod AND ReadMemStats AND a route hit.
  # net/http/pprof is nice but not strictly required — someone could
  # expose pprof via a different framework.
  ready=false
  if [[ "$has_go_mod" == "true" \
     && "$(echo "$readms" | jq 'length')" -gt 0 \
     && "$(echo "$route" | jq 'length')" -gt 0 ]]; then
    ready=true
  fi

  status="needs-handler"
  if [[ "$has_go_mod" != "true" ]]; then
    status="not-go"
  elif [[ "$ready" == "true" ]]; then
    status="ready"
  fi

  jq -nc \
    --arg full "$full" --arg branch "$branch" --arg lang "$language" \
    --arg status "$status" \
    --argjson archived "$archived" \
    --argjson has_go_mod "$has_go_mod" \
    --argjson ready "$ready" \
    --argjson gomods "$gomods" \
    --argjson mains "$mains" \
    --argjson readms "$readms" \
    --argjson pprof "$pprof" \
    --argjson route "$route" \
    --argjson wf "$([[ "$workflow" == "true" ]] && echo true || echo false)" \
    '{full_name:$full, default_branch:$branch, language:$lang, archived:$archived,
      status:$status, has_go_mod:$has_go_mod, implements_memstats_handler:$ready,
      go_mod_paths:$gomods, main_go_paths:$mains,
      readmemstats_paths:$readms, pprof_import_paths:$pprof,
      memstats_route_paths:$route, has_staging_workflow:$wf}' \
    >> "$WORKDIR/results.jsonl"

  rm -rf "$dest"     # reclaim disk as we go
done < "$WORKDIR/repos.jsonl"

# ─────────────────────────────────────────────────────────────────────
# 3. Emit outputs
# ─────────────────────────────────────────────────────────────────────
jq -s --arg target "$TARGET" --arg kind "$TARGET_KIND" \
  '{scanned_at: (now | strftime("%Y-%m-%dT%H:%M:%SZ")),
    target: $target, target_kind: $kind,
    counts: {
      total: (length),
      not_go: [.[] | select(.status=="not-go")] | length,
      ready:  [.[] | select(.status=="ready")]  | length,
      needs_handler: [.[] | select(.status=="needs-handler")] | length,
      clone_failed:  [.[] | select(.status=="clone-failed")]  | length
    },
    repos: .}' \
  "$WORKDIR/results.jsonl" > "$OUT_JSON"

# Human-readable checklist grouped by status.
{
  echo "# Fleet memory-check readiness — ${TARGET_KIND}/${TARGET}"
  echo
  echo "_Scanned: $(date -u +%Y-%m-%dT%H:%M:%SZ)_"
  echo
  jq -r '. as $root
    | "**Totals:** \($root.counts.total) repos · " +
      "✅ ready: \($root.counts.ready) · " +
      "🟡 needs handler: \($root.counts.needs_handler) · " +
      "⚪ not Go: \($root.counts.not_go) · " +
      "⚠️ clone failed: \($root.counts.clone_failed)"' "$OUT_JSON"
  echo

  echo "## ✅ Ready — turn on the gate"
  echo
  echo "These repos already expose \`/debug/memstats\` and call \`runtime.ReadMemStats\`. Drop in the 6-line caller and configure branch protection."
  echo
  jq -r '.repos[] | select(.status=="ready") |
    ([.memstats_route_paths[] | split(":")[0]] | unique | join(", ")) as $paths |
    "- [ ] **\(.full_name)** \u2014 branch `\(.default_branch)`, workflow present: \(.has_staging_workflow)\n      Handler in: \($paths)"' "$OUT_JSON"
  echo

  echo "## 🟡 Needs handler — add /debug/memstats"
  echo
  echo "Go repos that don't yet expose the endpoint. Add the 4-line handler (see repo README) and secrets, then enable the workflow."
  echo
  jq -r '.repos[] | select(.status=="needs-handler") |
    (if (.go_mod_paths|length)>0  then (.go_mod_paths|join(", "))  else "(none found)" end) as $gomods |
    (if (.main_go_paths|length)>0 then (.main_go_paths|join(", ")) else "(none)"       end) as $mains  |
    ((.pprof_import_paths|length)>0) as $has_pprof |
    ((.readmemstats_paths|length)>0) as $has_readms |
    "- [ ] **\(.full_name)** \u2014 branch `\(.default_branch)`\n      go.mod files: \($gomods)\n      main.go files: \($mains)\n      Has pprof import: \($has_pprof)\n      Has runtime.ReadMemStats: \($has_readms)"' "$OUT_JSON"
  echo

  echo "## ⚪ Not Go — nothing to do"
  echo
  jq -r '.repos[] | select(.status=="not-go") | "- \(.full_name)"' "$OUT_JSON"
  echo

  clone_failed=$(jq '.counts.clone_failed' "$OUT_JSON")
  if [[ "$clone_failed" -gt 0 ]]; then
    echo "## ⚠️ Clone failed"
    echo
    echo "These couldn't be scanned. Check permissions or run manually."
    echo
    jq -r '.repos[] | select(.status=="clone-failed") | "- \(.full_name)"' "$OUT_JSON"
  fi
} > "$OUT_MD"

echo
echo "Wrote:"
echo "  $OUT_JSON"
echo "  $OUT_MD"
