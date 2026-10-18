// Settings metadata helpers (§10 principle 10, D24). The "All settings"
// page is generated from GET /settings/_schema; values come from
// GET /settings (cluster keys) and GET /node/config (node keys) as nested
// objects keyed by the dotted schema path.
import type { SettingKey } from '../api/types';
import { parseDuration } from './format';

/** KUDURRU_<SECTION>__<KEY>, the environment form of a settings path. */
export function envVarName(path: string): string {
  return `KUDURRU_${path.split('.').map((s) => s.toUpperCase()).join('__')}`;
}

/** Reads a dotted path out of a nested object. */
export function getPath(obj: unknown, path: string): unknown {
  let cur: unknown = obj;
  for (const seg of path.split('.')) {
    if (cur === null || typeof cur !== 'object') return undefined;
    // nosemgrep: prototype-pollution-loop -- reads only; nothing is assigned through the path
    cur = (cur as Record<string, unknown>)[seg];
  }
  return cur;
}

/** A JSON merge patch (RFC 7396) setting one dotted path. */
export function mergePatchFor(path: string, value: unknown): Record<string, unknown> {
  const segs = path.split('.');
  const root: Record<string, unknown> = {};
  let cur = root;
  segs.forEach((seg, i) => {
    if (i === segs.length - 1) {
      cur[seg] = value;
    } else {
      const next: Record<string, unknown> = {};
      cur[seg] = next;
      cur = next;
    }
  });
  return root;
}

export type Lock = { locked: false } | { locked: true; reason: string };

/** Whether a key may be edited in the UI, and why not. */
export function lockFor(key: SettingKey, canWrite: boolean): Lock {
  if (key.hostOnly) {
    return {
      locked: true,
      reason: `Host-only: set it in kudurru.yaml or ${envVarName(key.path)} on the host; the API never accepts it.`,
    };
  }
  if (key.envLocked) {
    return { locked: true, reason: `Set by the environment variable ${envVarName(key.path)}; unset it to edit here.` };
  }
  if (!canWrite) {
    return { locked: true, reason: key.scope === 'node' ? 'Needs the node.config permission.' : 'Needs the settings.write permission.' };
  }
  return { locked: false };
}

/** The section a key belongs to: "querylog.retention.raw" → "querylog". */
export function sectionOf(path: string): string {
  return path.split('.')[0] ?? path;
}

function scalar(v: unknown): string {
  if (typeof v === 'string') return v;
  if (typeof v === 'number' || typeof v === 'boolean' || typeof v === 'bigint') return String(v);
  return JSON.stringify(v) ?? '';
}

/** Renders a value for display in its input. */
export function displayValue(key: SettingKey, value: unknown): string {
  if (value === undefined || value === null) return '';
  switch (key.type) {
    case 'string[]':
      return Array.isArray(value) ? value.map(scalar).join('\n') : scalar(value);
    case 'list':
    case 'map':
      return JSON.stringify(value, null, 2);
    default:
      return scalar(value);
  }
}

/** Parses an edited value; throws Error with a user-facing message. */
export function parseValue(key: SettingKey, text: string): unknown {
  const t = text.trim();
  switch (key.type) {
    case 'int': {
      if (!/^-?\d+$/.test(t)) throw new Error('Must be a whole number');
      return Number(t);
    }
    case 'bool':
      if (t !== 'true' && t !== 'false') throw new Error('true or false');
      return t === 'true';
    case 'duration':
      if (Number.isNaN(parseDuration(t))) throw new Error('A Go duration such as 30s, 10m or 1h30m');
      return t;
    case 'bytes':
      if (!/^\d+(\.\d+)?\s*(B|KB|MB|GB|TB|KiB|MiB|GiB|TiB)?$/i.test(t)) throw new Error('A size such as 500MB or 10GiB');
      return /^\d+$/.test(t) ? Number(t) : t;
    case 'string[]':
      return t === '' ? [] : t.split(/\n|,/).map((s) => s.trim()).filter(Boolean);
    case 'list':
    case 'map': {
      try {
        const v: unknown = JSON.parse(t || (key.type === 'list' ? '[]' : '{}'));
        if (key.type === 'list' && !Array.isArray(v)) throw new Error('not a list');
        if (key.type === 'map' && (typeof v !== 'object' || v === null || Array.isArray(v))) throw new Error('not an object');
        return v;
      } catch {
        throw new Error(key.type === 'list' ? 'A JSON array' : 'A JSON object');
      }
    }
    default:
      return text;
  }
}

/** Keys shown on the Basic tab; everything else is Expert. */
export const BASIC_KEYS = new Set([
  'upstreams.groups',
  'blocking.enabled',
  'blocking.defaultResponse',
  'blocking.blockTTL',
  'cache.maxEntries',
  'querylog.enabled',
  'querylog.privacy',
  'querylog.retention.raw',
  'querylog.retention.maxDBBytes',
  // The block page (C5); shown when the server has the keys.
  'blockPage.enabled',
  'blockPage.ipv4',
  'blockPage.ipv6',
  'blockPage.title',
  'blockPage.message',
  'blockPage.contact',
  'blockPage.showDetails',
  'listeners.blockPage.http',
  'listeners.blockPage.https',
]);
