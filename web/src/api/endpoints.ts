// One typed function per /api/v1 route the dashboard uses. Pages call these
// through the hooks in ./hooks; tests and mocks share the paths.
import type { ApiClient, Query } from './client';
import type {
  ApiToken,
  ApplyResult,
  AuditPage,
  AuditQuery,
  AuthStatus,
  BlockingState,
  ClientItem,
  ClientTimeseries,
  ClusterInfo,
  CreateTokenRequest,
  EdgeInfo,
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
} from './types';
import type { CatalogueInfo, CatalogueService, SafeSearchEngine } from './types';

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
      delete: (kind: ObjectKind, name: string, namespace?: string) => c.delete(objectPath(kind, name, namespace)),
      apply: (yamlOrJson: string, contentType: 'application/yaml' | 'application/json' = 'application/yaml') =>
        c.request<{ items: ApplyResult[] }>('POST', '/apply', { text: yamlOrJson, contentType }),
      batchDelete: (kind: ObjectKind, names: string[], namespace?: string) =>
        c.post<Processed>(`/${kind}:batchDelete`, { names, namespace }),
    },

    zones: {
      list: () => c.get<ListEnvelope<ZoneSummary>>('/zones'),
      get: (zone: string) => c.get<ZoneSummary>(`/zones/${enc(zone)}`),
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
      addDomains: (action: 'allow' | 'deny', kind: 'exact' | 'regex', domains: string[], namespace?: string, list?: string) =>
        c.post<Processed>(`/domains/${action}/${kind}`, { domains, namespace, list }),
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
      action: (id: number, action: 'allow' | 'deny') => c.post<Processed>(`/queries/${id}/actions`, { action }),
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
      /** The page token goes out as `cursor` (and as `continue`, the list-envelope name). */
      list: ({ cursor, ...q }: AuditQuery) => c.get<AuditPage>('/audit', { ...q, cursor, continue: cursor }),
      exportURL: () => c.url('/audit/export'),
    },

    // Blocked-services and safe-search catalogue (D60).
    catalogue: {
      info: () => c.get<CatalogueInfo>('/catalogue'),
      services: () => c.get<ListEnvelope<CatalogueService>>('/catalogue/services'),
      safeSearch: () => c.get<ListEnvelope<SafeSearchEngine>>('/catalogue/safesearch'),
    },
  };
}

export type Endpoints = ReturnType<typeof endpoints>;
