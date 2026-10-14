// The key format (design spec §0.2-0.4): compact JWS byte-identical to Go's
// internal/license.Sign, and a verifier that mirrors license.Verify.
//
// Signing: the header is {"alg":"EdDSA","kid":<kid>,"typ":"JWT"} (go-jose
// marshals a map, so the keys are sorted; never "Ed25519", which the nodes
// refuse); the payload is encoding/json's output for license.Claims: struct
// order jti, iss, sub, plan, nodes, features (omitted when empty), iat, nbf
// (omitted when 0), exp, whole seconds, no whitespace, and Go's HTML-safe
// escaping (< > & U+2028 U+2029 as \u003c \u003e \u0026 \u2028 \u2029;
// everything else as JSON.stringify writes it, which matches Go since 1.22).
// The three parts are base64url without padding and the signature is
// Ed25519 over "<header>.<payload>" (WebCrypto; Ed25519 is deterministic,
// so the same seed and claims give Go's exact string: server/test/golden).
// The signer also refuses what the portal must never sign: features: [],
// an nbf, fractional numbers and strings Go would rewrite (lone surrogates).
//
// Verifying (import): whitespace is removed as strings.Fields does (Go's
// unicode.IsSpace set), the compact form is split and decoded strictly
// (RawURLEncoding), alg must be EdDSA, the kid must be in the keyring, the
// signature must verify, and the payload is decoded as json.Unmarshal into
// license.Claims would (case-insensitive field names, integer literals
// only, null leaves a field unset) before Claims.Validate's rules.

import { ISSUER, isFeature, isPlan } from '../../shared/catalog';
import { isGoSpace } from '../http/body';
import type { Claims, ForeignClaims, Keyring } from './types';

// --- encoding ------------------------------------------------------------------

// < > & U+2028 U+2029 (built from code points so the source stays ASCII).
const HTML_UNSAFE = new RegExp(`[<>&${String.fromCharCode(0x2028, 0x2029)}]`, 'g');

/** A JSON string literal exactly as Go's encoding/json writes it (HTML-safe escaping). */
export function goJsonString(s: string): string {
  return JSON.stringify(s).replace(HTML_UNSAFE, (c) => '\\u' + c.charCodeAt(0).toString(16).padStart(4, '0'));
}

/** base64url without padding (RawURLEncoding). */
export function base64url(bytes: Uint8Array | string): string {
  return (typeof bytes === 'string' ? Buffer.from(bytes, 'utf8') : Buffer.from(bytes.buffer, bytes.byteOffset, bytes.byteLength)).toString('base64url');
}

/** The protected header go-jose writes for license.Sign: keys sorted, no whitespace. */
export function encodeHeader(kid: string): string {
  return `{"alg":"EdDSA","kid":${goJsonString(kid)},"typ":"JWT"}`;
}

/** The payload json.Marshal writes for license.Claims (struct order, omitempty features and nbf). */
export function encodeClaims(c: Claims | ForeignClaims): string {
  let out = `{"jti":${goJsonString(c.jti)},"iss":${goJsonString(c.iss)},"sub":${goJsonString(c.sub)},"plan":${goJsonString(c.plan)},"nodes":${String(c.nodes)}`;
  if (c.features && c.features.length > 0) out += `,"features":[${c.features.map(goJsonString).join(',')}]`;
  out += `,"iat":${String(c.iat)}`;
  if (c.nbf !== undefined && c.nbf !== 0) out += `,"nbf":${String(c.nbf)}`;
  out += `,"exp":${String(c.exp)}}`;
  return out;
}

// --- validation ----------------------------------------------------------------

/** Whether `s` has an unpaired UTF-16 surrogate (Go would write U+FFFD instead). */
function hasLoneSurrogate(s: string): boolean {
  return /[\ud800-\udbff](?![\udc00-\udfff])|(?<![\ud800-\udbff])[\udc00-\udfff]/.test(s);
}

/** Whether `s` is empty or only Go whitespace (strings.TrimSpace(s) == ""). */
function goBlank(s: string): boolean {
  for (let i = 0; i < s.length; i++) if (!isGoSpace(s.charCodeAt(i))) return false;
  return true;
}

/**
 * internal/license Claims.Validate: the reason the claims fail, worded as
 * Go words it, or null when they pass. Shape only, never the time window.
 */
