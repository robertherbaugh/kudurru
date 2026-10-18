// The public price list of the commercial editions, as kudurru.io shows it
// (website/src/content.ts COMMERCIAL_PLANS and VOLUME_PRICING, and
// website/src/pricing.ts annualPrice): a new Order Form's fees start from it,
// and a license admin may agree others. server/test/pricing-drift.test.ts
// fails when the two drift. All amounts are whole US cents.

import type { BusinessEdition } from './catalog';

/** The annual platform fee and the fee per production node of each edition, in cents. */
export const COMMERCIAL_PRICES: Readonly<Record<BusinessEdition, { platformCents: number; perNodeCents: number }>> = Object.freeze({
  core: { platformCents: 9_900, perNodeCents: 9_900 },
  enterprise: { platformCents: 49_900, perNodeCents: 29_900 },
});

/** Node fees are discounted from `discountFrom` nodes; from `quoteFrom` nodes a deployment is quoted individually. */
export const VOLUME_PRICING: Readonly<{ discountFrom: number; quoteFrom: number; discount: number }> = Object.freeze({ discountFrom: 25, quoteFrom: 50, discount: 0.1 });

/** What the price list says for an edition and a node count. */
export interface ListPrice {
  platformCents: number;
  /** After the volume discount, when it applies. */
  perNodeCents: number;
  /** platform + nodes × per node. */
  annualCents: number;
  discounted: boolean;
}

/** The list price, or null when the deployment needs an individual quote (or `nodes` is not a whole number from 1). */
export function listPrice(edition: BusinessEdition, nodes: number): ListPrice | null {
  if (!Number.isSafeInteger(nodes) || nodes < 1 || nodes >= VOLUME_PRICING.quoteFrom) return null;
  const plan = COMMERCIAL_PRICES[edition];
  const discounted = nodes >= VOLUME_PRICING.discountFrom;
  const perNodeCents = Math.round(plan.perNodeCents * (discounted ? 1 - VOLUME_PRICING.discount : 1));
  return { platformCents: plan.platformCents, perNodeCents, annualCents: plan.platformCents + perNodeCents * nodes, discounted };
}

/** Cents as people read US dollars: 199400 → "$1,994.00". */
export function usd(cents: number): string {
  return new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD', minimumFractionDigits: 2, maximumFractionDigits: 2 }).format(cents / 100);
}
