#!/usr/bin/env python3
"""
render-dashboard.py — turn fleet-verdicts.json (from collect-verdicts.sh) into
two deliverables:

  1. fleet-dashboard.md   — human-readable single-pane summary. Only surfaces
                            repos with RETENTION_LEAK / ALLOC_CHURN / MIXED
                            verdicts. Everything else is collapsed into totals.
  2. fleet-dashboard.html — a self-contained HTML view for hosting on Pages /
                            embedding in a wiki. Same filtering rules.

Usage:
    render-dashboard.py --in fleet-verdicts.json \\
        --out-md fleet-dashboard.md \\
        --out-html fleet-dashboard.html \\
        [--include-unknown]   # also surface UNKNOWN (evaluator error) rows

The dashboard is intentionally opinionated: green rows are suppressed. If the
whole fleet is green the dashboard says exactly that in one line so nobody has
to hunt through PR checks.
"""

from __future__ import annotations

import argparse
import datetime as dt
import html
import json
import sys
from pathlib import Path
from typing import Any

REGRESSION_VERDICTS = {"RETENTION_LEAK", "ALLOC_CHURN", "MIXED"}

VERDICT_META = {
    "RETENTION_LEAK": {"emoji": "🔺", "color": "#b42318", "label": "Retention leak"},
    "ALLOC_CHURN":    {"emoji": "🌀", "color": "#b54708", "label": "Allocation churn"},
    "MIXED":          {"emoji": "☣️",  "color": "#7a271a", "label": "Leak + churn"},
    "UNKNOWN":        {"emoji": "❓", "color": "#475467", "label": "Evaluator error"},
    "CLEAN":          {"emoji": "✅", "color": "#067647", "label": "Clean"},
    "NONE":           {"emoji": "⚪", "color": "#98a2b3", "label": "No data"},
}


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


def worst_row(repo: dict[str, Any]) -> dict[str, Any] | None:
    """Return the single most-egregious verdict entry (default branch or PR)
    for a regressing repo. Falls back to default_branch_verdict."""
    candidates: list[tuple[int, dict[str, Any]]] = []
    rank = {"MIXED": 5, "RETENTION_LEAK": 4, "ALLOC_CHURN": 3, "UNKNOWN": 2,
            "CLEAN": 1, "NONE": 0}
    db = repo.get("default_branch_verdict") or {}
    if db.get("verdict"):
        candidates.append((rank.get(db["verdict"], 0), {**db, "source": "default"}))
    for pr in repo.get("pr_verdicts", []) or []:
        candidates.append((rank.get(pr.get("verdict", "NONE"), 0),
                           {**pr, "source": "pr"}))
    if not candidates:
        return None
    candidates.sort(key=lambda x: x[0], reverse=True)
    return candidates[0][1]


def load_data(path: Path) -> dict[str, Any]:
    return json.loads(path.read_text())


# ─────────────────────────────────────────────────────────── Markdown renderer

