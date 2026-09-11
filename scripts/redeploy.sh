#!/usr/bin/env bash
# Redeploy the recorder (predbot-record, authenticated WS) to the predbot VM (192.168.3.173).
#
# Runs tests, cross-compiles a static amd64 binary, syncs binary + config +
# markets + unit to the VM, and restarts the service, then verifies it came back
# healthy. Idempotent: safe to re-run; creates the user/dirs if missing.
#
# Usage:
#   scripts/redeploy.sh                 # test, build, deploy, verify
#   scripts/redeploy.sh --skip-tests    # skip the test step (faster)
#
# Override target via env: REMOTE=root@host  SERVICE=predbot-record
set -euo pipefail

REMOTE="${REMOTE:-root@192.168.3.173}"
SERVICE="${SERVICE:-predbot-record}"
REMOTE_DIR="${REMOTE_DIR:-/opt/predbot}"
UNIT_SRC="systemd/predbot-record.service"
SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=8)

# This script builds ./cmd/record and installs systemd/predbot-record.service.
# Pointing SERVICE at another unit does NOT redeploy that service — it OVERWRITES
# that unit file with the recorder's and restarts it. Doing this to predbot-live
# on 2026-08-25 replaced the live-trading unit with the recorder's, enabled a
# unit that is deliberately static, and left two recorders fighting over :9487.
# Live trading has its own path: scripts/trading.sh (deploy-live).
if [[ "$SERVICE" != predbot-record* ]]; then
  echo "refusing: redeploy.sh only deploys the RECORDER (got SERVICE=$SERVICE)." >&2
  echo "For live trading use scripts/trading.sh deploy-live." >&2
  exit 2
fi

cd "$(dirname "$0")/.."   # repo root
BIN_TMP="$(mktemp)"
trap 'rm -f "$BIN_TMP"' EXIT

step() { printf '\n\033[1;36m==> %s\033[0m\n' "$1"; }

if [[ "${1:-}" != "--skip-tests" ]]; then
  step "Running tests"
  GOTOOLCHAIN=auto go test ./...
fi

step "Cross-compiling static amd64 binary"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOTOOLCHAIN=auto go build -trimpath -o "$BIN_TMP" ./cmd/record
echo "built $(du -h "$BIN_TMP" | cut -f1)"

step "Ensuring user + directories on $REMOTE"
ssh "${SSH_OPTS[@]}" "$REMOTE" '
  id predbot >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin predbot
  mkdir -p '"$REMOTE_DIR"'/config /var/lib/predbot/data
'

step "Copying artifacts"
# Binary goes to a temp name first: mv-into-place avoids ETXTBSY while the old
# binary is still running, and keeps the swap atomic.
scp "${SSH_OPTS[@]}" -q "$BIN_TMP"                 "$REMOTE:$REMOTE_DIR/predbot-record.new"
scp "${SSH_OPTS[@]}" -q config/config.yaml config/markets.yaml "$REMOTE:$REMOTE_DIR/config/"
scp "${SSH_OPTS[@]}" -q "$UNIT_SRC"                "$REMOTE:/etc/systemd/system/$SERVICE.service"

step "Installing + restarting"
ssh "${SSH_OPTS[@]}" "$REMOTE" '
  set -e
  mv -f '"$REMOTE_DIR"'/predbot-record.new '"$REMOTE_DIR"'/predbot-record
  chmod 0755 '"$REMOTE_DIR"'/predbot-record
  chown -R predbot:predbot '"$REMOTE_DIR"' /var/lib/predbot
  systemctl daemon-reload
  systemctl enable '"$SERVICE"' >/dev/null 2>&1 || true
  systemctl restart '"$SERVICE"'
'

step "Verifying"
sleep 5
ssh "${SSH_OPTS[@]}" "$REMOTE" '
  set -e
  state=$(systemctl is-active '"$SERVICE"')
  echo "service: $state"
  [ "$state" = active ] || { journalctl -u '"$SERVICE"' --no-pager -n 15 -o cat; exit 1; }
  code=$(curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:9487/healthz || true)
  echo "health: $code"
  journalctl -u '"$SERVICE"' --no-pager -n 4 -o cat
'
step "Done"
echo "Dashboard: https://rpi5/predbot  (recorder metrics; live bot: https://rpi5/predbot-live)"
