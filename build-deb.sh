#!/usr/bin/env bash
# Legacy: build a Debian package.
#
# Releases no longer ship a .deb; ./build-portable-package.sh produces the
# supported artifact. A .deb and the portable installer put their systemd
# units in different directories, and a leftover /etc unit silently shadows
# the package's, so mixing the two installs makes upgrades stop taking effect.
# This script is kept for people who still want a package; install.sh refuses
# to run while one is installed.
set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DIST="$ROOT_DIR/dist"
BIN_DIR="$DIST/bin"
VERSION="${VERSION:-1.0.0}"
ARCH="${ARCH:-$(dpkg --print-architecture)}"
PKG_NAME="mysterium-vpn_${VERSION}_${ARCH}"
PKG_DIR="$DIST/deb/$PKG_NAME"

command -v go >/dev/null || { echo 'ERROR: Go is required to build the package.' >&2; exit 1; }
command -v dpkg-deb >/dev/null || { echo 'ERROR: dpkg-deb is required.' >&2; exit 1; }
command -v pkg-config >/dev/null || { echo 'ERROR: pkg-config is required.' >&2; exit 1; }
pkg-config --exists gtk4 || {
  echo 'ERROR: GTK 4 development files are missing.' >&2
  echo 'On Ubuntu/Debian: sudo apt update && sudo apt install -y build-essential pkg-config libgtk-4-dev gobject-introspection' >&2
  exit 1
}

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

printf '\n==> Staging %s\n' "$PKG_NAME"
rm -rf "$PKG_DIR"
install -d "$PKG_DIR/DEBIAN"
install -d "$PKG_DIR/usr/bin"
install -d "$PKG_DIR/usr/lib/mysterium-vpn"
install -d "$PKG_DIR/usr/lib/systemd/system"
install -d "$PKG_DIR/usr/share/applications"
install -d "$PKG_DIR/usr/share/doc/mysterium-vpn"

install -m 0755 "$BIN_DIR/mystd" "$PKG_DIR/usr/lib/mysterium-vpn/mystd"
install -m 0755 "$BIN_DIR/myst-gui" "$PKG_DIR/usr/bin/myst-gui"
install -m 0755 "$ROOT_DIR/packaging/mystd-launch" "$PKG_DIR/usr/lib/mysterium-vpn/mystd-launch"
install -m 0755 "$ROOT_DIR/packaging/desktop-group" "$PKG_DIR/usr/lib/mysterium-vpn/desktop-group"
install -m 0644 "$ROOT_DIR/packaging/mystd.service" "$PKG_DIR/usr/lib/systemd/system/mystd.service"
install -m 0644 "$ROOT_DIR/packaging/mysterium-vpn.desktop" "$PKG_DIR/usr/share/applications/mysterium-vpn.desktop"

for size in 48 64 128 256 512; do
  icon="$ROOT_DIR/packaging/icons/mysterium-vpn-${size}.png"
  [[ -f "$icon" ]] || { echo "ERROR: missing icon $icon" >&2; exit 1; }
  install -d "$PKG_DIR/usr/share/icons/hicolor/${size}x${size}/apps"
  install -m 0644 "$icon" "$PKG_DIR/usr/share/icons/hicolor/${size}x${size}/apps/mysterium-vpn.png"
done

install -m 0644 "$ROOT_DIR/packaging/README-INSTALL.txt" "$PKG_DIR/usr/share/doc/mysterium-vpn/README-INSTALL.txt"
install -m 0644 "$ROOT_DIR/packaging/copyright" "$PKG_DIR/usr/share/doc/mysterium-vpn/copyright"

install -m 0755 "$ROOT_DIR/packaging/deb/postinst" "$PKG_DIR/DEBIAN/postinst"
install -m 0755 "$ROOT_DIR/packaging/deb/prerm" "$PKG_DIR/DEBIAN/prerm"
install -m 0755 "$ROOT_DIR/packaging/deb/postrm" "$PKG_DIR/DEBIAN/postrm"

# Installed-Size is in KiB and is what the package manager uses to predict
# disk use. It has to be written before the control file is finalised.
SIZE=$(du -sk "$PKG_DIR" | cut -f1)
sed -e "s/@VERSION@/$VERSION/g" -e "s/@ARCH@/$ARCH/g" -e "s/@SIZE@/$SIZE/g" \
  "$ROOT_DIR/packaging/deb/control.in" > "$PKG_DIR/DEBIAN/control"

# md5sums are optional but cheap, and they let dpkg verify unpacked files.
(
  cd "$PKG_DIR"
  find . -type f ! -path './DEBIAN/*' -printf '%P\0' |
    LC_ALL=C sort -z |
    xargs -0 md5sum > DEBIAN/md5sums
)

printf '\n==> Building the package\n'
mkdir -p "$DIST"
rm -f "$DIST/${PKG_NAME}.deb"
dpkg-deb --build --root-owner-group "$PKG_DIR" "$DIST/${PKG_NAME}.deb"

printf '\nCreated:\n  %s\n' "$DIST/${PKG_NAME}.deb"
printf '\nCopy that file to the other Ubuntu PC and double-click it.\n'
