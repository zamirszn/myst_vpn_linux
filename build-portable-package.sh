#!/usr/bin/env bash
# Build the release artifacts: a portable archive containing the daemon, the
# GTK app, the launcher, and install.sh.
#
# This is the only installer the release ships. There is deliberately no .deb:
# the package and the portable installer put their systemd units in different
# directories (/usr/lib and /etc), and when both have been used the /etc copy
# shadows the package's, so package upgrades silently stop taking effect.
set -euo pipefail
ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DIST="$ROOT_DIR/dist"
VERSION="${VERSION:-1.0.0}"
ARCH="${ARCH:-$(dpkg --print-architecture)}"
STAGE="$DIST/mysterium-vpn-linux-${ARCH}"
BIN_DIR="$DIST/bin"

if ! command -v go >/dev/null 2>&1; then
  echo 'ERROR: Go is required to build the package.' >&2
  exit 1
fi
if ! command -v pkg-config >/dev/null 2>&1 || ! pkg-config --exists gtk4; then
  echo 'ERROR: GTK 4 development files are missing.' >&2
  echo 'On Ubuntu/Debian: sudo apt update && sudo apt install -y build-essential pkg-config libgtk-4-dev gobject-introspection' >&2
  exit 1
fi

export CGO_ENABLED=1
mkdir -p "$BIN_DIR"

printf '\n==> Building the daemon\n'
(cd "$ROOT_DIR" && go build -buildvcs=false -trimpath \
  -ldflags "-s -w" \
  -o "$BIN_DIR/mystd" ./cmd/mystd)

printf '\n==> Building the GTK app\n'
(cd "$ROOT_DIR/desktop" && go build -buildvcs=false -trimpath \
  -ldflags "-s -w -X main.version=$VERSION" \
  -o "$BIN_DIR/myst-gui" ./cmd/myst-gui)

rm -rf "$STAGE"
mkdir -p "$STAGE/icons"
install -m 0755 "$BIN_DIR/mystd" "$STAGE/mystd"
install -m 0755 "$BIN_DIR/myst-gui" "$STAGE/myst-gui"
install -m 0755 "$ROOT_DIR/packaging/mystd-launch" "$STAGE/mystd-launch"
install -m 0755 "$ROOT_DIR/packaging/desktop-group" "$STAGE/desktop-group"
install -m 0755 "$ROOT_DIR/packaging/install.sh" "$STAGE/install.sh"
install -m 0755 "$ROOT_DIR/packaging/uninstall.sh" "$STAGE/uninstall.sh"
install -m 0644 "$ROOT_DIR/packaging/mystd.service" "$STAGE/mystd.service"
install -m 0644 "$ROOT_DIR/packaging/mysterium-vpn.desktop" "$STAGE/mysterium-vpn.desktop"
install -m 0644 "$ROOT_DIR/packaging/mysterium-vpn.png" "$STAGE/mysterium-vpn.png"
install -m 0644 "$ROOT_DIR/packaging/README-INSTALL.txt" "$STAGE/README-INSTALL.txt"
for size in 48 64 128 256 512; do
  install -m 0644 "$ROOT_DIR/packaging/icons/mysterium-vpn-${size}.png" \
    "$STAGE/icons/mysterium-vpn-${size}.png"
done

file "$STAGE/mystd" "$STAGE/myst-gui"
chmod 0755 "$STAGE/mystd" "$STAGE/myst-gui"
mkdir -p "$DIST"
rm -f "$DIST/mysterium-vpn-linux-${ARCH}.tar.gz" "$DIST/mysterium-vpn-linux-${ARCH}.zip"
tar -C "$DIST" -czf "$DIST/mysterium-vpn-linux-${ARCH}.tar.gz" "mysterium-vpn-linux-${ARCH}"
if command -v zip >/dev/null 2>&1; then
  (cd "$DIST" && zip -qr "mysterium-vpn-linux-${ARCH}.zip" "mysterium-vpn-linux-${ARCH}")
else
  echo 'zip utility is unavailable; created the .tar.gz package only.' >&2
fi

printf '\nCreated:\n'
printf '  %s\n' "$DIST/mysterium-vpn-linux-${ARCH}.tar.gz"
[[ ! -f "$DIST/mysterium-vpn-linux-${ARCH}.zip" ]] || printf '  %s\n' "$DIST/mysterium-vpn-linux-${ARCH}.zip"
printf '\nOn the other Ubuntu PC:\n'
printf '  tar xzf mysterium-vpn-linux-%s.tar.gz\n' "$ARCH"
printf '  cd mysterium-vpn-linux-%s && sudo ./install.sh\n' "$ARCH"
