// Settings metadata the Settings page renders from (C1). GET
// /settings/_schema carries enum, min/max, presets, format, ref, items,
// keys, values, fields, group and label per key (internal/config
// schema.go). A server from before C1 sends none of them: keys then fall
// back to a section from the path's first segment, a label from the path
// and a control from the bare type (text, JSON for lists and maps), which
// is what the page did before.
import type { SettingKey, SettingMeta, SettingShape } from '../api/types';

/** UI-only additions to a shape: what a map's keys are called. */
export type UiShape = SettingShape & { keyLabel?: string; keyPlaceholder?: string };

export type UiMeta = SettingMeta & { keyLabel?: string; keyPlaceholder?: string };

/** What the map keys of a few keys are, which the schema does not name. */
const KEY_LABELS: Record<string, { keyLabel: string; keyPlaceholder?: string }> = {
  'upstreams.groups': { keyLabel: 'Group' },
  'upstreams.conditional': { keyLabel: 'Domain suffix', keyPlaceholder: 'corp.example' },
  'ratelimit.ipv4': { keyLabel: 'Prefix length', keyPlaceholder: '24' },
  'ratelimit.ipv6': { keyLabel: 'Prefix length', keyPlaceholder: '64' },
  'externalDNS.webhook.zoneSelector': { keyLabel: 'Label' },
};

/** Sections for keys without a group (servers before C1), by the path's first segment. */
const SECTION_GROUPS: Record<string, string> = {
  node: 'Node',
  store: 'Node',
  objects: 'Node',
  objectsDir: 'Node',
  settingsFile: 'Node',
  listeners: 'Listeners',
  upstreams: 'Upstreams',
  cache: 'Cache',
  blocking: 'Blocking',
  blockPage: 'Block page',
  querylog: 'Query log',
  ratelimit: 'Rate limiting',
  auth: 'Security',
  rbac: 'Security',
  notify: 'Zone transfers',
  tsigKeys: 'Zone transfers',
  externalDNS: 'Integrations',
  log: 'Logging',
};

/** The order sections appear in: config.Groups of internal/config/schema.go; others follow alphabetically. */
export const GROUP_ORDER = [
  'Node',
  'Listeners',
  'Upstreams',
  'Cache',
  'Blocking',
  'Block page',
  'Query log',
  'Rate limiting',
  'Security',
  'Zone transfers',
  'Integrations',
  'Logging',
];

/** "maxDBBytes" → "max DB bytes", "customIP" → "custom IP". */
function words(seg: string): string {
  return seg
    .replace(/([a-z0-9])([A-Z])/g, '$1 $2')
    .replace(/([A-Z]+)([A-Z][a-z])/g, '$1 $2')
    .split(' ')
    .map((w) => (/^[A-Z0-9]+$/.test(w) && w.length > 1 ? w : w.toLowerCase()))
    .join(' ');
}

/** A label from the path when nothing names one: "cache.serveStale.ttl" → "Serve stale ttl". */
export function labelFromPath(path: string): string {
  const segs = path.split('.');
  const s = (segs.length > 1 ? segs.slice(1) : segs).map(words).join(' ');
  return s.charAt(0).toUpperCase() + s.slice(1);
}

/** The metadata of a key: its C1 fields (nulls dropped) and the map-key labels. */
export function metaFor(k: SettingKey): UiMeta {
  const m: UiMeta = { ...KEY_LABELS[k.path] };
  for (const f of ['enum', 'min', 'max', 'presets', 'format', 'ref', 'items', 'keys', 'values', 'fields', 'group', 'label'] as const) {
    if (k[f] !== undefined && k[f] !== null) (m as Record<string, unknown>)[f] = k[f];
  }
  return m;
}

export function groupOf(k: SettingKey, m: UiMeta = metaFor(k)): string {
  return m.group || (k.path.startsWith('listeners.blockPage.') ? 'Block page' : (SECTION_GROUPS[k.path.split('.')[0] ?? ''] ?? 'Other'));
}

export function labelOf(k: SettingKey, m: UiMeta = metaFor(k)): string {
  return m.label || labelFromPath(k.path);
}

/** The shape a key's value is edited with. */
export function shapeOf(k: SettingKey, m: UiMeta = metaFor(k)): UiShape {
  const { group: _g, label: _l, ...shape } = m;
  return { type: k.type, ...shape };
}

/** Sorts section names by GROUP_ORDER, then alphabetically. */
export function compareGroups(a: string, b: string): number {
  const ia = GROUP_ORDER.indexOf(a);
  const ib = GROUP_ORDER.indexOf(b);
  if (ia >= 0 && ib >= 0) return ia - ib;
  if (ia >= 0) return -1;
  if (ib >= 0) return 1;
  return a.localeCompare(b);
}
