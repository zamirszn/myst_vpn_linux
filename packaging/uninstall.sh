#!/usr/bin/env bash
set -euo pipefail
if [[ "${EUID}" -ne 0 ]]; then
  echo "Run with sudo: sudo ./uninstall.sh" >&2
  exit 1
fi
systemctl disable --now mystd 2>/dev/null || true
rm -f /etc/systemd/system/mystd.service
rm -f /usr/local/share/applications/mysterium-vpn.desktop
rm -f /usr/local/bin/mystd /usr/local/bin/myst-gui
systemctl daemon-reload
printf 'Removed Mysterium VPN executables and service.\n'
printf 'Account data was preserved in /var/lib/mystd. Remove it manually only if you intentionally want to delete saved credentials.\n'
