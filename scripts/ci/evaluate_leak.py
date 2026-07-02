#!/usr/bin/env python3
"""
evaluate_leak.py — read every diff_*.json produced by `gomem report`, read
the first & last gcstats_*.json snapshots produced by `gomem capture`, and
classify the observed heap-growth pattern:

  RETENTION_LEAK — HeapInuse and/or per-function flat delta grew steadily,
                   and TotalAlloc / retained bytes is a normal (~1–10×)
                   ratio. Classic leak.
  ALLOC_CHURN    — HeapInuse stayed roughly flat but TotalAlloc and NumGC
                   climbed sharply. GC is doing its job; the code is
                   allocating heavily on hot paths. This is a *performance*
                   regression, not a leak.
  MIXED          — HeapInuse grew AND the churn ratio is very high.
                   Something is both leaking AND allocating hot. Both
                   remediations are recommended.
  CLEAN          — Neither retention nor churn thresholds tripped.

Emits:
  1. stdout — JSON blob consumed by later workflow steps
  2. --out-comment — Markdown PR comment
  3. --out-summary — appended GitHub Actions step summary
"""
from __future__ import annotations

import argparse
import glob
import json
import os
import pathlib
import sys
from typing import Any


# ─── unit helpers ────────────────────────────────────────────────────────────

def human_bytes(n: int | float) -> str:
    try:
        n = int(n)
    except (TypeError, ValueError):
        return str(n)
    neg = "-" if n < 0 else ""
    v = abs(n)
    KB, MB, GB = 1024, 1024 * 1024, 1024 * 1024 * 1024
    if v >= GB:
        return f"{neg}{v/GB:.2f} GB"
    if v >= MB:
        return f"{neg}{v/MB:.2f} MB"
    if v >= KB:
        return f"{neg}{v/KB:.1f} KB"
    return f"{neg}{v} B"


def human_rate(bps: float) -> str:
    return f"{human_bytes(int(bps))}/s"


def trim_source(path: str) -> str:
    if not path:
        return ""
    parts = path.split("/")
    if len(parts) <= 3:
        return path
    return ".../" + "/".join(parts[-3:])


# ─── report loading ──────────────────────────────────────────────────────────

def load_diff_reports(reports_dir: pathlib.Path) -> list[dict[str, Any]]:
    files = sorted(reports_dir.glob("diff_*.json"))
    out = []
    for f in files:
        try:
            out.append(json.loads(f.read_text()))
        except Exception as e:  # noqa: BLE001
            print(f"warning: failed to parse {f}: {e}", file=sys.stderr)
    return out


def build_full_window_diff(reports: list[dict[str, Any]]) -> dict[str, Any] | None:
    if not reports:
        return None
    if len(reports) == 1:
        return reports[0]

    totals_delta = 0
    totals_before = reports[0].get("total_inuse_before_bytes", 0)
    totals_after = reports[-1].get("total_inuse_after_bytes", 0)
    for r in reports:
        totals_delta += int(r.get("total_inuse_delta_bytes", 0))

    agg: dict[str, dict[str, Any]] = {}
    for r in reports:
        for fn in r.get("top_functions", []):
            key = fn["function"]
            slot = agg.setdefault(
                key,
                {
                    "function": key,
                    "file": fn.get("file", ""),
                    "line": fn.get("line", 0),
                    "flat_delta": 0,
                    "cum_delta": 0,
                },
            )
            slot["flat_delta"] += int(fn.get("flat_delta", 0))
            slot["cum_delta"] += int(fn.get("cum_delta", 0))
            if not slot["file"] and fn.get("file"):
                slot["file"] = fn["file"]
                slot["line"] = fn.get("line", 0)

    top = sorted(agg.values(), key=lambda f: f["flat_delta"], reverse=True)
    return {
        "generated_at": reports[-1].get("generated_at"),
        "base_file": reports[0].get("base_file"),
        "current_file": reports[-1].get("current_file"),
        "total_inuse_before_bytes": totals_before,
        "total_inuse_after_bytes": totals_after,
        "total_inuse_delta_bytes": totals_delta,
        "top_functions": top[:5],
    }


# ─── gcstats loading ─────────────────────────────────────────────────────────

