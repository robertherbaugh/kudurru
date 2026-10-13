// Where to go after signing in (design spec §6 "Sign-in" step 1): a port of
// kudurru's ee/oidc/next.go SafeNext, so the post-login redirect can never
// leave the portal. Only a same-origin absolute path survives; everything
// else becomes "/": empty values, more than 2048 bytes, anything not
// starting with "/", protocol-relative "//host" and the "/\host" form
// browsers normalise to it, control characters (C0 and DEL), backslashes,
// lone surrogates, and what Go's url.Parse would reject or read as having
// a scheme, host or user info (in a "/"-rooted value: an invalid %
// escape in the path or fragment). It is applied at the start of the
// sign-in and again before the final redirect.
//
// The value is used as the Location header verbatim, except that
// characters outside printable ASCII are percent-encoded one by one
// (locationOf). It is never resolved against a base URL: URL resolution
// turns "/.//evil.example" into "//evil.example".

import { hasLoneSurrogate } from '../http/body';

/** The longest `next` kept, in UTF-8 bytes (next.go maxNextLen). */
export const MAX_NEXT_BYTES = 2048;

/** Whether every % in `s` starts a two-digit hex escape (Go's url.unescape). */
function validEscapes(s: string): boolean {
  for (let i = 0; i < s.length; i++) {
    if (s[i] !== '%') continue;
    if (!/^[0-9A-Fa-f]{2}$/.test(s.slice(i + 1, i + 3))) return false;
    i += 2;
  }
  return true;
}

/** `next` if it is a same-origin absolute path, else "/". */
export function safeNext(next: string | null | undefined): string {
  if (typeof next !== 'string' || next === '' || next[0] !== '/') return '/';
  if (Buffer.byteLength(next, 'utf8') > MAX_NEXT_BYTES) return '/';
  if (next.length > 1 && (next[1] === '/' || next[1] === '\\')) return '/';
  for (let i = 0; i < next.length; i++) {
    const c = next.charCodeAt(i);
    if (c < 0x20 || c === 0x7f || c === 0x5c) return '/';
  }
  if (hasLoneSurrogate(next)) return '/';
  // Go's url.Parse: the fragment and the path are unescaped (the query is not).
  const hash = next.indexOf('#');
  const beforeHash = hash < 0 ? next : next.slice(0, hash);
  const fragment = hash < 0 ? '' : next.slice(hash + 1);
  const q = beforeHash.indexOf('?');
  const path = q < 0 ? beforeHash : beforeHash.slice(0, q);
  if (!validEscapes(path) || !validEscapes(fragment)) return '/';
  return next;
}

/** A safeNext() value as a Location header: characters outside printable ASCII percent-encoded, nothing else changed. */
export function locationOf(next: string): string {
  return safeNext(next).replace(/[^\x21-\x7e]/gu, (ch) => encodeURIComponent(ch));
}
