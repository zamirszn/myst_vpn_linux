#!/usr/bin/env node
/**
 * Generate a WireGuard config from the Mysterium VPN 2.0 consumer API.
 *
 *   POST /api/v1/connection/router   { country, ip_type }
 *
 * This is the sibling of /connection/connect-proxy: same body shape, but it
 * returns a WireGuard configuration instead of an HTTP CONNECT proxy. If it
 * returns a standard wg-quick config, a Linux client can use kernel WireGuard
 * rather than the TCP-only proxy path.
 *
 * SIDE EFFECT: this creates a router config on the account, which consumes one
 * device slot. Limit is 1 router config per account; remove it via the dashboard
 * ("Remove Router Config") when done.
 *
 * Safety: the response WILL contain a WireGuard private key. The full response is
 * written to a 0600 file and never printed. Console output shows structure and
 * non-secret fields only; PrivateKey/PresharedKey lines are masked.
 *
 *   NODE_PATH=/home/z/rpa/node_modules node probe-wireguard-config.js [country] [ip_type]
 */
const fs = require('fs');
const path = require('path');

const BASE = 'https://app.mysteriumvpn.com/api/v1';
const OUT = path.join(process.env.HOME, '.myst-capture');
const TOKEN_FILE = path.join(OUT, 'token-from-browser.json');
const CONFIG_FILE = path.join(OUT, 'wireguard-config.json');

const COUNTRY = process.argv[2] || 'GB';
const IP_TYPE = process.argv[3] || 'hosting';

const SENSITIVE = /(key|passwo?r?d|secret|token|credential|private|preshared)/i;

// Mask secrets inside a wg-quick style INI blob, but keep the shape visible.
function maskWgText(text) {
  return text
    .replace(/^(\s*(?:PrivateKey|PresharedKey)\s*=\s*).*$/gim, '$1<redacted>')
    .replace(/^(\s*(?:PublicKey)\s*=\s*)(.{8}).*$/gim, '$1$2…<redacted>');
}

function sanitize(v, depth = 0) {
  if (depth > 6) return '<deep>';
  if (Array.isArray(v)) return v.slice(0, 6).map((x) => sanitize(x, depth + 1));
  if (v && typeof v === 'object') {
    const out = {};
    for (const [k, val] of Object.entries(v)) {
      if (SENSITIVE.test(k)) {
        out[k] = typeof val === 'string' ? `<redacted:${val.length} chars>` : '<redacted>';
      } else if (typeof val === 'string' && /\[Interface\]/i.test(val)) {
        out[k] = maskWgText(val);
      } else {
        out[k] = sanitize(val, depth + 1);
      }
    }
    return out;
  }
  if (typeof v === 'string') {
    if (/^[A-Za-z0-9+/]{43}=$/.test(v)) return `<base64key:${v.length}>`;
    if (/^[0-9a-f]{64}$/i.test(v)) return '<hex64>';
    if (v.length > 200) return `<long:${v.length} chars>`;
  }
  return v;
}

(async () => {
  const raw = JSON.parse(fs.readFileSync(TOKEN_FILE, 'utf8'));
  const token = raw.access_token || raw.accessToken;
  if (!token) throw new Error('no access_token in captured token file');

  console.log(`POST /connection/router  { country: "${COUNTRY}", ip_type: "${IP_TYPE}" }\n`);

  const r = await fetch(`${BASE}/connection/router`, {
    method: 'POST',
    headers: {
      Accept: 'application/json',
      'Content-Type': 'application/json',
      Authorization: `Bearer ${token}`,
    },
    body: JSON.stringify({ country: COUNTRY, ip_type: IP_TYPE }),
  });

  const text = await r.text();
  console.log(`-> HTTP ${r.status}\n`);

  if (r.status !== 200) {
    // A validation error here is free — nothing was created. Print it in full;
    // if ip_type is an enum, Zod will enumerate the valid values.
    console.log('response body:');
    console.log(text.slice(0, 1500));
    if (r.status === 400) {
      console.log('\n(nothing created — validation failed, so no device slot consumed)');
    }
    process.exit(r.status === 400 ? 2 : 1);
  }

  fs.writeFileSync(CONFIG_FILE, text, { mode: 0o600 });
  console.log(`full response -> ${CONFIG_FILE}  [mode 0600]\n`);

  let json = null;
  try {
    json = JSON.parse(text);
  } catch {}

  console.log('=== structure (secrets redacted) ===');
  console.log(json ? JSON.stringify(sanitize(json), null, 2) : maskWgText(text).slice(0, 2000));

  // Does it look like a usable wg-quick config?
  const blob = json ? JSON.stringify(json) : text;
  const hasInterface = /\[Interface\]/i.test(blob);
  const hasPeer = /\[Peer\]/i.test(blob);
  const hasEndpoint = /Endpoint\s*=/i.test(blob);

  console.log('\n=== verdict ===');
  console.log(`  [Interface] section : ${hasInterface ? 'yes' : 'no'}`);
  console.log(`  [Peer] section      : ${hasPeer ? 'yes' : 'no'}`);
  console.log(`  Endpoint= present   : ${hasEndpoint ? 'yes' : 'no'}`);
  console.log(
    hasInterface && hasPeer && hasEndpoint
      ? '\n  -> standard WireGuard config. Likely usable with wg-quick on plain Linux.'
      : '\n  -> not a plain wg-quick config; inspect the saved file.'
  );
  console.log('\n  cleanup: remove the router config in the dashboard (Home -> Remove Router Config)');
})().catch((e) => {
  console.error('failed:', e.message);
  process.exit(1);
});
