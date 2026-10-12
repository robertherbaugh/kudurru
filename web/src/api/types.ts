// Wire types for /api/v1 (docs/architecture.md §10).
//
// Routes served by huma alias the generated schemas of api/openapi.json
// (`bun run gen:api` writes src/api/generated.ts); the aliases narrow a
// few `string` fields to their enums. Objects mirror api/v1alpha1 and the
// stats and query-log types internal/querylog/sqlitelog, field for field.
// The forwarded /auth/* and /iam/* routes are hand-written from
// internal/auth/stack (routes.go, iam.go) because their OpenAPI
// documentation did not match the handlers; regenerate and alias them once
// it does. Nothing outside this file names a generated schema.
import type { components } from './generated';

type S = components['schemas'];

// ---------------------------------------------------------------------------
// Envelopes and errors

/** Every error body: {error:{key,message,hint}, took_ms}. */
export interface ApiErrorBody {
  error: {
    key: string;
    message: string;
    hint?: string | MissingPermission | Record<string, unknown>;
    /** The paths a 400 invalid failed on (settings: "upstreams.groups.corp.strategy"). */
    fields?: string[];
  };
  took_ms?: number;
}

/** The hint of a 403 "forbidden" error. */
export interface MissingPermission {
  missing: { resource: string; verb: string; scope: string };
}

/** List envelope: {items, continue, took_ms}. */
export interface ListEnvelope<T> {
  items: T[];
  continue?: string;
  took_ms?: number;
}

/** Multi-node scope stamped on every stats and query-log response. */
export interface ScopeInfo {
  scope: 'node' | 'cluster';
  nodes: { name: string; ok: boolean; took_ms: number; error?: string }[];
  partial: boolean;
}

// ---------------------------------------------------------------------------
// Objects (api/v1alpha1)

export interface ObjectMeta {
  name: string;
  namespace?: string;
  uid?: string;
  resourceVersion?: string;
  generation?: number;
  creationTimestamp?: string;
  labels?: Record<string, string>;
  annotations?: Record<string, string>;
}

export interface Condition {
  type: string;
  status: 'True' | 'False' | 'Unknown';
  reason?: string;
  message?: string;
  lastTransitionTime?: string;
  observedGeneration?: number;
}

export interface KObject<K extends string, S, St> {
  apiVersion: 'kudurru.io/v1alpha1';
  kind: K;
  metadata: ObjectMeta;
  spec: S;
  status?: St;
}

export type ZoneType = 'primary' | 'forward' | 'secondary' | 'stub' | 'external' | 'catalog' | 'secondaryCatalog';
export type AccessMode =
  | 'deny'
  | 'allow'
  | 'allowOnlyPrivate'
  | 'allowOnlyZoneNameServers'
  | 'useNetworkACL'
  | 'zoneNameServersAndACL';
export type NotifyMode = 'none' | 'zoneNameServers' | 'specified' | 'both';

export interface DNSZoneSpec {
  zoneName: string;
  type: ZoneType;
  soa?: { mname?: string; rname?: string; refresh?: number; retry?: number; expire?: number; minimum?: number };
  nameservers?: string[];
  defaultTTL?: number;
  policy?: {
    queryAccess?: AccessMode;
    queryACL?: string[];
    zoneTransfer?: AccessMode;
    transferACL?: string[];
    transferTSIGKeyRefs?: string[];
    notify?: NotifyMode;
    notifyTargets?: string[];
    update?: AccessMode;
  };
  forward?: { upstreamGroup?: string; dnssecValidation?: boolean };
  secondary?: { primaries: string[]; tsigKeyRef?: string; transferProtocol?: 'tcp' | 'tls' };
  stub?: { nameservers: string[] };
  recordSetSelector?: { namespaces?: string[] };
}

export interface DNSZoneStatus {
  observedGeneration?: number;
  serial?: number;
  contentHash?: string;
  recordCount?: number;
  nameserverAddresses?: string[];
  conditions?: Condition[];
}

export type DNSZone = KObject<'DNSZone', DNSZoneSpec, DNSZoneStatus>;

