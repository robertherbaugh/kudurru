// Integrations (docs/deploy/integrations.md, D367, D400): the form of an
// instance is derived from its provider's manifest (GET
// /integrations/manifests), never from a list of providers in the
// dashboard. The provider block's JSON Schema becomes a C1 shape that
// ShapeField renders; the fields this node's host refuses (manifest
// `refused`) are left out, so the form never offers them. Credentials are
// references (a file path or an environment name) of the forms the node
// resolves; there is no field for a value. What a schema cannot say is
// kept here by provider block and compared with the Go code by
// lib/integrations.test.ts: the publish rule (PUBLISH_RULES), the
// destination fields, the conditions between fields (FIELD_CONDITIONS) and
// why a refused field is refused.
import { ApiError } from '../api/client';
import type { CredentialReference, Integration, IntegrationManifest, IntegrationManifests, IntegrationSpec, IntegrationSummary, SettingFormat } from '../api/types';
import { FORMAT_EXAMPLES, formatError } from '../components/fields/formats';
import { fromDraft, shapeError, toDraft } from './shapes';
import type { UiShape } from './settingsMeta';

/** A JSON Schema node of a manifest's configSchema (the subset providers use). */
export interface JSONSchema {
  type?: string | string[];
  properties?: Record<string, JSONSchema>;
  additionalProperties?: boolean | JSONSchema;
  required?: string[];
  items?: JSONSchema;
  enum?: unknown[];
  pattern?: string;
  format?: string;
  minimum?: number;
  maximum?: number;
  minLength?: number;
  maxLength?: number;
  minItems?: number;
  maxItems?: number;
  maxProperties?: number;
  uniqueItems?: boolean;
  description?: string;
}

export const CAPABILITY_LABELS: Record<string, string> = {
  RecordSink: 'Publishes DNS records',
  ClientSource: 'Reads client inventory',
  Hook: 'Receives events',
  Actions: 'Offers actions',
};

/** The reason codes a manifest or status names, as a sentence. */
export const REASON_TEXT: Record<string, string> = {
  UnsupportedOnKubernetes: 'This provider does not run on Kubernetes.',
  RecordPublicationUnsupportedOnKubernetes: 'Record publication from this provider does not run on Kubernetes.',
  UniFiPushUnsupportedOnKubernetes: 'UniFi writes (spec.unifi.push) run on SQLite only: on Kubernetes an Integration that enables one is refused.',
  NamespaceNotAllowed: 'Only Integrations in the release namespace run on Kubernetes.',
  LicenseUnavailable: 'The license does not cover integrations.',
  Suspended: 'Suspended (spec.suspend).',
  CredentialsUnavailable: 'A credential reference does not resolve on this node.',
  NoMatchingObservation: 'The host has not reported a status for this revision yet.',
};

const ACRONYMS = new Set(['id', 'ip', 'tls', 'dns', 'ttl', 'dhcp', 'url', 'ca', 'hmac', 'api', 'mac']);

/** "tlsFingerprint" -> "TLS fingerprint", "zoneID" -> "Zone ID". */
export function humanize(name: string): string {
  const words = name.replace(/([a-z0-9])([A-Z])/g, '$1 $2').replace(/([A-Z]+)([A-Z][a-z])/g, '$1 $2').split(/[\s_-]+/).filter(Boolean);
  return words
    .map((w, i) => {
      const lower = w.toLowerCase();
      if (ACRONYMS.has(lower)) return lower.toUpperCase();
      return i === 0 ? lower.charAt(0).toUpperCase() + lower.slice(1) : lower;
    })
    .join(' ');
}

function typeOf(s: JSONSchema): string | undefined {
  return Array.isArray(s.type) ? s.type.find((t) => t !== 'null') : s.type;
}

/** A JSON Schema string format the field controls check (ipv4, ...), or undefined. */
function formatOf(s: JSONSchema): SettingFormat | undefined {
  return s.format && s.format in FORMAT_EXAMPLES ? (s.format as SettingFormat) : undefined;
}

