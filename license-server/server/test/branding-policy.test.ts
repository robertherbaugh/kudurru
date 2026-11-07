// Terms policy is shared by issue, renewal, change and request approval.
// The final signer is tested separately so callers cannot bypass this gate.
import { describe, expect, test } from 'bun:test';
import { PLANS } from '../../shared/catalog';
import { HttpError } from '../http/errors';
import { assertPolicy, claimsFor, policyIssues, type KeyTerms } from '../licensing/claims';

const now = new Date('2026-10-12T09:30:00Z');
const base: KeyTerms = { licensee: 'Organization', plan: 'enterprise', nodes: 1, features: null, expiresOn: '2026-11-12' };

describe('branding terms policy', () => {
  test.each(PLANS.map((p) => p.key))('%s unrestricted terms remain signable', (plan) => {
    const terms = { ...base, plan };
    expect(policyIssues(terms, now)).toEqual([]);
    expect(claimsFor('lic_branding', terms, now).features).toBeUndefined();
  });

  test.each(['personal', 'business', 'trial'] as const)('%s cannot explicitly include branding', (plan) => {
    const terms = { ...base, plan, features: ['oidc', 'branding'] };
    expect(policyIssues(terms, now)).toEqual([{ field: 'features', message: 'Custom branding requires the Enterprise plan.' }]);
    let caught: unknown;
    try {
      assertPolicy(terms, now, (field) => `terms.${field}`);
    } catch (err) {
      caught = err;
    }
    expect(caught).toBeInstanceOf(HttpError);
    const err = caught as HttpError;
    expect(err.status).toBe(400);
    expect(err.fields).toEqual(['terms.features']);
  });

  test('Enterprise can explicitly include branding or omit it from a restricted list', () => {
    for (const features of [['branding'], ['branding', 'oidc'], ['oidc']] as const) {
      const terms = { ...base, features };
      expect(policyIssues(terms, now)).toEqual([]);
      expect(claimsFor('lic_branding', terms, now).features).toEqual(features);
    }
  });

  test('existing plan and explicit-list rules remain in force', () => {
    expect(policyIssues({ ...base, plan: 'personal', features: ['oidc'] }, now).map((i) => i.field)).toEqual(['features']);
    expect(policyIssues({ ...base, plan: 'business', features: ['oidc'] }, now)).toEqual([]);
    expect(policyIssues({ ...base, plan: 'trial', features: ['oidc'] }, now)).toEqual([]);
    for (const features of [[], ['branding', 'branding'], ['teleport']]) {
      expect(policyIssues({ ...base, features }, now).map((i) => i.field)).toEqual(['features']);
    }
  });
});
