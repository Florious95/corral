#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

if ! command -v flutter >/dev/null 2>&1; then
    echo "Flutter is not installed or not available on PATH." >&2
    exit 1
fi

flutter pub get
flutter build apk "$@"
