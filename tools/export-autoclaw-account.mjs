#!/usr/bin/env node
// Export the currently logged-in AutoClaw desktop account as an autoclaw2api
// account JSON. Windows only: the vault key is protected with DPAPI.
//
//   node tools/export-autoclaw-account.mjs
//   node tools/export-autoclaw-account.mjs --out test-accounts/default.json
//   node tools/export-autoclaw-account.mjs --user-data "C:\\Users\\me\\AppData\\Roaming\\AutoClaw-oversea-official"
//
// The script never prints the tokens unless --print is passed.

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import crypto from 'node:crypto';
import { execFileSync } from 'node:child_process';

function arg(name, fallback = '') {
  const i = process.argv.indexOf(name);
  return i >= 0 && process.argv[i + 1] ? process.argv[i + 1] : fallback;
}
const flag = (name) => process.argv.includes(name);

const userData = arg('--user-data', path.join(process.env.APPDATA || path.join(os.homedir(), 'AppData', 'Roaming'), 'AutoClaw-oversea-official'));
const accountsRoot = path.join(userData, 'accounts');
const outPath = arg('--out', '');
const alias = arg('--alias', 'default');
const accountKeyArg = arg('--account', '');

if (!fs.existsSync(accountsRoot)) {
  console.error(`AutoClaw accounts directory not found: ${accountsRoot}`);
  process.exit(2);
}

const candidates = fs.readdirSync(accountsRoot, { withFileTypes: true })
  .filter((e) => e.isDirectory())
  .map((e) => {
    const dir = path.join(accountsRoot, e.name);
    const vault = path.join(dir, 'account-credentials.enc');
    let mtime = 0;
    try { mtime = fs.statSync(vault).mtimeMs; } catch {}
    return { key: e.name, dir, vault, mtime };
  })
  .filter((e) => fs.existsSync(e.vault))
  .sort((a, b) => b.mtime - a.mtime);

if (candidates.length === 0) {
  console.error('No account-credentials.enc found. Log in to AutoClaw first.');
  process.exit(2);
}
const account = accountKeyArg ? candidates.find((c) => c.key === accountKeyArg) : candidates[0];
if (!account) {
  console.error(`Account not found: ${accountKeyArg}`);
  process.exit(2);
}

const localState = path.join(userData, 'Local State');
if (!fs.existsSync(localState)) {
  console.error(`Local State not found: ${localState}`);
  process.exit(2);
}

// DPAPI-unwrap the Chromium OSCrypt key with the current Windows user.
const ps = `
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Security
$ls = Get-Content -Raw -LiteralPath $env:AC_LOCAL_STATE | ConvertFrom-Json
$raw = [Convert]::FromBase64String($ls.os_crypt.encrypted_key)
if ($raw.Length -le 5) { throw 'encrypted_key too short' }
$key = [Security.Cryptography.ProtectedData]::Unprotect($raw[5..($raw.Length-1)], $null, 'CurrentUser')
($key | ForEach-Object { $_.ToString('x2') }) -join ''
`;
let keyHex;
try {
  keyHex = execFileSync('powershell.exe', ['-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-Command', ps], {
    env: { ...process.env, AC_LOCAL_STATE: localState },
    encoding: 'utf8',
    windowsHide: true,
  }).trim();
} catch (err) {
  console.error('DPAPI unwrap failed. Run this on the same Windows user that AutoClaw uses.');
  console.error(String(err.stderr || err.message || err).trim());
  process.exit(3);
}
if (!/^[0-9a-f]{64}$/i.test(keyHex)) {
  console.error(`Unexpected key length: ${keyHex.length}`);
  process.exit(3);
}
const key = Buffer.from(keyHex, 'hex');

const blob = fs.readFileSync(account.vault);
if (blob.subarray(0, 3).toString() !== 'v10') {
  console.error('Unsupported vault format (expected v10 prefix).');
  process.exit(3);
}
const rest = blob.subarray(3);
if (rest.length <= 12 + 16) {
  console.error('Vault is too short.');
  process.exit(3);
}
let plaintext;
try {
  const decipher = crypto.createDecipheriv('aes-256-gcm', key, rest.subarray(0, 12));
  decipher.setAuthTag(rest.subarray(rest.length - 16));
  plaintext = Buffer.concat([decipher.update(rest.subarray(12, rest.length - 16)), decipher.final()]).toString('utf8');
} catch (err) {
  console.error('Vault decrypt failed:', err.message);
  process.exit(3);
}

let vault;
try { vault = JSON.parse(plaintext); } catch {
  console.error('Decrypted vault is not JSON.');
  process.exit(3);
}
const accessToken = String(vault.accessToken || '').replace(/^Bearer\s+/i, '').trim();
const refreshToken = String(vault.refreshToken || '').replace(/^Bearer\s+/i, '').trim();
if (!accessToken) {
  console.error('Vault does not contain an accessToken.');
  process.exit(3);
}

let deviceId = '';
try {
  deviceId = JSON.parse(fs.readFileSync(path.join(userData, 'device', 'device-id.json'), 'utf8')).deviceId || '';
} catch {}
let profile = null;
try { profile = JSON.parse(fs.readFileSync(path.join(account.dir, 'account-profile.json'), 'utf8')).profile || null; } catch {}

let claims = {};
try {
  const payload = accessToken.split('.')[1];
  claims = JSON.parse(Buffer.from(payload, 'base64url').toString('utf8'));
} catch {}

const record = {
  type: 'autoclaw2api',
  alias,
  access_token: accessToken,
  refresh_token: refreshToken,
  device_id: deviceId || String(claims.device_id || ''),
  user_id: claims.user_id != null ? String(claims.user_id) : '',
  source: 'auto-export',
};
if (profile && profile.email) record.label = profile.email;

const text = JSON.stringify(record, null, 2) + '\n';
if (outPath) {
  fs.mkdirSync(path.dirname(outPath), { recursive: true });
  fs.writeFileSync(outPath, text, { mode: 0o600 });
  console.log(`wrote ${outPath} (account ${account.key}, alias ${alias})`);
} else if (flag('--print')) {
  process.stdout.write(text);
} else {
  console.log(`account ${account.key}, alias ${alias}`);
  console.log('Use --out <path> to write the auth JSON, or --print to stdout.');
}
