#!/usr/bin/env bash
# Build everything that goes on the other machine: the double-click .deb, and a
# portable archive for systems where the package manager will not be used.
set -euo pipefail
ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DIST="$ROOT_DIR/dist"
VERSION="${VERSION:-1.0.0}"
ARCH="${ARCH:-$(dpkg --print-architecture)}"
STAGE="$DIST/mysterium-vpn-linux-${ARCH}"

# Builds dist/bin/{mystd,myst-gui} and dist/mysterium-vpn_<version>_<arch>.deb.
VERSION="$VERSION" ARCH="$ARCH" "$ROOT_DIR/build-deb.sh"

rm -rf "$STAGE"
mkdir -p "$STAGE/icons"
install -m 0755 "$DIST/bin/mystd" "$STAGE/mystd"
install -m 0755 "$DIST/bin/myst-gui" "$STAGE/myst-gui"
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

# The recommended, terminal-free installer travels in the same archive.
cp "$DIST/mysterium-vpn_${VERSION}_${ARCH}.deb" "$STAGE/"

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
printf '  %s\n' "$DIST/mysterium-vpn_${VERSION}_${ARCH}.deb"
printf '  %s\n' "$DIST/mysterium-vpn-linux-${ARCH}.tar.gz"
[[ ! -f "$DIST/mysterium-vpn-linux-${ARCH}.zip" ]] || printf '  %s\n' "$DIST/mysterium-vpn-linux-${ARCH}.zip"
printf '\nSend the .deb to the other Ubuntu PC and double-click it.\n'
