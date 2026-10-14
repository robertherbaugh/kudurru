// Key dates (design spec §0.9). The wire and the UI carry a key's expiry as
// `expiresOn`, the last valid UTC day ('YYYY-MM-DD'); the signed exp claim is
// 23:59:59 UTC of that day, so `kudurru license status` (which prints exp as
// a UTC date) shows the same day the portal does. Every other instant on the
// wire is an RFC 3339 UTC string. All arithmetic is in UTC whole days and
// whole seconds; nothing here reads the local time zone.

import { GRACE_SECONDS } from './catalog';

/** A UTC calendar day, 'YYYY-MM-DD' (the last day a key is valid). */
export type ExpiresOn = string;

/** Seconds in a day. */
export const DAY_SECONDS = 86_400;

const PATTERN = /^(\d{4})-(\d{2})-(\d{2})$/;

/** Splits a valid 'YYYY-MM-DD' into the Unix seconds of its UTC midnight, or null. */
function midnightOf(s: string): number | null {
  const m = PATTERN.exec(s);
  if (!m) return null;
  const y = Number(m[1]);
  const mo = Number(m[2]);
  const d = Number(m[3]);
  if (y < 1970 || mo < 1 || mo > 12 || d < 1 || d > 31) return null;
  const ms = Date.UTC(y, mo - 1, d);
  const back = new Date(ms);
  // Rejects 2026-02-30 and friends (Date.UTC rolls them over).
  if (back.getUTCFullYear() !== y || back.getUTCMonth() !== mo - 1 || back.getUTCDate() !== d) return null;
  return ms / 1000;
}

function mustMidnight(expiresOn: string): number {
  const t = midnightOf(expiresOn);
  if (t === null) throw new RangeError(`not a YYYY-MM-DD date: ${JSON.stringify(expiresOn)}`);
  return t;
}

/** Whether `s` is a real calendar day written 'YYYY-MM-DD' (year 1970 to 9999). */
export function isExpiresOn(s: unknown): s is ExpiresOn {
  return typeof s === 'string' && midnightOf(s) !== null;
}

/** The exp claim for a key valid through `expiresOn`: 23:59:59 UTC of that day, in Unix seconds. Throws on an invalid date. */
export function expFor(expiresOn: ExpiresOn): number {
  return mustMidnight(expiresOn) + DAY_SECONDS - 1;
}

/** The UTC day of an exp claim (Unix seconds) or an instant: what `kudurru license status` prints as "expires". */
export function expiresOnOf(exp: number | Date): ExpiresOn {
  const ms = typeof exp === 'number' ? exp * 1000 : exp.getTime();
  if (!Number.isFinite(ms)) throw new RangeError('not a valid instant');
  return new Date(ms).toISOString().slice(0, 10);
}

/** `expiresOn` moved by `days` whole UTC days (negative moves back). */
export function addDays(expiresOn: ExpiresOn, days: number): ExpiresOn {
  if (!Number.isInteger(days)) throw new RangeError(`days must be a whole number, got ${days}`);
  return expiresOnOf(mustMidnight(expiresOn) + days * DAY_SECONDS);
}

/** Today as a UTC day. */
export function todayUtc(now: Date = new Date()): ExpiresOn {
  return expiresOnOf(now);
}

/**
 * The last UTC day a key valid through `expiresOn` still works on nodes,
 * grace included: the day of exp + GRACE_SECONDS (D + 30). Also the
 * "keeps working until" day of a withdrawn license, taken from its latest
 * key.
 */
export function graceEndsOn(expiresOn: ExpiresOn): ExpiresOn {
  return expiresOnOf(expFor(expiresOn) + GRACE_SECONDS);
}

/** Whole UTC days from `from` to `to` (positive when `to` is later). */
export function daysBetween(from: ExpiresOn, to: ExpiresOn): number {
  return Math.round((mustMidnight(to) - mustMidnight(from)) / DAY_SECONDS);
}

/** The later of two days ('YYYY-MM-DD' strings compare in calendar order). */
export function maxExpiresOn(a: ExpiresOn, b: ExpiresOn): ExpiresOn {
  return a >= b ? a : b;
}

/** Unix seconds of an instant, as Go's time.Unix(): the iat of a key signed at `d`. */
export function unixSeconds(d: Date): number {
  return Math.floor(d.getTime() / 1000);
}

/** An instant on the wire: RFC 3339 in UTC with milliseconds ('2026-10-12T21:04:05.123Z'). */
export function toInstant(d: Date): string {
  return d.toISOString();
}

/** toInstant for nullable columns. */
export function toInstantOrNull(d: Date | null | undefined): string | null {
  return d ? d.toISOString() : null;
}