/** kudurru extension block of an Endpoint (D28). */
export interface EndpointExt {
  comment?: string;
  disabled?: boolean;
  expiresAt?: string;
  view?: string;
}

/** external-dns Endpoint superset. */
export interface Endpoint {
  dnsName: string;
  recordType: string;
  targets: string[];
  recordTTL?: number;
  setIdentifier?: string;
  labels?: Record<string, string>;
  providerSpecific?: { name: string; value: string }[];
  kudurru?: EndpointExt;
}

export interface DNSRecordSetSpec {
  zoneRef: { name: string; namespace?: string };
  endpoints: Endpoint[];
}

export type DNSRecordSet = KObject<
  'DNSRecordSet',
  DNSRecordSetSpec,
  { observedGeneration?: number; names?: number; conditions?: Condition[] }
>;

export type ListFormat = 'auto' | 'hosts' | 'domains' | 'adblock' | 'regex';
/** blockPage answers A/AAAA with the block-page address (ux-plan C5). */
export type BlockResponse = 'nxdomain' | 'zeroIP' | 'refused' | 'customIP' | 'blockPage';

export interface ListSource {
  url?: string;
  inline?: string[];
  configMapRef?: { name: string; namespace?: string };
  format?: ListFormat;
  name?: string;
  /** Hosts and plain-domain entries also match their subdomains. */
  subdomains?: boolean;
  /** false: not fetched or compiled, cache kept (ux-plan C2); omitted = true. */
  enabled?: boolean;
  /** Structured entries (the dashboard's allow and deny lists ui-*); exclusive with url, inline and configMapRef. */
  entries?: ListSourceEntry[];
  /** Normalised rules of this url, configMap or inline source that are switched off. */
  exclude?: string[];
}

export interface BlockListSpec {
  mode?: 'block' | 'allow';
  sources: ListSource[];
  refreshInterval?: string;
  response?: BlockResponse;
  customIP?: { v4?: string; v6?: string };
}

export interface SourceStatus {
  name: string;
  digest?: string;
  etag?: string;
  entries?: number;
  invalid?: number;
  lastFetched?: string;
  error?: string;
  /** false for a disabled source (ux-plan C2). */
  enabled?: boolean;
  /** Entries of the source switched off (exclude[] hits, or entries[] with enabled: false). */
  excluded?: number;
  /** exclude[] values that match no entry of the current body (at most 50). */
  staleExcludes?: string[];
}

export interface BlockListStatus {
  observedGeneration?: number;
  entries?: number;
  ruleCount?: number;
  invalid?: number;
  /** Valid rules parsed but not applied ($important, $badfilter, mid-pattern wildcards, hosts rewrites). */
  unsupported?: number;
  lastFetched?: string;
  sources?: SourceStatus[];
  conditions?: Condition[];
}

export type BlockList = KObject<'BlockList', BlockListSpec, BlockListStatus>;

export interface ClientMatcher {
  ip?: string;
  cidr?: string;
  mac?: string;
  clientID?: string;
  hostname?: string;
  nameGlob?: string;
  name?: string;
  comment?: string;
}

export type ClientGroup = KObject<
  'ClientGroup',
  { clients?: ClientMatcher[]; sources?: { integrationRef?: { name: string; namespace?: string } } },
  { observedGeneration?: number; members?: number; conditions?: Condition[] }
>;

export interface PolicySpec {
  clientGroupRefs?: string[];
  blockListRefs?: string[];
  allowListRefs?: string[];
  allowOnly?: boolean;
  schedule?: { weekdays?: string[]; start: string; end: string; tz?: string };
  upstreamGroup?: string;
  blockMode?: BlockResponse;
  blockTTL?: string;
  queryLog?: boolean;
  priority?: number;
  rewrites?: { name: string; type?: string; targets?: string[]; cname?: string }[];
  /** Catalogue service IDs (GET /catalogue/services) blocked like a list. */
  blockedServices?: string[];
  /** Catalogue category IDs (GET /catalogue/categories): every service in them, including ones added later. */
  blockedCategories?: string[];
  /** Exceptions to blockedCategories (services of a blocked category this policy does not block). */
  allowedServices?: string[];
  safeSearch?: boolean;
  /** strict (default) or moderate; only YouTube has two levels. */
  safeSearchMode?: 'strict' | 'moderate';
}

