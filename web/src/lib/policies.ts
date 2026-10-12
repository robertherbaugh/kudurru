// What the policy pages need to know about policies, groups and clients
// without asking the server: references, schedules (internal/policy
// schedule.go ported), the state badge, a PolicyState derived from the
// objects when GET /policies/{ns}/{name}/state is not served, and the
// client identification and policy selection of internal/policy (doc.go:
// tiers, priority order, merge, fallback) for the client page's "why".
import type { EnumOption } from '../components/fields/EnumSelect';
import type { BlockList, CatalogueCategory, CatalogueService, ClientGroup, ClientMatcher, Policy, PolicySpec, PolicyState, SafeSearchEngine } from '../api/types';
import { listPath, RESPONSE_OPTIONS } from './lists';
import { MANAGED_BY, objectEnabled } from './managedBy';
import { isIPv4, isIPv6 } from './rr';
import { effectiveServices, selectionOf } from './serviceCatalogue';

export const DEFAULT_NAMESPACE = 'default';
/** The bootstrap fallback policy (internal/policy DefaultPolicyName). */
export const FALLBACK_POLICY = 'default';
/** The bootstrap group that matches every client. */
export const DEFAULT_GROUP = 'default';

// ---------------------------------------------------------------------------
// References

/** "namespace/name" of an object. */
export function refOfObject(o: { metadata: { name: string; namespace?: string } }): string {
  return `${o.metadata.namespace ?? DEFAULT_NAMESPACE}/${o.metadata.name}`;
}

/** Splits "ns/name" (or a bare name in namespace ns). */
export function splitRef(ref: string, ns = DEFAULT_NAMESPACE): { namespace: string; name: string } {
  const i = ref.indexOf('/');
  return i >= 0 ? { namespace: ref.slice(0, i), name: ref.slice(i + 1) } : { namespace: ns, name: ref };
}

/** A ref without the "default/" prefix, for display. */
export function shortRef(ref: string): string {
  return ref.startsWith(`${DEFAULT_NAMESPACE}/`) ? ref.slice(DEFAULT_NAMESPACE.length + 1) : ref;
}

/** The detail page of a policy, list or group ref ("ns/name"). */
export function objectHref(kind: 'policies' | 'lists' | 'groups', ref: string, ns = DEFAULT_NAMESPACE): string {
  const { namespace, name } = splitRef(ref, ns);
  if (kind === 'lists') return listPath({ metadata: { name, namespace } });
  return `/${kind}/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}`;
}

export function clientHref(key: string): string {
  return `/clients/${encodeURIComponent(key)}`;
}

// ---------------------------------------------------------------------------
// Typed choices

/** The block responses of a Policy: the list choices, worded for a policy (no customIP field of its own). */
export const BLOCK_RESPONSES: EnumOption[] = RESPONSE_OPTIONS.map((o) =>
  o.value === 'customIP'
    ? { ...o, help: 'Answer with the address of blocking.customIP (Settings).' }
    : o.value === 'blockPage'
      ? { ...o, help: 'Answer with the block page address (Settings → Block page)' }
      : o,
);

export function blockResponseLabel(v: string | undefined): string {
  return BLOCK_RESPONSES.find((o) => o.value === v)?.label ?? (v || 'Default (from settings)');
}

export const SAFE_SEARCH_MODES: EnumOption[] = [
  { value: 'strict', label: 'Strict', help: 'Every engine at its strictest level.' },
  { value: 'moderate', label: 'Moderate (YouTube)', help: 'YouTube Restricted Mode moderate; every other engine stays strict.' },
];

/** Weekdays in the order the schedule editor shows them (the API takes mon..sun, any case). */
export const WEEKDAYS = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'] as const;
const WEEKDAY_NUM: Record<string, number> = { sun: 0, sunday: 0, mon: 1, monday: 1, tue: 2, tues: 2, tuesday: 2, wed: 3, wednesday: 3, thu: 4, thur: 4, thurs: 4, thursday: 4, fri: 5, friday: 5, sat: 6, saturday: 6 };

/** The weekday number (0 = Sunday) of a schedule weekday, or undefined. */
export function weekdayNumber(d: string): number | undefined {
  return WEEKDAY_NUM[d.trim().toLowerCase()];
}

