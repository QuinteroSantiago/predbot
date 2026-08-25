#!/usr/bin/env bash
# Phase 3a divergence report: compare what LIVE paper trading actually did
# (paper-fills.jsonl, pulled from the VM) against what the BACKTEST predicts on
# the same recorded data. Divergence = a bug that only shows up in real time
# (fade, latency, staleness, clock).
set -euo pipefail
REPO="$(cd "$(dirname "$0")/.."; pwd)"
DATA="${DATA:-/home/santiago/backups/predbot/data}"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

# Pull the live paper fills log from the VM.
scp -o BatchMode=yes -q k3s-vm:/var/lib/predbot/data/paper-fills.jsonl "$TMP/paper.jsonl" 2>/dev/null || : > "$TMP/paper.jsonl"

# Run the backtest on the same recorded data to get its predicted opportunities.
cd "$REPO"
GOTOOLCHAIN=auto go run ./cmd/backtest -data "$DATA" -export "$TMP/backtest.json" >/dev/null 2>&1 || true

python3 - "$TMP/paper.jsonl" "$TMP/backtest.json" <<'PY'
import json, sys, collections
paper = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
bt = json.load(open(sys.argv[2])) if __import__("os").path.getsize(sys.argv[2]) else []

def by_market(rows, netkey):
    m = collections.defaultdict(lambda: [0, 0.0])
    for r in rows:
        m[r["canonical"]][0] += 1
        m[r["canonical"]][1] += r[netkey]
    return m

pm, bm = by_market(paper, "net_usd"), by_market(bt, "net_usd")
pnet = sum(r["net_usd"] for r in paper); bnet = sum(r["net_usd"] for r in bt)

print("=== Phase 3a divergence report: live paper vs backtest ===")
print(f"LIVE paper : {len(paper):4d} fills, net ${pnet:.4f}")
print(f"BACKTEST   : {len(bt):4d} opportunities, net ${bnet:.4f}")
print(f"divergence : ${pnet - bnet:+.4f} net, {len(paper)-len(bt):+d} trades\n")
markets = sorted(set(pm) | set(bm))
if markets:
    print(f"{'market':28s} {'paper(n,net)':>18s} {'backtest(n,net)':>18s}")
    for c in markets:
        p, b = pm.get(c, [0, 0.0]), bm.get(c, [0, 0.0])
        print(f"{c:28s} {p[0]:3d} ${p[1]:8.4f}    {b[0]:3d} ${b[1]:8.4f}")
else:
    print("(no trades on either side yet — run after the paper trader has taken fills)")
print("\nInterpretation: matching totals => backtest is trustworthy live. Big gaps =>")
print("real fade/latency/staleness the backtest fill model doesn't capture.")
PY
