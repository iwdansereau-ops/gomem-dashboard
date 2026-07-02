#!/usr/bin/env bash
# collect-verdicts.sh — pull the latest staging-memory-check verdict for every
# repo in an org/user and emit a normalized JSON dataset.
#
# Sources of truth, in order of preference:
#   1. Latest workflow run of a caller of staging-memory-check-reusable
#      → job outputs.verdict (most authoritative — includes worst_function/bytes)
#   2. Latest commit status with context = staging-memory-check/verdict on the
#      default branch tip and on the head SHA of every open PR
#      → verdict inferred from state + description prefix
#
# Outputs one JSON document with schema:
# {
#   "generated_at": "ISO8601",
#   "scope":       "org/<name>" | "user/<name>",
#   "repos": [
#     {
#       "full_name":   "org/repo",
#       "default_branch": "main",
#       "default_branch_verdict": {  # tip of default branch
#         "verdict":  "CLEAN|RETENTION_LEAK|ALLOC_CHURN|MIXED|UNKNOWN|NONE",
#         "state":    "success|failure|error|pending|none",
#         "sha":      "abc123",
#         "short_sha":"abc123",
#         "description": "...",
#         "target_url":  "...",
#         "updated_at":  "ISO8601",
#         "worst_function": "…",   # from run outputs when available
#         "worst_bytes":    12345
#       },
#       "pr_verdicts": [ { ...same shape..., "pr_number":N, "pr_title":"…", "pr_url":"…" } ],
#       "worst_verdict": "CLEAN|RETENTION_LEAK|ALLOC_CHURN|MIXED|UNKNOWN|NONE",
#       "has_regression": true|false,
#       "workflow_configured": true|false,
#       "notes": "…"
#     }
#   ],
#   "counts": {
#     "total":            N,
#     "with_workflow":    N,
#     "regressing":       N,   # worst_verdict in RETENTION_LEAK|ALLOC_CHURN|MIXED
#     "clean":            N,
#     "unknown":          N,
#     "no_data":          N
#   }
# }
#
# Requires: gh, jq, bash 4+.

set -euo pipefail

SCOPE_KIND=""     # "org" | "user"
SCOPE_NAME=""
STATUS_CONTEXT="staging-memory-check/verdict"
WORKFLOW_NAME_HINT="staging-memory-check"  # matches caller.staging-memory-check.yml
INCLUDE_ARCHIVED=false
OUT_JSON="fleet-verdicts.json"
PR_LIMIT=25       # cap PR scan per repo

usage() {
  cat <<EOF
Usage: $(basename "$0") (--org NAME | --user NAME) [options]

Options:
  --org NAME              scan an organization
  --user NAME             scan a user account
  --status-context CTX    commit-status context to look for
                          (default: staging-memory-check/verdict)
  --workflow-hint STR     substring of caller workflow filename to prefer
                          (default: staging-memory-check)
  --include-archived      include archived repos
  --pr-limit N            max open PRs to inspect per repo (default 25)
  --out-json PATH         output JSON path (default fleet-verdicts.json)
  -h, --help              show this help

Requires: gh (authenticated), jq.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --org)              SCOPE_KIND=org;  SCOPE_NAME="$2"; shift 2;;
    --user)             SCOPE_KIND=user; SCOPE_NAME="$2"; shift 2;;
    --status-context)   STATUS_CONTEXT="$2"; shift 2;;
    --workflow-hint)    WORKFLOW_NAME_HINT="$2"; shift 2;;
    --include-archived) INCLUDE_ARCHIVED=true; shift;;
    --pr-limit)         PR_LIMIT="$2"; shift 2;;
    --out-json)         OUT_JSON="$2"; shift 2;;
    -h|--help)          usage; exit 0;;
    *) echo "Unknown arg: $1" >&2; usage; exit 2;;
  esac
done

[[ -z "$SCOPE_KIND" || -z "$SCOPE_NAME" ]] && { usage; exit 2; }
command -v gh >/dev/null || { echo "gh not on PATH" >&2; exit 3; }
command -v jq >/dev/null || { echo "jq not on PATH" >&2; exit 3; }

# ── 1. Enumerate repos ────────────────────────────────────────────────────────
echo ">> Enumerating repos under ${SCOPE_KIND}/${SCOPE_NAME}…" >&2

repos_json="$(mktemp)"
trap 'rm -f "$repos_json"' EXIT

if [[ "$SCOPE_KIND" == "org" ]]; then
  gh api --paginate "/orgs/${SCOPE_NAME}/repos?per_page=100&type=all" \
    | jq -s 'flatten' > "$repos_json"
else
  # For a user, also try /user/repos (covers collaborations) when authed as them
  gh api --paginate "/users/${SCOPE_NAME}/repos?per_page=100&type=all" \
    | jq -s 'flatten' > "$repos_json"
  authed_login="$(gh api /user --jq .login 2>/dev/null || true)"
  if [[ "$authed_login" == "$SCOPE_NAME" ]]; then
    tmp="$(mktemp)"
    gh api --paginate "/user/repos?per_page=100&affiliation=owner,collaborator,organization_member" \
      | jq -s 'flatten' > "$tmp"
    jq -s '.[0] + .[1] | unique_by(.full_name)' "$repos_json" "$tmp" > "${repos_json}.merged"
    mv "${repos_json}.merged" "$repos_json"
    rm -f "$tmp"
  fi