// ---------------------------------------------------------------------------
// Schedules (internal/policy/schedule.go)

export type Schedule = NonNullable<PolicySpec['schedule']>;

const CLOCK = /^([01]?\d|2[0-3]):[0-5]\d$/;

/** A message for an invalid schedule, null when the server accepts it. */
export function scheduleError(s: Schedule): string | null {
  for (const d of s.weekdays ?? []) if (weekdayNumber(d) === undefined) return `Unknown weekday "${d}"`;
  if (s.start && !CLOCK.test(s.start)) return 'Start is HH:MM';
  if (s.end && !CLOCK.test(s.end) && s.end !== '24:00') return 'End is HH:MM';
  if (s.tz && !validTimeZone(s.tz)) return `Unknown time zone "${s.tz}"`;
  return null;
}

export function validTimeZone(tz: string): boolean {
  try {
    new Intl.DateTimeFormat('en-US', { timeZone: tz });
    return true;
  } catch {
    return false;
  }
}

function minutes(v: string | undefined, fallback: number): number {
  if (!v) return fallback;
  const [h, m] = v.split(':').map(Number) as [number, number];
  return h * 60 + m;
}

interface Compiled {
  days: number;
  start: number;
  end: number;
  fmt: Intl.DateTimeFormat;
}

const SHORT_DAY: Record<string, number> = { Sun: 0, Mon: 1, Tue: 2, Wed: 3, Thu: 4, Fri: 5, Sat: 6 };

function compile(s: Schedule): Compiled | null {
  if (scheduleError(s)) return null;
  let days = 0;
  for (const d of s.weekdays ?? []) days |= 1 << (weekdayNumber(d) ?? 0);
  if (days === 0) days = 0x7f;
  const fmt = new Intl.DateTimeFormat('en-US', { timeZone: s.tz || 'UTC', weekday: 'short', hour: '2-digit', minute: '2-digit', hourCycle: 'h23' });
  return { days, start: minutes(s.start, 0), end: minutes(s.end, 1440), fmt };
}

function activeAt(c: Compiled, t: Date): boolean {
  const parts = c.fmt.formatToParts(t);
  const get = (type: string) => parts.find((p) => p.type === type)?.value ?? '';
  const wd = SHORT_DAY[get('weekday')] ?? 0;
  const now = Number(get('hour')) * 60 + Number(get('minute'));
  if (c.start < c.end) return (c.days & (1 << wd)) !== 0 && now >= c.start && now < c.end;
  // Crosses midnight (or 24 hours when start == end): the evening belongs
  // to today, the morning to yesterday's window.
  if (now >= c.start && (c.days & (1 << wd)) !== 0) return true;
  return now < c.end && (c.days & (1 << ((wd + 6) % 7))) !== 0;
}

/** Whether the schedule allows t; an invalid schedule applies at all times (as on the server). */
export function scheduleActive(s: Schedule | undefined, t: Date): boolean {
  if (!s) return true;
  const c = compile(s);
  return c ? activeAt(c, t) : true;
}

/** The next minute the schedule switches on or off, within 8 days. */
export function nextScheduleChange(s: Schedule | undefined, from: Date): Date | undefined {
  const c = s ? compile(s) : null;
  if (!c) return undefined;
  const base = Math.floor(from.getTime() / 60_000) * 60_000;
  const now = activeAt(c, new Date(base));
  const step = 15 * 60_000;
  for (let t = base + step; t <= base + 8 * 86_400_000; t += step) {
    if (activeAt(c, new Date(t)) !== now) {
      // Refine to the minute inside the last step.
      for (let m = t - step + 60_000; m <= t; m += 60_000) if (activeAt(c, new Date(m)) !== now) return new Date(m);
      return new Date(t);
    }
  }
  return undefined;
}

