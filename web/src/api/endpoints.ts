// One typed function per /api/v1 route the dashboard uses. Pages call these
// through the hooks in ./hooks; tests and mocks share the paths.
import type { ApiClient, Query } from './client';
import type {
  ApiToken,
  ApplyResult,
  AuditPage,
  AuditQuery,
  AuditVerification,
  AuthStatus,
  BlockingState,
  ClientItem,
  ClientTimeseries,
  ClusterInfo,
  CreateTokenRequest,
  EdgeInfo,
  LicenseInfo,
  CreatedToken,
  DiagnosisMessage,
  DoctorReport,
  DnsQueryRequest,
  DnsQueryResult,
  ListEnvelope,
  IamList,
  Me,
  NodeHealth,
  NodeInfo,
  ObjectKind,
  PasswordChangeRequest,
  Processed,
  PurgeResult,
  QueriesPage,
  QueryFilter,
  QuerylogStatus,
  RecentBlocked,
  SettingKey,
  SettingsBody,
  SuggestionField,
  Suggestions,
  Summary,
  Timeseries,
  TopDim,
  TopResult,
  UpstreamsResult,
  VersionInfo,
  ZoneRecord,
  ZoneSummary,
  AnyObject,
  Endpoint,
  IamUser,
  IamServiceAccount,
  IamRoleBinding,
  IamIdentityProvider,
  ZoneImportResult,
  ListSearchHit,
  ListEntriesPatch,
  ListEntriesPatchResult,
  SourceEntriesPage,
  SourceEntriesQuery,
} from './types';
import type { DomainAction, DomainBatch, DomainEntriesCreate, DomainEntriesPage, DomainEntriesQuery, DomainEntry, DomainEntryPatch, DomainWriteResult } from './types';
import type { CatalogueCategory, CatalogueIconIndex, CatalogueInfo, CatalogueService, PolicyState, SafeSearchEngine } from './types';
import type { UnblockApproval, UnblockDecisionResult, UnblockDenial, UnblockRequest, UnblockRequestList, UnblockState } from './types';
import type { ZoneDeleteResult } from './types';

/** `scope=node|cluster&node=<name>` on every stats and log call (§10). */
export interface ScopeParams {
  scope: 'cluster' | 'node';
  node?: string | undefined;
}

function scopeQuery(s: ScopeParams | undefined): Query {
  if (!s) return {};
  return s.scope === 'node' && s.node ? { scope: 'node', node: s.node } : { scope: 'cluster' };
}

const enc = encodeURIComponent;

/** A copy of an object without its server-owned status. */
export function withoutStatus<T extends AnyObject>(obj: T): Omit<T, 'status'> {
  const { status: _status, ...rest } = obj;
  return rest;
}

/** /objects path of one object; cluster-scoped kinds omit the namespace. */
export function objectPath(kind: ObjectKind, name: string, namespace?: string): string {
  return namespace ? `/objects/${kind}/${enc(namespace)}/${enc(name)}` : `/objects/${kind}/${enc(name)}`;
}

