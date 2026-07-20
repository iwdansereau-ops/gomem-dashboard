#!/usr/bin/env python3
"""
Regression tests for the fleet-dashboard privacy invariant:

    Private repositories must NEVER appear in any public artifact — neither the
    rendered Markdown/HTML dashboard nor the committed last-verdicts.json
    baseline.

Run with the stdlib test runner (no third-party deps):

    python3 -m unittest discover -s scripts/ci/tests -p 'test_*.py' -v

or directly:

    python3 scripts/ci/tests/test_privacy.py
"""

from __future__ import annotations

import importlib.util
import json
import subprocess
import sys
import unittest
from pathlib import Path

CI_DIR = Path(__file__).resolve().parents[1]          # scripts/ci
REPO_ROOT = CI_DIR.parents[1]                          # repo root
SNAPSHOT = REPO_ROOT / ".dashboard" / "last-verdicts.json"
ASSERT_SCRIPT = CI_DIR / "assert-public-only.py"


def _load_module(name: str, path: Path):
    spec = importlib.util.spec_from_file_location(name, path)
    assert spec and spec.loader
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


render = _load_module("render_dashboard", CI_DIR / "render-dashboard.py")

# Marker strings that identify the private repo in the fixture. If any of these
# leak into rendered output, the test fails.
PRIVATE_MARKERS = [
    "acme-internal/secret-service",
    "SECRET PR TITLE do not leak",
    "cafef00d",  # private short_sha
]


def _fixture_with_private_regression() -> dict:
    """A snapshot with one PRIVATE and one PUBLIC repo, both regressing, so the
    renderer would surface both if it didn't filter."""
    def regressing(full_name: str, private: bool, sha: str, pr_title: str) -> dict:
        return {
            "full_name": full_name,
            "default_branch": "main",
            "private": private,
            "default_branch_verdict": {
                "verdict": "RETENTION_LEAK", "state": "failure",
                "sha": sha + "0" * 33, "short_sha": sha,
                "description": f"Retention leak: pkg.Fn +999 B (flat).",
                "target_url": "https://example.com/run/1",
                "updated_at": "2026-07-20T00:00:00Z",
                "worst_function": "pkg.Fn", "worst_bytes": 999,
            },
            "pr_verdicts": [{
                "verdict": "MIXED", "state": "failure",
                "sha": sha + "f" * 33, "short_sha": sha,
                "description": "Retention leak + allocation churn.",
                "target_url": None, "updated_at": None,
                "worst_function": None, "worst_bytes": None,
                "pr_number": 42, "pr_title": pr_title,
                "pr_url": f"https://github.com/{full_name}/pull/42",
            }],
            "worst_verdict": "MIXED", "has_regression": True,
            "workflow_configured": True, "notes": None,
        }

    return {
        "generated_at": "2026-07-20T01:00:00Z",
        "scope": "user/acme-internal",
        "repos": [
            regressing("acme-internal/secret-service", True, "cafef00d",
                       "SECRET PR TITLE do not leak"),
            regressing("acme-internal/public-widget", False, "0badf00d",
                       "public PR title ok"),
        ],
        "counts": {"total": 2, "with_workflow": 2, "regressing": 2,
                   "clean": 0, "unknown": 0, "no_data": 0},
    }


class TestRendererExcludesPrivate(unittest.TestCase):
    def setUp(self) -> None:
        self.data = _fixture_with_private_regression()

    def test_markdown_has_no_private_repo(self) -> None:
        md = render.render_markdown(self.data, include_unknown=True)
        for marker in PRIVATE_MARKERS:
            self.assertNotIn(marker, md,
                             f"private marker {marker!r} leaked into Markdown")
        # Public repo must still be surfaced.
        self.assertIn("acme-internal/public-widget", md)

    def test_html_has_no_private_repo(self) -> None:
        htmlout = render.render_html(self.data, include_unknown=True)
        for marker in PRIVATE_MARKERS:
            self.assertNotIn(marker, htmlout,
                             f"private marker {marker!r} leaked into HTML")
        self.assertIn("acme-internal/public-widget", htmlout)

    def test_public_repos_helper(self) -> None:
        kept = render.public_repos(self.data["repos"])
        names = {r["full_name"] for r in kept}
        self.assertEqual(names, {"acme-internal/public-widget"})


class TestAssertPublicOnlyScript(unittest.TestCase):
    def _run(self, obj: dict, *extra: str) -> subprocess.CompletedProcess:
        import os
        import tempfile
        fd, path = tempfile.mkstemp(suffix=".json")
        os.close(fd)
        self.addCleanup(lambda: Path(path).unlink(missing_ok=True))
        Path(path).write_text(json.dumps(obj))
        return subprocess.run(
            [sys.executable, str(ASSERT_SCRIPT), path, *extra],
            capture_output=True, text=True,
        )

    def test_fails_on_private(self) -> None:
        obj = {"repos": [{"full_name": "a/b", "private": True}]}
        cp = self._run(obj)
        self.assertEqual(cp.returncode, 1, cp.stderr)
        self.assertIn("a/b", cp.stderr)

    def test_passes_on_public(self) -> None:
        obj = {"repos": [{"full_name": "a/b", "private": False}]}
        cp = self._run(obj)
        self.assertEqual(cp.returncode, 0, cp.stderr)

    def test_require_flag_fails_on_missing_field(self) -> None:
        obj = {"repos": [{"full_name": "a/b"}]}  # no private field
        cp = self._run(obj, "--require-flag")
        self.assertEqual(cp.returncode, 1, cp.stderr)


class TestCommittedSnapshotIsPublicOnly(unittest.TestCase):
    """The real committed baseline must never carry private fleet data."""

    def setUp(self) -> None:
        self.data = json.loads(SNAPSHOT.read_text())

    def test_every_repo_marked_public(self) -> None:
        for repo in self.data.get("repos", []):
            self.assertIn("private", repo,
                          f"{repo.get('full_name')} missing 'private' flag")
            self.assertIs(repo["private"], False,
                          f"{repo.get('full_name')} is private but committed")

    def test_assert_script_passes_on_committed_snapshot(self) -> None:
        cp = subprocess.run(
            [sys.executable, str(ASSERT_SCRIPT), str(SNAPSHOT), "--require-flag"],
            capture_output=True, text=True,
        )
        self.assertEqual(cp.returncode, 0, cp.stderr)

    def test_no_known_private_repo_names_present(self) -> None:
        # Independent of the 'private' flag: the raw bytes of the committed file
        # must not mention any repo we know to be private. Guards against a
        # future regression that re-adds private rows without the flag.
        raw = SNAPSHOT.read_text()
        known_private = [
            "obsidian-vault", "library-of-alexandria", "skyrim-crashlogs",
            "odisena-master", "odisena-ai-gateway", "avpt-cicd-dashboard",
            "perf-infra", "terraform-qs-refresh", "otelharness",
        ]
        for name in known_private:
            self.assertNotIn(name, raw,
                             f"known-private repo {name!r} present in snapshot")


if __name__ == "__main__":
    unittest.main(verbosity=2)
