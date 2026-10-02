#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
OUTPUT="${ROOT_DIR}/myst"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --output|-o) [[ $# -ge 2 ]] || { echo "missing value for $1" >&2; exit 2; }; OUTPUT="$2"; shift 2;;
    --help|-h) sed -n '1,28p' "$0"; exit 0;;
    *) echo "unknown option: $1" >&2; exit 2;;
  esac
done

cd "$ROOT_DIR"
command -v go >/dev/null || { echo "error: Go is required" >&2; exit 1; }
command -v pkg-config >/dev/null || { echo "error: pkg-config is required for GTK4" >&2; exit 1; }
pkg-config --exists gtk4 || { echo "error: GTK4 development files are required (pkg-config gtk4)" >&2; exit 1; }

VERSION="${VERSION:-}"
if [[ -z "$VERSION" ]] && command -v git >/dev/null 2>&1 && git rev-parse --git-dir >/dev/null 2>&1; then
  VERSION="$(git describe --tags --always --dirty 2>/dev/null || true)"
fi
VERSION="${VERSION:-dev}"
mkdir -p "$(dirname -- "$OUTPUT")"
TMP="${OUTPUT}.tmp.$$"
trap 'rm -f -- "$TMP"' EXIT

# GTK is a native dependency, so CGO must remain enabled. This is still one
# executable: GUI, daemon, auth, IPC and VPN code are linked into the same ELF.
export CGO_ENABLED=1
LDFLAGS="-s -w -X main.version=${VERSION}"

echo '==> testing all packages'
go test ./...

echo "==> building single executable: myst ${VERSION}"
go build -buildvcs=false -trimpath -ldflags="$LDFLAGS" -o "$TMP" ./cmd/myst
chmod 0755 "$TMP"
mv -f -- "$TMP" "$OUTPUT"
trap - EXIT

file "$OUTPUT"
if command -v nm >/dev/null 2>&1; then
  if ! nm -g "$OUTPUT" 2>/dev/null | grep -q "daemon.RunServer"; then
    # Go symbol names may be stripped/encoded, so this is informational only.
    echo "note: symbol table is stripped; daemon code is linked into the same binary."
  fi
fi
echo
echo "Built: $OUTPUT"
echo "Run:   $OUTPUT"
echo "GUI:   $OUTPUT (default)"
echo "Daemon: $OUTPUT --daemon (internal)"
