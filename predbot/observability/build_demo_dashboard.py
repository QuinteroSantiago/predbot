#!/usr/bin/env python3
"""Builds "predbot — Live Ops (demo)": a single narrative dashboard that reads
top-to-bottom like a trading-infra interview walkthrough —
  Reliability  →  Latency (the money-shot)  →  The Edge (measurement honesty)  →  Execution & Risk.

All series are LIVE from the always-on recorder (paper path); nothing here needs
real trading to be armed. Upserts via the homelab-observability Grafana helper so
it deploys the same way the other predbot dashboards do. Source of truth for the
JSON is observability/predbot-demo.dashboard.json (written alongside the upsert).
"""
import json
import os
import sys

sys.path.insert(0, os.path.expanduser("~/homelab-observability/lib"))
from grafana import DATASOURCE, target, upsert_dashboard  # noqa: E402

UID = "predbot-demo"
Y = 0  # running grid cursor


def _grid(h, w, x):
    global Y
    return {"h": h, "w": w, "x": x, "y": Y}


def row(title):
    global Y
    p = {"type": "row", "title": title, "collapsed": False,
         "gridPos": {"h": 1, "w": 24, "x": 0, "y": Y}, "panels": []}
    Y += 1
    return p


def stat(title, expr, unit="short", w=4, x=0, decimals=None, color="value",
         desc="", thresholds=None, legend=""):
    t = target(expr, legend, instant=True)
    defaults = {"unit": unit, "color": {"mode": "thresholds"}}
    if decimals is not None:
        defaults["decimals"] = decimals
    if thresholds:
        defaults["thresholds"] = {"mode": "absolute", "steps": thresholds}
    else:
        defaults["thresholds"] = {"mode": "absolute", "steps": [{"color": "text", "value": None}]}
    return {"type": "stat", "title": title, "description": desc,
            "gridPos": _grid(4, w, x), "datasource": DATASOURCE, "targets": [t],
            "fieldConfig": {"defaults": defaults, "overrides": []},
            "options": {"reduceOptions": {"calcs": ["lastNotNull"]},
                        "colorMode": color, "graphMode": "area",
                        "textMode": "value_and_name", "justifyMode": "auto"}}


def ts(title, targets, unit="short", w=12, x=0, desc="", h=8, fillthreshold=None, decimals=None):
    defaults = {"unit": unit, "custom": {"drawStyle": "line", "lineWidth": 2,
                "fillOpacity": 8, "showPoints": "never", "spanNulls": True}}
    if decimals is not None:
        defaults["decimals"] = decimals
    ov = []
    if fillthreshold is not None:
        # dash the constant fee-wall / threshold reference line
        ov.append({"matcher": {"id": "byName", "options": fillthreshold},
                   "properties": [{"id": "custom.lineStyle", "value": {"fill": "dash", "dash": [8, 6]}},
                                  {"id": "color", "value": {"mode": "fixed", "fixedColor": "red"}},
                                  {"id": "custom.fillOpacity", "value": 0}]})
    return {"type": "timeseries", "title": title, "description": desc,
            "gridPos": _grid(h, w, x), "datasource": DATASOURCE, "targets": targets,
            "fieldConfig": {"defaults": defaults, "overrides": ov},
            "options": {"legend": {"displayMode": "list", "placement": "bottom"},
                        "tooltip": {"mode": "multi", "sort": "desc"}}}


def text(title, content, w=24, x=0, h=3):
    return {"type": "text", "title": title, "gridPos": _grid(h, w, x),
            "options": {"mode": "markdown", "content": content}}


panels = []


def add(*ps):
    global Y
    maxh = max(p["gridPos"]["h"] for p in ps)
    panels.extend(ps)
    Y += maxh


# ── Intro ──────────────────────────────────────────────────────────────────
panels.append(row("What this is"))
add(text("", (
    "**A live, OpenTelemetry-instrumented cross-venue prediction-market trading system** "
    "(Kalshi + Polymarket US, Go). This board reads top-to-bottom like the interview walkthrough: "
    "**Reliability → Latency → The Edge → Execution.** Every series is live from the always-on "
    "recorder — no real money is at risk. The *honest* headline is in **The Edge**: at retail depth "
    "the cross-venue gap almost never clears the ~3.49¢ fee wall, and this system proves it with data "
    "instead of losing money finding out."), h=3))