/** Provider strings that are Go durations (metav1.Duration), by field name. */
const DURATION_NAMES = /^(poll|timeout|interval|resync)$/i;

function helpOf(s: JSONSchema): string | undefined {
  const parts: string[] = [];
  if (s.description) parts.push(s.description);
  if (s.pattern?.startsWith('^https://')) parts.push('An https:// URL.');
  if (s.pattern && /^\^sha256:/.test(s.pattern)) parts.push('sha256: followed by 64 hex digits.');
  if (s.pattern === '^[a-f0-9]{32}$') parts.push('32 lower-case hex digits.');
  return parts.length ? parts.join(' ') : undefined;
}

/**
 * The C1 shape of a JSON Schema node, or null when the node (at `path`)
 * is refused by this node's host or cannot be edited structurally.
 */
export function schemaShape(s: JSONSchema, path: string, refused: ReadonlySet<string>): UiShape | null {
  if (refused.has(path)) return null;
  const help = helpOf(s);
  switch (typeOf(s)) {
    case 'boolean':
      return { type: 'bool', help };
    case 'integer':
    case 'number':
      return { type: 'int', min: s.minimum, max: s.maximum, help };
    case 'string': {
      const name = path.slice(path.lastIndexOf('.') + 1);
      if (s.enum?.length) return { type: 'string', enum: s.enum.map((v) => ({ value: String(v), label: String(v) })), help };
      if (DURATION_NAMES.test(name)) return { type: 'duration', help };
      if (s.pattern?.startsWith('^https://')) return { type: 'string', format: 'url', help };
      const format = formatOf(s);
      return { type: 'string', ...(format ? { format } : {}), help };
    }
    case 'array': {
      const items = s.items ?? {};
      if (typeOf(items) === 'string') {
        const format = formatOf(items);
        return { type: 'string[]', items: { type: 'string', ...(items.enum?.length ? { enum: items.enum.map((v) => ({ value: String(v), label: String(v) })) } : {}), ...(format ? { format } : {}) }, help };
      }
      const item = schemaShape(items, `${path}[]`, refused);
      return item ? { type: 'list', items: item, help } : null;
    }
    case 'object': {
      if (s.properties && Object.keys(s.properties).length) {
        const required = new Set(s.required ?? []);
        const fields: NonNullable<UiShape['fields']> = [];
        for (const [name, child] of Object.entries(s.properties)) {
          const shape = schemaShape(child, `${path}.${name}`, refused);
          if (!shape) continue;
          // An optional choice starts unset: the provider's default applies.
          if (!required.has(name) && shape.type === 'string' && shape.enum?.length) shape.enum = [{ value: '', label: 'Provider default' }, ...shape.enum];
          fields.push({ ...shape, name, label: humanize(name), required: required.has(name) });
        }
        return fields.length ? { type: 'object', fields, help } : null;
      }
      if (s.additionalProperties && typeof s.additionalProperties === 'object') {
        const values = schemaShape(s.additionalProperties, `${path}.*`, refused);
        return values ? { type: 'map', keys: { type: 'string' }, values, help, keyLabel: 'Key' } : null;
      }
      return null;
    }
    default:
      return null;
  }
}

export function configSchema(m: IntegrationManifest): JSONSchema {
  const s = m.configSchema;
  if (typeof s === 'string') {
    try {
      return JSON.parse(s) as JSONSchema;
    } catch {
      return {};
    }
  }
  return (s ?? {});
}

/**
 * Conditions between fields of a provider block that its JSON Schema
 * cannot state; the provider checks them (ee/integration/unifi
 * validatePush, D401) and refuses the instance. When `when` is on, every
 * `needs` field must be set. With `only`, the `needs` fields mean nothing
 * without `when`, so they are not offered where the node refuses `when`.
 * lib/integrations.test.ts keeps them in step with the Go checks.
 */