export function validateClaims(c: { jti: string; iss: string; sub: string; plan: string; nodes: number | bigint; iat: number | bigint; exp: number | bigint }): string | null {
  if (c.jti === '') return 'jti is empty';
  if (c.iss !== ISSUER) return `iss is ${JSON.stringify(c.iss)}, want "${ISSUER}"`;
  if (goBlank(c.sub)) return 'sub (licensee) is empty';
  if (!isPlan(c.plan)) return `plan ${JSON.stringify(c.plan)} is not one of personal, business, enterprise, trial`;
  if (BigInt(c.nodes) < 1n) return `nodes is ${String(c.nodes)}, want at least 1`;
  if (BigInt(c.exp) <= BigInt(c.iat)) return 'exp is not after iat';
  return null;
}

/** The signer refused the claims (a bug in the caller: services validate first). */
export class SignRefusedError extends Error {
  override readonly name = 'SignRefusedError';
}

/**
 * Throws SignRefusedError unless the portal may sign `c`: Go's Validate,
 * plus whole safe-integer numbers, no nbf (spec §0.4), features absent or a
 * non-empty list of known, distinct features (§0.3), and no lone
 * surrogates in any string.
 */
export function assertSignable(kid: string, c: Claims): void {
  const refuse = (why: string): never => {
    throw new SignRefusedError(`refusing to sign license ${JSON.stringify(c.jti)}: ${why}`);
  };
  for (const [name, n] of [['nodes', c.nodes], ['iat', c.iat], ['exp', c.exp]] as const) {
    if (!Number.isSafeInteger(n)) refuse(`${name} must be a whole number of seconds or nodes`);
  }
  if (c.nbf !== undefined) refuse('nbf is never set (a future nbf breaks a node that installs the key early)');
  if (c.features !== undefined) {
    if (c.features.length === 0) refuse('features is never empty (omit it to grant every feature)');
    if (!c.features.every(isFeature)) refuse('features names an unknown feature');
    if (new Set(c.features).size !== c.features.length) refuse('features has duplicates');
  }
  for (const s of [kid, c.jti, c.iss, c.sub, c.plan]) if (hasLoneSurrogate(s)) refuse('a string has an unpaired surrogate');
  if (kid === '') refuse('the kid is empty');
  const invalid = validateClaims(c);
  if (invalid) refuse(invalid);
}

// --- signing -------------------------------------------------------------------

/** The DER prefix of a PKCS #8 Ed25519 private key (RFC 8410); the 32-byte seed follows it. */
const PKCS8_ED25519_PREFIX = Uint8Array.from([0x30, 0x2e, 0x02, 0x01, 0x00, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x04, 0x22, 0x04, 0x20]);

/**
 * Imports a 32-byte Ed25519 seed as a non-extractable WebCrypto signing
 * key. The DER buffer built here is zeroed before returning; the caller
 * zeroes `seed`.
 */
export async function importSigningKey(seed: Uint8Array): Promise<CryptoKey> {
  if (seed.byteLength !== 32) throw new Error(`an Ed25519 seed is 32 bytes, got ${seed.byteLength}`);
  const der = new Uint8Array(PKCS8_ED25519_PREFIX.length + 32);
  try {
    der.set(PKCS8_ED25519_PREFIX, 0);
    der.set(seed, PKCS8_ED25519_PREFIX.length);
    return await crypto.subtle.importKey('pkcs8', der, 'Ed25519', false, ['sign']);
  } finally {
    der.fill(0);
  }
}

/** Imports a public key as the .pub file has it (std base64 of the 32 raw bytes). */
export async function importPublicKey(publicKey: string): Promise<CryptoKey> {
  const raw = Buffer.from(publicKey, 'base64');
  if (raw.byteLength !== 32) throw new Error(`an Ed25519 public key is 32 bytes, got ${raw.byteLength}`);
  return await crypto.subtle.importKey('raw', raw, 'Ed25519', false, ['verify']);
}

const encoder = new TextEncoder();

/** The compact JWS of `claims` signed by `key` under `kid`, byte for byte what license.Sign returns. */
export async function signCompact(key: CryptoKey, kid: string, claims: Claims): Promise<string> {
  assertSignable(kid, claims);
  const input = `${base64url(encodeHeader(kid))}.${base64url(encodeClaims(claims))}`;
  const signature = new Uint8Array(await crypto.subtle.sign('Ed25519', key, encoder.encode(input)));
  return `${input}.${base64url(signature)}`;
}

// --- verifying -----------------------------------------------------------------

/** Why a key did not verify: license.Verify's three errors, plus values this portal cannot hold. */
export type VerifyErrorCode = 'malformed' | 'unknown_key' | 'bad_signature' | 'unsupported';

