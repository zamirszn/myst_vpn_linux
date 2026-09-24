#!/usr/bin/env node
/**
 * Probe the Mysterium VPN 2.0 consumer API for a NON-proxy connection mode.
 *
 * Why this exists: the dashboard's own capture log shows it calling
 * `GET /api/v1/connection/router` (-> 404). That is very likely the WireGuard
 * config endpoint used by the "VPN for router" / setup-guides page. If it hands
 * out a WireGuard config, a Linux client can use the standard kernel WireGuard
 * path instead of the TCP-only HTTP CONNECT proxy.
 *
 * READ-ONLY BY DESIGN. Every probe here is a GET. Nothing consumes a device slot.
 *
 * Safety: WireGuard private keys and any credential-shaped field are REDACTED
 * before printing. Response shape is what we want, never key material.
 *
 *   NODE_PATH=/home/z/rpa/node_modules node probe-connection-modes.js
 */
const fs = require('fs');
const path = require('path');

const BASE = 'https://app.mysteriumvpn.com/api/v1';
const TOKEN_FILE = path.join(process.env.HOME, '.myst-capture', 'token-from-browser.json');

// Field names that mean "do not print this".
const SENSITIVE = /(key|passwo?r?d|secret|token|credential|private|preshared)/i;

function sanitize(v, depth = 0) {
  if (depth > 6) return '<deep>';
  if (Array.isArray(v)) return v.slice(0, 6).map((x) => sanitize(x, depth + 1));
  if (v && typeof v === 'object') {
    const out = {};
    for (const [k, val] of Object.entries(v)) {
      out[k] = SENSITIVE.test(k)
        ? typeof val === 'string'
          ? `<redacted:${val.length} chars>`
          : '<redacted>'
        : sanitize(val, depth + 1);
    }
    return out;
  }
  if (typeof v === 'string') {
    // WireGuard keys are 44-char base64 (32 bytes). Never let one through.
    if (/^[A-Za-z0-9+/]{43}=$/.test(v)) return `<base64key:${v.length}>`;
    if (/^[0-9a-f]{64}$/i.test(v)) return '<hex64>';
    if (v.length > 120) return `<long:${v.length} chars>`;
  }
  return v;
}

(async () => {
  if (!fs.existsSync(TOKEN_FILE)) {
    console.error(`no token at ${TOKEN_FILE} — log in via capture-api.js first.`);
    process.exit(1);
  }
  const raw = JSON.parse(fs.readFileSync(TOKEN_FILE, 'utf8'));
  const token = raw.access_token || raw.accessToken;
  if (!token) {
    console.error('no access_token in captured token file.');
    process.exit(1);
  }

  const get = async (p) => {
    const r = await fetch(BASE + p, {
      headers: { Accept: 'application/json', Authorization: `Bearer ${token}` },
    });
    const text = await r.text();
    let json = null;
    try {
      json = JSON.parse(text);
    } catch {}
    return { status: r.status, json, text };
  };

  // Candidate paths. `/connection/router` is the one the dashboard actually called.
  const PROBES = [
    '/connection/router',
    '/connection/router?ip_type=residential',
    '/connection/router?ip_type=hosting',
    '/connection/config',
    '/connection/wireguard',
    '/connection/modes',
    '/connection',
    '/subscription',
  ];

  for (const p of PROBES) {
    let res;
    try {
      res = await get(p);
    } catch (e) {
      console.log(`GET ${p.padEnd(42)} -> transport error: ${e.message}`);
      continue;
    }
    console.log(`\n=== GET ${p} -> HTTP ${res.status} ===`);
    if (res.json) {
      console.log(JSON.stringify(sanitize(res.json), null, 2));
    } else {
      console.log(res.text.slice(0, 400) || '(empty body)');
    }
  }

  console.log('\n--- read-only probe complete; no device slot consumed ---');
})().catch((e) => {
  console.error('failed:', e.message);
  process.exit(1);
});
