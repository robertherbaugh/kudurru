// Loading the signer and the trusted keyring (design spec §0.6, §10
// "keyring/config"): the file and dev-only env forms, every startup refusal
// (untrusted kid, mismatched seed, test kids and test keys outside dev
// mode, empty or bad trusted dirs), no key configured = signing
// unavailable, and a loaded signer that signs what Go would.
import { describe, expect, test } from 'bun:test';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { decodeStdBase64Bytes, loadSigning, parsePublicKey, readKeyring } from '../licensing/keyring';
import { verify } from '../licensing/jws';
import type { Claims } from '../licensing/types';
import { MemoryLogger } from '../log';
import { GOLDEN, GOLDEN_VECTORS, goldenTrustedKeysDir, testConfig } from './fixtures';

interface KeyPair {
  seed: string;
  publicKey: string;
}

async function newKeyPair(): Promise<KeyPair> {
  const pair = (await crypto.subtle.generateKey('Ed25519', true, ['sign', 'verify'])) as CryptoKeyPair;
  const pkcs8 = new Uint8Array(await crypto.subtle.exportKey('pkcs8', pair.privateKey));
  const raw = new Uint8Array(await crypto.subtle.exportKey('raw', pair.publicKey));
  return { seed: Buffer.from(pkcs8.slice(-32)).toString('base64'), publicKey: Buffer.from(raw).toString('base64') };
}

/** A trusted keys dir with these <kid>.pub files. */
function trustedDir(files: Record<string, string>): string {
  const dir = mkdtempSync(join(tmpdir(), 'kls-keyring-'));
  for (const [name, content] of Object.entries(files)) writeFileSync(join(dir, name), content);
  return dir;
}

/** A licensegen-format key file (base64 seed and a newline). */
function keyFile(seed: string): string {
  const dir = mkdtempSync(join(tmpdir(), 'kls-key-'));
  const path = join(dir, 'signing.key');
  writeFileSync(path, `${seed}\n`, { mode: 0o600 });
  return path;
}

function signingConfig(signing: Partial<ReturnType<typeof testConfig>['signing']>, devMode = true): ReturnType<typeof testConfig> {
  return testConfig({ devMode, signing: { keyFile: null, keyEnv: null, kid: null, trustedSince: null, ...signing } });
}

