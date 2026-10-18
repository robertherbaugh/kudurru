// Upstream configuration helpers. There is no upstream API: groups,
// bootstrap resolvers, conditional forwarding, ECS and the recursive
// resolver's options are the cluster settings keys upstreams.*
// (internal/config, internal/upstream), read with GET /settings and written
// with PATCH /settings as JSON merge patches.
import { useQuery } from '@tanstack/react-query';
import { qk, useApi, useCan } from '../api/context';
import type { SettingsBody } from '../api/types';
import { parseDuration } from './format';
import { getPath } from './settings';
import { hostError, isIPv4, isIPv6 } from './rr';

/** The group used when no policy, zone or conditional rule names one; it must exist. */
export const DEFAULT_GROUP = 'default';

/** How a group resolves: forward to its servers (the default) or recursively from the root servers. */
export type UpstreamMode = 'forward' | 'recursive';

export const UPSTREAM_MODES = [
  { value: 'forward', label: 'Forward', help: 'Send queries to the servers you list, such as your router, Cloudflare or Quad9.' },
  {
    value: 'recursive',
    label: 'Recursive',
    help: 'kudurru resolves names itself, starting from the root servers, so no single provider sees every name, and validates DNSSEC. It needs outbound DNS (UDP and TCP port 53) to the internet.',
  },
] as const;

/** The one server of a recursive group in GET /stats/upstreams, metrics and the query log (<group>/recursive). */
export const RECURSIVE_SERVER = 'recursive';

/** The timeout a group gets when it sets none: per server when forwarding, per lookup when recursive. */
export function defaultTimeout(mode: UpstreamMode): string {
  return mode === 'recursive' ? '4s' : '2s';
}

/** The group's timeout, or its mode's default. */
export function timeoutOf(g: UpstreamGroupConfig): string {
  return g.timeout || defaultTimeout(g.mode);
}

/** The server entries health and traffic are reported for: the configured servers, or the recursive resolver. */
export function serverEntries(g: UpstreamGroupConfig): string[] {
  return g.mode === 'recursive' ? [RECURSIVE_SERVER] : g.servers;
}

export const STRATEGIES = [
  { value: 'parallel_best', label: 'Parallel best', help: 'Race two healthy servers and take the first usable answer (default).' },
  { value: 'strict', label: 'Strict order', help: 'Ask the servers in order, moving on only on failure.' },
  { value: 'random', label: 'Random', help: 'Ask one healthy server at random.' },
  { value: 'rtt_weighted', label: 'Fastest first', help: 'Pick by smoothed round-trip time, weighted at random.' },
] as const;

/** The label of a strategy ("" is the default, parallel_best). */
export function strategyLabelOf(s: string): string {
  return STRATEGIES.find((x) => x.value === (s || 'parallel_best'))?.label ?? s;
}

export const ECS_MODES = [
  { value: 'strip', label: 'Strip (never send a client subnet)' },
  { value: 'forward', label: "Forward the client's own option" },
  { value: 'add', label: 'Forward, or derive one from a public client address' },
] as const;

export interface UpstreamGroupConfig {
  mode: UpstreamMode;
  servers: string[];
  strategy: string;
  timeout: string;
}

/** upstreams.recursion: options of every recursive group. */
export interface RecursionConfig {
  dnssec: boolean;
  qnameMinimization: boolean;
  ipv6: string;
  trustAnchors: string[];
  rootHints: string[];
}

export const RECURSION_IPV6 = [
  { value: 'auto', label: 'Auto', help: 'Use IPv6 when this host has an IPv6 route to the root servers.' },
  { value: 'on', label: 'On', help: 'Always ask IPv6 name servers too.' },
  { value: 'off', label: 'Off', help: 'Ask name servers over IPv4 only.' },
] as const;

export interface UpstreamsConfig {
  groups: Record<string, UpstreamGroupConfig>;
  bootstrap: string[];
  conditional: Record<string, string>;
  ecs: { mode: string; ipv4Prefix: number; ipv6Prefix: number };
  recursion: RecursionConfig;
}

function strings(v: unknown): string[] {
  return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : [];
}

function record(v: unknown): Record<string, unknown> {
  return v && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : {};
}