export const FIELD_CONDITIONS: readonly { when: string; needs: readonly string[]; only?: boolean; message: string }[] = [
  { when: 'spec.unifi.push.dhcpDNSServers', needs: ['spec.unifi.push.dnsServers', 'spec.unifi.push.networks'], only: true, message: 'Push: DHCP DNS servers needs at least one DNS server and one network' },
  { when: 'spec.unifi.push.clientActions', needs: ['spec.unifi.legacy'], message: 'Push: Client actions needs Legacy (block and unblock use the legacy controller API)' },
  { when: 'spec.unifi.push.mirrorRecords.enabled', needs: ['spec.unifi.push.mirrorRecords.domains'], message: 'Push: Mirror records needs at least one domain' },
];

/** The fields this node does not offer: the refused ones and those that mean nothing without a refused one. */
function hiddenFields(m: IntegrationManifest): Set<string> {
  const refused = new Set(m.refused);
  const hidden = new Set(refused);
  for (const c of FIELD_CONDITIONS) if (c.only && refused.has(c.when)) for (const n of c.needs) hidden.add(n);
  return hidden;
}

/** The shape of a provider's block (spec.<configField>) on this node: refused fields left out. */
export function providerShape(m: IntegrationManifest): UiShape {
  const shape = schemaShape(configSchema(m), `spec.${m.configField}`, hiddenFields(m));
  return shape?.type === 'object' ? shape : { type: 'object', fields: [] };
}

/**
 * Why this node does not offer the refused fields of a manifest: the
 * manifest lists the paths, the reasons are the status reasons the host
 * gives an instance that sets one (cmd/kudurru kubeIntegrationRefusal).
 */
const REFUSED_FIELD_REASONS: readonly { prefix: string; reason: string }[] = [
  { prefix: 'spec.unifi.inventory', reason: 'RecordPublicationUnsupportedOnKubernetes' },
  { prefix: 'spec.unifi.push.', reason: 'UniFiPushUnsupportedOnKubernetes' },
];

/** The refused fields of a manifest grouped by the reason the host gives (reason '' when unknown). */
export function refusedFields(m: IntegrationManifest): { reason: string; text: string; fields: string[] }[] {
  const out: { reason: string; text: string; fields: string[] }[] = [];
  for (const f of m.refused) {
    const reason = REFUSED_FIELD_REASONS.find((r) => f === r.prefix || f.startsWith(r.prefix))?.reason ?? '';
    let group = out.find((g) => g.reason === reason);
    if (!group) {
      group = { reason, text: REASON_TEXT[reason] ?? 'This node refuses an instance that sets them.', fields: [] };
      out.push(group);
    }
    group.fields.push(f);
  }
  return out;
}

function valueAt(v: unknown, path: readonly string[]): unknown {
  let x = v;
  for (const p of path) x = x && typeof x === 'object' && !Array.isArray(x) ? (x as Record<string, unknown>)[p] : undefined;
  return x;
}

/** The first FIELD_CONDITIONS problem of a built provider block, or null. */
function conditionError(m: IntegrationManifest, block: Record<string, unknown>): string | null {
  const prefix = `spec.${m.configField}.`;
  const rel = (p: string) => p.slice(prefix.length).split('.');
  const set = (x: unknown) => x !== undefined && x !== null && x !== false && x !== '' && !(Array.isArray(x) && x.length === 0);
  for (const c of FIELD_CONDITIONS) {
    if (!c.when.startsWith(prefix) || valueAt(block, rel(c.when)) !== true) continue;
    if (c.needs.some((n) => !set(valueAt(block, rel(n))))) return c.message;
  }
  return null;
}

/**
 * The first problem of a provider block value against the schema
 * (required, enum, pattern, bounds), prefixed with where it is.
 */