def load_gcstats_window(profiles_dir: pathlib.Path) -> dict[str, Any] | None:
    """Read the first and last gcstats_*.json, compute the delta.

    Returns None if fewer than 2 gcstats snapshots are available — the
    caller then omits the GC section from the comment.
    """
    files = sorted(profiles_dir.glob("gcstats_*.json"))
    if len(files) < 2:
        return None
    try:
        base = json.loads(files[0].read_text())
        cur = json.loads(files[-1].read_text())
    except Exception as e:  # noqa: BLE001
        print(f"warning: failed to parse gcstats: {e}", file=sys.stderr)
        return None

    def _t(x):
        # ISO-8601 with 'Z' suffix — datetime.fromisoformat handles it in 3.11+
        # but be defensive.
        from datetime import datetime
        s = x["captured_at"].replace("Z", "+00:00")
        return datetime.fromisoformat(s)

    dur = max((_t(cur) - _t(base)).total_seconds(), 1.0)
    total_alloc_delta = int(cur["TotalAlloc"]) - int(base["TotalAlloc"])
    mallocs_delta = int(cur["Mallocs"]) - int(base["Mallocs"])
    frees_delta = int(cur["Frees"]) - int(base["Frees"])
    num_gc_delta = int(cur["NumGC"]) - int(base["NumGC"])
    pause_ns_delta = int(cur["PauseTotalNs"]) - int(base["PauseTotalNs"])
    heap_inuse_delta = int(cur["HeapInuse"]) - int(base["HeapInuse"])
    heap_objects_delta = int(cur["HeapObjects"]) - int(base["HeapObjects"])

    alloc_rate = total_alloc_delta / dur
    gc_per_sec = num_gc_delta / dur
    avg_pause_ms = (pause_ns_delta / num_gc_delta / 1e6) if num_gc_delta > 0 else 0.0

    if heap_inuse_delta > 0:
        churn_ratio: float | None = total_alloc_delta / heap_inuse_delta
    elif total_alloc_delta > 0:
        churn_ratio = None  # "infinite" — allocated everything, retained ≤ 0
    else:
        churn_ratio = 0.0

    return {
        "base_file": files[0].name,
        "current_file": files[-1].name,
        "duration_seconds": dur,
        "total_alloc_delta_bytes": total_alloc_delta,
        "mallocs_delta": mallocs_delta,
        "frees_delta": frees_delta,
        "num_gc_delta": num_gc_delta,
        "pause_ns_delta": pause_ns_delta,
        "heap_inuse_delta_bytes": heap_inuse_delta,
        "heap_objects_delta": heap_objects_delta,
        "alloc_rate_bytes_per_sec": alloc_rate,
        "gc_per_sec": gc_per_sec,
        "avg_gc_pause_ms": avg_pause_ms,
        "churn_ratio": churn_ratio,
        "end_heap_inuse_bytes": int(cur["HeapInuse"]),
        "end_num_gc": int(cur["NumGC"]),
        "gc_cpu_fraction_end": float(cur.get("GCCPUFraction", 0.0)),
    }


# ─── classifier ──────────────────────────────────────────────────────────────

# Tunable thresholds. Deliberately conservative — false positives on this
# classification would waste engineer time.
CHURN_RATIO_THRESHOLD = 20.0     # bytes allocated per byte retained
CHURN_GC_PER_SEC_THRESHOLD = 1.0  # ≥ 1 GC/s over the 15-min window is aggressive
CHURN_ALLOC_RATE_THRESHOLD = 5 * 1024 * 1024  # ≥ 5 MB/s sustained allocation