export function endpoints(c: ApiClient) {
  return {
    /** Download URL of a whole object collection (JSON list envelope). */
    objectsURL: (kind: ObjectKind) => c.url(`/objects/${kind}`),

    auth: {
      status: () => c.get<AuthStatus>('/auth/status'),
      me: () => c.get<Me>('/auth/me'),
      login: (username: string, password: string) => c.post<Me>('/auth/login', { username, password }),
      logout: () => c.post<void>('/auth/logout'),
      oidcStartURL: (idp: string, next: string) => c.url(`/auth/oidc/${enc(idp)}/start`, { next }),
      tokens: () => c.get<ListEnvelope<ApiToken>>('/auth/tokens'),
      createToken: (req: CreateTokenRequest) => c.post<CreatedToken>('/auth/tokens', req),
      /** Answers the refreshed session (a new CSRF token); every other session of the user is revoked. */
      changePassword: (req: PasswordChangeRequest) => c.post<Me | undefined>('/auth/password', req),
      deleteToken: (id: string) => c.delete(`/auth/tokens/${enc(id)}`),
    },

    objects: {
      list: <T extends AnyObject>(kind: ObjectKind, q?: { namespace?: string; labelSelector?: string; limit?: number; continue?: string }) =>
        c.get<ListEnvelope<T>>(`/objects/${kind}`, q),
      get: <T extends AnyObject>(kind: ObjectKind, name: string, namespace?: string) =>
        c.get<T>(objectPath(kind, name, namespace)),
      /** Whole-object PUT; status is server-owned and never sent. */
      put: <T extends AnyObject>(kind: ObjectKind, obj: T) =>
        c.put<T>(
          objectPath(kind, obj.metadata.name, obj.metadata.namespace),
          withoutStatus(obj),
          obj.metadata.resourceVersion ? { 'If-Match': obj.metadata.resourceVersion } : undefined,
        ),
      /** Create-only (POST /objects/{kind}): 409 conflict when the object exists. */
      create: <T extends AnyObject>(kind: ObjectKind, obj: T) => c.post<T>(`/objects/${kind}`, withoutStatus(obj)),
      delete: (kind: ObjectKind, name: string, namespace?: string) => c.delete(objectPath(kind, name, namespace)),
      apply: (yamlOrJson: string, contentType: 'application/yaml' | 'application/json' = 'application/yaml') =>
        c.request<{ items: ApplyResult[] }>('POST', '/apply', { text: yamlOrJson, contentType }),
      batchDelete: (kind: ObjectKind, names: string[], namespace?: string) =>
        c.post<Processed>(`/${kind}:batchDelete`, { names, namespace }),
    },

    zones: {
      list: () => c.get<ListEnvelope<ZoneSummary>>('/zones'),
      get: (zone: string) => c.get<ZoneSummary>(`/zones/${enc(zone)}`),
      /**
       * Deletes the zone with every record set that references it (D148);
       * `dryRun` runs every check and returns the counts without deleting.
       */
      delete: (zone: string, opts: { dryRun?: boolean; ifMatch?: string } = {}) =>
        c.request<ZoneDeleteResult>('DELETE', `/zones/${enc(zone)}`, {
          query: opts.dryRun ? { dryRun: true } : undefined,
          ...(opts.ifMatch ? { headers: { 'If-Match': opts.ifMatch } } : {}),
        }),
      records: (zone: string) => c.get<ListEnvelope<ZoneRecord>>(`/zones/${enc(zone)}/records`),
      upsertRecord: (zone: string, ep: Endpoint, recordSet?: string) =>
        c.post<ZoneRecord>(`/zones/${enc(zone)}/records`, ep, recordSet ? { recordSet } : undefined),
      deleteRecord: (zone: string, name: string, type: string, recordSet?: string, setIdentifier?: string) =>
        c.delete(`/zones/${enc(zone)}/records/${enc(name)}/${enc(type)}`, { recordSet, setIdentifier }),
      /** Imports a BIND file; `origin` creates the zone when it does not exist yet. */
      importBind: (zone: string, text: string, origin?: string) =>
        c.request<ZoneImportResult>('POST', `/zones/${enc(zone)}/import`, {
          text,
          contentType: 'text/dns',
          query: origin ? { origin } : undefined,
        }),
      exportURL: (zone: string) => c.url(`/zones/${enc(zone)}/export`),
      exportText: (zone: string) => c.request<string>('GET', `/zones/${enc(zone)}/export`, { asText: true, headers: { Accept: 'text/dns' } }),
      notify: (zone: string) => c.post<{ targets: string[] }>(`/zones/${enc(zone)}/notify`),
    },

    lists: {
      refresh: (namespace: string, name: string) => c.post<void>(`/blocklists/${enc(namespace)}/${enc(name)}/refresh`),
      search: (domain: string) => c.get<ListEnvelope<ListSearchHit>>('/blocklists/_search', { domain }),
      /**
       * A page of one source's entries, any kind: inline lines, the rules of a
       * url or configMap body this node cached, or structured entries; `q`
       * filters by substring, `enabled: 'false'` shows the switched-off ones.
       */
      entries: (namespace: string, name: string, source: string, q: SourceEntriesQuery = {}) =>
        c.get<SourceEntriesPage>(`/blocklists/${enc(namespace)}/${enc(name)}/sources/${enc(source)}/entries`, { ...q }),
      /** Adds and removes inline entries, switches entries of any source on and off. */
      patchEntries: (namespace: string, name: string, source: string, body: ListEntriesPatch) =>
        c.patch<ListEntriesPatchResult | undefined>(`/blocklists/${enc(namespace)}/${enc(name)}/sources/${enc(source)}/entries`, body, 'application/json'),
    },

    // The Allowlist and Denylist (one logical entry across ui-allow /
    // ui-allow-<group>, ui-deny / ui-deny-<group>); /domains lists both.
    domains: {
      list: (action: DomainAction | undefined, q: DomainEntriesQuery = {}) =>
        c.get<DomainEntriesPage>(action ? `/${action}list` : '/domains', { ...q }),
      get: (action: DomainAction, id: string) => c.get<DomainEntry>(`/${action}list/${enc(id)}`),
      create: (action: DomainAction, body: DomainEntriesCreate) => c.post<DomainWriteResult>(`/${action}list`, body),
      patch: (action: DomainAction, id: string, body: DomainEntryPatch) => c.patch<DomainWriteResult>(`/${action}list/${enc(id)}`, body, 'application/json'),
      delete: (action: DomainAction, id: string) => c.delete(`/${action}list/${enc(id)}`),
      batch: (action: DomainAction, body: DomainBatch) => c.post<DomainWriteResult>(`/${action}list:batch`, body),
    },

    policies: {
      /** The policy's current state (ux-plan C3). */
      state: (namespace: string, name: string) => c.get<PolicyState>(`/policies/${enc(namespace)}/${enc(name)}/state`),
    },

    clients: {
      list: (q: { window?: string } = {}) => c.get<ListEnvelope<ClientItem>>('/clients', q),
    },

    runtime: {
      blocking: () => c.get<BlockingState>('/dns/blocking'),
      setBlocking: (blocking: boolean, timer?: number) =>
        c.post<BlockingState>('/dns/blocking', timer ? { blocking, timer } : { blocking }),
      setBlockingUntil: (until: string) => c.post<BlockingState>('/dns/blocking', { blocking: false, until }),
      flushCache: (name?: string) => c.post<{ flushed: number }>('/cache/flush', name ? { name } : {}),
      dig: (req: DnsQueryRequest) => c.post<DnsQueryResult>('/dns/query', req),
    },

    stats: {
      summary: (s: ScopeParams, window = '24h') => c.get<Summary>('/stats/summary', { ...scopeQuery(s), window }),
      timeseries: (s: ScopeParams, q: { from?: string; to?: string; step?: string } = {}) =>
        c.get<Timeseries>('/stats/timeseries', { ...scopeQuery(s), step: 'auto', ...q }),
      top: (s: ScopeParams, dim: TopDim, q: { n?: number; blocked?: boolean; window?: string; from?: string; to?: string } = {}) =>
        c.get<TopResult>('/stats/top', { ...scopeQuery(s), dim, ...q }),
      upstreams: (s: ScopeParams, window = '24h') => c.get<UpstreamsResult>('/stats/upstreams', { ...scopeQuery(s), window }),
      clientsTimeseries: (s: ScopeParams, q: { n?: number; window?: string } = {}) =>
        c.get<ClientTimeseries>('/stats/clients/timeseries', { ...scopeQuery(s), ...q }),
      recentBlocked: (s: ScopeParams, n = 10) => c.get<RecentBlocked>('/stats/recent_blocked', { ...scopeQuery(s), n }),
    },

    queries: {
      list: (s: ScopeParams, f: QueryFilter, signal?: AbortSignal) =>
        c.get<QueriesPage>('/queries', { ...scopeQuery(s), ...(f as Query) }, signal),
      suggestions: (field: SuggestionField, prefix: string, signal?: AbortSignal) =>
        c.get<Suggestions>('/queries/suggestions', { field, prefix }, signal),
      streamURL: (s: ScopeParams, f: QueryFilter) => c.url('/queries/stream', { ...scopeQuery(s), ...(f as Query) }),
      purge: (before?: string) => c.delete<PurgeResult>('/querylog', before ? { before } : undefined),
      status: () => c.get<QuerylogStatus>('/querylog/status'),
    },

    settings: {
      get: () => c.get<SettingsBody>('/settings'),
      patch: (patch: Record<string, unknown>) => c.patch<SettingsBody>('/settings', patch),
      nodeConfig: () => c.get<SettingsBody>('/node/config'),
      patchNodeConfig: (patch: Record<string, unknown>) => c.patch<SettingsBody>('/node/config', patch),
      schema: () => c.get<ListEnvelope<SettingKey>>('/settings/_schema'),
    },

    license: {
      get: () => c.get<LicenseInfo>('/license'),
    },

    cluster: {
      nodes: () => c.get<ListEnvelope<NodeInfo>>('/nodes'),
      health: (name: string) => c.get<NodeHealth>(`/nodes/${enc(name)}/health`),
      info: () => c.get<ClusterInfo>('/cluster'),
      edge: () => c.get<EdgeInfo>('/edge'),
      messages: () => c.get<ListEnvelope<DiagnosisMessage>>('/info/messages'),
      version: () => c.get<VersionInfo>('/version'),
      doctor: () => c.get<DoctorReport>('/doctor'),
    },

    iam: {
      users: () => c.get<IamList<IamUser>>('/iam/users'),
      serviceAccounts: () => c.get<IamList<IamServiceAccount>>('/iam/serviceaccounts'),
      roleBindings: () => c.get<IamList<IamRoleBinding>>('/iam/rolebindings'),
      identityProviders: () => c.get<IamList<IamIdentityProvider>>('/iam/identityproviders'),
    },

    audit: {
      verify: () => c.get<AuditVerification>('/audit/verify'),
      /** The page token goes out as `cursor` (and as `continue`, the list-envelope name). */
      list: ({ cursor, ...q }: AuditQuery) => c.get<AuditPage>('/audit', { ...q, cursor, continue: cursor }),
      exportURL: () => c.url('/audit/export'),
    },

    // Blocked-services and safe-search catalogue (D60).
    catalogue: {
      info: () => c.get<CatalogueInfo>('/catalogue'),
      services: () => c.get<ListEnvelope<CatalogueService>>('/catalogue/services'),
      categories: () => c.get<ListEnvelope<CatalogueCategory>>('/catalogue/categories'),
      safeSearch: () => c.get<ListEnvelope<SafeSearchEngine>>('/catalogue/safesearch'),
      /** C4: the embedded icons (404 on servers without them). */
      icons: () => c.get<CatalogueIconIndex>('/catalogue/icons'),
      /** An icon's URL under this client's base: an `icon` API path (/api/v1/catalogue/icons/x.svg) or an icon id. */
      iconURL: (pathOrId: string) => c.url(pathOrId.includes('/') ? pathOrId.replace(/^(?:.*\/)?api\/v1(?=\/)/, '') : `/catalogue/icons/${enc(pathOrId)}.svg`),
    },

    // Block page (C5): the preview is text/html, framed by the Settings page
    // (its CSP allows frame-ancestors 'self').
    blockPage: {
      previewURL: (domain?: string) => c.url('/blockpage/preview', domain ? { domain } : undefined),
    },

    // Unblock requests from the block page: the list (newest first, with
    // the counts of every state), one request, and the decisions.
    unblock: {
      list: (q: { state?: UnblockState | undefined; limit?: number; continue?: string | undefined } = {}) => c.get<UnblockRequestList>('/unblockrequests', q),
      get: (namespace: string, name: string) => c.get<UnblockRequest>(`/unblockrequests/${enc(namespace)}/${enc(name)}`),
      approve: (namespace: string, name: string, body: UnblockApproval) =>
        c.post<UnblockDecisionResult>(`/unblockrequests/${enc(namespace)}/${enc(name)}/approve`, body),
      deny: (namespace: string, name: string, body: UnblockDenial) => c.post<UnblockDecisionResult>(`/unblockrequests/${enc(namespace)}/${enc(name)}/deny`, body),
      delete: (namespace: string, name: string) => c.delete(`/unblockrequests/${enc(namespace)}/${enc(name)}`),
    },
  };
}

export type Endpoints = ReturnType<typeof endpoints>;