export type Policy = KObject<'Policy', PolicySpec, { observedGeneration?: number; conditions?: Condition[] }>;

export type AnyObject = DNSZone | DNSRecordSet | BlockList | ClientGroup | Policy;

/** Lower-case plural resource names used by /objects/{kind}. */
export type ObjectKind = 'dnszones' | 'dnsrecordsets' | 'blocklists' | 'clientgroups' | 'policies';

/** Per-document result of POST /apply. */
export interface ApplyResult {
  kind: string;
  namespace?: string;
  name: string;
  result: 'created' | 'updated' | 'unchanged' | 'error';
  error?: string;
}

/** {processed:{success,errors}} returned by multi-add and batch routes. */
export interface Processed {
  processed: {
    success: { item: string }[];
    errors: { item: string; error: string }[];
  };
  took_ms?: number;
}

// ---------------------------------------------------------------------------
// Convenience views

/** GET /zones row and GET /zones/{zone} (internal/api zones.go). */
export type ZoneSummary = Omit<S['ZoneSummary'], 'type'> & {
  type: ZoneType;
  /** Transfer and NOTIFY state; not served yet, so the panel stays hidden. */
  transfer?: ZoneTransferStatus;
};

export interface ZoneTransferStatus {
  zoneTransfer: AccessMode;
  notify: NotifyMode;
  lastNotify?: { at: string; targets: string[]; ok: boolean; error?: string };
  lastTransfer?: { at: string; peer: string; kind: 'AXFR' | 'IXFR'; ok: boolean };
}

/** GET /zones/{zone}/records row: one flattened endpoint. */
export interface ZoneRecord {
  recordSet: string;
  namespace?: string;
  dnsName: string;
  recordType: string;
  targets: string[];
  recordTTL?: number;
  setIdentifier?: string;
  kudurru?: EndpointExt;
  managedBy?: string;
}

/** Result of POST /zones/{zone}/import (all or nothing: a bad line is a 400). */
export type ZoneImportResult = S['ImportResult'];

// ---------------------------------------------------------------------------
// Auth (forwarded to internal/auth/stack; hand-written, see the header)

export type PrincipalKind = 'User' | 'ServiceAccount' | 'KubernetesServiceAccount' | 'LocalRoot' | 'Node';

/** GET /auth/me, and the answer of POST /auth/login and /auth/password. */
export interface Me {
  principal: {
    kind: PrincipalKind;
    id: string;
    name: string;
    groups: string[];
    /** How the request authenticated (cookie, token, ...). */
    auth?: string;
    authMethod?: string;
    provider?: string;
  };
  /** Effective grants in wire form ("admin", "zone-admin@zone:home-example-com"). */
  roles: string[];
  /** Cookie sessions echo it in X-Kudurru-CSRF. */
  csrf?: string;
  /** resource → verbs granted at cluster scope. */
  permissions: Record<string, string[]>;
  /** scope ("zone:home-example-com") → resource → verbs. */
  scopedPermissions?: Record<string, Record<string, string[]>>;
  /** A local user still on a generated or reset password. */
  mustChangePassword?: boolean;
  expiresAt?: string;
  idleExpiresAt?: string;
}

/** GET /auth/status (stack StatusResponse). */
export interface AuthStatus {
  /** False only when auth.localLogin is disabled. */
  localLogin: boolean;
  localLoginMode?: 'enabled' | 'admins-only' | 'disabled';
  providers: { id: string; displayName: string }[] | null;
  hasDefaultCredentials: boolean;
  session?: { idleTimeout: string; lifetime: string };
}

/** Who owns a token (api/internalkinds TokenOwner). */
export interface TokenOwner {
  kind: 'User' | 'ServiceAccount';
  name: string;
  namespace?: string;
  id?: string;
  provider?: string;
}

