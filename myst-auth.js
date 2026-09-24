#!/usr/bin/env node
/**
 * Standalone magic-link auth for the Mysterium consumer API — no browser involved.
 *
 * This is the flow a Linux client would actually use, because the web dashboard's
 * cookie auth is a browser-only affordance. Runs entirely locally: your email and
 * your tokens never leave this machine except to the Mysterium API itself, and the
 * tokens are written to a 0600 file — stdout only ever sees statuses.
 *
 * Verified wire contract (recovered from the API's own validation errors, and
 * cross-checked against the OpenAPI-generated Dart client `vpn_api`):
 *
 *   POST /magic-link
 *     { email, client_id, code_challenge, code_challenge_method, continue_to? }
 *     client_id: "web" | "app" | "extension" | "router"
 *     code_challenge_method: "plain" | "S256" | "s256"
 *     -> { code?, first_time? }
 *
 *   POST /oauth/token            grant_type: refresh_token | authorization_code | google | apple
 *     authorization_code: { grant_type, client_id, code_verifier, code }
 *     refresh_token:      { grant_type, client_id, refresh_token }
 *     -> { access_token, token_type, expires_in?, refresh_token?, user_id, first_time? }
 *
 * Usage:
 *   export MYST_EMAIL='you@example.com'
 *   export MYST_CLIENT_ID='app'        # see the enum above
 *   NODE_PATH=/home/z/rpa/node_modules node myst-auth.js
 */
const crypto = require('crypto');
const fs = require('fs');
const http = require('http');
const path = require('path');

const BASE = 'https://app.mysteriumvpn.com/api/v1';
const OUT = path.join(process.env.HOME, '.myst-capture');
const TOKENS = path.join(OUT, 'tokens.json');
const CODE_FILE = path.join(OUT, 'magic-code.txt');
const PORT = Number(process.env.MYST_CALLBACK_PORT || 8123);
const REDIRECT = `http://127.0.0.1:${PORT}/cb`;

const EMAIL = process.env.MYST_EMAIL;
const CLIENT_ID = process.env.MYST_CLIENT_ID || 'app';

if (!EMAIL) {
  console.error("set MYST_EMAIL first (e.g. export MYST_EMAIL='you@example.com')");
  process.exit(1);
}
if (!['web', 'app', 'extension', 'router'].includes(CLIENT_ID)) {
  console.error(`MYST_CLIENT_ID must be one of: web | app | extension | router (got "${CLIENT_ID}")`);
  process.exit(1);
}

fs.mkdirSync(OUT, { recursive: true });

const b64url = (b) => b.toString('base64').replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
const post = async (p, body) => {
  const r = await fetch(BASE + p, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify(body),
  });
  const text = await r.text();
  let json = null;
  try {
    json = JSON.parse(text);
  } catch {}
  return { status: r.status, json, text };
};
const get = async (p, token) => {
  const r = await fetch(BASE + p, {
    headers: { Accept: 'application/json', ...(token ? { Authorization: `Bearer ${token}` } : {}) },
  });
  const text = await r.text();
  let json = null;
  try {
    json = JSON.parse(text);
  } catch {}
  return { status: r.status, json, text };
};

const mask = (v) => (typeof v === 'string' && v.length ? `<${v.length} chars>` : v);

