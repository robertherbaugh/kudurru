// The Allowlist / Denylist and the per-source entries browser of the mock
// server (the allowlist wire contract): logical entries collected from the
// structured `entries` source "ui" of the ui-allow / ui-deny lists (and
// their ui-allow-<group> / ui-deny-<group> copies), created, patched,
// deleted and batched there, with the lists created on demand and attached
// to the policies the API manages; and the entries of any source (inline
// lines, the cached body of a url or configMap source, structured entries)
// with exclude-based switches.
import type {
  BlockList,
  DomainAction,
  DomainEntriesCreate,
  DomainEntry,
  DomainEntryPatch,
  DomainEntryType,
  DomainListInfo,
  DomainUnattached,
  ListEntry,
  ListSource,
  ListSourceEntry,
  Policy,
  SourceEntriesKind,
} from '../api/types';
import { normaliseValue, valueError } from '../lib/domains';
import { sourceName } from '../lib/lists';
import type { MockDB } from './fixtures';

const MANAGED = 'kudurru.io/managed-by';
const ENABLED = 'kudurru.io/enabled';
/** Mock-only: the client group of a ui-allow-<group> / ui-deny-<group> list. */
export const GROUP_ANNOTATION = 'kudurru.io/client-group';
const UI_SOURCE = 'ui';

const nsOf = (o: { metadata: { namespace?: string } }) => o.metadata.namespace ?? 'default';
const refOf = (o: { metadata: { name: string; namespace?: string } }) => `${nsOf(o)}/${o.metadata.name}`;

function apiOwned(owner: string | undefined): boolean {
  return !owner || owner === 'api' || owner === 'bootstrap';
}

/** "*.x" for wildcards, "/re/" for expressions, the name otherwise (the compiled rule). */
export function renderRule(type: DomainEntryType, value: string): string {
  return type === 'regex' ? `/${value}/` : value;
}

