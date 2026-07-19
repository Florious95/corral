#!/usr/bin/env bash
# corral-up.sh - start Corral on this computer
#
# This script makes Corral available to the user:
#   1. Verify that the Tailscale client is connected to the tailnet.
#   2. Report the local tmux pane count.
#   3. Build and start the gateway if needed (including tsnet.Server exposure).
#   4. Show the authorization URL if tsnet is not authenticated.
#   5. Print the mobile access URLs (tailnet hostname or IP on port 8787).
#
# Environment variables:
#   RC_HOSTNAME    Override the tsnet node name (default: <hostname>-rc).
#   TS_AUTHKEY     Authenticate tsnet without an interactive URL.
#   TSNET_DISABLE  Set to 1 to disable tsnet and serve only on :8787 (development).

set -euo pipefail
cd "$(dirname "$0")/.."
ROOT="$(pwd)"

# ---- 1. Check the Tailscale client ----
if ! command -v tailscale >/dev/null 2>&1; then
    echo "Tailscale is not installed. Install it from https://tailscale.com/download and retry." >&2
    exit 1
fi
if ! tailscale status >/dev/null 2>&1; then
    echo "Tailscale is not connected. Run 'tailscale up' first." >&2
    exit 1
fi

# jq is optional; fall back to text output when it is unavailable.
if command -v jq >/dev/null 2>&1; then
    MY_TS_NAME=$(tailscale status --json | jq -r '.Self.HostName')
    MY_TS_IP=$(tailscale ip -4 2>/dev/null | head -1)
else
    MY_TS_NAME=$(tailscale status | awk 'NR==1 {print $2}')
    MY_TS_IP=$(tailscale ip -4 2>/dev/null | head -1)
fi
echo "Tailnet connected: $MY_TS_NAME ($MY_TS_IP)"

# ---- 2. Report tmux panes ----
if command -v tmux >/dev/null 2>&1; then
    TMUX_PANE_COUNT=$(tmux list-panes -a 2>/dev/null | wc -l | tr -d ' ' || echo 0)
    echo "Local tmux panes: $TMUX_PANE_COUNT"
else
    echo "tmux is not installed; the gateway cannot discover panes."
fi

# ---- 3. build + start gateway ----
if lsof -i:8787 >/dev/null 2>&1; then
    GW_PID=$(lsof -nP -i:8787 -sTCP:LISTEN -t)
    echo "Gateway is already running (pid=$GW_PID)."
else
    echo "Building gateway..."
    cd "$ROOT/gateway"
    GOPROXY=${GOPROXY:-https://proxy.golang.org,direct} go build -o corral-gateway . >/tmp/corral-build.log 2>&1 || {
        echo "Gateway build failed; see /tmp/corral-build.log." >&2
        exit 1
    }
    echo "Starting gateway..."
    nohup ./corral-gateway > /tmp/corral-gateway.log 2>&1 &
    disown
    sleep 2
    if ! lsof -i:8787 >/dev/null 2>&1; then
        echo "Gateway failed to start; see /tmp/corral-gateway.log." >&2
        tail -20 /tmp/corral-gateway.log >&2
        exit 1
    fi
    GW_PID=$(lsof -nP -i:8787 -sTCP:LISTEN -t)
    echo "Gateway started (pid=$GW_PID)."
fi

# ---- 4. Check tsnet status ----
sleep 1
TSNET_HOSTNAME=$(grep -oE 'tsnet: registered as [a-z0-9-]+' /tmp/corral-gateway.log 2>/dev/null | tail -1 | awk '{print $NF}' || true)
AUTH_URL=$(grep -oE 'https://login\.tailscale\.com/a/[a-z0-9]+' /tmp/corral-gateway.log 2>/dev/null | tail -1 || true)

echo ""
echo "---------------- Mobile access ----------------"
echo "Option 1 (available immediately):"
echo "   Install Tailscale on the phone, join the same tailnet, and open:"
echo "   http://${MY_TS_IP}:8787"
echo ""
if [ -n "$AUTH_URL" ]; then
    echo "Option 2 (optional dedicated tsnet node):"
    echo "   Open this authorization URL once:"
    echo "   $AUTH_URL"
    if [ -n "$TSNET_HOSTNAME" ]; then
        echo "   Then open: http://${TSNET_HOSTNAME}:8787"
    fi
elif [ -n "$TSNET_HOSTNAME" ]; then
    echo "Option 2 (dedicated tsnet node enabled):"
    echo "   http://${TSNET_HOSTNAME}:8787"
else
    echo "Option 2 (optional): set TS_AUTHKEY, unset TSNET_DISABLE, and restart the gateway,"
    echo "   or run TS_AUTHKEY=<key> ./gateway/corral-gateway"
fi
echo "-------------------------------------------------"
echo ""
echo "Gateway log: /tmp/corral-gateway.log"
echo "Network diagnostics: curl http://127.0.0.1:8787/api/network | jq"