export function schemaError(s: JSONSchema, v: unknown, where: string): string | null {
  if (v === undefined || v === null) return null;
  const at = (msg: string) => `${where}: ${msg}`;
  switch (typeOf(s)) {
    case 'string': {
      if (typeof v !== 'string') return at('must be text');
      if (s.enum?.length && !s.enum.includes(v)) return at(`one of ${s.enum.join(', ')}`);
      if (s.minLength !== undefined && v.length < s.minLength) return at(`at least ${s.minLength} characters`);
      if (s.maxLength !== undefined && v.length > s.maxLength) return at(`at most ${s.maxLength} characters`);
      if (s.pattern) {
        let re: RegExp | null;
        try {
          re = new RegExp(s.pattern);
        } catch {
          re = null;
        }
        if (re && !re.test(v)) return at(helpOf(s) ?? `must match ${s.pattern}`);
      }
      const fe = formatError(formatOf(s), v);
      return fe ? at(fe) : null;
    }
    case 'integer':
    case 'number': {
      if (typeof v !== 'number' || Number.isNaN(v)) return at('must be a number');
      if (s.minimum !== undefined && v < s.minimum) return at(`at least ${s.minimum}`);
      if (s.maximum !== undefined && v > s.maximum) return at(`at most ${s.maximum}`);
      return null;
    }
    case 'array': {
      if (!Array.isArray(v)) return at('must be a list');
      if (s.minItems !== undefined && v.length < s.minItems) return at(`at least ${s.minItems} ${s.minItems === 1 ? 'entry' : 'entries'}`);
      if (s.maxItems !== undefined && v.length > s.maxItems) return at(`at most ${s.maxItems} entries`);
      if (s.uniqueItems && new Set(v.map((x) => JSON.stringify(x))).size !== v.length) return at('lists an entry twice');
      for (let i = 0; i < v.length; i++) {
        const e = schemaError(s.items ?? {}, v[i], `${where} #${i + 1}`);
        if (e) return e;
      }
      return null;
    }
    case 'object': {
      if (typeof v !== 'object' || Array.isArray(v)) return at('must be a set of fields');
      const o = v as Record<string, unknown>;
      for (const r of s.required ?? []) {
        if (o[r] === undefined || o[r] === '') return `${where ? `${where} ` : ''}${humanize(r)} is required`;
      }
      if (s.maxProperties !== undefined && Object.keys(o).length > s.maxProperties) return at(`at most ${s.maxProperties} entries`);
      for (const [k, x] of Object.entries(o)) {
        const child = s.properties?.[k] ?? (typeof s.additionalProperties === 'object' ? s.additionalProperties : undefined);
        if (!child) continue;
        const e = schemaError(child, x, `${where ? `${where} ` : ''}${s.properties?.[k] ? humanize(k) : k}`);
        if (e) return e;
      }
      return null;
    }
    default:
      return null;
  }
}

// ---------------------------------------------------------------------------
// Credentials: references only

export type CredentialForm = 'file' | 'env';

export interface CredentialDraft {
  /** "" while the key is not bound. */
  source: CredentialForm | '';
  value: string;
  /** A stored secretRef is kept as it is (neither host resolves it). */
  secretRef?: { name: string; key: string };
}

export const CREDENTIAL_FORM_LABELS: Record<CredentialForm, string> = { file: 'File on the node', env: 'Environment variable' };

/** The problem with a credential reference, or null. */
export function credentialRefError(d: CredentialDraft): string | null {
  if (d.secretRef || d.source === '') return null;
  const v = d.value.trim();
  if (!v) return d.source === 'file' ? 'Enter the file path' : 'Enter the variable name';
  if (d.source === 'file') {
    if (!v.startsWith('/')) return 'An absolute path, e.g. /run/secrets/token';
    if (/\/\/|\/\.\.?(\/|$)/.test(v) || (v.length > 1 && v.endsWith('/'))) return 'A clean absolute path (no //, . or .. segments, no trailing /)';
    return null;
  }
  return /^[A-Za-z_][A-Za-z0-9_]*$/.test(v) ? null : 'Letters, digits and _, not starting with a digit';
}

export function credentialDraft(ref: CredentialReference | undefined): CredentialDraft {
  if (!ref) return { source: '', value: '' };
  if (ref.file) return { source: 'file', value: ref.file };
  if (ref.env) return { source: 'env', value: ref.env };
  if (ref.secretRef) return { source: '', value: '', secretRef: { ...ref.secretRef } };
  return { source: '', value: '' };
}

export function credentialRef(d: CredentialDraft): CredentialReference | undefined {
  if (d.secretRef) return { secretRef: { ...d.secretRef } };
  const v = d.value.trim();
  if (!d.source || !v) return undefined;
  return d.source === 'file' ? { file: v } : { env: v };
}

