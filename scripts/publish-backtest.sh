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

# Replace dst with a copy of src by renaming a fresh temp file over it. The web
# root is shared through the dev group, so dst may belong to another user:
# writing through it works, but install's chmod would fail on a file we don't
# own, and set -e would then skip the reindex.
put() {
  local tmp
  tmp="$(mktemp "$2.XXXXXX")"
  cp "$1" "$tmp"
  chmod 0664 "$tmp"
  mv -f "$tmp" "$2"
}

mkdir -p "$WEB/runs"
for f in "$@"; do
  id="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["run"]["id"])' "$f")"
  if [[ ! "$id" =~ ^[A-Za-z0-9_.-]+$ ]]; then
    echo "refusing $f: unsafe run id '$id'" >&2
    exit 1
  fi
  put "$f" "$WEB/runs/$id.json"
  echo "published $id"
done
put "$REPO/dashboard/backtests/index.html" "$WEB/index.html"
python3 "$REPO/dashboard/backtests/build_index.py" "$WEB/runs"
