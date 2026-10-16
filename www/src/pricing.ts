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

// Keep calculator states and their copy testable without a browser.
export function pricingState(edition: CommercialEdition, nodes: number) {
  if (!Number.isSafeInteger(nodes) || nodes < 1) {
    return { kind: "invalid" as const, message: "Enter a whole number of production nodes, 1 or more." };
  }
  const price = annualPrice(edition, nodes);
  if (!price) {
    return {
      kind: "quote" as const,
      total: "Let’s size it together",
      message: `Deployments of ${VOLUME_PRICING.quoteFrom} or more nodes are quoted individually. Contact us for volume pricing.`,
    };
  }
  const discount = `${VOLUME_PRICING.discount * 100}% off`;
  return {
    kind: "price" as const,
    platform: usd(price.platform),
    nodeLabel: `${nodes} × ${usd(price.perNode)} per node${price.discounted ? ` (${discount})` : ""}`,
    nodeFee: usd(price.nodeFee),
    total: usd(price.total),
    message: `Equivalent to ${usd(price.total / 12, true)}/month, billed annually.${price.discounted ? ` Includes ${discount} node fees; the platform fee is unchanged.` : ""}`,
  };
}
