#!/usr/bin/env bash
# Installer for the release archive.
#
# This is the supported install path: the release ships this script and no
# .deb, so a systemd unit can never be shadowed by the other install method.
# It needs a terminal once; the daemon's socket is given the desktop user's
# primary group, which they already belong to, so no log-out is needed.
set -euo pipefail
HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

if [[ "${EUID}" -ne 0 ]]; then
  echo "Run this installer with sudo: sudo ./install.sh" >&2
  exit 1
fi

# The .deb installs the same service under /usr/lib/systemd/system; this script
# installs it under /etc, where it shadows the package's copy. Both cannot be
# in charge at once, and when both are, package upgrades silently stop taking
# effect. Refuse to mix the two install methods.
if command -v dpkg-query >/dev/null 2>&1 &&
   [[ "$(dpkg-query -W -f='${db:Status-Status}' mysterium-vpn 2>/dev/null)" == installed ]]; then
  echo "The mysterium-vpn package is installed; use it instead of this archive." >&2
  echo "To switch to the portable install, remove the package first:" >&2
  echo "  sudo apt remove mysterium-vpn" >&2
  exit 1
fi
for f in "$HERE/mystd" "$HERE/myst-gui" "$HERE/mystd-launch" "$HERE/desktop-group" \
         "$HERE/mystd.service" "$HERE/mysterium-vpn.desktop"; do
  [[ -f "$f" ]] || { echo "Missing package file: $f" >&2; exit 1; }
done

if ! command -v systemctl >/dev/null 2>&1; then
  echo "systemd is required for the supplied daemon service." >&2
  exit 1
fi
if ! command -v pkexec >/dev/null 2>&1; then
  echo "Warning: pkexec was not found. The GUI can still use the installed system daemon." >&2
fi

# Work out which group will own the control socket. On a normal Ubuntu desktop
# this is the primary group of the person who ran sudo, so they are already a
# member and no log-out is needed.
SOCKET_GROUP="$(SUDO_USER="${SUDO_USER:-}" "$HERE/desktop-group" 2>/dev/null || true)"
if [[ -z "$SOCKET_GROUP" ]]; then
  echo "Warning: no desktop user was found. The service will resolve the group again when it starts." >&2
fi

# Back up existing binaries before replacing them, so a failed service restart
# can be rolled back from /usr/local/bin/*.pre-myst-package.
for bin in mystd myst-gui; do
  if [[ -x "/usr/local/bin/$bin" ]]; then
    cp -a "/usr/local/bin/$bin" "/usr/local/bin/$bin.pre-myst-package"
  fi
done

install -d -m 0755 /usr/local/lib/mysterium-vpn
install -m 0755 "$HERE/mystd" /usr/local/bin/mystd
install -m 0755 "$HERE/myst-gui" /usr/local/bin/myst-gui
install -m 0755 "$HERE/mystd-launch" /usr/local/lib/mysterium-vpn/mystd-launch
install -m 0755 "$HERE/desktop-group" /usr/local/lib/mysterium-vpn/desktop-group

# The unit in the archive points at the package location; the portable copy
# runs the launcher from /usr/local. The launcher itself finds the daemon there.
#
# A unit left behind by an earlier .deb lives in /usr/lib/systemd/system and
# would be shadowed by the /etc copy below, leaving two units on disk with
# different ExecStart lines. Move it aside so there is exactly one.
if [[ -e /usr/lib/systemd/system/mystd.service ]] &&
   ! dpkg-query -S /usr/lib/systemd/system/mystd.service >/dev/null 2>&1; then
  mv /usr/lib/systemd/system/mystd.service \
     /usr/lib/systemd/system/mystd.service.pre-myst-package
fi
sed 's#/usr/lib/mysterium-vpn/mystd-launch#/usr/local/lib/mysterium-vpn/mystd-launch#' \
  "$HERE/mystd.service" > /etc/systemd/system/mystd.service
chmod 0644 /etc/systemd/system/mystd.service

install -d -m 0755 /usr/local/share/applications
sed 's#/usr/bin/myst-gui#/usr/local/bin/myst-gui#' \
  "$HERE/mysterium-vpn.desktop" > /usr/local/share/applications/mysterium-vpn.desktop
chmod 0644 /usr/local/share/applications/mysterium-vpn.desktop

# Icons, if the archive carries them. They are optional: the app also embeds
# its header mark, so this only affects the application-menu icon.
if [[ -d "$HERE/icons" ]]; then
  for size in 48 64 128 256 512; do
    src="$HERE/icons/mysterium-vpn-${size}.png"
    [[ -f "$src" ]] || continue
    install -d -m 0755 "/usr/local/share/icons/hicolor/${size}x${size}/apps"
    install -m 0644 "$src" "/usr/local/share/icons/hicolor/${size}x${size}/apps/mysterium-vpn.png"
  done
else
  install -d -m 0755 /usr/local/share/icons/hicolor/256x256/apps
  install -m 0644 "$HERE/mysterium-vpn.png" \
    /usr/local/share/icons/hicolor/256x256/apps/mysterium-vpn.png 2>/dev/null || true
fi

systemctl daemon-reload
systemctl enable mystd >/dev/null 2>&1 || true
if ! systemctl restart mystd; then
  echo "ERROR: mystd failed to start. Logs follow:" >&2
  journalctl -u mystd -n 60 --no-pager >&2 || true
  echo "Previous binaries, if any, are saved as *.pre-myst-package in /usr/local/bin." >&2
  exit 1
fi

if command -v gtk-update-icon-cache >/dev/null 2>&1; then
  gtk-update-icon-cache -q -t /usr/local/share/icons/hicolor 2>/dev/null || true
fi
if command -v update-desktop-database >/dev/null 2>&1; then
  update-desktop-database -q /usr/local/share/applications 2>/dev/null || true
fi

printf '\nInstalled Mysterium VPN.\n'
if [[ -n "$SOCKET_GROUP" ]]; then
  printf 'The control socket is available to group %s — you are already in it, so no log-out is needed.\n' "$SOCKET_GROUP"
fi
printf 'Daemon status:\n'
systemctl --no-pager --full status mystd || true
printf '\nLaunch "Mysterium VPN" from the application menu, or run /usr/local/bin/myst-gui.\n'
