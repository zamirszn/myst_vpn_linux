#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
OUTPUT="${ROOT_DIR}/myst"
VERBOSE=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --output|-o)
      [[ $# -ge 2 ]] || {
        echo "missing value for $1" >&2
        exit 2
      }
      OUTPUT="$2"
      shift 2
      ;;

    --verbose|-v)
      VERBOSE=1
      shift
      ;;

    --help|-h)
      sed -n '1,45p' "$0"
      exit 0
      ;;

    *)
      echo "unknown option: $1" >&2
      exit 2
      ;;
  esac
done

cd "$ROOT_DIR"

command -v go >/dev/null || {
  echo "error: Go is required" >&2
  exit 1
}

command -v pkg-config >/dev/null || {
  echo "error: pkg-config is required for GTK4" >&2
  exit 1
}

pkg-config --exists gtk4 || {
  echo "error: GTK4 development files are required (pkg-config gtk4)" >&2
  exit 1
}

VERSION="${VERSION:-}"

if [[ -z "$VERSION" ]] && \
   command -v git >/dev/null 2>&1 && \
   git rev-parse --git-dir >/dev/null 2>&1; then
  VERSION="$(git describe --tags --always --dirty 2>/dev/null || true)"
fi

VERSION="${VERSION:-dev}"

mkdir -p "$(dirname -- "$OUTPUT")"

TMP="${OUTPUT}.tmp.$$"
trap 'rm -f -- "$TMP"' EXIT

# GTK is a native dependency, so CGO must remain enabled.
# GUI, daemon, auth, IPC and VPN code are linked into the same ELF.
export CGO_ENABLED=1

LDFLAGS="-s -w -X main.version=${VERSION}"

echo
echo "========================================"
echo " Mysterium Linux Release Build"
echo "========================================"
echo "Root:     $ROOT_DIR"
echo "Output:   $OUTPUT"
echo "Version:  $VERSION"
echo "Verbose:  $([[ "$VERBOSE" -eq 1 ]] && echo yes || echo no)"
echo "========================================"
echo

# ------------------------------------------------------------
# Tests
# ------------------------------------------------------------

echo "==> Testing all Go packages..."

if [[ "$VERBOSE" -eq 1 ]]; then
  go test -v ./...
else
  go test ./...
fi

echo
echo "==> Tests completed successfully."
echo

# ------------------------------------------------------------
# Build
# ------------------------------------------------------------

echo "==> Building single executable..."
echo "    CGO_ENABLED=$CGO_ENABLED"
echo "    Output=$TMP"
echo

if [[ "$VERBOSE" -eq 1 ]]; then

  echo "==> Go verbose build enabled"
  echo "    -v  Show packages being compiled"
  echo "    -x  Show commands executed by Go"
  echo

  go build \
    -v \
    -x \
    -buildvcs=false \
    -trimpath \
    -ldflags="$LDFLAGS" \
    -o "$TMP" \
    ./cmd/myst

else

  go build \
    -buildvcs=false \
    -trimpath \
    -ldflags="$LDFLAGS" \
    -o "$TMP" \
    ./cmd/myst

fi

echo
echo "==> Build completed."

# ------------------------------------------------------------
# Install final executable
# ------------------------------------------------------------

echo "==> Setting executable permissions..."
chmod 0755 "$TMP"

echo "==> Installing final executable..."
mv -f -- "$TMP" "$OUTPUT"

trap - EXIT

# ------------------------------------------------------------
# Verify
# ------------------------------------------------------------

echo
echo "==> Verifying executable..."

file "$OUTPUT"

if [[ "$VERBOSE" -eq 1 ]]; then
  echo
  echo "==> Executable information:"
  ls -lh "$OUTPUT"

  echo
  echo "==> Checking dynamic dependencies:"
  if command -v ldd >/dev/null 2>&1; then
    ldd "$OUTPUT" || true
  fi
fi

if command -v nm >/dev/null 2>&1; then
  if ! nm -g "$OUTPUT" 2>/dev/null | grep -q "daemon.RunServer"; then
    echo "note: symbol table is stripped; daemon code is linked into the same binary."
  fi
fi

echo
echo "========================================"
echo " Build successful!"
echo "========================================"
echo "Built:   $OUTPUT"
echo "Run:     $OUTPUT"
echo "GUI:     $OUTPUT (default)"
echo "Daemon:  $OUTPUT --daemon (internal)"
echo "========================================"
