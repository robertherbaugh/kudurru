import { pricingState } from "../pricing.ts";

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
    const selected = calculator.querySelector<HTMLInputElement>('input[name="pricing-edition"]:checked')?.value;
    const edition = selected === "core" ? "core" : "enterprise";
    const state = pricingState(edition, nodes.validity.valid ? nodes.valueAsNumber : NaN);
    nodes.setAttribute("aria-invalid", String(state.kind === "invalid"));
    rows.hidden = state.kind !== "price";
    total.hidden = state.kind === "invalid";
    message.textContent = state.message;
    if (state.kind === "invalid") return;
    result.textContent = state.total;
    if (state.kind === "quote") return;
    set("platform", state.platform);
    set("node-label", state.nodeLabel);
    set("nodes", state.nodeFee);
  };
  calculator.addEventListener("input", update);
  calculator.addEventListener("change", update);
  update();
}
