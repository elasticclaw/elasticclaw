#!/usr/bin/env bash
# Start the seeded chat-visual hub in the background and wait for its port.
#
#   hack/chat-visual/run-hub.sh          # start (default port 8085)
#   HUB_PORT=9090 hack/chat-visual/run-hub.sh
#   hack/chat-visual/run-hub.sh stop
#
# The port must match NEXT_PUBLIC_HUB_URL in web/.env.local.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HERE="$REPO_ROOT/hack/chat-visual"
HUB_PORT="${HUB_PORT:-8085}"
PIDFILE="$HERE/hub.pid"
LOGFILE="${HUB_LOG:-/tmp/ec-chat-visual-hub.log}"

stop_hub() {
  if [[ -f "$PIDFILE" ]]; then
    pid="$(cat "$PIDFILE")"
    if kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
      sleep 1
      kill -9 "$pid" 2>/dev/null || true
    fi
    rm -f "$PIDFILE"
  fi
  # The `go test` wrapper spawns the real binary; clear anything still on the port.
  lsof -ti tcp:"$HUB_PORT" 2>/dev/null | xargs -r kill -9 2>/dev/null || true
}

if [[ "${1:-start}" == "stop" ]]; then
  stop_hub
  echo "chat-visual hub stopped (port $HUB_PORT)"
  exit 0
fi

stop_hub

cd "$REPO_ROOT"
# nohup + pidfile, never a backgrounded foreground job: the harness blocks
# forever on select{} and must outlive the shell that started it.
nohup env MANUAL_VERIFY=1 HUB_PORT="$HUB_PORT" \
  go test ./pkg/hub -run TestChatVisualHarness -timeout 0 -count=1 -v \
  >"$LOGFILE" 2>&1 &
echo $! > "$PIDFILE"

for _ in $(seq 1 120); do
  if nc -z localhost "$HUB_PORT" 2>/dev/null; then
    echo "chat-visual hub up on http://localhost:$HUB_PORT (pid $(cat "$PIDFILE"), log $LOGFILE)"
    grep -E '^\[chat-visual\]' "$LOGFILE" || true
    exit 0
  fi
  sleep 1
done

echo "chat-visual hub failed to bind port $HUB_PORT; last log lines:" >&2
tail -n 40 "$LOGFILE" >&2
exit 1
