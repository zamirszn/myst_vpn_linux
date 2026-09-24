# Mysterium VPN — Linux client feasibility

**Status: verified working end to end.** 2026-09-24.

A third-party Linux client for Mysterium VPN is buildable using **proxy mode** on the
consumer API. Verified against a live paid account: bearer auth → `connect-proxy` →
HTTP CONNECT proxy carrying real traffic through the Mysterium network.

No attestation wall. No client secret. Nothing extracted from a shipped binary — the
API host was found by observing the dashboard's own traffic, and every request was
made with the account holder's own credentials.

---

## The proof

```
GET  /auth/check                 -> 200    bearer token, no browser involved
GET  /connection/config          -> 200
POST /connection/connect-proxy   -> 200    returns proxy_config
```

Traffic through the returned proxy exits from a different IP than the direct
connection, geolocating to the advertised region. Confirmed on two separate nodes.

---

## API contract

**Base URL** — `https://app.mysteriumvpn.com/api/v1`
Not documented publicly. Recoverable from the dashboard's own network traffic.

All request bodies are **snake_case** (`client_id`, `ip_type`, `reset_connection`,
`os_type`). The official OpenAPI-generated Dart client models them camelCase because
that is its own mapping layer — do not copy its field names.

### `POST /magic-link`

```json
{
  "email": "...",
  "client_id": "web | app | extension | router",
  "code_challenge": "...",
  "code_challenge_method": "plain | S256 | s256",
  "continue_to": "optional"
}
```

### `POST /oauth/token`

`grant_type` is a discriminator: `authorization_code | refresh_token | google | apple`.

```json
// authorization_code (the standalone-client path)
{ "grant_type": "authorization_code", "client_id": "...", "code_verifier": "...", "code": "..." }

// refresh_token (renewal — no browser needed)
{ "grant_type": "refresh_token", "client_id": "...", "refresh_token": "..." }
```

Response: `{ access_token, token_type, expires_in, refresh_token, user_id, first_time }`

### `POST /connection/connect-proxy`

```json
{ "ip_type": "residential", "reset_connection": false, "os_type": "linux", "country": "optional" }
```

Response:

```json
{
  "proxy_config": { "host": "...", "port": "8080", "username": "...", "password": "...", "expires_at": "..." },
  "limit_exceeded": false
}
```

### Other endpoints

| Endpoint | Notes |
|---|---|
| `GET /auth/check` | validates a bearer token |
| `GET /auth/config` | unauthenticated; returns `google_client_id`, `apple_service_id` |
| `GET /auth/sessions` | lists registered devices under `.data[]` |
| `GET /connection/config` | read-only connection info |
| `GET /connection/disconnect` | **cleanup — call this** |

---

## Findings that shape the architecture

### The API lies about the proxy port

`proxy_config.port` returns **`8080`**. Every node tested refuses 8080 and only
listens on **`1080`**. A client must probe or fall back — do not trust the
advertised port. This is the single highest-value discovery here; it would
otherwise look like a dead node.

### Auth

Bearer tokens work, and a standalone client never needs a browser once it holds one.
Two ways to get the first token:

1. **Magic link** (fully headless): `POST /magic-link` with a self-generated PKCE
   `S256` challenge → user clicks the emailed link → `POST /oauth/token` with
   `grant_type=authorization_code` and the matching `code_verifier`. The verifier must
   come from the client that requested the link; you cannot complete a link that
   another client minted.
2. **From a browser session** via `grant_type=google` / `apple`, then capture the
   `/oauth/token` response.

Access tokens last ~30 days and ship with a `refresh_token`, so
`grant_type=refresh_token` keeps a client alive indefinitely.

**Caveat:** the web dashboard itself authenticates by cookie, not bearer. Both work,
but only bearer is usable outside a browser.

### The proxy

- Speaks **HTTP CONNECT with Basic auth** (`realm="supernode-gateway"`). It is *not*
  SOCKS5 — sending a SOCKS5 handshake gets `Received invalid version`.
- Credentials rotate roughly every 24 hours (`expires_at`).

### Device limit

10 slots per account. Exceeding it returns **HTTP 500** with
`err_fetch_connect_config_allowed_devices_limit`. Handle that code explicitly —
treating it as a generic server fault produces a useless error for users.

---

## Recommended architecture

```
┌─────────────────────────────────────────────────────────┐
│  mysterium-client (daemon)                              │
│                                                         │
│  1. auth      magic-link/PKCE, or refresh_token         │
│  2. connect   POST /connection/connect-proxy            │
│  3. probe     find the live port (1080), not the        │
│               advertised one (8080)                     │
│  4. serve     local forwarder on 127.0.0.1:PORT that    │
│               injects upstream Basic auth               │
│  5. refresh   re-connect before expires_at (~24h)       │
└─────────────────────────────────────────────────────────┘
                            ▲
              apps point here, never at the upstream
```

**Serve a local forwarder rather than handing the upstream proxy to applications.**
Two reasons: rotating credentials stay invisible to apps, and the upstream password
does not get copied into every application's config.

---

## Limitations — be honest about these

**Proxy mode is TCP only.** HTTP CONNECT cannot carry UDP. No QUIC (Chrome and
YouTube silently fall back to TCP), no WireGuard-over-UDP, no VoIP, no UDP games.
Fine for browsing; **not** equivalent to the official apps' WireGuard tunnel, and it
should not be presented as such.

**Client identity.** Testing used `client_id=extension`, which labels a Linux client
as the Chrome extension. Acceptable for personal testing on your own account. It is a
question to resolve with Mysterium before distributing anything.

**Terms of service.** Check them before shipping a third-party client.

**License.** The official Dart client `mysterium-vpn-api-client-dart` (package
`vpn_api`) has no LICENSE file. It is also not published to pub.dev, so it would need
a git dependency. Clarify before depending on it in anything you distribute.

---

## Repo contents

| File | Purpose |
|---|---|
| `capture-api.js` | Headed browser that records request **metadata only** — method, host, path, and auth header *names*. Never values, never query strings. Used to discover the API host. |
| `summarize-capture.sh` | Filters the capture log down to the API surface, hiding marketing noise. |
| `myst-auth.js` | Standalone magic-link + PKCE flow. No browser. |
| `test-bearer.js` | Replays a token as `Authorization: Bearer` and tests `/auth/check`, `/connection/config`, and optionally `connect-proxy`. |
| `probe-connect-proxy.js` | Runs the probe from inside a logged-in browser page over CDP. |
| `test-connect-proxy.sh` | Original curl probe against a bearer token or cookie. |

## Security

No credentials are committed here, and none should be. Tokens, proxy credentials,
the browser profile, and capture logs live outside the repo under `~/.myst-capture/`
and `~/.myst-env` (mode 0600), and `.gitignore` covers them defensively.

Scripts are written to never print secrets: `access_token` and proxy passwords are
reported as `<N chars>` and written only to 0600 files.
