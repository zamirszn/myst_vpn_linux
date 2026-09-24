#!/usr/bin/env node
/**
 * Test whether the Mysterium consumer API accepts a BEARER token — the question that
 * decides whether a standalone Linux client is buildable at all.
 *
 * Reads the access token captured from a browser login (capture-api.js writes it to a
 * 0600 file) and replays it as `Authorization: Bearer ...`. The token itself is never
 * printed; only status codes are, and any proxy credentials go to a 0600 file.
 *
 *   NODE_PATH=/home/z/rpa/node_modules node test-bearer.js
 *   MYST_CONNECT=1 NODE_PATH=... node test-bearer.js    # also fire connect-proxy
 *
 * connect-proxy is opt-in because it consumes a connection slot on your plan.
 */
const fs = require('fs');
const path = require('path');

const BASE = 'https://app.mysteriumvpn.com/api/v1';
const OUT = path.join(process.env.HOME, '.myst-capture');
const TOKEN_FILE = path.join(OUT, 'token-from-browser.json');
const PROXY_FILE = path.join(OUT, 'proxy-config.json');

const mask = (v) => (typeof v === 'string' && v.length ? `<${v.length} chars>` : v);

(async () => {
  if (!fs.existsSync(TOKEN_FILE)) {
    console.error(`no token at ${TOKEN_FILE}`);
    console.error('run capture-api.js and log in with Google first.');
    process.exit(1);
  }

  let raw;
  try {
    raw = JSON.parse(fs.readFileSync(TOKEN_FILE, 'utf8'));
  } catch (e) {
    console.error(`could not parse ${TOKEN_FILE}: ${e.message}`);
    process.exit(1);
  }

  const token = raw.access_token || raw.accessToken;
  if (!token) {
    // Useful diagnosis: maybe the web app gets a cookie instead of a token body.
    console.log('no access_token in the captured response. Shape was:');
    console.log(`  keys: ${Object.keys(raw).join(', ')}`);
    console.log('If it names a cookie instead, the web app does not use bearer at all.');
    process.exit(2);
  }

  console.log(`token      : ${mask(token)}  (${raw.token_type || raw.tokenType || '?'})`);
  console.log(`user_id    : ${raw.user_id || raw.userId || '(absent)'}`);
  console.log(`expires_in : ${raw.expires_in ?? raw.expiresIn ?? '(absent)'}`);
  console.log(`has refresh: ${Boolean(raw.refresh_token || raw.refreshToken)}\n`);

  const call = async (method, p, body) => {
    const r = await fetch(BASE + p, {
      method,
      headers: {
        Accept: 'application/json',
        Authorization: `Bearer ${token}`,
        ...(body ? { 'Content-Type': 'application/json' } : {}),
      },
      body: body ? JSON.stringify(body) : undefined,
    });
    const text = await r.text();
    let json = null;
    try {
      json = JSON.parse(text);
    } catch {}
    return { status: r.status, json, text };
  };

  // ---- the decisive test --------------------------------------------------
  const check = await call('GET', '/auth/check');
  console.log(`GET /auth/check          -> HTTP ${check.status}`);

  if (check.status !== 200) {
    console.log('\n\x1b[31mBearer REJECTED.\x1b[0m body:');
    console.log(check.text.slice(0, 600));
    console.log(
      '\nIf this is a 401 with no attestation language, bearer may simply not be the\n' +
        'web app\'s mechanism. Worth retrying with a token from the magic-link flow.'
    );
    process.exit(3);
  }

  console.log('\x1b[32mBearer auth WORKS.\x1b[0m A standalone client can authenticate without a browser.');

  const cfg = await call('GET', '/connection/config?ip_type=residential');
  console.log(`GET /connection/config   -> HTTP ${cfg.status}`);

  if (process.env.MYST_CONNECT !== '1') {
    console.log('\n(read-only stops here — set MYST_CONNECT=1 to also fire connect-proxy,');
    console.log(' which consumes a connection slot on your plan)');
    return;
  }

  console.log('\nPOST /connection/connect-proxy ...');
  const proxy = await call('POST', '/connection/connect-proxy', {
    ip_type: 'residential',
    reset_connection: false,
    os_type: 'linux',
  });
  console.log(`POST /connection/connect-proxy -> HTTP ${proxy.status}`);

  if (proxy.status === 200 && proxy.json) {
    const c = proxy.json.proxyConfig || proxy.json.proxy_config || {};
    fs.writeFileSync(PROXY_FILE, JSON.stringify(proxy.json, null, 2), { mode: 0o600 });

    console.log('\n\x1b[32mSUCCESS — proxy mode works over bearer auth.\x1b[0m');
    console.log(`  limit_exceeded : ${proxy.json.limitExceeded ?? proxy.json.limit_exceeded ?? '(absent)'}`);
    console.log(`  host           : ${c.host}`);
    console.log(`  port           : ${c.port}`);
    console.log(`  username       : ${mask(c.username)}`);
    console.log(`  password       : ${mask(c.password)}`);
    console.log(`  expires_at     : ${c.expiresAt ?? c.expires_at}`);
    console.log(`\n  full response -> ${PROXY_FILE}  [mode 0600]`);
    console.log('  cleanup: GET /connection/disconnect');
  } else {
    console.log('body:', proxy.text.slice(0, 900));
  }
})().catch((e) => {
  console.error('failed:', e.message);
  process.exit(1);
});
