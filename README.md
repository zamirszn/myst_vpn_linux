# Mysterium VPN for Linux

A Linux client for **Mysterium VPN 2.0** — the subscription product, not the
legacy MYST/dVPN network. A real WireGuard tunnel, a command line, a tray
applet, a kill switch, and a monitor that repairs the tunnel when it dies.

Works on Ubuntu 24.04 against a live account. Version 1.0.0.

---

## Install

Build the package, then copy the `.deb` to the Ubuntu PC and **double-click
it**. Ubuntu opens it in App Center / Software Install; approve the password
prompt and the app appears in the application menu. No terminal, no group
changes, and **no log-out**: the service gives its control socket the primary
group of the person using the desktop, which that person already belongs to.

```sh
./build-deb.sh
# dist/mysterium-vpn_1.0.0_amd64.deb
```

`./build-portable-package.sh` additionally produces
`dist/mysterium-vpn-linux-amd64.zip`, which carries the same `.deb` plus a
portable `install.sh` for machines where the package manager will not be used.

The first launch opens a login screen: enter the email address of your
Mysterium VPN account, click Continue, open the email, then click **Paste** and
**Log in** in the app. The service keeps the account; the app never stores the
token itself.

If the app cannot find its service (for example after switching desktop users),
its main screen offers **Start VPN service**, which asks for the password and
restarts the daemon with the correct socket group.

---

## How to use

### Log in once

```sh
myst login
```

