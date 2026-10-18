// shared/pricing.ts against the website's price list (website/src/content.ts
// COMMERCIAL_PLANS and VOLUME_PRICING, website/src/pricing.ts annualPrice):
// a new Order Form's fees start from the price kudurru.io shows, so the two
// must not drift. The website is loaded at run time (its imports carry .ts
// extensions, which this project's type check refuses).
import { describe, expect, test } from 'bun:test';
import { join } from 'node:path';
import { COMMERCIAL_PRICES, listPrice, usd, VOLUME_PRICING } from '../../shared/pricing';

const WEBSITE = join(import.meta.dir, '..', '..', '..', 'website', 'src');

interface WebsitePlan {
  platform: number;
  perNode: number;
}
interface WebsiteContent {
  COMMERCIAL_PLANS: Record<'core' | 'enterprise', WebsitePlan>;
  VOLUME_PRICING: { discountFrom: number; quoteFrom: number; discount: number };
}
type AnnualPrice = (edition: 'core' | 'enterprise', nodes: number) => { platform: number; perNode: number; total: number; discounted: boolean } | null;

const content = (await import(join(WEBSITE, 'content.ts'))) as WebsiteContent;
const { annualPrice } = (await import(join(WEBSITE, 'pricing.ts'))) as { annualPrice: AnnualPrice };

describe('pricing drift (website/src)', () => {
  test('the editions and their fees are the website’s', () => {
    expect(Object.keys(COMMERCIAL_PRICES).sort()).toEqual(Object.keys(content.COMMERCIAL_PLANS).sort());
    for (const edition of ['core', 'enterprise'] as const) {
      expect(COMMERCIAL_PRICES[edition]).toEqual({ platformCents: content.COMMERCIAL_PLANS[edition].platform * 100, perNodeCents: content.COMMERCIAL_PLANS[edition].perNode * 100 });
    }
    expect({ ...VOLUME_PRICING }).toEqual({ ...content.VOLUME_PRICING });
  });

  test('every node count prices as the calculator does, quotes included', () => {
    for (const edition of ['core', 'enterprise'] as const) {
      for (let nodes = 0; nodes <= 60; nodes++) {
        const site = annualPrice(edition, nodes);
        const ours = listPrice(edition, nodes);
        if (!site) {
          expect(ours).toBeNull();
          continue;
        }
        expect(ours).toEqual({ platformCents: site.platform * 100, perNodeCents: Math.round(site.perNode * 100), annualCents: Math.round(site.total * 100), discounted: site.discounted });
      }
    }
  });

  test('cents read as dollars', () => {
    expect(usd(199_400)).toBe('$1,994.00');
    expect(usd(0)).toBe('$0.00');
  });
});