/** The messages of license.ErrMalformed, ErrUnknownKey and ErrBadSignature. */
const GO_MESSAGES: Record<Exclude<VerifyErrorCode, 'unsupported'>, string> = {
  malformed: 'license key is not a valid signed token',
  unknown_key: 'license key is signed with a key this build does not trust',
  bad_signature: 'license key signature does not verify',
};

export class VerifyError extends Error {
  override readonly name = 'VerifyError';
  constructor(
    readonly code: VerifyErrorCode,
    /** The reason after Go's message ("exp is not after iat"), when there is one. */
    readonly reason?: string,
  ) {
    super(code === 'unsupported' ? `license key cannot be stored by this portal: ${reason ?? ''}` : reason ? `${GO_MESSAGES[code]}: ${reason}` : GO_MESSAGES[code]);
  }
}

/** A key that verified. */
export interface VerifiedKey {
  /** The token without whitespace (what the node verifies; what the portal stores). */
  token: string;
  kid: string;
  claims: ForeignClaims;
}

/** license.go clean(): every Go unicode.IsSpace character removed. */
export function cleanToken(raw: string): string {
  let out = '';
  for (let i = 0; i < raw.length; i++) if (!isGoSpace(raw.charCodeAt(i))) out += raw[i];
  return out;
}

/** base64.RawURLEncoding.DecodeString: the URL alphabet, no padding, no length ≡ 1 (mod 4). */
function decodeRawUrl(part: string): Uint8Array<ArrayBuffer> {
  if (!/^[A-Za-z0-9_-]*$/.test(part) || part.length % 4 === 1) throw new VerifyError('malformed');
  return new Uint8Array(Buffer.from(part, 'base64url'));
}

const lenientDecoder = new TextDecoder('utf-8');

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return v !== null && typeof v === 'object' && !Array.isArray(v);
}

/** JSON.parse that keeps every number's source text (Go decodes integers from the literal). */
function parseWithNumberSources(text: string): unknown {
  return JSON.parse(text, function (_key: string, value: unknown, context?: { source?: string }): unknown {
    if (typeof value !== 'number') return value;
    const source = context?.source ?? (Number.isSafeInteger(value) ? String(value) : '');
    return { numberSource: source };
  });
}

function numberSource(v: unknown): string | null {
  return isPlainObject(v) && typeof v.numberSource === 'string' && Object.keys(v).length === 1 ? v.numberSource : null;
}

const INT64_MIN = -(2n ** 63n);
const INT64_MAX = 2n ** 63n - 1n;

/** Go's strconv.ParseInt(literal, 10, 64) on a JSON number: integer syntax only, within int64. */
function goInt(field: string, v: unknown): bigint {
  const source = numberSource(v);
  if (source === null || !/^-?(0|[1-9]\d*)$/.test(source)) throw new VerifyError('malformed', `json: cannot unmarshal into Go struct field Claims.${field}`);
  const n = BigInt(source);
  if (n < INT64_MIN || n > INT64_MAX) throw new VerifyError('malformed', `json: cannot unmarshal number ${source} into Go struct field Claims.${field} of type int64`);
  return n;
}

/** The Claims field a JSON key decodes into (exact name first, then Go's case folding), or null. */
const FIELDS = ['jti', 'iss', 'sub', 'plan', 'nodes', 'features', 'iat', 'nbf', 'exp'] as const;
type Field = (typeof FIELDS)[number];

function fieldOf(key: string): Field | null {
  const exact = FIELDS.find((f) => f === key);
  if (exact) return exact;
  // Go folds ASCII case, and the two non-ASCII runes in an ASCII letter's
  // fold set: U+017F (long s) and U+212A (Kelvin sign).
  const folded = key.replaceAll(String.fromCharCode(0x17f), 's').replaceAll(String.fromCharCode(0x212a), 'k').replace(/[A-Z]/g, (c) => c.toLowerCase());
  return FIELDS.find((f) => f === folded) ?? null;
}

/** Replaces unpaired surrogates (from \ud800 escapes) with U+FFFD, as Go's decoder does. */
function goString(s: string): string {
  return s.replace(/[\ud800-\udbff](?![\udc00-\udfff])|(?<![\ud800-\udbff])[\udc00-\udfff]/g, String.fromCharCode(0xfffd));
}

