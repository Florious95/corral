#!/usr/bin/env bash

set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
SOURCE=${RC_WEB_DIST_DIR:-"$ROOT/web/dist/"}
LOCAL_DEST=${RC_LOCAL_WEB_DEST:-"$HOME/Library/Application Support/Corral/web/dist/"}
REMOTE_DEST=${RC_REMOTE_WEB_DEST:-'\$HOME/Library/Application Support/Corral/web/dist/'}
TARGET_URL=${RC_TARGET_URL:-http://127.0.0.1:8787}
SSH_HOST=${RC_SSH_HOST:-}
CREDENTIAL_FILE=${RC_CREDENTIAL_FILE:-}
SSH_OPTS=(-o ConnectTimeout=8 -o ServerAliveInterval=5 -o ServerAliveCountMax=2)

if [[ ! -f "${SOURCE}index.html" ]]; then
    echo "missing ${SOURCE}index.html; run the web build first" >&2
    exit 1
fi

JS_REF=$(sed -n 's/.*src="\([^"]*\.js\)".*/\1/p' "${SOURCE}index.html" | head -n 1)
CSS_REF=$(sed -n 's/.*href="\([^"]*\.css\)".*/\1/p' "${SOURCE}index.html" | head -n 1)
if [[ -z "$JS_REF" || -z "$CSS_REF" || ! -f "${SOURCE}${JS_REF#/}" || ! -f "${SOURCE}${CSS_REF#/}" ]]; then
    echo "could not resolve built JS/CSS assets from ${SOURCE}index.html" >&2
    exit 1
fi
JS_SHA=$(shasum -a 256 "${SOURCE}${JS_REF#/}" | awk '{print $1}')
CSS_SHA=$(shasum -a 256 "${SOURCE}${CSS_REF#/}" | awk '{print $1}')

if [[ -z "$SSH_HOST" ]]; then
    mkdir -p "$LOCAL_DEST"
    rsync -a --delete "$SOURCE" "$LOCAL_DEST"
else
    remote_stage="/tmp/corral-web-dist/"
    ssh_prefix=()
    rsync_rsh="ssh ${SSH_OPTS[*]}"
    if [[ -n "$CREDENTIAL_FILE" ]]; then
        [[ -f "$CREDENTIAL_FILE" ]] || { echo "RC_CREDENTIAL_FILE does not exist" >&2; exit 1; }
        ssh_prefix=(sshpass -f "$CREDENTIAL_FILE")
        rsync_rsh="sshpass -f $CREDENTIAL_FILE ssh ${SSH_OPTS[*]}"
    fi
    "${ssh_prefix[@]}" ssh "${SSH_OPTS[@]}" "$SSH_HOST" "mkdir -p '$remote_stage'"
    rsync -az --delete --partial -e "$rsync_rsh" "$SOURCE" "$SSH_HOST:$remote_stage"
    "${ssh_prefix[@]}" ssh "${SSH_OPTS[@]}" "$SSH_HOST" \
        "mkdir -p \"$REMOTE_DEST\" && rsync -a --delete '$remote_stage' \"$REMOTE_DEST\""
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsS --max-time 15 -o "$tmp/index.html" "$TARGET_URL/"
remote_js=$(sed -n 's/.*src="\([^"]*\.js\)".*/\1/p' "$tmp/index.html" | head -n 1)
remote_css=$(sed -n 's/.*href="\([^"]*\.css\)".*/\1/p' "$tmp/index.html" | head -n 1)
[[ "$remote_js" == "$JS_REF" && "$remote_css" == "$CSS_REF" ]] || {
    echo "homepage asset references do not match the local build" >&2
    exit 1
}
curl -fsS --max-time 15 -o "$tmp/app.js" "$TARGET_URL$JS_REF"
curl -fsS --max-time 15 -o "$tmp/app.css" "$TARGET_URL$CSS_REF"
[[ "$(shasum -a 256 "$tmp/app.js" | awk '{print $1}')" == "$JS_SHA" ]]
[[ "$(shasum -a 256 "$tmp/app.css" | awk '{print $1}')" == "$CSS_SHA" ]]
echo "published and verified web assets at $TARGET_URL"
