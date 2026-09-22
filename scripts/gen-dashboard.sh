#!/usr/bin/env bash
# Regenerate the Fee Wall arbitrage dashboard from the latest recorded data and
# publish it to the homelab web root, where nginx serves it and the landing page
# links to it. Also publishes that night's taker and maker runs to the backtest
# runs page (/backtests/). Run by predbot-dashboard.timer after the daily
# backup, or by hand.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.."; pwd)"
DATA="${DATA:-/home/santiago/backups/predbot/data}"
OUT="${OUT:-/home/santiago/home-frontend/fee-wall.html}"
HOURS="${HOURS:-12}"
LABEL="daily · last ${HOURS}h"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

cd "$REPO"
GOTOOLCHAIN=auto go build -o "$TMP/backtest" ./cmd/backtest

# Replay the recorded data through the backtest, exporting the opportunity set
# and the sampled cross-venue spread surface. The same replay is the night's
# taker run for the backtest runs page.
"$TMP/backtest" \
  -data "$DATA" \
  -hours "$HOURS" \
  -export "$TMP/opps.json" \
  -export-spread "$TMP/spread.json" \
  -label "$LABEL" -report-json "$TMP/taker.json" \
  -progress=false >/dev/null

python3 dashboard/build.py "$TMP/opps.json" "$TMP/spread.json" dashboard/template.html "$OUT"
echo "dashboard published to $OUT"

# The maker run (the strategy that's live) and publishing are extras: a failure
# here is logged but never fails the Fee Wall, which is already published.
runs=("$TMP/taker.json")
if "$TMP/backtest" -data "$DATA" -hours "$HOURS" -maker \
     -label "$LABEL" -report-json "$TMP/maker.json" -progress=false >/dev/null; then
  runs+=("$TMP/maker.json")
else
  echo "maker run failed; publishing the taker run only" >&2
fi
scripts/publish-backtest.sh "${runs[@]}" || echo "backtest runs publish failed" >&2
