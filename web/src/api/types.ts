// Hand-written wire types for /api/v1 (docs/architecture.md §10).
//
// They mirror api/v1alpha1 (objects) and internal/querylog/sqlitelog
// (stats, query log) field for field. When api/openapi.json lands,
// `bun run gen:api` writes src/api/generated.ts and each type below becomes
// an alias of `components['schemas'][...]`; nothing outside this file names
// a schema directly, so that swap is mechanical.

// ---------------------------------------------------------------------------
// Envelopes and errors

/** Every error body: {error:{key,message,hint}, took_ms}. */
export interface ApiErrorBody {
  error: {
    key: string;
    message: string;
    hint?: string | MissingPermission | Record<string, unknown>;
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
export type BlockResponse = 'nxdomain' | 'zeroIP' | 'refused' | 'customIP';

export interface ListSource {
  url?: string;
  inline?: string[];
  configMapRef?: { name: string; namespace?: string };
  format?: ListFormat;
  name?: string;
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
}

export interface BlockListStatus {
  observedGeneration?: number;
  entries?: number;
  ruleCount?: number;
  invalid?: number;
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
  safeSearch?: boolean;
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

/** GET /zones row. */
export interface ZoneSummary {
  name: string;
  zoneName: string;
  type: ZoneType;
  serial: number;
  recordCount: number;
  ready: boolean;
  managedBy: string;
  /** Transfer and NOTIFY state; optional until the API exposes it. */
  transfer?: ZoneTransferStatus;
}

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

// ---------------------------------------------------------------------------
// Auth

export interface Principal {
  kind: 'User' | 'ServiceAccount' | 'KubernetesServiceAccount' | 'LocalRoot' | 'Node';
  id: string;
  name: string;
  groups?: string[];
}

export interface Me {
  principal: Principal;
  roles: string[];
  csrf: string;
  /** resource → verbs granted at cluster scope. */
  permissions: Record<string, string[]>;
  mustChangePassword?: boolean;
}

export interface AuthStatus {
  localLogin: 'enabled' | 'admins-only' | 'disabled' | boolean;
  providers: { id: string; displayName: string }[];
  hasDefaultCredentials: boolean;
}

export interface ApiToken {
  id: string;
  name: string;
  createdAt: string;
  expiresAt?: string;
  lastUsedAt?: string;
  lastUsedIP?: string;
  scopes?: string[];
}

export interface CreatedToken extends ApiToken {
  /** The full kdr_<id>_<secret>; only returned once. */
  token: string;
}

// ---------------------------------------------------------------------------
// Runtime

export interface BlockingState {
  blocking: boolean;
  /** Seconds left until blocking re-enables; absent when indefinite. */
  timer?: number;
  /** When blocking re-enables (RFC 3339). */
  until?: string;
}

export interface DnsQueryRequest {
  name: string;
  type: string;
  client?: string;
}

export interface DnsRR {
  name: string;
  type: string;
  ttl: number;
  class?: string;
  data: string;
}

export interface DnsQueryResult {
  rcode: string;
  status: string;
  status_class: StatusClass;
  answer: DnsRR[];
  authority: DnsRR[];
  additional: DnsRR[];
  upstream?: string;
  list?: string;
  rule?: string;
  zone?: string;
  cname_chain?: string[];
  ede?: { code: number; text?: string };
  took_ms: number;
  node?: string;
}

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

export interface UpstreamsResult extends ScopeInfo, Window {
  upstreams: UpstreamItem[];
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

/** One row of GET /settings/_schema (internal/config.Key). */
export interface SettingKey {
  path: string;
  type: SettingType;
  scope: 'node' | 'cluster';
  default: unknown;
  help: string;
  hostOnly?: boolean;
  envLocked?: boolean;
  restartRequired?: boolean;
  writeOnly?: boolean;
}

export interface NodeInfo {
  name: string;
  version: string;
  roles: string[];
  ready: boolean;
  leader: boolean;
  vipMaster?: boolean;
  qps: number;
  snapshotAge?: string;
  address?: string;
  storeDriver?: string;
  startedAt?: string;
}

export interface NodeHealth {
  name: string;
  checks: { name: string; ok: boolean; message?: string }[];
}

export interface ClusterInfo {
  mode: string;
  leader: string;
  nodes: string[];
  epoch?: number;
  readOnly?: boolean;
}

export interface DiagnosisMessage {
  id: string;
  severity: 'error' | 'warning' | 'info';
  reason: string;
  message: string;
  object?: { kind: string; namespace?: string; name: string };
  hint?: string;
  node?: string;
  since?: string;
}

export interface VersionInfo {
  version: string;
  commit: string;
  date: string;
  go?: string;
}

/** GET /clients row: one client seen by the query log or named in a group. */
export interface ClientInventoryItem {
  ip: string;
  name?: string;
  mac?: string;
  /** ClientGroup names the identify stage resolves this client to. */
  groups: string[];
  lastSeen?: string;
  queries24h: number;
  blocked24h: number;
}

/** GET /edge: dnsdist backends and VIP owners (single node: empty). */
export interface EdgeInfo {
  backends: { name: string; address: string; up: boolean; qps?: number }[];
  vip?: { address: string; owner?: string }[];
}

export interface AuditEntry {
  id: string;
  ts: string;
  actor: string;
  action: string;
  kind: string;
  namespace?: string;
  name: string;
  outcome: 'success' | 'denied' | 'error';
  authzPath?: string;
}