/** GET /auth/tokens row (stack TokenInfo); never the secret. */
export interface ApiToken {
  id: string;
  description?: string;
  owner: TokenOwner;
  createdAt: string;
  /** null: the token never expires. */
  expiresAt: string | null;
  lastUsedAt?: string;
  lastUsedIP?: string;
  scopes?: string[];
}

/** Body of POST /auth/tokens (stack CreateTokenRequest; unknown fields are rejected). */
export interface CreateTokenRequest {
  description?: string;
  /** A Go duration ("720h"); empty means auth.tokens.maxLifetime or never. */
  expiresIn?: string;
  /** Grants in wire form ("viewer", "zone-admin@zone:home-example-com"). */
  scopes?: string[];
}

export interface CreatedToken extends ApiToken {
  /** The full kdr_<id>_<secret>; only returned once. */
  token: string;
}

/** Body of POST /auth/password (stack PasswordRequest). */
export interface PasswordChangeRequest {
  currentPassword: string;
  newPassword: string;
}

// ---------------------------------------------------------------------------
// Runtime

/** GET|POST /dns/blocking (internal/api runtime.go). */
export interface BlockingState {
  blocking: boolean;
  /** Seconds left until blocking re-enables; absent when indefinite. */
  timer?: number;
  /** When blocking re-enables (RFC 3339). */
  until?: string;
  /** blocking.enabled from the settings; when false the toggle has no effect. */
  configured?: boolean;
  scope?: 'node' | 'cluster';
  node?: string;
}

export type DnsProto = NonNullable<S['DNSQueryRequest']['proto']>;
export type DnsQueryRequest = S['DNSQueryRequest'];
export type DnsFlags = S['DNSFlags'];

/** POST /dns/query: the sections are presentation lines (name ttl class type rdata, tab-separated). */
export type DnsQueryResult = Omit<S['DNSQueryResult'], 'status_class'> & { status_class: StatusClass };

// ---------------------------------------------------------------------------
// Stats (sqlitelog api.go)

export interface Window {
  from: string;
  until: string;
}

export interface Summary extends ScopeInfo, Window {
  queries: {
    total: number;
    blocked: number;
    percent_blocked: number;
    cached: number;
    forwarded: number;
    authoritative: number;
    dropped: number;
    other: number;
    unique_domains: number;
    qps_1m: number;
    qps_avg: number;
  };
  clients: { active: number; total: number };
  domains_on_lists: number;
  rcodes: Record<string, number>;
  protocols: Record<string, number>;
  types: Record<string, number>;
  latency: { avg_ms: number; p50_ms: number; p95_ms: number; p99_ms: number };
  approximate: boolean;
}

export interface TimeseriesPoint {
  ts: string;
  total: number;
  blocked: number;
  cached: number;
  forwarded: number;
  authoritative: number;
  other: number;
}

export interface Timeseries extends ScopeInfo, Window {
  step: number;
  points: TimeseriesPoint[];
  approximate: boolean;
}

export type TopDim = 'domains' | 'clients' | 'upstreams' | 'lists' | 'zones';

export interface TopItem {
  key: string;
  name?: string;
  group?: string;
  count: number;
  total: number;
  blocked: number;
  percent: number;
}

export interface TopResult extends ScopeInfo, Window {
  dim: TopDim;
  blocked: boolean;
  items: TopItem[];
  total_in_window: number;
  approximate: boolean;
  cutoff_rank?: number;
}

export interface UpstreamItem {
  addr: string;
  total: number;
  errors: number;
  timeouts: number;
  percent: number;
  avg_ms: number;
  p95_ms: number;
}

/** Health of one upstream server right now (GET /stats/upstreams servers[]). */
export type UpstreamServer = S['UpstreamServer'];

export interface UpstreamsResult extends ScopeInfo, Window {
  upstreams: UpstreamItem[] | null;
  /** Live health per configured server (not windowed). */
  servers: UpstreamServer[];
  approximate: boolean;
}

export interface ClientTimeseries extends ScopeInfo, Window {
  step: number;
  clients: { key: string; name?: string }[];
  /** counts[i] belongs to clients[i]; the last element is "other". */
  points: { ts: string; counts: number[] }[];
  approximate: boolean;
}

