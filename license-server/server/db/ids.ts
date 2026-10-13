// Ids and secrets. Every id is a prefix (ID_PREFIX in shared/api.ts) plus
// base64url of 9 random bytes, the license id format of hack/licensegen
// (lic_ + 12 characters). Bearer secrets (session cookies, CSRF tokens,
// invite and confirmation links) are 32 random bytes in base64url; only
// their SHA-256 is stored, so a database dump opens no session and no link.

import { createHash, randomBytes, timingSafeEqual } from 'node:crypto';
import { ID_PREFIX, type IdKind } from '../../shared/api';

/** A new id of this kind, e.g. newId('license') → 'lic_ejB_fnIEG90C'. */
export function newId(kind: IdKind): string {
  return ID_PREFIX[kind] + randomBytes(9).toString('base64url');
}

/** Whether `s` looks like an id of this kind (prefix plus 1 to 64 base64url characters). Imported licenses keep their own jti and may not match. */
export function isId(kind: IdKind, s: unknown): s is string {
  const prefix = ID_PREFIX[kind];
  return typeof s === 'string' && s.startsWith(prefix) && /^[A-Za-z0-9_-]{1,64}$/.test(s.slice(prefix.length));
}

/** A new bearer secret: 32 random bytes, base64url (43 characters). */
export function newSecret(): string {
  return randomBytes(32).toString('base64url');
}

/** The stored form of a bearer secret: SHA-256 of its UTF-8 text. */
export function hashSecret(secret: string): Buffer {
  return createHash('sha256').update(secret, 'utf8').digest();
}

/** Whether `s` has the shape of newSecret() output (cheap check before a database lookup). */
export function isSecretShaped(s: unknown): s is string {
  return typeof s === 'string' && /^[A-Za-z0-9_-]{43}$/.test(s);
}

/** Constant-time string comparison (CSRF tokens). Different lengths compare unequal. */
export function safeEqual(a: string, b: string): boolean {
  const x = Buffer.from(a, 'utf8');
  const y = Buffer.from(b, 'utf8');
  return x.length === y.length && timingSafeEqual(x, y);
}
