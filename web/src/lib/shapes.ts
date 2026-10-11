// Editing values by their C1 shape. A value becomes a draft the controls
// edit (numbers as text so half-typed input survives, maps as [key, value]
// rows so a new or duplicate key is representable), shapeError() checks a
// draft the way internal/config would, and fromDraft() turns it back into
// the API value. Object fields the shape does not name pass through.
import { bytesError } from '../components/fields/BytesField';
import { durationError, sameDuration } from '../components/fields/DurationField';
import { formatError } from '../components/fields/formats';
import { numberError } from '../components/fields/NumberField';
import type { UiShape } from './settingsMeta';

/** A scalar as text; objects and arrays are "" (they have their own editors). */
export function str(v: unknown): string {
  if (typeof v === 'string') return v;
  if (typeof v === 'number' || typeof v === 'boolean') return String(v);
  return '';
}

export type MapDraft = { __rows: [string, unknown][] };

function isRecord(v: unknown): v is Record<string, unknown> {
  return !!v && typeof v === 'object' && !Array.isArray(v);
}

export function isMapDraft(v: unknown): v is MapDraft {
  return isRecord(v) && Array.isArray(v.__rows);
}

/** Whether a shape can be edited structurally (lists and maps need an element shape). */
export function editable(s: UiShape): boolean {
  switch (s.type) {
    case 'list':
      return !!s.items && editable(s.items);
    case 'map':
      return !!s.values && editable(s.values);
    case 'object':
      return !!s.fields?.length && s.fields.every(editable);
    default:
      return true;
  }
}

/** The draft of a fresh element (a new list item or map value). */
export function emptyDraft(s: UiShape): unknown {
  switch (s.type) {
    case 'bool':
      return false;
    case 'int':
      return '';
    case 'string[]':
      return [];
    case 'list':
      return [];
    case 'map':
      return { __rows: [] } satisfies MapDraft;
    case 'object':
      return Object.fromEntries((s.fields ?? []).map((f) => [f.name, f.enum?.[0]?.value ?? emptyDraft(f)]));
    default:
      return s.enum?.[0]?.value ?? '';
  }
}

export function toDraft(s: UiShape, v: unknown): unknown {
  switch (s.type) {
    case 'bool':
      return v === true;
    case 'int':
      return str(v);
    case 'string[]':
      return Array.isArray(v) ? v.map((x) => str(x)) : [];
    case 'list':
      return Array.isArray(v) && s.items ? v.map((x) => toDraft(s.items as UiShape, x)) : v;
    case 'map':
      return isRecord(v) && s.values ? { __rows: Object.entries(v).map(([k, x]) => [k, toDraft(s.values as UiShape, x)]) } : v;
    case 'object': {
      const o = isRecord(v) ? { ...v } : {};
      for (const f of s.fields ?? []) o[f.name] = toDraft(f, o[f.name]);
      return o;
    }
    case 'bytes':
      return v ?? '';
    default:
      return v === undefined || v === null ? '' : v;
  }
}

/** The API value of a draft; empty optional object fields are dropped. */
export function fromDraft(s: UiShape, d: unknown): unknown {
  switch (s.type) {
    case 'int': {
      const t = str(d).trim();
      return t === '' ? undefined : Number(t);
    }
    case 'string[]':
      return Array.isArray(d) ? d.map((x) => str(x).trim()).filter(Boolean) : d;
    case 'list':
      return Array.isArray(d) && s.items ? d.map((x) => fromDraft(s.items as UiShape, x)) : d;
    case 'map':
      return isMapDraft(d) && s.values ? Object.fromEntries(d.__rows.map(([k, x]) => [k.trim(), fromDraft(s.values as UiShape, x)])) : d;
    case 'object': {
      if (!isRecord(d)) return d;
      const o: Record<string, unknown> = { ...d };
      for (const f of s.fields ?? []) {
        const v = fromDraft(f, o[f.name]);
        if (v === undefined || v === '' || (isRecord(v) && Object.keys(v).length === 0)) delete o[f.name];
        else o[f.name] = v;
      }
      return o;
    }
    case 'string':
      return typeof d === 'string' ? d.trim() : d;
    default:
      return d;
  }
}

/** The first problem of a draft, prefixed with where it is; null when valid. */
export function shapeError(s: UiShape, d: unknown, where = ''): string | null {
  const at = (msg: string) => (where ? `${where}: ${msg}` : msg);
  switch (s.type) {
    case 'int': {
      const e = numberError(str(d), { min: s.min, max: s.max, optional: !!where });
      return e ? at(e) : null;
    }
    case 'duration': {
      const e = durationError(str(d), { allowEmpty: !!where });
      return e ? at(e) : null;
    }
    case 'bytes': {
      const e = bytesError(d);
      return e ? at(e) : null;
    }
    case 'string': {
      const v = str(d);
      if (s.enum?.length && v && !s.enum.some((o) => o.value === v)) return at(`One of ${s.enum.map((o) => o.value).join(', ')}`);
      const e = formatError(s.format, v);
      return e ? at(e) : null;
    }
    case 'string[]': {
      const list = Array.isArray(d) ? d.map((x) => str(x).trim()).filter(Boolean) : [];
      for (let i = 0; i < list.length; i++) {
        const v = list[i] as string;
        const e = s.items?.enum?.length && !s.items.enum.some((o) => o.value === v) ? 'not one of the choices' : formatError(s.items?.format, v);
        if (e) return at(`${v}: ${e}`);
      }
      return null;
    }
    case 'list': {
      if (!Array.isArray(d) || !s.items) return null;
      for (let i = 0; i < d.length; i++) {
        const e = shapeError(s.items, d[i], `${where ? `${where} ` : ''}#${i + 1}`);
        if (e) return e;
      }
      return null;
    }
    case 'map': {
      if (!isMapDraft(d) || !s.values) return null;
      const seen = new Set<string>();
      for (const [k, v] of d.__rows) {
        const key = k.trim();
        if (!key) return at(`${s.keyLabel ?? 'A key'} is empty`);
        if (seen.has(key)) return at(`${key} is listed twice`);
        seen.add(key);
        const e = shapeError(s.values, v, `${where ? `${where} ` : ''}${key}`);
        if (e) return e;
      }
      return null;
    }
    case 'object': {
      if (!isRecord(d)) return null;
      for (const f of s.fields ?? []) {
        const e = shapeError(f, d[f.name], `${where ? `${where} ` : ''}${f.label ?? f.name}`);
        if (e) return e;
      }
      return null;
    }
    default:
      return null;
  }
}

/** Whether two API values are the same (durations and sizes by meaning, not spelling). */
export function sameValue(type: string, a: unknown, b: unknown): boolean {
  if (type === 'duration' && typeof a === 'string' && typeof b === 'string') {
    return durationError(a) === null && durationError(b) === null ? sameDuration(a, b) : a === b;
  }
  return JSON.stringify(normalise(a)) === JSON.stringify(normalise(b));
}

/** Sorted object keys so {a,b} equals {b,a}; undefined and [] / {} compare as empty. */
function normalise(v: unknown): unknown {
  if (Array.isArray(v)) return v.map(normalise);
  if (isRecord(v)) return Object.fromEntries(Object.keys(v).sort().map((k) => [k, normalise(v[k])]));
  return v ?? null;
}