/** A one-line description of a reference (names only; there are no values). */
export function describeRef(ref: CredentialReference | undefined): string {
  if (!ref) return 'not set';
  if (ref.file) return `file ${ref.file}`;
  if (ref.env) return `environment ${ref.env}`;
  if (ref.secretRef) return `Secret ${ref.secretRef.name}/${ref.secretRef.key} (not resolved by this node)`;
  return 'not set';
}

// ---------------------------------------------------------------------------
// The form of one instance

export interface IntegrationDraft {
  type: string;
  name: string;
  namespace: string;
  /** The provider block as a shape draft (lib/shapes). */
  config: unknown;
  credentials: Record<string, CredentialDraft>;
  resync: string;
  suspend: boolean;
  deletionPolicy: 'Retain' | 'Delete';
}

export const DELETION_POLICY_OPTIONS = [
  { value: 'Retain', label: 'Retain', help: 'Deleting the Integration leaves what it published at the provider.' },
  { value: 'Delete', label: 'Delete', help: 'Deleting the Integration first removes the records it provably owns at the provider, then the object.' },
];

/** A new instance of a manifest's type. */
export function newDraft(m: IntegrationManifest, namespace: string): IntegrationDraft {
  const shape = providerShape(m);
  return {
    type: m.type,
    name: '',
    namespace,
    config: toDraft(shape, {}),
    credentials: Object.fromEntries(m.credentials.map((c) => [c.name, { source: '', value: '' } satisfies CredentialDraft])),
    resync: '',
    suspend: false,
    deletionPolicy: 'Retain',
  };
}

/** The form of a stored instance. Fields the form does not show pass through. */
export function draftOf(obj: Integration, m: IntegrationManifest | undefined): IntegrationDraft {
  const shape = m ? providerShape(m) : { type: 'object' as const, fields: [] };
  const field = m?.configField ?? obj.spec.type;
  const creds: Record<string, CredentialDraft> = {};
  for (const c of m?.credentials ?? []) creds[c.name] = credentialDraft(obj.spec.credentials?.[c.name]);
  for (const [k, ref] of Object.entries(obj.spec.credentials ?? {})) creds[k] ??= credentialDraft(ref);
  return {
    type: obj.spec.type,
    name: obj.metadata.name,
    namespace: obj.metadata.namespace ?? 'default',
    config: toDraft(shape, obj.spec[field] ?? {}),
    credentials: creds,
    resync: obj.spec.resync ?? '',
    suspend: obj.spec.suspend === true,
    deletionPolicy: obj.spec.deletionPolicy === 'Delete' ? 'Delete' : 'Retain',
  };
}

/** The provider block of a draft as the API takes it. */
export function configValue(m: IntegrationManifest, d: IntegrationDraft): Record<string, unknown> {
  const v = prune(fromDraft(providerShape(m), d.config));
  return v && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : {};
}

/**
 * Drops false, empty lists and empty objects inside objects: the provider
 * fields are Go omitempty fields, so they mean the same as an absent one,
 * and a round trip through the form does not add them to the document.
 */
function prune(v: unknown): unknown {
  if (Array.isArray(v)) return v.map(prune);
  if (!v || typeof v !== 'object') return v;
  const out: Record<string, unknown> = {};
  for (const [k, x] of Object.entries(v as Record<string, unknown>)) {
    const p = prune(x);
    if (p === false || p === undefined || (Array.isArray(p) && p.length === 0) || (p && typeof p === 'object' && !Array.isArray(p) && Object.keys(p).length === 0)) continue;
    out[k] = p;
  }
  return out;
}

/** A shape with no field marked required, at any depth. */
function withoutRequired(s: UiShape): UiShape {
  return {
    ...s,
    ...(s.fields ? { fields: s.fields.map((f) => ({ ...withoutRequired(f), name: f.name, required: false })) } : {}),
    ...(s.items ? { items: withoutRequired(s.items) } : {}),
    ...(s.values ? { values: withoutRequired(s.values) } : {}),
  };
}

