#!/usr/bin/env python3
"""Build the Fee Wall dashboard HTML from backtest exports.

Usage: build.py <opps.json> <spread.json> <template.html> <out.html>

Reads the opportunities + sampled-spread JSON emitted by `cmd/backtest
-export/-export-spread`, assembles a compact payload, and injects it into the
template (which carries the __DATA__ placeholder). Deterministic; no network.
"""
import json
import sys


def main():
    opps_path, spread_path, tmpl_path, out_path = sys.argv[1:5]
    opps = json.load(open(opps_path))
    spread = json.load(open(spread_path))

    cats = sorted({s["category"] for s in spread})
    comp = sorted(
        ([s["ts_ms"], s["category"], round(s["gross_edge_cents"], 2), s["canonical"]] for s in spread),
        key=lambda r: r[0],
    )
    ts = [r[0] for r in comp] or [0]
    payload = {
        "opps": opps,
        "spread": comp,
        "cats": cats,
        "meta": {
            "n_markets": len({s["canonical"] for s in spread}) or 24,
            "n_obs": len(spread),
            "above_wall": sum(1 for s in spread if s["gross_edge_cents"] > 3.25),
            "tradeable": len(opps),
            "fee_wall": 3.25,
            "t0": min(ts),
            "t1": max(ts),
            "net_if_taken": round(sum(o["net_usd"] for o in opps), 4),
            "best_edge": round(max((s["gross_edge_cents"] for s in spread), default=0), 2),
        },
    }

    tmpl = open(tmpl_path).read()
    html = tmpl.replace("__DATA__", json.dumps(payload, separators=(",", ":")))
    open(out_path, "w").write(html)
    print(f"built {out_path}: {len(html)} bytes, {len(opps)} opps, {len(spread)} spread points")


if __name__ == "__main__":
    main()
