import { annualPrice, usd } from "../pricing.ts";
import { VOLUME_PRICING } from "../content.ts";

export function initPricing(): void {
  const calculator = document.querySelector<HTMLElement>("[data-pricing]");
  if (!calculator) return;
  const nodes = calculator.querySelector<HTMLInputElement>("[data-pricing-nodes]")!;
  const result = calculator.querySelector<HTMLElement>("[data-pricing-result]")!;
  const message = calculator.querySelector<HTMLElement>("[data-pricing-message]")!;
  const rows = calculator.querySelector<HTMLElement>("[data-pricing-rows]")!;
  const total = calculator.querySelector<HTMLElement>("[data-pricing-total]")!;
  const set = (name: string, value: string) => {
    calculator.querySelector<HTMLElement>(`[data-price="${name}"]`)!.textContent = value;
  };
  const update = () => {
    const valid = nodes.validity.valid && Number.isSafeInteger(nodes.valueAsNumber) && nodes.valueAsNumber >= 1;
    nodes.setAttribute("aria-invalid", String(!valid));
    rows.hidden = !valid;
    total.hidden = !valid;
    if (!valid) {
      message.textContent = "Enter a whole number of production nodes, 1 or more.";
      return;
    }
    const selected = calculator.querySelector<HTMLInputElement>('input[name="pricing-edition"]:checked')?.value;
    const edition = selected === "core" ? "core" : "enterprise";
    const price = annualPrice(edition, nodes.valueAsNumber);
    rows.hidden = price === null;
    if (!price) {
      result.textContent = "Let’s size it together";
      message.textContent = `Deployments of ${VOLUME_PRICING.quoteFrom} or more nodes are quoted individually. Contact us for volume pricing.`;
      return;
    }
    set("platform", usd(price.platform));
    set("node-label", `${nodes.valueAsNumber} × ${usd(price.perNode)} per node${price.discounted ? ` (${VOLUME_PRICING.discount * 100}% off)` : ""}`);
    set("nodes", usd(price.nodeFee));
    result.textContent = usd(price.total);
    message.textContent = `Equivalent to ${usd(price.total / 12, true)}/month, billed annually.${price.discounted ? ` Includes ${VOLUME_PRICING.discount * 100}% off node fees; the platform fee is unchanged.` : ""}`;
  };
  calculator.addEventListener("input", update);
  calculator.addEventListener("change", update);
  update();
}
