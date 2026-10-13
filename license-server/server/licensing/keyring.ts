// Loading the signing state at startup (design spec §0.6): the trusted
// keyring from LICENSE_TRUSTED_KEYS_DIR (<kid>.pub files, the format kudurru
// releases embed) and the one signer from LICENSE_SIGNING_KEY_FILE +
// LICENSE_SIGNING_KID (the env form LICENSE_SIGNING_KEY only in dev mode).
//
// Refusals (thrown as Error with one clear sentence; main.ts exits): an
// empty or unreadable trusted dir, a bad .pub file, both key forms set, the
// env form outside dev mode, a key without a kid, a kid that is not in the
// trusted dir, a seed that does not derive the trusted public key, and,
// outside dev mode, a test kid (e2e-test, server-test-*) or a known test
// public key (the golden key, the e2e key). No key configured is not an
// error: the server runs and issuing answers 503 (signingUnavailable).
//
// The seed is read into a buffer, decoded without making a string of it,
// imported non-extractable and zeroed, with the DER buffer around it. The
// env form arrives as a string (Config) that cannot be zeroed, which is one
// more reason it is dev-only. Nothing here logs or returns key material.

import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import type { Config } from '../config';
import type { Logger } from '../log';
import { cleanToken, importPublicKey, importSigningKey, signCompact } from './jws';
import { signingReady, signingUnavailable, type Claims, type Keyring, type Signer, type SigningState } from './types';

/** The kid of the e2e fixture key (internal/license/testkey.go), trusted only by -tags licensetest builds. */
export const E2E_TEST_KID = 'e2e-test';

/** Public keys no production signer may have: the golden test key (server/test/golden) and the e2e fixture key. */
export const TEST_PUBLIC_KEYS: ReadonlySet<string> = new Set([
  // server/test/golden/golden.json publicKey (kid server-test-1): its seed is public.
  '3M2ooVUquWRsKteU3aWtQJkw2PnBXM4FAp08cGXtCsA=',
  // internal/license/testkey.go (kid e2e-test).
  'RbzaQR8cNwJyeKs5uS9TJ7+iUQ0Flu1yge2RgHQlWjs=',
]);

/** The kids of production signing keys (docs/architecture/licensing.md: kudurru-YYYY-N). */
const PRODUCTION_KID = /^kudurru-\d{4}-\d+$/;

/** Whether a kid names a test key (refused outside dev mode). */
export function isTestKid(kid: string): boolean {
  return kid === E2E_TEST_KID || kid.startsWith('server-test-');
}

const B64 = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/';
const B64_VALUE = new Int16Array(128).fill(-1);
for (let i = 0; i < B64.length; i++) B64_VALUE[B64.charCodeAt(i)] = i;

function isAsciiSpace(b: number): boolean {
  return b === 0x20 || b === 0x09 || b === 0x0a || b === 0x0d || b === 0x0b || b === 0x0c;
}

/**
 * Decodes standard padded base64 (Go's base64.StdEncoding) from ASCII
 * bytes, trimming surrounding ASCII whitespace, without creating a string.
 * Returns null on anything else. The caller zeroes input and output.
 */
export function decodeStdBase64Bytes(input: Uint8Array): Uint8Array | null {
  let start = 0;
  let end = input.length;
  while (start < end && isAsciiSpace(input[start]!)) start++;
  while (end > start && isAsciiSpace(input[end - 1]!)) end--;
  const len = end - start;
  if (len === 0 || len % 4 !== 0) return null;
  let pad = 0;
  if (input[end - 1] === 0x3d) pad++;
  if (input[end - 2] === 0x3d) pad++;
  const out = new Uint8Array((len / 4) * 3 - pad);
  let o = 0;
  for (let i = start; i < end; i += 4) {
    const last = i + 4 === end;
    const v = [0, 0, 0, 0];
    for (let j = 0; j < 4; j++) {
      const b = input[i + j]!;
      if (b === 0x3d && last && j >= 4 - pad) {
        v[j] = 0;
        continue;
      }
      const x = b < 128 ? B64_VALUE[b]! : -1;
      if (x < 0) {
        out.fill(0);
        return null;
      }
      v[j] = x;
    }
    const n = (v[0]! << 18) | (v[1]! << 12) | (v[2]! << 6) | v[3]!;
    v.fill(0);
    out[o++] = (n >> 16) & 0xff;
    if (o < out.length) out[o++] = (n >> 8) & 0xff;
    if (o < out.length) out[o++] = n & 0xff;
  }
  return out;
}

/** A public key as license.ParsePublicKey reads it (whitespace removed, std base64, 32 bytes), re-encoded canonically; null when invalid. */
export function parsePublicKey(text: string): string | null {
  const cleaned = cleanToken(text);
  if (!/^[A-Za-z0-9+/]*={0,2}$/.test(cleaned) || cleaned.length % 4 !== 0) return null;
  const raw = Buffer.from(cleaned, 'base64');
  if (raw.byteLength !== 32 || raw.toString('base64') !== cleaned) return null;
  return cleaned;
}

/**
 * Reads the trusted keyring: every <kid>.pub file of `dir`. Throws when the
 * directory cannot be read, holds no .pub file, or a file is not a 32-byte
 * public key in base64.
 */