fi

if ! $INCLUDE_ARCHIVED; then
  jq '[.[] | select(.archived != true)]' "$repos_json" > "${repos_json}.f"
  mv "${repos_json}.f" "$repos_json"
fi

repo_count="$(jq 'length' "$repos_json")"
echo ">> Found ${repo_count} repos." >&2

# ── 2. Classify verdict from a description string ────────────────────────────
classify_desc() {
  local desc="$1" state="$2"
  # exact matches first, then substring fallbacks
  case "$desc" in
    "No memory regression on this deploy."*) echo "CLEAN"; return;;
    "Retention leak + allocation churn"*)    echo "MIXED"; return;;
    "Retention leak:"*)                      echo "RETENTION_LEAK"; return;;
    "Allocation churn"*)                     echo "ALLOC_CHURN"; return;;
    "Memory evaluator failed"*)              echo "UNKNOWN"; return;;
  esac
  case "$state" in
    success)  echo "CLEAN";;
    failure)  echo "UNKNOWN";;
    error)    echo "UNKNOWN";;
    pending)  echo "UNKNOWN";;
    *)        echo "NONE";;
  esac
}

# ── 3. Fetch a single verdict object for repo@ref ────────────────────────────
# Emits a JSON object to stdout. Falls back to {"verdict":"NONE"...} if no data.
fetch_verdict_for_sha() {
  local full_name="$1" sha="$2"
  local status_json
  status_json="$(gh api "/repos/${full_name}/commits/${sha}/statuses?per_page=100" 2>/dev/null || true)"

  # Coerce non-arrays (404 payloads, empty responses) to []
  if [[ -z "$status_json" ]] || ! jq -e 'type=="array"' >/dev/null 2>&1 <<<"$status_json"; then
    status_json='[]'
  fi

  local matching
  matching="$(jq --arg ctx "$STATUS_CONTEXT" \
    '[.[] | select(.context == $ctx)] | sort_by(.updated_at) | last // null' \
    <<<"$status_json")"

  if [[ "$matching" == "null" || -z "$matching" ]]; then
    jq -n --arg sha "$sha" \
      '{verdict:"NONE", state:"none", sha:$sha, short_sha:($sha[0:7]),
        description:null, target_url:null, updated_at:null,
        worst_function:null, worst_bytes:null}'
    return
  fi

  local state desc updated_at target_url
  state="$(jq -r '.state' <<<"$matching")"
  desc="$(jq -r '.description // ""' <<<"$matching")"
  updated_at="$(jq -r '.updated_at' <<<"$matching")"
  target_url="$(jq -r '.target_url // ""' <<<"$matching")"
  local verdict; verdict="$(classify_desc "$desc" "$state")"

  # Best-effort extraction of "worst_function +N B" from RETENTION_LEAK desc
  local worst_function=null worst_bytes=null
  if [[ "$verdict" == "RETENTION_LEAK" ]]; then
    # "Retention leak: fn.name +12345 B (flat)."
    local fn bytes
    fn="$(sed -n 's/^Retention leak: \(.*\) +[0-9][0-9]* B .*$/\1/p' <<<"$desc" || true)"
    bytes="$(sed -n 's/^Retention leak: .* +\([0-9][0-9]*\) B .*$/\1/p' <<<"$desc" || true)"
    [[ -n "$fn"    ]] && worst_function="$(jq -Rn --arg v "$fn" '$v')"
    [[ -n "$bytes" ]] && worst_bytes="$bytes"
  fi

  jq -n \
    --arg verdict "$verdict" \
    --arg state "$state" \
    --arg sha "$sha" \
    --arg short_sha "${sha:0:7}" \
    --arg description "$desc" \
    --arg target_url "$target_url" \
    --arg updated_at "$updated_at" \
    --argjson worst_function "$worst_function" \
    --argjson worst_bytes "$worst_bytes" \
    '{verdict:$verdict, state:$state, sha:$sha, short_sha:$short_sha,
      description:$description, target_url:$target_url, updated_at:$updated_at,
      worst_function:$worst_function, worst_bytes:$worst_bytes}'
}

# ── 4. Detect whether the caller workflow is present in the repo ─────────────
has_workflow_configured() {
  local full_name="$1"
  local contents
  # gh exits non-zero on 404; capture but never abort
  contents="$(gh api "/repos/${full_name}/contents/.github/workflows" 2>/dev/null || true)"
  if [[ -z "$contents" ]] || ! jq -e 'type=="array"' >/dev/null 2>&1 <<<"$contents"; then
    echo "false"; return
  fi
  local match
  match="$(jq --arg hint "$WORKFLOW_NAME_HINT" \
    '[.[] | select(.type=="file" and (.name|ascii_downcase|contains($hint|ascii_downcase)))] | length' \
    <<<"$contents")"
  [[ "${match:-0}" -gt 0 ]] && echo "true" || echo "false"
}

