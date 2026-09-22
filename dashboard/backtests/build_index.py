#!/usr/bin/env python3
"""Rebuild runs/index.json for the backtest runs page.

Usage: build_index.py <runs-dir>

Reads every <id>.json run report (cmd/backtest -report-json) in the directory
and writes index.json: one summary row per run, newest first. The page loads
the index for its history view and fetches a run's full file on click.
Unreadable or foreign files are skipped with a warning, never fatal.
"""
import json
import os
import re
import sys

SCHEMA = 1
ID_RE = re.compile(r"^[A-Za-z0-9_.-]+$")


def base(run):
    return {
        "id": run["id"],
        "kind": run.get("kind", "trading"),
        "label": run.get("label", ""),
        "mode": run["mode"],
        "window_hours": run.get("window_hours", 0),
        "data_from": run["data_from"],
        "data_to": run["data_to"],
        "span_s": run.get("span_s"),
        "snapshots": run["snapshots"],
        "markets": run["markets"],
        "finished_at": run["finished_at"],
        "duration_s": run["duration_s"],
        "revision": run.get("revision", ""),
    }


def leadlag_summary(d):
    r = d["leadlag"]
    return {**base(d["run"]), **{k: r[k] for k in (
        "analyzed", "kalshi_leads", "poly_leads", "contemporaneous", "no_signal",
        "median_lead_s", "median_peak_corr")}}


def longshot_summary(d):
    r = d["longshot"]
    # Buckets whose edge is outside a rough 95% range, with enough markets to say.
    clear = [b for b in r["buckets"] if b["n"] >= 30 and abs(b["edge"]) > 1.96 * b["stderr"]]
    return {**base(d["run"]), **{k: r[k] for k in (
        "outcomes", "markets", "verdict", "slope", "intercept", "brier", "brier_base")},
        "clear_buckets": len(clear),
        "solid_buckets": sum(1 for b in r["buckets"] if b["n"] >= 30)}


def summary(d):
    kind = d["run"].get("kind", "trading")
    if kind == "leadlag":
        return leadlag_summary(d)
    if kind == "longshot":
        return longshot_summary(d)
    run, rep, maker = d["run"], d["report"], d.get("maker")
    placed = maker["rests_placed"] if maker else None
    return {
        **base(run),
        "adverse": run.get("adverse", False),
        "imported": bool(run.get("imported_from")),
        "net_pnl_usd": rep["net_pnl_usd"],
        "annualized": rep["annualized"],
        "excess_annualized": rep["excess_annualized"],
        "win_rate": rep["win_rate"],
        "max_drawdown_usd": rep["max_drawdown_usd"],
        "pairs_attempted": rep["pairs_attempted"],
        "pairs_completed": rep["pairs_completed"],
        "settled_pairs": rep["settled_pairs"],
        "rests_placed": placed,
        "fill_rate": (maker["rests_filled"] / placed) if placed else None,
    }


def main():
    runs_dir = sys.argv[1]
    rows = []
    for name in sorted(os.listdir(runs_dir)):
        if not name.endswith(".json") or name == "index.json":
            continue
        path = os.path.join(runs_dir, name)
        try:
            with open(path) as f:
                d = json.load(f)
            if d.get("schema") != SCHEMA:
                raise ValueError(f"schema {d.get('schema')!r}, want {SCHEMA}")
            if not ID_RE.match(d["run"]["id"]) or name != d["run"]["id"] + ".json":
                raise ValueError("file name doesn't match run id")
            rows.append(summary(d))
        except (OSError, ValueError, KeyError, TypeError) as e:
            print(f"skipping {name}: {e}", file=sys.stderr)
    rows.sort(key=lambda r: r["finished_at"], reverse=True)
    tmp = os.path.join(runs_dir, "index.json.tmp")
    with open(tmp, "w") as f:
        json.dump({"schema": SCHEMA, "runs": rows}, f, separators=(",", ":"))
    os.replace(tmp, os.path.join(runs_dir, "index.json"))
    print(f"indexed {len(rows)} runs")


if __name__ == "__main__":
    main()
