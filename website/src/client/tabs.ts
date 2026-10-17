// ARIA tabs for every [data-tabs] container: click or arrow keys select a
// tab, the matching panel is shown and replays its entry animation.

export function initTabs(root: ParentNode = document): void {
  for (const group of root.querySelectorAll<HTMLElement>("[data-tabs]")) {
    const tabs = [...group.querySelectorAll<HTMLButtonElement>('[role="tab"]')];
    const select = (tab: HTMLButtonElement, focus: boolean) => {
      for (const t of tabs) {
        const on = t === tab;
        t.setAttribute("aria-selected", String(on));
        t.tabIndex = on ? 0 : -1;
        const panel = document.getElementById(t.getAttribute("aria-controls") ?? "");
        if (!panel) continue;
        panel.hidden = !on;
        if (on) replay(panel);
      }
      if (focus) tab.focus();
    };
    tabs.forEach((tab, i) => {
      tab.addEventListener("click", () => select(tab, false));
      tab.addEventListener("keydown", (e) => {
        const vertical = tab.closest('[aria-orientation="vertical"]') !== null;
        const prev = vertical ? "ArrowUp" : "ArrowLeft";
        const next = vertical ? "ArrowDown" : "ArrowRight";
        let to: HTMLButtonElement | undefined;
        if (e.key === prev) to = tabs[(i - 1 + tabs.length) % tabs.length];
        else if (e.key === next) to = tabs[(i + 1) % tabs.length];
        else if (e.key === "Home") to = tabs[0];
        else if (e.key === "End") to = tabs[tabs.length - 1];
        if (!to) return;
        e.preventDefault();
        select(to, true);
      });
    });
  }
}

// Panels animate when they carry .run; removing and re-adding it on the
// next frame restarts the CSS transitions.
function replay(panel: HTMLElement): void {
  panel.classList.remove("run");
  requestAnimationFrame(() => requestAnimationFrame(() => panel.classList.add("run")));
}

export function runVisiblePanels(root: ParentNode = document): void {
  const panels = [...root.querySelectorAll<HTMLElement>(".scn-panel:not([hidden])")];
  if (!("IntersectionObserver" in window)) {
    panels.forEach(replay);
    return;
  }
  const io = new IntersectionObserver(
    (entries) => {
      for (const e of entries) {
        if (!e.isIntersecting) continue;
        replay(e.target as HTMLElement);
        io.unobserve(e.target);
      }
    },
    { threshold: 0.35 },
  );
  panels.forEach((p) => io.observe(p));
}