/** "Sun–Thu 21:00–07:00 (America/Chicago)" style. */
export function scheduleSummary(s: Schedule | undefined): string {
  if (!s) return 'Always';
  const days = (s.weekdays ?? []).map((d) => d.slice(0, 1).toUpperCase() + d.slice(1, 3).toLowerCase());
  const daysText = days.length === 0 || days.length === 7 ? 'Every day' : days.join(' ');
  return `${daysText} ${s.start || '00:00'}–${s.end || '24:00'}${s.tz ? ` (${s.tz})` : ' (UTC)'}`;
}

// ---------------------------------------------------------------------------
// State

export type PolicyBadgeKey = 'active' | 'scheduled-off' | 'disabled' | 'shadowed';

export interface PolicyBadge {
  key: PolicyBadgeKey;
  label: string;
  variant: 'success' | 'neutral' | 'warning' | 'info';
  help: string;
}

/** The one badge a policy's state reduces to. */
export function policyBadge(s: Pick<PolicyState, 'enabled' | 'active' | 'shadowedBy'>): PolicyBadge {
  if (!s.enabled) return { key: 'disabled', label: 'Disabled', variant: 'neutral', help: 'Stored but not applied (kudurru.io/enabled: "false").' };
  if (!s.active) return { key: 'scheduled-off', label: 'Scheduled off', variant: 'info', help: 'Outside its schedule right now.' };
  if (s.shadowedBy?.length) {
    return { key: 'shadowed', label: 'Shadowed', variant: 'warning', help: `Active, but ${s.shadowedBy.map(shortRef).join(', ')} rank higher for the same groups; their settings win, lists are merged.` };
  }
  return { key: 'active', label: 'Active', variant: 'success', help: 'Applies to its groups right now.' };
}

export interface DeriveContext {
  policies: Policy[];
  groups: ClientGroup[];
  lists: BlockList[];
  services?: CatalogueService[];
  categories?: CatalogueCategory[];
  engines?: SafeSearchEngine[];
  /** The configured upstream group names, when known. */
  upstreamGroups?: string[];
  now: Date;
}

/** Whether a policy is in force at now (enabled and inside its schedule). */
export function policyActive(p: Policy, now: Date): boolean {
  return objectEnabled(p) && scheduleActive(p.spec.schedule, now);
}

/**
 * Enabled policies that share a group with p and rank above it (priority,
 * then namespace/name), as internal/policy ShadowedBy: schedules are not
 * considered.
 */
export function shadowingPolicies(p: Policy, all: Policy[]): string[] {
  const ns = p.metadata.namespace ?? DEFAULT_NAMESPACE;
  const groups = new Set(p.spec.clientGroupRefs ?? []);
  return sortByPriority(all)
    .filter((o) => refOfObject(o) !== refOfObject(p) && (o.metadata.namespace ?? DEFAULT_NAMESPACE) === ns && objectEnabled(o))
    .filter((o) => (o.spec.clientGroupRefs ?? []).some((g) => groups.has(g)))
    .filter((o) => ranksAbove(o, p))
    .map(refOfObject);
}

function ranksAbove(a: Policy, b: Policy): boolean {
  const pa = a.spec.priority ?? 0;
  const pb = b.spec.priority ?? 0;
  return pa !== pb ? pa > pb : refOfObject(a) < refOfObject(b);
}

/** Highest priority first, ties by namespace/name (internal/policy sortPolicies). */
export function sortByPriority(ps: Policy[]): Policy[] {
  return [...ps].sort((a, b) => (b.spec.priority ?? 0) - (a.spec.priority ?? 0) || refOfObject(a).localeCompare(refOfObject(b)));
}

/** The policies naming a group (same namespace), highest priority first. */
export function policiesOfGroup(g: ClientGroup, policies: Policy[]): Policy[] {
  const ns = g.metadata.namespace ?? DEFAULT_NAMESPACE;
  return sortByPriority(policies.filter((p) => (p.metadata.namespace ?? DEFAULT_NAMESPACE) === ns && (p.spec.clientGroupRefs ?? []).includes(g.metadata.name)));
}

/**
 * A PolicyState computed from the objects, for servers without
 * GET /policies/{ns}/{name}/state: the server's rules (internal/api
 * policies.go) without the query-log parts (stats24h, seen24h) and with
 * the block response left empty when the policy does not set one. Group
 * and list refs are the spec's names, as the server answers them.
 */
