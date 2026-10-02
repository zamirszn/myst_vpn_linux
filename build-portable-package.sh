#!/usr/bin/env bash
set -euo pipefail
ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DIST="$ROOT_DIR/dist"
STAGE="$DIST/mysterium-linux-amd64"

command -v go >/dev/null || { echo 'ERROR: Go is required to build the package.' >&2; exit 1; }
command -v pkg-config >/dev/null || { echo 'ERROR: pkg-config is required.' >&2; exit 1; }
pkg-config --exists gtk4 || {
  echo 'ERROR: GTK 4 development files are missing.' >&2
  echo 'On Ubuntu/Debian install them with: sudo apt update && sudo apt install -y build-essential pkg-config libgtk-4-dev policykit-1 zip' >&2
  exit 1
}

rm -rf "$STAGE"
mkdir -p "$STAGE"
export CGO_ENABLED=1

printf '\n==> Building daemon from this source tree\n'
(cd "$ROOT_DIR" && go build -buildvcs=false -trimpath -o "$STAGE/mystd" ./cmd/mystd)
printf '\n==> Building GTK GUI from the matching desktop module\n'
(cd "$ROOT_DIR/desktop" && go build -buildvcs=false -trimpath -o "$STAGE/myst-gui" ./cmd/myst-gui)

install -m 0644 "$ROOT_DIR/packaging/mystd.service" "$STAGE/mystd.service"
install -m 0644 "$ROOT_DIR/packaging/mysterium-vpn.desktop" "$STAGE/mysterium-vpn.desktop"
install -m 0755 "$ROOT_DIR/packaging/install.sh" "$STAGE/install.sh"
install -m 0755 "$ROOT_DIR/packaging/uninstall.sh" "$STAGE/uninstall.sh"
cat > "$STAGE/README-INSTALL.txt" <<'DOC'
MYSTERIUM VPN - PORTABLE UBUNTU PACKAGE

This package contains both the GUI (myst-gui) and the matching daemon (mystd)
built from the same source tree. It is an amd64 Linux package, not a universal
Linux binary. Use on Ubuntu/Debian x86_64 with GTK 4 and systemd.

INSTALL
  1. Extract the archive.
  2. Open a terminal in the extracted folder.
  3. Run: sudo ./install.sh
  4. Log out of the desktop session and log back in (the installer adds your
     user to the 'myst' group so the GUI can access the daemon socket).
  5. Launch Mysterium VPN from the application menu, or run myst-gui.

The installer enables and starts mystd.service and installs both executables.
The token/account state is stored by the daemon under /var/lib/mystd and is
not included in this package. Each computer logs in separately via magic link.

UNINSTALL
  sudo ./uninstall.sh

Uninstall preserves /var/lib/mystd so credentials are not silently deleted.
To intentionally erase saved credentials, first log out in the app, then
review /var/lib/mystd before deleting it manually.

BUILD PROVENANCE
This package was built from one source tree so the GUI and daemon IPC protocol
match. If installation fails, inspect: sudo journalctl -u mystd -n 100 --no-pager
DOC

# Verify both artifacts are ELF executables before packaging.
file "$STAGE/mystd" "$STAGE/myst-gui"
chmod 0755 "$STAGE/mystd" "$STAGE/myst-gui"
mkdir -p "$DIST"
rm -f "$DIST/mysterium-linux-amd64.tar.gz" "$DIST/mysterium-linux-amd64.zip"
tar -C "$DIST" -czf "$DIST/mysterium-linux-amd64.tar.gz" "mysterium-linux-amd64"
if command -v zip >/dev/null 2>&1; then
  (cd "$DIST" && zip -qr mysterium-linux-amd64.zip mysterium-linux-amd64)
else
  echo 'zip utility is unavailable; created the .tar.gz package only.' >&2
fi
printf '\nPackage created:\n  %s\n' "$DIST/mysterium-linux-amd64.tar.gz"
[[ ! -f "$DIST/mysterium-linux-amd64.zip" ]] || printf '  %s\n' "$DIST/mysterium-linux-amd64.zip"
printf '\nTransfer the generated archive to the other Ubuntu PC, extract it, then run sudo ./install.sh.\n'
