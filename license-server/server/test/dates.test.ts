// shared/dates.ts and the license state of shared/catalog.ts: the wire's
// expiresOn day maps to exp = 23:59:59 UTC of that day and back, whatever
// the local time zone, and states change at the exact second a node's do
// (internal/license/manager.go).
import { describe, expect, test } from 'bun:test';
import { GRACE_SECONDS, graceEnds, licenseState } from '../../shared/catalog';
import { addDays, daysBetween, expFor, expiresOnOf, graceEndsOn, isExpiresOn, maxExpiresOn, todayUtc, toInstant, unixSeconds } from '../../shared/dates';

describe('expiresOn ↔ exp', () => {
  // exp is the last second of the day, so `kudurru license status` prints the same day.
  test('expFor is 23:59:59 UTC of the day', () => {
    expect(expFor('2027-10-12')).toBe(Date.UTC(2027, 9, 12) / 1000 + 86_399);
    expect(new Date(expFor('2027-10-12') * 1000).toISOString()).toBe('2027-10-12T23:59:59.000Z');
  });

  test.each(['1970-01-01', '2026-10-12', '2028-02-29', '2026-12-31', '2099-01-01', '9999-12-31'])('round trip %s', (d) => {
    expect(expiresOnOf(expFor(d))).toBe(d);
    expect(expiresOnOf(new Date(expFor(d) * 1000))).toBe(d);
  });

  // The golden vectors (Go) use midnight exps; expiresOnOf names the UTC day that contains exp.
  test('expiresOnOf of a midnight exp is that day', () => {
    expect(expiresOnOf(1823385600)).toBe('2027-10-13');
    expect(expiresOnOf(1823385600 - 1)).toBe('2027-10-12');
  });

  test('expFor refuses what is not a day', () => {
    expect(() => expFor('2026-02-30')).toThrow(RangeError);
    expect(() => expFor('tomorrow')).toThrow(RangeError);
  });
});

describe('isExpiresOn', () => {
  test.each(['2026-10-12', '2028-02-29', '1970-01-01', '9999-12-31'])('accepts %s', (d) => expect(isExpiresOn(d)).toBe(true));
  test.each([['2026-02-29'], ['2026-13-01'], ['2026-00-10'], ['2026-1-01'], [' 2026-01-01'], ['2026-01-01T00:00:00Z'], ['1969-12-31'], [''], [20260101], [null], [undefined]])(
    'refuses %p',
    (d) => expect(isExpiresOn(d)).toBe(false),
  );
});

describe('day arithmetic', () => {
  test('addDays crosses months, years and leap days, both ways', () => {
    expect(addDays('2026-10-12', 365)).toBe('2027-10-12');
    expect(addDays('2027-10-12', 365)).toBe('2028-10-11');
    expect(addDays('2028-02-28', 1)).toBe('2028-02-29');
    expect(addDays('2026-12-31', 1)).toBe('2027-01-01');
    expect(addDays('2026-03-01', -1)).toBe('2026-02-28');
    expect(() => addDays('2026-03-01', 1.5)).toThrow(RangeError);
  });

  // The UTC day, not the local one: 23:30 in New York on the 12th is the 13th in UTC.
  test('todayUtc ignores the local zone', () => {
    expect(todayUtc(new Date('2026-10-12T23:30:00-05:00'))).toBe('2026-10-13');
    expect(todayUtc(new Date('2026-10-13T00:30:00+02:00'))).toBe('2026-10-12');
  });

  test('graceEndsOn is 30 days later', () => {
    expect(graceEndsOn('2026-10-12')).toBe('2026-11-11');
    expect(expFor(graceEndsOn('2026-10-12')) - expFor('2026-10-12')).toBe(GRACE_SECONDS);
  });

  test('daysBetween, maxExpiresOn', () => {
    expect(daysBetween('2026-10-12', '2027-10-12')).toBe(365);
    expect(daysBetween('2027-10-12', '2026-10-12')).toBe(-365);
    expect(maxExpiresOn('2026-10-12', '2026-09-30')).toBe('2026-10-12');
    expect(maxExpiresOn('2026-01-01', '2026-10-12')).toBe('2026-10-12');
  });

  test('unixSeconds floors like Go, instants are RFC 3339 UTC', () => {
    expect(unixSeconds(new Date(1_791_849_600_999))).toBe(1_791_849_600);
    expect(toInstant(new Date(Date.UTC(2026, 9, 12, 21, 4, 5, 123)))).toBe('2026-10-12T21:04:05.123Z');
  });
});

describe('licenseState', () => {
  const exp = expFor('2027-10-12');
  const at = (seconds: number, ms = 0) => new Date(seconds * 1000 + ms);

  // Each boundary is the exact second a node changes state.
  test.each([
    ['active', exp - 30 * 86_400 - 1, 0],
    ['expiring', exp - 30 * 86_400, 0],
    ['expiring', exp - 1, 999],
    ['grace', exp, 0],
    ['grace', exp + GRACE_SECONDS - 1, 999],
    ['expired', exp + GRACE_SECONDS, 0],
  ] as const)('%s at exp%+d s', (state, seconds, ms) => {
    expect(licenseState(exp, false, at(seconds, ms))).toBe(state);
    expect(licenseState(new Date(exp * 1000), false, at(seconds, ms))).toBe(state);
  });

  test('withdrawn wins', () => {
    expect(licenseState(exp, true, at(exp - 100 * 86_400))).toBe('withdrawn');
  });

  test('graceEnds is exp + 30 days', () => {
    expect(graceEnds(exp).getTime()).toBe((exp + GRACE_SECONDS) * 1000);
  });
});