def render_markdown(data: dict[str, Any], include_unknown: bool) -> str:
    counts = data.get("counts", {})
    generated = data.get("generated_at", "?")
    scope = data.get("scope", "?")
    repos = data.get("repos", [])

    surface = {v for v in REGRESSION_VERDICTS}
    if include_unknown:
        surface.add("UNKNOWN")

    regressing = [r for r in repos if r.get("worst_verdict") in surface]
    # Sort worst first, then alphabetical
    order = {"MIXED": 0, "RETENTION_LEAK": 1, "ALLOC_CHURN": 2, "UNKNOWN": 3}
    regressing.sort(key=lambda r: (order.get(r.get("worst_verdict", ""), 9),
                                    r.get("full_name", "")))

    lines: list[str] = []
    lines.append(f"# 🧠 Memory Regression Dashboard — `{scope}`")
    lines.append("")
    lines.append(f"_Generated: {generated}_")
    lines.append("")
    lines.append("| Metric | Count |")
    lines.append("|---|---:|")
    lines.append(f"| Repositories scanned | {counts.get('total', 0)} |")
    lines.append(f"| Onboarded (workflow configured) | {counts.get('with_workflow', 0)} |")
    lines.append(f"| 🔺 Regressing | **{counts.get('regressing', 0)}** |")
    lines.append(f"| ✅ Clean | {counts.get('clean', 0)} |")
    lines.append(f"| ❓ Evaluator errored | {counts.get('unknown', 0)} |")
    lines.append(f"| ⚪ No verdict yet | {counts.get('no_data', 0)} |")
    lines.append("")

    if not regressing:
        lines.append("## 🎉 All clear")
        lines.append("")
        lines.append("No repository is currently reporting a `RETENTION_LEAK`, "
                     "`ALLOC_CHURN`, or `MIXED` verdict on either its default "
                     "branch or any open PR.")
        lines.append("")
        return "\n".join(lines)

    lines.append(f"## 🚨 {len(regressing)} service(s) with active memory regressions")
    lines.append("")
    lines.append("| Verdict | Repository | Ref | Worst offender | Δ retained | Evidence |")
    lines.append("|---|---|---|---|---:|---|")

    for r in regressing:
        meta = VERDICT_META[r["worst_verdict"]]
        worst = worst_row(r) or {}
        ref = "default branch"
        if worst.get("source") == "pr" and worst.get("pr_number"):
            ref = f"[PR #{worst['pr_number']}]({worst.get('pr_url','')}) `{worst.get('short_sha','')}`"
        else:
            ref = f"`{worst.get('short_sha','')}` on `{r.get('default_branch','?')}`"

        wf = worst.get("worst_function") or "—"
        wb = fmt_bytes(worst.get("worst_bytes"))
        target = worst.get("target_url") or ""
        evidence = f"[open]({target})" if target else "—"

        lines.append(
            f"| {meta['emoji']} **{meta['label']}** "
            f"| [{r['full_name']}](https://github.com/{r['full_name']}) "
            f"| {ref} "
            f"| `{wf}` "
            f"| {wb} "
            f"| {evidence} |"
        )

    lines.append("")
    lines.append("### Per-repo detail")
    lines.append("")
    for r in regressing:
        lines.append(f"#### {VERDICT_META[r['worst_verdict']]['emoji']} "
                     f"`{r['full_name']}` — {VERDICT_META[r['worst_verdict']]['label']}")
        db = r.get("default_branch_verdict") or {}
        if db.get("verdict") in surface:
            lines.append(f"- **{r['default_branch']}** `{db.get('short_sha','?')}` — "
                         f"{VERDICT_META[db['verdict']]['label']}: "
                         f"{db.get('description') or '—'}"
                         + (f"  \n  [workflow run]({db['target_url']})"
                            if db.get('target_url') else ""))
        for pr in r.get("pr_verdicts", []) or []:
            if pr.get("verdict") not in surface:
                continue
            lines.append(f"- **PR #{pr.get('pr_number')}** "
                         f"[{pr.get('pr_title','')}]({pr.get('pr_url','')}) "
                         f"`{pr.get('short_sha','?')}` — "
                         f"{VERDICT_META[pr['verdict']]['label']}: "
                         f"{pr.get('description') or '—'}")
        lines.append("")

    return "\n".join(lines)


# ─────────────────────────────────────────────────────────── HTML renderer

