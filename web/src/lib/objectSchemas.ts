// Field descriptors for the CRUD forms of BlockList, ClientGroup and
// Policy (§10 principle 6). They follow api/v1alpha1 field for field; the
// generic ObjectForm renders them, so a new spec field is one line here.
// When api/openapi.json lands these can be derived from its schemas.
import type { ObjectKind } from '../api/types';
import { parseDuration } from './format';
import { isIPv4, isIPv6 } from './rr';

export type FieldDef =
  | { type: 'text'; key: string; label: string; help?: string; placeholder?: string; required?: boolean; validate?: (v: string) => string | null }
  | { type: 'number'; key: string; label: string; help?: string; min?: number; max?: number }
  | { type: 'bool'; key: string; label: string; help?: string }
  | { type: 'enum'; key: string; label: string; help?: string; options: { value: string; label: string }[]; required?: boolean }
  | { type: 'duration'; key: string; label: string; help?: string; placeholder?: string }
  | { type: 'strings'; key: string; label: string; help?: string; placeholder?: string; validate?: (v: string) => string | null }
  | { type: 'refs'; key: string; label: string; help?: string; refKind: ObjectKind; filter?: (o: { spec?: unknown }) => boolean }
  | { type: 'objects'; key: string; label: string; help?: string; itemLabel: string; fields: FieldDef[]; summary?: (item: Record<string, unknown>) => string }
  | { type: 'object'; key: string; label: string; help?: string; fields: FieldDef[] }
  /** Service IDs of the blocked-services catalogue (GET /catalogue/services). */
  | { type: 'services'; key: string; label: string; help?: string }
  | { type: 'upstreamGroup'; key: string; label: string; help?: string };

export interface KindSchema {
  kind: ObjectKind;
  /** The Kind of the object (apiVersion kudurru.io/v1alpha1). */
  objectKind: 'BlockList' | 'ClientGroup' | 'Policy' | 'DNSZone' | 'DNSRecordSet';
  singular: string;
  plural: string;
  namespaced: boolean;
  /** The RBAC resource guarding writes. */
  resource: string;
  fields: FieldDef[];
  /** A new object's spec. */
  empty: () => Record<string, unknown>;
}

/** A string field of an untyped form value ("" for anything else). */
function str(v: unknown): string {
  return typeof v === 'string' ? v : '';
}

const DNS_NAME = /^(\*\.)?([A-Za-z0-9_]([A-Za-z0-9_-]{0,61}[A-Za-z0-9_])?\.)*[A-Za-z0-9_]([A-Za-z0-9_-]{0,61}[A-Za-z0-9_])?\.?$/;

/** A Kubernetes object name (RFC 1123 subdomain). */
export function nameError(name: string): string | null {
  if (!name) return 'Required';
  if (name.length > 253) return 'At most 253 characters';
  if (!/^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$/.test(name)) {
    return 'Lower-case letters, digits, "-" and "."; must start and end with a letter or digit';
  }
  return null;
}

export function durationError(v: string): string | null {
  if (!v) return null;
  return Number.isNaN(parseDuration(v)) ? 'A duration such as 30m, 24h or 1h30m' : null;
}

export function cidrError(v: string): string | null {
  if (!v) return null;
  const [addr, bits, extra] = v.split('/');
  if (extra !== undefined || bits === undefined || !/^\d+$/.test(bits)) return 'An address/prefix such as 192.168.1.0/24';
  const n = Number(bits);
  if (isIPv4(addr ?? '')) return n <= 32 ? null : 'IPv4 prefix length is at most 32';
  if (isIPv6(addr ?? '')) return n <= 128 ? null : 'IPv6 prefix length is at most 128';
  return 'Not an IP address before the "/"';
}

export function ipError(v: string): string | null {
  if (!v) return null;
  return isIPv4(v) || isIPv6(v) ? null : 'Not an IPv4 or IPv6 address';
}

export function macError(v: string): string | null {
  if (!v) return null;
  return /^([0-9a-f]{2}[:-]){5}[0-9a-f]{2}$/i.test(v) ? null : 'A MAC address such as aa:bb:cc:dd:ee:ff';
}

