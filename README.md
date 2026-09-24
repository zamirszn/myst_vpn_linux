# Mysterium VPN 2.0 — Linux client feasibility

**Status: two viable paths verified against a live paid account.** 2026-09-24.

A third-party Linux client for **Mysterium VPN 2.0** (the subscription product — not the
legacy MYST/dVPN network) is buildable. Two connection modes exist on the consumer API,
and both work with the account holder's own credentials:

| | **WireGuard** (preferred) | **HTTP proxy** (fallback) |
|---|---|---|
| Endpoint | `POST /connection/router` | `POST /connection/connect-proxy` |
| Transport | Full tunnel, UDP works | TCP only |
| Performance | Kernel WireGuard | Userspace forwarding |
| Parity with official apps | Yes | No |
| Root required | Yes (TUN + routes) | No |

**Build the WireGuard path.** It is a real tunnel, it is what the official apps use, and
the config is stock `wg-quick` format. The proxy path is a useful fallback where root is
unavailable, but it cannot carry UDP — no QUIC, VoIP, or games.

No attestation wall. No client secret. Nothing extracted from a shipped binary — the API
host was found by observing the dashboard's own traffic, and every request was made with
the account holder's own credentials.

---

## WireGuard path (preferred)

Three calls, no crypto layer anywhere:

```
POST /api/v1/connection/router          {country, ip_type}  -> {filename, filesize}
GET  /api/v1/connection/router/download                     -> text/plain wg-quick config
GET  /api/v1/connection/disconnect                          -> teardown
```

`GET /connection/router` returns the same metadata, or `404` when no config exists yet —
useful as an existence check.

### The returned config is stock wg-quick

```
[Interface]
PrivateKey          = <redacted>
Address             = 10.181.86.246, fe80::1234:5678:90ab:cdef/128
DNS                 = 1.1.1.1

[Peer]
PublicKey           = BWIx1BlaS5iZ5C+eYlONhfL6H749GebmdcEPLaphJzY=
AllowedIPs          = <163 CIDRs spanning the full IPv4 space>
Endpoint            = 91.98.228.210:56666
PersistentKeepalive = 25
```

`wg-quick up myst0` handles it unmodified. Consequences for the build:

- **No VPN engine to write.** The protocol and crypto are WireGuard's, already in the kernel.
- **Root buys exactly what stock WireGuard needs** — TUN device, routes, DNS. Nothing more.
  The privileged helper writes the config, calls `wg-quick up`, manages the kill switch,
  and tears down. It is a supervisor, not a packet path.
- `Endpoint` is a bare IP, so bringing the tunnel up needs no DNS resolution.
- `PersistentKeepalive = 25` survives NAT.

### The IPv6 gap — fix this

**The config assigns an IPv6 address but routes no IPv6.** `AllowedIPs` is IPv4-only;
there is no `::/0`. On any host with IPv6 connectivity, that traffic **bypasses the tunnel
entirely** — a silent leak. This is a defect in Mysterium's own router config, not
something introduced here.

A client must either append `::/0` to `AllowedIPs`, or disable IPv6 on the interface.
`wg-test.sh` supports this via `WG_ADD_IPV6=1`.

### Key handling — resolve before building

The server **generates the private key and sends it over the wire.** Reasonable for a
router, where you paste a config into a web UI. Strictly worse for a Linux client.

`public_key` **is** a recognised optional field on `POST /connection/router` — confirmed
by a Zod type error naming it (`expected string, received number at "public_key"`) rather
than treating it as an unknown key. So the intended flow is available:

> generate the keypair locally → submit only the public key → **the private key never
> leaves the machine.**

Not yet confirmed end-to-end, because the account was at its 10/10 slot limit. Verify by
removing the existing router config, then POSTing with a `public_key`. **If it works, build
on it.** Generating keys locally is the correct design.

### Measured behaviour

All measured against a live account on 2026-09-24.

**A `/connection/connect` tunnel consumes no device slot.** The `_dvpn` device list was
counted across a full cycle:

```
slots before connect   : 7
slots after connect    : 7
slots after disconnect : 7
```

This contradicts the router path below, and it matters: a Linux machine using this client
does not eat one of the account's 10 device slots. Note the device list contains only
phone entries — a WireGuard connection does not register as a `_dvpn` device at all.