/**
 * The first problem of a draft's provider block, or null. The draft check
 * covers what only the draft shows (half-typed numbers, durations, map
 * keys); required-ness is checked on the built value against the schema,
 * because a required child of an optional object is required only when
 * that object is set (Cloudflare adoptFrom.externalDNSOwner, UniFi
 * inventory.zoneRef), and the built value leaves an untouched optional
 * object out.
 */
export function configError(m: IntegrationManifest, d: IntegrationDraft): string | null {
  const value = configValue(m, d);
  return shapeError(withoutRequired(providerShape(m)), d.config) ?? schemaError(configSchema(m), value, '') ?? conditionError(m, value);
}

/** The first problem of a draft's credentials, or null. */
export function credentialsError(m: IntegrationManifest, d: IntegrationDraft, forms: readonly string[]): string | null {
  for (const c of m.credentials) {
    const cd = d.credentials[c.name] ?? { source: '', value: '' };
    if (c.required && !cd.secretRef && (cd.source === '' || !cd.value.trim())) return `${humanize(c.name)} is required`;
    if (cd.source && !forms.includes(cd.source) && cd.value.trim()) return `${humanize(c.name)}: this node does not resolve ${cd.source} references`;
    const e = credentialRefError(cd);
    if (e) return `${humanize(c.name)}: ${e}`;
  }
  return null;
}

/** The Integration a draft describes, over the stored object when there is one. */
export function buildIntegration(m: IntegrationManifest, d: IntegrationDraft, stored?: Integration): Integration {
  const spec: IntegrationSpec = { ...(stored?.spec ?? {}), type: d.type };
  spec[m.configField] = configValue(m, d);
  const credentials: Record<string, CredentialReference> = {};
  for (const [k, cd] of Object.entries(d.credentials)) {
    const ref = credentialRef(cd);
    if (ref) credentials[k] = ref;
  }
  if (Object.keys(credentials).length) spec.credentials = credentials;
  else delete spec.credentials;
  if (d.resync.trim()) spec.resync = d.resync.trim();
  else delete spec.resync;
  if (d.suspend) spec.suspend = true;
  else delete spec.suspend;
  spec.deletionPolicy = d.deletionPolicy;
  const metadata = stored ? { ...stored.metadata } : { name: d.name.trim(), namespace: d.namespace.trim() };
  return { apiVersion: 'kudurru.io/v1alpha1', kind: 'Integration', metadata, spec };
}

/** Whether the credential references of two specs differ (D400: needs integrations.secrets). */
export function credentialsChanged(a: IntegrationSpec | undefined, b: IntegrationSpec): boolean {
  const ca = a?.credentials ?? {};
  const cb = b.credentials ?? {};
  const keys = new Set([...Object.keys(ca), ...Object.keys(cb)]);
  for (const k of keys) if (JSON.stringify(ca[k] ?? null) !== JSON.stringify(cb[k] ?? null)) return true;
  return false;
}

/**
 * The spec fields that decide where bound credentials are presented
 * (D400 integrationDestinationFields, cmd/kudurru): changing one while
 * credentials are bound needs integrations.secrets, like changing a
 * reference.
 */
export const DESTINATION_FIELDS = ['spec.type', 'spec.unifi.endpoint', 'spec.unifi.tlsFingerprint', 'spec.webhook.endpoint'];

function at(spec: IntegrationSpec | undefined, path: string): unknown {
  let v: unknown = spec;
  for (const part of path.split('.').slice(1)) v = v && typeof v === 'object' ? (v as Record<string, unknown>)[part] : undefined;
  return v;
}

/** Whether a write from `stored` to `next` needs integrations.secrets besides integrations.write. */
export function needsSecrets(stored: IntegrationSpec | undefined, next: IntegrationSpec): boolean {
  if (credentialsChanged(stored, next)) return true;
  const bound = Object.keys(stored?.credentials ?? {}).length > 0 || Object.keys(next.credentials ?? {}).length > 0;
  return bound && DESTINATION_FIELDS.some((p) => JSON.stringify(at(stored, p) ?? null) !== JSON.stringify(at(next, p) ?? null));
}

