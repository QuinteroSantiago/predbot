#!/usr/bin/env bash
# The measurement that decides the maker-rebate thesis.
#
# Runs the full-archive backtest with:
#   -maker    rest one leg (earning the Polymarket rebate) instead of taking both
#   -adverse  measure where the price goes AFTER a maker fill, driven by real
#             trade prints (a book cannot tell a trade from a cancellation)
#
# Verdict rule, printed in the output: the Poly maker rebate is ~0.31c/contract
# at p=0.5. If mean adverse drift exceeds the rebate plus captured spread, maker
# resting LOSES despite the attractive fee arithmetic.
#
# Writes a dated report and keeps it. Run by predbot-maker-analysis.timer.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.."; pwd)"
DATA="${DATA:-/var/lib/predbot/archive}"
OUTDIR="${OUTDIR:-/var/lib/predbot/reports}"
HOURS="${HOURS:-0}"   # 0 = the whole archive

mkdir -p "$OUTDIR"
STAMP="$(date -u +%Y-%m-%dT%H%M)"
OUT="$OUTDIR/maker-analysis-$STAMP.txt"

cd "$REPO"
{
  echo "predbot maker-rebate analysis — $STAMP UTC"
  echo "data: $DATA (window: ${HOURS}h, 0=all)"
  echo "trade prints recorded: $(zcat "$DATA"/trades/*.jsonl.gz 2>/dev/null | wc -l)"
  echo
  echo "--- TAKER baseline (both legs cross the book) ---"
  GOTOOLCHAIN=auto go run ./cmd/backtest -data "$DATA" -hours "$HOURS" 2>/dev/null | grep -vE "^\s*$"
  echo
  echo "--- MAKER + adverse selection ---"
  GOTOOLCHAIN=auto go run ./cmd/backtest -data "$DATA" -hours "$HOURS" -maker -adverse 2>/dev/null | grep -vE "^\s*$"
} > "$OUT" 2>&1

echo "report written: $OUT"
# Surface the headline lines in the journal so `journalctl -u ...` is enough.
grep -E "rests placed|adverse drift|Net P&L|attempted/completed|trade prints|VERDICT|no samples" "$OUT" || true
