#!/usr/bin/env node
/**
 * Mysterium VPN 2.0 — WireGuard connect via the real client endpoint.
 *
 *   POST /api/v1/connection/connect   { public_key, os_type, ... }
 *     -> { id, wg_config, hash, exit_ip, ... }
 *
 * `wg_config` is a template containing %private_key%. The server NEVER sees the
 * private key: we generate the keypair locally, send only the public half, and
 * substitute the private half client-side. This mirrors what the official Flutter
 * client does (mysterium-vpn/lib/repositories/vpn/wireguard_repository.dart:78):
 *
 *     final replaced = config.replaceFirst('%private_key%', key.privateKey);
 *
 * This is the endpoint to build on. /connection/router — used earlier — is a
 * dashboard-only shortcut capped at one config per account and is absent from the
 * public generated client.
 *
 * SIDE EFFECT: provisions a real tunnel and consumes a device slot.
 *
 *   NODE_PATH=/home/z/rpa/node_modules node myst-connect.js [country] [ip_type]
 */
const fs = require('fs');
const path = require('path');
const crypto = require('crypto');

const API = process.env.MYST_API || 'https://app.mysteriumvpn.com/api/v1';
const OUT = path.join(process.env.HOME, '.myst-capture');
const TOKEN_FILE = path.join(OUT, 'token-from-browser.json');
const KEY_FILE = path.join(OUT, 'wg-keypair.json');
const CONF_FILE = path.join(OUT, 'myst0.conf');
const RESP_FILE = path.join(OUT, 'connect-response.json');

const COUNTRY = process.argv[2] || 'GB';
const IP_TYPE = process.argv[3] || 'hosting';

// Curve25519 (X25519) keypair in WireGuard's base64 encoding.
function wgKeypair() {
  const { publicKey, privateKey } = crypto.generateKeyPairSync('x25519');
  const pub = publicKey.export({ type: 'spki', format: 'der' }).slice(-32);
  const priv = Buffer.from(privateKey.export({ type: 'pkcs8', format: 'der' }).slice(-32));
  // WireGuard clamps the private scalar. X25519 clamps identically on use, so this
  // is the canonical on-disk form and derives the same public key we transmitted.
  priv[0] &= 248;
  priv[31] &= 127;
  priv[31] |= 64;
  return { pub: pub.toString('base64'), priv: priv.toString('base64') };
}

const mask = (v) => (typeof v === 'string' && v.length ? `<${v.length} chars>` : v);

(async () => {
  const token = JSON.parse(fs.readFileSync(TOKEN_FILE, 'utf8'));
  const bearer = token.access_token || token.accessToken;
  if (!bearer) throw new Error('no access_token in captured token file');

  // ---- 1. local keypair -----------------------------------------------------
  console.log('== 1. keypair (generated locally) ==');
  const kp = wgKeypair();
  fs.writeFileSync(KEY_FILE, JSON.stringify(kp, null, 2), { mode: 0o600 });
  console.log(`  public_key  : ${kp.pub}`);
  console.log(`  private_key : ${mask(kp.priv)}  -> ${KEY_FILE} [0600]`);
  console.log('  the private key is never transmitted.\n');

  // ---- 2. connect -----------------------------------------------------------
  console.log(`== 2. POST ${API}/connection/connect ==`);
  const body = { public_key: kp.pub, os_type: 'linux', ip_type: IP_TYPE, country: COUNTRY };
  console.log(`  request: ${JSON.stringify({ ...body, public_key: kp.pub.slice(0, 12) + '...' })}`);

  const r = await fetch(`${API}/connection/connect`, {
    method: 'POST',
    headers: {
      Accept: 'application/json',
      'Content-Type': 'application/json',
      Authorization: `Bearer ${bearer}`,
    },
    body: JSON.stringify(body),
  });
  const text = await r.text();
  console.log(`  -> HTTP ${r.status}\n`);

  if (r.status !== 200) {
    console.log(text.slice(0, 1200));
    process.exit(1);
  }

  const json = JSON.parse(text);
  fs.writeFileSync(RESP_FILE, JSON.stringify(json, null, 2), { mode: 0o600 });

  console.log('== 3. response ==');
  for (const k of ['id', 'hash', 'exit_ip', 'ip_type', 'country', 'city', 'limit_exceeded']) {
    if (json[k] !== undefined) console.log(`  ${k.padEnd(15)}: ${json[k]}`);
  }
  console.log(`  wg_config      : ${json.wg_config ? `${json.wg_config.length} chars` : '(ABSENT)'}`);
  console.log(`  full response  -> ${RESP_FILE} [0600]\n`);

  // ---- 4. assemble ----------------------------------------------------------
  console.log('== 4. assemble config ==');
  if (!json.wg_config) {
    console.log('  no wg_config in response — cannot assemble.');
    process.exit(1);
  }
  if (!json.wg_config.includes('%private_key%')) {
    console.log('  WARNING: no %private_key% placeholder — server may have generated the key.');
    console.log('  The transmitted public_key would then not match. Inspect the saved response.');
  } else {
    console.log('  found %private_key% placeholder — substituting locally');
  }
  const conf = json.wg_config.replace('%private_key%', kp.priv);
  fs.writeFileSync(CONF_FILE, conf, { mode: 0o600 });

  console.log(`  wrote ${CONF_FILE} [0600]\n`);

  // ---- 5. verdict -----------------------------------------------------------
  const has = (re) => (re.test(conf) ? 'yes' : 'no');
  console.log('== 5. config check ==');
  console.log(`  [Interface]      : ${has(/\[Interface\]/i)}`);
  console.log(`  [Peer]           : ${has(/\[Peer\]/i)}`);
  console.log(`  PrivateKey set   : ${has(/^PrivateKey\s*=\s*\S/m)}`);
  console.log(`  Endpoint         : ${has(/^Endpoint\s*=/m)}`);
  console.log(`  PersistentKeepalive: ${has(/^PersistentKeepalive\s*=/m)}`);
  console.log(`  IPv6 routed (::/0): ${has(/::\/0/)}`);
  console.log('\n  masked config:');
  console.log(
    conf
      .replace(/^(\s*(?:PrivateKey|PresharedKey)\s*=\s*).*$/gim, '$1<redacted>')
      .replace(/^(\s*AllowedIPs\s*=\s*).{60}.*$/gim, '$1<...long CIDR list...>')
      .split('\n')
      .map((l) => '    ' + l)
      .join('\n')
  );

  console.log('\n  next:  sudo ./wg-test.sh ~/.myst-capture/myst0.conf');
  console.log('  (add WG_ADD_IPV6=1 to close the IPv6 gap)');
  console.log(`\n  disconnect: GET ${API}/connection/disconnect`);
})().catch((e) => {
  console.error('failed:', e.message);
  process.exit(1);
});