(async () => {
  // ---- 1. PKCE (RFC 7636) -------------------------------------------------
  const verifier = b64url(crypto.randomBytes(48)); // 64 chars, within 43..128
  const challenge = b64url(crypto.createHash('sha256').update(verifier).digest());
  console.log(`PKCE      : S256, verifier ${verifier.length} chars`);

  // ---- 2. request the magic link -----------------------------------------
  const ml = await post('/magic-link', {
    email: EMAIL,
    client_id: CLIENT_ID,
    code_challenge: challenge,
    code_challenge_method: 'S256',
    continue_to: REDIRECT,
  });
  console.log(`POST /magic-link -> HTTP ${ml.status}  (client_id="${CLIENT_ID}")`);

  let code = ml.json && (ml.json.code || ml.json.magic_code) ? ml.json.code || ml.json.magic_code : null;

  if (!code && ml.status !== 200 && ml.status !== 201 && ml.status !== 204) {
    console.log('body:', ml.text.slice(0, 600));
    process.exit(2);
  }

  if (code) {
    console.log('  the API returned a code directly — skipping the email round-trip');
  } else {
    // ---- 3. wait for the emailed link to land on our local callback ------
    console.log(`\n  Check ${EMAIL} and click the link.`);
    console.log(`  Waiting on ${REDIRECT} (or write the link into ${CODE_FILE}) ...\n`);

    try {
      fs.unlinkSync(CODE_FILE);
    } catch {}

    code = await new Promise((resolve) => {
      const server = http.createServer((req, res) => {
        const u = new URL(req.url, `http://127.0.0.1:${PORT}`);
        const params = Object.fromEntries(u.searchParams);

        // Log the SHAPE of whatever comes back rather than assuming it carries ?code=.
        // The exact parameter name is the thing we are trying to learn.
        const shown = Object.entries(params)
          .map(([k, v]) => `${k}=${/code|token|secret/i.test(k) ? `<${v.length} chars>` : v}`)
          .join('  ');
        console.log(`\n  callback hit: path=${u.pathname}  ${shown || '(no query params)'}`);

        // If no code came back, a session cookie may have been set instead — meaning
        // the next step is a call to /oauth/authorize rather than a direct exchange.
        const cookieNames = (req.headers.cookie || '')
          .split(';')
          .map((c) => c.split('=')[0].trim())
          .filter(Boolean);
        if (cookieNames.length) console.log(`  cookies received: ${cookieNames.join(', ')}`);

        const c = params.code || params.authorization_code || params.activation_code || null;

        res.writeHead(c ? 200 : 202, { 'Content-Type': 'text/html' });
        res.end(
          c
            ? '<h2>Captured. You can close this tab.</h2>'
            : '<h2>Reached the local callback, but no code was present. Check the terminal.</h2>'
        );

        if (c) {
          server.close();
          resolve(c);
        }
      });
      server.listen(PORT, '127.0.0.1');

      // Fallback: the backend may not allow an arbitrary continue_to, in which
      // case you paste the emailed link into CODE_FILE and we lift the code out.
      const poll = setInterval(() => {
        let raw;
        try {
          raw = fs.readFileSync(CODE_FILE, 'utf8').trim();
        } catch {
          return;
        }
        if (!raw) return;
        const m = raw.match(/[?&]code=([^&\s]+)/);
        const found = m ? decodeURIComponent(m[1]) : raw;
        clearInterval(poll);
        server.close();
        resolve(found);
      }, 1000);

      setTimeout(
        () => {
          clearInterval(poll);
          server.close();
          resolve(null);
        },
        10 * 60 * 1000
      );
    });
  }

  if (!code) {
    console.log('timed out waiting for the code');
    process.exit(3);
  }
  console.log(`code      : ${mask(code)}`);

  // ---- 4. exchange it (grant_type=authorization_code) ---------------------
  const tok = await post('/oauth/token', {
    grant_type: 'authorization_code',
    client_id: CLIENT_ID,
    code_verifier: verifier,
    code,
  });
  console.log(`POST /oauth/token -> HTTP ${tok.status}`);

  if (tok.status !== 200 || !tok.json || !tok.json.access_token) {
    console.log('body:', tok.text.slice(0, 700));
    process.exit(4);
  }

  const t = tok.json;
  fs.writeFileSync(TOKENS, JSON.stringify(t, null, 2), { mode: 0o600 });
  console.log('\n\x1b[32mGot a token.\x1b[0m');
  console.log(`  token_type   : ${t.token_type}`);
  console.log(`  expires_in   : ${t.expires_in ?? '(absent)'}`);
  console.log(`  user_id      : ${t.user_id}`);
  console.log(`  access_token : ${mask(t.access_token)}`);
  console.log(`  refresh_token: ${t.refresh_token ? mask(t.refresh_token) : '(none)'}`);
  console.log(`  saved to     : ${TOKENS}  [mode 0600]`);

  // ---- 5. THE KEY TEST: does the API accept a Bearer token? --------------
  const check = await get('/auth/check', t.access_token);
  console.log(`\nGET /auth/check with Bearer -> HTTP ${check.status}`);
  if (check.status === 200) {
    console.log('\x1b[32mBearer auth WORKS — a standalone Linux client can authenticate.\x1b[0m');
    const cfg = await get('/connection/config?ip_type=residential', t.access_token);
    console.log(`GET /connection/config       -> HTTP ${cfg.status}`);
  } else {
    console.log('Bearer rejected. body:', check.text.slice(0, 400));
  }
})().catch((e) => {
  console.error('failed:', e.message);
  process.exit(1);
});
