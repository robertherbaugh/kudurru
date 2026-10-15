import { COMMERCIAL_PLANS, VOLUME_PRICING } from "./content.ts";

export type CommercialEdition = keyof typeof COMMERCIAL_PLANS;

// Calculate in cents so discounted node fees keep their exact dollar value.
// A null result means the deployment needs an individual quote.
export function annualPrice(edition: CommercialEdition, nodes: number) {
  if (!Number.isSafeInteger(nodes) || nodes < 1) return null;
  if (nodes >= VOLUME_PRICING.quoteFrom) return null;
  const plan = COMMERCIAL_PLANS[edition];
  const discounted = nodes >= VOLUME_PRICING.discountFrom;
  const perNodeCents = Math.round(plan.perNode * 100 * (discounted ? 1 - VOLUME_PRICING.discount : 1));
  const nodeCents = perNodeCents * nodes;
  const totalCents = plan.platform * 100 + nodeCents;
  return { platform: plan.platform, perNode: perNodeCents / 100, nodeFee: nodeCents / 100, total: totalCents / 100, discounted };
}

export function usd(amount: number, decimals = false): string {
  return new Intl.NumberFormat("en-US", {
    style: "currency", currency: "USD", minimumFractionDigits: decimals || !Number.isInteger(amount) ? 2 : 0, maximumFractionDigits: 2,
  }).format(amount);
}