**`limit_exceeded: true` is returned on every connect, even with slots free.** Unexplained.
It does not track the 10-device limit and it does not block anything — every connect still
returned a working config. Treat the flag as advisory, and do not infer a quota from it.

**Distinct keypairs get distinct concurrent connections.** Two connects with different
public keys, no disconnect between them:

```
keypair qUb6qsj8…  id=db9ec63f…  addr=10.181.156.136  endpoint=91.98.228.210:56666
keypair 2UTonKkV…  id=07981048…  addr=10.181.101.112  endpoint=91.98.228.210:56666
```

Different ids, different tunnel addresses. So several machines each get their own
connection. **Not yet verified:** that two can carry traffic *simultaneously* — the server
issuing two connections is not the same as both handshaking at once. That needs two machines
or two interfaces.

### Do not build on `/connection/router`

`/connection/router` is the **router** path, and it is strictly worse for this purpose:

| | `/connection/router` | `/connection/connect` |
|---|---|---|
| Per account | **1 only** | one per keypair |
| Setup | manual, in the dashboard | none |
| Private key | **generated by the server and transmitted** | generated locally, never transmitted |

It is absent from the public generated client. A second POST while a config exists fails with
`Your account has no device slots left` — which is a *router* limit, not the device limit, and
must not be confused with the `_dvpn` slot count above.

---

## Proxy path (fallback, no root needed)

```
POST /api/v1/connection/connect-proxy   {ip_type, reset_connection, os_type, country?}
  -> { proxy_config: {host, port, username, password, expires_at}, limit_exceeded }
GET  /api/v1/connection/disconnect
```

### The API lies about the proxy port

`proxy_config.port` returns **`8080`**. Every node tested refuses 8080 and only listens on
**`1080`**. A client must probe or fall back — do not trust the advertised port. This is
the single highest-value discovery on this path; it would otherwise look like a dead node.

### The proxy speaks HTTP CONNECT, not SOCKS5

Basic auth, `realm="supernode-gateway"`. A SOCKS5 handshake gets
`Received invalid version`. Credentials rotate roughly every 24 hours (`expires_at`), so a
refresh timer is required.

**Serve a local forwarder** on `127.0.0.1:PORT` rather than handing the upstream proxy to
applications: rotating credentials stay invisible to apps, and the upstream password does
not get copied into every application's config.

---

## API contract

**Base URL** — `https://app.mysteriumvpn.com/api/v1`
Not documented publicly. Recoverable from the dashboard's own network traffic.

Request bodies are **snake_case** (`client_id`, `ip_type`, `reset_connection`, `os_type`).
The official OpenAPI-generated Dart client models them camelCase because that is its own
mapping layer — do not copy its field names.

| Endpoint | Notes |
|---|---|
| `POST /magic-link` | `{email, client_id, code_challenge, code_challenge_method, continue_to?}` |
| `POST /oauth/token` | `grant_type`: `authorization_code \| refresh_token \| google \| apple` |
| `GET  /auth/check` | validates a bearer token |
| `GET  /auth/config` | unauthenticated; returns `google_client_id`, `apple_service_id` |
| `GET  /auth/sessions` | registered devices under `.data[]` |
| `GET  /connection/config` | country list, `top_countries` |
| `POST /connection/router` | **WireGuard** — `{country, ip_type, public_key?}` |
| `GET  /connection/router` | config metadata, `404` if none |
| `GET  /connection/router/download` | the `wg-quick` config as `text/plain` |
| `POST /connection/connect-proxy` | HTTP proxy config |
| `GET  /connection/disconnect` | **cleanup — call this** |
| `GET  /subscription` | plan, period, status |

`client_id` is a closed enum: `web | app | extension | router`.

### Auth

Bearer tokens work; a standalone client never needs a browser once it holds one.

1. **Magic link** (fully headless): `POST /magic-link` with a self-generated PKCE `S256`
   challenge → user clicks the emailed link → `POST /oauth/token` with
   `grant_type=authorization_code` and the matching `code_verifier`. The verifier must come
   from the client that requested the link; you cannot complete a link another client minted.
2. **From a browser session** via `grant_type=google` / `apple`, capturing the `/oauth/token`
   response.

Tokens last ~30 days and ship with a `refresh_token`, so `grant_type=refresh_token` keeps a
client alive indefinitely.