def classify(window: dict[str, Any], gc: dict[str, Any] | None,
             leak_threshold: int) -> tuple[str, str, list[str]]:
    """Return (label, one_line_verdict, bullet_reasons)."""
    top = window.get("top_functions", [])
    worst_flat = int(top[0]["flat_delta"]) if top else 0
    total_inuse_delta = int(window.get("total_inuse_delta_bytes", 0))
    leak_signal = worst_flat > leak_threshold

    if gc is None:
        # Without GC stats we can only decide on retention.
        if leak_signal:
            return (
                "RETENTION_LEAK",
                "Retention leak suspected (GC stats unavailable — see setup notes).",
                [
                    f"Worst per-function flat delta {human_bytes(worst_flat)} > "
                    f"threshold {human_bytes(leak_threshold)}.",
                    "No `/debug/memstats` endpoint reachable, so allocation-churn "
                    "vs retention could not be distinguished automatically.",
                ],
            )
        return (
            "CLEAN",
            "No regression detected (GC stats unavailable).",
            [f"Worst per-function flat delta {human_bytes(worst_flat)} stayed "
             f"under {human_bytes(leak_threshold)}."],
        )

    ratio = gc["churn_ratio"]
    alloc_rate = gc["alloc_rate_bytes_per_sec"]
    gc_per_sec = gc["gc_per_sec"]
    heap_inuse_delta = gc["heap_inuse_delta_bytes"]

    churn_signal = (
        (ratio is None or ratio >= CHURN_RATIO_THRESHOLD)
        and (gc_per_sec >= CHURN_GC_PER_SEC_THRESHOLD
             or alloc_rate >= CHURN_ALLOC_RATE_THRESHOLD)
    )

    reasons: list[str] = []
    if leak_signal:
        reasons.append(
            f"Per-function retention: `{top[0]['function']}` retained "
            f"{human_bytes(worst_flat)} (> {human_bytes(leak_threshold)} threshold)."
        )
        reasons.append(
            f"HeapInuse Δ over the window: {human_bytes(heap_inuse_delta)}."
        )
    if churn_signal:
        ratio_str = "∞ (retained ≤ 0)" if ratio is None else f"{ratio:.1f}×"
        reasons.append(
            f"Allocation churn: {human_rate(alloc_rate)} allocated, "
            f"{gc_per_sec:.2f} GC/s, churn ratio {ratio_str} "
            f"(threshold {CHURN_RATIO_THRESHOLD:.0f}×)."
        )
        reasons.append(
            f"NumGC Δ: {gc['num_gc_delta']} cycles in "
            f"{gc['duration_seconds']:.0f}s, avg pause "
            f"{gc['avg_gc_pause_ms']:.2f}ms, GC CPU fraction "
            f"{gc['gc_cpu_fraction_end']*100:.1f}%."
        )

    if leak_signal and churn_signal:
        return (
            "MIXED",
            "Both retention leak AND allocation churn detected.",
            reasons,
        )
    if leak_signal:
        # A leak that's *not* churny: normal (< 20×) alloc-to-retention ratio.
        ratio_str = "n/a" if ratio is None else f"{ratio:.1f}×"
        reasons.append(
            f"Allocation-to-retention ratio {ratio_str} is within normal "
            f"range — this looks like retention, not GC thrash."
        )
        return ("RETENTION_LEAK", "True retention leak detected.", reasons)
    if churn_signal:
        return (
            "ALLOC_CHURN",
            "Allocation churn / GC thrash — not a leak.",
            reasons,
        )
    return (
        "CLEAN",
        "No regression detected.",
        [
            f"Worst per-function flat delta {human_bytes(worst_flat)} stayed under "
            f"{human_bytes(leak_threshold)}.",
            f"HeapInuse Δ: {human_bytes(heap_inuse_delta)}, "
            f"churn ratio {'n/a' if ratio is None else f'{ratio:.1f}×'}.",
        ],
    )


# ─── comment rendering ───────────────────────────────────────────────────────

LABEL_BADGES = {
    "RETENTION_LEAK": "🚨 Retention leak",
    "ALLOC_CHURN":    "⚠️ Allocation churn",
    "MIXED":          "🚨 Leak + churn",
    "CLEAN":          "✅ Clean",
}


