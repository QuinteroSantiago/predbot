#!/usr/bin/env bash
# Regenerate the Fee Wall arbitrage dashboard from the latest recorded data and
# publish it to the homelab web root, where nginx serves it and the landing page
# links to it. Run by predbot-dashboard.timer after the daily backup, or by hand.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.."; pwd)"
DATA="${DATA:-/home/santiago/backups/predbot/data}"
OUT="${OUT:-/home/santiago/home-frontend/fee-wall.html}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

cd "$REPO"
# Replay the recorded data through the backtest, exporting the opportunity set
# and the sampled cross-venue spread surface.
GOTOOLCHAIN=auto go run ./cmd/backtest \
  -data "$DATA" \
  -hours "${HOURS:-12}" \
  -export "$TMP/opps.json" \
  -export-spread "$TMP/spread.json" >/dev/null

python3 dashboard/build.py "$TMP/opps.json" "$TMP/spread.json" dashboard/template.html "$OUT"
echo "dashboard published to $OUT"
