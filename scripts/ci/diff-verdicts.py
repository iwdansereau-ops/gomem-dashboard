#!/usr/bin/env python3
"""
diff-verdicts.py — compare two fleet-verdicts.json snapshots and print any
NEW regressions (services or PRs that flipped from a non-regressing state
into RETENTION_LEAK / ALLOC_CHURN / MIXED since the previous snapshot).

Emits JSON on stdout with schema:
{
  "generated_at": "ISO8601 of the current snapshot",
  "scope":        "org/... or user/...",
  "new_regressions": [
    {
      "repo": "example-corp/order-router",
      "verdict": "RETENTION_LEAK",
      "ref_kind": "pr" | "default_branch",
      "ref_label": "PR #913 · c17aa72" | "main · c17aa72",
      "sha": "c17aa72...",
      "short_sha": "c17aa72",
      "worst_function": "(*orderCache).Put" | null,
      "worst_bytes": 812432 | null,
      "worst_bytes_human": "793.4 KiB",
      "description": "Retention leak: (*orderCache).Put +812432 B (flat).",
      "pr_number": 913 | null,
      "pr_title":  "cache orders by tenant id" | null,
      "pr_url":    "https://github.com/..." | null,
      "target_url":"https://github.com/.../actions/runs/42088",
      "previous_state": "CLEAN" | "NONE" | "UNKNOWN" | "ABSENT"
    }
  ]
}

Exit code: 0 always (empty new_regressions is not an error).

Usage:
    diff-verdicts.py --previous prev.json --current curr.json
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

REGRESSION_VERDICTS = {"RETENTION_LEAK", "ALLOC_CHURN", "MIXED"}


def fmt_bytes(n: int | None) -> str:
    if not n:
        return "?"
    units = ["B", "KiB", "MiB", "GiB"]
    v = float(n)
    for u in units:
        if v < 1024 or u == units[-1]:
            return f"{v:.1f} {u}" if u != "B" else f"{int(v)} B"
        v /= 1024
    return f"{n} B"


def index_snapshot(data: dict[str, Any]) -> dict[tuple[str, str, str], dict[str, Any]]:
    """Return {(repo, kind, key) -> verdict_entry} for every ref in a snapshot.

    kind = "default_branch" (key = "") or "pr" (key = pr_number-as-string).
    """
    out: dict[tuple[str, str, str], dict[str, Any]] = {}
    for repo in data.get("repos", []) or []:
        full = repo.get("full_name", "")
        db = repo.get("default_branch_verdict") or {}
        if db:
            out[(full, "default_branch", "")] = {**db, "repo": repo}
        for pr in repo.get("pr_verdicts", []) or []:
            key = str(pr.get("pr_number", pr.get("sha", "")))
            out[(full, "pr", key)] = {**pr, "repo": repo}
    return out


def previous_state(prev_entry: dict[str, Any] | None) -> str:
    if prev_entry is None:
        return "ABSENT"
    v = prev_entry.get("verdict") or "NONE"
    return v


def build_new_regression(
    repo_full: str,
    kind: str,
    curr: dict[str, Any],
    prev_state: str,
) -> dict[str, Any]:
    verdict = curr.get("verdict", "")
    sha = curr.get("sha") or ""
    short_sha = curr.get("short_sha") or (sha[:7] if sha else "")
    pr_number = curr.get("pr_number")
    pr_title = curr.get("pr_title")
    pr_url = curr.get("pr_url")
    default_branch = (curr.get("repo") or {}).get("default_branch") or "main"

    if kind == "pr" and pr_number is not None:
        ref_label = f"PR #{pr_number} · {short_sha}"
    else:
        ref_label = f"{default_branch} · {short_sha}"

    return {
        "repo": repo_full,
        "verdict": verdict,
        "ref_kind": kind,
        "ref_label": ref_label,
        "sha": sha,
        "short_sha": short_sha,
        "worst_function": curr.get("worst_function"),
        "worst_bytes": curr.get("worst_bytes"),
        "worst_bytes_human": fmt_bytes(curr.get("worst_bytes")),
        "description": curr.get("description"),
        "pr_number": pr_number,
        "pr_title": pr_title,
        "pr_url": pr_url,
        "target_url": curr.get("target_url"),
        "previous_state": prev_state,
    }


def diff(previous: dict[str, Any], current: dict[str, Any]) -> dict[str, Any]:
    prev_idx = index_snapshot(previous) if previous else {}
    curr_idx = index_snapshot(current)

    new_regressions: list[dict[str, Any]] = []
    for (repo, kind, key), curr in curr_idx.items():
        if curr.get("verdict") not in REGRESSION_VERDICTS:
            continue
        prev = prev_idx.get((repo, kind, key))
        prev_state = previous_state(prev)
        # A "new" regression is one where the previous state was NOT already
        # a regression verdict on the same (repo, kind, key). This dedupes
        # against reruns of the same failed check.
        if prev_state in REGRESSION_VERDICTS:
            # Still count it if the SHA changed (a newer commit re-regressed)
            prev_sha = (prev or {}).get("sha")
            if prev_sha == curr.get("sha"):
                continue
        new_regressions.append(build_new_regression(repo, kind, curr, prev_state))

    # Sort worst-first, then by repo
    order = {"MIXED": 0, "RETENTION_LEAK": 1, "ALLOC_CHURN": 2}
    new_regressions.sort(key=lambda r: (order.get(r["verdict"], 9), r["repo"]))

    return {
        "generated_at": current.get("generated_at"),
        "scope": current.get("scope"),
        "new_regressions": new_regressions,
    }


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--previous", type=Path, required=False,
                   help="Previous snapshot. If missing, treats every "
                        "current regression as new.")
    p.add_argument("--current", type=Path, required=True)
    args = p.parse_args()

    curr = json.loads(args.current.read_text())
    prev: dict[str, Any] = {}
    if args.previous and args.previous.exists():
        try:
            prev = json.loads(args.previous.read_text())
        except json.JSONDecodeError:
            prev = {}

    result = diff(prev, curr)
    json.dump(result, sys.stdout, indent=2)
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
