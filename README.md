# Mysterium VPN for Linux

A Linux client for **Mysterium VPN 2.0** — the subscription product, not the
legacy MYST/dVPN network. A real WireGuard tunnel, a command line, a tray
applet, a kill switch, and a monitor that repairs the tunnel when it dies.

Works on Ubuntu 24.04 against a live account. Version 0.1.0.

---

## Install

```sh
sudo apt install ./myst-client_0.1.0_amd64.deb
```

Then **log out and back in once**. The installer adds you to the `myst` group,
which is what lets your user talk to the daemon; until you log back in, every
command fails with `permission denied` against a daemon that looks perfectly
healthy.

Not built yet — the `.deb` is part of the remaining work. Until then, see
[Building from source](#building-from-source).

---

## How to use

### Log in once

```sh
myst login --from ~/token.json
```

Imports a captured token so the client can authenticate without a browser. See
[Logging in](#logging-in) for where to get one.

### Connect

```sh
myst connect                  # fastest region
myst connect --country gb     # a specific one
```

`myst connect` returns once the tunnel is up and the handshake has completed, so
a successful exit means the tunnel really is carrying traffic.

### Check on it

```sh
myst status
```

```
connected
  up since    2m14s
  exit ip     176.241.28.114 (gb, gloucester)
  interface   myst0
  ipv6        routed
  wake lock   held
  kill switch active
```

Add `--json` for the raw status, if you are scripting against it.

### Disconnect

```sh
myst disconnect
```

### Or use the window

```sh
myst-gui
```

A window with the state, a Connect/Disconnect button, a region picker, the exit
IP with a Copy button, and indicators for IPv6 and the kill switch. It polls the
daemon every two seconds, so a change made anywhere else — from the CLI, from the
tray, or by the health monitor repairing a dropped tunnel — shows up there
within a moment.

The region picker opens on **Fastest**, which asks for no country in particular
and lets the API pick a free node. Choose a country and it will use that one.

Like the tray, it holds no credentials and no privileges. Every action is a
request over the control socket, so it runs as your user while the only root
process remains the daemon.

### Or use the tray

The tray applet is a shield icon that shows the state at a glance:

| Icon | Meaning |
|---|---|
| green, keyhole | connected |
| amber | connecting |
| grey outline | not connected |
| red, slashed | the last operation failed |
| grey, slashed | the daemon is not answering |

**Left click** connects or disconnects. **Right click** opens a menu with the
state, Connect/Disconnect, a region submenu, Copy exit IP, and Quit.

To start it with your session, see [The tray applet](#the-tray-applet).

---

## What it does for you

**The tunnel is real.** Not a proxy — kernel WireGuard, the same thing the
official apps use, so UDP works and QUIC, VoIP and games behave.

**The kill switch.** Neither WireGuard nor `wg-quick` blocks traffic when a
tunnel drops; without one, a dropped tunnel means traffic quietly continues in
the clear from the same machine. While connected, the daemon installs an
nftables filter that permits only the tunnel, the WireGuard handshake, and DHCP.
It goes in *before* the interface comes up and comes out *after* it goes down, so
there is never a window in which traffic can escape.

Verified on a live kernel: with the filter armed, a ping to the LAN gateway that
succeeds when disconnected returned **100% loss**, while a public-IP lookup
*through the tunnel* still answered — proving the machine had working internet
and simply could not send anything outside the tunnel. The full test is in
[the kill switch](#the-kill-switch).

**The health monitor.** A tunnel can die without the daemon being told — the wifi
changes, the laptop resumes, the node restarts. A WireGuard interface is
*virtual*, so it stays `UP` while carrying nothing, and the daemon cannot tell a
working tunnel from a dead one by looking at the interface. Left alone, the
daemon went on reporting `connected` with the kill switch armed, which drops
everything that is not the tunnel — so the machine lost its network entirely and
looked like a broken wifi card rather than a dropped VPN.

`mystd` now watches a live tunnel and repairs it. Three signals, cheapest first:

| Signal | Catches | Noticed within |
| --- | --- | --- |
| Default route changed | the machine moving between networks | one poll |
| Interface gone | `wg-quick down`, module unload, killed process | one poll |
| No handshake for 90s | a node that stopped answering, resume from suspend | ~3 keepalive intervals |

The state becomes `reconnecting`, and the daemon rebuilds the tunnel for **the
same region** — a recovery must not quietly move you to another country.

**And if it cannot, you get your network back.** Recovery is bounded (3 attempts
or two minutes). When it runs out, the daemon tears everything down — interface,
kill switch, wake lock — and reports an error saying plainly that the tunnel is
gone and the network is working again. That fall-back is the point: a dead tunnel
can cost you a few seconds of connectivity, but it can no longer cost you your
connectivity indefinitely.

**Your laptop cannot suspend out from under it.** Suspend silently kills a
WireGuard tunnel, and the interface survives the sleep, so nothing looks wrong on
resume — the handshake is simply stale. While connected, the daemon holds a
logind inhibitor so the machine cannot suspend. It is released on disconnect, and
automatically if the daemon dies.

---

## Commands

| Command | What it does |
|---|---|
| `myst login --from <token.json>` | import a captured token (`-` reads stdin) |
| `myst logout` | forget the stored token |
| `myst connect [--country XX] [--ip-type hosting\|residential]` | bring the tunnel up |
| `myst disconnect` | tear it down |
| `myst status [--json]` | state, region, exit IP, kill switch, wake lock |

With no `--country`, the API picks the fastest region.

`myst status` answers immediately, even while a connect or a reconnect is
grinding through its attempts — that is the moment you most want an answer, so it
is published under a lock that is never held across a network call.

---

## Troubleshooting

**`permission denied` talking to the daemon.** Your session does not carry the
`myst` group yet. Log out and back in. Check with `id -nG`.

**The daemon will not start after you edited the unit.** Almost always a stale
binary. The unit file and the binary are installed independently, so it is
possible to edit `ExecStart` to pass a flag the installed binary predates. Go's
`flag` package rejects an unknown flag with exit status 2, and the unit then
crash-loops every `RestartSec`; `systemctl status` reports `activating
(auto-restart)`, which reads like a startup failure rather than a stale binary.
Check with:

```sh
strings /usr/local/bin/mystd | grep <flag>
```

**You are offline and the daemon will not answer.** The kill switch is still
armed with nothing behind it. This is the failure the monitor exists to prevent,
but if it happens, the escape hatch is one command:

```sh
sudo nft delete table inet myst_killswitch
```

It is one self-contained table. Nothing you already have — ufw, Docker,
hand-written rules — is read or modified, and removing that table restores
networking immediately.

---

## Limitations

**The kill switch covers host traffic only.** It filters the output hook, so
forwarded traffic — containers, VMs on a bridge — is not filtered. With Docker
running, container traffic can still leave in the clear when the tunnel drops.
The filter is also egress-only: unsolicited inbound is not filtered, which is the
same posture as having no kill switch, since nothing but the handshake is
listening.

**The kill switch has never been exercised by a real drop.** Its blocking is
verified, but every test so far has ended with a deliberate `myst disconnect`. A
tunnel that dies *on its own* is a different path — the one the health monitor
exists to handle — and while the monitor is covered by unit tests, it has not yet
been watched through a real drop on a live tunnel.

**IPv6 leaks by default in Mysterium's own config.** It assigns an IPv6 address
but routes no IPv6, so on a host with IPv6 connectivity that traffic bypasses the
tunnel. This client appends `::/0` to close it; `-no-ipv6` turns that off.

**A region can be temporarily unserved.** Ask for a country with no free node
and the API answers with an HTTP 500 — which reads like a server fault but is
not one. The country simply has nothing available at that moment, and the fix is
to pick another. The client says exactly that instead of passing the 500
through, because "api 500 2332: Could not retrieve wireguard connect
configuration" tells you nothing you can act on.

This is why the window's picker defaults to **Fastest** and the CLI takes no
country unless you name one: asking for nothing lets the API choose a node that
is actually free, rather than one you did not mean to choose.

**The tray's region list is curated, not fetched.** The full country list lives
behind an authenticated endpoint, and the token belongs to the daemon — copying
it into an unprivileged desktop applet would undo the point of the split. The
applet offers the countries people actually pick and accepts any code passed to
`-country`, so `myst connect --country xx` reaches regions the menu does not
list.

**Not yet settled:** whether two WireGuard connections can handshake
simultaneously on one account, a terms-of-service review for third-party clients,
and the magic-link login flow (today you import a captured token instead).

**Client identity.** Testing used `client_id=extension`, which labels a Linux
client as the Chrome extension. Acceptable for personal testing on your own
account; a question to resolve with Mysterium before distributing.

---

## Logging in

The client needs a bearer token. There is no browser flow implemented yet, so
import one captured from the dashboard's own traffic:

```sh
myst login --from ~/token.json
```

Credentials land in `/var/lib/mystd/token.json` at 0600. Tokens last about 30
days and carry a refresh token, so the client renews itself indefinitely after
that — you should not have to do this again.

The headless magic-link flow is designed but not built; see
[docs/api-notes.md](docs/api-notes.md) for the API contract.

---

## Building from source

The daemon and CLI have **no external dependencies** — Go's standard library
only, and they build with Go 1.22. The `desktop/` module is the exception, and
it is a separate module precisely so that stays true: a tray icon needs a D-Bus
library, and a window needs GTK.

Building the window also needs the GTK4 development headers, and the current
gotk4 release requires **Go 1.24 or newer**. The tray alone builds without
either — it is pure Go and speaks D-Bus directly.

```sh
sudo apt install libgtk-4-dev     # only needed for the window

# daemon and CLI
go build -o mystd ./cmd/mystd
go build -o myst  ./cmd/myst

# desktop front ends — the tray applet and the GTK window
(cd desktop && go build -o myst-tray ./cmd/myst-tray)
(cd desktop && go build -o myst-gui  ./cmd/myst-gui)

sudo install -m755 mystd myst myst-tray myst-gui /usr/local/bin/
sudo install -m644 deploy/mystd.service /etc/systemd/system/
sudo install -m644 desktop/myst-gui.desktop /usr/share/applications/

sudo groupadd --system myst
sudo usermod -aG myst "$USER"
sudo sed -i 's|^ExecStart=/usr/local/bin/mystd$|ExecStart=/usr/local/bin/mystd -socket-group myst|' \
    /etc/systemd/system/mystd.service
sudo systemctl daemon-reload
sudo systemctl enable --now mystd
```

Log out and back in for the group to take effect, then `myst connect`.

The first build of the window takes a while — gotk4 is a large generated cgo
binding and it is compiled from scratch once. Later builds are cached and fast.

### Layout

```
cmd/mystd        privileged daemon: owns the tunnel, serves the control socket
cmd/myst         unprivileged CLI: connect / disconnect / status / login / logout
desktop/         the front ends: tray applet and GTK window, in their own module
  cmd/myst-tray/   the tray applet
  cmd/myst-gui/    the GTK4 window
  internal/        the control-socket client and region list they share
internal/        the packages behind the daemon and CLI
deploy/          systemd unit
docs/            API notes from the reverse-engineering work
```

| Package | Responsibility |
|---|---|
| `internal/wg` | keypair generation, config assembly, `wg-quick` control |
| `internal/api` | the consumer API client |
| `internal/auth` | token storage and refresh |
| `internal/daemon` | the connect/disconnect/recover state machine |
| `internal/ipc` | unix-socket control protocol |
| `internal/killswitch` | nftables egress filter, armed while connected |
| `internal/wakelock` | suspend inhibitor, held while connected |

---

## The control socket

The daemon runs as root, so a unix socket it creates is `root:root`. At mode
0660 that means **only root can talk to it** — the CLI and the tray would both
fail with `permission denied` against a daemon that looks perfectly healthy.

`mystd -socket-group <group>` hands the socket to a group instead. The mode is
deliberately not widened to 0666: anything that can open this socket can make a
root process bring a tunnel up or down, so access is granted by group membership
rather than to every account on the box. Naming a group that does not exist is a
startup failure, not a silent fallback to root-only — a typo should not look like
a working daemon.

`mystd` logs which of the two it is doing at startup.

---

## The tray applet

```sh
(cd desktop && go build -o myst-tray ./cmd/myst-tray)
sudo install -m755 myst-tray /usr/local/bin/

mkdir -p ~/.config/autostart
cat > ~/.config/autostart/myst-tray.desktop <<'EOF'
[Desktop Entry]
Type=Application
Name=Mysterium VPN
Comment=System tray applet for the Mysterium VPN daemon
Exec=/usr/local/bin/myst-tray
Icon=network-vpn
Terminal=false
Categories=Network;
StartupNotify=false
X-GNOME-Autostart-enabled=true
EOF
```

GNOME needs the AppIndicator extension for any of this to show up. If it is
missing the applet says so and exits rather than running invisibly:

```sh
gnome-extensions enable ubuntu-appindicators@ubuntu.com
```

It speaks StatusNotifierItem and `com.canonical.dbusmenu` directly — no cgo, no
GTK, and no `libayatana-appindicator` headers to install.

---

## The kill switch

While connected, the daemon installs this in its own nftables table:

```nft
table inet myst_killswitch {
	chain output {
		type filter hook output priority 0; policy accept;

		oifname "lo" accept
		oifname "myst0" accept

		# the WireGuard handshake, or the tunnel could never come back
		ip daddr 91.98.228.210 udp dport 56666 accept

		udp sport 68 udp dport 67 accept      # DHCP renewal
		udp sport 546 udp dport 547 accept    # DHCPv6
		meta l4proto ipv6-icmp accept         # neighbour discovery

		counter drop
	}
}
```

Four things about this are deliberate:

**It fails closed.** The filter goes in before the interface comes up and comes
out after it goes down, so there is never a window in which traffic could escape.
A test asserts that ordering rather than assuming it.

**No `ct state established,related accept`.** The obvious rule to add, and the
wrong one: connections opened before the filter was armed would keep flowing out
of the physical interface, which is exactly the leak being prevented.

**The endpoint always stays reachable.** A filter that blocked the WireGuard
handshake would stop the tunnel from ever re-establishing, leaving the machine
offline. The daemon refuses to arm with no endpoint at all, and resolves a
hostname endpoint *before* arming, so the rule is right either way.

**It is one table** — see the escape hatch under
[Troubleshooting](#troubleshooting).

`mystd` clears a stale filter on startup, so a daemon killed mid-connect cannot
leave you without a network. Disable it with `mystd -no-killswitch`.

### The verification

Tested on a live kernel (Ubuntu 6.11). The destination and the route to it were
held constant across every phase — the LAN gateway at `10.0.0.1`, which leaves
via the local subnet route on the physical device and so never enters the tunnel.
The filter is therefore the only thing in a position to drop it:

| Phase | Route to `10.0.0.1` | Public IP | Ping to gateway |
| --- | --- | --- | --- |
| disconnected | `dev wlo1` | `<home ip>` | 0% loss |
| connected, filter armed | `dev wlo1` | `176.241.28.114` (HU) | **100% loss** |
| disconnected | `dev wlo1` | `<home ip>` | 0% loss |

The middle row is the one that matters, and the control alongside it is what
makes it unambiguous: while the gateway was unreachable, a public-IP lookup
*through the tunnel* still answered. The machine had working internet — it simply
could not send anything outside the tunnel. That is precisely the leak this
exists to prevent, now observed rather than assumed.

The endpoint exemption is load-bearing in that result: the handshake that brought
the tunnel up had to traverse the armed filter, because the filter goes in first.

---

## Flags

| Flag | Effect |
|---|---|
| `mystd -socket-group <group>` | hand the control socket to a group |
| `mystd -no-killswitch` | do not arm the egress filter |
| `mystd -no-wakelock` | do not inhibit suspend |
| `mystd -no-monitor` | do not watch a live tunnel for drops |
| `mystd -no-ipv6` | do not route IPv6 through the tunnel |

The monitor's checks are two local commands every ten seconds, so there is little
reason to turn it off.

---

## Security

No credentials are committed here, and none should be. Tokens, WireGuard configs,
proxy credentials, the browser profile, and capture logs live outside the repo
under `~/.myst-capture/` and `~/.myst-env` (mode 0600), and `.gitignore` covers
them defensively.

The private key never leaves the machine: it is generated locally, only the public
key is submitted to the API, and the config holding it is written 0600 and
removed on disconnect.

**License.** The official Dart client `mysterium-vpn-api-client-dart` (package
`vpn_api`) has no LICENSE file, and is not published to pub.dev, so it would need
a git dependency. Clarify before depending on it in anything you distribute.
Nothing in this repo depends on it.

**Terms of service.** Check them before shipping a third-party client.

---

## The API

The API is not publicly documented. Everything known about it — the endpoints,
the auth flows, the IPv6 gap, the proxy port that lies — is written up in
[docs/api-notes.md](docs/api-notes.md), along with the probe scripts that
established it. Nothing there is needed to install or use the client.