export function entryId(ns: string, action: DomainAction, type: DomainEntryType, value: string): string {
  return btoa(unescape(encodeURIComponent(`${ns}|${action}|${type}|${value}`)))
    .replace(/\+/g, '-')
    .replace(/\//g, '_')
    .replace(/=+$/, '');
}

export function parseId(id: string): { ns: string; action: DomainAction; type: DomainEntryType; value: string } | null {
  try {
    const raw = decodeURIComponent(escape(atob(id.replace(/-/g, '+').replace(/_/g, '/'))));
    const [ns, action, type, ...rest] = raw.split('|');
    if (!ns || (action !== 'allow' && action !== 'deny') || (type !== 'exact' && type !== 'wildcard' && type !== 'regex')) return null;
    return { ns, action, type, value: rest.join('|') };
  } catch {
    return null;
  }
}

/** The action and group ("ns/name") of a ui-* list, or null for any other list. */
export function uiListInfo(l: BlockList): { action: DomainAction; group?: string } | null {
  const m = /^ui-(allow|deny)(?:-(.+))?$/.exec(l.metadata.name);
  if (!m) return null;
  const action = m[1] as DomainAction;
  if (!m[2]) return { action };
  return { action, group: l.metadata.annotations?.[GROUP_ANNOTATION] ?? `${nsOf(l)}/${m[2]}` };
}

function uiSource(l: BlockList): ListSource | undefined {
  return l.spec.sources.find((s) => s.entries) ?? undefined;
}

function refers(p: Policy, field: 'allowListRefs' | 'blockListRefs', l: BlockList): boolean {
  return (p.spec[field] ?? []).some((r) => r === l.metadata.name || r === refOf(l)) && nsOf(p) === nsOf(l);
}

const FIELD = { allow: 'allowListRefs', deny: 'blockListRefs' } as const;

/** The policies a ui list belongs in: everyone lists in every policy of the namespace, group lists in those covering the group. */
function wantedPolicies(db: MockDB, l: BlockList): Policy[] {
  const info = uiListInfo(l);
  if (!info) return [];
  const ns = nsOf(l);
  const gname = info.group?.split('/')[1];
  return db.policies.filter((p) => nsOf(p) === ns && (!gname || (p.spec.clientGroupRefs ?? []).includes(gname)));
}

function usedBy(db: MockDB, l: BlockList, action: DomainAction): string[] {
  return db.policies.filter((p) => refers(p, FIELD[action], l)).map(refOf);
}

function inScope(l: BlockList, ns: string | undefined): boolean {
  return !ns || nsOf(l) === ns;
}

export function uiLists(db: MockDB, action?: DomainAction, ns?: string): BlockList[] {
  return db.lists.filter((l) => {
    const info = uiListInfo(l);
    return !!info && (!action || info.action === action) && inScope(l, ns) && !!uiSource(l);
  });
}

/** Names in block lists (allow overrides) or allow lists (deny overrides) that contain the entry. */
function overridesOf(db: MockDB, e: Pick<DomainEntry, 'action' | 'type' | 'value' | 'namespace'>): { list: string; rule: string }[] {
  if (e.type === 'regex') return [];
  const name = e.type === 'wildcard' ? e.value.slice(2) : e.value;
  const out: { list: string; rule: string }[] = [];
  if (e.action === 'allow') {
    for (const l of db.lists) {
      if (l.spec.mode === 'allow' || uiListInfo(l)?.action === 'allow' || nsOf(l) !== e.namespace) continue;
      for (const s of l.spec.sources) {
        const rules = s.inline ?? (s.entries ?? []).filter((x) => x.enabled !== false).map((x) => x.value);
        const hit = rules.find((r) => r === name || r === `||${name}^` || r === `*.${name}` || r === `0.0.0.0 ${name}`);
        if (hit) {
          out.push({ list: refOf(l), rule: hit });
          break;
        }
      }
    }
  } else {
    for (const l of uiLists(db, 'allow', e.namespace)) {
      const hit = (uiSource(l)?.entries ?? []).find((x) => x.enabled !== false && (x.type ?? 'exact') !== 'regex' && (x.value === e.value || x.value === `*.${name}`));
      if (hit) out.push({ list: refOf(l), rule: renderRule(hit.type ?? 'exact', hit.value) });
    }
  }
  return out;
}

function readOnlyReason(owner: string | undefined): string | undefined {
  if (apiOwned(owner)) return undefined;
  if (owner?.startsWith('file:')) return `managed by ${owner.slice(5)}; edit the file on the host`;
  if (owner?.startsWith('kube')) return 'managed in Kubernetes';
  return `managed by ${owner}`;
}

/** Every logical entry, newest first, then by value. */
export function collectEntries(db: MockDB, action?: DomainAction, ns?: string): DomainEntry[] {
  const byKey = new Map<string, DomainEntry>();
  for (const l of uiLists(db, action, ns)) {
    const info = uiListInfo(l);
    const src = uiSource(l);
    if (!info || !src) continue;
    const owner = l.metadata.annotations?.[MANAGED];
    for (const x of src.entries ?? []) {
      const type = x.type ?? 'exact';
      const key = `${nsOf(l)}|${info.action}|${type}|${x.value}`;
      let e = byKey.get(key);
      if (!e) {
        e = {
          id: entryId(nsOf(l), info.action, type, x.value),
          action: info.action,
          namespace: nsOf(l),
          value: x.value,
          type,
          rule: renderRule(type, x.value),
          enabled: x.enabled !== false,
          ...(x.comment ? { comment: x.comment } : {}),
          everyone: false,
          groups: [],
          lists: [],
          policies: [],
          ...(x.created ? { created: x.created } : {}),
          ...(x.createdBy ? { createdBy: x.createdBy } : {}),
          ...(x.modified ? { modified: x.modified } : {}),
          origin: x.origin ?? 'manual',
          ...(x.request ? { request: x.request } : {}),
          overrides: [],
          readOnly: false,
        };
        byKey.set(key, e);
      }
      if (info.group) e.groups.push(info.group);
      else e.everyone = true;
      e.lists.push({ list: refOf(l), source: src.name ?? UI_SOURCE, ...(info.group ? { group: info.group } : {}), ...(owner ? { managedBy: owner } : {}) });
      for (const p of usedBy(db, l, info.action)) if (!e.policies.includes(p)) e.policies.push(p);
      const why = readOnlyReason(owner);
      if (why) {
        e.readOnly = true;
        e.readOnlyReason = why;
      }
    }
  }
  const out = [...byKey.values()];
  for (const e of out) e.overrides = overridesOf(db, e);
  return out.sort((a, b) => (b.created ?? '').localeCompare(a.created ?? '') || a.value.localeCompare(b.value));
}

export function listInfos(db: MockDB, action?: DomainAction, ns?: string): DomainListInfo[] {
  return uiLists(db, action, ns).map((l) => {
    const info = uiListInfo(l) as { action: DomainAction; group?: string };
    const owner = l.metadata.annotations?.[MANAGED];
    return {
      list: refOf(l),
      action: info.action,
      ...(info.group ? { group: info.group } : {}),
      enabled: l.metadata.annotations?.[ENABLED] !== 'false',
      entries: uiSource(l)?.entries?.length ?? 0,
      ...(owner ? { managedBy: owner } : {}),
      policies: usedBy(db, l, info.action),
    };
  });
}

/** Policies that should reference a ui list but cannot be changed through the API. */
export function unattachedOf(db: MockDB, action?: DomainAction, ns?: string): DomainUnattached[] {
  const out: DomainUnattached[] = [];
  for (const l of uiLists(db, action, ns)) {
    const info = uiListInfo(l);
    if (!info) continue;
    for (const p of wantedPolicies(db, l)) {
      const owner = p.metadata.annotations?.[MANAGED];
      if (apiOwned(owner) || refers(p, FIELD[info.action], l)) continue;
      out.push({ list: refOf(l), policy: refOf(p), managedBy: owner ?? '' });
    }
  }
  return out;
}

export interface EntryFilters {
  type?: string | null;
  enabled?: string | null;
  scope?: string | null;
  q?: string | null;
}

export function filterEntries(items: DomainEntry[], f: EntryFilters): DomainEntry[] {
  const q = (f.q ?? '').toLowerCase();
  return items.filter(
    (e) =>
      (!f.type || e.type === f.type) &&
      (!f.enabled || String(e.enabled) === f.enabled) &&
      (!f.scope || (f.scope === 'everyone' ? e.everyone : e.groups.includes(f.scope))) &&
      (!q || e.value.toLowerCase().includes(q) || (e.comment ?? '').toLowerCase().includes(q)),
  );
}

export class WriteError extends Error {
  constructor(
    readonly status: number,
    readonly key: string,
    message: string,
    readonly hint?: string,
  ) {
    super(message);
  }
}

/** The ui list of action for everyone (no group) or for a group "ns/name", created on demand. */
function ensureList(db: MockDB, ns: string, action: DomainAction, group: string | undefined): BlockList {
  const gname = group?.split('/')[1];
  const name = gname ? `ui-${action}-${gname}` : `ui-${action}`;
  let l = db.lists.find((x) => x.metadata.name === name && nsOf(x) === ns);
  if (!l) {
    db.rv += 1;
    l = {
      apiVersion: 'kudurru.io/v1alpha1',
      kind: 'BlockList',
      metadata: { name, namespace: ns, resourceVersion: String(db.rv), annotations: { [MANAGED]: 'api', ...(group ? { [GROUP_ANNOTATION]: group } : {}) } },
      spec: { mode: action === 'allow' ? 'allow' : 'block', sources: [{ name: UI_SOURCE, entries: [] }] },
      status: { entries: 0, ruleCount: 0, sources: [{ name: UI_SOURCE, entries: 0 }], conditions: [{ type: 'Ready', status: 'True', reason: 'AsExpected', message: '' }] },
    };
    db.lists.push(l);
  } else if (!uiSource(l)) {
    l.spec.sources.push({ name: UI_SOURCE, entries: [] });
  }
  return l;
}

/** Adds the list to the policies it belongs in; answers the attached and the skipped ones. */
function attach(db: MockDB, l: BlockList, action: DomainAction): { attached: string[]; skipped: string[] } {
  const attached: string[] = [];
  const skipped: string[] = [];
  for (const p of wantedPolicies(db, l)) {
    if (refers(p, FIELD[action], l)) continue;
    if (!apiOwned(p.metadata.annotations?.[MANAGED])) {
      skipped.push(refOf(p));
      continue;
    }
    p.spec[FIELD[action]] = [...(p.spec[FIELD[action]] ?? []), l.metadata.name];
    attached.push(refOf(p));
  }
  return { attached, skipped };
}

/** Recounts a ui list after a change (status.entries counts the enabled entries). */
export function recount(db: MockDB, l: BlockList): void {
  db.rv += 1;
  l.metadata.resourceVersion = String(db.rv);
  l.metadata.generation = (l.metadata.generation ?? 0) + 1;
  const sources = l.spec.sources.map((s, i) => {
    const name = sourceName(i, s);
    const prev = l.status?.sources?.find((x) => x.name === name) ?? { name };
    if (s.entries) {
      const off = s.entries.filter((x) => x.enabled === false).length;
      return { ...prev, name, entries: s.entries.length - off, ...(off ? { excluded: off } : { excluded: undefined }) };
    }
    if (s.inline) {
      const off = (s.exclude ?? []).filter((x) => s.inline?.includes(x)).length;
      return { ...prev, name, entries: s.inline.length - off, ...(off ? { excluded: off } : {}) };
    }
    return prev;
  });
  const entries = sources.reduce((n, s) => n + (s.enabled === false ? 0 : (s.entries ?? 0)), 0);
  l.status = { ...(l.status ?? {}), entries, ruleCount: entries, sources };
}

function groupRef(db: MockDB, ns: string, g: string): string {
  const ref = g.includes('/') ? g : `${ns}/${g}`;
  const [gns, gname] = ref.split('/');
  if (!db.groups.some((x) => nsOf(x) === gns && x.metadata.name === gname)) throw new WriteError(422, 'unprocessable', `unknown client group ${ref}`);
  if (!db.policies.some((p) => nsOf(p) === gns && (p.spec.clientGroupRefs ?? []).includes(gname ?? ''))) {
    throw new WriteError(422, 'unprocessable', `no policy covers client group ${ref}`, 'create a policy for the group or use everyone');
  }
  return ref;
}

export interface WriteResult {
  items: DomainEntry[];
  processed: { success: { item: string }[]; errors: { item: string; error: string }[] };
  attached: string[];
  skipped: string[];
  flushed: number;
}

function find(db: MockDB, ns: string, action: DomainAction, type: DomainEntryType, value: string): DomainEntry | undefined {
  return collectEntries(db, action, ns).find((e) => e.type === type && e.value === value);
}

function merge(into: WriteResult, r: { attached: string[]; skipped: string[] }) {
  for (const p of r.attached) if (!into.attached.includes(p)) into.attached.push(p);
  for (const p of r.skipped) if (!into.skipped.includes(p)) into.skipped.push(p);
}

/** POST /allowlist|/denylist. */
export function createEntries(db: MockDB, action: DomainAction, body: DomainEntriesCreate, actor: string, origin?: { origin: ListSourceEntry['origin']; request?: string }): WriteResult {
  const ns = body.namespace || 'default';
  const type = body.type ?? 'exact';
  if (!Array.isArray(body.values) || body.values.length < 1 || body.values.length > 1000) throw new WriteError(400, 'invalid', 'values: 1 to 1000 values');
  if ((body.comment ?? '').length > 500) throw new WriteError(400, 'invalid', 'comment: at most 500 characters');
  const groups = (body.groups ?? []).map((g) => groupRef(db, ns, g));
  const targets = groups.length ? groups : [undefined];
  const res: WriteResult = { items: [], processed: { success: [], errors: [] }, attached: [], skipped: [], flushed: 0 };
  const now = new Date().toISOString().replace(/\.\d{3}Z$/, 'Z');
  for (const raw of body.values) {
    const err = valueError(type, raw);
    if (err) {
      res.processed.errors.push({ item: raw, error: err });
      continue;
    }
    const value = normaliseValue(type, raw);
    let added = false;
    for (const g of targets) {
      const l = ensureList(db, ns, action, g);
      const src = uiSource(l) as ListSource;
      const entries = src.entries ?? [];
      if (entries.some((x) => (x.type ?? 'exact') === type && x.value === value)) continue;
      entries.push({
        value,
        type,
        ...(body.enabled === false ? { enabled: false } : {}),
        ...(body.comment ? { comment: body.comment } : {}),
        created: now,
        createdBy: actor,
        origin: origin?.origin ?? body.origin ?? 'manual',
        ...(origin?.request ? { request: origin.request } : {}),
      });
      src.entries = entries;
      recount(db, l);
      merge(res, attach(db, l, action));
      added = true;
    }
    if (!added) {
      res.processed.errors.push({ item: raw, error: `already on the ${action === 'allow' ? 'Allowlist' : 'Denylist'}` });
      continue;
    }
    res.processed.success.push({ item: value });
    const e = find(db, ns, action, type, value);
    if (e) res.items.push(e);
    res.flushed += 1;
  }
  return res;
}

/** The copies (list, source, index) of one logical entry. */
function copiesOf(db: MockDB, ns: string, action: DomainAction, type: DomainEntryType, value: string) {
  return uiLists(db, action, ns).flatMap((l) => {
    const src = uiSource(l);
    const i = (src?.entries ?? []).findIndex((x) => (x.type ?? 'exact') === type && x.value === value);
    return src && i >= 0 ? [{ list: l, src, index: i, group: uiListInfo(l)?.group }] : [];
  });
}

function mustFind(db: MockDB, action: DomainAction, id: string) {
  const k = parseId(id);
  if (!k || k.action !== action) throw new WriteError(404, 'not_found', `no ${action} entry ${id}`);
  const copies = copiesOf(db, k.ns, action, k.type, k.value);
  if (!copies.length) throw new WriteError(404, 'not_found', `no ${action} entry ${k.value}`);
  const locked = copies.find((c) => !apiOwned(c.list.metadata.annotations?.[MANAGED]));
  return { k, copies, locked };
}

function lockedError(c: { list: BlockList }): WriteError {
  const owner = c.list.metadata.annotations?.[MANAGED] ?? '';
  return new WriteError(409, 'managed_by', `BlockList ${c.list.metadata.name} is managed by ${owner}`, owner);
}

/** PATCH /allowlist/{id}. */
export function patchEntry(db: MockDB, action: DomainAction, id: string, body: DomainEntryPatch): WriteResult {
  const { k, copies, locked } = mustFind(db, action, id);
  if (locked) throw lockedError(locked);
  const type = body.type ?? k.type;
  let value = k.value;
  if (body.value !== undefined || body.type !== undefined) {
    const err = valueError(type, body.value ?? k.value);
    if (err) throw new WriteError(400, 'invalid', `value: ${err}`);
    value = normaliseValue(type, body.value ?? k.value);
  }
  if (body.scope === 'groups' && !(body.groups ?? []).length) throw new WriteError(400, 'invalid', 'groups: scope groups needs at least one group');
  if ((body.comment ?? '').length > 500) throw new WriteError(400, 'invalid', 'comment: at most 500 characters');
  const now = new Date().toISOString().replace(/\.\d{3}Z$/, 'Z');
  const base = copies[0]?.src.entries?.[copies[0].index] as ListSourceEntry;
  const next: ListSourceEntry = {
    ...base,
    value,
    type,
    modified: now,
    ...(body.enabled !== undefined ? { enabled: body.enabled } : {}),
    ...(body.comment !== undefined ? { comment: body.comment } : {}),
  };
  if (next.enabled !== false) delete next.enabled;
  if (!next.comment) delete next.comment;
  const res: WriteResult = { items: [], processed: { success: [{ item: value }], errors: [] }, attached: [], skipped: [], flushed: 1 };
  const targets: (string | undefined)[] | null = body.scope === 'everyone' ? [undefined] : body.scope === 'groups' ? (body.groups ?? []).map((g) => groupRef(db, k.ns, g)) : null;
  for (const c of copies) {
    const entries = c.src.entries ?? [];
    if (targets && !targets.includes(c.group)) entries.splice(c.index, 1);
    else entries[c.index] = { ...next };
    recount(db, c.list);
  }
  if (targets) {
    for (const g of targets) {
      if (copies.some((c) => c.group === g)) continue;
      const l = ensureList(db, k.ns, action, g);
      const src = uiSource(l) as ListSource;
      src.entries = [...(src.entries ?? []), { ...next }];
      recount(db, l);
      merge(res, attach(db, l, action));
    }
  }
  const e = find(db, k.ns, action, type, value);
  if (e) res.items.push(e);
  return res;
}

/** DELETE /allowlist/{id}. */
export function deleteEntry(db: MockDB, action: DomainAction, id: string): void {
  const { copies, locked } = mustFind(db, action, id);
  if (locked) throw lockedError(locked);
  for (const c of [...copies].reverse()) {
    c.src.entries?.splice(c.index, 1);
    recount(db, c.list);
  }
}

/** POST /allowlist:batch. */
export function batchEntries(db: MockDB, action: DomainAction, ids: string[], op: string): WriteResult {
  if (op !== 'enable' && op !== 'disable' && op !== 'delete') throw new WriteError(400, 'invalid', 'op: enable, disable or delete');
  const res: WriteResult = { items: [], processed: { success: [], errors: [] }, attached: [], skipped: [], flushed: 0 };
  for (const id of ids) {
    try {
      if (op === 'delete') deleteEntry(db, action, id);
      else res.items.push(...patchEntry(db, action, id, { enabled: op === 'enable' }).items);
      res.processed.success.push({ item: id });
      res.flushed += 1;
    } catch (e) {
      res.processed.errors.push({ item: parseId(id)?.value ?? id, error: (e as Error).message });
    }
  }
  return res;
}

/** Writes an approved unblock request's entry (internal/unblock decide.go): structured, origin unblock. */
export function approveEntry(db: MockDB, ns: string, group: string | undefined, domain: string, subdomains: boolean, request: string, actor: string): boolean {
  const type: DomainEntryType = subdomains ? 'wildcard' : 'exact';
  const value = subdomains ? `*.${domain}` : domain;
  const l = ensureList(db, ns, 'allow', group);
  const src = uiSource(l) as ListSource;
  if ((src.entries ?? []).some((x) => (x.type ?? 'exact') === type && x.value === value)) return false;
  src.entries = [...(src.entries ?? []), { value, type, created: new Date().toISOString().replace(/\.\d{3}Z$/, 'Z'), createdBy: actor, origin: 'unblock', request }];
  recount(db, l);
  return true;
}

/** Removes an approval's entry again (deny after approve). */
export function releaseEntry(db: MockDB, listRef: string, entry: string): void {
  const l = db.lists.find((x) => refOf(x) === listRef);
  const src = l ? uiSource(l) : undefined;
  if (!l || !src?.entries) return;
  const value = entry.startsWith('||') ? `*.${entry.slice(2).replace(/\^$/, '')}` : entry;
  src.entries = src.entries.filter((x) => x.value !== value);
  recount(db, l);
}

// ---------------------------------------------------------------------------
// The entries of one source, any kind.

const BODIES = new Map<string, string[]>();

/**
 * The normalised rules of the cached body of a url or configMap source:
 * generated once per source (deterministic), as many as the status counts
 * plus the switched-off ones, with the source's exclude[] values that the
 * fixtures put in the body at the front.
 */
export function cachedBody(l: BlockList, src: ListSource, name: string): string[] {
  const key = `${refOf(l)}/${name}`;
  let body = BODIES.get(key);
  if (!body) {
    const st = l.status?.sources?.find((s) => s.name === name);
    const stale = new Set(st?.staleExcludes ?? []);
    const seeded = (src.exclude ?? []).filter((x) => !stale.has(x));
    const n = Math.max(0, (st?.entries ?? 0) + seeded.length);
    const adblock = src.format === 'adblock';
    body = [...seeded];
    const seen = new Set(seeded);
    const words = ['ads', 'track', 'pixel', 'metrics', 'beacon', 'stats', 'banner', 'click'];
    for (let i = 0; body.length < n; i++) {
      const host = `${words[i % words.length]}${Math.floor(i / words.length) || ''}.${name.replace(/[^a-z0-9]+/g, '')}.example`;
      const rule = adblock ? `||${host}^` : host;
      if (!seen.has(rule)) body.push(rule);
    }
    BODIES.set(key, body);
  }
  return body;
}

/** Test hook: forget the generated bodies (resetDB keeps them otherwise). */
export function setCachedBody(key: string, rules: string[] | undefined): void {
  if (rules) BODIES.set(key, rules);
  else BODIES.delete(key);
}

export function sourceKindOf(src: ListSource): SourceEntriesKind {
  if (src.url) return 'url';
  if (src.configMapRef) return 'configMap';
  if (src.entries) return 'entries';
  return 'inline';
}

/** Every entry of a source with its switch, or null while this node has no body (503). */
export function sourceItems(l: BlockList, src: ListSource, name: string): ListEntry[] | null {
  const kind = sourceKindOf(src);
  if (kind === 'entries') return (src.entries ?? []).map((x, i) => ({ value: renderRule(x.type ?? 'exact', x.value), line: i + 1, enabled: x.enabled !== false }));
  const off = new Set(src.exclude ?? []);
  if (kind === 'inline') return (src.inline ?? []).map((value, i) => ({ value, line: i + 1, enabled: !off.has(value) }));
  const st = l.status?.sources?.find((s) => s.name === name);
  if (!st?.digest && !st?.lastFetched) return null;
  return cachedBody(l, src, name).map((value, i) => ({ value, line: i + 1, enabled: !off.has(value) }));
}

/** Switches values of a source on or off; answers the values that changed. */
export function switchEntries(src: ListSource, items: ListEntry[], enable: string[], disable: string[]): { enabled: string[]; disabled: string[] } {
  const enabled: string[] = [];
  const disabled: string[] = [];
  if (src.entries) {
    for (const x of src.entries) {
      const rule = renderRule(x.type ?? 'exact', x.value);
      if (enable.includes(rule) && x.enabled === false) {
        delete x.enabled;
        enabled.push(rule);
      } else if (disable.includes(rule) && x.enabled !== false) {
        x.enabled = false;
        disabled.push(rule);
      }
    }
    return { enabled, disabled };
  }
  const known = new Set(items.map((i) => i.value));
  const ex = new Set(src.exclude ?? []);
  for (const v of enable) if (ex.delete(v)) enabled.push(v);
  for (const v of disable) {
    if (known.has(v) && !ex.has(v)) {
      ex.add(v);
      disabled.push(v);
    }
  }
  if (ex.size) src.exclude = [...ex];
  else delete src.exclude;
  return { enabled, disabled };
}
