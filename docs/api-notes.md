# API notes — how this client's API surface was established

Research notes, kept because re-deriving them would mean redoing the work. This
is the *reference* half of the project; [the README](../README.md) is the
user-facing half. Nothing here is needed to install or use the client.

Everything below was verified against a live paid account on 2026-09-24.

## Two connection modes exist

| | **WireGuard** (what this client uses) | **HTTP proxy** (fallback) |
|---|---|---|
| Endpoint | `POST /connection/router` | `POST /connection/connect-proxy` |
| Transport | Full tunnel, UDP works | TCP only |
| Performance | Kernel WireGuard | Userspace forwarding |
| Parity with official apps | Yes | No |
| Root required | Yes (TUN + routes) | No |

No attestation wall, no client secret, nothing extracted from a shipped binary.
The API host was found by observing the dashboard's own traffic, and every
request was made with the account holder's own credentials.

---

## WireGuard path

```
POST /api/v1/connection/router          {country, ip_type}  -> {filename, filesize}
GET  /api/v1/connection/router/download                     -> text/plain wg-quick config
GET  /api/v1/connection/disconnect                          -> teardown
```

`GET /connection/router` returns the same metadata, or `404` when no config
exists yet — useful as an existence check.

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

- **No VPN engine to write.** The protocol and crypto are WireGuard's, already
  in the kernel.
- **Root buys exactly what stock WireGuard needs** — TUN device, routes, DNS.
  Nothing more. The privileged helper writes the config, calls `wg-quick up`,
  manages the kill switch, and tears down. It is a supervisor, not a packet path.
- `Endpoint` is a bare IP, so bringing the tunnel up needs no DNS resolution.
- `PersistentKeepalive = 25` survives NAT.

### The IPv6 gap

**The config assigns an IPv6 address but routes no IPv6.** `AllowedIPs` is
IPv4-only; there is no `::/0`. On any host with IPv6 connectivity, that traffic
**bypasses the tunnel entirely** — a silent leak. This is a defect in Mysterium's
own router config, not something introduced here.

A client must either append `::/0` to `AllowedIPs`, or disable IPv6 on the
interface. This client does the former by default; `-no-ipv6` turns it off.

### Key handling

The server **generates the private key and sends it over the wire.** Reasonable
for a router, where you paste a config into a web UI. Strictly worse for a Linux
client.

`public_key` **is** a recognised optional field on `POST /connection/router` —
confirmed by a Zod type error naming it (`expected string, received number at
"public_key"`) rather than treating it as an unknown key. So the intended flow is
available:

> generate the keypair locally → submit only the public key → **the private key
> never leaves the machine.**

This client generates locally and submits the public key. The private key is
written to `/etc/wireguard/myst0.conf` at 0600 and removed on disconnect.

### Measured behaviour

**A `/connection/connect` tunnel consumes no device slot.** The `_dvpn` device
list was counted across a full cycle:

```
slots before connect   : 7
slots after connect    : 7
slots after disconnect : 7
```

This contradicts the router path below, and it matters: a Linux machine using
this client does not eat one of the account's 10 device slots. Note the device
list contains only phone entries — a WireGuard connection does not register as a
`_dvpn` device at all.

**`limit_exceeded: true` is returned on every connect, even with slots free.**
Unexplained. It does not track the 10-device limit and it does not block
anything — every connect still returned a working config. Treat the flag as
advisory, and do not infer a quota from it.

**Distinct keypairs get distinct concurrent connections.** Two connects with
different public keys, no disconnect between them:

```
keypair qUb6qsj8…  id=db9ec63f…  addr=10.181.156.136  endpoint=91.98.228.210:56666
keypair 2UTonKkV…  id=07981048…  addr=10.181.101.112  endpoint=91.98.228.210:56666
```

Different ids, different tunnel addresses. So several machines each get their own
connection. **Not yet verified:** that two can carry traffic *simultaneously* —
the server issuing two connections is not the same as both handshaking at once.
That needs two machines or two interfaces.

### Do not build on `/connection/router`

`/connection/router` is the **router** path, and it is strictly worse for this
purpose:

| | `/connection/router` | `/connection/connect` |
|---|---|---|
| Per account | **1 only** | one per keypair |
| Setup | manual, in the dashboard | none |
| Private key | **generated by the server and transmitted** | generated locally, never transmitted |

It is absent from the public generated client. A second POST while a config
exists fails with `Your account has no device slots left` — which is a *router*
limit, not the device limit, and must not be confused with the `_dvpn` slot count
above.

---

## Proxy path (fallback, no root needed)

```
POST /api/v1/connection/connect-proxy   {ip_type, reset_connection, os_type, country?}
  -> { proxy_config: {host, port, username, password, expires_at}, limit_exceeded }
GET  /api/v1/connection/disconnect
```

Not implemented in this client. Recorded because the two findings below are
expensive to rediscover.

### The API lies about the proxy port

`proxy_config.port` returns **`8080`**. Every node tested refuses 8080 and only
listens on **`1080`**. A client must probe or fall back — do not trust the
advertised port. This is the single highest-value discovery on this path; it
would otherwise look like a dead node.

### The proxy speaks HTTP CONNECT, not SOCKS5

Basic auth, `realm="supernode-gateway"`. A SOCKS5 handshake gets `Received
invalid version`. Credentials rotate roughly every 24 hours (`expires_at`), so a
refresh timer is required.

**Serve a local forwarder** on `127.0.0.1:PORT` rather than handing the upstream
proxy to applications: rotating credentials stay invisible to apps, and the
upstream password does not get copied into every application's config.

---

## API contract

**Base URL** — `https://app.mysteriumvpn.com/api/v1`
Not documented publicly. Recoverable from the dashboard's own network traffic.

Request bodies are **snake_case** (`client_id`, `ip_type`, `reset_connection`,
`os_type`). The official OpenAPI-generated Dart client models them camelCase
because that is its own mapping layer — do not copy its field names.

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

1. **Magic link** (fully headless): `POST /magic-link` with a self-generated PKCE
   `S256` challenge → user clicks the emailed link → `POST /oauth/token` with
   `grant_type=authorization_code` and the matching `code_verifier`. The verifier
   must come from the client that requested the link; you cannot complete a link
   another client minted.
2. **From a browser session** via `grant_type=google` / `apple`, capturing the
   `/oauth/token` response.

Tokens last ~30 days and ship with a `refresh_token`, so
`grant_type=refresh_token` keeps a client alive indefinitely.

**Caveat:** the web dashboard authenticates by cookie, not bearer. Both work, but
only bearer is usable outside a browser.

### Client identity

Testing used `client_id=extension`, which labels a Linux client as the Chrome
extension. Acceptable for personal testing on your own account; a question to
resolve with Mysterium before distributing.

---

## Recommended architecture

```
┌──────────────────────────────────────────────────────────┐
│  mystd (daemon + privileged helper)                      │
│                                                          │
│  1. auth      magic-link/PKCE, or refresh_token          │
│  2. keys      generate WireGuard keypair locally         │
│  3. connect   POST /connection/router {public_key}       │
│  4. fetch     GET  /connection/router/download           │
│  5. up        write /etc/wireguard/myst0.conf (0600)     │
│               wg-quick up myst0                          │
│  6. protect   nftables kill switch; add ::/0             │
│  7. watch     rebuild the tunnel if it dies              │
└──────────────────────────────────────────────────────────┘
```

Two things to include from the start because they cannot be retrofitted cleanly:
**a kill switch** (the config provides none — `wg-quick` doesn't either) and
**IPv6 handling** (see the gap above).

---

## Probe scripts

The scripts that established all of the above. They are kept for reference and
are not part of the client's build.

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

## Where the secrets live

No credentials are committed to this repo, and none should be. Tokens, WireGuard
configs, proxy credentials, the browser profile, and capture logs live outside
the repo under `~/.myst-capture/` and `~/.myst-env` (mode 0600), and `.gitignore`
covers them defensively.

Scripts never print secrets: `access_token`, proxy passwords, and `PrivateKey` /
`PresharedKey` lines are reported as `<redacted>` and written only to 0600 files.
