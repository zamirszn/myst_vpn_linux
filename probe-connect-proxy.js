#!/usr/bin/env node
/**
 * Probe /connection/connect-proxy from INSIDE your already-logged-in browser session.
 *
 * Attaches to the running capture browser over CDP and issues the requests from the
 * page context, so the browser's own session (cookie or stored bearer token) does the
 * authenticating. Nothing has to be copied, exported, or pasted.
 *
 * The token, if one is found in page storage, never leaves the page. Proxy credentials
 * returned on success are written to a 0600 file and NEVER printed — stdout gets only a
 * masked summary, so the transcript stays clean.
 *
 * Usage:
 *   NODE_PATH=/home/z/rpa/node_modules node probe-connect-proxy.js
 */
const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');

const BASE = 'https://app.mysteriumvpn.com/api/v1';
const OUT = path.join(process.env.HOME, '.myst-capture');
const SECRET_OUT = path.join(OUT, 'proxy-config.json');

fs.mkdirSync(OUT, { recursive: true });

const mask = (v) => (typeof v === 'string' && v.length ? `<${v.length} chars>` : v);

(async () => {
  const browser = await chromium.connectOverCDP('http://127.0.0.1:9222');
  const ctx = browser.contexts()[0];
  if (!ctx) throw new Error('no browser context — is capture-api.js running?');

  // Prefer a page ALREADY on the app origin. Navigating would reload the SPA and, if it
  // holds its access token in memory only, discard a live session — which is exactly how
  // the first attempt logged itself out.
  let page = ctx.pages().find((p) => p.url().startsWith('https://app.mysteriumvpn.com'));
  if (!page) {
    page = ctx.pages().find((p) => p.url().includes('mysteriumvpn.com')) || (await ctx.newPage());
    await page.goto('https://app.mysteriumvpn.com/dashboard', { waitUntil: 'domcontentloaded' });
    await page.waitForTimeout(2000);
  }

  if (page.url().includes('/login')) {
    console.log('page is sitting on /login — log in first, then re-run this probe.');
    process.exit(3);
  }

  const result = await page.evaluate(async (base) => {
    // JWTs are base64url JSON — they always begin "eyJ". The earlier, looser
    // "three dot-separated parts" test matched an Intercom config blob.
    const looksJwt = (v) => typeof v === 'string' && v.startsWith('eyJ') && v.length > 40;

    const findToken = () => {
      const stores = [
        ['localStorage', localStorage],
        ['sessionStorage', sessionStorage],
      ];
      for (const [name, store] of stores) {
        for (let i = 0; i < store.length; i++) {
          const k = store.key(i);
          const v = store.getItem(k);
          if (!v) continue;
          if (looksJwt(v)) return { where: name, key: k, token: v };
          try {
            for (const [jk, jv] of Object.entries(JSON.parse(v) || {})) {
              if (looksJwt(jv)) return { where: `${name}:json`, key: `${k}.${jk}`, token: jv };
            }
          } catch {}
        }
      }
      return null;
    };

    const call = async (method, p, body, token) => {
      const headers = {};
      if (body) headers['Content-Type'] = 'application/json';
      if (token) headers.Authorization = `Bearer ${token}`;
      const r = await fetch(base + p, {
        method,
        credentials: 'include',
        headers,
        body: body ? JSON.stringify(body) : undefined,
      });
      const text = await r.text();
      let json = null;
      try {
        json = JSON.parse(text);
      } catch {}
      return { status: r.status, json, text: text.slice(0, 4000) };
    };

    const found = findToken();
    let token = found ? found.token : null;

    // Preference order: explicit bearer if the app stores one, else the plain cookie.
    let check = await call('GET', '/auth/check', null, token);
    let mode = token ? `bearer (${found.where}, key "${found.key}")` : 'cookie';

    if ((check.status === 401 || check.status === 403) && token) {
      token = null;
      check = await call('GET', '/auth/check', null, null);
      mode = 'cookie';
    }

    const authed = check.status === 200;
    const out = { mode, authed, check, storage: found ? { where: found.where, key: found.key } : null };

    if (!authed) return out;

    out.cfg = await call('GET', '/connection/config?ip_type=residential', null, token);
    out.proxy = await call(
      'POST',
      '/connection/connect-proxy',
      { ip_type: 'residential', reset_connection: false, os_type: 'linux' },
      token
    );
    return out;
  }, BASE);

  // ---- report -------------------------------------------------------------
  console.log(`auth mode : ${result.mode}`);
  if (result.storage) console.log(`token in  : ${result.storage.where} ["${result.storage.key}"]`);
  console.log(`auth/check: HTTP ${result.check.status}`);

  if (!result.authed) {
    console.log('\nNot authenticated inside the page — is the dashboard tab still logged in?');
    console.log('body:', result.check.text.slice(0, 500));
    process.exit(2);
  }

  console.log(`connection/config   : HTTP ${result.cfg.status}`);
  console.log(`connection/connect-proxy: HTTP ${result.proxy.status}`);

  const p = result.proxy;
  if (p.status === 200 && p.json) {
    const cfg = p.json.proxyConfig || p.json.proxy_config || {};
    const full = { ...p.json };
    fs.writeFileSync(SECRET_OUT, JSON.stringify(full, null, 2), { mode: 0o600 });

    console.log('\n\x1b[32mSUCCESS — connect-proxy works.\x1b[0m');
    console.log(`  limitExceeded : ${p.json.limitExceeded ?? p.json.limit_exceeded ?? '(absent)'}`);
    console.log('  proxyConfig   :');
    console.log(`    host      : ${cfg.host}`);
    console.log(`    port      : ${cfg.port}`);
    console.log(`    username  : ${mask(cfg.username)}`);
    console.log(`    password  : ${mask(cfg.password)}`);
    console.log(`    expiresAt : ${cfg.expiresAt ?? cfg.expires_at}`);
    console.log(`\n  full response (incl. credentials) -> ${SECRET_OUT}  [mode 0600]`);
    console.log('  Cleanup when done: GET /connection/disconnect');
  } else {
    console.log('\n\x1b[33mNot a 200 — body below (error bodies carry no credentials):\x1b[0m');
    console.log(p.text.slice(0, 1500));
  }

  // Drop the CDP socket without closing the browser — leave the capture session up.
  process.exit(0);
})().catch((e) => {
  console.error('probe failed:', e.message);
  process.exit(1);
});
