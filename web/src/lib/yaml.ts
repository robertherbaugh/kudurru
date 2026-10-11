// A small YAML emitter for the "View as YAML" escape hatch on object
// pages: JSON values (objects, arrays, strings, numbers, booleans, null)
// as block YAML that objects.d and `kubectl apply` accept. Strings are
// quoted whenever a plain scalar could read as something else.
const PLAIN = /^[A-Za-z_./][A-Za-z0-9_./:@+-]*( [A-Za-z0-9_./:@+()-]+)*$/;
const RESERVED = /^(true|false|yes|no|on|off|null|~|y|n)$/i;

function scalar(v: unknown): string {
  if (v === null || v === undefined) return 'null';
  if (typeof v === 'number' || typeof v === 'boolean') return String(v);
  const s = typeof v === 'string' ? v : JSON.stringify(v);
  if (s !== '' && PLAIN.test(s) && !RESERVED.test(s) && !s.includes(': ') && !s.endsWith(':')) return s;
  return JSON.stringify(s);
}

function isEmpty(v: unknown): boolean {
  return (Array.isArray(v) && v.length === 0) || (!!v && typeof v === 'object' && !Array.isArray(v) && Object.keys(v).length === 0);
}

function emptyText(v: unknown): string {
  return Array.isArray(v) ? '[]' : '{}';
}

function emit(v: unknown, indent: string, out: string[]): void {
  if (Array.isArray(v)) {
    for (const item of v) {
      if (item && typeof item === 'object' && !isEmpty(item)) {
        const sub: string[] = [];
        emit(item, `${indent}  `, sub);
        // The item's first line goes after the dash.
        out.push(`${indent}- ${(sub[0] ?? '').trimStart()}`, ...sub.slice(1));
      } else {
        out.push(`${indent}- ${isEmpty(item) ? emptyText(item) : scalar(item)}`);
      }
    }
    return;
  }
  for (const [k, val] of Object.entries(v as Record<string, unknown>)) {
    if (val === undefined) continue;
    const key = scalar(k);
    if (val && typeof val === 'object' && !isEmpty(val)) {
      out.push(`${indent}${key}:`);
      emit(val, Array.isArray(val) ? indent : `${indent}  `, out);
    } else {
      out.push(`${indent}${key}: ${isEmpty(val) ? emptyText(val) : scalar(val)}`);
    }
  }
}

/** A JSON value as a YAML document. */
export function toYAML(v: unknown): string {
  if (v === null || typeof v !== 'object') return `${scalar(v)}\n`;
  const out: string[] = [];
  emit(v, '', out);
  return `${out.join('\n')}\n`;
}