HTML_TEMPLATE = """<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>Memory Regression Dashboard — {scope}</title>
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <style>
    :root {{
      --bg:#0b0f17; --panel:#111827; --border:#1f2937;
      --fg:#e5e7eb; --muted:#94a3b8; --accent:#38bdf8;
      --red:#f87171; --amber:#fbbf24; --green:#34d399; --gray:#94a3b8;
    }}
    * {{ box-sizing:border-box; }}
    body {{
      margin:0; padding:24px; background:var(--bg); color:var(--fg);
      font: 14px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto,
            Helvetica, Arial, sans-serif;
    }}
    h1 {{ margin:0 0 4px; font-size:22px; letter-spacing:-0.01em; }}
    .sub {{ color:var(--muted); font-size:12px; margin-bottom:20px; }}
    .cards {{ display:grid; grid-template-columns:repeat(auto-fit,minmax(150px,1fr));
              gap:12px; margin-bottom:24px; }}
    .card {{ background:var(--panel); border:1px solid var(--border);
             border-radius:12px; padding:14px 16px; }}
    .card .label {{ font-size:11px; text-transform:uppercase; letter-spacing:0.08em;
                    color:var(--muted); }}
    .card .value {{ font-size:22px; font-weight:600; margin-top:4px; }}
    .card.red    .value {{ color:var(--red); }}
    .card.green  .value {{ color:var(--green); }}
    .card.amber  .value {{ color:var(--amber); }}
    .card.gray   .value {{ color:var(--gray); }}

    .section-title {{ font-size:16px; margin: 8px 0 12px; }}
    table {{ width:100%; border-collapse:collapse; background:var(--panel);
             border:1px solid var(--border); border-radius:12px; overflow:hidden; }}
    th, td {{ text-align:left; padding:10px 12px; border-bottom:1px solid var(--border);
              vertical-align: top; }}
    th {{ font-size:11px; text-transform:uppercase; letter-spacing:0.08em;
          color:var(--muted); background: #0f172a; }}
    tr:last-child td {{ border-bottom:none; }}
    tr:hover td {{ background:#0f172a; }}
    a {{ color:var(--accent); text-decoration:none; }}
    a:hover {{ text-decoration:underline; }}
    code {{ background:#0f172a; padding:1px 6px; border-radius:6px;
            font: 12px/1 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
            color:#e2e8f0; }}
    .pill {{ display:inline-block; padding:2px 8px; border-radius:999px;
             font-size:11px; font-weight:600; letter-spacing:0.03em; }}
    .pill.MIXED          {{ background:#450a0a; color:#fecaca; }}
    .pill.RETENTION_LEAK {{ background:#7f1d1d; color:#fee2e2; }}
    .pill.ALLOC_CHURN    {{ background:#78350f; color:#fed7aa; }}
    .pill.UNKNOWN        {{ background:#334155; color:#e2e8f0; }}
    .empty {{ background:var(--panel); border:1px solid var(--border);
              border-radius:12px; padding:32px; text-align:center;
              color:var(--muted); }}
    .empty h2 {{ color:var(--green); margin:0 0 8px; }}
    footer {{ margin-top:24px; color:var(--muted); font-size:12px; }}
  </style>
</head>
<body>
  <h1>🧠 Memory Regression Dashboard — <code>{scope}</code></h1>
  <div class="sub">Generated {generated} · showing services with active regressions</div>

  <div class="cards">
    <div class="card"><div class="label">Scanned</div><div class="value">{total}</div></div>
    <div class="card gray"><div class="label">Onboarded</div><div class="value">{with_workflow}</div></div>
    <div class="card red"><div class="label">Regressing</div><div class="value">{regressing}</div></div>
    <div class="card green"><div class="label">Clean</div><div class="value">{clean}</div></div>
    <div class="card amber"><div class="label">Evaluator errored</div><div class="value">{unknown}</div></div>
    <div class="card gray"><div class="label">No verdict yet</div><div class="value">{no_data}</div></div>
  </div>

  {body}

  <footer>
    Powered by <a href="https://github.com/iwdansereau-ops/gomem-dashboard">gomem-dashboard</a>
    · re-runs on cron via <code>.github/workflows/fleet-dashboard.yml</code>
    · only surfaces RETENTION_LEAK / ALLOC_CHURN / MIXED verdicts
  </footer>
</body>
</html>
"""