/** Whether a top-level provider field decides where credentials go (it is read-only without integrations.secrets). */
export function isDestinationField(m: IntegrationManifest, field: string): boolean {
  return DESTINATION_FIELDS.includes(`spec.${m.configField}.${field}`);
}

/**
 * cmd/kudurru integrationPublishes, by provider block: a Cloudflare block
 * always, a webhook that names domains, a UniFi instance that mirrors
 * records as DNS policies (a reviewed-only publisher, D401).
 * lib/integrations.test.ts compares it with the Go rule.
 */
export const PUBLISH_RULES: Readonly<Record<string, (block: Record<string, unknown>) => boolean>> = {
  cloudflare: () => true,
  webhook: (b) => Array.isArray(b.domains) && b.domains.length > 0,
  unifi: (b) => valueAt(b, ['push', 'mirrorRecords', 'enabled']) === true,
};

/**
 * Whether a sync of this instance publishes records, so it must be bound
 * to a reviewed preview digest (mode publish); otherwise it observes (mode
 * observe, no digest). The server's rule, by provider block (PUBLISH_RULES).
 */
export function publishes(m: IntegrationManifest | undefined, spec: IntegrationSpec | undefined): boolean {
  if (!m || !spec) return false;
  const block = spec[m.configField];
  if (!block || typeof block !== 'object' || Array.isArray(block)) return false;
  return PUBLISH_RULES[m.configField]?.(block as Record<string, unknown>) ?? false;
}

/** The manifest of a type, when this build has one. */
export function manifestFor(ms: IntegrationManifests | undefined, type: string): IntegrationManifest | undefined {
  return ms?.items.find((m) => m.type === type);
}

/** Why an instance of this manifest does not run on this node, or undefined. */
export function unsupportedReason(m: IntegrationManifest): string | undefined {
  if (m.supported) return undefined;
  return (m.reason && REASON_TEXT[m.reason]) || `Refused by this node${m.reason ? ` (${m.reason})` : ''}.`;
}

export function integrationHref(namespace: string, name: string): string {
  return `/integrations/${encodeURIComponent(namespace)}/${encodeURIComponent(name)}`;
}

/** The badge of an instance's state. */
export function phaseVariant(s: Pick<IntegrationSummary, 'phase' | 'statusKnown' | 'terminating'>): 'success' | 'warning' | 'error' | 'neutral' | 'info' {
  if (s.terminating) return 'warning';
  if (!s.statusKnown) return 'neutral';
  switch (s.phase) {
    case 'Ready':
    case 'Running':
      return 'success';
    case 'Degraded':
    case 'Paused':
    case 'Terminating':
      return 'warning';
    case 'Failed':
    case 'Refused':
      return 'error';
    default:
      return 'neutral';
  }
}

export type IntegrationCommand = 'check' | 'preview' | 'sync' | 'act' | 'actions' | 'release';

/**
 * A sentence for a refused or failed integration command: a stale review
 * (409) says what it is, an uncertain effect (503 integration_uncertain)
 * says it was not replayed. Other errors keep their own description.
 */
export function commandErrorText(err: unknown, cmd: IntegrationCommand): string | null {
  if (!(err instanceof ApiError)) return null;
  if (err.key === 'integration_uncertain') {
    return 'The provider did not acknowledge the operation: some of it may have taken effect. It was not sent again; check the provider and the instance status before you start another one.';
  }
  if (err.status === 409) {
    if (cmd === 'sync') return 'The server refused the sync: the preview expired or no longer matches the instance, its sources or the provider, or this node lost its authority. Nothing was published. Preview again and review the new plan.';
    if (cmd === 'release') return 'The server refused the release: the Integration is not terminating any more, or it changed since you reviewed it. Reload and review it again.';
    return 'The server refused it: the Integration changed since you reviewed it, it is terminating, or this node lost its authority. Reload and try again.';
  }
  if (err.status === 501) return 'This provider does not offer that operation.';
  return null;
}
