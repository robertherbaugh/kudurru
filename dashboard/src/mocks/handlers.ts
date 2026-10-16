// MSW request handlers implementing the /api/v1 contract over the fixture
// database. They are deliberately close to the real server and answer its
// shapes (internal/api, internal/auth/stack): the error envelope, 401/403
// with missing{resource,verb,scope}, 409 managed_by with the owner, 412 on
// If-Match mismatch, keyset cursors, took_ms, the settings envelope and
// the presentation-form dig answer.
import { http, HttpResponse, sse, type JsonBodyType } from 'msw';
import type {
  AnyObject,
  ApiToken,
  ApplyResult,
  AuditEvent,
  BlockList,
  ClientGroup,
  ClientItem,
  DNSZone,
  DnsQueryRequest,
  DnsQueryResult,
  Endpoint,
  Me,
  ObjectKind,
  Policy,
  QueryRow,
  SourceStatus,
  SessionRecordInfo,
  StatusClass,
  TopDim,
  TopItem,
  ZoneRecord,
  ZoneSummary,
} from '../api/types';
import { toYAML } from '../lib/yaml';
import { createDB, CLIENTS, makeQueryRow, setPath, type MockDB } from './fixtures';
import {
  approveEntry,
  batchEntries,
  collectEntries,
  createEntries,
  deleteEntry,
  filterEntries,
  listInfos,
  patchEntry,
  releaseEntry,
  sourceItems,
  sourceKindOf,
  switchEntries,
  unattachedOf,
  WriteError,
} from './domains';
import { getPath } from '../lib/settings';
import { entryError, sourceName } from '../lib/lists';
import { derivePolicyState } from '../lib/policies';
import { rng } from './rand';
import { dhcpHandlers, resetDHCP } from './dhcp';
import type { UnblockAllow, UnblockApproval, UnblockDenial, UnblockRequest } from '../api/types';
import type { DomainAction, DomainBatch, DomainEntriesCreate, DomainEntryPatch, ListEntriesPatch } from '../api/types';

let db: MockDB = createDB();
// UI fixtures only: fixed codes deliberately do not model cryptographic authority.
const mockFactors = new Set<string>();
const mockSetups = new Map<string, { challenge: string; expiresAt: string }>();
// Safe UI fixtures only. Production ownership is the server's immutable UID check.
const mockSessions = new Map<string, SessionRecordInfo[]>();

function ownSessions(username: string): SessionRecordInfo[] {
  let rows = mockSessions.get(username);
  if (!rows) {
    const issued = db.now - 60_000;
    rows = [
      { id: `mock-session-${username}-other`, issuedAt: new Date(issued + 30_000).toISOString(), expiresAt: new Date(issued + 43_200_000).toISOString(), idleExpiresAt: new Date(issued + 1_800_000).toISOString(), current: false, revoked: false },
      { id: `mock-session-${username}-current`, issuedAt: new Date(issued).toISOString(), expiresAt: new Date(issued + 43_200_000).toISOString(), idleExpiresAt: new Date(issued + 1_800_000).toISOString(), current: true, revoked: false },
    ];
    mockSessions.set(username, rows);
  }
  return rows;
}

/** Resets the database to the fixtures (tests call it in beforeEach). */
export function resetDB(now?: number): MockDB {
  db = createDB(now);
  resetDHCP();
  mockFactors.clear();
  mockSetups.clear();
  mockSessions.clear();
  return db;
}

/** The live database, for assertions in tests. */
export function mockDB(): MockDB {
  return db;
}

/** Signs a user in without the login form (tests). */
export function signIn(username = 'admin'): void {
  const user = db.users.find((u) => u.username === username);
  db.session = user ? { user } : null;
}

const CSRF = 'csrf-mock-token';
const B = '*/api/v1';
const MANAGED = 'kudurru.io/managed-by';

function ok(body: Record<string, unknown> | unknown[], init?: ResponseInit) {
  const payload = Array.isArray(body) ? body : { ...body, took_ms: 1.2 };
  return HttpResponse.json(payload as JsonBodyType, init);
}

function fail(status: number, key: string, message: string, hint?: unknown, headers?: Record<string, string>, fields?: string[]) {
  return HttpResponse.json(
    { error: { key, message, ...(hint !== undefined ? { hint } : {}), ...(fields?.length ? { fields } : {}) }, took_ms: 0.4 } as JsonBodyType,
    { status, ...(headers ? { headers } : {}) },
  );
}

/** The 402 a feature without a key answers (internal/api requireFeature), or null when the license has it. */
function needsLicense(feature: string, what: string) {
  if (db.license.features.includes(feature)) return null;
  return fail(402, 'license_required', `${what} needs a license key`, 'add a license key (a free personal key covers 3 nodes; see https://kudurru.io/license); DNS keeps working meanwhile');
}

/** internal/rbac/permissions.yaml: resource -> verbs. */
export const RESOURCE_VERBS: Record<string, string[]> = {
  zones: ['read', 'write', 'delete', 'transfer', 'sign', 'share'],
  records: ['read', 'write', 'delete'],
  blocklists: ['read', 'write', 'delete', 'refresh'],
  policies: ['read', 'write', 'delete'],
  clientgroups: ['read', 'write', 'delete'],
  clients: ['read', 'write'],
  querylog: ['read', 'export', 'purge'],
  stats: ['read'],
  cache: ['read', 'flush'],
  blocking: ['read', 'toggle'],
  settings: ['read', 'write'],
  node: ['config'],
  dhcp: ['read', 'write', 'delete'],
  dhcpleases: ['read', 'import', 'export'],
  users: ['read', 'write', 'delete'],
  groups: ['read', 'write', 'delete'],
  serviceaccounts: ['read', 'write', 'delete'],
  tokens: ['read', 'write', 'delete', 'own'],
  identityproviders: ['read', 'write', 'delete'],
  rolebindings: ['read', 'write', 'delete'],
  integrations: ['read', 'write', 'delete', 'sync', 'check', 'secrets', 'act'],
  cluster: ['read', 'join', 'promote', 'drain'],
  edge: ['read', 'write'],
  audit: ['read', 'export'],
  unblockrequests: ['read', 'decide', 'delete'],
};

/** The viewer role of permissions.yaml plus querylog.read (the mock "viewer"). */
const VIEWER = [
  'zones.read', 'records.read', 'stats.read', 'cache.read', 'blocklists.read', 'policies.read', 'clientgroups.read',
  'clients.read', 'blocking.read', 'settings.read', 'tokens.own', 'querylog.read', 'cluster.read', 'dhcp.read',
];

/** The permission matrix GET /auth/me returns for a set of roles. */
export function permissions(roles: string[]): Record<string, string[]> {
  const out: Record<string, string[]> = {};
  if (roles.includes('admin')) {
    for (const [r, verbs] of Object.entries(RESOURCE_VERBS)) out[r] = [...verbs];
    return out;
  }
  for (const p of VIEWER) {
    const [r, v] = p.split('.') as [string, string];
    (out[r] ??= []).push(v);
  }
  // The auditor role reads unblock requests but cannot decide them.
  if (roles.includes('auditor')) {
    out.unblockrequests = ['read'];
    out.dhcpleases = ['read', 'export'];
  }
  return out;
}

function me(): Me {
  const u = db.session?.user;
  if (!u) throw new Error('no session');
  return {
    principal: { kind: 'User', id: `local:${u.username}`, name: u.name, groups: u.roles.includes('admin') ? ['kudurru-admins'] : [], auth: 'cookie', provider: 'local' },
    roles: u.roles,
    csrf: CSRF,
    permissions: permissions(u.roles),
    ...(u.mustChangePassword ? { mustChangePassword: true } : {}),
  };
}

type Guard = Response | null;

/** 401 without a session; 403 (with the missing permission) for viewers. */
function guard(request: Request, resource: string, verb: string): Guard {
  if (!db.session) return fail(401, 'unauthenticated', 'Sign in to continue');
  if (request.method !== 'GET' && request.headers.get('X-Kudurru-CSRF') !== CSRF) {
    return fail(403, 'csrf', 'Missing or wrong X-Kudurru-CSRF header');
  }
  const perms = permissions(db.session.user.roles)[resource] ?? [];
  if (!perms.includes(verb)) {
    return fail(403, 'forbidden', `You may not ${verb} ${resource}`, { missing: { resource, verb, scope: 'cluster' } });
  }
  return null;
}

function scopeInfo(url: URL) {
  const node = url.searchParams.get('node');
  const names = node ? [node] : db.nodes.map((n) => n.name);
  return { scope: node ? 'node' : 'cluster', nodes: names.map((name) => ({ name, ok: true, took_ms: 2.1 })), partial: false };
}

// ---------------------------------------------------------------------------
// Objects

function collection(kind: ObjectKind): AnyObject[] {
  switch (kind) {
    case 'dnszones':
      return db.zones;
    case 'dnsrecordsets':
      return db.recordSets;
    case 'blocklists':
      return db.lists;
    case 'clientgroups':
      return db.groups;
    case 'policies':
      return db.policies;
  }
}

const KIND_NAMES: Record<ObjectKind, AnyObject['kind']> = {
  dnszones: 'DNSZone',
  dnsrecordsets: 'DNSRecordSet',
  blocklists: 'BlockList',
  clientgroups: 'ClientGroup',
  policies: 'Policy',
};

const RESOURCE_OF: Record<ObjectKind, string> = {
  dnszones: 'zones',
  dnsrecordsets: 'records',
  blocklists: 'blocklists',
  clientgroups: 'clientgroups',
  policies: 'policies',
};

function isKind(k: string): k is ObjectKind {
  return k in KIND_NAMES;
}

function find(kind: ObjectKind, name: string, namespace?: string): AnyObject | undefined {
  return collection(kind).find((o) => o.metadata.name === name && (kind === 'dnszones' || (o.metadata.namespace ?? 'default') === (namespace ?? 'default')));
}