export function derivePolicyState(p: Policy, ctx: DeriveContext): PolicyState {
  const ns = p.metadata.namespace ?? DEFAULT_NAMESPACE;
  const s = p.spec;
  const enabled = objectEnabled(p);
  const schedErr = s.schedule ? scheduleError(s.schedule) : null;
  const schedOn = scheduleActive(s.schedule, ctx.now);
  const next = nextScheduleChange(s.schedule, ctx.now);
  const findIn = <T extends { metadata: { name: string; namespace?: string } }>(xs: T[], name: string) =>
    xs.find((x) => x.metadata.name === name && (x.metadata.namespace ?? DEFAULT_NAMESPACE) === ns);
  const listState = (name: string, role: 'block' | 'allow'): PolicyState['lists'][number] => {
    const l = findIn(ctx.lists, name);
    if (!l) return { ref: name, role, mode: role, exists: false, enabled: false, ready: false, entries: 0, sourcesEnabled: 0, sourcesTotal: 0 };
    const on = objectEnabled(l);
    return {
      ref: name,
      role,
      mode: l.spec.mode ?? 'block',
      exists: true,
      enabled: on,
      ready: on && (l.status?.conditions ?? []).some((c) => c.type === 'Ready' && c.status === 'True'),
      entries: l.status?.entries ?? 0,
      sourcesEnabled: l.spec.sources.filter((src) => src.enabled !== false).length,
      sourcesTotal: l.spec.sources.length,
    };
  };
  const shadowed = shadowingPolicies(p, ctx.policies);
  const svc = new Map((ctx.services ?? []).map((x) => [x.id, x]));
  const icons = (x: { icon?: string; iconOnDark?: string } | undefined) => ({ ...(x?.icon ? { icon: x.icon } : {}), ...(x?.iconOnDark ? { iconOnDark: x.iconOnDark } : {}) });
  const cats = new Map((ctx.categories ?? []).map((x) => [x.id, x]));
  const serviceState = (id: string) => {
    const c = svc.get(id);
    return { id, name: c?.name ?? id, known: !!c, rules: c?.rules ?? 0, ...icons(c) };
  };
  return {
    policy: `${ns}/${p.metadata.name}`,
    enabled,
    active: enabled && schedOn,
    ...(s.schedule
      ? { schedule: schedErr ? { now: true, summary: 'invalid schedule', error: schedErr } : { now: schedOn, ...(next ? { nextChange: next.toISOString() } : {}), summary: scheduleSummary(s.schedule) } }
      : {}),
    priority: s.priority ?? 0,
    ...(shadowed.length ? { shadowedBy: shadowed } : {}),
    ...(p.metadata.annotations?.[MANAGED_BY] ? { managedBy: p.metadata.annotations[MANAGED_BY] } : {}),
    groups: (s.clientGroupRefs ?? []).map((name) => {
      const g = findIn(ctx.groups, name);
      return { ref: name, exists: !!g, enabled: !!g && objectEnabled(g), clients: g?.spec.clients?.length ?? 0 };
    }),
    lists: [...(s.blockListRefs ?? []).map((n) => listState(n, 'block')), ...(s.allowListRefs ?? []).map((n) => listState(n, 'allow'))],
    services: (s.blockedServices ?? []).map(serviceState),
    allowedServices: (s.allowedServices ?? []).map(serviceState),
    categories: (s.blockedCategories ?? []).map((id) => {
      const c = cats.get(id);
      const members = (ctx.services ?? []).filter((x) => x.category === id);
      const allowed = members.filter((x) => (s.allowedServices ?? []).includes(x.id)).length;
      return { id, name: c?.name ?? id, known: !!c, services: members.length, allowed, ...(c?.icon ? { icon: c.icon } : {}) };
    }),
    effectiveServices: effectiveServices(selectionOf(s), ctx.services ?? []).map((id) => {
      const c = svc.get(id);
      return {
        id,
        name: c?.name ?? id,
        known: !!c,
        rules: c?.rules ?? 0,
        via: (s.blockedServices ?? []).includes(id) ? ('service' as const) : ('category' as const),
        origin: c?.origin ?? 'kudurru',
        ...(c ? { category: c.category } : {}),
        ...icons(c),
      };
    }),
    safeSearch: {
      enabled: s.safeSearch === true,
      mode: s.safeSearchMode === 'moderate' ? 'moderate' : 'strict',
      engines: (ctx.engines ?? []).map((e) => ({ id: e.id, name: e.name, ...icons(e) })),
    },
    ...(s.upstreamGroup ? { upstreamGroup: { name: s.upstreamGroup, exists: ctx.upstreamGroups ? ctx.upstreamGroups.includes(s.upstreamGroup) : true } } : {}),
    blockMode: s.blockMode ?? '',
    ...(s.blockTTL ? { blockTTL: s.blockTTL } : {}),
    conditions: p.status?.conditions ?? [],
  };
}

