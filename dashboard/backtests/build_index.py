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


def autocorr_verdict(f):
    if f["markets"] < 10:
        return "none"
    if f["beta"] - 1.96 * f["beta_se"] > 0:
        return "momentum"
    if f["beta"] + 1.96 * f["beta_se"] < 0:
        return "reversion"
    return "none"


def autocorr_summary(d):
    r = d["autocorr"]
    verdicts = [autocorr_verdict(f) for f in r["fits"]]
    readable = [x for x in r["trades"] if x["markets"] >= 10 and x["events"] >= 30]
    edges = [x for x in readable if move_edge(x)]
    strongest = max(r["fits"], key=lambda f: abs(f["beta"]) / f["beta_se"] if f["beta_se"] else 0, default=None)
    return {**base(d["run"]), "momentum": verdicts.count("momentum"), "reversion": verdicts.count("reversion"),
            "flat": verdicts.count("none"), "fits": len(verdicts), "edges": len(edges), "rules_readable": len(readable),
            "strongest": {k: strongest[k] for k in ("venue", "horizon_s", "beta", "beta_se")} if strongest else None}


def outcomereg_summary(d):
    r = d["outcomereg"]
    cv = r.get("cv") or {}
    diff, se = cv.get("logloss_diff", 0), cv.get("logloss_diff_se", 0)
    beats = "beats" if cv.get("n") and diff - 1.96 * se > 0 else "worse" if cv.get("n") and diff + 1.96 * se < 0 else "ties"
    return {**base(d["run"]), "obs": r["obs"], "clusters": r["clusters"], "note": r.get("note", ""),
            "calibration": [{k: c[k] for k in ("venue", "slope", "slope_se")} for c in r.get("calibration") or []],
            "cv_diff": diff, "cv_diff_se": se, "cv_n": cv.get("n", 0), "cv_verdict": beats}


def fairvalue_summary(d):
    r = d["fairvalue"]
    edges = [x for x in r.get("trades") or [] if x["markets"] >= 10 and x["trades"] >= 30 and x["pnl"] - 1.96 * x["pnl_se"] > 0]
    gain, se = r.get("gain_vs_best", 0), r.get("gain_se", 0)
    verdict = "beats" if r.get("cv_n") and gain - 1.96 * se > 0 else "worse" if r.get("cv_n") and gain + 1.96 * se < 0 else "ties"
    return {**base(d["run"]), "note": r.get("note", ""), "markets": r["markets"], "cv_n": r.get("cv_n", 0),
            "gain_vs_best": gain, "gain_se": se, "best_single": r.get("best_single", ""), "cv_verdict": verdict,
            "w_kalshi": r.get("w_kalshi", 0), "w_poly": r.get("w_poly", 0), "edges": len(edges),
            "rules": len(r.get("trades") or [])}


def summary(d):
    kind = d["run"].get("kind", "trading")
    if kind == "fairvalue":
        return fairvalue_summary(d)
    if kind == "outcomereg":
        return outcomereg_summary(d)
    if kind == "autocorr":
        return autocorr_summary(d)
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
        "peak_capital_usd": sum((rep.get("peak_capital_usd") or {}).values()) or None,
        "return_on_peak": rep.get("return_on_peak"),
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