/** json.Unmarshal(payload, &license.Claims{}) followed by Claims.Validate. */
function decodeClaims(payload: Uint8Array): ForeignClaims {
  let raw: unknown;
  try {
    raw = parseWithNumberSources(lenientDecoder.decode(payload));
  } catch {
    throw new VerifyError('malformed', 'the payload is not JSON');
  }
  const c = { jti: '', iss: '', sub: '', plan: '', nodes: 0n, features: null as string[] | null, iat: 0n, nbf: 0n, exp: 0n };
  if (raw !== null) {
    if (!isPlainObject(raw)) throw new VerifyError('malformed', 'json: cannot unmarshal the payload into Go value of type license.Claims');
    const seen = new Map<Field, string>();
    for (const [key, value] of Object.entries(raw)) {
      const field = fieldOf(key);
      if (!field) continue;
      // JSON.parse keeps the last of identical keys, as Go does; keys that
      // differ only in case would depend on order JSON.parse does not keep.
      const before = seen.get(field);
      if (before !== undefined) throw new VerifyError('malformed', `the payload names ${field} twice (${JSON.stringify(before)} and ${JSON.stringify(key)})`);
      seen.set(field, key);
      if (value === null) continue;
      switch (field) {
        case 'jti':
        case 'iss':
        case 'sub':
        case 'plan':
          if (typeof value !== 'string') throw new VerifyError('malformed', `json: cannot unmarshal into Go struct field Claims.${field} of type string`);
          c[field] = goString(value);
          break;
        case 'nodes':
        case 'iat':
        case 'nbf':
        case 'exp':
          c[field] = goInt(field, value);
          break;
        case 'features':
          if (!Array.isArray(value) || !value.every((f) => f === null || typeof f === 'string')) {
            throw new VerifyError('malformed', 'json: cannot unmarshal into Go struct field Claims.features of type []license.Feature');
          }
          c.features = value.map((f) => (typeof f === 'string' ? goString(f) : ''));
          break;
      }
    }
  }
  const invalid = validateClaims(c);
  if (invalid) throw new VerifyError('malformed', invalid);
  for (const [name, n] of [['nodes', c.nodes], ['iat', c.iat], ['nbf', c.nbf], ['exp', c.exp]] as const) {
    if (n > BigInt(Number.MAX_SAFE_INTEGER) || n < BigInt(Number.MIN_SAFE_INTEGER)) throw new VerifyError('unsupported', `${name} is out of range`);
  }
  const claims: ForeignClaims = {
    jti: c.jti,
    iss: ISSUER,
    sub: c.sub,
    plan: c.plan as ForeignClaims['plan'],
    nodes: Number(c.nodes),
    iat: Number(c.iat),
    exp: Number(c.exp),
  };
  if (c.features && c.features.length > 0) claims.features = c.features;
  if (c.nbf !== 0n) claims.nbf = Number(c.nbf);
  return claims;
}

/**
 * license.Verify: the key's claims when its signature verifies against the
 * keyring and its claims pass Validate (not the time window). Throws
 * VerifyError. Unlike go-jose, the JSON serialization of a JWS is refused:
 * nothing emits it for license keys and the portal stores compact tokens.
 */
export async function verify(raw: string, keyring: Keyring): Promise<VerifiedKey> {
  const token = cleanToken(raw);
  if (token === '') throw new VerifyError('malformed', 'the key is empty');
  if (token.startsWith('{')) throw new VerifyError('malformed', 'only the compact form of a key is accepted');
  const parts = token.split('.');
  if (parts.length !== 3) throw new VerifyError('malformed');
  const [h, p, s] = parts as [string, string, string];
  const headerBytes = decodeRawUrl(h);
  const payload = decodeRawUrl(p);
  const signature = decodeRawUrl(s);

  let header: unknown;
  try {
    header = JSON.parse(lenientDecoder.decode(headerBytes));
  } catch {
    throw new VerifyError('malformed');
  }
  if (!isPlainObject(header) || header.alg !== 'EdDSA') throw new VerifyError('malformed');
  const kidValue = header.kid ?? '';
  if (typeof kidValue !== 'string') throw new VerifyError('malformed');
  const kid = goString(kidValue);

  const publicKey = keyring.get(kid);
  if (publicKey === undefined) throw new VerifyError('unknown_key');
  // go-jose supports no critical header a license key could carry.
  if (header.crit !== undefined && header.crit !== null) throw new VerifyError('bad_signature');
  const ok = signature.byteLength === 64 && (await crypto.subtle.verify('Ed25519', await importPublicKey(publicKey), signature, encoder.encode(`${h}.${p}`)));
  if (!ok) throw new VerifyError('bad_signature');
  return { token, kid, claims: decodeClaims(payload) };
}
