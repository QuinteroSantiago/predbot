#!/usr/bin/env bash
# Publish backtest run reports (cmd/backtest -report-json) to the backtest runs
# page at /backtests/ on the homelab web root, and refresh the page itself.
#
#   scripts/publish-backtest.sh run.json [more.json ...]
#   scripts/publish-backtest.sh            # just reinstall the page + reindex
#
# Each report lands at runs/<run id>.json; runs/index.json is rebuilt from
# whatever is there. Re-publishing the same run overwrites it.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.."; pwd)"
WEB="${WEB:-/home/santiago/home-frontend/backtests}"

mkdir -p "$WEB/runs"
for f in "$@"; do
  id="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["run"]["id"])' "$f")"
  if [[ ! "$id" =~ ^[A-Za-z0-9_.-]+$ ]]; then
    echo "refusing $f: unsafe run id '$id'" >&2
    exit 1
  fi
  install -m 0664 "$f" "$WEB/runs/$id.json"
  echo "published $id"
done
install -m 0664 "$REPO/dashboard/backtests/index.html" "$WEB/index.html"
python3 "$REPO/dashboard/backtests/build_index.py" "$WEB/runs"
