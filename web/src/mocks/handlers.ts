// MSW request handlers implementing the /api/v1 contract over the fixture
// database. They are deliberately close to the real server: the error
// envelope, 401/403 with missing{resource,verb,scope}, 409 managed_by with
// the owner, 412 on If-Match mismatch, keyset cursors and took_ms.
import { http, HttpResponse, sse, type JsonBodyType } from 'msw';
import type {
  AnyObject,
  ApplyResult,
  BlockList,
  ClientGroup,
  DNSRecordSet,
  DNSZone,
  DnsQueryResult,
  Endpoint,
  Me,
  ObjectKind,
  Policy,
  QueryRow,
  StatusClass,
  TopDim,
  TopItem,
  ZoneRecord,
  ZoneSummary,
} from '../api/types';
import { createDB, CLIENTS, makeQueryRow, type MockDB } from './fixtures';
import { rng } from './rand';

let db: MockDB = createDB();

/** Resets the database to the fixtures (tests call it in beforeEach). */
export function resetDB(now?: number): MockDB {
  db = createDB(now);
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

function fail(status: number, key: string, message: string, hint?: unknown, headers?: Record<string, string>) {
  return HttpResponse.json(
    { error: { key, message, ...(hint !== undefined ? { hint } : {}) }, took_ms: 0.4 } as JsonBodyType,
    { status, ...(headers ? { headers } : {}) },
  );
}

const ALL_RESOURCES = [
  'zones', 'records', 'blocklists', 'policies', 'clientgroups', 'clients', 'querylog', 'stats', 'cache', 'blocking',
  'settings', 'node', 'users', 'groups', 'serviceaccounts', 'tokens', 'identityproviders', 'rolebindings', 'integrations',
  'cluster', 'edge', 'audit',
];

function permissions(roles: string[]): Record<string, string[]> {
  const out: Record<string, string[]> = {};
  for (const r of ALL_RESOURCES) {
    out[r] = roles.includes('admin') ? ['get', 'list', 'create', 'update', 'delete', 'patch', 'action'] : ['get', 'list'];
  }
  if (!roles.includes('admin')) out.tokens = ['get', 'list', 'create', 'delete'];
  return out;
}

function me(): Me {
  const u = db.session?.user;
  if (!u) throw new Error('no session');
  return {
    principal: { kind: 'User', id: `local:${u.username}`, name: u.name, groups: u.roles.includes('admin') ? ['kudurru-admins'] : [] },
    roles: u.roles,
    csrf: CSRF,
    permissions: permissions(u.roles),
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
  if (existing) list.splice(list.indexOf(existing), 1, next);
  else list.push(next);
  db.audit.unshift({ id: `a${db.rv}`, ts: new Date().toISOString(), actor: db.session?.user.name ?? 'anonymous', action: existing ? 'update' : 'create', kind: KIND_NAMES[kind], ...(ns ? { namespace: ns } : {}), name: obj.metadata.name, outcome: 'success' });
  return { status: existing ? 200 : 201, body: next, result: existing ? 'updated' : 'created' };
}

function deleteObject(kind: ObjectKind, name: string, namespace?: string): { status: number; body?: unknown } {
  const obj = find(kind, name, namespace);
  if (!obj) return { status: 404, body: { error: { key: 'not_found', message: `${KIND_NAMES[kind]} ${name} not found` } } };
  const owner = obj.metadata.annotations?.[MANAGED] ?? '';
  if (owner && owner !== 'api' && owner !== 'bootstrap') {
    return { status: 409, body: { error: { key: 'managed_by', message: `${KIND_NAMES[kind]} ${name} is managed by ${owner}`, hint: owner } } };
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
  return {
    name: z.metadata.name,
    zoneName: z.spec.zoneName,
    type: z.spec.type,
    serial: z.status?.serial ?? 0,
    recordCount: z.status?.recordCount ?? 0,
    ready: (z.status?.conditions ?? []).some((c) => c.type === 'Ready' && c.status === 'True'),
    managedBy: z.metadata.annotations?.[MANAGED] ?? '',
    transfer: {
      zoneTransfer: z.spec.policy?.zoneTransfer ?? 'deny',
      notify: z.spec.policy?.notify ?? 'none',
      ...(z.spec.type === 'primary'
        ? {
            lastNotify: { at: new Date(db.now - 1_800_000).toISOString(), targets: z.spec.policy?.notifyTargets ?? z.spec.nameservers ?? [], ok: true },
            ...(z.spec.policy?.zoneTransfer && z.spec.policy.zoneTransfer !== 'deny'
              ? { lastTransfer: { at: new Date(db.now - 1_700_000).toISOString(), peer: '192.168.1.2', kind: 'IXFR' as const, ok: true } }
              : {}),
          }
        : {}),
    },
  };
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
  for (const f of ['qtype', 'upstream', 'list', 'zone'] as const) {
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
        key = r.upstream;
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

function digAnswer(name: string, type: string, client?: string): DnsQueryResult {
  const n = name.replace(/\.$/, '').toLowerCase();
  const blockedList = db.lists.find((l) => l.spec.mode !== 'allow' && l.spec.sources.some((s) => s.inline?.includes(n)));
  const knownBlocked = [...db.queries].find((q) => q.domain === n && q.status_class === 'blocked');
  if (blockedList || knownBlocked) {
    return {
      rcode: 'NOERROR',
      status: knownBlocked?.status ?? 'blocked_exact',
      status_class: 'blocked',
      answer: [{ name: `${n}.`, type, ttl: 60, class: 'IN', data: type === 'AAAA' ? '::' : '0.0.0.0' }],
      authority: [],
      additional: [],
      list: knownBlocked?.list ?? `default/${blockedList?.metadata.name ?? ''}`,
      rule: knownBlocked?.rule ?? n,
      took_ms: 0.12,
      node: 'kudurru-0',
      ...(client ? {} : {}),
    };
  }
  const local = recordsOf('home-arpa').concat(recordsOf('example-internal')).filter((r) => r.dnsName.toLowerCase() === n);
  if (local.length) {
    const rr = local.filter((r) => r.recordType === type || r.recordType === 'CNAME');
    return {
      rcode: rr.length ? 'NOERROR' : 'NOERROR',
      status: 'authoritative',
      status_class: 'permitted',
      answer: rr.flatMap((r) => r.targets.map((t) => ({ name: `${r.dnsName}.`, type: r.recordType, ttl: r.recordTTL ?? 300, class: 'IN', data: t }))),
      authority: rr.length ? [] : [{ name: n.endsWith('home.arpa') ? 'home.arpa.' : 'example.internal.', type: 'SOA', ttl: 300, class: 'IN', data: 'ns1. hostmaster. 1 3600 600 604800 300' }],
      additional: [],
      zone: n.endsWith('home.arpa') ? 'home.arpa.' : 'example.internal.',
      took_ms: 0.08,
      node: 'kudurru-0',
    };
  }
  if (n.endsWith('.invalid') || n.startsWith('nx')) {
    return { rcode: 'NXDOMAIN', status: 'forwarded', status_class: 'permitted', answer: [], authority: [{ name: '.', type: 'SOA', ttl: 86400, class: 'IN', data: 'a.root-servers.net. nstld.verisign-grs.com. 2026101000 1800 900 604800 86400' }], additional: [], upstream: 'tls://1.1.1.1:853', took_ms: 14.2, node: 'kudurru-0' };
  }
  const cname = n === 'www.youtube.com' ? ['youtube-ui.l.google.com.'] : undefined;
  return {
    rcode: 'NOERROR',
    status: 'forwarded',
    status_class: 'permitted',
    answer: [
      ...(cname ? [{ name: `${n}.`, type: 'CNAME', ttl: 300, class: 'IN', data: cname[0] as string }] : []),
      { name: cname ? (cname[0] as string) : `${n}.`, type, ttl: 300, class: 'IN', data: type === 'AAAA' ? '2606:4700::6810:84e5' : type === 'TXT' ? '"v=spf1 -all"' : '104.16.132.229' },
    ],
    authority: [],
    additional: [],
    upstream: 'tls://1.1.1.1:853',
    ...(cname ? { cname_chain: [`${n}.`, ...cname] } : {}),
    took_ms: 18.6,
    node: 'kudurru-0',
  };
}

function appendInline(listName: string, mode: 'allow' | 'block', namespace: string, domains: string[], regex: boolean) {
  let list = db.lists.find((l) => l.metadata.name === listName && (l.metadata.namespace ?? 'default') === namespace);
  if (!list) {
    db.rv += 1;
    list = {
      apiVersion: 'kudurru.io/v1alpha1',
      kind: 'BlockList',
      metadata: { name: listName, namespace, resourceVersion: String(db.rv), annotations: { [MANAGED]: 'api' } },
      spec: { mode, sources: [{ name: 'inline', inline: [], format: regex ? 'regex' : 'domains' }] },
      status: { entries: 0 },
    };
    db.lists.push(list);
  }
  const src = list.spec.sources[0];
  if (!src) throw new Error('list without a source');
  src.inline = src.inline ?? [];
  const success: { item: string }[] = [];
  const errors: { item: string; error: string }[] = [];
  for (const d of domains) {
    const v = d.trim().toLowerCase();
    if (!v) continue;
    if (regex) {
      try {
        new RegExp(v.replace(/^\/|\/$/g, ''));
        success.push({ item: v });
        src.inline.push(v.startsWith('/') ? v : `/${v}/`);
      } catch {
        errors.push({ item: d, error: 'not a valid regular expression' });
      }
    } else if (!/^(\*\.)?([a-z0-9_]([a-z0-9_-]*[a-z0-9_])?\.)+[a-z0-9-]{2,}$/.test(v)) {
      errors.push({ item: d, error: 'not a domain name' });
    } else if (src.inline.includes(v)) {
      errors.push({ item: d, error: `already on ${namespace}/${listName}` });
    } else {
      success.push({ item: v });
      src.inline.push(v);
    }
  }
  list.status = { ...(list.status ?? {}), entries: src.inline.length, ruleCount: src.inline.length };
  return { processed: { success, errors } };
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
      const g = guard(request, RESOURCE_OF[t.kind], 'get');
      if (g) return g;
      const obj = find(t.kind, t.name, t.ns);
      return obj ? ok(obj as unknown as Record<string, unknown>) : fail(404, 'not_found', `${KIND_NAMES[t.kind]} ${t.name} not found`);
    }),
    http.put(pattern, async ({ request, params }) => {
      const t = target(params);
      if (t instanceof Response) return t;
      const g = guard(request, RESOURCE_OF[t.kind], 'update');
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
  // auth ------------------------------------------------------------------
  http.get(`${B}/auth/status`, () =>
    ok({ localLogin: 'enabled', providers: [{ id: 'dex', displayName: 'Dex (corporate SSO)' }, { id: 'github', displayName: 'GitHub' }], hasDefaultCredentials: db.users.some((u) => u.username === 'admin' && u.password === 'admin') }),
  ),
  http.post(`${B}/auth/login`, async ({ request }) => {
    const body = (await request.json()) as { username?: string; password?: string };
    const user = db.users.find((u) => u.username === body.username && u.password === body.password);
    if (!user) return fail(401, 'unauthenticated', 'Wrong username or password');
    db.session = { user };
    return ok({ ...me() });
  }),
  http.post(`${B}/auth/logout`, () => {
    db.session = null;
    return new HttpResponse(null, { status: 204 });
  }),
  http.get(`${B}/auth/me`, () => (db.session ? ok({ ...me() }) : fail(401, 'unauthenticated', 'No session'))),
  http.get(`${B}/auth/oidc/:idp/start`, ({ params }) => fail(501, 'not_implemented', `The mock cannot redirect to ${String(params.idp)}`)),
  http.get(`${B}/auth/tokens`, ({ request }) => guard(request, 'tokens', 'list') ?? ok({ items: db.tokens })),
  http.post(`${B}/auth/tokens`, async ({ request }) => {
    const g = guard(request, 'tokens', 'create');
    if (g) return g;
    const body = (await request.json()) as { name?: string; expiresIn?: string };
    if (!body.name) return fail(400, 'invalid', 'name: required');
    const id = Math.random().toString(36).slice(2, 10);
    const t = { id, name: body.name, createdAt: new Date().toISOString(), ...(body.expiresIn ? { expiresAt: new Date(Date.now() + 30 * 86_400_000).toISOString() } : {}) };
    db.tokens.push(t);
    return ok({ ...t, token: `kdr_${id}_${Math.random().toString(36).slice(2)}${Math.random().toString(36).slice(2)}` }, { status: 201 });
  }),
  http.delete(`${B}/auth/tokens/:id`, ({ request, params }) => {
    const g = guard(request, 'tokens', 'delete');
    if (g) return g;
    db.tokens = db.tokens.filter((t) => t.id !== params.id);
    return new HttpResponse(null, { status: 204 });
  }),

  // generic objects --------------------------------------------------------
  http.get(`${B}/objects/:kind`, ({ request, params }) => {
    const kind = String(params.kind);
    if (!isKind(kind)) return fail(404, 'not_found', `unknown kind ${kind}`);
    const g = guard(request, RESOURCE_OF[kind], 'list');
    if (g) return g;
    const url = new URL(request.url);
    const ns = url.searchParams.get('namespace');
    const items = collection(kind).filter((o) => !ns || o.metadata.namespace === ns);
    return listEnvelope(items, url);
  }),
  ...objectRoutes(`${B}/objects/:kind/:a`),
  ...objectRoutes(`${B}/objects/:kind/:a/:b`),
  http.post(`${B}/apply`, async ({ request }) => {
    const g = guard(request, 'zones', 'update');
    if (g) return g;
    const text = await request.text();
    let docs: AnyObject[];
    try {
      docs = JSON.parse(text) as AnyObject[];
      if (!Array.isArray(docs)) docs = [docs as unknown as AnyObject];
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
    if (!raw.endsWith(':batchDelete')) return fail(404, 'not_found', `no route POST /${raw}`);
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
  http.get(`${B}/zones`, ({ request }) => guard(request, 'zones', 'list') ?? ok({ items: db.zones.map(zoneSummary) })),
  http.get(`${B}/zones/:zone`, ({ request, params }) => {
    const g = guard(request, 'zones', 'get');
    if (g) return g;
    const z = db.zones.find((x) => x.metadata.name === params.zone);
    return z ? ok({ ...zoneSummary(z) }) : fail(404, 'not_found', `zone ${String(params.zone)} not found`);
  }),
  http.get(`${B}/zones/:zone/records`, ({ request, params }) => {
    const g = guard(request, 'records', 'list');
    if (g) return g;
    if (!db.zones.some((z) => z.metadata.name === params.zone)) return fail(404, 'not_found', `zone ${String(params.zone)} not found`);
    return ok({ items: recordsOf(String(params.zone)) });
  }),
  http.post(`${B}/zones/:zone/records`, async ({ request, params }) => {
    const g = guard(request, 'records', 'create');
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
      rs = { apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSRecordSet', metadata: { name: rsName, namespace: 'default', resourceVersion: String(db.rv), annotations: { [MANAGED]: 'api' } }, spec: { zoneRef: { name: zone }, endpoints: [] } } as DNSRecordSet;
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
    const rsName = new URL(request.url).searchParams.get('recordSet');
    let found = false;
    for (const rs of db.recordSets) {
      if (rs.spec.zoneRef.name !== zone || (rsName && rs.metadata.name !== rsName)) continue;
      const before = rs.spec.endpoints.length;
      const keep = rs.spec.endpoints.filter((x) => !(x.dnsName === params.name && x.recordType === params.type));
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
    const g = guard(request, 'records', 'create');
    if (g) return g;
    const zone = String(params.zone);
    const z = db.zones.find((x) => x.metadata.name === zone);
    if (!z) return fail(404, 'not_found', `zone ${zone} not found`);
    const text = await request.text();
    const origin = z.spec.zoneName.replace(/\.$/, '');
    const skipped: { line: number; error: string }[] = [];
    const eps: Endpoint[] = [];
    text.split('\n').forEach((line, i) => {
      const l = line.replace(/;.*$/, '').trim();
      if (!l || l.startsWith('$')) return;
      const m = /^(\S+)\s+(?:(\d+)\s+)?(?:IN\s+)?([A-Z0-9]+)\s+(.+)$/.exec(l);
      if (!m) {
        skipped.push({ line: i + 1, error: 'cannot parse' });
        return;
      }
      const [, owner, ttl, type, data] = m as unknown as [string, string, string | undefined, string, string];
      if (type === 'SOA') return;
      const name = owner === '@' ? origin : owner.endsWith('.') ? owner.slice(0, -1) : `${owner}.${origin}`;
      eps.push({ dnsName: name, recordType: type, targets: [data.trim()], ...(ttl ? { recordTTL: Number(ttl) } : {}) });
    });
    const rsName = `import-${zone}`;
    db.recordSets = db.recordSets.filter((x) => x.metadata.name !== rsName);
    db.recordSets.push({ apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSRecordSet', metadata: { name: rsName, namespace: 'default', annotations: { [MANAGED]: 'api' } }, spec: { zoneRef: { name: zone }, endpoints: eps } });
    bumpZone(zone);
    return ok({ imported: eps.length, recordSet: rsName, skipped });
  }),
  http.get(`${B}/zones/:zone/export`, ({ request, params }) => {
    const g = guard(request, 'zones', 'get');
    if (g) return g;
    const z = db.zones.find((x) => x.metadata.name === params.zone);
    if (!z) return fail(404, 'not_found', `zone ${String(params.zone)} not found`);
    return new HttpResponse(exportZone(z), { headers: { 'Content-Type': 'text/dns; charset=utf-8', 'Content-Disposition': `attachment; filename="${z.metadata.name}.zone"` } });
  }),
  http.post(`${B}/zones/:zone/notify`, ({ request, params }) => {
    const g = guard(request, 'zones', 'action');
    if (g) return g;
    const z = db.zones.find((x) => x.metadata.name === params.zone);
    if (!z) return fail(404, 'not_found', `zone ${String(params.zone)} not found`);
    if (z.spec.type !== 'primary') return fail(422, 'unprocessable', 'NOTIFY is sent for primary zones only');
    return ok({ targets: z.spec.policy?.notifyTargets ?? z.spec.nameservers ?? [] });
  }),

  // lists and policy -------------------------------------------------------
  http.post(`${B}/blocklists/:ns/:name/refresh`, ({ request, params }) => {
    const g = guard(request, 'blocklists', 'action');
    if (g) return g;
    const l = db.lists.find((x) => x.metadata.name === params.name && x.metadata.namespace === params.ns);
    if (!l) return fail(404, 'not_found', `BlockList ${String(params.ns)}/${String(params.name)} not found`);
    if (l.status) l.status.lastFetched = new Date().toISOString();
    return new HttpResponse(null, { status: 202 });
  }),
  http.get(`${B}/blocklists/_search`, ({ request }) => {
    const g = guard(request, 'blocklists', 'get');
    if (g) return g;
    const domain = (new URL(request.url).searchParams.get('domain') ?? '').toLowerCase();
    const items = db.lists.flatMap((l) =>
      l.spec.sources.flatMap((s) =>
        (s.inline ?? [])
          .filter((rule) => rule === domain || (rule.startsWith('/') && new RegExp(rule.slice(1, -1)).test(domain)))
          .map((rule) => ({ list: l.metadata.name, namespace: l.metadata.namespace ?? 'default', rule, mode: l.spec.mode ?? 'block' })),
      ),
    );
    const fromLog = db.queries.find((q) => q.domain === domain && q.list);
    if (fromLog?.list && !items.some((i) => `${i.namespace}/${i.list}` === fromLog.list)) {
      const [ns, name] = fromLog.list.split('/') as [string, string];
      items.push({ list: name, namespace: ns, rule: fromLog.rule ?? domain, mode: 'block' });
    }
    return ok({ items });
  }),
  http.post(`${B}/domains/:action/:kind`, async ({ request, params }) => {
    const g = guard(request, 'blocklists', 'update');
    if (g) return g;
    const action = String(params.action);
    const kind = String(params.kind);
    if (!['allow', 'deny'].includes(action) || !['exact', 'regex'].includes(kind)) return fail(404, 'not_found', 'no such route');
    const body = (await request.json()) as { domains: string[]; namespace?: string; list?: string };
    const listName = body.list ?? (action === 'allow' ? 'ui-allow' : 'ui-deny');
    return ok(appendInline(listName, action === 'allow' ? 'allow' : 'block', body.namespace ?? 'default', body.domains ?? [], kind === 'regex'));
  }),
  http.get(`${B}/clients`, ({ request }) => {
    const g = guard(request, 'clients', 'list');
    if (g) return g;
    const items = CLIENTS.map((c) => {
      const rows = db.queries.filter((q) => q.client.ip === c.ip);
      return {
        ip: c.ip,
        name: c.name,
        groups: c.group === 'default' ? ['default'] : [c.group, 'default'],
        lastSeen: rows[0]?.ts,
        queries24h: rows.length * 20,
        blocked24h: rows.filter((q) => q.status_class === 'blocked').length * 20,
        ...(c.ip.startsWith('192.168.1.3') ? { mac: `aa:bb:cc:dd:ee:${c.ip.slice(-2)}` } : {}),
      };
    });
    return ok({ items });
  }),

  // runtime ---------------------------------------------------------------
  http.get(`${B}/dns/blocking`, ({ request }) => {
    const g = guard(request, 'blocking', 'get');
    if (g) return g;
    return ok(blockingState());
  }),
  http.post(`${B}/dns/blocking`, async ({ request }) => {
    const g = guard(request, 'blocking', 'update');
    if (g) return g;
    const body = (await request.json()) as { blocking: boolean; timer?: number; until?: string };
    if (typeof body.blocking !== 'boolean') return fail(400, 'invalid', 'blocking: must be a boolean');
    if (body.timer !== undefined && (!(body.timer > 0) || body.timer > 86_400 * 7)) return fail(400, 'invalid', 'timer: 1 s to 7 days');
    const until = body.blocking ? undefined : body.timer ? Date.now() + body.timer * 1000 : body.until ? new Date(body.until).getTime() : undefined;
    db.blocking = { blocking: body.blocking, ...(until ? { until } : {}) };
    return ok(blockingState());
  }),
  http.post(`${B}/cache/flush`, async ({ request }) => {
    const g = guard(request, 'cache', 'action');
    if (g) return g;
    const body = (await request.json().catch(() => ({}))) as { name?: string };
    return ok({ flushed: body.name ? 1 : 48213 });
  }),
  http.post(`${B}/dns/query`, async ({ request }) => {
    const g = guard(request, 'querylog', 'get');
    if (g) return g;
    const body = (await request.json()) as { name?: string; type?: string; client?: string };
    if (!body.name) return fail(400, 'invalid', 'name: required');
    return ok({ ...digAnswer(body.name, (body.type ?? 'A').toUpperCase(), body.client) });
  }),

  // stats -----------------------------------------------------------------
  http.get(`${B}/stats/summary`, ({ request }) => {
    const g = guard(request, 'stats', 'get');
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
    const g = guard(request, 'stats', 'get');
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
    const g = guard(request, 'stats', 'get');
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
    const g = guard(request, 'stats', 'get');
    if (g) return g;
    const url = new URL(request.url);
    const top = topFrom('upstreams', false, 10, seriesTotals().total / db.queries.length);
    return ok({
      ...scopeInfo(url),
      from: new Date(db.now - 86_400_000).toISOString(),
      until: new Date(db.now).toISOString(),
      upstreams: top.map((x, i) => ({ addr: x.key, total: x.total, errors: 3 + i, timeouts: i, percent: x.percent, avg_ms: 12 + i * 6, p95_ms: 40 + i * 15 })),
      approximate: false,
    });
  }),
  http.get(`${B}/stats/clients/timeseries`, ({ request }) => {
    const g = guard(request, 'stats', 'get');
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
      clients: top.map((x) => ({ key: x.key, ...(x.name ? { name: x.name } : {}) })),
      points: db.series.map((p, i) => ({ ts: p.ts, counts: [...weights.map((w, j) => Math.round(p.total * w * 0.85 * (0.85 + ((i * (j + 3)) % 7) / 20))), Math.round(p.total * rest)] })),
      approximate: false,
    });
  }),
  http.get(`${B}/stats/recent_blocked`, ({ request }) => {
    const g = guard(request, 'stats', 'get');
    if (g) return g;
    const url = new URL(request.url);
    const n = Number(url.searchParams.get('n') ?? '10');
    const items = db.queries
      .filter((q) => q.status_class === 'blocked')
      .slice(0, n)
      .map((q) => ({ id: q.id, ts: q.ts, domain: q.domain, client: q.client.ip, ...(q.client.name ? { client_name: q.client.name } : {}), ...(q.list ? { list: q.list } : {}), status: q.status }));
    return ok({ ...scopeInfo(url), items });
  }),

  // query log ---------------------------------------------------------------
  http.get(`${B}/queries`, ({ request }) => {
    const g = guard(request, 'querylog', 'get');
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
    const g = guard(request, 'querylog', 'get');
    if (g) return g;
    const url = new URL(request.url);
    const field = url.searchParams.get('field') ?? 'domain';
    const prefix = (url.searchParams.get('prefix') ?? '').toLowerCase();
    const values = new Set<string>();
    for (const q of db.queries) {
      const v = field === 'client' ? q.client.name ?? q.client.ip : field === 'upstream' ? q.upstream : field === 'list' ? q.list : field === 'zone' ? q.zone : q.domain;
      if (v && v.toLowerCase().includes(prefix)) values.add(v);
      if (values.size >= 10) break;
    }
    return ok({ field, values: [...values] });
  }),
  sse(`${B}/queries/stream`, ({ client, request }) => {
    const last = Number(request.headers.get('Last-Event-ID') ?? new URL(request.url).searchParams.get('lastEventId') ?? '0');
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
  http.post(`${B}/queries/:id/actions`, async ({ request, params }) => {
    const g = guard(request, 'blocklists', 'update');
    if (g) return g;
    const row = db.queries.find((q) => q.id === Number(params.id));
    if (!row) return fail(404, 'not_found', `query ${String(params.id)} is not in the log`);
    const body = (await request.json()) as { action: 'allow' | 'deny' };
    if (body.action !== 'allow' && body.action !== 'deny') return fail(400, 'invalid', 'action: allow or deny');
    return ok(appendInline(body.action === 'allow' ? 'ui-allow' : 'ui-deny', body.action === 'allow' ? 'allow' : 'block', 'default', [row.domain], false));
  }),
  http.delete(`${B}/querylog`, ({ request }) => {
    const g = guard(request, 'querylog', 'delete');
    if (g) return g;
    const before = new URL(request.url).searchParams.get('before');
    db.queries = before ? db.queries.filter((q) => q.ts >= new Date(before).toISOString()) : [];
    return new HttpResponse(null, { status: 204 });
  }),
  http.get(`${B}/querylog/status`, ({ request }) => {
    const g = guard(request, 'querylog', 'get');
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
  http.get(`${B}/settings/_schema`, ({ request }) => guard(request, 'settings', 'get') ?? ok({ items: db.schema })),
  http.get(`${B}/settings`, ({ request }) => guard(request, 'settings', 'get') ?? ok(db.settings)),
  http.patch(`${B}/settings`, async ({ request }) => {
    const g = guard(request, 'settings', 'update');
    if (g) return g;
    const patch = (await request.json()) as Record<string, unknown>;
    const bad = leafPaths(patch).find((p) => db.schema.find((k) => k.path === p)?.hostOnly || db.schema.find((k) => k.path === p)?.scope === 'node');
    if (bad) return fail(400, 'host_only', `${bad} can only be set in kudurru.yaml or the environment on the host`, bad);
    db.settings = mergePatch(db.settings, patch);
    return ok(db.settings);
  }),
  http.get(`${B}/node/config`, ({ request }) => guard(request, 'node', 'get') ?? ok(db.nodeConfig)),
  http.patch(`${B}/node/config`, async ({ request }) => {
    const g = guard(request, 'node', 'update');
    if (g) return g;
    const patch = (await request.json()) as Record<string, unknown>;
    const bad = leafPaths(patch).find((p) => db.schema.find((k) => k.path === p)?.hostOnly);
    if (bad) return fail(400, 'host_only', `${bad} can only be set in kudurru.yaml or the environment on the host`, bad);
    db.nodeConfig = mergePatch(db.nodeConfig, patch);
    return ok(db.nodeConfig);
  }),

  // nodes and cluster -------------------------------------------------------
  http.get(`${B}/nodes`, ({ request }) => guard(request, 'cluster', 'get') ?? ok({ items: db.nodes })),
  http.get(`${B}/nodes/:name/health`, ({ request, params }) => {
    const g = guard(request, 'cluster', 'get');
    if (g) return g;
    const n = db.nodes.find((x) => x.name === params.name);
    if (!n) return fail(404, 'not_found', `node ${String(params.name)} not found`);
    return ok({
      name: n.name,
      checks: [
        { name: 'dns', ok: true },
        { name: 'zonecache', ok: true, message: `synced, snapshot ${n.snapshotAge ?? '?'} old` },
        { name: 'listresolver', ok: true },
        { name: 'store', ok: true, message: n.storeDriver ?? 'sqlite' },
        { name: 'querylog', ok: n.name === 'kudurru-0', ...(n.name === 'kudurru-0' ? {} : { message: 'WAL 812 MiB, checkpoint lagging' }) },
      ],
    });
  }),
  http.get(`${B}/cluster`, ({ request }) => guard(request, 'cluster', 'get') ?? ok({ mode: 'ha-static', leader: 'kudurru-0', nodes: db.nodes.map((n) => n.name), epoch: 7, readOnly: false })),
  http.get(`${B}/edge`, ({ request }) =>
    guard(request, 'edge', 'get') ??
    ok({ backends: [{ name: 'kudurru-0', address: '192.168.1.2:53', up: true, qps: 14.2 }, { name: 'kudurru-1', address: '192.168.1.3:53', up: true, qps: 5.9 }], vip: [{ address: '192.168.1.53', owner: 'kudurru-0' }] }),
  ),
  http.get(`${B}/info/messages`, ({ request }) => guard(request, 'cluster', 'get') ?? ok({ items: db.messages })),
  http.get(`${B}/version`, () => ok({ version: 'v0.1.0-rc.1', commit: '3e52e5a', date: '2026-10-10T18:00:00Z', go: 'go1.27.2' })),

  // iam and audit -------------------------------------------------------------
  http.get(`${B}/iam/:what`, ({ request, params }) => {
    const what = String(params.what);
    const g = guard(request, what === 'rolebindings' ? 'rolebindings' : what === 'identityproviders' ? 'identityproviders' : what === 'serviceaccounts' ? 'serviceaccounts' : 'users', 'list');
    if (g) return g;
    const items: Record<string, unknown[]> = {
      users: db.users.map((u) => ({ name: u.username, displayName: u.name, local: true, roles: u.roles, disabled: false })),
      serviceaccounts: [{ name: 'grafana', namespace: 'default', tokens: 1 }, { name: 'external-dns', namespace: 'kudurru', tokens: 0 }],
      rolebindings: [
        { name: 'admins', role: 'admin', subjects: [{ kind: 'Group', name: 'kudurru-admins' }, { kind: 'User', name: 'local:admin' }] },
        { name: 'viewers', role: 'viewer', subjects: [{ kind: 'Group', name: 'everyone' }] },
        { name: 'external-dns', role: 'integration:external-dns', subjects: [{ kind: 'ServiceAccount', name: 'kudurru/external-dns' }] },
      ],
      identityproviders: [{ name: 'dex', issuer: 'https://dex.example.com', clientID: 'kudurru' }, { name: 'github', issuer: 'https://token.actions.githubusercontent.com', clientID: 'kudurru-gh' }],
    };
    return items[what] ? ok({ items: items[what] }) : fail(404, 'not_found', `no IAM collection ${what}`);
  }),
  http.get(`${B}/audit`, ({ request }) => {
    const g = guard(request, 'audit', 'list');
    if (g) return g;
    const url = new URL(request.url);
    const kind = url.searchParams.get('kind');
    const actor = url.searchParams.get('actor');
    const name = url.searchParams.get('name');
    const items = db.audit.filter((a) => (!kind || a.kind === kind) && (!actor || a.actor.includes(actor)) && (!name || a.name.includes(name)));
    return ok({ items });
  }),
];

function blockingState() {
  if (db.blocking.until && db.blocking.until <= Date.now()) db.blocking = { blocking: true };
  const until = db.blocking.until;
  return { blocking: db.blocking.blocking, ...(until ? { until: new Date(until).toISOString(), timer: Math.max(0, Math.round((until - Date.now()) / 1000)) } : {}) };
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
