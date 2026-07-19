#!/usr/bin/env bash
set -euo pipefail

STATE_DIR=${CORRAL_STATE_DIR:-"$HOME/Library/Application Support/Corral"}
PIDFILE=${CORRAL_PIDFILE:-"$STATE_DIR/gateway.pid"}

if [[ ! -f "$PIDFILE" ]]; then
    echo "Gateway is not running."
    exit 0
fi

pid=$(tr -d '[:space:]' < "$PIDFILE")
if [[ ! "$pid" =~ ^[0-9]+$ ]] || ! kill -0 "$pid" 2>/dev/null; then
    rm -f "$PIDFILE"
    echo "Gateway is not running."
    exit 0
fi

command_name=$(ps -o comm= -p "$pid" | xargs)
if [[ ${command_name##*/} != corral-gateway ]]; then
    echo "Pidfile points to a non-gateway process (pid=$pid); refusing to signal it." >&2
    exit 1
fi

kill -TERM "$pid"
for _ in {1..100}; do
    state=$(ps -o stat= -p "$pid" 2>/dev/null | tr -d '[:space:]' || true)
    if [[ -z "$state" || "$state" == Z* ]]; then
        rm -f "$PIDFILE"
        echo "Gateway stopped."
        exit 0
    fi
    sleep 0.1
done

echo "Gateway did not stop within 10 seconds; sending SIGKILL." >&2
kill -KILL "$pid" 2>/dev/null || true
rm -f "$PIDFILE"
echo "Gateway stopped."