**Caveat:** the web dashboard authenticates by cookie, not bearer. Both work, but only
bearer is usable outside a browser.

---

## Recommended architecture

```
┌──────────────────────────────────────────────────────────┐
│  mysterium-client (daemon + privileged helper)           │
│                                                          │
│  1. auth      magic-link/PKCE, or refresh_token          │
│  2. keys      generate WireGuard keypair locally         │
│  3. connect   POST /connection/router {public_key}       │
│  4. fetch     GET  /connection/router/download           │
│  5. up        write /etc/wireguard/myst0.conf (0600)     │
│               wg-quick up myst0                          │
│  6. protect   nftables kill switch; add ::/0             │
│  7. renew     regenerate before the config goes stale    │
└──────────────────────────────────────────────────────────┘
```

Two things to include from the start because they cannot be retrofitted cleanly:
**a kill switch** (the config provides none — `wg-quick` doesn't either) and **IPv6
handling** (see the gap above).

---

## The client

The daemon and CLI are built in Go with **no external dependencies** —
`crypto/ecdh` for the keypair, `net/http` for the API, `os/exec` for `wg-quick`.
The tray applet is the one exception and lives in its own module; see
[The tray applet](#the-tray-applet).

```
cmd/mystd        privileged daemon: owns the tunnel, serves the control socket
cmd/myst         unprivileged CLI: connect / disconnect / status / login / logout
tray/            tray applet: its own Go module, because it needs a D-Bus library
```

| Package | Responsibility |
|---|---|
| `internal/wg` | keypair generation, config assembly, `wg-quick` control |
| `internal/api` | the consumer API client |
| `internal/auth` | token storage and refresh |
| `internal/daemon` | the connect/disconnect state machine |
| `internal/ipc` | unix-socket control protocol |
| `internal/killswitch` | nftables egress filter, armed while connected |
| `internal/wakelock` | suspend inhibitor, held while connected |
| `tray/` | StatusNotifierItem + dbusmenu tray icon (separate module) |

### Build and run

```sh
# daemon and CLI — no external dependencies
go build -o mystd ./cmd/mystd
go build -o myst  ./cmd/myst
sudo install -m755 mystd myst /usr/local/bin/
sudo install -m644 deploy/mystd.service /etc/systemd/system/

# let your desktop user drive the daemon; see "The control socket" below
sudo groupadd --system myst
sudo usermod -aG myst "$USER"
sudo sed -i 's|^ExecStart=/usr/local/bin/mystd$|ExecStart=/usr/local/bin/mystd -socket-group myst|' \
    /etc/systemd/system/mystd.service
sudo systemctl daemon-reload
sudo systemctl enable --now mystd

myst connect --country gb
myst status
myst disconnect
```

Log out and back in once, so the new group membership takes effect.

**Rebuild and re-install `mystd` whenever its flags change.** The unit file and
the binary are installed independently, so it is possible to edit `ExecStart` to
pass a flag the installed binary predates. Go's `flag` package rejects an unknown
flag with exit status 2, and the unit then crash-loops every `RestartSec`.
`systemctl status` reports `activating (auto-restart)`, which reads like a
startup failure rather than a stale binary — check
`strings /usr/local/bin/mystd | grep <flag>` when a daemon refuses to start right
after a unit change.

`myst login --from <token.json>` imports a captured token so the client can
authenticate without a browser. Credentials land in `/var/lib/mystd/token.json`
at 0600. The magic-link/PKCE flow is not yet implemented.

### The control socket

The daemon runs as root, so a unix socket it creates is `root:root`. At mode
0660 that means **only root can talk to it** — the CLI and the tray would both
fail with `permission denied` against a daemon that looks perfectly healthy.

`mystd -socket-group <group>` hands the socket to a group instead:

```sh
sudo groupadd --system myst
sudo usermod -aG myst "$USER"        # log out and back in afterwards
```

The mode is deliberately not widened to 0666 in the alternative. Anything that
can open this socket can make a root process bring a tunnel up or down, so
access is granted by group membership rather than to every account on the box.
Naming a group that does not exist is a startup failure, not a silent fallback
to root-only — a typo should not look like a working daemon.

`mystd` logs which of the two it is doing at startup.

### The tray applet

```
cd tray && go build -o myst-tray .
sudo install -m755 myst-tray /usr/local/bin/
```

To have it come up with the session, drop an autostart entry in place:

```sh
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

The applet reaches the daemon through the control socket, so the session needs
the `myst` group — **log out and back in after `usermod`**, or every action fails
with `permission denied` against a daemon that looks perfectly healthy.

A shield in the system tray, filled by state:

| | |
|---|---|
| green, keyhole | connected |
| amber | connecting |
| grey outline | not connected |
| red, slashed | the last operation failed |
| grey, slashed | the daemon is not answering |

Left click connects or disconnects. Right click opens a menu with the state,
Connect/Disconnect, a region submenu, Copy exit IP, and Quit.

It holds **no credentials and no privileges**. Every action is a request over
the control socket, so the tray can be run by the logged-in user while the only
root process remains the daemon.

**It is a separate Go module.** Everything else in this repo builds with the
standard library alone; a tray icon cannot, because Go has no D-Bus in its
standard library. Rather than give up that property project-wide, the tray lives
in `tray/` with its own `go.mod` and one dependency, `github.com/godbus/dbus/v5`.
It speaks StatusNotifierItem and `com.canonical.dbusmenu` directly — no cgo, no
GTK, and no `libayatana-appindicator` headers to install.

GNOME needs the AppIndicator extension for any of this to show up. If it is
missing the applet says so and exits rather than running invisibly:

```sh
gnome-extensions enable ubuntu-appindicators@ubuntu.com
```

The region list is curated rather than fetched. The full country list lives
behind `GET /connection/config`, which needs an access token — and the token
belongs to the daemon, not to an unprivileged desktop applet. Copying the
credential into the applet to populate a menu would undo the point of the
split, so the applet offers the countries people actually pick and accepts any
code passed to `-country`.

### The wake lock

Suspend silently kills a WireGuard tunnel. The interface survives the sleep, so
nothing looks wrong on resume — the handshake is simply stale and traffic stops,
or leaks in the window before it re-establishes.

While connected, the daemon holds a logind inhibitor (`systemd-inhibit
--what=sleep:idle --mode=block`) so the machine cannot suspend out from under the
tunnel. It is released on disconnect and, because the inhibitor lasts exactly as
long as the child process, automatically if the daemon dies — a crash cannot
leave the machine unable to sleep forever. `myst status` reports whether it is
held; disable it with `mystd -no-wakelock`.

This is about **suspend, not logout**. The daemon is a *system* service, so
logging out of the desktop does not affect it and no `loginctl enable-linger` is
needed.

### The kill switch

Neither the WireGuard config nor `wg-quick` blocks traffic when a tunnel drops.
Without one, a dropped tunnel means traffic quietly continues in the clear from
the same machine — the failure mode a VPN exists to prevent.

While connected, the daemon installs an nftables egress filter in its own table:

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

**It fails closed.** The filter goes in *before* the interface comes up and comes
out *after* it goes down, so there is never a window in which traffic could
escape. A test asserts that ordering rather than assuming it.

**No `ct state established,related accept`.** The obvious rule to add, and the
wrong one: connections opened before the filter was armed would keep flowing out
of the physical interface, which is exactly the leak being prevented.

**The endpoint always stays reachable.** A filter that blocked the WireGuard
handshake would stop the tunnel from ever re-establishing, leaving the machine
offline. The daemon refuses to arm with no endpoint at all, and resolves a
hostname endpoint *before* arming, so the rule is right either way.

**It is one table.** Nothing you already have — ufw, Docker, hand-written rules —
is read or modified; the filter is created and deleted as a unit. That is also
the escape hatch:

```sh
sudo nft delete table inet myst_killswitch
```

`mystd` clears a stale filter on startup, so a daemon killed mid-connect cannot
leave you without a network. `myst status` reports whether it is armed; disable
it with `mystd -no-killswitch`.

**What it does not cover.** It filters the *output* hook, so it governs traffic
originating on this host. Traffic forwarded on behalf of containers does not
traverse that hook and is **not** filtered — with Docker running, container
traffic can still leave in the clear when the tunnel drops. The filter is also
egress-only: unsolicited inbound is not filtered, which is the same posture as
having no kill switch, since nothing but the handshake is listening.

### What was already true on this machine

`mystd` is **not** what keeps this box awake. It was already configured that way,
and the systemd masks predate this project by over a year (2025-05-28):

| Setting | Value | Effect |
|---|---|---|
| `sleep.target` | masked | cannot suspend at all |
| `suspend.target` | masked | cannot suspend at all |
| `org.gnome.desktop.session idle-delay` | `0` | screen never blanks |
| `org.gnome.desktop.screensaver lock-enabled` | `false` | screen never locks |
| `sleep-inactive-ac-type` / `-battery-type` | `nothing` | GNOME never suspends |

The one gap: `hibernate.target` and `hybrid-sleep.target` are **not** masked, so
`systemctl hibernate` still works. To close it:

```sh
sudo systemctl mask hibernate.target hybrid-sleep.target
```

`systemd-inhibit --list` shows what is currently holding the machine awake.

---

## Limitations — be honest about these

**The kill switch covers host traffic only.** It filters the output hook, so
forwarded traffic — containers, VMs on a bridge — is not covered. Closing that
means adding a forward chain, which risks breaking Docker's networking and has
not been attempted here.

**The kill switch loads, but its blocking has never been provoked.** Verified on
a live kernel (Ubuntu, 6.11) on 2026-09-24: `myst connect` arms the table, the
tunnel establishes with the endpoint still reachable *through* the filter, and
`myst disconnect` removes the table — leaving the public IP back at its
pre-connect value and DNS working. That covers installation, the endpoint
exemption, and teardown.

What that does **not** cover is the case the switch exists for. The tunnel was
never dropped while the switch was armed, so the `counter drop` rule has not been
observed dropping a packet. "It loads and does not break the tunnel" is a
different claim from "it blocks the leak", and only the first has been shown.

**The tray needs a StatusNotifier host.** On GNOME that means the AppIndicator
extension; without it the applet exits with an explanation rather than running
invisibly. Everything else here is desktop-agnostic.

**IPv6 leaks by default.** See above.

**Client identity.** Testing used `client_id=extension`, which labels a Linux client as the
Chrome extension. Acceptable for personal testing on your own account; a question to resolve
with Mysterium before distributing.

**Terms of service.** Check them before shipping a third-party client.

**License.** The official Dart client `mysterium-vpn-api-client-dart` (package `vpn_api`)
has no LICENSE file, and is not published to pub.dev, so it would need a git dependency.
Clarify before depending on it in anything you distribute.

---

## Repo contents

| File | Purpose |
|---|---|
| `cmd/mystd`, `cmd/myst` | The daemon and the CLI. |
| `internal/` | The packages behind them. |
| `tray/` | The tray applet, in its own module. |
| `deploy/mystd.service` | systemd unit for the daemon, with notes on the socket group. |
| `probe-wireguard-config.js` | Generates a WireGuard config via `POST /connection/router`, saves it 0600, prints structure with keys redacted. **Consumes a device slot.** |
| `wg-test.sh` | Brings the tunnel up with `wg-quick`, verifies the exit IP, tears down. **Run with sudo.** Arms an auto-teardown before going up. |
| `probe-connection-modes.js` | Read-only sweep for connection-mode endpoints. Consumes nothing. |
| `capture-api.js` | Headed browser recording request **metadata only** — method, host, path, auth header *names*. Never values, never query strings. Used to discover the API host. |
| `summarize-capture.sh` | Filters the capture log to the API surface. |
| `myst-auth.js` | Standalone magic-link + PKCE flow. No browser. |
| `test-bearer.js` | Replays a token as `Authorization: Bearer`; tests `/auth/check`, `/connection/config`, optionally `connect-proxy`. |
| `probe-connect-proxy.js` | Runs the probe from inside a logged-in browser page over CDP. |
| `test-connect-proxy.sh` | Original curl probe against a bearer token or cookie. |

## Security

No credentials are committed here, and none should be. Tokens, WireGuard configs, proxy
credentials, the browser profile, and capture logs live outside the repo under
`~/.myst-capture/` and `~/.myst-env` (mode 0600), and `.gitignore` covers them defensively.

Scripts never print secrets: `access_token`, proxy passwords, and `PrivateKey` /
`PresharedKey` lines are reported as `<redacted>` and written only to 0600 files.