describe('loadSigning', () => {
  test('dev mode, env form, golden key: signs byte for byte what Go signs', async () => {
    const log = new MemoryLogger();
    const signing = await loadSigning(testConfig(), log);
    expect(signing.signer?.kid).toBe(GOLDEN.kid);
    expect(signing.signer?.publicKey).toBe(GOLDEN.publicKey);
    expect(signing.signer?.testKey).toBe(true);
    expect(signing.unavailableReason).toBeNull();
    expect([...signing.keyring.keys()]).toEqual([GOLDEN.kid]);
    const vector = GOLDEN_VECTORS[1]!;
    expect(await signing.signer!.sign(vector.claims as Claims)).toBe(vector.key);
    expect(log.text()).not.toContain(GOLDEN.seed);
    expect(log.find('signing key loaded')[0]).toMatchObject({ kid: GOLDEN.kid, source: 'env' });
  });

  test('file form (licensegen format, trailing newline) with LICENSE_SIGNING_TRUSTED_SINCE', async () => {
    const signing = await loadSigning(signingConfig({ keyFile: keyFile(GOLDEN.seed), kid: GOLDEN.kid, trustedSince: 'v0.1.0' }), new MemoryLogger());
    expect(signing.signer?.trustedSince).toBe('v0.1.0');
    const token = await signing.signer!.sign({ jti: 'lic_AAAAAAAAAAAA', iss: 'kudurru', sub: 'Hank', plan: 'personal', nodes: 3, iat: 1, exp: 2 });
    expect((await verify(token, signing.keyring)).claims.sub).toBe('Hank');
  });

  test('a production-style kid outside dev mode loads and is not a test key', async () => {
    const pair = await newKeyPair();
    const dir = trustedDir({ 'kudurru-2099-1.pub': `${pair.publicKey}\n`, 'kudurru-2026-1.pub': `${GOLDEN.publicKey.replace('3M2', '4M2')}\n` });
    const signing = await loadSigning(signingConfig({ keyFile: keyFile(pair.seed), kid: 'kudurru-2099-1', trustedKeysDir: dir }, false), new MemoryLogger());
    expect(signing.signer?.testKey).toBe(false);
    expect([...signing.keyring.keys()].sort()).toEqual(['kudurru-2026-1', 'kudurru-2099-1']);
  });

  test('a dev key (dev-1) in dev mode is a test key: no kudurru release trusts it', async () => {
    const pair = await newKeyPair();
    const dir = trustedDir({ 'dev-1.pub': pair.publicKey });
    const signing = await loadSigning(signingConfig({ keyFile: keyFile(pair.seed), kid: 'dev-1', trustedKeysDir: dir }), new MemoryLogger());
    expect(signing.signer?.testKey).toBe(true);
  });

  test('no key configured: signing unavailable with the reason, the keyring still loaded', async () => {
    const log = new MemoryLogger();
    const signing = await loadSigning(signingConfig({ kid: 'kudurru-2026-1' }, false), log);
    expect(signing.signer).toBeNull();
    expect(signing.unavailableReason).toBe('no signing key is configured');
    expect(signing.keyring.get(GOLDEN.kid)).toBe(GOLDEN.publicKey);
    expect(log.find('LICENSE_SIGNING_KID is set but no signing key is configured')).toHaveLength(1);
  });

  const refusals: [string, () => Promise<ReturnType<typeof testConfig>>, string][] = [
    ['the env form outside dev mode', () => Promise.resolve(signingConfig({ keyEnv: GOLDEN.seed, kid: GOLDEN.kid }, false)), 'only with LICENSE_SERVER_DEV=true'],
    ['both forms', () => Promise.resolve(signingConfig({ keyEnv: GOLDEN.seed, keyFile: keyFile(GOLDEN.seed), kid: GOLDEN.kid })), 'both set'],
    ['a key without a kid', () => Promise.resolve(signingConfig({ keyFile: keyFile(GOLDEN.seed) })), 'LICENSE_SIGNING_KID is required'],
    ['a kid that is not trusted', () => Promise.resolve(signingConfig({ keyFile: keyFile(GOLDEN.seed), kid: 'kudurru-2027-1' })), 'is not in LICENSE_TRUSTED_KEYS_DIR'],
    [
      'a seed that does not derive the trusted public key',
      async () => {
        const pair = await newKeyPair();
        return signingConfig({ keyFile: keyFile(pair.seed), kid: GOLDEN.kid });
      },
      'does not match server-test-1.pub',
    ],
    ['a test kid outside dev mode', () => Promise.resolve(signingConfig({ keyFile: keyFile(GOLDEN.seed), kid: GOLDEN.kid }, false)), 'is a test key'],
    [
      'the e2e-test kid outside dev mode',
      async () => {
        const pair = await newKeyPair();
        return signingConfig({ keyFile: keyFile(pair.seed), kid: 'e2e-test', trustedKeysDir: trustedDir({ 'e2e-test.pub': pair.publicKey }) }, false);
      },
      'is a test key',
    ],
    [
      'the golden public key under another kid outside dev mode',
      () => Promise.resolve(signingConfig({ keyFile: keyFile(GOLDEN.seed), kid: 'kudurru-2030-1', trustedKeysDir: trustedDir({ 'kudurru-2030-1.pub': GOLDEN.publicKey }) }, false)),
      'published test key',
    ],
    ['a key file that is not a seed', () => Promise.resolve(signingConfig({ keyFile: keyFile('not base64!'), kid: GOLDEN.kid })), 'does not hold an Ed25519 seed'],
    ['a 31-byte seed', () => Promise.resolve(signingConfig({ keyFile: keyFile(Buffer.alloc(31, 7).toString('base64')), kid: GOLDEN.kid })), 'does not hold an Ed25519 seed'],
    ['a missing key file', () => Promise.resolve(signingConfig({ keyFile: '/nonexistent/signing.key', kid: GOLDEN.kid })), 'cannot read /nonexistent/signing.key'],
    ['an empty trusted dir', () => Promise.resolve(signingConfig({ trustedKeysDir: trustedDir({ 'README.md': 'x' }) })), 'no <kid>.pub files'],
    ['a bad .pub file', () => Promise.resolve(signingConfig({ trustedKeysDir: trustedDir({ 'k.pub': 'AAAA' }) })), 'k.pub is not an Ed25519 public key'],
    ['a missing trusted dir', () => Promise.resolve(signingConfig({ trustedKeysDir: '/nonexistent/keys' })), 'cannot read /nonexistent/keys'],
  ];
  for (const [name, config, message] of refusals) {
    test(`refuses ${name}`, async () => {
      const cfg = await config();
      let err: unknown;
      try {
        await loadSigning(cfg, new MemoryLogger());
      } catch (e) {
        err = e;
      }
      expect(err).toBeInstanceOf(Error);
      expect((err as Error).message).toContain(message);
      expect((err as Error).message).not.toContain(GOLDEN.seed);
    });
  }
});

describe('key file formats', () => {
  test('std base64 decoding without strings: padding, whitespace, refusals', () => {
    const seed = Buffer.from(GOLDEN.seed, 'base64');
    expect(Buffer.from(decodeStdBase64Bytes(Buffer.from(`  ${GOLDEN.seed}\r\n`))!)).toEqual(seed);
    for (const s of ['YQ==', 'YWI=', 'YWJj']) expect(Buffer.from(decodeStdBase64Bytes(Buffer.from(s))!).toString()).toBe(Buffer.from(s, 'base64').toString());
    for (const bad of ['', 'YQ=', 'Y===', '====', 'YW=j', 'YW_j', 'YWJj\nYWJj']) expect(decodeStdBase64Bytes(Buffer.from(bad))).toBeNull();
  });

  test('.pub files as license.ParsePublicKey reads them', () => {
    expect(parsePublicKey(` ${GOLDEN.publicKey}\n`)).toBe(GOLDEN.publicKey);
    expect(parsePublicKey(GOLDEN.publicKey.slice(0, 20) + '\n' + GOLDEN.publicKey.slice(20))).toBe(GOLDEN.publicKey);
    expect(parsePublicKey('AAAA')).toBeNull();
    expect(parsePublicKey(GOLDEN.publicKey.replace(/=$/, ''))).toBeNull();
    expect(readKeyring(goldenTrustedKeysDir()).get(GOLDEN.kid)).toBe(GOLDEN.publicKey);
  });
});
