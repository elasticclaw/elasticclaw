package hub

// daytonaGatewayRivalPattern is the pgrep -f pattern for a gateway process. It
// must not match the supervisor's own command line, which embeds the script.
const daytonaGatewayRivalPattern = `(^|/)openclaw[ -]gateway( |$)`

// daytonaGatewaySupervisorScript keeps `openclaw gateway run` alive on Daytona,
// where no claw-bridge bootstrap supervises it. It runs as
// `bash -c <script> name PIDFILE LOG [PORT]` and restarts the gateway with
// backoff after any exit, up to a budget that resets once a gateway stays up
// for five minutes. ~/.openclaw/env is sourced before every start, so a
// restart picks up env changes made after bootstrap.
//
// To restart the gateway explicitly, kill it and let the supervisor start the
// next one. To run a gateway by hand, TERM the supervisor first (pid in
// ~/.openclaw/run/openclaw-gateway-supervisor.pid). A gateway started behind
// the supervisor's back is still tolerated: when a restart is due and another
// gateway process or a listener on the port exists, the supervisor gives it
// 30s to become healthy and then yields instead of racing it for the port.
func daytonaGatewaySupervisorScript() string {
	return `PIDFILE="$1"
LOG="$2"
PORT="${3:-18789}"
exec >> "$LOG" 2>&1
budget=5
echo $$ > "$PIDFILE"
trap 'rm -f "$PIDFILE"' EXIT
child=""
trap 'if [ -n "$child" ]; then kill "$child" 2>/dev/null; wait "$child" 2>/dev/null; fi; exit 0' TERM INT
# Restart in-process on config changes instead of spawning a detached gateway
# the supervisor cannot see.
export OPENCLAW_NO_RESPAWN=1
gateway_healthy() {
  curl -sf --max-time 5 "http://localhost:$PORT/healthz" >/dev/null 2>&1
}
# A gateway this supervisor did not start: its process, or a listener on the port.
rival_gateway() {
  pgrep -f '` + daytonaGatewayRivalPattern + `' >/dev/null 2>&1 || (exec 3<>"/dev/tcp/127.0.0.1/$PORT") 2>/dev/null
}
restarts=0
backoff=5
while :; do
  started_at=$(date +%s)
  if [ -f "$HOME/.openclaw/env" ]; then . "$HOME/.openclaw/env"; fi
  openclaw gateway run </dev/null &
  child=$!
  wait "$child"
  rc=$?
  child=""
  if [ $(($(date +%s) - started_at)) -ge 300 ]; then
    restarts=0
    backoff=5
  fi
  if [ "$restarts" -ge "$budget" ]; then
    echo "[gateway-supervisor] openclaw gateway exited (code=$rc); restart budget exhausted after $budget attempts"
    exit 1
  fi
  restarts=$((restarts + 1))
  echo "[gateway-supervisor] openclaw gateway exited (code=$rc); restarting (attempt $restarts/$budget) in ${backoff}s"
  sleep "$backoff"
  waited=0
  while ! gateway_healthy && rival_gateway; do
    if [ "$waited" -ge 30 ]; then
      echo "[gateway-supervisor] another gateway is not healthy after ${waited}s; restarting anyway"
      break
    fi
    sleep 1
    waited=$((waited + 1))
  done
  if gateway_healthy; then
    echo "[gateway-supervisor] another gateway is already serving port $PORT; supervisor exiting"
    exit 0
  fi
  backoff=$((backoff * 2))
done`
}

// daytonaStartGatewayCommand configures the gateway bind/port, starts the
// gateway under its supervisor (once: a retried bootstrap step reuses the
// running supervisor), and waits for it to become ready.
func daytonaStartGatewayCommand() string {
	return `
python3 - <<'PYEOF'
import json, os
path = os.path.expanduser('~/.openclaw/openclaw.json')
with open(path) as f: cfg = json.load(f)
cfg.setdefault('gateway', {})['bind'] = 'loopback'
cfg['gateway']['port'] = 18789
# Keep token auth that onboard generated - don't change auth mode
with open(path, 'w') as f: json.dump(cfg, f, indent=2)
print('gateway config updated')
PYEOF
export NVM_DIR="/usr/local/share/nvm"; [ -s "$NVM_DIR/nvm.sh" ] && source "$NVM_DIR/nvm.sh"
export NVM_DIR=/usr/local/share/nvm; export PATH=$NVM_DIR/current/bin:$PATH
mkdir -p ~/.openclaw/run
GATEWAY_PIDFILE="$HOME/.openclaw/run/openclaw-gateway-supervisor.pid"
if [ -s "$GATEWAY_PIDFILE" ] && kill -0 "$(cat "$GATEWAY_PIDFILE")" 2>/dev/null; then
  echo "gateway supervisor already running pid=$(cat "$GATEWAY_PIDFILE")"
else
  rm -f "$GATEWAY_PIDFILE"
  setsid nohup bash -c ` + shellQuote(daytonaGatewaySupervisorScript()) + ` openclaw-gateway-supervisor "$GATEWAY_PIDFILE" ~/.openclaw/gateway.log >/dev/null 2>&1 </dev/null &
fi
# Phase 1: wait for HTTP server to be listening (quick)
for _ in $(seq 1 30); do
  curl -sf http://localhost:18789/healthz >/dev/null && echo 'gateway listening' && break
  sleep 1
done
curl -sf http://localhost:18789/healthz >/dev/null || { echo 'gateway failed to listen'; tail -n 100 ~/.openclaw/gateway.log 2>/dev/null || true; exit 1; }
# Phase 2: wait for gateway startup to complete. Do not use openclaw health
# here: it pairs the CLI device with read-only scopes before claw-bridge can
# connect, then claw-bridge is rejected as a scope-upgrade.
for _ in $(seq 1 30); do
  if grep -q 'gateway ready' ~/.openclaw/gateway.log 2>/dev/null; then
    echo "gateway ready"
    exit 0
  fi
  curl -sf http://localhost:18789/healthz >/dev/null || break
  sleep 1
done
# Fallback: if the readiness log line is unavailable but the gateway is still
# listening and healthy, don't fail the bootstrap.
if curl -sf http://localhost:18789/healthz >/dev/null; then
  echo "gateway ready (healthz)"
  exit 0
fi
echo 'gateway not ready'
tail -n 100 ~/.openclaw/gateway.log 2>/dev/null || true
exit 1`
}
