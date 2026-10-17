// A small JSON-to-YAML printer for the read-only "View as YAML" of an
// object (what `kudurru apply -f` or kubectl take). Block style, keys in
// their object order, strings quoted when YAML would read them as anything
// else.

const PLAIN = /^[A-Za-z0-9_./][A-Za-z0-9_ ./:@+-]*$/;
const RESERVED = /^(true|false|yes|no|on|off|null|~|y|n)$/i;

function scalar(v: unknown): string {
  if (v === null || v === undefined) return 'null';
  if (typeof v === 'number' || typeof v === 'boolean') return String(v);
  const s = typeof v === 'string' ? v : JSON.stringify(v);
  if (s === '' || !PLAIN.test(s) || RESERVED.test(s) || /^[-+]?(\d|\.\d)/.test(s) || /\s$/.test(s) || s.includes(': ') || s.includes(' #')) {
    return JSON.stringify(s);
  }
  return s;
}

function isEmpty(v: unknown): boolean {
  return (Array.isArray(v) && v.length === 0) || (!!v && typeof v === 'object' && !Array.isArray(v) && Object.keys(v).length === 0);
}

function lines(v: unknown, indent: string): string[] {
  if (Array.isArray(v)) {
    return v.flatMap((item) => {
      if (item && typeof item === 'object' && !isEmpty(item)) {
        const [first = '', ...rest] = lines(item, `${indent}  `);
        return [`${indent}- ${first.trimStart()}`, ...rest];
      }
      return [`${indent}- ${isEmpty(item) ? (Array.isArray(item) ? '[]' : '{}') : scalar(item)}`];
    });
  }
  if (v && typeof v === 'object') {
    return Object.entries(v as Record<string, unknown>)
      .filter(([, val]) => val !== undefined)
      .flatMap(([k, val]) => {
        const key = scalar(k);
        if (isEmpty(val)) return [`${indent}${key}: ${Array.isArray(val) ? '[]' : '{}'}`];
        if (val && typeof val === 'object') return [`${indent}${key}:`, ...lines(val, Array.isArray(val) ? indent : `${indent}  `)];
        return [`${indent}${key}: ${scalar(val)}`];
      });
  }
  return [`${indent}${scalar(v)}`];
}

export function toYAML(v: unknown): string {
  return `${lines(v, '').join('\n')}\n`;
}