export function readKeyring(dir: string): Keyring {
  let names: string[];
  try {
    names = readdirSync(dir, { withFileTypes: true })
      .filter((e) => !e.isDirectory() && e.name.endsWith('.pub'))
      .map((e) => e.name)
      .sort();
  } catch (e) {
    throw new Error(`LICENSE_TRUSTED_KEYS_DIR: cannot read ${dir}: ${(e as Error).message}`, { cause: e });
  }
  const ring = new Map<string, string>();
  for (const name of names) {
    const kid = name.slice(0, -'.pub'.length);
    if (kid === '') continue;
    let text: string;
    try {
      text = readFileSync(join(dir, name), 'utf8');
    } catch (e) {
      throw new Error(`LICENSE_TRUSTED_KEYS_DIR: cannot read ${name}: ${(e as Error).message}`, { cause: e });
    }
    const pub = parsePublicKey(text);
    if (!pub) throw new Error(`LICENSE_TRUSTED_KEYS_DIR: ${name} is not an Ed25519 public key in base64 (the format of internal/license/keys)`);
    ring.set(kid, pub);
  }
  if (ring.size === 0) throw new Error(`LICENSE_TRUSTED_KEYS_DIR: no <kid>.pub files in ${dir}; point it at the public keys kudurru releases embed (internal/license/keys)`);
  return ring;
}

/** Reads the seed bytes from the configured source (file or, in dev mode, the env value). */
function readSeedSource(config: Config): Uint8Array {
  if (config.signing.keyFile) {
    try {
      return readFileSync(config.signing.keyFile);
    } catch (e) {
      throw new Error(`LICENSE_SIGNING_KEY_FILE: cannot read ${config.signing.keyFile}: ${(e as NodeJS.ErrnoException).code ?? 'error'}`, { cause: e });
    }
  }
  return Buffer.from(config.signing.keyEnv ?? '', 'utf8');
}

const PROBE = new TextEncoder().encode('kudurru license portal: signing key probe');

/**
 * The signing state of the process (AppDeps.signing). Throws Error with a
 * clear message on every refusal of spec §0.6; returns signingUnavailable
 * ('no signing key is configured', keyring) when no key is set.
 */
export async function loadSigning(config: Config, log: Logger): Promise<SigningState> {
  const keyring = readKeyring(config.signing.trustedKeysDir);
  const { keyFile, keyEnv, kid, trustedSince } = config.signing;
  const dir = config.signing.trustedKeysDir;

  if (keyFile && keyEnv) throw new Error('LICENSE_SIGNING_KEY_FILE and LICENSE_SIGNING_KEY are both set; set only LICENSE_SIGNING_KEY_FILE');
  if (keyEnv && !config.devMode) throw new Error('LICENSE_SIGNING_KEY is accepted only with LICENSE_SERVER_DEV=true; mount the key as a file and set LICENSE_SIGNING_KEY_FILE');
  if (!keyFile && !keyEnv) {
    if (kid) log.warn('LICENSE_SIGNING_KID is set but no signing key is configured', { kid });
    return signingUnavailable('no signing key is configured', keyring);
  }
  if (!kid) throw new Error(`LICENSE_SIGNING_KID is required with ${keyFile ? 'LICENSE_SIGNING_KEY_FILE' : 'LICENSE_SIGNING_KEY'}: the name of its .pub file in ${dir}`);
  const trusted = keyring.get(kid);
  if (trusted === undefined) {
    throw new Error(`the signing kid ${JSON.stringify(kid)} is not in LICENSE_TRUSTED_KEYS_DIR (${dir}): no kudurru release would accept its keys; trusted kids: ${[...keyring.keys()].sort().join(', ')}`);
  }
  if (!config.devMode) {
    if (isTestKid(kid)) throw new Error(`the signing kid ${JSON.stringify(kid)} is a test key; it is accepted only with LICENSE_SERVER_DEV=true`);
    if (TEST_PUBLIC_KEYS.has(trusted)) throw new Error(`the public key of ${kid}.pub is a published test key; it is accepted only with LICENSE_SERVER_DEV=true`);
  }

  const source = readSeedSource(config);
  const seed = decodeStdBase64Bytes(source);
  source.fill(0);
  if (!seed || seed.byteLength !== 32) {
    seed?.fill(0);
    throw new Error(`${keyFile ? 'LICENSE_SIGNING_KEY_FILE' : 'LICENSE_SIGNING_KEY'} does not hold an Ed25519 seed (standard base64 of 32 bytes, the format hack/licensegen writes)`);
  }
  let key: CryptoKey;
  try {
    key = await importSigningKey(seed);
  } finally {
    seed.fill(0);
  }

  // The key is not extractable, so prove the pairing by signing a probe and
  // verifying it with the trusted public key.
  const probe = new Uint8Array(await crypto.subtle.sign('Ed25519', key, PROBE));
  const matches = await crypto.subtle.verify('Ed25519', await importPublicKey(trusted), probe, PROBE);
  if (!matches) throw new Error(`the signing key does not match ${kid}.pub in LICENSE_TRUSTED_KEYS_DIR (${dir}): check LICENSE_SIGNING_KID and the key file`);

  const testKey = isTestKid(kid) || TEST_PUBLIC_KEYS.has(trusted) || (config.devMode && !PRODUCTION_KID.test(kid));
  const signer: Signer = {
    kid,
    publicKey: trusted,
    ...(trustedSince ? { trustedSince } : {}),
    testKey,
    sign: (claims: Claims) => signCompact(key, kid, claims),
  };
  log.info('signing key loaded', { kid, testKey, trustedSince: trustedSince ?? null, source: keyFile ? 'file' : 'env' });
  return signingReady(signer, keyring);
}
