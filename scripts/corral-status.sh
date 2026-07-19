#!/usr/bin/env bash
set -euo pipefail

STATE_DIR=${CORRAL_STATE_DIR:-"$HOME/Library/Application Support/Corral"}
PIDFILE=${CORRAL_PIDFILE:-"$STATE_DIR/gateway.pid"}
LOG_FILE=${CORRAL_LOG:-/tmp/corral-gateway.log}

pane_count=0
if command -v tmux >/dev/null 2>&1; then
    pane_count=$(tmux list-panes -a 2>/dev/null | wc -l | tr -d ' ' || true)
fi

if [[ ! -f "$PIDFILE" ]]; then
    echo "Gateway: not running"
    echo "Local tmux panes: ${pane_count:-0}"
    exit 0
fi

pid=$(tr -d '[:space:]' < "$PIDFILE")
if [[ ! "$pid" =~ ^[0-9]+$ ]] || ! kill -0 "$pid" 2>/dev/null; then
    echo "Gateway: not running (stale pidfile: $PIDFILE)"
    echo "Local tmux panes: ${pane_count:-0}"
    exit 0
fi

command_name=$(ps -o comm= -p "$pid" | xargs)
if [[ ${command_name##*/} != corral-gateway ]]; then
    echo "Gateway: not running (pidfile points to another process: $pid)"
    echo "Local tmux panes: ${pane_count:-0}"
    exit 0
fi

listeners=$(lsof -a -p "$pid" -iTCP -sTCP:LISTEN -nP -F n 2>/dev/null | sed -n 's/^n//p' | paste -sd, -)
if grep -q 'tsnet: disabled by TSNET_DISABLE=1' "$LOG_FILE" 2>/dev/null; then
    tsnet_status=disabled
elif tsnet_name=$(grep -oE 'tsnet: registered as [a-z0-9-]+' "$LOG_FILE" 2>/dev/null | tail -1 | awk '{print $NF}') && [[ -n "$tsnet_name" ]]; then
    tsnet_status="active ($tsnet_name)"
elif grep -q 'login\.tailscale\.com/a/' "$LOG_FILE" 2>/dev/null; then
    tsnet_status='awaiting authorization'
else
    tsnet_status='not ready'
fi

echo "Gateway: running (pid=$pid)"
echo "Listening: ${listeners:-none}"
echo "tsnet: $tsnet_status"
echo "Local tmux panes: ${pane_count:-0}"
