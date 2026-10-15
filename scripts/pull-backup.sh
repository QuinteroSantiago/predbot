#!/usr/bin/env bash
# Pull the predbot recorder's data from the predbot VM (predbot-vm) to the Pi.
# Flushes the VM's current-hour gzip files first (SIGHUP) so the latest data is
# complete, then rsyncs incrementally. No --delete: the Pi keeps history even if
# something is ever removed on the VM (safe backup direction).
#
# Run daily by predbot-backup.timer. Override target/dest via env.
set -euo pipefail

# Use the ~/.ssh/config 'predbot-vm' alias, which pins an IdentityFile
# so this works under systemd with no ssh-agent (raw root@IP relies on the agent).
REMOTE="${REMOTE:-predbot-vm}"
SRC="/var/lib/predbot/data/"
DEST="${DEST:-/var/lib/predbot/archive}"
SSH="ssh -o BatchMode=yes -o ConnectTimeout=10"

mkdir -p "$DEST"

# Flush the current hour on the VM so its gzip stream is complete before copy.
$SSH "$REMOTE" 'systemctl kill -s HUP predbot-record 2>/dev/null || true'
sleep 2

rsync -az --info=stats2 -e "$SSH" "$REMOTE:$SRC" "$DEST/"
echo "predbot backup ok: $(du -sh "$DEST" | cut -f1) in $DEST ($(find "$DEST" -name '*.gz' | wc -l) files)"
