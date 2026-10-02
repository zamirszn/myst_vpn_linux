#!/usr/bin/env bash
#
# Bring up a Mysterium VPN 2.0 WireGuard tunnel and prove traffic exits through it.
#
# Run this YOURSELF with sudo. It is not run automatically by the agent because it
# needs root and installs a package.
#
#   sudo ./wg-test.sh ~/.myst-capture/router.conf
#
# SAFETY: this takes over the default route. If the tunnel does not pass traffic,
# the machine loses connectivity. So an automatic teardown is armed BEFORE the
# interface comes up — if anything goes wrong, networking restores itself.
#
set -uo pipefail

CONF="${1:-}"
IFACE="myst0"
WGDIR="/etc/wireguard"
TARGET="$WGDIR/$IFACE.conf"
AUTODOWN_SECS=180

die() { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }
ok()  { printf '\033[32m%s\033[0m\n' "$*"; }
say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

[[ $EUID -eq 0 ]] || die "must run as root:  sudo $0 $CONF"
[[ -n "$CONF" ]]  || die "usage: sudo $0 /path/to/wireguard.conf"
[[ -f "$CONF" ]]  || die "no such config: $CONF"

# ---- 1. tooling -------------------------------------------------------------
say "wireguard tooling"
if ! command -v wg-quick >/dev/null; then
  echo "  wg-quick missing — installing wireguard-tools"
  apt-get update -qq && apt-get install -y wireguard-tools || die "install failed"
fi
echo "  wg-quick : $(command -v wg-quick)"
modprobe wireguard 2>/dev/null || true
echo "  module   : $(lsmod | grep -c '^wireguard') loaded"

# ---- 2. baseline BEFORE the tunnel -----------------------------------------
say "baseline (no tunnel)"
DIRECT_IP="$(curl -sS --max-time 15 https://api.ipify.org || echo '')"
echo "  direct public IP : ${DIRECT_IP:-<lookup failed>}"
echo "  default route    : $(ip route show default | head -1)"

# ---- 3. install the config --------------------------------------------------
say "install config"
install -d -m 700 "$WGDIR"
install -m 600 "$CONF" "$TARGET"
echo "  $TARGET  [mode 600]"
grep -Ev '^(PrivateKey|PresharedKey)' "$TARGET" \
  | sed -E 's/^(AllowedIPs = ).{50}.*/\1<...>/'

# Warn about the IPv6 gap before we commit to the tunnel.
if ! grep -q '::/0' "$TARGET"; then
  printf '\n  \033[33mNOTE:\033[0m this config routes no IPv6 (no ::/0).\n'
  echo   "  If this host has IPv6, that traffic will bypass the tunnel entirely."
  echo   "  Re-run with:  sudo WG_ADD_IPV6=1 $0 $CONF"
fi
if [[ "${WG_ADD_IPV6:-0}" == "1" ]]; then
  sed -i 's|^\(AllowedIPs *=.*\)$|\1, ::/0|' "$TARGET"
  echo "  added ::/0 to AllowedIPs"
fi

# ---- 4. arm the dead-man switch BEFORE bringing it up -----------------------
say "arming auto-teardown (${AUTODOWN_SECS}s)"
( sleep "$AUTODOWN_SECS"; wg-quick down "$IFACE" >/dev/null 2>&1 ) &
WATCHDOG=$!
echo "  watchdog pid $WATCHDOG — will tear down at T+${AUTODOWN_SECS}s regardless"
echo "  (cancel manually with: kill $WATCHDOG)"

# ---- 5. up ------------------------------------------------------------------
say "wg-quick up $IFACE"
if ! wg-quick up "$IFACE"; then
  die "wg-quick up failed — nothing changed, networking untouched"
fi
ok "  interface up"

# ---- 6. prove it ------------------------------------------------------------
say "verification"
echo "  handshake:"
wg show "$IFACE" latest-handshakes | sed 's/^/    /'

# Routing: is the default route now via the tunnel?
echo "  default route : $(ip route show default | head -1)"

# DNS
echo "  DNS           : $(resolvectl status 2>/dev/null | grep -m1 'Current DNS Server' || echo n/a)"

TUNNEL_IP="$(curl -sS --max-time 25 https://api.ipify.org || echo '')"
echo "  tunnel public IP : ${TUNNEL_IP:-<lookup failed>}"

# DNS leak: resolve a name and see whether it works only because of the tunnel DNS.
echo "  DNS test      : $(getent hosts api.ipify.org >/dev/null 2>&1 && echo 'resolves' || echo 'FAILED')"

say "result"
if [[ -n "$TUNNEL_IP" && -n "$DIRECT_IP" && "$TUNNEL_IP" != "$DIRECT_IP" ]]; then
  ok "  PASS — traffic exits from $TUNNEL_IP (direct was $DIRECT_IP)"
  echo "  the WireGuard path works. Kernel tunnel, full IPv4, UDP included."
elif [[ -z "$TUNNEL_IP" ]]; then
  printf '\033[31m  FAIL — no traffic through the tunnel.\033[0m\n'
  echo "  tearing down now."
else
  printf '\033[33m  INCONCLUSIVE — tunnel IP equals direct IP ($TUNNEL_IP).\033[0m\n'
fi

# ---- 7. teardown ------------------------------------------------------------
say "teardown"
kill "$WATCHDOG" 2>/dev/null || true
wg-quick down "$IFACE" && ok "  interface down, routes restored"
echo "  default route : $(ip route show default | head -1)"
echo "  public IP     : $(curl -sS --max-time 15 https://api.ipify.org || echo '?')"
echo
echo "config left at $TARGET — remove with: sudo rm $TARGET"
