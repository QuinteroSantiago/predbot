#!/usr/bin/env python3
"""Convert a backtest's TEXT report into a run report for the backtest runs page.

Usage:
  import_text.py report.txt --label "full archive" [--finished ISO] [--host NAME] > run.json

For runs made before -report-json existed. The text report has the headline
numbers (P&L, fees, pairs, return and risk, maker rests) but not the individual
pairs, so an imported run has no equity curve or per-market P&L; the page says
so. --finished defaults to the file's modification time (copy it with scp -p
to keep that).
"""
import argparse
import datetime as dt
import json
import os
import re
import sys

MONEY = r"\$(-?[0-9]+(?:\.[0-9]+)?)"


def go_duration_s(s):
    """Parse a Go duration like 2064h35m0s into seconds."""
    total, found = 0.0, False
    for num, unit in re.findall(r"([0-9.]+)(h|ms|m|s)", s):
        total += float(num) * {"h": 3600, "m": 60, "s": 1, "ms": 1e-3}[unit]
        found = True
    return total if found else None


def grab(text, pattern, cast=float, default=None):
    m = re.search(pattern, text, re.M)
    return cast(m.group(1)) if m else default


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("report")
    ap.add_argument("--label", default="")
    ap.add_argument("--finished", help="when the run finished, ISO 8601 (default: file mtime)")
    ap.add_argument("--host", default="")
    a = ap.parse_args()

    text = open(a.report).read()
    if "=== Backtest report" not in text:
        sys.exit(f"{a.report}: no '=== Backtest report' block — not a trading-run text report")

    if a.finished:
        finished = dt.datetime.fromisoformat(a.finished)
        if finished.tzinfo is None:
            finished = finished.astimezone()
    else:
        finished = dt.datetime.fromtimestamp(os.path.getmtime(a.report)).astimezone()
    finished = finished.astimezone(dt.timezone.utc)

    maker = "MAKER MODE:" in text
    mode = "maker" if maker else "taker"

    # Fees: the text prints them as P&L contributions (negated); the JSON
    # carries them as costs, like the Report.
    fees = {}
    in_fees = False
    for line in text.splitlines():
        if line.startswith("Fees (total)"):
            in_fees = True
            continue
        if in_fees:
            m = re.match(r"\s+-\s+(\S+)\s+" + MONEY, line)
            if not m:
                break
            fees[m.group(1)] = -float(m.group(2))

    pairs = re.search(r"^Pairs attempted/completed/legged/faded\s+(\d+) / (\d+) / (\d+) / (\d+)", text, re.M)
    attempted, completed, legged, faded = (int(x) for x in pairs.groups()) if pairs else (0, 0, 0, 0)
    settled = grab(text, r"^Win rate \(net>0\)\s+[0-9.]+% of (\d+)", int, 0)
    ann = re.search(r"^Capital-weighted annualized return\s+([+-]?[0-9.]+)%\s+\(avg hold ([0-9.]+) days\)", text, re.M)
    excess = re.search(r"^\s+vs idle-balance hurdle ([0-9.]+)% -> excess\s+([+-]?[0-9.]+)%", text, re.M)
    rests = re.search(r"^MAKER MODE: rests placed (\d+), rests FILLED (\d+)", text, re.M)
    replayed = re.search(r"^Replayed (\d+) snapshots(?: \(streaming\))? over (\S+) across (\d+) markets", text, re.M)

    report = {
        "gross_usd": grab(text, r"^Gross gap captured\s+" + MONEY, float, 0.0),
        "fees_usd": sum(fees.values()),
        "fees_by_venue_usd": fees,
        "slippage_usd": -grab(text, r"^Slippage \(fill vs top\)\s+" + MONEY, float, 0.0),
        "legging_cost_usd": -grab(text, r"^Legging cost\s+" + MONEY, float, 0.0),
        "net_pnl_usd": grab(text, r"^Net P&L\s+" + MONEY, float, 0.0),
        "capital_locked_usd": grab(text, r"^Capital locked\s+" + MONEY, float, 0.0),
        "pairs_attempted": attempted,
        "pairs_completed": completed,
        "pairs_legged": legged,
        "pairs_faded": faded,
        "intra_crossings": grab(text, r"^Intra-venue crossings \(feed check\)\s+(\d+)", int, 0),
        "annualized": float(ann.group(1)) / 100 if ann else 0.0,
        "excess_annualized": float(excess.group(2)) / 100 if excess else 0.0,
        "idle_yield": float(excess.group(1)) / 100 if excess else 0.0,
        "avg_hold_days": float(ann.group(2)) if ann else 0.0,
        "sharpe_per_trade": grab(text, r"^Sharpe \(per-trade\)\s+(-?[0-9.]+)", float, 0.0),
        "win_rate": grab(text, r"^Win rate \(net>0\)\s+([0-9.]+)%", float, 0.0) / 100,
        "max_drawdown_usd": -grab(text, r"^Max drawdown \(cumulative net\)\s+" + MONEY, float, 0.0),
        "settled_pairs": settled,
    }

    # Maker reports from before commit 2b610fb dated every maker pair at the
    # epoch, so hold time ran from 1970 (~20,000 days) and the annualized
    # figures are meaningless. Drop them rather than publish them.
    if maker and report["avg_hold_days"] > 3650:
        print(f"{a.report}: avg hold {report['avg_hold_days']:.0f} days — pre-fix maker dating bug; "
              "dropping annualized return and hold time", file=sys.stderr)
        report["annualized"] = report["excess_annualized"] = report["avg_hold_days"] = None

    out = {
        "schema": 1,
        "run": {
            "id": finished.strftime("%Y%m%dT%H%M%SZ") + "-" + mode + "-imported",
            "label": a.label,
            "mode": mode,
            "adverse": "Adverse selection" in text,
            "window_hours": 0,
            "data_from": None,
            "data_to": None,
            "span_s": go_duration_s(replayed.group(2)) if replayed else None,
            "snapshots": int(replayed.group(1)) if replayed else 0,
            "markets": int(replayed.group(3)) if replayed else 0,
            "started_at": None,
            "finished_at": finished.isoformat().replace("+00:00", "Z"),
            "duration_s": None,
            "host": a.host,
            "shards": None,
            "parse_workers": None,
            "args": [],
            "imported_from": os.path.basename(a.report),
        },
        "report": report,
        # The text lists attempts per market, not gap-open episodes: unknown.
        "opportunities": None,
        "pairs": [],
        "markets": [],
    }
    if rests:
        out["maker"] = {"rests_placed": int(rests.group(1)), "rests_filled": int(rests.group(2))}
    json.dump(out, sys.stdout, indent=1)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