// ---------------------------------------------------------------------------
// Addresses

function v4(s: string): bigint | null {
  if (!isIPv4(s)) return null;
  return s.split('.').reduce((acc, o) => (acc << 8n) | BigInt(Number(o)), 0n);
}

function v6(s: string): bigint | null {
  if (!isIPv6(s)) return null;
  let str = s;
  let tail: bigint[] = [];
  const last = str.lastIndexOf(':');
  if (str.slice(last + 1).includes('.')) {
    const t = v4(str.slice(last + 1));
    if (t === null) return null;
    tail = [t >> 16n, t & 0xffffn];
    str = str.slice(0, last + 1);
    if (!str.endsWith('::')) str = str.slice(0, -1);
  }
  const dbl = str.indexOf('::');
  const head = (dbl >= 0 ? str.slice(0, dbl) : str).split(':').filter(Boolean);
  const rest = dbl >= 0 ? str.slice(dbl + 2).split(':').filter(Boolean) : [];
  const fill = dbl >= 0 ? 8 - head.length - rest.length - tail.length : 0;
  if (fill < 0) return null;
  const groups = [...head.map((x) => BigInt(parseInt(x, 16))), ...new Array<bigint>(fill).fill(0n), ...rest.map((x) => BigInt(parseInt(x, 16))), ...tail];
  if (groups.length !== 8) return null;
  return groups.reduce((acc, g) => (acc << 16n) | g, 0n);
}

/** An address as a number with its family, or null. IPv4-mapped IPv6 reads as IPv4. */
export function parseAddr(s: string): { family: 4 | 6; value: bigint } | null {
  const a = v4(s.trim());
  if (a !== null) return { family: 4, value: a };
  const b = v6(s.trim());
  if (b === null) return null;
  if (b >> 32n === 0xffffn) return { family: 4, value: b & 0xffffffffn };
  return { family: 6, value: b };
}

/** Prefix length of a CIDR containing addr, or -1. A bare address is a host prefix. */
export function cidrMatch(cidr: string, addr: string): number {
  const [net, bitsText] = cidr.trim().split('/');
  const a = parseAddr(addr);
  const n = parseAddr(net ?? '');
  if (!a || !n || a.family !== n.family) return -1;
  const width = a.family === 4 ? 32 : 128;
  const bits = bitsText === undefined ? width : Number(bitsText);
  if (!Number.isInteger(bits) || bits < 0 || bits > width) return -1;
  const shift = BigInt(width - bits);
  return a.value >> shift === n.value >> shift ? bits : -1;
}

// ---------------------------------------------------------------------------
// Client identification and policy selection (internal/policy doc.go)

export type MatchTier = 'ip' | 'cidr' | 'clientID' | 'catchAll' | 'default' | 'none';

export interface Membership {
  /** "namespace/name", sorted. */
  groups: string[];
  tier: MatchTier;
  /** The matcher that put the client in each group. */
  matchers: Record<string, ClientMatcher | undefined>;
}

/**
 * The groups a client (an IP address or a client ID) joins: the first tier
 * with a match wins (exact IP, longest CIDR, client ID, catch-all CIDRs,
 * then the default group). MAC and hostname matchers need sources the
 * dashboard does not see, so they are not evaluated here.
 */
