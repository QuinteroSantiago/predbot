#!/usr/bin/env bash
# Retention for the predbot recorder.
#
# Deletes all recorder data older than KEEP_DAYS. Edge-pinned hours
# (data/keep/<YYYY-MM-DDTHH>.keep) are NOT exempt: at the 2c edge threshold
# ~95% of hours got pinned, so honoring pins meant no retention at all and the
# VM disk filled. Full history, pinned hours included, lives on the Pi
# (predbot-backup pulls daily with no --delete).
#
# Run on the VM by predbot-prune.timer. Dry-run by default; pass --apply.
set -euo pipefail

DATA="${DATA:-/var/lib/predbot/data}"
KEEP_DAYS="${KEEP_DAYS:-14}"
APPLY="${1:-}"

cutoff=$(date -u -d "$KEEP_DAYS days ago" +%s)
deleted=0; freed=0

shopt -s nullglob
for f in "$DATA"/raw/*.jsonl.gz "$DATA"/normalized/*.jsonl.gz "$DATA"/trades/*.jsonl.gz; do
  base=$(basename "$f")
  # <venue>-<YYYY-MM-DDTHH>.jsonl.gz or <venue>-trades-<YYYY-MM-DDTHH>.jsonl.gz.
  # Match the timestamp itself: stripping up to the last "-2" (the old approach)
  # also matched the day on the 20th-29th, so those files never parsed and were
  # silently never pruned.
  [[ "$base" =~ ([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2})\.jsonl\.gz$ ]] || continue
  hour="${BASH_REMATCH[1]}"
  # file's hour as epoch (treat as UTC)
  ts=$(date -u -d "${hour//T/ }:00:00" +%s 2>/dev/null || echo 0)
  [ "$ts" = 0 ] && continue
  [ "$ts" -ge "$cutoff" ] && continue          # inside the retention window

  sz=$(stat -c %s "$f")
  freed=$((freed+sz)); deleted=$((deleted+1))
  [ "$APPLY" = "--apply" ] && rm -f "$f"
done

printf 'retention: keep_days=%s  deleted=%s files (%s MB)  mode=%s\n' \
  "$KEEP_DAYS" "$deleted" "$((freed/1024/1024))" \
  "$([ "$APPLY" = "--apply" ] && echo APPLIED || echo DRY-RUN)"
