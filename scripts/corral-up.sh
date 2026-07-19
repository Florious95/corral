#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
STATE_DIR=${CORRAL_STATE_DIR:-"$HOME/Library/Application Support/Corral"}
PIDFILE=${CORRAL_PIDFILE:-"$STATE_DIR/gateway.pid"}
LOG_FILE=${CORRAL_LOG:-/tmp/corral-gateway.log}
BUILD_LOG=${CORRAL_BUILD_LOG:-/tmp/corral-build.log}
ADDR=${GATEWAY_ADDR:-0.0.0.0:8787}
PORT=${ADDR##*:}
PROBE_HOST=${ADDR%:*}
case "$PROBE_HOST" in
    0.0.0.0 | "" | "[::]" | ::) PROBE_HOST=127.0.0.1 ;;
esac
HEALTH_URL="http://${PROBE_HOST}:${PORT}/api/health"
BINARY="$ROOT/gateway/corral-gateway"

mkdir -p "$STATE_DIR"
chmod 700 "$STATE_DIR"

if [[ -f "$PIDFILE" ]]; then
    existing_pid=$(tr -d '[:space:]' < "$PIDFILE")
    if [[ "$existing_pid" =~ ^[0-9]+$ ]] && kill -0 "$existing_pid" 2>/dev/null; then
        existing_command=$(ps -o comm= -p "$existing_pid" | xargs)
        if [[ ${existing_command##*/} != corral-gateway ]]; then
            echo "Pidfile points to a non-gateway process (pid=$existing_pid); refusing to continue." >&2
            exit 1
        fi
        echo "Gateway is already running (pid=$existing_pid)."
        exit 0
    fi
    rm -f "$PIDFILE"
fi

if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "Port $PORT is already in use by a process not managed by this pidfile." >&2
    exit 1
fi

if [[ ${TSNET_DISABLE:-0} != 1 ]]; then
    if ! command -v tailscale >/dev/null 2>&1; then
        echo "Tailscale is not installed. Install it from https://tailscale.com/download and retry." >&2
        exit 1
    fi
    if ! tailscale status >/dev/null 2>&1; then
        echo "Tailscale is not connected. Run 'tailscale up' first." >&2
        exit 1
    fi
fi

if command -v tmux >/dev/null 2>&1; then
    pane_count=$(tmux list-panes -a 2>/dev/null | wc -l | tr -d ' ' || true)
    echo "Local tmux panes: ${pane_count:-0}"
else
    echo "tmux is not installed; the gateway cannot discover panes."
fi

echo "Building gateway..."
(cd "$ROOT/gateway" && GOPROXY=${GOPROXY:-https://proxy.golang.org,direct} go build -o "$BINARY" .) >"$BUILD_LOG" 2>&1 || {
    echo "Gateway build failed; see $BUILD_LOG." >&2
    exit 1
}

echo "Starting gateway on $ADDR..."
nohup "$BINARY" >"$LOG_FILE" 2>&1 &
gateway_pid=$!
printf '%s\n' "$gateway_pid" > "$PIDFILE"
chmod 600 "$PIDFILE"

for _ in {1..100}; do
    if curl -fsS --max-time 1 "$HEALTH_URL" >/dev/null 2>&1; then
        echo "Gateway started (pid=$gateway_pid, address=$ADDR)."
        if [[ ${TSNET_DISABLE:-0} == 1 ]]; then
            echo "tsnet: disabled"
        else
            echo "Gateway log: $LOG_FILE"
        fi
        exit 0
    fi
    if ! kill -0 "$gateway_pid" 2>/dev/null; then
        break
    fi
    sleep 0.1
done

echo "Gateway failed to become healthy; see $LOG_FILE." >&2
kill -TERM "$gateway_pid" 2>/dev/null || true
rm -f "$PIDFILE"
tail -20 "$LOG_FILE" >&2 || true
exit 1
