#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

if [[ "${EUID}" -ne 0 ]]; then
  echo "Run this installer with sudo: sudo ./install.sh" >&2
  exit 1
fi
if [[ -z "${SUDO_USER:-}" || "${SUDO_USER}" == "root" ]]; then
  echo "Run this as a normal desktop user with sudo, so the installer can grant socket access." >&2
  echo "Example: sudo -u YOUR_USER sudo ./install.sh" >&2
  exit 1
fi
for f in "$HERE/mystd" "$HERE/myst-gui" "$HERE/mystd.service" "$HERE/mysterium-vpn.desktop"; do
  [[ -f "$f" ]] || { echo "Missing package file: $f" >&2; exit 1; }
done

if ! command -v systemctl >/dev/null 2>&1; then
  echo "systemd is required for the supplied daemon service." >&2
  exit 1
fi
if ! command -v pkexec >/dev/null 2>&1; then
  echo "Warning: pkexec was not found. The GUI can still use the installed system daemon." >&2
fi

# Back up existing binaries before replacing them, so a failed service restart
# can be rolled back by the user from /usr/local/bin/*.pre-myst-package.
for bin in mystd myst-gui; do
  if [[ -x "/usr/local/bin/$bin" ]]; then
    cp -a "/usr/local/bin/$bin" "/usr/local/bin/$bin.pre-myst-package"
  fi
done

getent group myst >/dev/null || groupadd --system myst
usermod -aG myst "$SUDO_USER"
install -m 0755 "$HERE/mystd" /usr/local/bin/mystd
install -m 0755 "$HERE/myst-gui" /usr/local/bin/myst-gui
install -m 0644 "$HERE/mystd.service" /etc/systemd/system/mystd.service
install -D -m 0644 "$HERE/mysterium-vpn.desktop" /usr/local/share/applications/mysterium-vpn.desktop

systemctl daemon-reload
systemctl enable mystd
if ! systemctl restart mystd; then
  echo "ERROR: mystd failed to start. Logs follow:" >&2
  journalctl -u mystd -n 60 --no-pager >&2 || true
  echo "Previous binaries, if any, are saved as *.pre-myst-package in /usr/local/bin." >&2
  exit 1
fi

printf '\nInstalled Mysterium VPN GUI and daemon.\n'
printf 'Daemon status:\n'
systemctl --no-pager --full status mystd || true
printf '\nIMPORTANT: user %s was added to the myst group. Log out of your desktop session and log back in before launching the GUI.\n' "$SUDO_USER"
printf 'Launch from the application menu, or run: /usr/local/bin/myst-gui\n'
