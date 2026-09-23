#!/usr/bin/env bash
# Run a batch of backtests one after another, detached, for the /backtests/ page.
#
#   ./sweep.sh sensitivity [maker|taker]   # baseline + one setting pushed at a time
#   ./sweep.sh weekly                      # maker and taker, one run per week of data
#   ./sweep.sh restwait                    # maker: how long should a rest sit before cancel?
#   ./sweep.sh status <preset>             # what's done, what's running
#
# Each run writes sweep-<preset>/<name>.{txt,log,json}; publish the JSONs from
# the Pi with scripts/publish-backtest.sh. Runs go one at a time (each already
# uses every core). Lives in scripts/offbox/; copy it next to bt-new and data/.
set -euo pipefail
cd "$(dirname "$0")"

preset="${1:-}"
dir="sweep-$preset"

if [[ "$preset" == status ]]; then
  d="sweep-${2:?usage: $0 status <preset>}"
  cat "$d/status" 2>/dev/null || echo "no sweep in $d"
  exit 0
fi

# plan prints one run per line: name|flags...
plan() {
  case "$preset" in
    sensitivity)
      local mode="${1:-maker}" m=()
      [[ "$mode" == maker ]] && m=(-maker)
      [[ "$mode" == maker || "$mode" == taker ]] || { echo "mode must be maker or taker" >&2; exit 2; }
      local base="${m[*]:-}"
      echo "$mode baseline|$base"
      # Latency only reaches taker fills: the maker sim books its hedge the
      # moment the rest fills, so latency variants would all match baseline.
      if [[ "$mode" == taker ]]; then
        echo "$mode latency 300ms|$base -latency-ms 300"
        echo "$mode latency 500ms|$base -latency-ms 500"
        echo "$mode latency 1000ms|$base -latency-ms 1000"
      fi
      echo "$mode min edge 2c|$base -min-edge 2"
      echo "$mode min edge 3c|$base -min-edge 3"
      echo "$mode floor off|$base -min-depth 0"
      echo "$mode floor 5 contracts|$base -min-depth 5"
      echo "$mode floor 20 contracts|$base -min-depth 20"
      echo "$mode Kalshi fees +50%|$base -kalshi-fee-mult 0.105"
      ;;
    restwait)
      # How long a maker rest should sit (-rest-wait), with the hedge landing
      # -latency-ms after the fill. The first run is the old model — rests never
      # expire and the hedge is instant — as the reference the others are
      # measured against. Clips follow backtest.max_clip (50, the live cap).
      echo "old model: rest forever, instant hedge|-maker -rest-wait 0 -latency-ms 0"
      echo "rest 30s (live today)|-maker -rest-wait 30s"
      echo "rest 2m|-maker -rest-wait 2m"
      echo "rest 10m|-maker -rest-wait 10m"
      echo "rest until filled|-maker -rest-wait 0"
      echo "rest 30s, hedge 1s late|-maker -rest-wait 30s -latency-ms 1000"
      echo "rest 2m, hedge 1s late|-maker -rest-wait 2m -latency-ms 1000"
      ;;
    weekly)
      # Week boundaries from the recorded hour stamps (UTC days).
      local first last
      first="$(ls data/normalized | sed -nE 's/.*-([0-9]{4}-[0-9]{2}-[0-9]{2})T[0-9]{2}\.jsonl\.gz$/\1/p' | sort -u | head -1)"
      last="$(ls data/normalized | sed -nE 's/.*-([0-9]{4}-[0-9]{2}-[0-9]{2})T[0-9]{2}\.jsonl\.gz$/\1/p' | sort -u | tail -1)"
      [[ -n "$first" ]] || { echo "no hourly files in data/normalized" >&2; exit 1; }
      local from="$first" n=1 to
      while [[ ! "$from" > "$last" ]]; do
        to="$(date -u -d "$from + 7 days" +%F)"
        for mode in maker taker; do
          local mf=""; [[ "$mode" == maker ]] && mf="-maker"
          echo "week $n ($from) $mode|$mf -from $from -to $to"
        done
        from="$to"; n=$((n + 1))
      done
      ;;
    *) echo "usage: $0 sensitivity [maker|taker] | weekly | restwait | status <preset>" >&2; exit 2 ;;
  esac
}

label_for() {
  case "$preset" in
    sensitivity) echo "robustness sweep" ;;
    weekly) echo "weekly split" ;;
    restwait) echo "rest-wait sweep" ;;
  esac
}

# Foreground: validate, refuse a second sweep of the same preset, then relaunch
# detached so closing SSH doesn't stop it.
if [[ "${SWEEP_DETACHED:-}" != 1 ]]; then
  runs="$(plan "${@:2}")"
  mkdir -p "$dir"
  if [[ -f "$dir/pid" ]] && kill -0 "$(cat "$dir/pid")" 2>/dev/null; then
    echo "a $preset sweep is already running (pid $(cat "$dir/pid")); ./sweep.sh status $preset"
    exit 1
  fi
  printf '%s\n' "$runs" > "$dir/plan"
  SWEEP_DETACHED=1 setsid nohup "$0" "$@" > "$dir/sweep.log" 2>&1 < /dev/null &
  echo $! > "$dir/pid"
  echo "$preset sweep started (pid $!): $(wc -l < "$dir/plan") runs"
  echo "progress: ./sweep.sh status $preset    live run: tail -f $PWD/$dir/*.log"
  exit 0
fi

label="$(label_for)"
total="$(wc -l < "$dir/plan")"
i=0
: > "$dir/status"
while IFS='|' read -r name flags; do
  i=$((i + 1))
  slug="$(echo "$name" | tr 'A-Z' 'a-z' | tr -c 'a-z0-9\n' '-' | sed 's/--*/-/g; s/-$//')"
  echo "[$i/$total] running  $name" >> "$dir/status"
  # shellcheck disable=SC2086  # flags is a deliberate word list
  if ./bt-new -config config.yaml -markets markets.yaml -data data $flags \
       -label "$label" -variant "$name" -report-json "$dir/$slug.json" \
       > "$dir/$slug.txt" 2> "$dir/$slug.log" < /dev/null; then
    net="$(sed -nE 's/^Net P&L +(\S+).*/\1/p' "$dir/$slug.txt")"
    sed -i "\$s/.*/[$i\/$total] done     $name  net $net/" "$dir/status"
  else
    sed -i "\$s/.*/[$i\/$total] FAILED   $name  (see $dir\/$slug.log)/" "$dir/status"
  fi
done < "$dir/plan"
echo "ALL DONE" >> "$dir/status"
rm -f "$dir/pid"
