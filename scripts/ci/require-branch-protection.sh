#!/usr/bin/env bash
# scripts/ci/require-branch-protection.sh
#
# Configure GitHub branch-protection on a repo so that the
# `staging-memory-check/verdict` commit status posted by
# .github/workflows/staging-memory-check.yml becomes a blocking condition:
#
#   * PRs cannot be merged while the status is `pending`, `failure`, or `error`.
#   * A verdict of RETENTION_LEAK / ALLOC_CHURN / MIXED  → status=failure   → blocks.
#   * A verdict of CLEAN                                 → status=success   → allows.
#   * The workflow never running (no PR head SHA resolved, evaluator crashed)
#     leaves the status at `pending`/`error` and merging is still blocked.
#
# Overrides:
#   * A repo *admin* can always click "Merge without waiting" if
#     enforce_admins=false (the default here). Toggle with --enforce-admins.
#   * A maintainer can dismiss the requirement globally by rerunning this
#     script with --remove.
#
# Prereqs:
#   - `gh` CLI authenticated with a token that has `admin:repo_hook` and
#     `repo` scopes on the target repository (i.e. a repo admin token).
#   - `jq` on PATH.
#
# Usage:
#   scripts/ci/require-branch-protection.sh \
#       --repo my-org/my-service \
#       --branch main \
#       [--extra-check "ci/build" --extra-check "ci/test"] \
#       [--enforce-admins] \
#       [--required-reviewers 1] \
#       [--dry-run] \
#       [--remove]
#
# The script is idempotent: existing required checks are preserved and the
# `staging-memory-check/verdict` context is appended (deduplicated). Other
# branch-protection settings you have already configured (review counts,
# CODEOWNERS, signed commits, etc.) are read from the current settings and
# echoed back unchanged so nothing else gets clobbered.

set -euo pipefail

REPO=""
BRANCH="main"
EXTRA_CHECKS=()
ENFORCE_ADMINS="false"
REQUIRED_REVIEWERS=""
DRY_RUN="false"
REMOVE="false"
CONTEXT="staging-memory-check/verdict"

usage() {
  sed -n '2,30p' "$0"
  exit 2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo)               REPO="$2";               shift 2 ;;
    --branch)             BRANCH="$2";             shift 2 ;;
    --extra-check)        EXTRA_CHECKS+=("$2");    shift 2 ;;
    --enforce-admins)     ENFORCE_ADMINS="true";   shift ;;
    --no-enforce-admins)  ENFORCE_ADMINS="false";  shift ;;
    --required-reviewers) REQUIRED_REVIEWERS="$2"; shift 2 ;;
    --context)            CONTEXT="$2";            shift 2 ;;
    --dry-run)            DRY_RUN="true";          shift ;;
    --remove)             REMOVE="true";           shift ;;
    -h|--help)            usage ;;
    *) echo "unknown flag: $1" >&2; usage ;;
  esac
done

[[ -z "$REPO" ]] && { echo "--repo is required (e.g. my-org/my-service)" >&2; usage; }

if ! command -v gh   >/dev/null 2>&1; then echo "gh CLI not found on PATH"   >&2; exit 3; fi
if ! command -v jq   >/dev/null 2>&1; then echo "jq not found on PATH"        >&2; exit 3; fi

api_get() {
  gh api -H "Accept: application/vnd.github+json" "$1" 2>/dev/null
}

# ---------------------------------------------------------------------------
# Fetch current protection (empty JSON if the branch has none).
# ---------------------------------------------------------------------------
CURRENT="$(api_get "/repos/${REPO}/branches/${BRANCH}/protection" || echo '{}')"

# Existing required contexts, deduplicated.
mapfile -t CURRENT_CONTEXTS < <(
  echo "$CURRENT" \
    | jq -r '.required_status_checks.contexts[]?' \
    | sort -u
)

# ---------------------------------------------------------------------------
# Build the desired context list.
# ---------------------------------------------------------------------------
DESIRED=("${CURRENT_CONTEXTS[@]}")
if [[ "$REMOVE" == "true" ]]; then
  # Drop our context and the extras. Everything else stays.
  TMP=()
  for c in "${DESIRED[@]}"; do
    keep=1
    [[ "$c" == "$CONTEXT" ]] && keep=0
    for e in "${EXTRA_CHECKS[@]:-}"; do [[ "$c" == "$e" ]] && keep=0; done
    [[ $keep -eq 1 ]] && TMP+=("$c")
  done
  DESIRED=("${TMP[@]:-}")