export interface BlockedItem {
  id: number;
  ts: string;
  domain: string;
  client: string;
  client_name?: string;
  list?: string;
  status: string;
}

export interface RecentBlocked extends ScopeInfo {
  items: BlockedItem[];
}

// ---------------------------------------------------------------------------
// Query log

export type StatusClass = 'permitted' | 'cached' | 'blocked' | 'error';

export interface QueryFlags {
  RD: boolean;
  DO: boolean;
  TC: boolean;
  AD: boolean;
  CD: boolean;
  Cached: boolean;
  Stale: boolean;
  Prefetch: boolean;
  ECS: boolean;
  Traceparent: boolean;
}

export interface QueryRow {
  id: number;
  ts: string;
  node: string;
  proto: string;
  client: { ip: string; name?: string; group?: string };
  domain: string;
  cname?: string;
  qtype: string;
  qclass: number;
  status: string;
  status_class: StatusClass;
  rcode: string;
  reply: { type: string; time_ms?: number; answer?: string };
  upstream?: string;
  zone?: string;
  list?: string;
  rule?: string;
  /** The deciding policy, "namespace/name" (ux-plan C3). */
  policy?: string;
  dnssec: string;
  ede?: number;
  ecs?: string;
  trace_id?: string;
  flags: QueryFlags;
}

export interface QueryFilter {
  client?: string;
  domain?: string;
  status?: string;
  status_class?: StatusClass | '';
  qtype?: string;
  upstream?: string;
  list?: string;
  zone?: string;
  /** The deciding policy, "namespace/name". */
  policy?: string;
  since?: string;
  until?: string;
  cursor?: string;
  limit?: number;
}

export interface QueriesPage extends ScopeInfo {
  queries: QueryRow[];
  cursor_next?: string;
  records_total: number;
  records_filtered: number;
  earliest_ts_disk?: string;
  earliest_ts_memory?: string;
}

export type SuggestionField = 'domain' | 'client' | 'upstream' | 'list' | 'zone';

export interface Suggestions {
  field: SuggestionField;
  values: string[];
}

export interface QuerylogStatus {
  rows: number;
  db_bytes: number;
  wal_bytes: number;
  oldest_ts?: string;
  privacy: string;
  retention: { raw_seconds: number; max_db_bytes: number; rollup_1m: string; rollup_1h: string; rollup_1d: string };
  subscribers: number;
}

// ---------------------------------------------------------------------------
// Settings and nodes

export type SettingType = 'string' | 'int' | 'bool' | 'duration' | 'bytes' | 'string[]' | 'list' | 'map';

/** One row of GET /settings/_schema (internal/config.Key plus the effective value). */
export type SettingKey = Omit<S['SettingKey'], 'type' | 'value'> & {
  type: SettingType;
  /** The effective value ("<redacted>" for writeOnly keys). */
  value?: unknown;
};

/** C1 value formats of string keys (and string[] items). */
export type SettingFormat = NonNullable<S['SettingShape']['format']>;

/** C1: the kinds of object a value may name. */
export type SettingRef = NonNullable<S['SettingShape']['ref']>;

/** C1: one allowed value of a closed set. */
export type SettingEnumOption = S['SettingOption'];

/** C1: the shape of a value (a list item, a map key or value). */
export type SettingShape = S['SettingShape'];

/** C1: one field of an object shape (with label, help, required, default). */
export type SettingShapeField = S['SettingField'];

/** C1 settings metadata of a key (GET /settings/_schema); every field is optional. */
export type SettingMeta = Pick<S['SettingKey'], 'enum' | 'min' | 'max' | 'presets' | 'format' | 'ref' | 'items' | 'keys' | 'values' | 'fields' | 'group' | 'label'>;

/** GET|PATCH /settings and /node/config: the keys of one scope under `settings`. */
export type SettingsBody = S['SettingsBody'];

/** GET /nodes row. `ready` is absent for peers until scatter-gather lands. */
export type NodeInfo = S['NodeInfo'];

/** One component of the /healthz document (internal/health Component). */
export interface HealthComponent {
  ready: boolean;
  message?: string;
  since?: string;
}

