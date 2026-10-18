// Writes the development signing key of the license portal (spec §12), so
// compose.yaml and a server on the host can sign keys without Go and
// without the real key:
//
//   bun scripts/dev-keygen.ts [--kid dev-1] [--out dev] [--force]
//
//   <out>/secrets/signing.key       the private key in licensegen's format (standard
//                                   base64 of the 32-byte Ed25519 seed, a newline), mode 0600
//   <out>/trusted-keys/<kid>.pub    the public key in the format of internal/license/keys
//                                   (standard base64 of the 32 raw bytes, a newline)
//   <out>/secrets/oidc_client_secret  the client secret dev/dex.yaml registers, when missing
//
// No kudurru release trusts a key made here: only a server pointed at
// <out>/trusted-keys does (LICENSE_TRUSTED_KEYS_DIR), and only in dev mode
// (LICENSE_SERVER_DEV=true). Existing key files are kept unless --force is
// given, so a second run cannot silently invalidate keys already issued.
// <out> defaults to dev/ next to this script; a relative --out is resolved
// against the working directory.

import { chmod, mkdir, writeFile } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { join, resolve } from 'node:path';

/** dev/dex.yaml registers this value for the client license-portal; keep them equal. */
const DEX_CLIENT_SECRET = 'dev-only-license-portal-client-secret';

/** The DER prefix of a PKCS #8 Ed25519 private key (RFC 8410); the 32-byte seed follows it. */
const PKCS8_ED25519_PREFIX = Uint8Array.from([
  0x30, 0x2e, 0x02, 0x01, 0x00, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x04, 0x22, 0x04, 0x20,
]);

/** A kid becomes a file name, here and in the trusted-keys directory the server reads. */
const KID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;

interface Options {
  kid: string;
  out: string;
  force: boolean;
}

function usage(message: string): never {
  console.error(`dev-keygen: ${message}`);
  console.error('usage: bun scripts/dev-keygen.ts [--kid dev-1] [--out dev] [--force]');
  process.exit(2);
}

function parseArgs(argv: string[]): Options {
  const options: Options = { kid: 'dev-1', out: join(import.meta.dir, '..', 'dev'), force: false };
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i] ?? '';
    const value = (): string => {
      const next = argv[++i];
      if (next === undefined || next.startsWith('--')) usage(`${arg} needs a value`);
      return next;
    };
    switch (arg) {
      case '--kid':
        options.kid = value();
        break;
      case '--out':
        options.out = resolve(value());
        break;
      case '--force':
        options.force = true;
        break;
      case '--help':
      case '-h':
        usage('writes a development signing key');
        break;
      default:
        usage(`unknown argument ${arg}`);
    }
  }
  if (!KID_PATTERN.test(options.kid)) usage(`--kid ${options.kid}: use letters, digits, '.', '_' and '-' (at most 64)`);
  return options;
}

function base64(bytes: Uint8Array): string {
  return Buffer.from(bytes).toString('base64');
}

/** Generates an Ed25519 key pair and returns the seed and the raw public key. */
async function generate(): Promise<{ seed: Uint8Array; publicKey: Uint8Array }> {
  // The string form: the object form matches a Node typing overload that returns a single CryptoKey.
  const pair = await crypto.subtle.generateKey('Ed25519', true, ['sign', 'verify']);
  if (!('privateKey' in pair)) throw new Error('WebCrypto returned no Ed25519 key pair');
  const pkcs8 = new Uint8Array(await crypto.subtle.exportKey('pkcs8', pair.privateKey));
  const publicKey = new Uint8Array(await crypto.subtle.exportKey('raw', pair.publicKey));
  const prefixMatches = PKCS8_ED25519_PREFIX.every((b, i) => pkcs8[i] === b);
  if (pkcs8.length !== PKCS8_ED25519_PREFIX.length + 32 || !prefixMatches || publicKey.length !== 32) {
    throw new Error('unexpected Ed25519 key encoding from WebCrypto');
  }
  const seed = pkcs8.slice(PKCS8_ED25519_PREFIX.length);
  pkcs8.fill(0);

  // The seed must derive the public key, as the server checks at startup:
  // re-import it the way the server does (PKCS #8, not extractable) and
  // verify a signature with the exported public key.
  const der = new Uint8Array(PKCS8_ED25519_PREFIX.length + 32);
  der.set(PKCS8_ED25519_PREFIX);
  der.set(seed, PKCS8_ED25519_PREFIX.length);
  const signer = await crypto.subtle.importKey('pkcs8', der, { name: 'Ed25519' }, false, ['sign']);
  der.fill(0);
  const verifier = await crypto.subtle.importKey('raw', publicKey, { name: 'Ed25519' }, false, ['verify']);
  const probe = new TextEncoder().encode('kudurru license portal dev-keygen');
  const signature = await crypto.subtle.sign({ name: 'Ed25519' }, signer, probe);
  if (!(await crypto.subtle.verify({ name: 'Ed25519' }, verifier, signature, probe))) {
    throw new Error('the generated seed does not derive the exported public key');
  }
  return { seed, publicKey };
}

/** Writes a file with the given mode; without force an existing file is an error (O_EXCL). */
async function writeNew(path: string, content: string, mode: number, force: boolean): Promise<void> {
  await writeFile(path, content, { mode, flag: force ? 'w' : 'wx' });
  // An overwritten file keeps its old mode; set it explicitly.
  await chmod(path, mode);
}

async function main(): Promise<void> {
  const options = parseArgs(process.argv.slice(2));
  const secretsDir = join(options.out, 'secrets');
  const trustedDir = join(options.out, 'trusted-keys');
  const keyPath = join(secretsDir, 'signing.key');
  const pubPath = join(trustedDir, `${options.kid}.pub`);
  const clientSecretPath = join(secretsDir, 'oidc_client_secret');

  const existing = [keyPath, pubPath].filter((p) => existsSync(p));
  if (existing.length > 0 && !options.force) {
    console.error(`dev-keygen: ${existing.join(' and ')} already exist${existing.length === 1 ? 's' : ''}; keeping them.`);
    console.error('Keys issued with the current dev key stop verifying once it is replaced; pass --force to replace it anyway.');
    process.exit(1);
  }

  await mkdir(secretsDir, { recursive: true, mode: 0o700 });
  await mkdir(trustedDir, { recursive: true, mode: 0o755 });

  const { seed, publicKey } = await generate();
  try {
    await writeNew(keyPath, `${base64(seed)}\n`, 0o600, options.force);
  } finally {
    seed.fill(0);
  }
  await writeNew(pubPath, `${base64(publicKey)}\n`, 0o644, options.force);
  console.log(`wrote ${keyPath} (private, mode 0600) and ${pubPath}`);

  if (!existsSync(clientSecretPath)) {
    await writeNew(clientSecretPath, `${DEX_CLIENT_SECRET}\n`, 0o600, false);
    console.log(`wrote ${clientSecretPath} (the client secret of dev/dex.yaml)`);
  }

  if (options.kid !== 'dev-1') {
    console.log(`compose.yaml expects kid dev-1: set LICENSE_SIGNING_KID=${options.kid} where you run the server.`);
  }
  console.log('This key is for development only: no kudurru release trusts it.');
}

await main();