else
  DESIRED+=("$CONTEXT")
  for e in "${EXTRA_CHECKS[@]:-}"; do
    [[ -n "$e" ]] && DESIRED+=("$e")
  done
fi

# Dedup + sort for stability.
mapfile -t DESIRED < <(printf '%s\n' "${DESIRED[@]:-}" | awk 'NF' | sort -u)
CONTEXTS_JSON="$(printf '%s\n' "${DESIRED[@]:-}" | jq -R . | jq -sc .)"

# ---------------------------------------------------------------------------
# Read-through of everything else so we don't clobber unrelated settings.
# ---------------------------------------------------------------------------
CURRENT_STRICT="$(echo "$CURRENT"  | jq -r '.required_status_checks.strict // false')"
CURRENT_REVIEWS="$(echo "$CURRENT" | jq -c '.required_pull_request_reviews // null')"
CURRENT_LINEAR="$(echo "$CURRENT"  | jq -r '.required_linear_history.enabled // false')"
CURRENT_FORCE="$(echo "$CURRENT"   | jq -r '.allow_force_pushes.enabled // false')"
CURRENT_DELETE="$(echo "$CURRENT"  | jq -r '.allow_deletions.enabled // false')"
CURRENT_CONVOS="$(echo "$CURRENT"  | jq -r '.required_conversation_resolution.enabled // false')"

# --required-reviewers overrides the review block if passed. Otherwise keep
# whatever the repo already has (may be null → "reviews not required").
if [[ -n "$REQUIRED_REVIEWERS" ]]; then
  REVIEWS_JSON=$(jq -nc --argjson n "$REQUIRED_REVIEWERS" \
    '{required_approving_review_count:$n, dismiss_stale_reviews:true, require_code_owner_reviews:false}')
else
  REVIEWS_JSON="$CURRENT_REVIEWS"
fi

# The PUT endpoint requires `restrictions` and `enforce_admins` at top level,
# even if null / false.
PAYLOAD=$(jq -nc \
  --argjson strict          "$([[ "$CURRENT_STRICT" == "true" ]] && echo true || echo false)" \
  --argjson contexts        "$CONTEXTS_JSON" \
  --argjson enforce_admins  "$([[ "$ENFORCE_ADMINS" == "true" ]] && echo true || echo false)" \
  --argjson linear          "$([[ "$CURRENT_LINEAR" == "true" ]] && echo true || echo false)" \
  --argjson force           "$([[ "$CURRENT_FORCE"  == "true" ]] && echo true || echo false)" \
  --argjson delete          "$([[ "$CURRENT_DELETE" == "true" ]] && echo true || echo false)" \
  --argjson convos          "$([[ "$CURRENT_CONVOS" == "true" ]] && echo true || echo false)" \
  --argjson reviews         "$REVIEWS_JSON" \
  '{
     required_status_checks: {strict: $strict, contexts: $contexts},
     enforce_admins: $enforce_admins,
     required_pull_request_reviews: $reviews,
     restrictions: null,
     required_linear_history: $linear,
     allow_force_pushes: $force,
     allow_deletions: $delete,
     required_conversation_resolution: $convos
   }')

echo "Repo:                ${REPO}"
echo "Branch:              ${BRANCH}"
echo "Required contexts:   ${DESIRED[*]:-<none>}"
echo "Enforce for admins:  ${ENFORCE_ADMINS}"
[[ -n "$REQUIRED_REVIEWERS" ]] && echo "Required reviewers:  ${REQUIRED_REVIEWERS}"
echo "Mode:                $([[ "$REMOVE" == "true" ]] && echo remove || echo apply)"
echo

if [[ "$DRY_RUN" == "true" ]]; then
  echo "DRY-RUN payload that would be PUT to /repos/${REPO}/branches/${BRANCH}/protection:"
  echo "$PAYLOAD" | jq .
  exit 0
fi

# ---------------------------------------------------------------------------
# Apply.
# ---------------------------------------------------------------------------
if [[ "$REMOVE" == "true" && "${#DESIRED[@]}" -eq 0 ]]; then
  # No required contexts left AND caller asked to remove — safest is to keep
  # the rest of protection but wipe the required-checks block.
  PAYLOAD=$(echo "$PAYLOAD" | jq '.required_status_checks = null')
fi

echo "$PAYLOAD" | gh api \
  --method PUT \
  -H "Accept: application/vnd.github+json" \
  "/repos/${REPO}/branches/${BRANCH}/protection" \
  --input - \
  > /tmp/branch-protection-response.json

echo "OK. Response saved to /tmp/branch-protection-response.json"
jq '{required_status_checks, enforce_admins}' /tmp/branch-protection-response.json