def render_comment(
    label: str,
    verdict: str,
    reasons: list[str],
    window: dict[str, Any],
    gc: dict[str, Any] | None,
    threshold: int,
    sha: str,
    short_sha: str,
    run_url: str,
) -> str:
    top = window.get("top_functions", [])[:5]
    total_inuse = int(window.get("total_inuse_delta_bytes", 0))
    badge = LABEL_BADGES[label]

    if label == "CLEAN":
        header = (
            f"### ✅ Staging memory check passed on `{short_sha}`\n\n{verdict}"
        )
    else:
        header = (
            f"### {badge} on `{short_sha}`\n\n**{verdict}**"
        )

    lines: list[str] = [
        "<!-- gomem-staging-memory-check -->",
        header,
        "",
        f"- **Verdict:** `{label}`",
        f"- **Total `inuse_space` delta:** {human_bytes(total_inuse)}",
    ]
    if gc:
        lines += [
            f"- **HeapInuse delta (runtime.MemStats):** {human_bytes(gc['heap_inuse_delta_bytes'])}",
            f"- **TotalAlloc delta:** {human_bytes(gc['total_alloc_delta_bytes'])} "
            f"({human_rate(gc['alloc_rate_bytes_per_sec'])} sustained)",
            f"- **NumGC delta:** {gc['num_gc_delta']} cycles "
            f"({gc['gc_per_sec']:.2f} GC/s, avg pause {gc['avg_gc_pause_ms']:.2f} ms)",
        ]
    lines += [
        f"- **Snapshots:** 5 heap + 5 gcstats over "
        f"{int(gc['duration_seconds']) if gc else 900}s",
        f"- **Threshold per function:** {human_bytes(threshold)} (`flat_delta`)",
        f"- **Deployed commit:** [`{short_sha}`](../commit/{sha})",
        f"- **Full report + SVG call graph:** [workflow run]({run_url}) "
        f"(download the `gomem-staging-{short_sha}` artifact)",
        "",
    ]

    # Classification rationale
    if reasons:
        lines += ["#### Why this verdict", ""]
        for r in reasons:
            lines.append(f"- {r}")
        lines.append("")

    # GC / allocation-frequency section
    if gc is not None:
        if gc["churn_ratio"] is None:
            churn_cell = "∞ (retained ≤ 0)"
        else:
            churn_cell = f"{gc['churn_ratio']:.1f}×"
        lines += [
            "#### GC & allocation metrics (first → last snapshot)",
            "",
            "| Metric | Value |",
            "|---|---|",
            f"| `TotalAlloc` Δ | {human_bytes(gc['total_alloc_delta_bytes'])} |",
            f"| Sustained alloc rate | {human_rate(gc['alloc_rate_bytes_per_sec'])} |",
            f"| `NumGC` Δ | {gc['num_gc_delta']} cycles |",
            f"| GC frequency | {gc['gc_per_sec']:.2f} /s |",
            f"| Avg GC pause | {gc['avg_gc_pause_ms']:.2f} ms |",
            f"| GC CPU fraction (end) | {gc['gc_cpu_fraction_end']*100:.2f}% |",
            f"| `HeapInuse` Δ | {human_bytes(gc['heap_inuse_delta_bytes'])} |",
            f"| `HeapObjects` Δ | {gc['heap_objects_delta']:+,} |",
            f"| Churn ratio (alloc/retained) | {churn_cell} |",
            "",
        ]
        # Interpretive hint
        if label == "ALLOC_CHURN":
            lines += [
                "> ℹ️  **Interpretation:** the process is allocating aggressively but "
                "GC is reclaiming the bytes each cycle — `HeapInuse` stayed roughly flat "
                "while `TotalAlloc` and `NumGC` climbed. This is a **CPU / latency** "
                "regression (GC pause growth, wasted allocation on hot paths), not a "
                "memory leak. Look for temporary slice/map allocations inside tight "
                "loops that could be pooled with `sync.Pool` or hoisted out of the hot "
                "path.",
                "",
            ]
        elif label == "MIXED":
            lines += [
                "> ℹ️  **Interpretation:** heap growth is driven partly by retention "
                "AND partly by allocation churn. Both remediations below are relevant.",
                "",
            ]
        elif label == "RETENTION_LEAK":
            lines += [
                "> ℹ️  **Interpretation:** allocation-to-retention ratio is normal — "
                "bytes allocated are staying live. This looks like a real leak, not "
                "GC thrash.",
                "",
            ]

    # Top functions
    if top:
        lines += [
            "#### Top 5 functions by retained bytes",
            "",
            "| # | Function | Flat Δ | Cum Δ | Source |",
            "|--:|----------|-------:|------:|--------|",
        ]
        for i, fn in enumerate(top, 1):
            flat = int(fn.get("flat_delta", 0))
            cum = int(fn.get("cum_delta", 0))
            src = trim_source(fn.get("file", ""))
            line_no = fn.get("line", 0)
            src_cell = f"`{src}:{line_no}`" if src else "—"
            marker = " 🚨" if flat > threshold else ""
            lines.append(
                f"| {i}{marker} | `{fn['function']}` | "
                f"{human_bytes(flat)} | {human_bytes(cum)} | {src_cell} |"
            )
        lines.append("")

    # Remediation
    if label in ("RETENTION_LEAK", "MIXED"):
        offenders = [f for f in top if int(f.get("flat_delta", 0)) > threshold]
        if offenders:
            lines += ["#### Suggested next steps (retention)", ""]
            for fn in offenders:
                src = trim_source(fn.get("file", ""))
                line_no = fn.get("line", 0)
                src_ref = f"`{src}:{line_no}`" if src else "the function above"
                lines.append(
                    f"- Inspect {src_ref}: unbounded slice/map appends · "
                    "missing cache eviction · goroutine blocked on channel · "
                    "`sync.Pool` retention · unclosed body / rows / file handle."
                )
            lines.append("")
    if label in ("ALLOC_CHURN", "MIXED"):
        lines += [
            "#### Suggested next steps (allocation churn)",
            "",
            "- Profile with `go tool pprof -alloc_objects` (or the artifact's "
            "profiles) to find hot allocation sites — these are usually more "
            "actionable than the `inuse_space` top-N for churn regressions.",
            "- Look for per-request allocations that could reuse buffers via "
            "`sync.Pool` or pre-sized slices/maps.",
            "- Check for `[]byte(str)` / `string([]byte)` conversions in hot "
            "loops, `fmt.Sprintf` for concatenation, and log statements that "
            "format even when the level is disabled.",
            f"- GC frequency of {gc['gc_per_sec']:.2f}/s with "
            f"{gc['avg_gc_pause_ms']:.2f} ms average pause suggests tuning "
            "`GOGC` or `GOMEMLIMIT` if the fix isn't obvious from the code.",
            "",
        ]

    if label != "CLEAN":
        lines += [
            "Reproduce locally against the same commit:",
            "",
            "```bash",
            f"git checkout {sha}",
            "go build -o bin/gomem ./cmd/gomem",
            "./scripts/staging-capture.sh $STAGING_PPROF_URL 180 5",
            "./bin/gomem serve --dir ./profiles --reports ./reports",
            "```",
            "",
        ]

    lines.append(
        "_This comment is updated in place by the `staging-memory-check` "
        "workflow after every successful staging deploy._"
    )
    return "\n".join(lines) + "\n"


