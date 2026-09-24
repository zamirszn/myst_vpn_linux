# reference — captured artifacts

Real artifacts from a working session against a live Mysterium VPN 2.0 account,
kept so the build can be reproduced and the config shape stays documented.

## What is here

| File | Notes |
|---|---|
| `sample-config.redacted.conf` | **Read this one.** The real config shape with all key material masked. |
| `myst0.conf` | The live config, private key included. Ephemeral — dies when the device slot is freed. |
| `wg-keypair.json` | The Curve25519 keypair, generated locally. Private key included. |

## What is deliberately NOT here

**The account bearer/refresh token.** `~/.myst-capture/token-from-browser.json` holds a
credential valid for roughly 30 days with a `refresh_token` that renews it — full access
to the account: subscription, billing, device list, and the ability to provision
connections. It is:

- **not needed here** — it can be reissued in seconds via `POST /magic-link` and the PKCE
  flow in `myst-auth.js`;
- **permanent if committed** — git history outlives any later deletion, repo visibility
  change, collaborator add, or fork.

Same reasoning excludes `proxy-config.json`, whose credentials rotate about every 24 hours.

## On the WireGuard private key being committed

This is a deliberate, consented choice, and the blast radius is small: it is bound to a
single device slot on one account and stops mattering the moment that connection is torn
down (`GET /connection/disconnect`) or the slot is freed. A Mysterium WireGuard key is not
a long-lived identity — it is a per-connection credential.

Note this is *not* true of the account token, which is why the two were split.

**If this repo is ever made public, or a collaborator is added, revoke the connection
first.** Replacing the key costs one `POST /connection/connect`.
