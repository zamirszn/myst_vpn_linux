#!/usr/bin/env bash
set -euo pipefail
ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT_DIR/desktop"
command -v go >/dev/null || { echo 'error: Go is required' >&2; exit 1; }
command -v pkg-config >/dev/null || { echo 'error: pkg-config is required' >&2; exit 1; }
pkg-config --exists gtk4 || { echo 'error: GTK4 development files are required (pkg-config gtk4)' >&2; exit 1; }
export CGO_ENABLED=1
OUT="$ROOT_DIR/desktop/myst-gui"
TMP="${OUT}.tmp.$$"
trap 'rm -f "$TMP"' EXIT
go test ./cmd/myst-gui ./internal/daemonclient
go build -buildvcs=false -trimpath -o "$TMP" ./cmd/myst-gui
chmod 0755 "$TMP"
mv -f "$TMP" "$OUT"
trap - EXIT
echo "Built updated GUI: $OUT"
