// Copied from web/src/lib/format.ts at dd3dcb0; keep in step by hand.
// Changes: none.
// Formatting helpers shared by tiles, tables and charts.

const compact = new Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 });
const whole = new Intl.NumberFormat(undefined, { maximumFractionDigits: 0 });

/** 1,284 / 12.9K / 4.2M: auto-compacts above 10,000. */
export function formatCount(n: number | undefined | null): string {
  if (n === undefined || n === null || Number.isNaN(n)) return '–';
  return Math.abs(n) >= 10_000 ? compact.format(n) : whole.format(n);
}

/** Exact thousands-separated integer (tables, tooltips). */
export function formatExact(n: number | undefined | null): string {
  if (n === undefined || n === null || Number.isNaN(n)) return '–';
  return whole.format(n);
}

export function formatPercent(p: number | undefined | null, digits = 1): string {
  if (p === undefined || p === null || Number.isNaN(p)) return '–';
  return `${p.toFixed(digits)} %`;
}

export function formatQPS(q: number | undefined | null): string {
  if (q === undefined || q === null || Number.isNaN(q)) return '–';
  return q >= 100 ? whole.format(q) : q.toFixed(1);
}

export function formatMS(ms: number | undefined | null): string {
  if (ms === undefined || ms === null || Number.isNaN(ms)) return '–';
  if (ms < 1) return `${(ms * 1000).toFixed(0)} µs`;
  if (ms < 100) return `${ms.toFixed(1)} ms`;
  return `${whole.format(ms)} ms`;
}

const UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
export function formatBytes(b: number | undefined | null): string {
  if (b === undefined || b === null || Number.isNaN(b)) return '–';
  let v = b;
  let i = 0;
  while (v >= 1024 && i < UNITS.length - 1) {
    v /= 1024;
    i++;
  }
  return `${i === 0 ? v.toFixed(0) : v.toFixed(1)} ${UNITS[i]}`;
}

const timeFmt = new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit' });
const dateTimeFmt = new Intl.DateTimeFormat(undefined, {
  year: 'numeric',
  month: 'short',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
});

export function formatTime(ts: string | number | Date): string {
  return timeFmt.format(new Date(ts));
}

export function formatDateTime(ts: string | number | Date | undefined): string {
  if (ts === undefined) return '–';
  const d = new Date(ts);
  return Number.isNaN(d.getTime()) ? '–' : dateTimeFmt.format(d);
}

const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' });
/** "3 minutes ago" relative to now (or a given clock). */
export function formatRelative(ts: string | undefined, now: number = Date.now()): string {
  if (!ts) return 'never';
  const t = new Date(ts).getTime();
  if (Number.isNaN(t)) return '–';
  const s = Math.round((t - now) / 1000);
  const abs = Math.abs(s);
  if (abs < 60) return rtf.format(s, 'second');
  if (abs < 3600) return rtf.format(Math.round(s / 60), 'minute');
  if (abs < 86400) return rtf.format(Math.round(s / 3600), 'hour');
  return rtf.format(Math.round(s / 86400), 'day');
}

/** Seconds as "4:05", "1:02:03" or "45 s" style countdown text. */
export function formatCountdown(totalSeconds: number): string {
  const s = Math.max(0, Math.ceil(totalSeconds));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  const pad = (n: number) => String(n).padStart(2, '0');
  return h > 0 ? `${h}:${pad(m)}:${pad(sec)}` : `${m}:${pad(sec)}`;
}

/**
 * Parses a Go duration string ("10m", "1h30m", "1800ms", "24h") into
 * seconds; returns NaN for anything else.
 */
export function parseDuration(s: string): number {
  const str = s.trim();
  if (str === '0') return 0;
  const re = /(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/gy;
  let total = 0;
  let consumed = 0;
  let m: RegExpExecArray | null;
  while ((m = re.exec(str)) !== null) {
    const v = Number(m[1]);
    const unit = m[2];
    const mult =
      unit === 'h' ? 3600 : unit === 'm' ? 60 : unit === 's' ? 1 : unit === 'ms' ? 1e-3 : unit === 'ns' ? 1e-9 : 1e-6;
    total += v * mult;
    consumed = re.lastIndex;
  }
  return consumed === str.length && consumed > 0 ? total : Number.NaN;
}

/** Seconds as a Go duration string: 300 → "5m", 5400 → "1h30m". */
export function toGoDuration(seconds: number): string {
  if (seconds <= 0) return '0s';
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = Math.round(seconds % 60);
  return `${h ? `${h}h` : ''}${m ? `${m}m` : ''}${s ? `${s}s` : ''}`;
}