/** The /healthz document of a node. */
export interface HealthDoc {
  status?: string;
  components?: Record<string, HealthComponent>;
  unready?: string[] | null;
  version?: string;
  node?: string;
  [key: string]: unknown;
}

/** GET /nodes/{name}/health. */
export type NodeHealth = Omit<S['NodeHealth'], 'health'> & { health: HealthDoc };

/** GET /cluster: store driver, leader, epoch and shape. */
export type ClusterInfo = S['ClusterInfo'];

/** GET /edge: the dnsdist and keepalived supervisors and the dnsdist backends. */
export type EdgeInfo = S['EdgeInfo'];
export type EdgeComponentState = S['EdgeComponent'];

/** GET /info/messages row. `object` is "Kind/namespace/name" or "Kind/name". */
export type DiagnosisMessage = S['DiagnosisMessage'];

export type VersionInfo = S['VersionOutputBody'];

/** GET /clients row: the top-clients ranking of the window. */
export type ClientItem = S['ClientItem'] & {
  /** Last query of the client (RFC 3339), when the server knows it. */
  last_seen?: string;
};

/** One audit event (internal/audit Event). */
export type AuditEvent = S['Event'];

/** GET /audit: a page of events with a keyset cursor. */
export type AuditPage = S['AuditPage'];

export interface AuditQuery {
  kind?: string | undefined;
  name?: string | undefined;
  actor?: string | undefined;
  since?: string | undefined;
  limit?: number;
  /** The `continue` (or `cursor_next`) of the previous page. */
  cursor?: string | undefined;
  /** desc lists newest first; the server default is oldest first. */
  order?: 'asc' | 'desc';
}

/** One hit of GET /blocklists/_search?domain=; `list` is "namespace/name". */
export type ListSearchHit = Omit<S['ListSearchHit'], 'mode'> & { mode: 'block' | 'allow' };

/** DELETE /querylog. */
export type PurgeResult = S['PurgeResult'];

/** GET /doctor: the server-side half of `kudurru doctor` as this node sees it. */
export type DoctorCheck = S['DoctorCheck'];
export type DoctorReport = S['DoctorReport'];

// ---------------------------------------------------------------------------
// IAM (read-only in v0.1; the write API is v0.2). Hand-written from
// internal/auth/stack/iam.go and internal/auth/adminsock (see the header).

/** A local user (adminsock User). */
export interface IamUser {
  name: string;
  displayName?: string;
  builtinAdmin: boolean;
  disabled: boolean;
  mustChangePassword: boolean;
  groups?: string[];
  lastLoginAt?: string;
  passwordChangedAt?: string;
}

/** A kudurru service account (stack ServiceAccountInfo). */
export interface IamServiceAccount {
  namespace: string;
  name: string;
  description?: string;
  disabled: boolean;
  builtinAdmin: boolean;
  /** Number of API tokens it holds. */
  tokens: number;
}

/** An RBAC scope: cluster, or one zone ("zone" + name). */
export interface RbacScope {
  kind: 'cluster' | 'zone' | 'zonegroup' | 'clientgroup' | '';
  name?: string;
}

export interface RbacSubject {
  kind: 'User' | 'Group' | 'ServiceAccount';
  name: string;
  namespace?: string;
}

/** A role binding from the config file (source "config") or a RoleBinding object (stack RoleBindingInfo). */
export interface IamRoleBinding {
  source: 'config' | 'object';
  namespace?: string;
  name: string;
  role: string;
  scope: RbacScope;
  subjects: RbacSubject[] | null;
  managedBy?: string;
}

/** A configured OIDC provider, without secrets (stack IdentityProviderInfo). */
export interface IamIdentityProvider {
  id: string;
  displayName: string;
  issuer: string;
  clientID: string;
  cliClientID?: string;
  scopes?: string[] | null;
  redirectURL?: string;
  source?: 'config' | 'object';
}

/** The IAM list envelope; diagnoses explain ignored bindings or providers. */
export interface IamList<T> extends ListEnvelope<T> {
  diagnoses?: string[];
}