/** The upstreams section of GET /settings, with missing parts filled in. */
export function upstreamsOf(body: SettingsBody | undefined): UpstreamsConfig {
  const u = record(getPath(body?.settings, 'upstreams'));
  const groups: Record<string, UpstreamGroupConfig> = {};
  for (const [name, g] of Object.entries(record(u.groups))) {
    const r = record(g);
    groups[name] = {
      mode: r.mode === 'recursive' ? 'recursive' : 'forward',
      servers: strings(r.servers),
      strategy: typeof r.strategy === 'string' ? r.strategy : '',
      timeout: typeof r.timeout === 'string' ? r.timeout : '',
    };
  }
  const conditional: Record<string, string> = {};
  for (const [k, v] of Object.entries(record(u.conditional))) if (typeof v === 'string') conditional[k] = v;
  const ecs = record(u.ecs);
  const rec = record(u.recursion);
  return {
    groups,
    bootstrap: strings(u.bootstrap),
    conditional,
    ecs: {
      mode: typeof ecs.mode === 'string' && ecs.mode ? ecs.mode : 'strip',
      ipv4Prefix: typeof ecs.ipv4Prefix === 'number' ? ecs.ipv4Prefix : 24,
      ipv6Prefix: typeof ecs.ipv6Prefix === 'number' ? ecs.ipv6Prefix : 56,
    },
    recursion: {
      dnssec: rec.dnssec !== false,
      qnameMinimization: rec.qnameMinimization !== false,
      ipv6: typeof rec.ipv6 === 'string' && rec.ipv6 ? rec.ipv6 : 'auto',
      trustAnchors: strings(rec.trustAnchors),
      rootHints: strings(rec.rootHints),
    },
  };
}

/**
 * The merge patch value that saves a group as shown: a recursive group
 * drops its servers and strategy (the server refuses servers in recursive
 * mode), a forwarding group drops the mode, so switching either way never
 * leaves the other mode's fields behind.
 */
export function groupPatch(g: UpstreamGroupConfig): Record<string, unknown> {
  if (g.mode === 'recursive') return { mode: 'recursive', servers: null, strategy: null, timeout: g.timeout };
  return { mode: null, servers: g.servers.map((s) => s.trim()).filter(Boolean), strategy: g.strategy, timeout: g.timeout };
}

/** Group names, "default" first. */
export function groupNames(cfg: UpstreamsConfig): string[] {
  return Object.keys(cfg.groups).sort((a, b) => (a === DEFAULT_GROUP ? -1 : b === DEFAULT_GROUP ? 1 : a.localeCompare(b)));
}

/** Splits the optional display name off a server entry ("cloudflare=tls://1.1.1.1"). */
export function splitServerName(entry: string): { name: string; addr: string } {
  const e = entry.trim();
  const i = e.indexOf('=');
  if (i > 0 && !/[:/[\]]/.test(e.slice(0, i))) return { name: e.slice(0, i), addr: e.slice(i + 1).trim() };
  return { name: '', addr: e };
}

/** The label metrics, the query log and GET /stats/upstreams use for a server. */
export function serverLabel(entry: string): string {
  const { name, addr } = splitServerName(entry);
  return name || addr;
}

const SCHEMES = ['udp', 'tcp', 'tls', 'https', 'quic', 'h3', 'sdns'];

function portError(p: string): string | null {
  if (!/^\d{1,5}$/.test(p) || Number(p) < 1 || Number(p) > 65535) return 'The port must be 1-65535';
  return null;
}

/** host[:port], [v6]:port or a bare IPv6 address; host is an IP or a host name. */
export function hostPortError(s: string, opts: { ipOnly?: boolean } = {}): string | null {
  if (!s) return 'An address is required';
  if (s.startsWith('[')) {
    const end = s.indexOf(']');
    if (end < 0 || !isIPv6(s.slice(1, end))) return 'Not an IPv6 address in brackets';
    const rest = s.slice(end + 1);
    if (rest === '') return null;
    return rest.startsWith(':') ? portError(rest.slice(1)) : 'Expected ":port" after "]"';
  }
  if (isIPv6(s)) return null;
  const i = s.lastIndexOf(':');
  const host = i >= 0 ? s.slice(0, i) : s;
  if (i >= 0) {
    const pe = portError(s.slice(i + 1));
    if (pe) return pe;
  }
  if (isIPv4(host)) return null;
  if (opts.ipOnly) return 'An IP address (with an optional :port)';
  if (/^\d+(\.\d+)*$/.test(host)) return 'Not an IPv4 address';
  return hostError(host) ? 'Not an IP address or host name' : null;
}