# ── 1. Reliability ────────────────────────────────────────────────────────
panels.append(row("1 · Reliability  ·  can you operate it 24/7?"))
add(
    stat("Uptime", "time() - predbot_start_time_seconds", unit="dtdurations", w=5, x=0,
         desc="Wall-clock since the recorder last started; a step here marks a restart."),
    stat("Reconnects (1h)", "sum(increase(predbot_reconnects_total[1h])) or vector(0)", w=5, x=5,
         desc="WebSocket reconnects across both venues in the last hour. Feeds flap; the ingester recovers with exponential backoff.",
         thresholds=[{"color": "green", "value": None}, {"color": "yellow", "value": 5}, {"color": "red", "value": 20}]),
    stat("Seq gaps (1h)", "sum(increase(predbot_seq_gaps_total[1h])) or vector(0)", w=5, x=10,
         desc="Kalshi order-book sequence gaps — a detected feed discontinuity that triggers a resync.",
         thresholds=[{"color": "green", "value": None}, {"color": "yellow", "value": 1}]),
    stat("Parse errors (1h)", "sum(increase(predbot_parse_errors_total[1h])) or vector(0)", w=4, x=15,
         desc="Unparseable frames. Chaos-tested: the ingester never panics on garbage input.",
         thresholds=[{"color": "green", "value": None}, {"color": "red", "value": 1}]),
    stat("MLB games tracked", "count(predbot_cross_edge_cents) or vector(0)", w=5, x=19,
         desc="Live markets cross-listed on BOTH venues right now, discovered at runtime."),
)
add(
    ts("Feed events / sec (throughput)",
       [target("sum by (venue) (rate(predbot_snapshots_total[5m]))", "{{venue}}")],
       unit="ops", w=12, x=0, desc="Normalized order-book snapshots per second, per venue."),
    ts("Reliability events / sec  ·  reconnects · seq-gaps · parse-errors",
       [target("sum by (venue) (rate(predbot_reconnects_total[5m])) or vector(0)", "reconnects {{venue}}"),
        target("sum(rate(predbot_seq_gaps_total[5m])) or vector(0)", "seq gaps"),
        target("sum by (venue) (rate(predbot_parse_errors_total[5m])) or vector(0)", "parse err {{venue}}")],
       unit="ops", w=12, x=12, desc="Should sit at zero; every non-zero spike is a recovery event. A flat zero line here is the healthy state (these counters only create a series once an event fires)."),
)

# ── 2. Latency ────────────────────────────────────────────────────────────
panels.append(row("2 · Latency  ·  where does the time go? (p50 / p95)"))
add(
    stat("Strategy sweep p95", "histogram_quantile(0.95, sum by (le) (rate(predbot_tick_to_decision_seconds_bucket[5m])))",
         unit="s", w=6, x=0, decimals=4,
         desc="Wall time to evaluate the cross-venue strategy across EVERY tracked market, per tick. Sub-millisecond."),
    stat("Kalshi feed p95", 'histogram_quantile(0.95, sum by (le) (rate(predbot_feed_latency_seconds_bucket{venue="kalshi"}[5m])))',
         unit="s", w=6, x=6, decimals=4, desc="Exchange event → our recv, dominated by public-internet WS RTT."),
    stat("Poly feed p95", 'histogram_quantile(0.95, sum by (le) (rate(predbot_feed_latency_seconds_bucket{venue="polymarketus"}[5m])))',
         unit="s", w=6, x=12, decimals=4, desc="Polymarket's wire is ~4-5× slower than Kalshi's."),
    stat("Book age p95", "histogram_quantile(0.95, sum by (le) (rate(predbot_book_age_seconds_bucket[5m])))",
         unit="s", w=6, x=18, decimals=4, desc="recv → book-updated internal processing latency."),
)
add(
    ts("Strategy sweep latency (tick → decision)",
       [target("histogram_quantile(0.50, sum by (le) (rate(predbot_tick_to_decision_seconds_bucket[5m])))", "p50"),
        target("histogram_quantile(0.95, sum by (le) (rate(predbot_tick_to_decision_seconds_bucket[5m])))", "p95")],
       unit="s", w=12, x=0, decimals=4,
       desc="The concurrency core: a full pure-function strategy pass over all markets. Single-writer book, immutable snapshots."),
    ts("Feed latency by venue (exchange → recv)",
       [target('histogram_quantile(0.50, sum by (le,venue) (rate(predbot_feed_latency_seconds_bucket[5m])))', "p50 {{venue}}"),
        target('histogram_quantile(0.95, sum by (le,venue) (rate(predbot_feed_latency_seconds_bucket[5m])))', "p95 {{venue}}")],
       unit="s", w=12, x=12, decimals=4, desc="Kalshi is the faster wire; Poly's RTT is the pipeline's floor."),
)

# ── 3. The Edge ───────────────────────────────────────────────────────────
panels.append(row("3 · The Edge  ·  measurement honesty (does the arb survive fees?)"))
opp = ("count( (predbot_cross_edge_cents > 3.49) and on(market,category) "
       "(predbot_cross_edge_size >= 10) and on(market,category) "
       "(predbot_cross_edge_trade_age_seconds < 300) ) or vector(0)")
