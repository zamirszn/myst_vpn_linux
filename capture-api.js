#!/usr/bin/env node
/**
 * Observe — never capture — the traffic of a browser session you drive yourself.
 *
 * Opens a headed Chromium that YOU log into by hand, so a Cloudflare challenge or
 * a 2FA prompt is yours to solve. It records request METADATA only: method, host,
 * path, resource type, status, and the NAMES of any auth-bearing headers.
 *
 * What it deliberately never writes:
 *   - header VALUES  (cookie, bearer token, ... are recorded as present/absent only)
 *   - query strings  (dropped entirely — that is where a magic-link code would ride)
 *   - long opaque path segments (redacted, in case a token rides in the path)
 *
 * The point is to learn WHICH HOST the API lives on and WHAT the endpoint shapes
 * are. The browser already holds your credentials; the log never does.
 *
 * Usage:
 *   NODE_PATH=/home/z/rpa/node_modules node capture-api.js
 *
 * Output: ~/.myst-capture/requests.ndjson  (append-only, written live)
 * Profile: ~/.myst-capture/profile         (your session cookies live here — delete when done)
 */
const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');

const OUT = path.join(process.env.HOME, '.myst-capture');
const LOG = path.join(OUT, 'requests.ndjson');
const PROFILE = path.join(OUT, 'profile');
const TOKEN_FILE = path.join(OUT, 'token-from-browser.json');

fs.mkdirSync(OUT, { recursive: true });

// Header names whose values are credentials. Recorded as present/absent, never copied.
const SECRET_HEADERS = new Set([
  'authorization', 'cookie', 'set-cookie', 'proxy-authorization',
  'x-api-key', 'x-auth-token', 'x-access-token', 'x-client-secret',
]);

// Drop any path segment long enough to be an opaque token. Keeps UUIDs (~36) readable.
const redactPath = (p) =>
  p
    .split('/')
    .map((seg) => (seg.length > 60 && /^[A-Za-z0-9._~-]+$/.test(seg) ? `<redacted:${seg.length}>` : seg))
    .join('/');

const append = (obj) => {
  try {
    fs.appendFileSync(LOG, JSON.stringify(obj) + '\n');
  } catch {}
};

append({ kind: 'session-start', ts: new Date().toISOString() });

(async () => {
  const ctx = await chromium.launchPersistentContext(PROFILE, {
    headless: false,
    viewport: null,
    args: ['--start-maximized', '--remote-debugging-port=9222'],
  });

  ctx.on('request', (req) => {
    let u;
    try {
      u = new URL(req.url());
    } catch {
      return;
    }
    append({
      ts: new Date().toISOString(),
      kind: 'request',
      method: req.method(),
      host: u.host,
      path: redactPath(u.pathname), // query deliberately dropped
      type: req.resourceType(),
      authHeaders: Object.keys(req.headers())
        .map((h) => h.toLowerCase())
        .filter((h) => SECRET_HEADERS.has(h)), // names only — never values
    });
  });

  ctx.on('response', (res) => {
    const rt = res.request().resourceType();
    if (rt !== 'xhr' && rt !== 'fetch') return;
    let u;
    try {
      u = new URL(res.url());
    } catch {
      return;
    }
    append({
      ts: new Date().toISOString(),
      kind: 'response',
      method: res.request().method(),
      host: u.host,
      path: redactPath(u.pathname),
      status: res.status(),
      type: rt,
    });

    // The token exchange is the one response worth keeping. It is written to a 0600
    // file and NEVER printed or appended to the ndjson — the point is to have a real
    // access token to test Bearer auth with, without it landing in a transcript.
    if (u.host === 'app.mysteriumvpn.com' && u.pathname.endsWith('/oauth/token')) {
      res
        .text()
        .then((body) => {
          fs.writeFileSync(TOKEN_FILE, body, { mode: 0o600 });
          console.log(`  [captured /oauth/token response -> ${TOKEN_FILE}]`);
        })
        .catch(() => {});
    }
  });

  const page = ctx.pages()[0] || (await ctx.newPage());
  console.log(`capture -> ${LOG}`);
  console.log('Log in by hand. Close the browser window when you are done.');

  await page.goto('https://mysteriumvpn.com', { waitUntil: 'domcontentloaded' }).catch(() => {});

  await new Promise((resolve) => ctx.on('close', resolve));
  console.log('browser closed — capture stopped');
  process.exit(0);
})().catch((e) => {
  console.error('capture failed:', e.message);
  process.exit(1);
});