// Blocked-services and safe-search catalogue (D60) --------------------------

/** One blockable service of GET /catalogue/services. */
export interface CatalogueService {
  id: string;
  name: string;
  /** The category ID (GET /catalogue/categories). */
  category: string;
  /** kudurru, or adguard for a service of the opt-in AdGuard import (GPL-3.0). */
  origin?: 'kudurru' | 'adguard';
  /** SPDX licence of the entry. */
  license?: string;
  /** Services this one also needs: block them too to stop it (Fortnite requires epicgames). */
  requires?: string[];
  /** Services of the same vendor or closely tied to it. */
  related?: string[];
  /** A caveat about blocking the service. */
  note?: string;
  /** The list name the query log records, "service:<id>". */
  list: string;
  rules: number;
  domains: string[];
  source: string;
  /** API path of the service's logo (/api/v1/catalogue/icons/{slug}.svg); absent: no logo, show a monogram. */
  icon?: string;
  /** The logo for dark backgrounds, when the default one is dark artwork. */
  iconOnDark?: string;
}

/** One safe-search engine of GET /catalogue/safesearch. */
export interface SafeSearchEngine {
  id: string;
  name: string;
  /** The list name the query log records, "safesearch:<id>". */
  list: string;
  target?: string;
  moderateTarget?: string;
  addresses?: string[];
  domains: number;
  source: string;
  /** API path of the engine's logo; absent when the binary has none. */
  icon?: string;
  iconOnDark?: string;
}

/** One category of GET /catalogue/categories. */
export type CatalogueCategory = S['CatalogueCategory'];

/** One active source of services (GET /catalogue sources). */
export type CatalogueSourceInfo = Omit<S['CatalogueSourceInfo'], 'refresh'> & { refresh?: CatalogueInfo['refresh'] };

/** GET /catalogue. */
export interface CatalogueInfo {
  schemaVersion: number;
  version: string;
  license: string;
  homepage?: string;
  origin: string;
  digest: string;
  services: number;
  categories?: number;
  safeSearchEngines: number;
  refresh?: { url: string; lastAttempt?: string; lastSuccess?: string; lastError?: string };
  /** The active sources of services and their licences (kudurru always, AdGuard when opted in). */
  sources?: CatalogueSourceInfo[];
}

/** GET /catalogue/icons (C4): the icons this binary embeds. */
export type CatalogueIconIndex = ListEnvelope<S['CatalogueIcon']>;

// Policy current state (ux-plan C3) ------------------------------------------

/**
 * GET /policies/{namespace}/{name}/state. Refs are "namespace/name";
 * shadowedBy ignores schedules; stats24h and groups[].seen24h are absent
 * without a query log.
 */
export type PolicyState = Omit<S['PolicyState'], 'took_ms' | 'conditions' | 'shadowedBy'> & {
  conditions: Condition[];
  shadowedBy?: string[] | null;
  took_ms?: number;
};

// Unblock requests (the block page's "ask to allow") ------------------------

/** One request from the block page (GET /unblockrequests/{namespace}/{name}). */
export type UnblockRequest = S['UnblockRequest'];
export type UnblockState = UnblockRequest['state'];
/** How the client was recorded under querylog.privacy. */
export type UnblockClientPrivacy = UnblockRequest['clientPrivacy'];
/** Why the name was blocked; list is "ns/name", "service:<id>" or "safesearch:<engine>". */
export type UnblockBlock = S['UnblockBlock'];
/** The allow entry an approval created. */
export type UnblockAllow = S['UnblockAllow'];
/** One earlier decision of a request asked again. */
export type UnblockDecision = S['UnblockDecision'];
export type UnblockCounts = S['UnblockCounts'];
/** GET /unblockrequests: newest first; counts cover every request. */
export type UnblockRequestList = S['UnblockRequestList'];
/** POST …/approve; an empty duration is permanent. */
export type UnblockApproval = S['UnblockApproval'];
export type UnblockScope = NonNullable<UnblockApproval['scope']>;
export type UnblockMatch = NonNullable<UnblockApproval['match']>;
/** POST …/deny. */
export type UnblockDenial = S['UnblockDenial'];
/** The answer of approve and deny; skipped policies are managed elsewhere. */
export type UnblockDecisionResult = S['UnblockDecisionResult'];

