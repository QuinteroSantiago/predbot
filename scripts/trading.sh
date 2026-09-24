#!/usr/bin/env bash
# Control predbot LIVE TRADING (real money) on the predbot VM (192.168.3.173).
# Operator manual: https://rpi5/predbot-manual.html
#
#   trading.sh start    arm and run continuously (extra args go to preflight)
#   trading.sh stop     stop trading, then cancel every open Poly order
#   trading.sh kill     ENGAGE KILL SWITCH: stop, cancel open Poly orders, AND
#                       block restarts until cleared
#   trading.sh cancel-open   list open Poly orders and cancel them all
#   trading.sh status   is it running, what has it done
#   trading.sh logs     follow the live log
#   trading.sh clear    remove the KILLED marker (human decision, after review)
#   trading.sh adopt-once   permit the NEXT start to adopt the positions already
#                           on the venues. Consumed by that start — never sticks.
#   trading.sh deploy-live  test, build and install the live binary + unit
#                           (NEVER starts it — arming stays a separate act)
set -euo pipefail

REMOTE="${REMOTE:-predbot-vm}"
SVC=predbot-live
SSH=(ssh -o BatchMode=yes "$REMOTE")

case "${1:-status}" in
  start)
    # Extra args are passed to preflight. To arm while holding reviewed
    # positions, use the one-shot ADOPT marker (adopt-once): preflight READS it
    # and the service CONSUMES it, so both reach the same verdict and adoption
    # never outlives the review. Never put -adopt-positions in the unit — that
    # would adopt whatever is on the venues on every future restart.
    echo "Pre-flight reconciliation…"
    "${SSH[@]}" "set -a; . /etc/predbot/env; set +a; /opt/predbot/preflight ${*:2}" || {
      echo "PRE-FLIGHT FAILED — not starting."
      echo "If it refused over pre-existing positions: flatten them, or (having"
      echo "reviewed them) authorize a one-shot adoption and start again:"
      echo "  ssh $REMOTE 'sudo -u predbot touch /var/lib/predbot/ADOPT'   (or: $0 adopt-once)"
      exit 1; }
    "${SSH[@]}" "sudo systemctl start $SVC && sleep 3 && systemctl is-active $SVC"
    echo "LIVE TRADING STARTED. Stop with: $0 stop   |  Emergency: $0 kill"
    ;;
  stop)
    "${SSH[@]}" "sudo systemctl stop $SVC"
    echo "stopped. Cancelling any open Polymarket orders (the bot only rests there)…"
    "$0" cancel-open || echo "!!! open orders may remain — check the Polymarket app"
    ;;
  cancel-open)
    # Orphaned rests keep working after the bot stops and fill with no hedge.
    "${SSH[@]}" "set -a; . /etc/predbot/env; set +a; /opt/predbot/cancelopen -yes"
    ;;
  kill)
    # Engage the kill switch first (persists a marker that blocks restart), then stop.
    "${SSH[@]}" "sudo systemctl reload $SVC 2>/dev/null || true; sleep 1; sudo systemctl stop $SVC"
    "${SSH[@]}" "sudo -u predbot touch /var/lib/predbot/KILLED"
    "$0" cancel-open || echo "!!! open orders may remain — check the Polymarket app"
    echo "KILL SWITCH ENGAGED — trading stopped and restarts BLOCKED."
    echo "Review positions in both venue UIs, then: $0 clear"
    ;;
  status)
    "${SSH[@]}" "systemctl is-active $SVC || true; \
      test -f /var/lib/predbot/KILLED && echo 'KILLED marker present (restarts blocked)' || true; \
      journalctl -u $SVC --no-pager -n 12 -o cat 2>/dev/null | grep -iE 'stats|ARMED|KILL|placed|pair' | tail -6 || true"
    ;;
  logs)
    "${SSH[@]}" "journalctl -u $SVC -f -o cat"
    ;;
  adopt-once)
    # The mirror image of the KILLED marker: a deliberate human permission, and
    # one the bot destroys as it uses it. Adoption applies to the specific
    # positions someone has just reviewed, so it must not outlive that review —
    # which is why this is a marker and not a flag in the unit.
    echo "Positions that the next start would adopt:"
    "${SSH[@]}" 'set -a; . /etc/predbot/env; set +a; /opt/predbot/dumppos' 2>/dev/null \
      | grep -E '^\s{6}|PARSED' || true
    echo
    read -r -p "Adopt these on the next start? [y/N] " ok
    [[ "$ok" == "y" || "$ok" == "Y" ]] || { echo "aborted — marker NOT written."; exit 1; }
    "${SSH[@]}" "sudo -u predbot touch /var/lib/predbot/ADOPT"
    echo "ADOPT marker written. It is consumed by the next start; the one after"
    echo "that refuses pre-existing positions again."
    ;;

  deploy-live)
    # Deliberately separate from scripts/redeploy.sh, which deploys the RECORDER.
    # Never starts the service: installing code and arming real-money trading are
    # two different decisions and must stay two different commands.
    cd "$(dirname "$0")/.."

    # Refuse to deploy code that has no commit behind it. The journal stamps the
    # build id at ARM time, and that stamp is worthless if it can point at a
    # working tree that has since changed: the whole point is to be able to ask
    # "what code placed that trade?" months later and get a real answer.
    if ! git rev-parse --git-dir >/dev/null 2>&1; then
      echo "refusing: not a git repository — live builds must be traceable to a commit." >&2; exit 1
    fi
    if [[ -n "$(git status --porcelain)" ]]; then
      echo "refusing: working tree is dirty. Commit (or stash) before deploying live:" >&2
      git status --short >&2
      exit 1
    fi
    BUILD_ID="$(git rev-parse --short HEAD)"
    echo "Build $BUILD_ID"

    echo "Running tests…"
    GOTOOLCHAIN=auto go test ./... >/dev/null || { echo "TESTS FAILED — not deploying."; exit 1; }
    tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
    echo "Building amd64 binaries…"
    for c in live dumppos preflight cancelopen; do
      out="$tmp/$([ "$c" = live ] && echo predbot-live || echo "$c")"
      CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOTOOLCHAIN=auto go build -trimpath \
        -ldflags "-X main.build=$BUILD_ID" -o "$out" "./cmd/$c"
    done
    echo "Copying to $REMOTE…"
    scp -o BatchMode=yes -q "$tmp"/* "$REMOTE:/tmp/"
    scp -o BatchMode=yes -q systemd/predbot-live.service "$REMOTE:/tmp/predbot-live.service"
    "${SSH[@]}" '
      set -e
      for b in predbot-live dumppos preflight cancelopen; do
        sudo mv -f /tmp/$b /opt/predbot/$b
        sudo chmod 0755 /opt/predbot/$b
        sudo chown predbot:predbot /opt/predbot/$b
      done
      sudo mv -f /tmp/predbot-live.service /etc/systemd/system/predbot-live.service
      sudo systemctl daemon-reload
      # Must stay static: a reboot must never silently resume trading.
      sudo systemctl disable predbot-live >/dev/null 2>&1 || true
      echo "unit: $(systemctl is-enabled predbot-live 2>&1)  state: $(systemctl is-active predbot-live)"
    '
    echo "DEPLOYED build $BUILD_ID — not started. Arm with: $0 start"
    ;;
  clear)
    "${SSH[@]}" "sudo rm -f /var/lib/predbot/KILLED && echo 'KILLED marker removed'"
    ;;
  *)
    echo "usage: $0 {start|stop|kill|status|logs|clear|adopt-once|deploy-live|cancel-open}"; exit 2 ;;
esac