function validateObject(kind: ObjectKind, obj: AnyObject): string | null {
  if (!obj.metadata?.name || !/^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$/.test(obj.metadata.name)) {
    return 'metadata.name: must be a lower-case RFC 1123 name';
  }
  if (!obj.spec) return 'spec: required';
  if (kind === 'blocklists') {
    const s = (obj as BlockList).spec;
    if (!Array.isArray(s.sources) || s.sources.length === 0) return 'spec.sources: at least one source is required';
    for (const [i, src] of s.sources.entries()) {
      if (!src.url && !src.inline?.length && !src.configMapRef) return `spec.sources[${i}]: one of url, inline or configMapRef is required`;
      if (src.url && !/^https?:\/\//.test(src.url)) return `spec.sources[${i}].url: must be an http(s) URL`;
    }
  }
  if (kind === 'dnszones') {
    const s = (obj as DNSZone).spec;
    if (!s.zoneName) return 'spec.zoneName: required';
  }
  if (kind === 'clientgroups') {
    for (const [i, c] of ((obj as ClientGroup).spec.clients ?? []).entries()) {
      if (c.cidr && !/^[0-9a-fA-F.:]+\/\d{1,3}$/.test(c.cidr)) return `spec.clients[${i}].cidr: not a CIDR`;
    }
  }
  return null;
}

/** Whole-object write with manager "api"; mirrors store semantics. */
function writeObject(kind: ObjectKind, obj: AnyObject, ifMatch: string | null): { status: number; body: unknown; result?: 'created' | 'updated' | 'unchanged' } {
  const err = validateObject(kind, obj);
  if (err) return { status: 400, body: { error: { key: 'invalid', message: err } } };
  const ns = kind === 'dnszones' ? undefined : obj.metadata.namespace ?? 'default';
  const existing = find(kind, obj.metadata.name, ns);
  const list = collection(kind);
  if (existing) {
    const owner = existing.metadata.annotations?.[MANAGED] ?? '';
    if (owner && owner !== 'api' && owner !== 'bootstrap') {
      return { status: 409, body: { error: { key: 'managed_by', message: `${KIND_NAMES[kind]} ${obj.metadata.name} is managed by ${owner}`, hint: owner } } };
    }
    if (ifMatch && ifMatch !== existing.metadata.resourceVersion) {
      return { status: 412, body: { error: { key: 'precondition_failed', message: `resourceVersion ${ifMatch} is stale (now ${existing.metadata.resourceVersion})` } } };
    }
    if (JSON.stringify(existing.spec) === JSON.stringify(obj.spec) && JSON.stringify(existing.metadata.annotations ?? {}) === JSON.stringify({ ...(obj.metadata.annotations ?? {}), [MANAGED]: 'api' })) {
      return { status: 200, body: existing, result: 'unchanged' };
    }
  }
  db.rv += 1;
  const next = {
    ...obj,
    apiVersion: 'kudurru.io/v1alpha1',
    kind: KIND_NAMES[kind],
    metadata: {
      ...(existing?.metadata ?? {}),
      ...obj.metadata,
      ...(ns ? { namespace: ns } : {}),
      resourceVersion: String(db.rv),
      generation: (existing?.metadata.generation ?? 0) + 1,
      creationTimestamp: existing?.metadata.creationTimestamp ?? new Date(db.now).toISOString(),
      annotations: { ...(obj.metadata.annotations ?? {}), [MANAGED]: 'api' },
    },
    status: existing?.status ?? { conditions: [{ type: 'Ready', status: 'Unknown', reason: 'Pending', message: 'waiting for the controller' }] },
  } as AnyObject;
  if (kind === 'blocklists') compileList(next as BlockList);
  if (existing) list.splice(list.indexOf(existing), 1, next);
  else list.push(next);
  audit(`${KIND_NAMES[kind].toLowerCase()}.${existing ? 'update' : 'create'}`, { kind: KIND_NAMES[kind], ...(ns ? { namespace: ns } : {}), name: obj.metadata.name });
  return { status: existing ? 200 : 201, body: next, result: existing ? 'updated' : 'created' };
}

/**
 * The status the list controller reports after a write, as far as the mock
 * can tell without fetching: inline sources count their entries, URL and
 * ConfigMap sources keep their last counts, disabled sources (ux-plan C2)
 * carry enabled: false, no error and count nothing, and a list whose every
 * source is off is Ready with "all sources disabled".
 */
function compileList(l: BlockList): void {
  const prev = l.status ?? {};
  const sources = l.spec.sources.map((src, i): SourceStatus => {
    const name = sourceName(i, src);
    const { enabled: _e, error, ...old }: SourceStatus = prev.sources?.find((s) => s.name === name) ?? { name };
    const on = src.enabled !== false;
    const excluded = src.entries ? src.entries.filter((x) => x.enabled === false).length : src.inline ? (src.exclude ?? []).filter((x) => src.inline?.includes(x)).length : (old.excluded ?? 0);
    const entries = src.entries ? src.entries.length - excluded : src.inline ? src.inline.length - excluded : (old.entries ?? 0);
    const { excluded: _x, ...rest } = old;
    return { ...rest, name, entries, ...(excluded ? { excluded } : {}), ...(on ? (error ? { error } : {}) : { enabled: false }) };
  });
  const live = sources.filter((s) => s.enabled !== false);
  const entries = live.reduce((n, s) => n + (s.entries ?? 0), 0);
  let conditions = prev.conditions;
  if (!live.length) conditions = [{ type: 'Ready', status: 'True', reason: 'AllSourcesDisabled', message: 'all sources disabled', lastTransitionTime: new Date(db.now).toISOString() }];
  else if (conditions?.[0]?.reason === 'AllSourcesDisabled' || (conditions?.[0]?.reason === 'Pending' && l.spec.sources.every((s) => s.inline))) {
    conditions = [{ type: 'Ready', status: 'True', reason: 'AsExpected', message: '', lastTransitionTime: new Date(db.now).toISOString() }];
  }
  l.status = { ...prev, entries, ruleCount: entries, sources, ...(conditions ? { conditions } : {}) };
}

/** Records an audit event of the signed-in user (newest first, like GET /audit?order=desc). */
function audit(action: string, resource: AuditEvent['resource'], result = 'success'): void {
  const u = db.session?.user;
  const id = (db.audit[0]?.id ?? 1000) + 1;
  db.audit.unshift({
    id,
    ts: new Date().toISOString(),
    node: 'kudurru-0',
    actor: u ? { kind: 'User', id: `local:${u.username}`, name: u.name } : { name: 'anonymous' },
    action,
    resource,
    result,
    authz: 'binding',
    request: { ip: '192.168.1.10' },
  });
}

function deleteObject(kind: ObjectKind, name: string, namespace?: string): { status: number; body?: unknown } {
  const obj = find(kind, name, namespace);
  if (!obj) return { status: 404, body: { error: { key: 'not_found', message: `${KIND_NAMES[kind]} ${name} not found` } } };
  const owner = obj.metadata.annotations?.[MANAGED] ?? '';
  if (owner && owner !== 'api' && owner !== 'bootstrap') {
    return { status: 409, body: { error: { key: 'managed_by', message: `${KIND_NAMES[kind]} ${name} is managed by ${owner}`, hint: owner } } };
  }
  if (kind === 'dnszones') {
    // D148: a zone goes alone only when no record set references it.
    if ((obj as DNSZone).spec.zoneName === '.') {
      return { status: 409, body: { error: { key: 'default_zone', message: `zone ${name} is the root zone "." and cannot be deleted` } } };
    }
    const refs = db.recordSets.filter((rs) => rs.spec.zoneRef.name === name);
    if (refs.length) {
      const objects = refs.map((rs) => ({ kind: 'DNSRecordSet', namespace: rs.metadata.namespace ?? 'default', name: rs.metadata.name }));
      return { status: 409, body: { error: { key: 'zone_not_empty', message: `zone ${name} is referenced by ${refs.length} record sets`, hint: `DELETE /api/v1/zones/${name} deletes the zone with its record sets`, objects } } };
    }
  }
  const list = collection(kind);
  list.splice(list.indexOf(obj), 1);
  return { status: 204 };
}

function listEnvelope<T>(items: T[], url: URL) {
  const limit = Number(url.searchParams.get('limit') ?? '0');
  const start = Number(url.searchParams.get('continue') ?? '0');
  if (limit > 0) {
    const page = items.slice(start, start + limit);
    const next = start + limit < items.length ? String(start + limit) : undefined;
    return ok({ items: page, ...(next ? { continue: next } : {}) });
  }
  return ok({ items });
}

// ---------------------------------------------------------------------------
// Zones

function zoneSummary(z: DNSZone): ZoneSummary {
  const ready = (z.status?.conditions ?? []).find((c) => c.type === 'Ready');
  return {
    name: z.metadata.name,
    zoneName: z.spec.zoneName,
    type: z.spec.type,
    ...(z.spec.type === 'secondary' && db.secondaryRuntime[z.metadata.name] ? { secondaryRuntime: db.secondaryRuntime[z.metadata.name] } : {}),
    ...(z.spec.type === 'stub' && db.stubRuntime[z.metadata.name] ? { stubRuntime: db.stubRuntime[z.metadata.name] } : {}),
    serial: z.status?.serial ?? 0,
    recordCount: z.status?.recordCount ?? 0,
    recordSets: db.recordSets.filter((rs) => rs.spec.zoneRef.name === z.metadata.name).length,
    ready: ready?.status === 'True',
    ...(ready?.reason ? { reason: ready.reason } : {}),
    ...(ready?.message ? { message: ready.message } : {}),
    managedBy: z.metadata.annotations?.[MANAGED] ?? '',
    ...(z.metadata.resourceVersion ? { resourceVersion: z.metadata.resourceVersion } : {}),
    ...(z.spec.forward?.upstreamGroup ? { upstreamGroup: z.spec.forward.upstreamGroup } : {}),
    ...(z.spec.fallback ? { fallback: z.spec.fallback } : {}),
    ...(zoneDeleteRefusal(z) ? { deleteRefusal: zoneDeleteRefusal(z) } : {}),
  };
}

/** What DELETE /zones/{zone} refuses whatever the caller may do (D148). */
function zoneDeleteRefusal(z: DNSZone): 'default_zone' | 'managed_by' | undefined {
  if (z.spec.zoneName === '.') return 'default_zone';
  const managed = (o: { metadata: { annotations?: Record<string, string> } }) => {
    const owner = o.metadata.annotations?.[MANAGED] ?? '';
    return owner !== '' && owner !== 'api' && owner !== 'bootstrap';
  };
  if (managed(z) || db.recordSets.some((rs) => rs.spec.zoneRef.name === z.metadata.name && managed(rs))) return 'managed_by';
  return undefined;
}

function recordsOf(zone: string): ZoneRecord[] {
  const out: ZoneRecord[] = [];
  for (const rs of db.recordSets) {
    if (rs.spec.zoneRef.name !== zone) continue;
    for (const e of rs.spec.endpoints) {
      out.push({
        recordSet: rs.metadata.name,
        namespace: rs.metadata.namespace ?? 'default',
        dnsName: e.dnsName,
        recordType: e.recordType,
        targets: e.targets,
        ...(e.recordTTL ? { recordTTL: e.recordTTL } : {}),
        ...(e.setIdentifier ? { setIdentifier: e.setIdentifier } : {}),
        ...(e.kudurru ? { kudurru: e.kudurru } : {}),
        managedBy: rs.metadata.annotations?.[MANAGED] ?? '',
      });
    }
  }
  return out;
}

function bumpZone(zone: string): void {
  const z = db.zones.find((x) => x.metadata.name === zone);
  if (z?.status) {
    z.status.serial = (z.status.serial ?? 0) + 1;
    z.status.recordCount = recordsOf(zone).reduce((n, r) => n + r.targets.length, 0);
  }
}

function exportZone(z: DNSZone): string {
  const origin = z.spec.zoneName;
  const lines = [`$ORIGIN ${origin}`, `$TTL ${z.spec.defaultTTL ?? 300}`];
  const soa = z.spec.soa ?? {};
  lines.push(`@ IN SOA ${soa.mname ?? `ns1.${origin}`} ${soa.rname ?? `hostmaster.${origin}`} ${z.status?.serial ?? 1} ${soa.refresh ?? 3600} ${soa.retry ?? 600} ${soa.expire ?? 604800} ${soa.minimum ?? 300}`);
  for (const r of recordsOf(z.metadata.name)) {
    if (r.kudurru?.disabled) continue;
    for (const t of r.targets) lines.push(`${r.dnsName}. ${r.recordTTL ?? z.spec.defaultTTL ?? 300} IN ${r.recordType} ${t}`);
  }
  return `${lines.join('\n')}\n`;
}

// ---------------------------------------------------------------------------
// Query log and stats

function wildcard(pattern: string): RegExp {
  const esc = pattern.replace(/[.+?^${}()|[\]\\]/g, '\\$&').replace(/\*/g, '.*');
  return new RegExp(pattern.includes('*') ? `^${esc}$` : esc, 'i');
}

function filterQueries(url: URL): QueryRow[] {
  const p = url.searchParams;
  let rows = db.queries;
  const node = p.get('node');
  if (node) rows = rows.filter((r) => r.node === node);
  const client = p.get('client');
  if (client) rows = rows.filter((r) => r.client.ip === client || r.client.name === client || wildcard(client).test(r.client.name ?? ''));
  const domain = p.get('domain');
  if (domain) {
    const re = wildcard(domain);
    rows = rows.filter((r) => re.test(r.domain));
  }
  const status = p.getAll('status').flatMap((s) => s.split(',')).filter(Boolean);
  if (status.length) rows = rows.filter((r) => status.includes(r.status));
  const cls = p.get('status_class') as StatusClass | null;
  if (cls) rows = rows.filter((r) => r.status_class === cls);
  for (const f of ['qtype', 'upstream', 'list', 'zone', 'policy'] as const) {
    const v = p.get(f);
    if (v) rows = rows.filter((r) => (r[f] ?? '').toLowerCase() === v.toLowerCase());
  }
  const since = p.get('since');
  if (since) rows = rows.filter((r) => r.ts >= new Date(since).toISOString());
  const until = p.get('until');
  if (until) rows = rows.filter((r) => r.ts <= new Date(until).toISOString());
  return rows;
}

function windowMs(url: URL): number {
  const w = url.searchParams.get('window') ?? '24h';
  const m = /^(\d+)(h|d|m)$/.exec(w);
  if (!m) return 86_400_000;
  const n = Number(m[1]);
  return n * (m[2] === 'd' ? 86_400_000 : m[2] === 'h' ? 3_600_000 : 60_000);
}

function seriesTotals() {
  return db.series.reduce(
    (a, p) => ({
      total: a.total + p.total,
      blocked: a.blocked + p.blocked,
      cached: a.cached + p.cached,
      forwarded: a.forwarded + p.forwarded,
      authoritative: a.authoritative + p.authoritative,
      other: a.other + p.other,
    }),
    { total: 0, blocked: 0, cached: 0, forwarded: 0, authoritative: 0, other: 0 },
  );
}

function topFrom(dim: TopDim, blocked: boolean, n: number, scale: number): TopItem[] {
  const counts = new Map<string, { total: number; blocked: number; name?: string; group?: string }>();
  for (const r of db.queries) {
    let key: string | undefined;
    let name: string | undefined;
    let group: string | undefined;
    switch (dim) {
      case 'domains':
        key = r.domain;
        break;
      case 'clients':
        key = r.client.ip;
        name = r.client.name;
        group = r.client.group;
        break;
      case 'upstreams':
        // Answers from a local zone count as "zone:<name>", as on a node.
        key = r.upstream ?? (r.zone ? `zone:${r.zone.replace(/\.$/, '')}` : undefined);
        break;
      case 'lists':
        key = r.list;
        break;
      case 'zones':
        key = r.zone;
        break;
    }
    if (!key) continue;
    const c = counts.get(key) ?? { total: 0, blocked: 0, ...(name ? { name } : {}), ...(group ? { group } : {}) };
    c.total += 1;
    if (r.status_class === 'blocked') c.blocked += 1;
    counts.set(key, c);
  }
  const items = [...counts.entries()]
    .map(([key, c]) => ({ key, ...c, count: blocked ? c.blocked : c.total - c.blocked }))
    .filter((x) => x.count > 0)
    .sort((a, b) => b.count - a.count)
    .slice(0, n);
  const sum = items.reduce((s, x) => s + x.count, 0) || 1;
  return items.map((x) => ({
    key: x.key,
    ...(x.name ? { name: x.name } : {}),
    ...(x.group ? { group: x.group } : {}),
    count: Math.round(x.count * scale),
    total: Math.round(x.total * scale),
    blocked: Math.round(x.blocked * scale),
    percent: (x.count / sum) * 100,
  }));
}

/** One presentation line, as miekg/dns prints it (tab-separated). */
function rrLine(name: string, ttl: number, type: string, data: string): string {
  return `${name}\t${ttl}\tIN\t${type}\t${data}`;
}

const DIG_FLAGS = { aa: false, tc: false, rd: true, ra: true, ad: false, cd: false };

function digAnswer(req: DnsQueryRequest): DnsQueryResult {
  const type = (req.type ?? 'A').toUpperCase();
  const n = req.name.replace(/\.$/, '').toLowerCase();
  const base = { name: `${n}.`, type, client: req.client || '127.0.0.1', node: 'kudurru-0', took_ms: 0.3, authority: [] as string[], additional: [] as string[], flags: { ...DIG_FLAGS } };
  // Disabled lists and disabled sources (ux-plan C2) block nothing.
  const blockedList = db.lists.find(
    (l) => l.spec.mode !== 'allow' && l.metadata.annotations?.['kudurru.io/enabled'] !== 'false' && l.spec.sources.some((s) => s.enabled !== false && s.inline?.includes(n)),
  );
  const knownBlocked = [...db.queries].find((q) => q.domain === n && q.status_class === 'blocked');
  if (db.blocking.blocking && (blockedList || knownBlocked)) {
    return {
      ...base,
      rcode: 'NOERROR',
      status: knownBlocked?.status ?? 'blocked_exact',
      status_class: 'blocked',
      answer: [rrLine(`${n}.`, 600, type, type === 'AAAA' ? '::' : '0.0.0.0')],
      list: knownBlocked?.list ?? `default/${blockedList?.metadata.name ?? ''}`,
      rule: knownBlocked?.rule ?? n,
      latency_ms: 0.034,
    };
  }
  const local = recordsOf('home-arpa').concat(recordsOf('example-internal')).filter((r) => r.dnsName.toLowerCase() === n);
  if (local.length) {
    const rr = local.filter((r) => r.recordType === type || r.recordType === 'CNAME');
    const apex = n.endsWith('home.arpa') ? 'home.arpa.' : 'example.internal.';
    return {
      ...base,
      rcode: 'NOERROR',
      status: 'authoritative',
      status_class: 'permitted',
      answer: rr.flatMap((r) => r.targets.map((t) => rrLine(`${r.dnsName}.`, r.recordTTL ?? 300, r.recordType, t))),
      authority: rr.length ? [] : [rrLine(apex, 300, 'SOA', `ns1.${apex} hostmaster.${apex} 1 3600 600 604800 300`)],
      flags: { ...DIG_FLAGS, aa: true, ra: false },
      latency_ms: 0.08,
    };
  }
  if (n.startsWith('servfail')) {
    return { ...base, rcode: 'SERVFAIL', status: 'servfail_upstream', status_class: 'error', answer: [], upstream: 'default/cloudflare', error: 'every upstream of group default failed: i/o timeout', latency_ms: 2003.1 };
  }
  if (n.endsWith('.invalid') || n.startsWith('nx')) {
    return {
      ...base,
      rcode: 'NXDOMAIN',
      status: 'forwarded',
      status_class: 'permitted',
      answer: [],
      authority: [rrLine('.', 86400, 'SOA', 'a.root-servers.net. nstld.verisign-grs.com. 2026101000 1800 900 604800 86400')],
      upstream: 'default/cloudflare',
      latency_ms: 14.2,
    };
  }
  const cname = n === 'www.youtube.com' ? 'youtube-ui.l.google.com.' : undefined;
  const data = type === 'AAAA' ? '2606:4700::6810:84e5' : type === 'TXT' ? '"v=spf1 -all"' : '104.16.132.229';
  return {
    ...base,
    rcode: 'NOERROR',
    status: 'forwarded',
    status_class: 'permitted',
    answer: [
      ...(cname ? [rrLine(`${n}.`, 300, 'CNAME', cname)] : []),
      rrLine(cname ?? `${n}.`, 300, type, data),
      ...(req.dnssec ? [rrLine(cname ?? `${n}.`, 300, 'RRSIG', `${type} 13 2 300 20261012192501 20261010172501 34505 ${n}. fWoOfIKKt2xfi8w3SxvNe62Zszspk9KT61IliWVje6p9KcqVMYHVEXfl9rV1aFYbRJ2h+1oWuH95FLEXxwaFpw==`)] : []),
    ],
    additional: req.dnssec ? ['\n;; OPT PSEUDOSECTION:\n; EDNS: version 0; flags: do; udp: 1232'] : [],
    flags: { ...DIG_FLAGS, ad: !!req.dnssec },
    upstream: 'default/cloudflare',
    latency_ms: 18.6,
  };
}

/** The upstream groups of the cluster settings (a map keyed by group name). */
function upstreamGroups(): Record<string, { mode?: string; servers?: string[] | null; strategy?: string; timeout?: string }> {
  return (getPath(db.settings, 'upstreams.groups') as Record<string, { servers?: string[] | null }> | undefined) ?? {};
}

/** The display label of a server entry ("name=addr" → name). */
function serverLabel(entry: string): string {
  const i = entry.indexOf('=');
  return i > 0 && !/[:/[\]]/.test(entry.slice(0, i)) ? entry.slice(0, i) : entry;
}

/**
 * The compatibility routes (POST /domains/{allow|deny}/{exact|regex}, the
 * query-log row action) write structured entries for everyone like the
 * Allowlist routes; an exact "*.x" becomes a wildcard entry.
 */
function compatAdd(action: DomainAction, kind: 'exact' | 'regex', namespace: string, domains: string[], origin: 'api' | 'querylog') {
  const success: { item: string }[] = [];
  const errors: { item: string; error: string }[] = [];
  for (const d of domains) {
    const v = d.trim();
    if (!v) continue;
    const type = kind === 'regex' ? 'regex' : v.startsWith('*.') || v.startsWith('||') ? 'wildcard' : 'exact';
    const r = createEntries(db, action, { values: [v], type, namespace, origin }, actorOf());
    success.push(...r.processed.success);
    errors.push(...r.processed.errors);
  }
  return { processed: { success, errors } };
}

/** The signed-in user as an entry's createdBy. */
function actorOf(): string {
  return db.session ? `User:${db.session.user.username}` : 'system';
}

/** Source `source` (its status name or index) of BlockList ns/name, or the error answer. */
function anySource(ns: string, name: string, source: string): { list: BlockList; src: BlockList['spec']['sources'][number]; source: string; name: string } | Response {
  const list = db.lists.find((l) => l.metadata.name === name && (l.metadata.namespace ?? 'default') === ns);
  if (!list) return fail(404, 'not_found', `BlockList ${ns}/${name} not found`);
  let i = list.spec.sources.findIndex((s, j) => sourceName(j, s) === source);
  if (i < 0 && /^\d+$/.test(source)) i = Number(source);
  const src = list.spec.sources[i];
  if (!src) return fail(404, 'not_found', `BlockList ${ns}/${name} has no source ${source}`);
  return { list, src, source: sourceName(i, src), name: `${ns}/${name}` };
}

type ObjParams = { kind?: string | readonly string[]; a?: string | readonly string[]; b?: string | readonly string[] };

/** GET/PUT/DELETE of one object; cluster-scoped kinds omit the namespace segment. */
function objectRoutes(pattern: string) {
  const target = (params: ObjParams): { kind: ObjectKind; ns?: string; name: string } | Response => {
    const kind = String(params.kind);
    if (!isKind(kind)) return fail(404, 'not_found', `unknown kind ${kind}`);
    return params.b ? { kind, ns: String(params.a), name: String(params.b) } : { kind, name: String(params.a) };
  };
  return [
    http.get(pattern, ({ request, params }) => {
      const t = target(params);
      if (t instanceof Response) return t;
      const g = guard(request, RESOURCE_OF[t.kind], 'read');
      if (g) return g;
      const obj = find(t.kind, t.name, t.ns);
      return obj ? ok(obj as unknown as Record<string, unknown>) : fail(404, 'not_found', `${KIND_NAMES[t.kind]} ${t.name} not found`);
    }),
    http.put(pattern, async ({ request, params }) => {
      const t = target(params);
      if (t instanceof Response) return t;
      const g = guard(request, RESOURCE_OF[t.kind], 'write');
      if (g) return g;
      const obj = (await request.json()) as AnyObject;
      if (obj.metadata?.name !== t.name) return fail(400, 'invalid', `metadata.name: must be ${t.name}, the name in the path`);
      const res = writeObject(t.kind, obj, request.headers.get('If-Match'));
      return HttpResponse.json(res.body as JsonBodyType, { status: res.status });
    }),
    http.delete(pattern, ({ request, params }) => {
      const t = target(params);
      if (t instanceof Response) return t;
      const g = guard(request, RESOURCE_OF[t.kind], 'delete');
      if (g) return g;
      const res = deleteObject(t.kind, t.name, t.ns);
      return res.status === 204 ? new HttpResponse(null, { status: 204 }) : HttpResponse.json(res.body as JsonBodyType, { status: res.status });
    }),
  ];
}

let tailTimer = 0;

// ---------------------------------------------------------------------------

export const handlers = [
  ...dhcpHandlers(guard),
  // auth ------------------------------------------------------------------
  http.get(`${B}/auth/status`, () =>
    ok({
      localLogin: true,
      localLoginMode: 'enabled',
      providers: [{ id: 'dex', displayName: 'Dex (corporate SSO)' }, { id: 'github', displayName: 'GitHub' }],
      hasDefaultCredentials: db.users.some((u) => u.username === 'admin' && u.password === 'admin'),
      session: { idleTimeout: '30m0s', lifetime: '12h0m0s' },
    }),
  ),
  http.post(`${B}/auth/login`, async ({ request }) => {
    const body = (await request.json()) as { username?: string; password?: string; totpCode?: string };
    if (!body.username || !body.password) return fail(400, 'invalid', 'username and password are required');
    const user = db.users.find((u) => u.username === body.username && u.password === body.password);
    if (!user) return fail(401, 'unauthenticated', 'invalid username or password');
    if (mockFactors.has(user.username) && body.totpCode !== '123456') return fail(401, 'unauthenticated', 'Authenticator code required');
    db.session = { user };
    return ok({ ...me() });
  }),
  http.post(`${B}/auth/logout`, () => {
    db.session = null;
    return new HttpResponse(null, { status: 204 });
  }),
  http.get(`${B}/auth/me`, () => (db.session ? ok({ ...me() }) : fail(401, 'unauthenticated', 'No session'))),
  http.get(`${B}/auth/sessions`, () => db.session ? ok({ items: structuredClone(ownSessions(db.session.user.username)) }) : fail(401, 'unauthenticated', 'No session')),
  http.delete(`${B}/auth/sessions/:id`, ({ request, params }) => {
    if (!db.session) return fail(401, 'unauthenticated', 'No session');
    if (request.headers.get('X-Kudurru-CSRF') !== CSRF) return fail(403, 'csrf', 'CSRF required');
    const row = ownSessions(db.session.user.username).find((s) => s.id === params.id);
    if (!row) return fail(404, 'not_found', 'Session not found');
    row.revoked = true;
    if (row.current) db.session = null;
    return new HttpResponse(null, { status: 204 });
  }),
  http.post(`${B}/auth/password`, async ({ request }) => {
    if (!db.session) return fail(401, 'unauthenticated', 'Sign in to continue');
    if (request.headers.get('X-Kudurru-CSRF') !== CSRF) return fail(403, 'csrf', 'Missing or wrong X-Kudurru-CSRF header');
    const body = (await request.json()) as Record<string, unknown>;
    const unknown = Object.keys(body).find((k) => !['currentPassword', 'newPassword', 'revokeTokens', 'totpCode'].includes(k));
    if (unknown) return fail(400, 'invalid', `body: json: unknown field "${unknown}"`);
    const next = typeof body.newPassword === 'string' ? body.newPassword : '';
    if ([...next].length < 8) return fail(400, 'invalid', 'newPassword: users: password is shorter than 8 characters');
    if (next === body.currentPassword) return fail(400, 'invalid', 'newPassword: must differ from the current password');
    const u = db.session.user;
    if (body.currentPassword !== u.password) return fail(403, 'forbidden', 'the current password is wrong');
    if (mockFactors.has(u.username) && body.totpCode !== '123456') return fail(401, 'unauthenticated', 'Authenticator code required');
    u.password = next;
    u.mustChangePassword = false;
    return ok({ ...me() });
  }),
  http.get(`${B}/auth/totp`, () => db.session ? ok({ enabled: mockFactors.has(db.session.user.username) }) : fail(401, 'unauthenticated', 'No session')),
  http.post(`${B}/auth/totp/:action`, async ({ request, params }) => {
    if (!db.session) return fail(401, 'unauthenticated', 'No session');
    if (request.headers.get('X-Kudurru-CSRF') !== CSRF) return fail(403, 'csrf', 'CSRF required');
    const user = db.session.user;
    if (user.mustChangePassword) return fail(403, 'forbidden', 'Password change required');
    const action = String(params.action);
    if (!['setup', 'confirm', 'disable'].includes(action)) return fail(404, 'not_found', 'Unknown factor action');
    const body = await request.json() as Record<string, unknown>;
    const keys = action === 'confirm' ? ['currentPassword', 'totpCode', 'challenge', 'code'] : ['currentPassword', 'totpCode'];
    if (Object.keys(body).some((key) => !keys.includes(key))) return fail(400, 'invalid', 'Unexpected field');
    if (body.currentPassword !== user.password || (mockFactors.has(user.username) && body.totpCode !== '123456')) return fail(401, 'unauthenticated', 'Proof not accepted');
    if (action === 'setup') {
      const candidate = { challenge: `mock-challenge-${crypto.randomUUID()}`, expiresAt: new Date(Date.now() + 300_000).toISOString() };
      mockSetups.set(user.username, candidate);
      return ok({ ...candidate, secret: 'JBSWY3DPEHPK3PXP', otpauthURI: 'otpauth://totp/kudurru:mock?secret=JBSWY3DPEHPK3PXP&issuer=kudurru' });
    }
    if (action === 'confirm') {
      const candidate = mockSetups.get(user.username);
      if (!candidate || candidate.challenge !== body.challenge || Date.parse(candidate.expiresAt) <= Date.now()) return fail(409, 'conflict', 'Setup changed');
      if (body.code !== '123456') return fail(401, 'unauthenticated', 'New code not accepted');
      mockFactors.add(user.username);
    } else mockFactors.delete(user.username);
    mockSetups.delete(user.username);
    db.session = null;
    return new HttpResponse(null, { status: 204 });
  }),
  http.post(`${B}/iam/users/:name/totp/recover`, async ({ request, params }) => {
    const denied = guard(request, 'users', 'write');
    if (denied) return denied;
    const name = String(params.name);
    const body = await request.json() as Record<string, unknown>;
    if (Object.keys(body).length !== 1 || typeof body.uid !== 'string') return fail(400, 'invalid', 'UID required');
    if (!db.users.some((u) => u.username === name) || body.uid !== `mock-user-${name}`) return fail(409, 'conflict', 'Account changed');
    mockFactors.delete(name); mockSetups.delete(name);
    if (db.session?.user.username === name) db.session = null;
    return new HttpResponse(null, { status: 204 });
  }),
  http.get(`${B}/auth/oidc/:idp/start`, ({ params }) => fail(501, 'not_implemented', `The mock cannot redirect to ${String(params.idp)}`)),
  http.get(`${B}/auth/tokens`, ({ request }) => guard(request, 'tokens', 'own') ?? ok({ items: db.tokens, continue: '' })),
  http.post(`${B}/auth/tokens`, async ({ request }) => {
    const g = guard(request, 'tokens', 'own');
    if (g) return g;
    // The real handler decodes with DisallowUnknownFields.
    const body = (await request.json()) as Record<string, unknown>;
    const unknown = Object.keys(body).find((k) => !['description', 'expiresIn', 'scopes'].includes(k));
    if (unknown) return fail(400, 'invalid', `body: json: unknown field "${unknown}"`);
    const expiresIn = typeof body.expiresIn === 'string' ? body.expiresIn : '';
    if (expiresIn && !/^\d+(h|m|s)$/.test(expiresIn)) return fail(400, 'invalid', 'expiresIn: not a positive Go duration');
    const u = db.session?.user;
    const id = Math.random().toString(36).slice(2, 10);
    const t: ApiToken = {
      id,
      ...(typeof body.description === 'string' && body.description ? { description: body.description } : {}),
      owner: { kind: 'User', name: u?.username ?? '', id: `local:${u?.username ?? ''}`, provider: 'local' },
      createdAt: new Date().toISOString(),
      expiresAt: expiresIn ? new Date(Date.now() + 30 * 86_400_000).toISOString() : null,
      ...(Array.isArray(body.scopes) && body.scopes.length ? { scopes: body.scopes as string[] } : {}),
    };
    db.tokens.push(t);
    return ok({ ...t, token: `kdr_${id}_${Math.random().toString(36).slice(2)}${Math.random().toString(36).slice(2)}` }, { status: 201 });
  }),
  http.delete(`${B}/auth/tokens/:id`, ({ request, params }) => {
    const g = guard(request, 'tokens', 'own');
    if (g) return g;
    db.tokens = db.tokens.filter((t) => t.id !== params.id);
    return new HttpResponse(null, { status: 204 });
  }),

  // generic objects --------------------------------------------------------
  http.get(`${B}/objects/:kind`, ({ request, params }) => {
    const kind = String(params.kind);
    if (!isKind(kind)) return fail(404, 'not_found', `unknown kind ${kind}`);
    const g = guard(request, RESOURCE_OF[kind], 'read');
    if (g) return g;
    const url = new URL(request.url);
    const ns = url.searchParams.get('namespace');
    const items = collection(kind).filter((o) => !ns || o.metadata.namespace === ns);
    return listEnvelope(items, url);
  }),
  http.post(`${B}/objects/:kind`, async ({ request, params }) => {
    const kind = String(params.kind);
    if (!isKind(kind)) return fail(404, 'not_found', `unknown kind ${kind}`);
    const g = guard(request, RESOURCE_OF[kind], 'write');
    if (g) return g;
    const obj = (await request.json()) as AnyObject;
    if (obj.metadata?.name && find(kind, obj.metadata.name, obj.metadata.namespace)) {
      return fail(409, 'conflict', `${KIND_NAMES[kind]} ${obj.metadata.namespace ?? 'default'}/${obj.metadata.name} already exists`, 'PUT it to replace it');
    }
    const res = writeObject(kind, obj, null);
    return HttpResponse.json(res.body as JsonBodyType, { status: res.status });
  }),
  ...objectRoutes(`${B}/objects/:kind/:a`),
  ...objectRoutes(`${B}/objects/:kind/:a/:b`),
  http.get(`${B}/imports/runs`, () => HttpResponse.json({ items: [] })),
  http.get(`${B}/imports/existing`, () => HttpResponse.json({ items: [] })),
  http.post(`${B}/apply`, async ({ request }) => {
    const g = guard(request, 'zones', 'write');
    if (g) return g;
    const text = await request.text();
    let docs: AnyObject[];
    try {
      docs = JSON.parse(text) as AnyObject[];
      if (!Array.isArray(docs)) docs = [docs];
    } catch {
      return fail(400, 'invalid', 'The mock accepts JSON arrays only; the server also takes multi-document YAML');
    }
    const items: ApplyResult[] = docs.map((d) => {
      const kind = (Object.entries(KIND_NAMES).find(([, v]) => v === d.kind)?.[0] ?? '') as ObjectKind;
      if (!isKind(kind)) return { kind: d.kind, name: d.metadata?.name ?? '', result: 'error', error: `unknown kind ${d.kind}` };
      const res = writeObject(kind, d, null);
      const base = { kind: d.kind, ...(d.metadata.namespace ? { namespace: d.metadata.namespace } : {}), name: d.metadata.name };
      return res.result ? { ...base, result: res.result } : { ...base, result: 'error', error: (res.body as { error: { message: string } }).error.message };
    });
    return ok({ items });
  }),
  http.post(`${B}/:kindBatch`, async ({ request, params }) => {
    const raw = String(params.kindBatch);
    // Other one-segment POST routes (/allowlist, /denylist:batch, …) are matched further down.
    if (!raw.endsWith(':batchDelete')) return undefined;
    const kind = raw.slice(0, -':batchDelete'.length);
    if (!isKind(kind)) return fail(404, 'not_found', `unknown kind ${kind}`);
    const g = guard(request, RESOURCE_OF[kind], 'delete');
    if (g) return g;
    const body = (await request.json()) as { names: string[]; namespace?: string };
    const success: { item: string }[] = [];
    const errors: { item: string; error: string }[] = [];
    for (const n of body.names) {
      const res = deleteObject(kind, n, body.namespace);
      if (res.status === 204) success.push({ item: n });
      else errors.push({ item: n, error: (res.body as { error: { message: string } }).error.message });
    }
    return ok({ processed: { success, errors } });
  }),

  // zones -----------------------------------------------------------------
  http.get(`${B}/zones`, ({ request }) => guard(request, 'zones', 'read') ?? ok({ items: db.zones.map(zoneSummary) })),
  http.get(`${B}/zones/:zone`, ({ request, params }) => {
    const g = guard(request, 'zones', 'read');
    if (g) return g;
    const z = db.zones.find((x) => x.metadata.name === params.zone);
    return z ? ok({ ...zoneSummary(z) }) : fail(404, 'not_found', `zone ${String(params.zone)} not found`);
  }),
  http.delete(`${B}/zones/:zone`, ({ request, params }) => {
    const g = guard(request, 'zones', 'delete');
    if (g) return g;
    const zone = String(params.zone);
    const z = db.zones.find((x) => x.metadata.name === zone);
    if (!z) return fail(404, 'not_found', `zone ${zone} not found`);
    const sets = db.recordSets.filter((rs) => rs.spec.zoneRef.name === zone);
    if (sets.length) {
      const g2 = guard(request, 'records', 'delete');
      if (g2) return g2;
    }
    const refusal = zoneDeleteRefusal(z);
    if (refusal === 'default_zone') {
      return fail(409, 'default_zone', `zone ${zone} is the root zone "." and cannot be deleted`, 'edit it instead: change spec.forward.upstreamGroup, or set spec.policy.queryAccess to deny');
    }
    if (refusal === 'managed_by') {
      const objects = [z, ...sets]
        .filter((o) => !['', 'api', 'bootstrap'].includes(o.metadata.annotations?.[MANAGED] ?? ''))
        .map((o) => ({ kind: o.kind, ...(o.metadata.namespace ? { namespace: o.metadata.namespace } : {}), name: o.metadata.name, managedBy: o.metadata.annotations?.[MANAGED] ?? '' }));
      return HttpResponse.json(
        { error: { key: 'managed_by', message: `zone ${zone} cannot be deleted: managed by another writer`, hint: objects[0]?.managedBy, objects }, took_ms: 0.4 } as JsonBodyType,
        { status: 409 },
      );
    }
    const dryRun = new URL(request.url).searchParams.get('dryRun') === 'true';
    const recordSets = sets.map((rs) => `${rs.metadata.namespace ?? 'default'}/${rs.metadata.name}`).sort();
    const endpointCount = sets.reduce((n, rs) => n + rs.spec.endpoints.length, 0);
    if (!dryRun) {
      db.recordSets = db.recordSets.filter((rs) => rs.spec.zoneRef.name !== zone);
      db.zones = db.zones.filter((x) => x !== z);
    }
    return ok({ zone, zoneName: z.spec.zoneName, type: z.spec.type, dryRun, recordSets, recordSetCount: sets.length, endpointCount, flushed: dryRun ? 0 : 1 });
  }),
  http.get(`${B}/zones/:zone/records`, ({ request, params }) => {
    const g = guard(request, 'records', 'read');
    if (g) return g;
    if (!db.zones.some((z) => z.metadata.name === params.zone)) return fail(404, 'not_found', `zone ${String(params.zone)} not found`);
    return ok({ items: recordsOf(String(params.zone)) });
  }),
  http.post(`${B}/zones/:zone/records`, async ({ request, params }) => {
    const g = guard(request, 'records', 'write');
    if (g) return g;
    const zone = String(params.zone);
    const z = db.zones.find((x) => x.metadata.name === zone);
    if (!z) return fail(404, 'not_found', `zone ${zone} not found`);
    if (z.spec.type !== 'primary') return fail(422, 'unprocessable', `zone ${zone} is ${z.spec.type}; only primary zones hold records`);
    const e = (await request.json()) as Endpoint;
    if (!e.dnsName || !e.recordType || !e.targets?.length) return fail(400, 'invalid', 'dnsName, recordType and targets are required');
    const origin = z.spec.zoneName.replace(/\.$/, '');
    if (e.dnsName !== origin && !e.dnsName.endsWith(`.${origin}`)) return fail(400, 'invalid', `dnsName: ${e.dnsName} is not inside ${origin}`);
    if (e.recordType === 'A' && e.targets.some((t) => !/^\d+\.\d+\.\d+\.\d+$/.test(t))) return fail(400, 'invalid', 'targets: not an IPv4 address');
    const rsName = new URL(request.url).searchParams.get('recordSet') ?? `api-${zone}`;
    let rs = db.recordSets.find((x) => x.metadata.name === rsName);
    if (rs && rs.metadata.annotations?.[MANAGED] !== 'api') {
      const owner = rs.metadata.annotations?.[MANAGED] ?? '';
      return fail(409, 'managed_by', `DNSRecordSet ${rsName} is managed by ${owner}`, owner);
    }
    if (!rs) {
      db.rv += 1;
      rs = { apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSRecordSet', metadata: { name: rsName, namespace: 'default', resourceVersion: String(db.rv), annotations: { [MANAGED]: 'api' } }, spec: { zoneRef: { name: zone }, endpoints: [] } };
      db.recordSets.push(rs);
    }
    const idx = rs.spec.endpoints.findIndex((x) => x.dnsName === e.dnsName && x.recordType === e.recordType && (x.setIdentifier ?? '') === (e.setIdentifier ?? ''));
    if (idx >= 0) rs.spec.endpoints[idx] = e;
    else rs.spec.endpoints.push(e);
    bumpZone(zone);
    return ok({ recordSet: rsName, namespace: 'default', ...e, managedBy: 'api' }, { status: idx >= 0 ? 200 : 201 });
  }),
  http.delete(`${B}/zones/:zone/records/:name/:type`, ({ request, params }) => {
    const g = guard(request, 'records', 'delete');
    if (g) return g;
    const zone = String(params.zone);
    const q = new URL(request.url).searchParams;
    const rsName = q.get('recordSet');
    const setID = q.get('setIdentifier');
    let found = false;
    for (const rs of db.recordSets) {
      if (rs.spec.zoneRef.name !== zone || (rsName && rs.metadata.name !== rsName)) continue;
      const before = rs.spec.endpoints.length;
      const keep = rs.spec.endpoints.filter((x) => !(x.dnsName === params.name && x.recordType === params.type && (!setID || x.setIdentifier === setID)));
      if (keep.length !== before) {
        const owner = rs.metadata.annotations?.[MANAGED] ?? '';
        if (owner !== 'api') return fail(409, 'managed_by', `DNSRecordSet ${rs.metadata.name} is managed by ${owner}`, owner);
        rs.spec.endpoints = keep;
        found = true;
      }
    }
    if (!found) return fail(404, 'not_found', `${String(params.name)} ${String(params.type)} not found in ${zone}`);
    bumpZone(zone);
    return new HttpResponse(null, { status: 204 });
  }),
  http.post(`${B}/zones/:zone/import`, async ({ request, params }) => {
    const g = guard(request, 'records', 'write');
    if (g) return g;
    const zone = String(params.zone);
    const originParam = new URL(request.url).searchParams.get('origin');
    let z = db.zones.find((x) => x.metadata.name === zone);
    const created = !z && !!originParam;
    if (!z && !originParam) return fail(404, 'not_found', `DNSZone ${zone} not found`, 'pass ?origin=<zone apex> to create the zone from the file');
    const text = await request.text();
    const apex = (z?.spec.zoneName ?? originParam ?? '').replace(/\.$/, '');
    const eps: Endpoint[] = [];
    let serial = 0;
    let bad: string | undefined;
    text.split('\n').forEach((line, i) => {
      const l = line.replace(/;.*$/, '').trim();
      if (bad || !l || l.startsWith('$')) return;
      const m = /^(\S+)\s+(?:(\d+)\s+)?(?:IN\s+)?([A-Z0-9]+)\s+(.+)$/.exec(l);
      if (!m) {
        bad = `line ${i + 1}: cannot parse "${l}"`;
        return;
      }
      const [, owner, ttl, type, data] = m as unknown as [string, string, string | undefined, string, string];
      if (type === 'SOA') {
        serial = Number(data.trim().split(/\s+/)[2] ?? '0') || 0;
        return;
      }
      const name = owner === '@' ? apex : owner.endsWith('.') ? owner.slice(0, -1) : `${owner}.${apex}`;
      eps.push({ dnsName: name, recordType: type, targets: [data.trim()], ...(ttl ? { recordTTL: Number(ttl) } : {}) });
    });
    if (bad) return fail(400, 'invalid', `zone: ${bad}`, `the body must be an RFC 1035 master file for ${apex}.`);
    if (!z) {
      db.rv += 1;
      z = { apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSZone', metadata: { name: zone, resourceVersion: String(db.rv), annotations: { [MANAGED]: 'api' } }, spec: { zoneName: `${apex}.`, type: 'primary' }, status: { serial, recordCount: 0, conditions: [{ type: 'Ready', status: 'True', reason: 'Serving' }] } };
      db.zones.push(z);
    }
    const rsName = `import-${zone}-1`;
    db.recordSets = db.recordSets.filter((x) => x.metadata.name !== rsName);
    db.recordSets.push({ apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSRecordSet', metadata: { name: rsName, namespace: 'default', annotations: { [MANAGED]: 'api' } }, spec: { zoneRef: { name: zone }, endpoints: eps } });
    bumpZone(zone);
    return ok({ zone, zoneName: `${apex}.`, created, recordSets: [`default/${rsName}`], endpoints: eps.length, serial, warnings: serial ? [] : ['no SOA record; defaults apply'] });
  }),
  http.get(`${B}/zones/:zone/export`, ({ request, params }) => {
    const g = guard(request, 'zones', 'read');
    if (g) return g;
    const z = db.zones.find((x) => x.metadata.name === params.zone);
    if (!z) return fail(404, 'not_found', `zone ${String(params.zone)} not found`);
    return new HttpResponse(exportZone(z), { headers: { 'Content-Type': 'text/dns; charset=utf-8', 'Content-Disposition': `attachment; filename="${z.metadata.name}.zone"` } });
  }),
  http.post(`${B}/zones/:zone/notify`, ({ request, params }) => {
    const g = guard(request, 'zones', 'transfer');
    if (g) return g;
    const z = db.zones.find((x) => x.metadata.name === params.zone);
    if (!z) return fail(404, 'not_found', `zone ${String(params.zone)} not found`);
    if (z.spec.type !== 'primary') return fail(422, 'unprocessable', 'NOTIFY is sent for primary zones only');
    return ok({ targets: z.spec.policy?.notifyTargets ?? z.spec.nameservers ?? [] });
  }),

  // lists and policy -------------------------------------------------------
  http.post(`${B}/blocklists/:ns/:name/refresh`, ({ request, params }) => {
    const g = guard(request, 'blocklists', 'refresh');
    if (g) return g;
    const l = db.lists.find((x) => x.metadata.name === params.name && x.metadata.namespace === params.ns);
    if (!l) return fail(404, 'not_found', `BlockList ${String(params.ns)}/${String(params.name)} not found`);
    if (l.status) l.status.lastFetched = new Date().toISOString();
    return new HttpResponse(null, { status: 202 });
  }),
  http.get(`${B}/blocklists/_search`, ({ request }) => {
    const g = guard(request, 'blocklists', 'read');
    if (g) return g;
    const domain = (new URL(request.url).searchParams.get('domain') ?? '').toLowerCase();
    const items = db.lists.flatMap((l) =>
      l.spec.sources.flatMap((s) =>
        (s.inline ?? [])
          .filter((rule) => rule === domain || (rule.startsWith('/') && new RegExp(rule.slice(1, -1)).test(domain)))
          .map((rule) => ({ list: `${l.metadata.namespace ?? 'default'}/${l.metadata.name}`, rule, mode: l.spec.mode ?? 'block' })),
      ),
    );
    const fromLog = db.queries.find((q) => q.domain === domain && q.list);
    if (fromLog?.list && !items.some((i) => i.list === fromLog.list)) {
      items.push({ list: fromLog.list, rule: fromLog.rule ?? domain, mode: 'block' });
    }
    return ok({ items });
  }),
  // The entries of one source, any kind (allowlist contract B): a page with
  // ?q=&enabled=&limit=&continue=; PATCH {add, remove} for inline sources and
  // {enable, disable} for every kind (exclude[], or entries[].enabled).
  http.get(`${B}/blocklists/:ns/:name/sources/:source/entries`, ({ request, params }) => {
    const g = guard(request, 'blocklists', 'read');
    if (g) return g;
    const t = anySource(String(params.ns), String(params.name), String(params.source));
    if (t instanceof Response) return t;
    const all = sourceItems(t.list, t.src, t.source);
    if (!all) {
      return fail(503, 'unavailable', `the fetched body of source "${t.source}" is not on this node yet`, 'the list is fetched at startup and every refresh interval; try again shortly or press Refresh now');
    }
    const url = new URL(request.url);
    const q = (url.searchParams.get('q') ?? '').toLowerCase();
    const enabled = url.searchParams.get('enabled');
    const limit = Math.min(Math.max(Number(url.searchParams.get('limit') ?? '100') || 100, 1), 1000);
    const start = Number(url.searchParams.get('continue') ?? '0') || 0;
    const matching = all.filter((e) => (!q || e.value.toLowerCase().includes(q)) && (!enabled || String(e.enabled) === enabled));
    const next = start + limit < matching.length ? String(start + limit) : undefined;
    const kind = sourceKindOf(t.src);
    const known = new Set(all.map((e) => e.value));
    const st = t.list.status?.sources?.find((x) => x.name === t.source);
    return ok(
      {
        items: matching.slice(start, start + limit),
        ...(next ? { continue: next } : {}),
        total: all.length,
        matched: matching.length,
        excluded: all.filter((e) => !e.enabled).length,
        staleExcludes: kind === 'entries' ? [] : (t.src.exclude ?? []).filter((x) => !known.has(x)).slice(0, 50),
        kind,
        ...(kind === 'url' || kind === 'configMap' ? { ...(st?.digest ? { digest: st.digest } : {}), ...(st?.lastFetched ? { fetched: st.lastFetched } : {}) } : {}),
        stale: false,
      },
      { headers: { ETag: t.list.metadata.resourceVersion ?? '' } },
    );
  }),
  http.patch(`${B}/blocklists/:ns/:name/sources/:source/entries`, async ({ request, params }) => {
    const g = guard(request, 'blocklists', 'write');
    if (g) return g;
    const t = anySource(String(params.ns), String(params.name), String(params.source));
    if (t instanceof Response) return t;
    const owner = t.list.metadata.annotations?.[MANAGED] ?? '';
    if (owner && owner !== 'api' && owner !== 'bootstrap') return fail(409, 'managed_by', `BlockList ${t.list.metadata.name} is managed by ${owner}`, owner);
    const ifMatch = request.headers.get('If-Match');
    if (ifMatch && ifMatch !== t.list.metadata.resourceVersion) return fail(412, 'precondition_failed', `resourceVersion ${ifMatch} is stale`);
    const body = (await request.json()) as ListEntriesPatch;
    const kind = sourceKindOf(t.src);
    if ((body.add?.length || body.remove?.length) && kind !== 'inline') {
      return fail(422, 'unprocessable', `source ${t.source} is a ${kind} source: only inline sources take add and remove; switch entries off with disable`);
    }
    const items = sourceItems(t.list, t.src, t.source);
    if (!items) return fail(503, 'unavailable', `the fetched body of source "${t.source}" is not on this node yet`);
    const inline = [...(t.src.inline ?? [])];
    const success: { item: string }[] = [];
    const errors: { item: string; error: string }[] = [];
    const added: string[] = [];
    const removed: string[] = [];
    for (const raw of body.add ?? []) {
      const e = raw.trim();
      const err = e ? entryError(t.src.format, e) : 'empty entry';
      if (err) errors.push({ item: raw, error: err });
      else if (inline.includes(e)) errors.push({ item: e, error: `already on ${t.name}` });
      else {
        inline.push(e);
        added.push(e);
        success.push({ item: e });
      }
    }
    for (const e of body.remove ?? []) {
      const i = inline.indexOf(e);
      if (i < 0) errors.push({ item: e, error: `not on ${t.name}` });
      else {
        inline.splice(i, 1);
        removed.push(e);
        success.push({ item: e });
      }
    }
    if (kind === 'inline' && (added.length || removed.length)) t.src.inline = inline;
    const current = kind === 'inline' ? inline.map((value, i) => ({ value, line: i + 1, enabled: true })) : items;
    const sw = switchEntries(t.src, current, body.enable ?? [], body.disable ?? []);
    const changed = success.length + sw.enabled.length + sw.disabled.length;
    if (changed) {
      db.rv += 1;
      t.list.metadata.resourceVersion = String(db.rv);
      t.list.metadata.generation = (t.list.metadata.generation ?? 0) + 1;
      t.list.metadata.annotations = { ...(t.list.metadata.annotations ?? {}), [MANAGED]: 'api' };
      compileList(t.list);
      if (kind === 'url' || kind === 'configMap') {
        const inBody = new Set(items.map((e) => e.value));
        const off = (t.src.exclude ?? []).filter((x) => inBody.has(x)).length;
        const stale = (t.src.exclude ?? []).filter((x) => !inBody.has(x));
        t.list.status = {
          ...(t.list.status ?? {}),
          sources: (t.list.status?.sources ?? []).map((x) => {
            if (x.name !== t.source) return x;
            const { staleExcludes: _s, excluded: _e, ...rest } = x;
            return { ...rest, entries: items.length - off, ...(off ? { excluded: off } : {}), ...(stale.length ? { staleExcludes: stale } : {}) };
          }),
        };
        const live = (t.list.status.sources ?? []).filter((x) => x.enabled !== false);
        t.list.status.entries = live.reduce((n, x) => n + (x.entries ?? 0), 0);
        t.list.status.ruleCount = t.list.status.entries;
      }
      audit('blocklist.update', { kind: 'BlockList', namespace: String(params.ns), name: t.list.metadata.name });
    }
    const after = sourceItems(t.list, t.src, t.source) ?? [];
    return ok({
      added,
      removed,
      enabled: sw.enabled,
      disabled: sw.disabled,
      total: after.length,
      excluded: after.filter((e) => !e.enabled).length,
      resourceVersion: t.list.metadata.resourceVersion ?? '',
      // Per-item results of add and remove (rejected entries keep the batch going).
      processed: { success, errors },
    });
  }),
  http.post(`${B}/domains/:action/:kind`, async ({ request, params }) => {
    const g = guard(request, 'blocklists', 'write');
    if (g) return g;
    const action = String(params.action);
    const kind = String(params.kind);
    if (!['allow', 'deny'].includes(action) || !['exact', 'regex'].includes(kind)) return fail(404, 'not_found', 'no such route');
    const body = (await request.json()) as { domains: string[]; namespace?: string; list?: string };
    return ok(compatAdd(action as DomainAction, kind as 'exact' | 'regex', body.namespace ?? 'default', body.domains ?? [], 'api'));
  }),
  ...domainEntryHandlers(),
  http.get(`${B}/clients`, ({ request }) => {
    const g = guard(request, 'clients', 'read');
    if (g) return g;
    const url = new URL(request.url);
    const queries = filterQueries(url);
    // The top-clients ranking of the window; clients without queries are absent.
    const items: ClientItem[] = CLIENTS.flatMap((c) => {
      const rows = queries.filter((q) => q.client.ip === c.ip);
      if (!rows.length) return [];
      return [
        {
          key: c.ip,
          name: c.name,
          group: `default/${c.group}`,
          count: rows.length * 20,
          blocked: rows.filter((q) => q.status_class === 'blocked').length * 20,
          ...(rows[0] ? { last_seen: rows[0].ts } : {}),
        },
      ];
    }).sort((a, b) => b.count - a.count);
    return ok({ ...scopeInfo(url), items });
  }),

  // runtime ---------------------------------------------------------------
  http.get(`${B}/dns/blocking`, ({ request }) => {
    const g = guard(request, 'blocking', 'read');
    if (g) return g;
    return ok(blockingState());
  }),
  http.post(`${B}/dns/blocking`, async ({ request }) => {
    const g = guard(request, 'blocking', 'toggle');
    if (g) return g;
    const body = (await request.json()) as { blocking: boolean; timer?: number; until?: string };
    if (typeof body.blocking !== 'boolean') return fail(400, 'invalid', 'blocking: must be a boolean');
    if (body.timer !== undefined && (!(body.timer > 0) || body.timer > 86_400 * 7)) return fail(400, 'invalid', 'timer: 1 s to 7 days');
    const until = body.blocking ? undefined : body.timer ? Date.now() + body.timer * 1000 : body.until ? new Date(body.until).getTime() : undefined;
    db.blocking = { blocking: body.blocking, ...(until ? { until } : {}) };
    return ok(blockingState());
  }),
  http.post(`${B}/cache/flush`, async ({ request }) => {
    const g = guard(request, 'cache', 'flush');
    if (g) return g;
    const body = (await request.json().catch(() => ({}))) as { name?: string };
    return ok({ flushed: body.name ? 1 : 48213 });
  }),
  http.post(`${B}/dns/query`, async ({ request }) => {
    const g = guard(request, 'querylog', 'read');
    if (g) return g;
    const body = (await request.json()) as Partial<DnsQueryRequest>;
    if (!body.name) return fail(422, 'invalid', 'validation failed: body.name: expected length >= 1');
    if (body.proto && !['udp', 'tcp', 'dot', 'doh', 'doq', 'doh3'].includes(body.proto)) return fail(422, 'invalid', 'body.proto: expected value to be one of "udp, tcp, dot, doh, doq, doh3"');
    return ok({ ...digAnswer({ ...body, name: body.name }) });
  }),

  // stats -----------------------------------------------------------------
  http.get(`${B}/stats/summary`, ({ request }) => {
    const g = guard(request, 'stats', 'read');
    if (g) return g;
    const url = new URL(request.url);
    const t = seriesTotals();
    const types: Record<string, number> = {};
    for (const q of db.queries) types[q.qtype] = (types[q.qtype] ?? 0) + Math.round(t.total / db.queries.length);
    const nodeFactor = url.searchParams.get('node') === 'kudurru-1' ? 0.3 : url.searchParams.get('node') === 'kudurru-0' ? 0.7 : 1;
    const f = (n: number) => Math.round(n * nodeFactor);
    return ok({
      ...scopeInfo(url),
      from: new Date(db.now - windowMs(url)).toISOString(),
      until: new Date(db.now).toISOString(),
      queries: {
        total: f(t.total),
        blocked: f(t.blocked),
        percent_blocked: (t.blocked / t.total) * 100,
        cached: f(t.cached),
        forwarded: f(t.forwarded),
        authoritative: f(t.authoritative),
        dropped: 12,
        other: f(t.other),
        unique_domains: 4211,
        qps_1m: 20.1 * nodeFactor,
        qps_avg: (t.total / 86_400) * nodeFactor,
      },
      clients: { active: 11, total: CLIENTS.length },
      domains_on_lists: db.lists.filter((l) => l.spec.mode !== 'allow').reduce((n, l) => n + (l.status?.entries ?? 0), 0),
      rcodes: { NOERROR: f(t.total * 0.93), NXDOMAIN: f(t.total * 0.06), SERVFAIL: f(t.total * 0.01) },
      protocols: { udp: f(t.total * 0.7), tcp: f(t.total * 0.05), dot: f(t.total * 0.15), doh: f(t.total * 0.1) },
      types,
      latency: { avg_ms: 6.4, p50_ms: 0.4, p95_ms: 31.2, p99_ms: 88.0 },
      approximate: false,
    });
  }),
  http.get(`${B}/stats/timeseries`, ({ request }) => {
    const g = guard(request, 'stats', 'read');
    if (g) return g;
    const url = new URL(request.url);
    const from = url.searchParams.get('from');
    const to = url.searchParams.get('to');
    let points = db.series;
    if (from && to) {
      // Long-term views: synthesise daily points from the same curve.
      const start = new Date(from).getTime();
      const end = new Date(to).getTime();
      const days = Math.max(1, Math.round((end - start) / 86_400_000));
      if (days > 1) {
        const r = rng(start / 1000);
        const tot = seriesTotals();
        points = Array.from({ length: days }, (_, i) => {
          const k = 0.8 + r() * 0.4;
          return {
            ts: new Date(start + i * 86_400_000).toISOString(),
            total: Math.round(tot.total * k),
            blocked: Math.round(tot.blocked * k),
            cached: Math.round(tot.cached * k),
            forwarded: Math.round(tot.forwarded * k),
            authoritative: Math.round(tot.authoritative * k),
            other: Math.round(tot.other * k),
          };
        });
        return ok({ ...scopeInfo(url), from, until: to, step: 86_400, points, approximate: false });
      }
    }
    return ok({ ...scopeInfo(url), from: points[0]?.ts ?? '', until: points[points.length - 1]?.ts ?? '', step: 300, points, approximate: false });
  }),
  http.get(`${B}/stats/top`, ({ request }) => {
    const g = guard(request, 'stats', 'read');
    if (g) return g;
    const url = new URL(request.url);
    const dim = (url.searchParams.get('dim') ?? 'domains') as TopDim;
    const blocked = url.searchParams.get('blocked') === 'true';
    const n = Number(url.searchParams.get('n') ?? '10');
    const t = seriesTotals();
    const longTerm = !!url.searchParams.get('from');
    const items = topFrom(dim, blocked, n, t.total / db.queries.length);
    return ok({ ...scopeInfo(url), from: new Date(db.now - windowMs(url)).toISOString(), until: new Date(db.now).toISOString(), dim, blocked, items, total_in_window: blocked ? t.blocked : t.total - t.blocked, approximate: longTerm, ...(longTerm ? { cutoff_rank: n * 4 } : {}) });
  }),
  http.get(`${B}/stats/upstreams`, ({ request }) => {
    const g = guard(request, 'stats', 'read');
    if (g) return g;
    const url = new URL(request.url);
    const top = topFrom('upstreams', false, 10, seriesTotals().total / db.queries.length);
    // servers[]: the live health of every configured server (not windowed);
    // a recursive group reports its resolver as the server "recursive".
    const servers = Object.entries(upstreamGroups()).flatMap(([group, g], gi) =>
      (g.mode === 'recursive' ? ['recursive'] : (g.servers ?? [])).map((entry, i) => {
        const down = gi === 1 && i === 0 && group !== 'default';
        return {
          group,
          server: serverLabel(entry),
          healthy: !down,
          consecutive_failures: down ? 4 : 0,
          rtt_ms: down ? 0 : 8.4 + i * 5.1,
          ...(down ? { backoff_until: new Date(Date.now() + 40_000).toISOString() } : {}),
        };
      }),
    );
    return ok({
      ...scopeInfo(url),
      from: new Date(db.now - 86_400_000).toISOString(),
      until: new Date(db.now).toISOString(),
      upstreams: top.map((x, i) => ({ addr: x.key, total: x.total, errors: 3 + i, timeouts: i, percent: x.percent, avg_ms: 12 + i * 6, p95_ms: 40 + i * 15 })),
      servers,
      approximate: false,
    });
  }),
  http.get(`${B}/stats/clients/timeseries`, ({ request }) => {
    const g = guard(request, 'stats', 'read');
    if (g) return g;
    const url = new URL(request.url);
    const n = Number(url.searchParams.get('n') ?? '5');
    const top = topFrom('clients', false, n, 1);
    const weights = top.map((x) => x.percent / 100);
    const rest = Math.max(0.05, 1 - weights.reduce((a, b) => a + b, 0) * 0.85);
    return ok({
      ...scopeInfo(url),
      from: db.series[0]?.ts ?? '',
      until: db.series[db.series.length - 1]?.ts ?? '',
      step: 300,
      clients: [...top.map((x) => ({ key: x.key, ...(x.name ? { name: x.name } : {}) })), { key: 'other' }],
      points: db.series.map((p, i) => ({ ts: p.ts, counts: [...weights.map((w, j) => Math.round(p.total * w * 0.85 * (0.85 + ((i * (j + 3)) % 7) / 20))), Math.round(p.total * rest)] })),
      approximate: false,
    });
  }),
  http.get(`${B}/stats/recent_blocked`, ({ request }) => {
    const g = guard(request, 'stats', 'read');
    if (g) return g;
    const url = new URL(request.url);
    const n = Number(url.searchParams.get('n') ?? '10');
    const items = db.queries
      .filter((q) => q.status_class === 'blocked')
      .slice(0, n)
      .map((q) => ({ id: q.id, node: q.node, ts: q.ts, domain: q.domain, client: q.client.ip, ...(q.client.name ? { client_name: q.client.name } : {}), ...(q.list ? { list: q.list } : {}), status: q.status }));
    return ok({ ...scopeInfo(url), items });
  }),

  // query log ---------------------------------------------------------------
  http.get(`${B}/queries`, ({ request }) => {
    const g = guard(request, 'querylog', 'read');
    if (g) return g;
    const url = new URL(request.url);
    const all = filterQueries(url);
    const cursor = url.searchParams.get('cursor');
    const limit = Math.min(500, Number(url.searchParams.get('limit') ?? '100'));
    const after = cursor ? all.filter((r) => r.id < Number(cursor)) : all;
    const page = after.slice(0, limit);
    const last = page[page.length - 1];
    return ok({
      ...scopeInfo(url),
      queries: page,
      ...(after.length > limit && last ? { cursor_next: String(last.id) } : {}),
      records_total: db.queries.length,
      records_filtered: all.length,
      earliest_ts_disk: db.queries[db.queries.length - 1]?.ts,
      earliest_ts_memory: db.queries[Math.min(199, db.queries.length - 1)]?.ts,
    });
  }),
  http.get(`${B}/queries/suggestions`, ({ request }) => {
    const g = guard(request, 'querylog', 'read');
    if (g) return g;
    const url = new URL(request.url);
    const field = url.searchParams.get('field') ?? 'domain';
    const prefix = (url.searchParams.get('prefix') ?? '').toLowerCase();
    const values = new Set<string>();
    for (const q of db.queries) {
      const v = field === 'client' ? q.client.name ?? q.client.ip : field === 'upstream' ? q.upstream : field === 'list' ? q.list : field === 'zone' ? q.zone : field === 'policy' ? q.policy : q.domain;
      if (v && v.toLowerCase().includes(prefix)) values.add(v);
      if (values.size >= 10) break;
    }
    return ok({ field, values: [...values] });
  }),
  // msw refuses to build an SSE handler where EventSource is missing (jsdom).
  ...(typeof EventSource === 'function'
    ? [
        sse<{ query: QueryRow }>(`${B}/queries/stream`, ({ client, request }) => {
          const last = Number(request.headers.get('Last-Event-ID') ?? new URL(request.url).searchParams.get('last_event_id') ?? '0');
          if (last) {
            for (const q of db.queries.filter((x) => x.id > last).reverse()) client.send({ id: String(q.id), event: 'query', data: q });
          }
          const r = rng(Date.now() % 100000);
          window.clearInterval(tailTimer);
          tailTimer = window.setInterval(() => {
            const row = makeQueryRow(db.nextQueryID++, Date.now(), r, r() < 0.7 ? 'kudurru-0' : 'kudurru-1');
            db.queries.unshift(row);
            client.send({ id: String(row.id), event: 'query', data: row });
          }, 1200);
        }),
      ]
    : []),
  http.post(`${B}/queries/:id/actions`, async ({ request, params }) => {
    const g = guard(request, 'blocklists', 'write');
    if (g) return g;
    const row = db.queries.find((q) => q.id === Number(params.id));
    if (!row) return fail(404, 'not_found', `query ${String(params.id)} is not in the log`);
    const body = (await request.json()) as { action: 'allow' | 'deny' };
    if (body.action !== 'allow' && body.action !== 'deny') return fail(400, 'invalid', 'action: allow or deny');
    return ok(compatAdd(body.action, 'exact', 'default', [row.domain], 'querylog'));
  }),
  http.delete(`${B}/querylog`, ({ request }) => {
    const g = guard(request, 'querylog', 'purge');
    if (g) return g;
    const before = new URL(request.url).searchParams.get('before');
    const n = db.queries.length;
    db.queries = before ? db.queries.filter((q) => q.ts >= new Date(before).toISOString()) : [];
    return ok({ deleted: n - db.queries.length, node: 'kudurru-0' });
  }),
  http.get(`${B}/querylog/status`, ({ request }) => {
    const g = guard(request, 'querylog', 'read');
    if (g) return g;
    return ok({
      rows: 1_204_332,
      db_bytes: 171_000_000,
      wal_bytes: 4_100_000,
      oldest_ts: new Date(db.now - 7 * 86_400_000).toISOString(),
      privacy: 'full',
      retention: { raw_seconds: 604_800, max_db_bytes: 10 * 1024 ** 3, rollup_1m: '7d', rollup_1h: '90d', rollup_1d: '400d' },
      subscribers: 1,
      node: 'kudurru-0',
    });
  }),

  // settings ----------------------------------------------------------------
  http.post(`${B}/settings/sections/:section/ownership/:action`, async ({ request, params }) => {
    const g = guard(request, 'settings', 'write'); if (g) return g;
    if (params.section !== 'upstreams') return fail(400, 'invalid', 'Only upstreams supports ownership.');
    const { revision } = await request.json() as { revision: string };
    if (revision !== String(db.settingsRevision ?? 0)) return fail(412, 'precondition_failed', 'Settings changed. Refresh and review.');
    if (params.action !== 'adopt' && params.action !== 'release') return fail(400, 'invalid', 'Unknown ownership action.');
    db.settingsOwner = params.action === 'adopt' ? 'ui' : 'gitops';
    db.settingsRevision = (db.settingsRevision ?? 0) + 1;
    return ok({ ...settingsBody('cluster'), persisted: true });
  }),
  http.post(`${B}/settings/sections/:section/yaml`, async ({ request, params }) => {
    const g = guard(request, 'settings', 'read'); if (g) return g;
    if (params.section !== 'upstreams') return fail(400, 'invalid', 'Only upstreams supports YAML generation.');
    const { proposed } = await request.json() as { proposed?: Record<string, unknown> };
    return ok({ yaml: toYAML({ config: { settings: { upstreams: proposed ?? db.settings.upstreams } } }), revision: String(db.settingsRevision ?? 0) });
  }),
  http.get(`${B}/settings/_schema`, ({ request }) =>
    guard(request, 'settings', 'read') ??
    ok({ items: db.schema.map((k) => ({ ...k, value: k.writeOnly ? '<redacted>' : getPath(k.scope === 'node' ? db.nodeConfig : db.settings, k.path) })) }),
  ),
  http.get(`${B}/settings`, ({ request }) => guard(request, 'settings', 'read') ?? ok(settingsBody('cluster'))),
  http.patch(`${B}/settings`, async ({ request }) => {
    const g = guard(request, 'settings', 'write');
    if (g) return g;
    const expected = request.headers.get('If-Match');
    if (expected && expected !== String(db.settingsRevision ?? 0)) return fail(412, 'precondition_failed', 'Settings changed. Refresh and review.');
    if (db.settingsOwner === 'gitops') return fail(409, 'managed_by', 'upstreams are managed by GitOps');
    if (db.settingsManagedBy && db.settingsOwner !== 'ui') {
      return fail(409, 'managed_by', `cluster settings are managed by ${db.settingsManagedBy}; change them there`, 'set config.settings in the Helm values (or edit the ConfigMap); every pod reloads it without a restart');
    }
    const patch = (await request.json()) as Record<string, unknown>;
    const changed = changedKeys(patch);
    const bad = changed.find((p) => db.schema.find((k) => k.path === p)?.hostOnly || db.schema.find((k) => k.path === p)?.scope === 'node');
    if (bad) return fail(400, 'host_only', `${bad} can only be set in kudurru.yaml or the environment on the host`, bad);
    const unknown = changed.find((p) => !db.schema.some((k) => k.path === p));
    if (unknown) return fail(400, 'invalid', `unknown settings key ${unknown}`);
    const next = withDefaults(mergePatch(db.settings, patch), patch, changed);
    const bad1 = validateSchema(next, changed);
    if (bad1) return fail(400, 'invalid', bad1.message, bad1.hint, undefined, [bad1.field]);
    const invalid = validateUpstreams(next) ?? validateBlockPage(next);
    if (invalid) return fail(400, 'invalid', invalid);
    db.settings = next;
    db.settingsRevision = (db.settingsRevision ?? 0) + 1;
    return ok({ ...settingsBody('cluster', changed), persisted: db.settingsPersisted });
  }),
  http.get(`${B}/node/config`, ({ request }) => guard(request, 'node', 'config') ?? ok(settingsBody('node'))),
  http.patch(`${B}/node/config`, async ({ request }) => {
    const g = guard(request, 'node', 'config');
    if (g) return g;
    const patch = (await request.json()) as Record<string, unknown>;
    const changed = changedKeys(patch);
    const bad = changed.find((p) => db.schema.find((k) => k.path === p)?.hostOnly);
    if (bad) return fail(400, 'host_only', `${bad} can only be set in kudurru.yaml or the environment on the host`, bad);
    const next = withDefaults(mergePatch(db.nodeConfig, patch), patch, changed);
    const invalid = validateSchema(next, changed);
    if (invalid) return fail(400, 'invalid', invalid.message, invalid.hint, undefined, [invalid.field]);
    db.nodeConfig = next;
    // Node-scope changes live in memory only.
    return ok({ ...settingsBody('node', changed), persisted: false });
  }),

  // nodes and cluster -------------------------------------------------------
  http.get(`${B}/nodes`, ({ request }) => guard(request, 'cluster', 'read') ?? ok({ items: db.nodes })),
  http.get(`${B}/nodes/:name/health`, ({ request, params }) => {
    const g = guard(request, 'cluster', 'read');
    if (g) return g;
    const n = db.nodes.find((x) => x.name === params.name);
    if (!n) return fail(404, 'not_found', `node ${String(params.name)} not found`);
    if (!n.self) return fail(501, 'not_implemented', "health of other nodes needs scatter-gather over the internal listener", `ask ${n.name} directly`);
    const since = new Date(db.now - 3 * 86_400_000).toISOString();
    return ok({
      node: n.name,
      ready: true,
      health: {
        status: 'ready',
        node: n.name,
        version: n.version,
        unready: null,
        components: {
          blocking: { ready: true, since },
          blocklists: { ready: true, since },
          dns: { ready: true, since },
          objects: { ready: true, since },
          zonecache: { ready: true, since },
        },
      },
    });
  }),
  http.get(`${B}/cluster`, ({ request }) =>
    guard(request, 'cluster', 'read') ??
    ok({ clusterID: 'dc493171-2dd3-4282-b0ab-c1a0a31ea26f', driver: 'sqlite', epoch: 7, leader: 'kudurru-0', nodes: db.nodes.length, readOnly: false, node: 'kudurru-0' }),
  ),
  http.get(`${B}/edge`, ({ request }) =>
    guard(request, 'edge', 'read') ??
    ok({ edge: { enabled: true, state: 'running' }, vip: { enabled: true, state: 'MASTER' }, backends: [{ name: 'kudurru-0', address: '192.168.1.2:53', up: true }, { name: 'kudurru-1', address: '192.168.1.3:53', up: true }], node: 'kudurru-0' }),
  ),
  http.get(`${B}/info/messages`, ({ request }) => guard(request, 'settings', 'read') ?? ok({ items: db.messages })),
  http.get(`${B}/version`, () => ok({ version: 'v0.1.0-rc.1', commit: '3e52e5a', date: '2026-10-10T18:00:00Z', go: 'go1.27.2', platform: 'linux/amd64', node: 'kudurru-0', api: 'v1' })),

  // iam and audit -------------------------------------------------------------
  http.get(`${B}/iam/:what`, ({ request, params }) => {
    const what = String(params.what);
    const g = guard(request, what === 'rolebindings' ? 'rolebindings' : what === 'identityproviders' ? 'identityproviders' : what === 'serviceaccounts' ? 'serviceaccounts' : 'users', 'read');
    if (g) return g;
    const items: Record<string, unknown[]> = {
      users: db.users.map((u) => ({
        name: u.username,
        uid: `mock-user-${u.username}`,
        ...(u.name !== u.username ? { displayName: u.name } : {}),
        builtinAdmin: u.username === 'admin',
        disabled: false,
        mustChangePassword: !!u.mustChangePassword,
        ...(u.roles.includes('admin') ? { groups: ['kudurru-admins'] } : {}),
        lastLoginAt: new Date(db.now - 3_600_000).toISOString(),
      })),
      serviceaccounts: [
        { namespace: 'default', name: 'grafana', description: 'dashboards', disabled: false, builtinAdmin: false, tokens: 1 },
        { namespace: 'kudurru', name: 'external-dns', disabled: false, builtinAdmin: false, tokens: 0 },
      ],
      rolebindings: [
        { source: 'config', name: 'rbac.bindings[0]', role: 'admin', scope: { kind: 'cluster' }, subjects: [{ kind: 'Group', name: 'kudurru-admins' }] },
        { source: 'object', namespace: 'default', name: 'viewers', role: 'viewer', scope: { kind: 'cluster' }, subjects: [{ kind: 'Group', name: 'everyone' }], managedBy: 'file:objects.d/rbac.yaml' },
        { source: 'object', namespace: 'default', name: 'home-admin', role: 'zone-admin', scope: { kind: 'zone', name: 'home-arpa' }, subjects: [{ kind: 'User', name: 'robert@example.com' }] },
        { source: 'object', namespace: 'kudurru', name: 'external-dns', role: 'integration:external-dns', scope: { kind: 'cluster' }, subjects: [{ kind: 'ServiceAccount', namespace: 'kudurru', name: 'external-dns' }] },
      ],
      identityproviders: [
        { id: 'dex', displayName: 'Dex (corporate SSO)', issuer: 'https://dex.example.com', clientID: 'kudurru', scopes: ['openid', 'email', 'groups'], redirectURL: 'https://kudurru.example.com/api/v1/auth/oidc/dex/callback', source: 'config' },
        { id: 'github', displayName: 'GitHub', issuer: 'https://token.actions.githubusercontent.com', clientID: 'kudurru-gh', scopes: ['openid'], redirectURL: 'https://kudurru.example.com/api/v1/auth/oidc/github/callback', source: 'object' },
      ],
    };
    return items[what] ? ok({ items: items[what] }) : fail(404, 'not_found', `no IAM collection ${what}`);
  }),
  http.get(`${B}/license`, ({ request }) =>
    guard(request, 'settings', 'read') ?? ok({ ...db.license, nodesInUse: db.nodes.length, overLimit: db.nodes.length > db.license.nodes }),
  ),
  http.get(`${B}/audit/verify`, ({ request }) =>
    guard(request, 'audit', 'read') ?? needsLicense('audit', 'the audit trail') ?? ok({ verified: true, checkedAt: new Date(db.now).toISOString() }),
  ),
  http.get(`${B}/audit`, ({ request }) => {
    const g = guard(request, 'audit', 'read') ?? needsLicense('audit', 'the audit trail');
    if (g) return g;
    const url = new URL(request.url);
    const p = url.searchParams;
    const kind = p.get('kind');
    const actor = p.get('actor');
    const name = p.get('name');
    const desc = p.get('order') === 'desc';
    const limit = Math.min(1000, Number(p.get('limit') || '100'));
    const cursor = Number(p.get('cursor') || '0');
    // db.audit is newest first; the server default is oldest first.
    let items = db.audit.filter(
      (a) => (!kind || a.resource.kind === kind) && (!actor || (a.actor.name ?? '').includes(actor) || `${a.actor.kind}:${a.actor.name}` === actor) && (!name || (a.resource.name ?? '').includes(name)),
    );
    if (!desc) items = [...items].reverse();
    if (cursor) items = items.filter((a) => (desc ? (a.id ?? 0) < cursor : (a.id ?? 0) > cursor));
    const page = items.slice(0, limit);
    const last = page[page.length - 1];
    const next = page.length === limit && last?.id ? String(last.id) : undefined;
    return ok({ items: page, node: 'kudurru-0', ...(next ? { cursor_next: next, continue: next } : {}) });
  }),

  // policy state (ux-plan C3) ----------------------------------------------
  http.get(`${B}/policies/:ns/:name/state`, ({ request, params }) => {
    const g = guard(request, 'policies', 'read');
    if (g) return g;
    const p = db.policies.find((x) => x.metadata.name === params.name && (x.metadata.namespace ?? 'default') === params.ns);
    if (!p) return fail(404, 'not_found', `Policy ${String(params.ns)}/${String(params.name)} not found`);
    return ok(policyState(p));
  }),

  // block page (C5) -------------------------------------------------------
  http.get(`${B}/blockpage/preview`, ({ request }) => {
    const g = guard(request, 'settings', 'read');
    if (g) return g;
    const domain = new URL(request.url).searchParams.get('domain') || 'example.com';
    const esc = (t: string) => t.replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`);
    const t = getPath(db.settings, 'blockPage.title');
    const title = esc(typeof t === 'string' && t ? t : 'This site is blocked');
    const html = `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>${title}</title><style>body{font-family:system-ui,sans-serif;margin:0;display:grid;place-items:center;min-height:100vh;background:#f6f5f2;color:#1d1b18}main{max-width:36rem;padding:2rem;border-radius:12px;background:#fff;box-shadow:0 1px 4px #0002}code{font-size:1.1em}dl{display:grid;grid-template-columns:auto 1fr;gap:.25rem 1rem}@media (prefers-color-scheme:dark){body{background:#1b1a18;color:#eee}main{background:#26241f}}</style></head><body><main><p>kudurru</p><h1>${title}</h1><p><code>${esc(domain)}</code> is blocked on this network.</p><dl><dt>Why</dt><dd>list default/ads (rule ||${esc(domain)}^)</dd><dt>Client</dt><dd>192.168.1.30</dd><dt>Node</dt><dd>kudurru-0</dd></dl><p>Ask your administrator if you think this is a mistake.</p></main></body></html>`;
    return new HttpResponse(html, { headers: { 'Content-Type': 'text/html; charset=utf-8' } });
  }),

  // blocked-services and safe-search catalogue (D60) ----------------------
  // The icon routes are public (plain <img src>).
  http.get(`${B}/catalogue/icons`, () => ok({ items: Object.keys(MOCK_ICONS).map((id) => ({ id, path: `/api/v1/catalogue/icons/${id}.svg`, ...(ON_DARK.has(id) ? { onDark: `/api/v1/catalogue/icons/${id}-dark.svg` } : {}) })) })),
  http.get(`${B}/catalogue/icons/:file`, ({ params }) => {
    const file = String(params.file).replace(/\.svg$/, '');
    const light = file.endsWith('-dark') && ON_DARK.has(file.slice(0, -'-dark'.length));
    const id = light ? file.slice(0, -'-dark'.length) : file;
    const icon = light && MOCK_ICONS[id] ? (['#f2f2f2', MOCK_ICONS[id][1]] as [string, string]) : MOCK_ICONS[id];
    if (!icon) return fail(404, 'not_found', `no icon ${id}`);
    const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" rx="7" fill="${icon[0]}"/><text x="16" y="21.5" font-family="system-ui,sans-serif" font-size="15" font-weight="700" text-anchor="middle" fill="${light ? '#111' : '#fff'}">${icon[1]}</text></svg>`;
    return new HttpResponse(svg, { headers: { 'Content-Type': 'image/svg+xml', 'Cache-Control': 'public, max-age=86400, immutable' } });
  }),
  http.get(`${B}/catalogue`, ({ request }) =>
    guard(request, 'policies', 'read') ??
    ok({
      schemaVersion: 2,
      version: '2026.10.12',
      license: 'Apache-2.0',
      origin: 'embedded',
      digest: '0'.repeat(64),
      services: MOCK_SERVICES.length,
      categories: MOCK_CATEGORIES.length,
      safeSearchEngines: MOCK_ENGINES.length,
      sources: [
        { id: 'kudurru', name: 'kudurru catalogue', license: 'Apache-2.0', origin: 'embedded', digest: '0'.repeat(64), loaded: true, services: MOCK_SERVICES.filter((s) => s.origin === 'kudurru').length },
        { id: 'adguard', name: 'AdGuard HostlistsRegistry services (opt-in)', license: 'GPL-3.0', origin: 'https://mirror.example/services.json', digest: '1'.repeat(64), loaded: true, services: 1, shadowed: 3 },
      ],
    }),
  ),
  http.get(`${B}/catalogue/services`, ({ request }) => guard(request, 'policies', 'read') ?? ok({ items: MOCK_SERVICES })),
  http.get(`${B}/catalogue/categories`, ({ request }) =>
    guard(request, 'policies', 'read') ?? ok({ items: MOCK_CATEGORIES.map((c) => ({ ...c, services: MOCK_SERVICES.filter((s) => s.category === c.id).length })) }),
  ),
  http.get(`${B}/catalogue/safesearch`, ({ request }) =>
    guard(request, 'policies', 'read') ??
    ok({
      items: [
        ...MOCK_ENGINES,
      ],
    }),
  ),

  // unblock requests ------------------------------------------------------
  ...unblockHandlers(),
];

function blockingState() {
  if (db.blocking.until && db.blocking.until <= Date.now()) db.blocking = { blocking: true };
  const until = db.blocking.until;
  return {
    blocking: db.blocking.blocking,
    ...(until ? { until: new Date(until).toISOString(), timer: Math.max(0, Math.round((until - Date.now()) / 1000)) } : {}),
    configured: db.blockingConfigured ?? (getPath(db.settings, 'blocking.enabled') !== false),
    scope: 'node',
    node: 'kudurru-0',
  };
}

/** GET|PATCH /settings and /node/config: the envelope around one scope's document. */
function settingsBody(scope: 'cluster' | 'node', changed?: string[]) {
  const restart = (changed ?? []).filter((p) => db.schema.find((k) => k.path === p)?.restartRequired);
  return {
    scope,
    node: 'kudurru-0',
    settings: scope === 'node' ? db.nodeConfig : db.settings,
    ...(scope === 'cluster' && db.settingsOwner ? { sections: { upstreams: { owner: db.settingsOwner, switchable: true, revision: String(db.settingsRevision ?? 0), envLocked: false } }, conflicts: db.settingsConflict ? ['upstreams'] : [] } : {}),
    ...(changed?.length ? { changed } : {}),
    ...(restart.length ? { restartRequired: restart } : {}),
  };
}

/** The schema keys a merge patch touches (map-typed keys stop at the key). */
function changedKeys(patch: Record<string, unknown>): string[] {
  const out = new Set<string>();
  for (const leaf of leafPaths(patch)) {
    const segs = leaf.split('.');
    let hit = leaf;
    for (let i = segs.length; i > 0; i--) {
      const p = segs.slice(0, i).join('.');
      if (db.schema.some((k) => k.path === p)) {
        hit = p;
        break;
      }
    }
    out.add(hit);
  }
  return [...out];
}

/** A null at a key resets it to its default (internal/api patchSettings). */
function withDefaults(next: Record<string, unknown>, patch: Record<string, unknown>, changed: string[]): Record<string, unknown> {
  for (const p of changed) {
    if (getPath(patch, p) !== null) continue;
    const k = db.schema.find((x) => x.path === p);
    if (k) setPath(next, p, structuredClone(k.default));
  }
  return next;
}

/** The C1 checks the server makes from the schema metadata (enum and bounds), with error.fields and the allowed values as the hint. */
function validateSchema(next: Record<string, unknown>, changed: string[]): { message: string; field: string; hint?: string } | null {
  for (const p of changed) {
    const k = db.schema.find((x) => x.path === p);
    const v = getPath(next, p);
    if (!k) continue;
    if (k.enum?.length && typeof v === 'string' && !k.enum.some((o) => o.value === v)) {
      const allowed = k.enum.map((o) => o.value);
      return { message: `settings: ${p}: "${v}" is not allowed`, field: p, hint: `${p} must be one of: ${allowed.join(', ')}` };
    }
    if (typeof v === 'number' && ((typeof k.min === 'number' && v < k.min) || (typeof k.max === 'number' && v > k.max))) {
      return { message: `settings: ${p}: ${v} is outside ${k.min ?? '-∞'}..${k.max ?? '∞'}`, field: p };
    }
  }
  return null;
}

/** What internal/config/blockpage.go refuses (the node's listener has an address in the mock). */
function validateBlockPage(settings: Record<string, unknown>): string | null {
  if (getPath(settings, 'blocking.defaultResponse') === 'blockPage' && getPath(settings, 'blockPage.enabled') !== true) {
    return 'settings: blocking.defaultResponse blockPage needs blockPage.enabled: true (the block page listener)';
  }
  return null;
}

const STRATEGIES = ['', 'parallel_best', 'strict', 'random', 'rtt_weighted'];

/** What internal/upstream Config.Validate refuses, in its words. */
function validateUpstreams(settings: Record<string, unknown>): string | null {
  const groups = (getPath(settings, 'upstreams.groups') as Record<string, { mode?: string; servers?: string[] | null; strategy?: string }> | undefined) ?? {};
  if (!groups.default) return 'upstreams: upstream: group "default" is required';
  for (const [n, g] of Object.entries(groups)) {
    if (g.mode === 'recursive') {
      if (g.servers?.length) return `upstreams.groups[${n}].servers: a recursive group asks the root servers itself; remove the servers or use mode forward`;
      continue;
    }
    if (g.mode && g.mode !== 'forward') return `upstreams.groups[${n}].mode: "${g.mode}" is not allowed`;
    if (!g.servers?.length) return `upstreams: upstream: group "${n}" has no servers`;
    if (!STRATEGIES.includes(g.strategy ?? '')) return `upstreams: upstream: group "${n}": unknown strategy "${g.strategy}"`;
  }
  const cond = (getPath(settings, 'upstreams.conditional') as Record<string, string> | undefined) ?? {};
  for (const [suffix, g] of Object.entries(cond)) if (!groups[g]) return `upstreams: conditional ${suffix}: unknown group "${g}"`;
  return null;
}

function leafPaths(obj: Record<string, unknown>, prefix = ''): string[] {
  return Object.entries(obj).flatMap(([k, v]) => {
    const p = prefix ? `${prefix}.${k}` : k;
    return v && typeof v === 'object' && !Array.isArray(v) ? leafPaths(v as Record<string, unknown>, p) : [p];
  });
}

/** RFC 7396 JSON merge patch. */
export function mergePatch(target: Record<string, unknown>, patch: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = { ...target };
  for (const [k, v] of Object.entries(patch)) {
    if (v === null) delete out[k];
    else if (typeof v === 'object' && !Array.isArray(v)) out[k] = mergePatch((out[k] as Record<string, unknown>) ?? {}, v as Record<string, unknown>);
    else out[k] = v;
  }
  return out;
}

export type { Policy };

/** Logos the mock serves (GET /catalogue/icons): a brand colour and a glyph. */
const MOCK_ICONS: Record<string, [string, string]> = {
  youtube: ['#ff0000', 'Y'],
  netflix: ['#141414', 'N'],
  tiktok: ['#010101', 'T'],
  facebook: ['#1877f2', 'f'],
  instagram: ['#c13584', 'I'],
  twitch: ['#9146ff', 'T'],
  discord: ['#5865f2', 'D'],
  spotify: ['#1db954', 'S'],
  steam: ['#1b2838', 'S'],
  chatgpt: ['#10a37f', 'G'],
  google: ['#4285f4', 'G'],
  bing: ['#008373', 'b'],
  duckduckgo: ['#de5833', 'D'],
  // Upstream providers (UpstreamWizardPage presets).
  cloudflare: ['#f38020', 'C'],
  'adguard-home': ['#68bc71', 'A'],
  nextdns: ['#007bff', 'N'],
  mullvad: ['#294d73', 'M'],
  cisco: ['#049fd9', 'C'],
  'control-d': ['#1a1a1a', 'D'],
};

/** Logos drawn dark on transparent get a light variant for dark backgrounds (GET /catalogue/icons onDark). */
const ON_DARK = new Set(['tiktok', 'control-d']);

const icon = (id: string) =>
  MOCK_ICONS[id] ? { icon: `/api/v1/catalogue/icons/${id}.svg`, ...(ON_DARK.has(id) ? { iconOnDark: `/api/v1/catalogue/icons/${id}-dark.svg` } : {}) } : {};

const svc = (id: string, name: string, category: string, domains: string[], extra: { requires?: string[]; related?: string[]; note?: string; origin?: 'kudurru' | 'adguard' } = {}) => ({
  id,
  name,
  category,
  list: `service:${id}`,
  rules: domains.length,
  domains: domains.map((d) => `||${d}^`),
  source: extra.origin === 'adguard' ? 'adguard-hostlists-registry' : 'first-party domains of the service',
  origin: extra.origin ?? ('kudurru' as const),
  license: extra.origin === 'adguard' ? 'GPL-3.0' : 'Apache-2.0',
  ...(extra.requires ? { requires: extra.requires } : {}),
  ...(extra.related ? { related: extra.related } : {}),
  ...(extra.note ? { note: extra.note } : {}),
  ...icon(id),
});

/** GET /catalogue/categories (the counts are filled in by the handler). */
const MOCK_CATEGORIES = [
  { id: 'ai', name: 'AI', description: 'Chatbots and assistants, and image, video, music and voice generators.', icon: 'sparkles', order: 10 },
  { id: 'social', name: 'Social media', description: 'Social networks, short video and photo sharing.', icon: 'users', order: 20 },
  { id: 'messaging', name: 'Messaging', description: 'Chat apps with voice and video calls.', icon: 'message-circle', order: 30 },
  { id: 'video', name: 'Video and streaming', description: 'Streaming services, video sites and live streaming.', icon: 'tv', order: 40 },
  { id: 'music', name: 'Music and audio', description: 'Music streaming and audio platforms.', icon: 'music', order: 50 },
  { id: 'gaming', name: 'Gaming', description: 'Game stores, console networks and online games.', icon: 'gamepad-2', order: 60 },
  { id: 'other', name: 'Other', description: 'Services without a matching category, such as imported ones.', icon: 'shapes', order: 1000 },
];

/** Entries of the embedded catalogue for GET /catalogue/services (roblox, fortnite and 9gag have no logo; example-tool is an AdGuard import). */
const MOCK_SERVICES = [
  svc('youtube', 'YouTube', 'video', ['youtube.com', 'ytimg.com']),
  svc('netflix', 'Netflix', 'video', ['netflix.com']),
  svc('twitch', 'Twitch', 'video', ['twitch.tv', 'ttvnw.net']),
  svc('tiktok', 'TikTok', 'social', ['tiktok.com']),
  svc('facebook', 'Facebook', 'social', ['facebook.com', 'fbcdn.net'], { related: ['instagram'] }),
  svc('instagram', 'Instagram', 'social', ['instagram.com', 'cdninstagram.com'], { related: ['facebook'] }),
  svc('9gag', '9GAG', 'social', ['9gag.com']),
  svc('discord', 'Discord', 'messaging', ['discord.com', 'discord.gg']),
  svc('spotify', 'Spotify', 'music', ['spotify.com']),
  svc('roblox', 'Roblox', 'gaming', ['roblox.com']),
  svc('epicgames', 'Epic Games', 'gaming', ['epicgames.com']),
  svc('fortnite', 'Fortnite', 'gaming', ['fortnite.com'], { requires: ['epicgames'], note: 'Fortnite signs in through Epic Games.' }),
  svc('steam', 'Steam', 'gaming', ['steampowered.com', 'steamcommunity.com']),
  svc('chatgpt', 'ChatGPT', 'ai', ['chatgpt.com', 'openai.com', 'sora.com']),
  svc('claude', 'Claude', 'ai', ['claude.ai', 'anthropic.com']),
  svc('gemini', 'Google Gemini', 'ai', ['gemini.google.com', 'aistudio.google.com']),
  svc('perplexity', 'Perplexity', 'ai', ['perplexity.ai']),
  svc('example-tool', 'Example Tool', 'other', ['tool.example'], { origin: 'adguard' }),
];

/** GET /catalogue/safesearch (pixabay has no logo). */
const MOCK_ENGINES = [
  { id: 'google', name: 'Google Search', list: 'safesearch:google', target: 'forcesafesearch.google.com', domains: 187, source: 'https://support.google.com/websearch/answer/186669', ...icon('google') },
  { id: 'youtube', name: 'YouTube', list: 'safesearch:youtube', target: 'restrict.youtube.com', moderateTarget: 'restrictmoderate.youtube.com', domains: 5, source: 'https://knowledge.workspace.google.com/admin/youtube/control-youtube-content-available-to-users', ...icon('youtube') },
  { id: 'bing', name: 'Bing', list: 'safesearch:bing', target: 'strict.bing.com', domains: 2, source: 'https://help.bing.microsoft.com/', ...icon('bing') },
  { id: 'duckduckgo', name: 'DuckDuckGo', list: 'safesearch:duckduckgo', target: 'safe.duckduckgo.com', domains: 1, source: 'https://duckduckgo.com/duckduckgo-help-pages/', ...icon('duckduckgo') },
  { id: 'pixabay', name: 'Pixabay', list: 'safesearch:pixabay', target: 'safesearch.pixabay.com', domains: 1, source: 'https://pixabay.com/' },
];

/** GET /policies/{ns}/{name}/state: the objects' state plus the query log of the last 24 hours. */
function policyState(p: Policy) {
  const st = derivePolicyState(p, { policies: db.policies, groups: db.groups, lists: db.lists, services: MOCK_SERVICES, categories: MOCK_CATEGORIES.map((c) => ({ ...c, services: 0 })), engines: MOCK_ENGINES, now: new Date() });
  const ref = st.policy;
  const rows = db.queries.filter((q) => q.policy === ref);
  const blocked = rows.filter((q) => q.status_class === 'blocked');
  const count = (keys: string[]) => {
    const m = new Map<string, number>();
    for (const k of keys) m.set(k, (m.get(k) ?? 0) + 1);
    return [...m.entries()].sort((a, b) => b[1] - a[1]).slice(0, 5);
  };
  const names = new Map(rows.map((q) => [q.client.ip, q.client.name]));
  const scale = 20;
  return {
    ...st,
    // The server fills in blocking.defaultResponse when the policy sets none.
    blockMode: st.blockMode || ((getPath(db.settings, 'blocking.defaultResponse') as string | undefined) ?? 'zeroIP'),
    // The upstream group exists when upstreams.groups names it.
    ...(st.upstreamGroup ? { upstreamGroup: { name: st.upstreamGroup.name, exists: st.upstreamGroup.name in upstreamGroups() } } : {}),
    groups: st.groups.map((g) => ({ ...g, seen24h: new Set(db.queries.filter((q) => q.client.group === g.ref).map((q) => q.client.ip)).size })),
    stats24h: {
      queries: rows.length * scale,
      blocked: blocked.length * scale,
      blockedPercent: rows.length ? Math.round((blocked.length * 1000) / rows.length) / 10 : 0,
      topBlocked: count(blocked.map((q) => q.domain)).map(([domain, n]) => ({ domain, count: n * scale })),
      topClients: count(rows.map((q) => q.client.ip)).map(([key, n]) => ({ key, ...(names.get(key) ? { name: names.get(key) } : {}), count: n * scale })),
    },
  };
}

// ---------------------------------------------------------------------------
// Allowlist / Denylist (allowlist contract C)

function writeFailed(e: unknown): Response {
  if (e instanceof WriteError) return fail(e.status, e.key, e.message, e.hint);
  throw e;
}

function entriesPage(request: Request, action: DomainAction | undefined) {
  const sp = new URL(request.url).searchParams;
  const ns = sp.get('namespace') || undefined;
  const all = collectEntries(db, action, ns);
  const matching = filterEntries(all, { kind: sp.get('kind'), type: sp.get('type'), enabled: sp.get('enabled'), scope: sp.get('scope'), q: sp.get('q') });
  const limit = Math.min(Math.max(Number(sp.get('limit') ?? '100') || 100, 1), 1000);
  const start = Number(sp.get('continue') ?? '0') || 0;
  const next = start + limit < matching.length ? String(start + limit) : undefined;
  return ok({
    items: matching.slice(start, start + limit),
    ...(next ? { continue: next } : {}),
    total: all.length,
    matched: matching.length,
    lists: listInfos(db, action, ns),
    unattached: unattachedOf(db, action, ns),
  });
}

function domainEntryHandlers() {
  const routes = (['allow', 'deny'] as const).flatMap((action) => {
    const base = `${B}/${action}list`;
    return [
      http.get(base, ({ request }) => guard(request, 'blocklists', 'read') ?? entriesPage(request, action)),
      http.post(base, async ({ request }) => {
        const g = guard(request, 'blocklists', 'write');
        if (g) return g;
        try {
          const body = (await request.json()) as DomainEntriesCreate;
          const r = createEntries(db, action, body, actorOf());
          if (r.processed.success.length) audit('blocklist.update', { kind: 'BlockList', namespace: body.namespace || 'default', name: `ui-${action}` });
          return ok({ ...r });
        } catch (e) {
          return writeFailed(e);
        }
      }),
      http.get(`${base}/:id`, ({ request, params }) => {
        const g = guard(request, 'blocklists', 'read');
        if (g) return g;
        const e = collectEntries(db, action).find((x) => x.id === String(params.id));
        return e ? ok({ ...e }) : fail(404, 'not_found', `no ${action} entry ${String(params.id)}`);
      }),
      http.patch(`${base}/:id`, async ({ request, params }) => {
        const g = guard(request, 'blocklists', 'write');
        if (g) return g;
        try {
          return ok({ ...patchEntry(db, action, String(params.id), (await request.json()) as DomainEntryPatch) });
        } catch (e) {
          return writeFailed(e);
        }
      }),
      http.delete(`${base}/:id`, ({ request, params }) => {
        const g = guard(request, 'blocklists', 'write');
        if (g) return g;
        try {
          deleteEntry(db, action, String(params.id));
          return new HttpResponse(null, { status: 204 });
        } catch (e) {
          return writeFailed(e);
        }
      }),
    ];
  });
  return [
    http.get(`${B}/domains`, ({ request }) => {
      const g = guard(request, 'blocklists', 'read');
      if (g) return g;
      const a = new URL(request.url).searchParams.get('action');
      return entriesPage(request, a === 'allow' || a === 'deny' ? a : undefined);
    }),
    // POST /allowlist:batch (a colon inside the segment, so matched by hand).
    http.post(`${B}/:batch`, async ({ request, params }) => {
      const m = /^(allow|deny)list:batch$/.exec(String(params.batch));
      if (!m) return undefined;
      const action = m[1] as DomainAction;
      const g = guard(request, 'blocklists', 'write');
      if (g) return g;
      try {
        const body = (await request.json()) as DomainBatch;
        const r = batchEntries(db, action, body.ids ?? [], body.op);
        audit('blocklist.update', { kind: 'BlockList', namespace: 'default', name: `ui-${action}` });
        return ok({ ...r });
      } catch (e) {
        return writeFailed(e);
      }
    }),
    ...routes,
  ];
}

// ---------------------------------------------------------------------------
// Unblock requests (internal/api/unblock.go, internal/unblock/decide.go)

function findUnblock(ns: unknown, name: unknown): UnblockRequest | undefined {
  return db.unblock.find((r) => r.namespace === String(ns) && r.name === String(name));
}

/** r's history with its own decision appended (pushHistory). */
function pushedHistory(r: UnblockRequest): UnblockRequest['history'] {
  if (r.state === 'Pending') return r.history;
  return [
    ...r.history,
    { state: r.state, ...(r.decidedBy ? { decidedBy: r.decidedBy } : {}), ...(r.decidedAt ? { decidedAt: r.decidedAt } : {}), ...(r.note ? { note: r.note } : {}), ...(r.allow ? { allow: r.allow } : {}) },
  ];
}

/** An approval whose entry is in force (unblock.Active). */
function unblockActive(r: UnblockRequest): boolean {
  return r.state === 'Approved' && !!r.allow && !r.allow.removed && (!r.allow.expiresAt || new Date(r.allow.expiresAt).getTime() > Date.now());
}

function goDurationMs(v: string): number {
  const re = /(\d+(?:\.\d+)?)(ms|s|m|h)/gy;
  let total = 0;
  let consumed = 0;
  let m: RegExpExecArray | null;
  while ((m = re.exec(v)) !== null) {
    const n = Number(m[1]);
    total += n * (m[2] === 'h' ? 3_600_000 : m[2] === 'm' ? 60_000 : m[2] === 's' ? 1000 : 1);
    consumed = re.lastIndex;
  }
  return consumed === v.length && consumed > 0 ? total : Number.NaN;
}

function stamp(ms = Date.now()): string {
  return new Date(ms).toISOString().replace(/\.\d{3}Z$/, 'Z');
}

/** Ends r's approval in force: its entry leaves the list when the approval added it. */
function endApproval(r: UnblockRequest): void {
  if (!unblockActive(r) || !r.allow) return;
  const allow = r.allow;
  if (allow.added) releaseEntry(db, allow.list, allow.entry);
  r.allow = { ...allow, removed: stamp() };
}

function unblockHandlers() {
  const missing = (params: Record<string, unknown>) => fail(404, 'not_found', `unblock request ${String(params.ns)}/${String(params.name)} not found`);
  return [
    http.get(`${B}/unblockrequests`, ({ request }) => {
      const g = guard(request, 'unblockrequests', 'read') ?? needsLicense('requests', 'unblock requests');
      if (g) return g;
      const url = new URL(request.url);
      const state = url.searchParams.get('state');
      const limit = Math.min(500, Number(url.searchParams.get('limit') || '100')) || 100;
      const start = Number(url.searchParams.get('continue') || '0');
      const all = [...db.unblock].sort((a, b) => b.requestedAt.localeCompare(a.requestedAt));
      const counts = { pending: 0, approved: 0, denied: 0, expired: 0 };
      for (const r of all) counts[r.state.toLowerCase() as keyof typeof counts]++;
      const matching = state ? all.filter((r) => r.state === state) : all;
      const items = matching.slice(start, start + limit);
      const next = start + limit < matching.length ? String(start + limit) : undefined;
      return ok({ items, counts, ...(next ? { continue: next } : {}) });
    }),
    http.get(`${B}/unblockrequests/:ns/:name`, ({ request, params }) => {
      const g = guard(request, 'unblockrequests', 'read');
      if (g) return g;
      const r = findUnblock(params.ns, params.name);
      return r ? ok({ ...r }) : missing(params);
    }),
    http.post(`${B}/unblockrequests/:ns/:name/approve`, async ({ request, params }) => {
      const g = guard(request, 'unblockrequests', 'decide');
      if (g) return g;
      const r = findUnblock(params.ns, params.name);
      if (!r) return missing(params);
      if (unblockActive(r)) return fail(409, 'conflict', `${r.namespace}/${r.name} is already approved; deny it to revoke the approval`);
      const body = (await request.json()) as UnblockApproval;
      const scope = body.scope ?? 'everyone';
      const match = body.match ?? 'exact';
      let ms = 0;
      if (body.duration) {
        ms = goDurationMs(body.duration);
        if (Number.isNaN(ms)) return fail(400, 'invalid', `duration: ${body.duration} is not a Go duration`, undefined, undefined, ['body.duration']);
      }
      let list = 'ui-allow';
      let group = '';
      let wanted: string[];
      if (scope === 'group') {
        group = body.group || r.groups[0] || '';
        if (!group) return fail(422, 'invalid', 'the request names no client group; approve it for everyone');
        if (!group.includes('/')) group = `${r.namespace}/${group}`;
        const gname = group.split('/')[1] ?? '';
        list = `ui-allow-${gname}`;
        wanted = db.policies.filter((p) => (p.spec.clientGroupRefs ?? []).includes(gname)).map((p) => p.metadata.name);
        if (!wanted.length) return fail(422, 'invalid', `no policy covers client group ${group}; approve it for everyone`);
      } else {
        wanted = [...new Set(['default', ...r.policies.filter((p) => p.startsWith(`${r.namespace}/`)).map((p) => p.split('/')[1] ?? '')])];
      }
      const entry = match === 'subdomains' ? `||${r.domain}^` : r.domain;
      // The backend writes a structured entry (origin unblock, the request's name).
      const added = approveEntry(db, r.namespace, group || undefined, r.domain, match === 'subdomains', `${r.namespace}/${r.name}`, actorOf());
      const attached: string[] = [];
      const skipped: string[] = [];
      for (const name of wanted) {
        const p = db.policies.find((x) => x.metadata.name === name);
        if (!p) continue;
        const owner = p.metadata.annotations?.[MANAGED] ?? '';
        if (owner.startsWith('file:') || owner.startsWith('kube')) {
          skipped.push(`${r.namespace}/${name}`);
          continue;
        }
        if (!(p.spec.allowListRefs ?? []).includes(list)) p.spec.allowListRefs = [...(p.spec.allowListRefs ?? []), list];
        attached.push(`${r.namespace}/${name}`);
      }
      const allow: UnblockAllow = {
        scope,
        match,
        ...(group ? { group } : {}),
        list: `${r.namespace}/${list}`,
        entry,
        added,
        policies: attached,
        ...(ms > 0 ? { expiresAt: stamp(Date.now() + ms) } : {}),
      };
      const history = pushedHistory(r);
      Object.assign(r, { state: 'Approved', decidedBy: db.session?.user.name ?? 'admin', decidedAt: stamp(), allow, history, resourceVersion: String(++db.rv) });
      delete r.expiresAt;
      if (body.note) r.note = body.note;
      else delete r.note;
      return ok({ request: { ...r }, attached, skipped, flushed: 1 });
    }),
    http.post(`${B}/unblockrequests/:ns/:name/deny`, async ({ request, params }) => {
      const g = guard(request, 'unblockrequests', 'decide');
      if (g) return g;
      const r = findUnblock(params.ns, params.name);
      if (!r) return missing(params);
      const body = (await request.json()) as UnblockDenial;
      endApproval(r);
      const history = pushedHistory(r);
      Object.assign(r, { state: 'Denied', decidedBy: db.session?.user.name ?? 'admin', decidedAt: stamp(), history, resourceVersion: String(++db.rv) });
      delete r.allow;
      delete r.expiresAt;
      if (body.note) r.note = body.note;
      else delete r.note;
      return ok({ request: { ...r }, attached: [], skipped: [], flushed: 0 });
    }),
    http.delete(`${B}/unblockrequests/:ns/:name`, ({ request, params }) => {
      const g = guard(request, 'unblockrequests', 'delete');
      if (g) return g;
      const r = findUnblock(params.ns, params.name);
      if (!r) return missing(params);
      if (unblockActive(r) && r.allow?.expiresAt) {
        return fail(409, 'conflict', `${r.namespace}/${r.name} is a time-limited approval in force; deny it first (which removes its allow entry)`);
      }
      db.unblock = db.unblock.filter((x) => x !== r);
      return new HttpResponse(null, { status: 204 });
    }),
  ];
}