Asks for your email, sends you a login link, and waits for you to paste it back.
The window does the same thing as a form: it opens on a login screen whenever
nobody is signed in. See [Logging in](#logging-in).

If you would rather import a token you captured yourself:

```sh
myst login --from ~/token.json
```

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
IP with a Copy button, and indicators for IPv6 and the kill switch. It opens on a
login screen until someone is signed in, and shows who, with a Sign out button,
afterwards. It polls the
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
| `myst login [--email you@example.com]` | log in with an emailed link |
| `myst login --from <token.json>` | import a captured token instead (`-` reads stdin) |
| `myst logout` | close any tunnel and forget the stored token |
| `myst connect [--country XX] [--ip-type hosting\|residential]` | bring the tunnel up |
| `myst disconnect` | tear it down |
| `myst status [--json]` | state, region, exit IP, kill switch, wake lock |

With no `--country`, the API picks the fastest region.

`myst status` answers immediately, even while a connect or a reconnect is
grinding through its attempts — that is the moment you most want an answer, so it
is published under a lock that is never held across a network call.

---

## Troubleshooting

**`permission denied` talking to the daemon.** The socket's group does not
match the user at the desktop. This happens after switching desktop users
without restarting the service. Click **Start VPN service** in the app (or run
`sudo systemctl restart mystd`); the launcher re-reads the active user's primary
group on every start. Check the socket with `ls -l /run/mystd/mystd.sock` and
the launcher's choice with
`sudo /usr/lib/mysterium-vpn/desktop-group`.

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

**"No available instance for filters" does not always mean what it says.** The
API answers a connect it cannot satisfy with HTTP 500 / code 2332, and the text
reads like the country having no free node. It is also what you get for a filter
that matches nothing at all — including a country code in the wrong case, which
is the bug that made every region selection fail while "Fastest" kept working.
The case bug is fixed, and the client now reports this error as "no servers are
available in <country> at the moment" rather than passing the 500 through. But
the message remains ambiguous, so treat it as "this request could not be
satisfied" rather than as a statement about capacity.

A country that genuinely has no node at that moment is still possible. It has
not been observed directly — every 2332 seen so far turned out to be the case
bug — so the wording is a best reading of the API, not a verified one.

This is why the picker defaults to **Fastest** and the CLI takes no country
unless you name one: asking for nothing lets the API choose a node that is
actually free, rather than one you did not mean to choose.

**The country list comes from the account, by way of the daemon.** The full list
lives behind an authenticated endpoint — about a hundred countries against the
eighteen worth hard-coding — and the token belongs to the daemon, so the daemon
serves the list over the control socket. No front end holds a credential, and
all three offer the same countries.

Both the tray and the window start on a short built-in fallback and swap in the
real list as soon as the daemon answers, so either is usable against a daemon
that is not running yet. The fallback is a fallback: it is smaller than the real
list, not a different one.

The full list is not a promise either. The API advertises more countries than it
will necessarily have free at any moment, and a code it does not serve fails at
connect time.

**Not yet settled:** whether two WireGuard connections can handshake
simultaneously on one account, a terms-of-service review for third-party clients,
and the emailed-link login against a live account (see [Logging in](#logging-in)).

**Client identity.** Testing used `client_id=extension`, which labels a Linux
client as the Chrome extension. Acceptable for personal testing on your own
account; a question to resolve with Mysterium before distributing.

---

## Logging in

Logging in works the way the official app does: you give your email address, the
API emails you a link, and the link logs you in.

```sh
myst login
```

```
email: you@example.com
A login link is on its way to you@example.com.

Open the email and paste its link here (or just the code from it).
The link only works for a few minutes.

link: https://…
logged in as you@example.com
```

The window asks the same two questions on its login screen: an address, then the
link. **Log out** is the Sign out button in the window's top corner, or
`myst logout`; either closes a live tunnel first and then deletes the stored
credentials.

**You paste the link rather than clicking it.** The official app is registered to
catch the link and opens itself. A third-party client cannot be, so in the email
right-click the link and choose *Copy link address*, then paste that. If clicking
it lands on a web page, that page's address works too. Either way you can paste
the whole link or only the code in it.

The daemon does the exchange and keeps the secret that makes the link redeemable,
so the window and the CLI stay free of credentials, as before. The consequence is
that restarting `mystd` between asking for the link and pasting it loses the
secret; ask for a new link.

Credentials land in `/var/lib/mystd/token.json` at 0600. Tokens last about 30
days and carry a refresh token, so the client renews itself indefinitely after
that — you should not have to do this again.

**What logging out does not do.** It removes the credentials from this machine and
releases the account's connection. It does not tell the API to revoke the refresh
token, because no endpoint for that has been verified. Treat a token you have
imported or logged in with as live until it expires.

**Not yet confirmed end to end.** The request and response shapes come from the
API's own validation errors and are covered by tests, but the emailed-link flow
has not been run against a live account from this client. Two things worth
watching on the first real login: whether the link carries the code in a form the
parser recognises (it accepts `code`, `authorization_code` and `activation_code`
in the query or fragment, or a bare code), and whether the API creates an account
for an address it has not seen or answers 404 as the CLI assumes.

To import a token you captured another way instead:

```sh
myst login --from ~/token.json
```

`--from -` reads standard input. Before this was added, `myst login` with no
arguments read a token from standard input; it now starts the emailed-link flow,
so piped tokens need the explicit `--from -`.

See [docs/api-notes.md](docs/api-notes.md) for the API contract.

---

## Building from source

### What you need

| | Needed for | Notes |
|---|---|---|
| Go 1.22 or newer | everything | the root module declares 1.22.2 |
| Go 1.24 or newer | the window | the desktop module declares 1.24.0 |
| `libgtk-4-dev` | the window | `sudo apt install libgtk-4-dev` |
| `nftables` | the kill switch | degrade: no filter is installed |
| `wireguard-tools` | the tunnel | **required** — `wg` and `wg-quick` |
| `iproute2` | the tunnel | **required** — `ip` |
| `systemd` | wake lock, service | degrade: the machine can suspend |

The daemon and CLI have **no external dependencies** — Go's standard library
only. The `desktop/` module is the exception, and it is a separate module
precisely so that stays true: a tray icon needs a D-Bus library, and a window
needs GTK.

**If your Go is older than 1.24, you do not need to upgrade it.** Both modules
set `GOTOOLCHAIN=auto` behaviour by default, so building `desktop/` with Go 1.22
makes Go fetch the 1.24 toolchain into its module cache and use it for that
build only. That works, and it needs network access the first time. If you would
rather control it, install Go 1.24 yourself and the download never happens.

The tray alone needs neither GTK nor 1.24 — it is pure Go and speaks D-Bus
directly. Everything except `myst-gui` builds with `CGO_ENABLED=0`.

### Build

```sh
sudo apt install libgtk-4-dev        # only needed for the window

# daemon and CLI — static, no cgo
CGO_ENABLED=0 go build -o mystd ./cmd/mystd
CGO_ENABLED=0 go build -o myst  ./cmd/myst

# tray — static, no cgo
(cd desktop && CGO_ENABLED=0 go build -o myst-tray ./cmd/myst-tray)

# window — cgo, links GTK4
(cd desktop &&               go build -o myst-gui  ./cmd/myst-gui)
```

The first build of the window takes a few minutes: gotk4 is a large generated
cgo binding and it is compiled from scratch once. Later builds are cached.

Check it before installing anything:

```sh
./mystd -h && ./myst version && ./myst-tray -h
```

### Install

The supported path is the package, because it is the one that needs no
terminal on the target machine:

```sh
./build-deb.sh                       # dist/mysterium-vpn_1.0.0_amd64.deb
./build-portable-package.sh          # also writes the zip with install.sh
```

The portable installer, for a machine where the `.deb` route is unavailable:

```sh
sudo ./install.sh
```

It installs the daemon, the launcher wrapper and the desktop entry under
`/usr/local`, and starts `mystd.service`. The wrapper gives the control socket
the desktop user's primary group, so **no log-out is needed** — unlike the old
`usermod -aG myst` instructions, which only took effect in a new session.

Then verify, in this order:

```sh
systemctl is-active mystd      # active
myst status                    # disconnected  (proves the socket and the group)
myst login --from <token.json> # see "Logging in"
myst countries | wc -l         # ~100     (proves the daemon has a working token)
```

### Rebuilding after a code change

**Rebuild the daemon, not just the front end.** This has cost real debugging
time twice: the window or CLI is rebuilt and reinstalled, the daemon is not, and
the new client talks to an old daemon. The symptom is not an error — it is a
front end quietly falling back to less data than it should have, because the
daemon answered `unknown command` to something the client had every reason to
expect.

```sh
# after changing anything under internal/ or cmd/
go build -o /tmp/mystd ./cmd/mystd && sudo install -m755 /tmp/mystd /usr/local/bin/
sudo systemctl restart mystd
```

`myst status` against a running daemon is the quickest way to tell what you are
actually talking to.

### Running without installing

Both front ends take `-socket`, so they can drive a daemon running out of a
checkout on a socket you own:

```sh
./mystd -socket /tmp/mystd.sock -socket-group "$(id -gn)" &
./myst -socket /tmp/mystd.sock status
(cd desktop && ./myst-gui)
```

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

The installed service does not hard-code a group. Its launcher
(`/usr/lib/mysterium-vpn/mystd-launch`, or `/usr/local/lib/...` for the
portable install) reads the active x11/wayland session's user and passes that
user's primary group, which they already belong to. That is what removes the
log-out step from installation. An administrator can override the choice by
writing a group name to `/etc/mysterium-vpn/socket-group`.

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

## Single-binary release build

The release build uses one executable for both the normal CLI and the privileged
control daemon. The daemon is selected internally with `--daemon`; normal
commands automatically start the same executable through `sudo` when the
standard daemon socket is not available.

Build it with:

```bash
./build-release.sh
```

This creates:

```text
./myst
```

You can then use:

```bash
./myst version
./myst login
./myst status
./myst connect
./myst disconnect
./myst logout
```

The first command that needs the daemon may ask for administrator authentication.
The daemon is detached from the CLI and uses the invoking user's primary group
for access to `/run/mystd/mystd.sock`.

For a custom output path:

```bash
./build-release.sh --output ./release/myst
```

For a reproducible version string:

```bash
VERSION=2.0.0 ./build-release.sh
```

The desktop GTK application remains a separate CGO/GTK target because GTK4 is
not part of the standard Go toolchain. The single-binary release target here is
the complete CLI + privileged daemon control plane; it does not copy GTK
runtime libraries into the executable.

## Single-binary desktop release

The release build produces one executable named `myst`. The executable contains
both the GTK desktop application and the privileged daemon implementation.

```bash
./build-release.sh
./myst
```

Running `./myst` opens the GUI. The GUI starts the same executable as
`./myst --daemon` through `pkexec` when the privileged backend is not already
running. Login, logout, account state, locations, and VPN controls are all
available from the GUI; a separate `mystd` binary is not required.

The GTK runtime itself remains a normal Linux shared-library dependency. The
application code and daemon are linked into the same ELF executable.