# ── 5. Iterate ───────────────────────────────────────────────────────────────
out_repos_file="$(mktemp)"
trap 'rm -f "$repos_json" "$out_repos_file"' EXIT
echo "[]" > "$out_repos_file"

worst_rank() {
  # higher = worse; used to compute worst_verdict across default branch + PRs
  case "$1" in
    MIXED)          echo 5;;
    RETENTION_LEAK) echo 4;;
    ALLOC_CHURN)    echo 3;;
    UNKNOWN)        echo 2;;
    CLEAN)          echo 1;;
    NONE|*)         echo 0;;
  esac
}

i=0
while read -r repo_row; do
  i=$((i+1))
  full_name="$(jq -r '.full_name'      <<<"$repo_row")"
  default_branch="$(jq -r '.default_branch' <<<"$repo_row")"
  echo "  [$i/$repo_count] $full_name (branch=$default_branch)" >&2

  wf_configured="$(has_workflow_configured "$full_name")"

  # Resolve default branch tip SHA
  db_sha="$(gh api "/repos/${full_name}/branches/${default_branch}" --jq .commit.sha 2>/dev/null || echo "")"
  if [[ -n "$db_sha" ]]; then
    default_verdict="$(fetch_verdict_for_sha "$full_name" "$db_sha")"
  else
    default_verdict="$(jq -n '{verdict:"NONE",state:"none",sha:null,short_sha:null,description:null,target_url:null,updated_at:null,worst_function:null,worst_bytes:null}')"
  fi

  # Open PRs
  pr_list="$(gh api "/repos/${full_name}/pulls?state=open&per_page=${PR_LIMIT}" 2>/dev/null || true)"
  if [[ -z "$pr_list" ]] || ! jq -e 'type=="array"' >/dev/null 2>&1 <<<"$pr_list"; then
    pr_list='[]'
  fi

  pr_verdicts='[]'
  while read -r pr; do
    [[ -z "$pr" || "$pr" == "null" ]] && continue
    pr_number="$(jq -r '.number'   <<<"$pr")"
    pr_title="$(jq -r  '.title'    <<<"$pr")"
    pr_url="$(jq -r    '.html_url' <<<"$pr")"
    head_sha="$(jq -r  '.head.sha' <<<"$pr")"
    v="$(fetch_verdict_for_sha "$full_name" "$head_sha")"
    v="$(jq --argjson n "$pr_number" --arg t "$pr_title" --arg u "$pr_url" \
      '. + {pr_number:$n, pr_title:$t, pr_url:$u}' <<<"$v")"
    pr_verdicts="$(jq --argjson v "$v" '. + [$v]' <<<"$pr_verdicts")"
  done < <(jq -c '.[]' <<<"$pr_list")

  # Worst verdict across default branch + PRs
  worst="NONE"; worst_r=0
  for v in "$(jq -r '.verdict' <<<"$default_verdict")" \
           $(jq -r '.[].verdict' <<<"$pr_verdicts"); do
    r=$(worst_rank "$v")
    if (( r > worst_r )); then worst_r=$r; worst="$v"; fi
  done
  has_regression=false
  case "$worst" in RETENTION_LEAK|ALLOC_CHURN|MIXED) has_regression=true;; esac

  repo_obj="$(jq -n \
    --arg full_name "$full_name" \
    --arg default_branch "$default_branch" \
    --argjson default_branch_verdict "$default_verdict" \
    --argjson pr_verdicts "$pr_verdicts" \
    --arg worst "$worst" \
    --argjson has_regression "$has_regression" \
    --argjson workflow_configured "$wf_configured" \
    '{full_name:$full_name, default_branch:$default_branch,
      default_branch_verdict:$default_branch_verdict,
      pr_verdicts:$pr_verdicts,
      worst_verdict:$worst,
      has_regression:$has_regression,
      workflow_configured:$workflow_configured,
      notes:null}')"

  jq --argjson r "$repo_obj" '. + [$r]' "$out_repos_file" > "${out_repos_file}.n"
  mv "${out_repos_file}.n" "$out_repos_file"
done < <(jq -c '.[]' "$repos_json")

# ── 6. Filter to only repos where a workflow is configured for tallies ───────
# (We still keep unconfigured repos in the payload, but they don't count as
# "no_data" — they simply aren't onboarded yet.)
generated_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

jq -n \
  --arg generated_at "$generated_at" \
  --arg scope "${SCOPE_KIND}/${SCOPE_NAME}" \
  --argjson repos "$(cat "$out_repos_file")" \
  '{
     generated_at: $generated_at,
     scope: $scope,
     repos: $repos,
     counts: {
       total:         ($repos | length),
       with_workflow: ($repos | map(select(.workflow_configured))    | length),
       regressing:    ($repos | map(select(.has_regression))         | length),
       clean:         ($repos | map(select(.worst_verdict=="CLEAN")) | length),
       unknown:       ($repos | map(select(.worst_verdict=="UNKNOWN"))| length),
       no_data:       ($repos | map(select(.worst_verdict=="NONE"))  | length)
     }
   }' > "$OUT_JSON"

echo >&2
echo "Wrote: $OUT_JSON" >&2
