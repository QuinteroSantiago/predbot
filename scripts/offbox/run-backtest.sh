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
#   ./run-backtest.sh fairvalue  # fair value from both prices; bet where one strays, out of sample
#   ./run-backtest.sh analyses   # every read-only analysis above, one after another
#   ./run-backtest.sh all        # maker, taker, adverse, then every analysis, one after another
#   ./run-backtest.sh stop       # stop EVERYTHING: the analyses chain, sweeps, and any run
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

# Stop everything: first the loops that chain runs together (so none starts the
# next), then the runs themselves. Loops are found by the pid files they leave;
# bt-new by its exact process name (never pkill -f: over SSH the pattern matches
# the remote shell running the command and kills your own session).
if [[ "$mode" == stop ]]; then
  stopped=0
  for f in analyses.pid sweep-*/pid; do
    [[ -f "$f" ]] || continue
    if kill "$(cat "$f")" 2>/dev/null; then
      echo "stopped ${f%/pid} loop (pid $(cat "$f"))"
      stopped=1
    fi
    rm -f "$f"
  done
  # An analyses chain started before pid files existed has no analyses.pid.
  if pkill -f '^bash -c for m in (leadlag longshot|\$LIST)' 2>/dev/null; then
    echo "stopped analyses chain"
    stopped=1
  fi
  if pgrep -x bt-new >/dev/null; then
    pgrep -a -x bt-new | sed 's/^/stopping: /'
    pkill -x bt-new
    stopped=1
  fi
  rm -f ./*-full.pid
  [[ $stopped == 1 ]] && echo "all stopped; finished runs keep their .txt/.json" || echo "nothing was running"
  exit 0
fi

# A chain of runs in sequence (each uses every core), detached: "analyses" is
# the read-only studies, "all" adds the three strategy replays in front.
if [[ "$mode" == analyses || "$mode" == all ]]; then
  if [[ -f analyses.pid ]] && kill -0 "$(cat analyses.pid)" 2>/dev/null; then
    echo "a chain is already running (pid $(cat analyses.pid)); ./run-backtest.sh stop first"
    exit 1
  fi
  list="leadlag longshot moves spreadmr autocorr outcomereg fairvalue"
  [[ "$mode" == all ]] && list="maker taker adverse $list"
  LIST="$list" setsid nohup bash -c 'for m in $LIST; do
      echo "== $m $(date -u +%FT%TZ)"; RUN_FOREGROUND=1 ./run-backtest.sh "$m" || echo "   $m failed or skipped"
    done; echo "ALL DONE $(date -u +%FT%TZ)"; rm -f analyses.pid' > analyses.log 2>&1 < /dev/null &
  echo $! > analyses.pid
  echo "running $(wc -w <<< "$list") runs in sequence (pid $!): $list"
  echo "progress: tail -f $PWD/analyses.log   (the current run: tail -F $PWD/*-full.log)"
  exit 0
fi
case "$mode" in
  maker)    flags=(-maker); json=(-label "full archive" -report-json "$mode-full.json") ;;
  taker)    flags=();       json=(-label "full archive" -report-json "$mode-full.json") ;;
  leadlag)  flags=(-leadlag); json=(-label "full archive" -report-json "$mode-full.json") ;;
  fairvalue)
    flags=(-fairvalue); json=(-label "full archive" -report-json "$mode-full.json")
    [[ -s data/outcomes.jsonl ]] || { echo "fairvalue needs data/outcomes.jsonl; re-rsync the Pi's ~/backups/predbot/data/." >&2; exit 1; }
    ;;
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
  *) echo "usage: $0 maker|taker|longshot|leadlag|adverse|moves|spreadmr|autocorr|outcomereg|fairvalue|analyses|all|stop" >&2; exit 2 ;;
esac

pidfile="$mode-full.pid"
if [[ "${RUN_FOREGROUND:-}" == 1 ]]; then
  exec ./bt-new -config config.yaml -markets markets.yaml -data data "${flags[@]}" "${json[@]}" \
    > "$mode-full.txt" 2> "$mode-full.log" < /dev/null
fi
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
