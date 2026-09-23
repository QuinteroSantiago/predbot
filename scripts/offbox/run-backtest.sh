#!/usr/bin/env bash
# Launch a full-archive backtest or analysis detached, so it survives SSH closing.
#
#   ./run-backtest.sh maker      # maker strategy replay
#   ./run-backtest.sh taker      # taker strategy replay
#   ./run-backtest.sh longshot   # calibration: do opening prices match how games resolved?
#   ./run-backtest.sh leadlag    # does one venue's price move before the other's?
#   ./run-backtest.sh adverse    # maker replay + where prices go after each real fill
#   ./run-backtest.sh moves      # overreaction: fade or follow sharp moves / one-sided trading?
#   ./run-backtest.sh spreadmr   # Kalshi−Poly gap as mean-reverting: half-life, and trading it
#   ./run-backtest.sh autocorr   # per venue: do price changes continue or undo, 5s–10m?
#   ./run-backtest.sh outcomereg # logistic regression of who won; out-of-sample vs the market
#
# Lives in scripts/offbox/ in the predbot repo; copy it next to the bt-new binary
# and the data/ directory on the compute box (see sweep.sh for batches).
#
# Writes <mode>-full.txt (the report) and <mode>-full.log (progress, a line
# every 15s), and <mode>-full.json for the /backtests/ page.
# One run per mode at a time; different modes can run side by side (they share
# the cores).
set -euo pipefail
cd "$(dirname "$0")"

mode="${1:-}"
json=()
case "$mode" in
  maker)    flags=(-maker); json=(-label "full archive" -report-json "$mode-full.json") ;;
  taker)    flags=();       json=(-label "full archive" -report-json "$mode-full.json") ;;
  leadlag)  flags=(-leadlag); json=(-label "full archive" -report-json "$mode-full.json") ;;
  outcomereg)
    flags=(-outcomereg); json=(-label "full archive" -report-json "$mode-full.json")
    [[ -s data/outcomes.jsonl ]] || { echo "outcomereg needs data/outcomes.jsonl; re-rsync the Pi's ~/backups/predbot/data/." >&2; exit 1; }
    ;;
  autocorr) flags=(-autocorr); json=(-label "full archive" -report-json "$mode-full.json") ;;
  spreadmr) flags=(-spreadmr); json=(-label "full archive" -report-json "$mode-full.json") ;;
  moves)    flags=(-moves); json=(-label "full archive" -report-json "$mode-full.json")
    compgen -G "data/trades/*.jsonl.gz" >/dev/null ||
      echo "note: no data/trades/ — only the price-move trigger will run (no one-sided-flow trigger)" >&2
    [[ -s data/outcomes.jsonl ]] ||
      echo "note: no data/outcomes.jsonl — no hold-to-settlement horizon" >&2
    ;;
  adverse)
    flags=(-maker -adverse); json=(-label "full archive" -variant "adverse selection" -report-json "$mode-full.json")
    if ! compgen -G "data/trades/*.jsonl.gz" >/dev/null; then
      echo "adverse needs data/trades/ (real trade prints). The recorder writes them on the" >&2
      echo "predbot VM; re-rsync the Pi's ~/backups/predbot/data/." >&2
      exit 1
    fi
    ;;
  longshot)
    flags=(-longshot); json=(-label "full archive" -report-json "$mode-full.json")
    if [[ ! -s data/outcomes.jsonl ]]; then
      echo "longshot needs data/outcomes.jsonl (how games resolved). It's recorded hourly on" >&2
      echo "the predbot VM and backed up to the Pi; re-rsync the Pi's ~/backups/predbot/data/." >&2
      exit 1
    fi
    ;;
  *) echo "usage: $0 maker|taker|longshot|leadlag|adverse|moves|spreadmr|autocorr|outcomereg" >&2; exit 2 ;;
esac

pidfile="$mode-full.pid"
if [[ -f "$pidfile" ]] && kill -0 "$(cat "$pidfile")" 2>/dev/null; then
  echo "a $mode run is already going (pid $(cat "$pidfile")); tail -f $PWD/$mode-full.log"
  exit 1
fi

setsid nohup ./bt-new \
  -config config.yaml \
  -markets markets.yaml \
  -data data \
  "${flags[@]}" "${json[@]}" \
  > "$mode-full.txt" 2> "$mode-full.log" < /dev/null &
echo $! > "$pidfile"

sleep 2
if kill -0 "$(cat "$pidfile")" 2>/dev/null; then
  echo "$mode run started (pid $(cat "$pidfile")); watch with: tail -f $PWD/$mode-full.log"
else
  echo "$mode run exited immediately:"
  cat "$mode-full.log"
  exit 1
fi
