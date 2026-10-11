// Charts read their colours from CSS (§10 principle 4): the --q-* tokens
// every theme in src/themes declares (high contrast re-points them in
// styles.css) colour the semantic query classes, so one palette serves
// tables, badges and both chart libraries, in light, dark and high
// contrast.

export type SeriesKey = 'blocked' | 'cached' | 'forwarded' | 'authoritative' | 'other' | 'permitted' | 'error';

const FALLBACK: Record<SeriesKey, string> = {
  blocked: '#e3193b',
  cached: '#2f6fde',
  forwarded: '#13a08b',
  authoritative: '#7b4cd8',
  other: '#8a96a3',
  permitted: '#0d8626',
  error: '#e9af08',
};

/** Resolves a CSS custom property on an element (default: the app root). */
export function cssVar(name: string, el?: Element | null): string {
  const target = el ?? document.querySelector('.k-app') ?? document.documentElement;
  try {
    return getComputedStyle(target).getPropertyValue(name).trim();
  } catch {
    return '';
  }
}

/**
 * The colour of a series. light-dark() and var() values are resolved by
 * painting them on a probe element, because canvas APIs need a concrete
 * colour.
 */
export function seriesColor(key: SeriesKey, el?: Element | null): string {
  return resolveColor(`var(--q-${key})`, el) || FALLBACK[key];
}

/** Categorical palette for pies (query types, upstreams, clients). */
export function categorical(el?: Element | null): string[] {
  const names = ['blue', 'teal', 'orange', 'purple', 'pink', 'green', 'cyan', 'indigo', 'brown', 'red'];
  const fallback = ['#2f6fde', '#13a08b', '#f08c00', '#7b4cd8', '#d6336c', '#2b8a3e', '#1098ad', '#4c6ef5', '#8d5524', '#e03131'];
  return names.map((n, i) => resolveColor(`var(--color-data-categorical-${n})`, el) || (fallback[i] as string));
}

/** Text and grid colours for chart axes. */
export function axisColors(el?: Element | null): { text: string; grid: string } {
  return {
    text: resolveColor('var(--color-text-secondary)', el) || '#6b7785',
    grid: resolveColor('var(--color-border)', el) || 'rgba(128,128,128,0.25)',
  };
}

let probe: HTMLSpanElement | null = null;

function resolveColor(value: string, el?: Element | null): string {
  if (typeof document === 'undefined') return '';
  const host = el ?? document.querySelector('.k-app') ?? document.body;
  if (!host) return '';
  if (!probe) {
    probe = document.createElement('span');
    probe.setAttribute('aria-hidden', 'true');
    probe.className = 'k-visually-hidden';
  }
  host.appendChild(probe);
  probe.style.color = '';
  probe.style.color = value;
  let c: string;
  try {
    c = getComputedStyle(probe).color;
  } catch {
    c = '';
  }
  probe.remove();
  // jsdom returns the literal var(...) string; treat that as unresolved.
  return c && !c.includes('var(') ? c : '';
}

/** Adds alpha to an rgb()/rgba()/#hex colour. */
export function withAlpha(color: string, alpha: number): string {
  const m = /^rgba?\((\d+)[ ,]+(\d+)[ ,]+(\d+)/.exec(color);
  if (m) return `rgba(${m[1]}, ${m[2]}, ${m[3]}, ${alpha})`;
  const h = /^#([0-9a-f]{6})$/i.exec(color);
  if (h) {
    const n = parseInt(h[1] as string, 16);
    return `rgba(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255}, ${alpha})`;
  }
  return color;
}

/** Resolves any CSS colour expression (var(), light-dark()) to rgb(). */
export function resolveCSSColor(value: string, el?: Element | null): string {
  return resolveColor(value, el);
}
