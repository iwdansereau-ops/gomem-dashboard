#!/usr/bin/env python3
"""
assert-public-only.py — fail if a fleet-verdicts snapshot contains any private
repository.

This is the last line of defense before world-readable fleet data is published
to GitHub Pages or committed to the repo. It runs in fleet-dashboard.yml right
after collect-verdicts.sh, and can also be used as a pre-commit guard for
.dashboard/last-verdicts.json.

A repo is considered private if its object has "private": true. As a stricter
check, --require-flag also fails if any repo is missing the "private" field
entirely (i.e. was produced by a collector that predates the privacy fix and
therefore cannot prove the repo is public).

Exit codes:
  0  no private repos found
  1  at least one private repo (or, with --require-flag, a repo missing the flag)
  2  usage / bad input

Usage:
    assert-public-only.py snapshot.json [snapshot2.json ...] [--require-flag]
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any


def offending_repos(data: dict[str, Any], require_flag: bool) -> list[str]:
    bad: list[str] = []
    for repo in data.get("repos", []) or []:
        name = repo.get("full_name", "<unknown>")
        if repo.get("private") is True:
            bad.append(f"{name} (private=true)")
        elif require_flag and "private" not in repo:
            bad.append(f"{name} (missing 'private' flag)")
    return bad


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("snapshots", nargs="+", type=Path)
    p.add_argument("--require-flag", action="store_true",
                   help="also fail if any repo lacks an explicit 'private' field")
    args = p.parse_args()

    failed = False
    for path in args.snapshots:
        try:
            data = json.loads(path.read_text())
        except (OSError, json.JSONDecodeError) as e:
            print(f"::error::cannot read {path}: {e}", file=sys.stderr)
            return 2
        bad = offending_repos(data, args.require_flag)
        if bad:
            failed = True
            print(f"::error::{path} contains private fleet data that must not be "
                  f"published:", file=sys.stderr)
            for b in bad:
                print(f"  - {b}", file=sys.stderr)
        else:
            print(f"OK: {path} — no private repositories.")

    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
