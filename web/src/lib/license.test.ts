import { describe, expect, it } from 'vitest';
import type { LicenseInfo } from '../api/types';
import { communityLicense } from '../mocks/fixtures';
import { daysUntil, FEATURES, featureLabel, licenseNotice, planLabel, policyLimitLabel, policyLimitNotice, stateLabel, stateTone } from './license';

const NOW = Date.parse('2026-10-12T12:00:00Z');
const day = (n: number) => new Date(NOW + n * 86_400_000).toISOString();

function active(over: Partial<LicenseInfo> = {}): LicenseInfo {
  return { state: 'active', plan: 'personal', nodes: 3, nodesInUse: 1, defaultNamespace: 'default', overLimit: false, features: ['rbac'], policyLimit: 0, expiresAt: day(400), graceEndsAt: day(430), took_ms: 0, ...over };
}

describe('licenseNotice', () => {
  it('says nothing without a key, and nothing for a key far from expiring', () => {
    expect(licenseNotice(undefined, NOW)).toBeNull();
    expect(licenseNotice(communityLicense(), NOW)).toBeNull();
    expect(licenseNotice(active(), NOW)).toBeNull();
  });

  it('reminds about an expiry within 30 days, once, as information', () => {
    expect(licenseNotice(active({ expiresAt: day(31) }), NOW)).toBeNull();
    const n = licenseNotice(active({ expiresAt: day(12) }), NOW);
    expect(n).toMatchObject({ status: 'info', title: 'The license key expires soon' });
    expect(n?.description).toContain('12 days');
    expect(licenseNotice(active({ expiresAt: day(1) }), NOW)?.description).toContain('1 day.');
  });

  it('warns in the grace period with the days that are left', () => {
    const n = licenseNotice(active({ state: 'grace', expiresAt: day(-5), graceEndsAt: day(25) }), NOW);
    expect(n).toMatchObject({ status: 'warning', title: 'The license key has expired' });
    expect(n?.description).toContain('25 more days');
  });

  it('reports an expired or invalid key as an error that says DNS is unaffected', () => {
    const expired = licenseNotice(active({ state: 'expired', expiresAt: day(-60), graceEndsAt: day(-30) }), NOW);
    expect(expired).toMatchObject({ status: 'error', title: 'The license key has expired' });
    const invalid = licenseNotice({ ...communityLicense(), state: 'invalid', error: 'license key signature does not verify' }, NOW);
    expect(invalid).toMatchObject({ status: 'error', title: 'The license key is invalid' });
    expect(invalid?.description).toContain('signature does not verify');
    for (const n of [expired, invalid]) expect(n?.description).toContain('DNS is not affected');
  });

  it('puts the node limit before everything else', () => {
    const n = licenseNotice(active({ nodes: 3, nodesInUse: 5, overLimit: true, state: 'grace', expiresAt: day(-1), graceEndsAt: day(29) }), NOW);
    expect(n).toMatchObject({ status: 'warning', title: 'More nodes than the license allows' });
    expect(n?.description).toContain('5 nodes');
    expect(n?.description).toContain('allows 3');
  });
});

describe('license wording', () => {
  it('names every feature the server knows, with a sentence each', () => {
    // internal/license Features(): keep in step when a feature is added.
    expect(FEATURES.map((f) => f.key).sort()).toEqual(['audit', 'edge', 'integrations', 'longterm', 'oidc', 'rbac', 'requests']);
    for (const f of FEATURES) expect(f.unlocks.length).toBeGreaterThan(20);
    expect(featureLabel('longterm')).toBe('Long-term statistics');
    expect(featureLabel('from-the-future')).toBe('from-the-future');
  });

  it('words states, plans and tones', () => {
    expect(stateLabel('community')).toBe('No key');
    expect(stateLabel('grace')).toBe('Expired, in grace period');
    expect(planLabel('personal')).toContain('free');
    expect(stateTone('active')).toBe('success');
    expect(stateTone('community')).toBe('neutral');
    expect(stateTone('invalid')).toBe('error');
  });

  it('words the policy limit in the singular without a key', () => {
    expect(communityLicense().policyLimit).toBe(1);
    expect(policyLimitLabel(0)).toBe('no limit');
    expect(policyLimitLabel(1)).toBe('1 (the default policy)');
    expect(policyLimitLabel(5)).toBe('up to 5');
    expect(policyLimitNotice(1, ['default']).title).toBe('Without a valid license key only the default policy is available');
    expect(policyLimitNotice(1, ['kids']).title).toBe('Without a valid license key only one policy is available');
    const over = policyLimitNotice(1, ['default', 'kids', 'guests']);
    expect(over.title).toBe('Without a valid license key no new policy can be created');
    expect(over.description).toContain('The free limit is one policy.');
    expect(policyLimitNotice(3, ['a', 'b', 'c', 'd']).description).toContain('The free limit is 3 policies.');
    expect(policyLimitNotice(3, ['a', 'b', 'c']).title).toBe('Without a valid license key at most 3 policies are available');
    for (const n of [policyLimitNotice(1, ['default']), over]) expect(`${n.title} ${n.description}`).not.toMatch(/\b1 policies\b/);
  });

  it('counts whole days', () => {
    expect(daysUntil(day(3), NOW)).toBe(3);
    expect(daysUntil(day(-2), NOW)).toBe(-2);
    expect(daysUntil(undefined, NOW)).toBeUndefined();
    expect(daysUntil('not a date', NOW)).toBeUndefined();
  });
});
