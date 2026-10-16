import { initTabs, runVisiblePanels } from "./tabs.ts";
import { initLicenseForm } from "./license.ts";
import { initTrace } from "./trace.ts";
import { initPricing } from "./pricing.ts";

function initMenu(): void {
  const button = document.querySelector<HTMLButtonElement>(".menu-toggle");
  const nav = document.getElementById("site-nav");
  if (!button || !nav) return;
  const set = (open: boolean) => {
    button.setAttribute("aria-expanded", String(open));
    document.body.classList.toggle("menu-open", open);
  };
  button.addEventListener("click", () => set(button.getAttribute("aria-expanded") !== "true"));
  nav.addEventListener("click", (e) => {
    if ((e.target as Element).closest("a")) set(false);
  });
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") set(false);
  });
}

function initCopy(): void {
  for (const button of document.querySelectorAll<HTMLButtonElement>("[data-copy]")) {
    const label = button.querySelector("span");
    button.addEventListener("click", async () => {
      const source = document.getElementById(button.dataset.copy ?? "");
      if (!source || !label) return;
      try {
        await navigator.clipboard.writeText(source.textContent ?? "");
        label.textContent = "Copied";
      } catch {
        label.textContent = "Select and copy";
      }
      setTimeout(() => (label.textContent = "Copy"), 1800);
    });
  }
}

document.documentElement.classList.add("js");
initMenu();
initCopy();
initTabs();
runVisiblePanels();
initTrace();
initLicenseForm();
initPricing();
