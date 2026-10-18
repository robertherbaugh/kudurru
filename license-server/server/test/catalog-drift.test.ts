// shared/catalog.ts against the Go code the nodes run (design spec §10
// "catalog drift"): internal/license/license.go's Plan constants, its
// Features() list, Grace and Issuer must match the portal's catalog, so the
// portal never offers a plan a node refuses or forgets a feature.
import { describe, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { FEATURES, GRACE_DAYS, GRACE_SECONDS, ISSUER, PLANS } from '../../shared/catalog';

const LICENSE_GO = join(import.meta.dir, '..', '..', '..', 'internal', 'license', 'license.go');
const source = readFileSync(LICENSE_GO, 'utf8');

/** Every `Name Type = "value"` constant of a type. */
function constantsOf(type: string): Map<string, string> {
  const out = new Map<string, string>();
  for (const m of source.matchAll(new RegExp(String.raw`^\s*(\w+)\s+${type}\s*=\s*"([^"]*)"`, 'gm'))) out.set(m[1]!, m[2]!);
  return out;
}

describe('catalog drift (internal/license/license.go)', () => {
  test('the plans are exactly the Plan constants', () => {
    const plans = [...constantsOf('Plan').values()].sort();
    expect(plans.length).toBeGreaterThan(0);
    expect(PLANS.map((p) => p.key).sort()).toEqual(plans as typeof PLANS[number]['key'][]);
  });

  test('the plans are exactly the cases of Plan.Valid', () => {
    const valid = /func \(p Plan\) Valid\(\) bool \{[\s\S]*?case ([^:]+):/.exec(source);
    expect(valid).not.toBeNull();
    const consts = constantsOf('Plan');
    const names = valid![1]!.split(',').map((s) => s.trim());
    expect(names.map((n) => consts.get(n)).sort()).toEqual(PLANS.map((p) => p.key).sort());
  });

  test('the features are exactly Features(), and every Feature constant is in it', () => {
    const body = /func Features\(\) \[\]Feature \{\s*return \[\]Feature\{([^}]*)\}/.exec(source);
    expect(body).not.toBeNull();
    const consts = constantsOf('Feature');
    const listed = body![1]!.split(',').map((s) => s.trim()).filter((s) => s !== '');
    const values = listed.map((n) => {
      const v = consts.get(n);
      if (v === undefined) throw new Error(`Features() names ${n}, which is not a Feature constant`);
      return v;
    });
    expect(values.sort()).toEqual([...consts.values()].sort());
    expect(FEATURES.map((f) => f.key).sort()).toEqual(values.sort() as typeof FEATURES[number]['key'][]);
  });

  test('Grace is GRACE_DAYS days', () => {
    const grace = /const Grace = (\d+) \* 24 \* time\.Hour/.exec(source);
    expect(grace).not.toBeNull();
    expect(Number(grace![1])).toBe(GRACE_DAYS);
    expect(GRACE_SECONDS).toBe(GRACE_DAYS * 86_400);
  });

  test('Issuer is ISSUER', () => {
    expect(/const Issuer = "([^"]+)"/.exec(source)?.[1]).toBe(ISSUER);
  });
});