def render_html(data: dict[str, Any], include_unknown: bool) -> str:
    counts = data.get("counts", {})
    generated = data.get("generated_at", "?")
    scope = data.get("scope", "?")
    repos = data.get("repos", [])

    surface = {v for v in REGRESSION_VERDICTS}
    if include_unknown:
        surface.add("UNKNOWN")

    regressing = [r for r in repos if r.get("worst_verdict") in surface]
    order = {"MIXED": 0, "RETENTION_LEAK": 1, "ALLOC_CHURN": 2, "UNKNOWN": 3}
    regressing.sort(key=lambda r: (order.get(r.get("worst_verdict", ""), 9),
                                    r.get("full_name", "")))

    if not regressing:
        body = ("<div class='empty'><h2>🎉 All clear</h2>"
                "<div>No repository is currently reporting a "
                "<code>RETENTION_LEAK</code>, <code>ALLOC_CHURN</code>, or "
                "<code>MIXED</code> verdict.</div></div>")
    else:
        rows: list[str] = []
        for r in regressing:
            worst = worst_row(r) or {}
            v = r["worst_verdict"]
            meta = VERDICT_META[v]
            if worst.get("source") == "pr" and worst.get("pr_number"):
                ref = (f"<a href='{html.escape(worst.get('pr_url',''))}'>"
                       f"PR #{worst['pr_number']}</a> "
                       f"<code>{html.escape(worst.get('short_sha','') or '')}</code>")
            else:
                ref = (f"<code>{html.escape(worst.get('short_sha','') or '')}</code> on "
                       f"<code>{html.escape(r.get('default_branch','?'))}</code>")
            wf = html.escape(worst.get("worst_function") or "—")
            wb = fmt_bytes(worst.get("worst_bytes"))
            target = worst.get("target_url") or ""
            evidence = (f"<a href='{html.escape(target)}'>open</a>"
                        if target else "—")
            rows.append(
                f"<tr>"
                f"<td><span class='pill {v}'>{meta['emoji']} {meta['label']}</span></td>"
                f"<td><a href='https://github.com/{html.escape(r['full_name'])}'>"
                f"{html.escape(r['full_name'])}</a></td>"
                f"<td>{ref}</td>"
                f"<td><code>{wf}</code></td>"
                f"<td>{wb}</td>"
                f"<td>{evidence}</td>"
                f"</tr>"
            )
        body = (
            f"<div class='section-title'>🚨 {len(regressing)} service(s) with "
            f"active memory regressions</div>"
            f"<table><thead><tr>"
            f"<th>Verdict</th><th>Repository</th><th>Ref</th>"
            f"<th>Worst offender</th><th>Δ retained</th><th>Evidence</th>"
            f"</tr></thead><tbody>"
            + "".join(rows)
            + "</tbody></table>"
        )

    return HTML_TEMPLATE.format(
        scope=html.escape(scope),
        generated=html.escape(generated),
        total=counts.get("total", 0),
        with_workflow=counts.get("with_workflow", 0),
        regressing=counts.get("regressing", 0),
        clean=counts.get("clean", 0),
        unknown=counts.get("unknown", 0),
        no_data=counts.get("no_data", 0),
        body=body,
    )


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--in", dest="inp", required=True, type=Path)
    p.add_argument("--out-md",   dest="out_md",   type=Path, default=None)
    p.add_argument("--out-html", dest="out_html", type=Path, default=None)
    p.add_argument("--include-unknown", action="store_true",
                   help="also surface UNKNOWN (evaluator errored) rows")
    args = p.parse_args()

    data = load_data(args.inp)
    if args.out_md:
        args.out_md.write_text(render_markdown(data, args.include_unknown))
        print(f"Wrote {args.out_md}")
    if args.out_html:
        args.out_html.write_text(render_html(data, args.include_unknown))
        print(f"Wrote {args.out_html}")
    if not args.out_md and not args.out_html:
        print("Nothing to do; pass --out-md or --out-html.", file=sys.stderr)
        return 2

    # Also emit a machine-parseable exit code when there's a regression
    if data.get("counts", {}).get("regressing", 0) > 0:
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