// Allowlist, Denylist and the per-source entries browser ---------------------
//
// The routes alias the generated schemas (api/openapi.json); the BlockList
// fields they work on (spec.sources[].entries and exclude, status
// excluded and staleExcludes) are hand-written above with the object.

export type DomainAction = S['DomainEntry']['action'];
/** exact "ads.example.com", wildcard "*.example.com" (the name and every subdomain), regex RE2 without slashes. */
export type DomainEntryType = S['DomainEntry']['type'];
export type DomainOrigin = S['DomainEntry']['origin'];

/** One structured entry of a source (spec.sources[].entries, the ui-* lists; api/v1alpha1 ListEntry). */
export interface ListSourceEntry {
  value: string;
  type?: DomainEntryType;
  /** Omitted = true. */
  enabled?: boolean;
  comment?: string;
  created?: string;
  /** Actor "kind:id", e.g. "User:operator", "system". */
  createdBy?: string;
  modified?: string;
  origin?: DomainOrigin;
  /** UnblockRequest "ns/name" when origin is unblock. */
  request?: string;
}

/** One entry of GET /blocklists/{ns}/{name}/sources/{source}/entries (inline: the line; url/configMap: the normalised rule; entries: the rendered rule). */
export type ListEntry = S['SourceEntry'];

export type SourceEntriesKind = S['SourceEntries']['kind'];

/** GET …/sources/{source}/entries (503 unavailable while this node has no cached body). */
export type SourceEntriesPage = Omit<S['SourceEntries'], 'took_ms'> & { took_ms?: number };

export interface SourceEntriesQuery {
  q?: string;
  enabled?: 'true' | 'false';
  limit?: number;
  continue?: string;
}

/** Body of PATCH …/sources/{source}/entries: add/remove for inline sources, enable/disable for every kind. */
export type ListEntriesPatch = { [K in keyof S['SourceEntriesPatch']]?: string[] };

/** Answer of PATCH …/entries; servers before the allowlist stream answered processed{success,errors}. */
export type ListEntriesPatchResult = Partial<S['SourceEntriesResult']> & { processed?: Processed['processed'] };

/** Where one copy of a logical entry lives. */
export type DomainEntryList = S['DomainEntryList'];

/** One logical allow or deny entry (namespace, action, type, value) across its lists. */
export type DomainEntry = S['DomainEntry'];

/** One list behind the entries of GET /allowlist, /denylist or /domains. */
export type DomainListInfo = S['DomainListInfo'];

/** A policy that should reference a list but is managed elsewhere. */
export type DomainUnattached = S['DomainUnattached'];

export type DomainEntriesPage = Omit<S['DomainEntries'], 'took_ms'> & { took_ms?: number };

export interface DomainEntriesQuery {
  namespace?: string;
  /** /domains only. */
  action?: DomainAction;
  type?: DomainEntryType;
  enabled?: 'true' | 'false';
  /** Only entries, or only list exceptions (entries switched off in one list). */
  kind?: 'entry' | 'exception';
  /** "everyone", a group "ns/name", or "list:ns/name" for the exceptions of one list. */
  scope?: string;
  /** Substring of the value or comment (and of the list name for exceptions). */
  q?: string;
  limit?: number;
  continue?: string;
}

/** POST /allowlist|/denylist: 1..1000 values of one type; no groups = everyone. */
export type DomainEntriesCreate = Omit<S['DomainEntriesCreate'], 'values' | 'groups'> & { values: string[]; groups?: string[] };

export type DomainEntryPatch = Omit<S['DomainEntryPatch'], 'groups'> & { groups?: string[] };

export type DomainBatch = Omit<S['DomainEntriesBatch'], 'ids'> & { ids: string[] };

/** The answer of every write; skipped: policies that should reference a list but are managed elsewhere. */
export type DomainWriteResult = Omit<S['DomainWriteResult'], 'took_ms'> & { took_ms?: number };