/** Domain, wildcard, adblock or /regex/ list entry (format-agnostic check). */
export function listEntryError(v: string): string | null {
  const t = v.trim();
  if (!t || t.startsWith('#') || t.startsWith('!')) return null;
  if (t.startsWith('/') && t.endsWith('/') && t.length > 2) {
    try {
      new RegExp(t.slice(1, -1));
      return null;
    } catch (e) {
      return `Invalid regular expression: ${(e as Error).message}`;
    }
  }
  if (t.startsWith('||') || t.startsWith('@@')) return null;
  const host = t.split(/\s+/).pop() ?? t;
  return DNS_NAME.test(host) ? null : `"${t}" is not a domain name`;
}

const RESPONSES = [
  { value: '', label: 'Default (from settings)' },
  { value: 'nxdomain', label: 'NXDOMAIN' },
  { value: 'zeroIP', label: '0.0.0.0 / ::' },
  { value: 'refused', label: 'REFUSED' },
  { value: 'customIP', label: 'Custom IP' },
  { value: 'blockPage', label: 'Block page' },
];

export const SCHEMAS: Record<'blocklists' | 'clientgroups' | 'policies', KindSchema> = {
  blocklists: {
    kind: 'blocklists',
    objectKind: 'BlockList',
    singular: 'list',
    plural: 'lists',
    namespaced: true,
    resource: 'blocklists',
    empty: () => ({ mode: 'block', sources: [{ name: 'main', url: '', format: 'auto' }] }),
    fields: [
      {
        type: 'enum',
        key: 'mode',
        label: 'Mode',
        options: [
          { value: 'block', label: 'Block list' },
          { value: 'allow', label: 'Allow list' },
        ],
        required: true,
      },
      {
        type: 'objects',
        key: 'sources',
        label: 'Sources',
        itemLabel: 'source',
        help: 'A URL, inline entries or a ConfigMap; every source is fetched and merged.',
        summary: (s) => str(s.url) || (Array.isArray(s.inline) ? `${s.inline.length} inline entries` : '') || str(s.name) || 'source',
        fields: [
          { type: 'text', key: 'name', label: 'Name', placeholder: 'main' },
          { type: 'text', key: 'url', label: 'URL', placeholder: 'https://example.com/hosts.txt', validate: (v) => (!v || /^https?:\/\/\S+$/.test(v) ? null : 'An http(s) URL') },
          {
            type: 'enum',
            key: 'format',
            label: 'Format',
            options: [
              { value: 'auto', label: 'Detect' },
              { value: 'hosts', label: 'hosts file' },
              { value: 'domains', label: 'One domain per line' },
              { value: 'adblock', label: 'Adblock syntax' },
              { value: 'regex', label: 'Regular expressions' },
            ],
          },
          { type: 'strings', key: 'inline', label: 'Inline entries', help: 'One per line; paste many at once.', placeholder: 'ads.example.com', validate: listEntryError },
        ],
      },
      { type: 'duration', key: 'refreshInterval', label: 'Refresh interval', placeholder: '24h', help: 'How often URL sources are re-fetched.' },
      { type: 'enum', key: 'response', label: 'Block response', options: RESPONSES },
      {
        type: 'object',
        key: 'customIP',
        label: 'Custom block IP',
        help: 'Used when the response is "Custom IP".',
        fields: [
          { type: 'text', key: 'v4', label: 'IPv4', placeholder: '192.0.2.1', validate: (v) => (!v || isIPv4(v) ? null : 'Not an IPv4 address') },
          { type: 'text', key: 'v6', label: 'IPv6', placeholder: '2001:db8::1', validate: (v) => (!v || isIPv6(v) ? null : 'Not an IPv6 address') },
        ],
      },
    ],
  },
  clientgroups: {
    kind: 'clientgroups',
    objectKind: 'ClientGroup',
    singular: 'group',
    plural: 'groups',
    namespaced: true,
    resource: 'clientgroups',
    empty: () => ({ clients: [] }),
    fields: [
      {
        type: 'objects',
        key: 'clients',
        label: 'Clients',
        itemLabel: 'client',
        help: 'A client matches when any one field matches. Paste many addresses into "Add many".',
        summary: (c) => str(c.name) || str(c.ip) || str(c.cidr) || str(c.mac) || str(c.clientID) || str(c.hostname) || str(c.nameGlob) || 'client',
        fields: [
          { type: 'text', key: 'ip', label: 'IP address', validate: ipError },
          { type: 'text', key: 'cidr', label: 'CIDR', validate: cidrError },
          { type: 'text', key: 'mac', label: 'MAC address', validate: macError },
          { type: 'text', key: 'clientID', label: 'Client ID (DoT/DoH/DoQ)' },
          { type: 'text', key: 'hostname', label: 'Host name' },
          { type: 'text', key: 'nameGlob', label: 'Name pattern', placeholder: 'kids-*' },
          { type: 'text', key: 'name', label: 'Display name' },
          { type: 'text', key: 'comment', label: 'Comment' },
        ],
      },
    ],
  },
  policies: {
    kind: 'policies',
    objectKind: 'Policy',
    singular: 'policy',
    plural: 'policies',
    namespaced: true,
    resource: 'policies',
    empty: () => ({ clientGroupRefs: [], blockListRefs: [], allowListRefs: [], priority: 100 }),
    fields: [
      { type: 'refs', key: 'clientGroupRefs', label: 'Client groups', refKind: 'clientgroups', help: 'Clients in these groups get this policy.' },
      { type: 'refs', key: 'blockListRefs', label: 'Block lists', refKind: 'blocklists', filter: (o) => (o.spec as { mode?: string } | undefined)?.mode !== 'allow' },
      { type: 'refs', key: 'allowListRefs', label: 'Allow lists', refKind: 'blocklists', filter: (o) => (o.spec as { mode?: string } | undefined)?.mode === 'allow' },
      { type: 'bool', key: 'allowOnly', label: 'Allow only', help: 'Block everything that is not on an allow list.' },
      { type: 'number', key: 'priority', label: 'Priority', min: 0, max: 100000, help: 'Higher wins when several policies match a client.' },
      { type: 'upstreamGroup', key: 'upstreamGroup', label: 'Upstream group' },
      { type: 'enum', key: 'blockMode', label: 'Block response', options: RESPONSES },
      { type: 'duration', key: 'blockTTL', label: 'Block TTL', placeholder: '1m' },
      { type: 'bool', key: 'queryLog', label: 'Log queries' },
      {
        type: 'services',
        key: 'blockedServices',
        label: 'Blocked services',
        help: 'Block every domain of these services, like a block list. Allow lists still win; the query log names the service.',
      },
      {
        type: 'bool',
        key: 'safeSearch',
        label: 'Enforce SafeSearch',
        help: "Answer Google, Bing, DuckDuckGo, Brave, Yandex, Pixabay and YouTube with their vendor's safe-search address.",
      },
      {
        type: 'enum',
        key: 'safeSearchMode',
        label: 'SafeSearch level',
        help: 'Only YouTube has two levels (Restricted Mode strict or moderate); every other engine is always strict.',
        options: [
          { value: 'strict', label: 'Strict' },
          { value: 'moderate', label: 'Moderate (YouTube)' },
        ],
      },
      {
        type: 'object',
        key: 'schedule',
        label: 'Schedule',
        help: 'Only active in this window (e.g. school nights).',
        fields: [
          { type: 'strings', key: 'weekdays', label: 'Weekdays', placeholder: 'Mon', validate: (v) => (/^(Mon|Tue|Wed|Thu|Fri|Sat|Sun)$/.test(v) ? null : 'Mon, Tue, Wed, Thu, Fri, Sat or Sun') },
          { type: 'text', key: 'start', label: 'Start', placeholder: '21:00', validate: (v) => (!v || /^([01]\d|2[0-3]):[0-5]\d$/.test(v) ? null : 'HH:MM') },
          { type: 'text', key: 'end', label: 'End', placeholder: '07:00', validate: (v) => (!v || /^([01]\d|2[0-3]):[0-5]\d$/.test(v) ? null : 'HH:MM') },
          { type: 'text', key: 'tz', label: 'Time zone', placeholder: 'Europe/Berlin' },
        ],
      },
      {
        type: 'objects',
        key: 'rewrites',
        label: 'Rewrites',
        itemLabel: 'rewrite',
        summary: (r) => `${str(r.name)} → ${str(r.cname) || (Array.isArray(r.targets) ? r.targets.map(str).join(', ') : '')}`,
        fields: [
          { type: 'text', key: 'name', label: 'Name', required: true, placeholder: 'printer.lan' },
          { type: 'text', key: 'type', label: 'Type', placeholder: 'A' },
          { type: 'strings', key: 'targets', label: 'Targets', placeholder: '192.168.1.50' },
          { type: 'text', key: 'cname', label: 'CNAME', placeholder: 'host.example.com.' },
        ],
      },
    ],
  },
};