widest = ("max( predbot_cross_edge_cents and on(market,category) "
          "(predbot_cross_edge_size >= 10) and on(market,category) "
          "(predbot_cross_edge_trade_age_seconds < 300) ) or vector(0)")
add(
    stat("Real opportunities", opp, w=6, x=0,
         desc="Edges clearing ALL three phantom-edge gates at once: > 3.49¢ fee wall AND ≥ 10 contracts of real depth AND traded in the last 5 min. Usually 0 — that is the finding.",
         thresholds=[{"color": "text", "value": None}, {"color": "green", "value": 1}]),
    stat("Widest executable edge", widest, unit="none", w=6, x=6, decimals=2,
         desc="The best gap that would actually be fillable right now, in cents. Compare to the 3.49¢ fee wall."),
    stat("Fee wall", "vector(3.49)", unit="none", w=6, x=12, decimals=2, color="background",
         desc="Round-trip taker/taker cost per contract. An edge must clear this to be free money — most don't.",
         thresholds=[{"color": "red", "value": None}]),
    stat("Widest RAW edge (ungated)", "max(predbot_cross_edge_cents) or vector(0)", unit="none", w=6, x=18, decimals=2,
         desc="Best top-of-book gap ignoring size & volume. The gap between this and the executable edge is the phantom-edge trap retail falls into."),
)
add(ts("Live cross-venue edge by MLB game  (size- & volume-vetted vs the 3.49¢ fee wall)",
       [target("predbot_cross_edge_cents", "{{market}}"),
        target("vector(3.49)", "fee wall (3.49¢)")],
       unit="none", w=24, x=0, h=9, fillthreshold="fee wall (3.49¢)",
       desc="Every tracked game's real-time gross edge. The dashed red line is the fee wall; series rarely poke above it, and when they do the size/volume gates usually reject them as phantoms."))

# ── 4. Execution & Risk ───────────────────────────────────────────────────
panels.append(row("4 · Execution & Risk  ·  the safety story"))
add(
    ts("Order outcomes / min  (paper path — same code as live)",
       [target("sum by (outcome) (rate(predbot_orders_total[5m]) * 60) or vector(0)", "{{outcome}}")],
       unit="opm", w=12, x=0, h=8,
       desc="Every simulated pair the risk engine approved, by result: filled / partial / faded. Near-flat by design — gate-clearing pairs are rare (the fee-wall finding). The real venue path bumps the SAME counter (venue label kalshi/poly) once armed."),
    ts("Paper P&L  ·  net / gross / fees (USD, cumulative)",
       [target("predbot_paper_net_usd", "net"),
        target("predbot_paper_gross_usd", "gross"),
        target("predbot_paper_fees_usd", "fees")],
       unit="currencyUSD", w=12, x=12, h=8,
       desc="Live SimExecutor P&L against the real feed with real fade & fees. The fee line is why gross rarely becomes net."),
)
add(
    stat("Naked-leg exposure", "sum(predbot_unhedged_contracts) or vector(0)", unit="none", w=6, x=0,
         desc="Contracts held on one venue without their hedge — the exact failure the kill switch guards. Any non-zero is an emergency.",
         thresholds=[{"color": "green", "value": None}, {"color": "red", "value": 0.01}]),
    stat("Open paper positions", "predbot_paper_open_positions or vector(0)", unit="none", w=6, x=6,
         desc="Pairs currently held to resolution in the paper account."),
    stat("Decision → ack p95 (real path)",
         "histogram_quantile(0.95, sum by (le) (rate(predbot_decision_to_ack_seconds_bucket[5m]))) or vector(0)",
         unit="s", w=6, x=12, decimals=3,
         desc="Real venue order round-trip; populates when the live bot is armed. Verified end-to-end with real 1-contract orders on both venues."),
    stat("Faded ratio", "predbot_paper_faded or vector(0)", unit="none", w=6, x=18,
         desc="Opportunities that vanished within the latency window Δ before the sim could fill — real adverse selection."),
)

dash = {
    "uid": UID,
    "title": "predbot — Live Ops (demo)",
    "tags": ["predbot", "demo"],
    "timezone": "browser",
    "schemaVersion": 39,
    "refresh": "10s",
    "time": {"from": "now-1h", "to": "now"},
    "templating": {"list": []},
    "annotations": {"list": []},
    "description": "Interview-walkthrough view of the predbot trading system: reliability, latency, the (honestly measured) edge, and execution safety. All series live from the always-on recorder.",
    "panels": panels,
}

if __name__ == "__main__":
    out = os.path.join(os.path.dirname(__file__), "predbot-demo.dashboard.json")
    with open(out, "w") as f:
        json.dump(dash, f, indent=2)
    print("wrote", out)
    upsert_dashboard(dash)
