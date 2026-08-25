#!/usr/bin/env bash
# Retention for the predbot recorder.
#
# Deletes routine data older than KEEP_DAYS, but NEVER deletes an hour that the
# recorder flagged as interesting — i.e. an hour in which a cross-venue edge
# appeared. Those hours (and their neighbours) are pinned forever by a marker
# file in data/keep/<YYYY-MM-DDTHH>.keep, so the evidence of every real (or
# near-miss) arbitrage survives pruning.
#
# Run on the VM by predbot-prune.timer. Dry-run by default; pass --apply.
set -euo pipefail

DATA="${DATA:-/var/lib/predbot/data}"
KEEP_DAYS="${KEEP_DAYS:-14}"
APPLY="${1:-}"

cutoff=$(date -u -d "$KEEP_DAYS days ago" +%s)
kept=0; deleted=0; freed=0

shopt -s nullglob
for f in "$DATA"/raw/*.jsonl.gz "$DATA"/normalized/*.jsonl.gz; do
  base=$(basename "$f")
  # <venue>-<YYYY-MM-DDTHH>.jsonl.gz
  hour=${base##*-2}; hour="2${hour%%.jsonl.gz}"
  # file's hour as epoch (treat as UTC)
  ts=$(date -u -d "${hour//T/ }:00:00" +%s 2>/dev/null || echo 0)
  [ "$ts" = 0 ] && continue
  [ "$ts" -ge "$cutoff" ] && continue          # inside the retention window

  if [ -f "$DATA/keep/$hour.keep" ]; then      # pinned: an edge appeared here
    kept=$((kept+1)); continue
  fi
  sz=$(stat -c %s "$f")
  freed=$((freed+sz)); deleted=$((deleted+1))
  [ "$APPLY" = "--apply" ] && rm -f "$f"
done

printf 'retention: keep_days=%s  deleted=%s files (%s MB)  pinned-by-edge=%s  mode=%s\n' \
  "$KEEP_DAYS" "$deleted" "$((freed/1024/1024))" "$kept" \
  "$([ "$APPLY" = "--apply" ] && echo APPLIED || echo DRY-RUN)"