export function identifyClient(key: string, groups: ClientGroup[]): Membership {
  const enabled = groups.filter((g) => objectEnabled(g));
  const isAddr = parseAddr(key) !== null;
  const pick = (tier: MatchTier, hits: [ClientGroup, ClientMatcher | undefined][]): Membership => {
    const matchers: Record<string, ClientMatcher | undefined> = {};
    for (const [g, m] of hits) matchers[refOfObject(g)] ??= m;
    return { groups: Object.keys(matchers).sort(), tier, matchers };
  };
  const each = (fn: (m: ClientMatcher) => boolean) => enabled.flatMap((g) => (g.spec.clients ?? []).filter(fn).map((m) => [g, m] as [ClientGroup, ClientMatcher]));
  if (isAddr) {
    const exact = each((m) => !!m.ip && cidrMatch(m.ip, key) >= 0);
    if (exact.length) return pick('ip', exact);
    let best = 0;
    let hits: [ClientGroup, ClientMatcher][] = [];
    for (const [g, m] of each((x) => !!x.cidr)) {
      const bits = cidrMatch(m.cidr ?? '', key);
      if (bits <= 0) continue;
      if (bits > best) {
        best = bits;
        hits = [];
      }
      if (bits === best) hits.push([g, m]);
    }
    if (hits.length) return pick('cidr', hits);
  } else {
    const id = key.trim().toLowerCase();
    const byID = each((m) => !!m.clientID && m.clientID.trim().toLowerCase() === id);
    if (byID.length) return pick('clientID', byID);
  }
  if (isAddr) {
    const catchAll = each((m) => !!m.cidr && cidrMatch(m.cidr, key) === 0);
    if (catchAll.length) return pick('catchAll', catchAll);
  }
  const def = enabled.find((g) => g.metadata.name === DEFAULT_GROUP && (g.metadata.namespace ?? DEFAULT_NAMESPACE) === DEFAULT_NAMESPACE);
  return def ? pick('default', [[def, undefined]]) : { groups: [], tier: 'none', matchers: {} };
}

export const TIER_TEXT: Record<MatchTier, string> = {
  ip: 'its exact IP address',
  cidr: 'the longest matching CIDR',
  clientID: 'its client ID',
  catchAll: 'a catch-all range (0.0.0.0/0 or ::/0)',
  default: 'nothing more specific (the default group matches everyone)',
  none: 'nothing: no group exists',
};

export interface Candidate {
  policy: Policy;
  ref: string;
  active: boolean;
  /** Why it is not active: "disabled" or "scheduled off". */
  off?: 'disabled' | 'scheduled off';
}

export interface Selection {
  candidates: Candidate[];
  /** The policy whose settings decide (block response, upstream group, ...). */
  winner?: Candidate;
  /** Every active policy: their lists are merged. */
  active: Candidate[];
  /** True when no candidate is active and the fallback policy applies. */
  fallback: boolean;
}

/** The policies of a membership at now, in the order internal/policy ranks them. */
export function selectPolicy(groupRefs: string[], policies: Policy[], now: Date): Selection {
  const wanted = new Set(groupRefs);
  const cand = sortByPriority(policies)
    .filter((p) => (p.spec.clientGroupRefs ?? []).some((g) => wanted.has(`${p.metadata.namespace ?? DEFAULT_NAMESPACE}/${g}`)))
    .map((p): Candidate => {
      const enabled = objectEnabled(p);
      const sched = scheduleActive(p.spec.schedule, now);
      return { policy: p, ref: refOfObject(p), active: enabled && sched, ...(!enabled ? { off: 'disabled' as const } : !sched ? { off: 'scheduled off' as const } : {}) };
    });
  const active = cand.filter((c) => c.active);
  if (active.length) return { candidates: cand, winner: active[0], active, fallback: false };
  const fb = policies.find((p) => p.metadata.name === FALLBACK_POLICY && (p.metadata.namespace ?? DEFAULT_NAMESPACE) === DEFAULT_NAMESPACE);
  if (fb && policyActive(fb, now)) {
    const c: Candidate = { policy: fb, ref: refOfObject(fb), active: true };
    return { candidates: cand, winner: c, active: [c], fallback: true };
  }
  return { candidates: cand, active: [], fallback: false };
}
