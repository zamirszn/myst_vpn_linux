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

Built in Go with **no external dependencies** — `crypto/ecdh` for the keypair,
`net/http` for the API, `os/exec` for `wg-quick`.

```
cmd/mystd        privileged daemon: owns the tunnel, serves the control socket
cmd/myst         unprivileged CLI: connect / disconnect / status / login / logout
cmd/myst-tray    tray applet (not yet written)
```

| Package | Responsibility |
|---|---|
| `internal/wg` | keypair generation, config assembly, `wg-quick` control |
| `internal/api` | the consumer API client |
| `internal/auth` | token storage and refresh |
| `internal/daemon` | the connect/disconnect state machine |
| `internal/ipc` | unix-socket control protocol |
| `internal/wakelock` | suspend inhibitor, held while connected |

### Build and run

```sh
go build ./...
sudo install -m755 mystd /usr/local/bin/mystd
sudo install -m644 deploy/mystd.service /etc/systemd/system/
sudo systemctl enable --now mystd

myst connect --country gb
myst status
myst disconnect
```

`myst login --from <token.json>` imports a captured token so the client can
authenticate without a browser. Credentials land in `/var/lib/mystd/token.json`
at 0600. The magic-link/PKCE flow is not yet implemented.

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

**The kill switch is yours to build.** Neither the config nor `wg-quick` provides one.
Shipping without it means leaks on tunnel failure.

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