/**
 * Validates one server entry the way internal/upstream accepts it
 * (dnsproxy syntax): ip[:port], host[:port], udp://, tcp://, tls://,
 * https://, quic://, h3:// and sdns:// stamps, optionally prefixed with a
 * display name ("cloudflare=tls://1.1.1.1").
 */
export function serverError(entry: string): string | null {
  const t = entry.trim();
  if (!t) return 'A server address is required';
  const { name, addr } = splitServerName(t);
  if (t.includes('=') && !name && /^[^:/[\]]*=/.test(t)) return 'The display name before "=" is empty';
  if (/\s/.test(addr)) return 'No spaces inside an address';
  const m = /^([a-z0-9]+):\/\/(.*)$/i.exec(addr);
  if (!m) {
    if (addr.includes('/')) return 'Use a scheme such as tls:// or https://';
    return hostPortError(addr);
  }
  const scheme = (m[1] ?? '').toLowerCase();
  const rest = m[2] ?? '';
  if (!SCHEMES.includes(scheme)) return `Unknown scheme ${scheme}://; use ${SCHEMES.map((s) => `${s}://`).join(', ')}`;
  if (scheme === 'sdns') return /^[A-Za-z0-9_-]{10,}={0,2}$/.test(rest) ? null : 'Not a DNS stamp (sdns:// and base64url)';
  if (scheme === 'https' || scheme === 'h3') {
    try {
      const u = new URL(`https://${rest}`);
      if (!u.hostname) return 'The URL has no host';
    } catch {
      return 'Not a valid URL';
    }
    return null;
  }
  if (rest.includes('/')) return `${scheme}:// takes host[:port] only`;
  return hostPortError(rest);
}

/** A bootstrap resolver is a plain IP resolver: ip[:port]. */
export function bootstrapError(entry: string): string | null {
  const t = entry.trim();
  if (!t) return null;
  return hostPortError(t.replace(/^udp:\/\//, ''), { ipOnly: true });
}

/** A group name: letters, digits, "-", "_" and "." (it is a map key in YAML too). */
export function groupNameError(name: string): string | null {
  if (!name) return 'Required';
  if (!/^[A-Za-z0-9][A-Za-z0-9_.-]*$/.test(name)) return 'Letters, digits, "-", "_" and "."';
  return null;
}

/** A conditional-forwarding key: a domain suffix, or several in dnsmasq form "[/a.example/b.example/]". */
export function conditionalKeyError(key: string): string | null {
  const t = key.trim();
  if (!t) return 'A domain suffix is required';
  const domains = t.startsWith('[') ? t.replace(/^\[\/?|\/?\]$/g, '').split('/').filter(Boolean) : [t];
  if (!domains.length) return 'No domain inside [/…/]';
  for (const d of domains) {
    const e = hostError(d, { allowRoot: false });
    if (e) return `${d}: ${e}`;
  }
  return null;
}

export function timeoutError(v: string): string | null {
  if (!v.trim()) return null;
  const s = parseDuration(v.trim());
  if (Number.isNaN(s)) return 'A duration such as 2s or 500ms';
  return s <= 0 ? 'Must be positive' : null;
}

/** One server per line; blank lines dropped. */
export function parseServers(text: string): string[] {
  return text
    .split(/\r?\n/)
    .map((s) => s.trim())
    .filter(Boolean);
}

/** A merge patch that replaces a map: changed keys are set, removed keys become null. */
export function mapPatch<T>(before: Record<string, T>, after: Record<string, T>): Record<string, T | null> {
  const out: Record<string, T | null> = {};
  for (const k of Object.keys(before)) if (!(k in after)) out[k] = null;
  for (const [k, v] of Object.entries(after)) if (JSON.stringify(before[k]) !== JSON.stringify(v)) out[k] = v;
  return out;
}

/** The upstream group names for selects (policy and forward-zone forms). */
export function useUpstreamGroups(): { names: string[]; isLoading: boolean; available: boolean } {
  const api = useApi();
  const can = useCan();
  const readable = can('settings', 'read');
  const q = useQuery({ queryKey: qk.settings, queryFn: () => api.settings.get(), enabled: readable, staleTime: 30_000 });
  return { names: q.data ? groupNames(upstreamsOf(q.data)) : [], isLoading: q.isLoading, available: readable && !!q.data };
}