# ─── main ────────────────────────────────────────────────────────────────────

def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--reports-dir", required=True, type=pathlib.Path)
    ap.add_argument("--profiles-dir", required=True, type=pathlib.Path,
                    help="directory containing gcstats_*.json (usually same as capture dir)")
    ap.add_argument("--threshold-bytes", required=True, type=int)
    ap.add_argument("--sha", required=True)
    ap.add_argument("--short-sha", required=True)
    ap.add_argument("--run-url", required=True)
    ap.add_argument("--out-comment", required=True, type=pathlib.Path)
    ap.add_argument("--out-summary", required=True, type=pathlib.Path)
    args = ap.parse_args()

    reports = load_diff_reports(args.reports_dir)
    gc = load_gcstats_window(args.profiles_dir)

    if not reports:
        result = {
            "has_regression": False,
            "verdict": "INCONCLUSIVE",
            "worst_bytes": 0,
            "worst_function": "",
            "total_delta_bytes": 0,
            "note": "no diff reports generated",
        }
        print(json.dumps(result))
        args.out_comment.write_text(
            "<!-- gomem-staging-memory-check -->\n"
            f"### ⚠️ Staging memory check inconclusive on `{args.short_sha}`\n\n"
            "No diff reports were produced — check the workflow logs.\n"
        )
        try:
            with open(args.out_summary, "a") as f:
                f.write("## Staging memory check\n\n" + args.out_comment.read_text())
        except OSError:
            pass
        return 0

    window = build_full_window_diff(reports)
    top = window.get("top_functions", [])
    worst = top[0] if top else {"function": "", "flat_delta": 0}
    worst_bytes = int(worst.get("flat_delta", 0))

    label, verdict, reasons = classify(window, gc, args.threshold_bytes)
    # A regression = anything except CLEAN. Both RETENTION_LEAK and MIXED
    # fail the workflow. ALLOC_CHURN also fails, since it's a genuine
    # regression the user asked us to catch (just a different *kind*).
    has_regression = label != "CLEAN"

    comment = render_comment(
        label=label,
        verdict=verdict,
        reasons=reasons,
        window=window,
        gc=gc,
        threshold=args.threshold_bytes,
        sha=args.sha,
        short_sha=args.short_sha,
        run_url=args.run_url,
    )
    args.out_comment.write_text(comment)
    try:
        with open(args.out_summary, "a") as f:
            f.write("## Staging memory check\n\n" + comment)
    except OSError:
        pass

    result = {
        "has_regression": has_regression,
        "verdict": label,
        "worst_bytes": worst_bytes,
        "worst_function": worst.get("function", ""),
        "total_delta_bytes": int(window.get("total_inuse_delta_bytes", 0)),
        "top_count": len(top),
        "gc_available": gc is not None,
    }
    if gc:
        result["alloc_rate_bytes_per_sec"] = gc["alloc_rate_bytes_per_sec"]
        result["gc_per_sec"] = gc["gc_per_sec"]
        result["num_gc_delta"] = gc["num_gc_delta"]
        result["total_alloc_delta_bytes"] = gc["total_alloc_delta_bytes"]
        result["heap_inuse_delta_bytes"] = gc["heap_inuse_delta_bytes"]
        result["churn_ratio"] = gc["churn_ratio"]
    print(json.dumps(result))
    return 0


if __name__ == "__main__":
    sys.exit(main())
