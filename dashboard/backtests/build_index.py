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
        "variant": run.get("variant", ""),
        "from": run.get("from"),
        "to": run.get("to"),
        "params": run.get("params"),  # absent before 2026-09-22 builds
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


def move_edge(r):
    """Mirrors backtest.MoveEdge: 10+ markets, 30+ events, 95% range clears zero."""
    if r["markets"] < 10 or r["events"] < 30:
        return None
    if r["fade"] - 1.96 * r["fade_se"] > 0:
        return "fade"
    if r["follow"] - 1.96 * r["follow_se"] > 0:
        return "follow"
    return None


def moves_summary(d):
    r = d["moves"]
    readable = [x for x in r["rows"] if x["markets"] >= 10 and x["events"] >= 30]
    edges = [x for x in readable if move_edge(x)]
    # What a taker round trip costs on average: follow + fade pays it twice.
    costs = [-(x["follow"] + x["fade"]) / 2 for x in readable if x["horizon_s"]]
    return {**base(d["run"]), **{k: r[k] for k in ("move_events", "flow_events", "settlement")},
            "rows_readable": len(readable), "edges": len(edges),
            "round_trip_cents": sum(costs) / len(costs) if costs else None}


def spreadmr_summary(d):
    r = d["spreadmr"]
    edges = [x for x in r["strategies"] if x["markets"] >= 10 and x["trades"] >= 30
             and x["pnl"] - 1.96 * x["pnl_se"] > 0]
    return {**base(d["run"]), **{k: r[k] for k in (
        "markets", "half_life_s", "half_life_lo_s", "half_life_hi_s", "median_sigma_eq", "median_abs_level")},
        "edges": len(edges), "strategies_tested": len(r["strategies"])}


def summary(d):
    kind = d["run"].get("kind", "trading")
    if kind == "spreadmr":
        return spreadmr_summary(d)
    if kind == "moves":
        return moves_summary(d)
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
        "adverse_30s_cents": next((h["mean_cents"] for h in (d.get("adverse") or {}).get("horizons", [])
                                   if h["seconds"] == 30 and h["fills"]), None),
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
