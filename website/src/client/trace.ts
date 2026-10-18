// The hero's sample query log: a new row from TRACE every ~1.3 s, newest on
// top. Stops while the tab is hidden or the log is off screen; with
// prefers-reduced-motion the server-rendered rows stay as they are.
import { STAGES, STATUS_LABEL, TRACE, type TraceRow } from "../content.ts";

const MAX_ROWS = 9;

function el<K extends keyof HTMLElementTagNameMap>(tag: K, cls: string, text?: string): HTMLElementTagNameMap[K] {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

export function micros(us: number): string {
  return us >= 1000 ? `${(us / 1000).toFixed(1)} ms` : `${us} µs`;
}

export function clock(d: Date): string {
  const p = (n: number) => String(n).padStart(2, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

// Mirrors traceRow in src/pages/index.ts.
export function row(r: TraceRow, time: string): HTMLLIElement {
  const li = el("li", `tr s-${r.status}`);
  const reached = STAGES.findIndex((s) => s.id === r.stage);
  const path = el("span", "tr-path");
  path.setAttribute("aria-hidden", "true");
  STAGES.forEach((_, i) => path.append(el("i", i < reached ? "on" : i === reached ? "hit" : "")));
  li.append(
    el("span", "tr-time", time),
    el("span", "tr-client", r.client),
    el("span", "tr-type", r.qtype),
    el("span", "tr-name", r.qname),
    path,
    el("span", "tr-status", STATUS_LABEL[r.status]),
    el("span", "tr-us", micros(r.micros)),
  );
  return li;
}

export function initTrace(): void {
  const list = document.querySelector<HTMLOListElement>("[data-trace]");
  const qps = document.querySelector<HTMLElement>("[data-qps]");
  if (!list || matchMedia("(prefers-reduced-motion: reduce)").matches) return;

  // Restamp the server-rendered rows with the visitor's clock.
  const now = Date.now();
  list.querySelectorAll(".tr-time").forEach((t, i) => {
    t.textContent = clock(new Date(now - i * 2900));
  });

  let next = MAX_ROWS % TRACE.length;
  let visible = true;
  let timer: ReturnType<typeof setTimeout> | undefined;

  const tick = () => {
    const r = TRACE[next++ % TRACE.length];
    if (r) {
      const li = row(r, clock(new Date()));
      li.classList.add("new");
      list.prepend(li);
      while (list.children.length > MAX_ROWS) list.lastElementChild?.remove();
      if (qps) qps.textContent = (18 + Math.random() * 5).toFixed(1);
    }
    schedule();
  };
  const schedule = () => {
    clearTimeout(timer);
    if (visible && !document.hidden) timer = setTimeout(tick, 900 + Math.random() * 900);
  };

  new IntersectionObserver((entries) => {
    visible = entries.some((e) => e.isIntersecting);
    schedule();
  }).observe(list);
  document.addEventListener("visibilitychange", schedule);
}
