// One typed function per /api/v1 route the dashboard uses. Pages call these
// through the hooks in ./hooks; tests and mocks share the paths.
import type { ApiClient, Query } from './client';
import type { ExternalUser, ExternalUserCommand, ExternalUserPage } from './types';
import type { DomainRegexImpact } from './types';
import type { InventoryBody, InventoryQuery } from './types';
import { dhcpEndpoints } from './dhcp';
import type {
  BrandingInfo,
  ApiToken,
  ApplyResult,
  AuditPage,
  AuditQuery,
  AuditVerification,
  AuthStatus,
  TOTPStatus,
  TOTPProofRequest,
  TOTPSetupResponse,
  TOTPConfirmRequest,
  TOTPRecoverRequest,
  BlockingState,
  BlockingCommandResult,
  ClientsResult,
  ClientTimeseries,
  ClusterInfo,
  ActivateClusterCommand,
  ClusterActivationReceipt,
  ClusterEnrollmentInfo,
  ClusterJoinTokenCommand,
  ClusterJoinTokenReceipt,
  ClusterPromoteCommand,
  ClusterPromotionInfo,
  ClusterPromotionReceipt,
  RootAnchorStatus,
  RPZFeedList,
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
  SessionRecordInfo,
  NodeHealth,
  NodeInfo,
  ObjectKind,
  PasswordChangeRequest,
  Processed,
  PurgeResult,
  QueriesPage,
  QueryFilter,
  QuerylogStatus,
  QuerylogSinks,
  EdgeBlock,
  EdgeBlockList,
  EdgeBlockRequest,
  RoleBindingDesired,
  RoleBindingDocument,
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
import type { CreateLocalUserRequest, CreateServiceAccountTokenRequest, IdentityProviderDesired, IdentityProviderDocument, ObjectReview, ReplaceIdentityProviderCommand, ReplaceRoleBindingCommand, ReplaceServiceAccountCommand, ResetLocalUserPasswordRequest, RoleBindingReview, ServiceAccountDesired, ServiceAccountDocument, UpdateLocalUserRequest } from './types';
import type { Integration, IntegrationExecutionResult, IntegrationManifests, IntegrationReleaseResult, IntegrationReview, IntegrationSummary } from './types';
import type { MigrationPlan, MigrationRequest, MigrationResult, MigrationRun, MigrationCutoverPlan, MigrationAuthorityChange } from './types';

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

/** A copy of an object without its server-owned status or response timing. */
export function withoutStatus<T extends AnyObject>(obj: T): Omit<T, 'status'> {
  const { status: _status, took_ms: _took, ...rest } = obj as T & { took_ms?: number };
  return rest as Omit<T, 'status'>;
}

/** An Integration to send: status is server-owned and never sent. */
function integrationBody(obj: Integration): Omit<Integration, 'status'> {
  const { status: _status, took_ms: _took, ...rest } = obj as Integration & { took_ms?: number };
  return rest;
}

/** /objects path of one object; cluster-scoped kinds omit the namespace. */
export function objectPath(kind: ObjectKind, name: string, namespace?: string): string {
  return namespace ? `/objects/${kind}/${enc(namespace)}/${enc(name)}` : `/objects/${kind}/${enc(name)}`;
}

export function endpoints(c: ApiClient) {
  return {
    dhcp: dhcpEndpoints(c),
    imports: {
      preview: (request: MigrationRequest) => c.post<MigrationPlan>('/imports/preview', request),
      apply: (plan: MigrationPlan) => c.post<MigrationResult>('/imports/apply', plan),
      start: (plan: MigrationPlan) => c.post<MigrationRun>('/imports/start', plan),
      runs: (cursor?: string) => c.get<ListEnvelope<MigrationRun>>('/imports/runs', { limit: 25, continue: cursor }),
      run: (id: string) => c.get<MigrationRun>(`/imports/runs/${enc(id)}`),
      cutoverPreview: (id: string, nameservers: string[], addresses: string[] = []) => c.post<MigrationCutoverPlan>(`/imports/runs/${enc(id)}/decommission/preview`, { nameservers, ...(addresses.length ? { addresses } : {}) }),
      cutover: (id: string, plan: MigrationCutoverPlan) => c.post<MigrationResult>(`/imports/runs/${enc(id)}/decommission`, plan),
      existing: () => c.get<ListEnvelope<MigrationAuthorityChange>>('/imports/existing'),
      adopt: (zones: MigrationAuthorityChange[]) => c.post<MigrationResult>('/imports/adopt', { zones }),
    },
    branding: { get: (signal?: AbortSignal) => c.get<BrandingInfo>('/branding', undefined, signal) },
    /** Download URL of a whole object collection (JSON list envelope). */
    objectsURL: (kind: ObjectKind) => c.url(`/objects/${kind}`),

    auth: {
      status: () => c.get<AuthStatus>('/auth/status'),
      totp: (signal?: AbortSignal) => c.request<TOTPStatus>('GET', '/auth/totp', { signal }),
      beginTOTP: (req: TOTPProofRequest, signal?: AbortSignal) => c.request<TOTPSetupResponse>('POST', '/auth/totp/setup', { body: req, signal }),
      confirmTOTP: (req: TOTPConfirmRequest, signal?: AbortSignal) => c.request<void>('POST', '/auth/totp/confirm', { body: req, signal }),
      disableTOTP: (req: TOTPProofRequest, signal?: AbortSignal) => c.request<void>('POST', '/auth/totp/disable', { body: req, signal }),
      me: () => c.get<Me>('/auth/me'),
      login: (username: string, password: string, totpCode?: string) => c.post<Me>('/auth/login', { username, password, ...(totpCode ? { totpCode } : {}) }),
      logout: () => c.post<void>('/auth/logout'),
      browserLogout: () => c.post<{ continuePath?: string } | undefined>('/auth/logout/browser'),
      sessions: (signal?: AbortSignal) => c.request<ListEnvelope<SessionRecordInfo>>('GET', '/auth/sessions', { signal }),
      revokeSession: (id: string, signal?: AbortSignal) => c.request<void>('DELETE', `/auth/sessions/${enc(id)}`, { signal }),
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

    /**
     * Integrations (docs/deploy/integrations.md). The objects are written
     * through the generic object routes (D400) with references only; check,
     * preview, sync, actions and release are bound to one reviewed UID and
     * revision and run on the primary.
     */
    integrations: {
      manifests: () => c.get<IntegrationManifests>('/integrations/manifests'),
      list: (namespace: string) => c.get<{ items: IntegrationSummary[] }>('/integrations', { namespace }),
      get: (namespace: string, name: string) => c.get<IntegrationSummary>(`/integrations/${enc(namespace)}/${enc(name)}`),
      objects: (q?: { namespace?: string }) => c.get<ListEnvelope<Integration>>('/objects/integrations', q),
      object: (namespace: string, name: string) => c.get<Integration>(`/objects/integrations/${enc(namespace)}/${enc(name)}`),
      /** Create-only: 409 when the object exists. */
      create: (obj: Integration) => c.post<Integration>('/objects/integrations', integrationBody(obj)),
      /** Replace, bound to the reviewed resourceVersion (412 otherwise). */
      replace: (obj: Integration) =>
        c.put<Integration>(
          `/objects/integrations/${enc(obj.metadata.namespace ?? '')}/${enc(obj.metadata.name)}`,
          integrationBody(obj),
          obj.metadata.resourceVersion ? { 'If-Match': obj.metadata.resourceVersion } : undefined,
        ),
      remove: (namespace: string, name: string, resourceVersion?: string) =>
        c.request<void>('DELETE', `/objects/integrations/${enc(namespace)}/${enc(name)}`, resourceVersion ? { headers: { 'If-Match': resourceVersion } } : {}),
      check: (namespace: string, name: string, review: IntegrationReview) => c.post<IntegrationExecutionResult>(`/integrations/${enc(namespace)}/${enc(name)}/check`, review),
      preview: (namespace: string, name: string, review: IntegrationReview) => c.post<IntegrationExecutionResult>(`/integrations/${enc(namespace)}/${enc(name)}/preview`, review),
      sync: (namespace: string, name: string, review: IntegrationReview) => c.post<IntegrationExecutionResult>(`/integrations/${enc(namespace)}/${enc(name)}/sync`, review),
      actions: (namespace: string, name: string, review: Pick<IntegrationReview, 'uid' | 'revision'>) =>
        c.get<IntegrationExecutionResult>(`/integrations/${enc(namespace)}/${enc(name)}/actions`, { uid: review.uid, revision: review.revision }),
      act: (namespace: string, name: string, action: string, review: IntegrationReview) =>
        c.post<IntegrationExecutionResult>(`/integrations/${enc(namespace)}/${enc(name)}/actions/${enc(action)}`, review),
      release: (namespace: string, name: string, review: Pick<IntegrationReview, 'uid' | 'revision'>) =>
        c.post<IntegrationReleaseResult>(`/integrations/${enc(namespace)}/${enc(name)}/release`, { uid: review.uid, revision: review.revision }),
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
      /**
       * `view` narrows the delete to one view (D403): a ClientGroup name, public, or "-" for the default data.
       * `namespace` narrows it to record sets of one namespace (a served view row's group namespace).
       */
      deleteRecord: (zone: string, name: string, type: string, recordSet?: string, setIdentifier?: string, view?: string, namespace?: string) =>
        c.delete(`/zones/${enc(zone)}/records/${enc(name)}/${enc(type)}`, { recordSet, setIdentifier, view, namespace }),
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
      /** The RPZ feeds of every list (or of one namespace's) on the answering node (D391). */
      rpzFeeds: (namespace?: string) => c.get<RPZFeedList>('/blocklists/_rpz', namespace ? { namespace } : undefined),
      /** The RPZ feeds of one list on the answering node: 404 when its configuration has no such list, 501 without a feed manager, 503 before any configuration. */
      rpz: (namespace: string, name: string) => c.get<RPZFeedList>(`/blocklists/${enc(namespace)}/${enc(name)}/rpz`),
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
      impact: (action: DomainAction, id: string, name: string) => c.get<DomainRegexImpact>(`/${action}list/${enc(id)}/impact`, { name }),
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
      list: (s: ScopeParams, q: { window?: string } = {}) => c.get<ClientsResult>('/clients', { ...scopeQuery(s), ...q }),
    },

    /** GET /inventory (inventory.read, D397): the answering node's view. */
    inventory: {
      list: (q: InventoryQuery = {}, signal?: AbortSignal) =>
        c.get<InventoryBody>('/inventory', Object.fromEntries(Object.entries(q).filter(([, v]) => v !== '' && v !== undefined)), signal),
    },

    runtime: {
      blocking: () => c.get<BlockingState>('/dns/blocking'),
      setBlocking: (blocking: boolean, timer?: number) =>
        c.post<BlockingCommandResult>('/dns/blocking', timer ? { blocking, timer } : { blocking }),
      setBlockingUntil: (until: string) => c.post<BlockingCommandResult>('/dns/blocking', { blocking: false, until }),
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
      exportURL: (s: ScopeParams, f: QueryFilter) => c.url('/queries/export', { ...scopeQuery(s), ...(f as Query), limit: 1000 }),
      purge: (before?: string) => c.delete<PurgeResult>('/querylog', before ? { before } : undefined),
      status: () => c.get<QuerylogStatus>('/querylog/status'),
      /** The answering node's additional sinks; never a credential, URL or error text. */
      sinks: () => c.get<QuerylogSinks>('/querylog/sinks'),
    },

    settings: {
      get: () => c.get<SettingsBody>('/settings'),
      patch: (patch: Record<string, unknown>, revision?: string) => c.request<SettingsBody>('PATCH', '/settings', { body: patch, contentType: 'application/merge-patch+json', headers: revision ? { 'If-Match': revision } : {} }),
      ownership: (section: string, action: 'adopt' | 'release', revision: string) => c.post<SettingsBody>(`/settings/sections/${enc(section)}/ownership/${action}`, { revision }),
      yaml: (section: string, proposed?: Record<string, unknown>) => c.post<{ yaml: string; revision: string }>(`/settings/sections/${enc(section)}/yaml`, { proposed }),
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
      // Operator edge blocks (D388): one request per command, never repeated.
      edgeBlocks: () => c.get<EdgeBlockList>('/edge/blocks'),
      createEdgeBlock: (req: EdgeBlockRequest) => c.post<EdgeBlock>('/edge/blocks', req),
      revokeEdgeBlock: (id: string) => c.delete<EdgeBlock>(`/edge/blocks/${enc(id)}`),
      // Enrolled SQLite membership (docs/deploy/sqlite-cluster.md). Each
      // command is one request that the dashboard never repeats; the signal
      // ends a request the server's 30-second bound did not answer.
      enrollment: () => c.get<ClusterEnrollmentInfo>('/cluster/enrollment'),
      activate: (cmd: ActivateClusterCommand, signal?: AbortSignal) => c.request<ClusterActivationReceipt>('POST', '/cluster/activate', { body: cmd, signal }),
      /** The receipt holds a bearer secret: call it directly, never through a query or mutation cache. */
      issueJoinToken: (cmd: ClusterJoinTokenCommand, signal?: AbortSignal) => c.request<ClusterJoinTokenReceipt>('POST', '/cluster/join-token', { body: cmd, signal }),
      promotion: () => c.get<ClusterPromotionInfo>('/cluster/promotion'),
      promote: (cmd: ClusterPromoteCommand, signal?: AbortSignal) => c.request<ClusterPromotionReceipt>('POST', '/cluster/promote', { body: cmd, signal }),
      anchors: () => c.get<RootAnchorStatus>('/cluster/anchors'),
      messages: () => c.get<ListEnvelope<DiagnosisMessage>>('/info/messages'),
      version: () => c.get<VersionInfo>('/version'),
      doctor: () => c.get<DoctorReport>('/doctor'),
    },

    iam: {
      recoverTOTP: (name: string, req: TOTPRecoverRequest, signal?: AbortSignal) => c.request<void>('POST', `/iam/users/${enc(name)}/totp/recover`, { body: req, signal }),
      users: () => c.get<IamList<IamUser>>('/iam/users'),
      externalUsers: (cursor = '', signal?: AbortSignal) => c.request<ExternalUserPage>('GET', '/iam/external-users', { query: { limit: 50, continue: cursor }, signal }),
      externalUser: (name: string, signal?: AbortSignal) => c.request<ExternalUser>('GET', `/iam/external-users/${enc(name)}`, { signal }),
      administerExternalUser: (name: string, action: 'disable' | 'enable' | 'logout', reviewed: ExternalUserCommand, signal?: AbortSignal) => c.request<void>('POST', `/iam/external-users/${enc(name)}/${action}`, { body: reviewed, signal }),
      serviceAccounts: () => c.get<IamList<IamServiceAccount>>('/iam/serviceaccounts'),
      roleBindings: () => c.get<IamList<IamRoleBinding>>('/iam/rolebindings'),
      /** Creates once (409 when the name exists); complete SQLite runtime only. */
      createRoleBinding: (binding: RoleBindingDesired, signal?: AbortSignal) => c.request<RoleBindingDocument>('POST', '/iam/rolebindings', { body: binding, signal }),
      identityProviders: () => c.get<IamList<IamIdentityProvider>>('/iam/identityproviders'),
      // Reviewed IAM writers (D294, D396): complete SQLite runtime only, one
      // request per command that the dashboard never repeats. Replace and
      // delete name the exact UID and resourceVersion of the reviewed read.
      roleBinding: (namespace: string, name: string, signal?: AbortSignal) => c.get<RoleBindingDocument>(`/iam/rolebindings/${enc(namespace)}/${enc(name)}`, undefined, signal),
      replaceRoleBinding: (namespace: string, name: string, cmd: ReplaceRoleBindingCommand, signal?: AbortSignal) => c.request<RoleBindingDocument>('PUT', `/iam/rolebindings/${enc(namespace)}/${enc(name)}`, { body: cmd, signal }),
      deleteRoleBinding: (namespace: string, name: string, review: RoleBindingReview, signal?: AbortSignal) => c.request<void>('DELETE', `/iam/rolebindings/${enc(namespace)}/${enc(name)}`, { body: review, signal }),
      /** The password goes only into this request; nothing keeps it. */
      createUser: (req: CreateLocalUserRequest, signal?: AbortSignal) => c.request<IamUser>('POST', '/iam/users', { body: req, signal }),
      updateUser: (name: string, req: UpdateLocalUserRequest, signal?: AbortSignal) => c.request<IamUser>('PATCH', `/iam/users/${enc(name)}`, { body: req, signal }),
      deleteUser: (name: string, review: ObjectReview, signal?: AbortSignal) => c.request<void>('DELETE', `/iam/users/${enc(name)}`, { body: review, signal }),
      resetUserPassword: (name: string, req: ResetLocalUserPasswordRequest, signal?: AbortSignal) => c.request<void>('POST', `/iam/users/${enc(name)}/password`, { body: req, signal }),
      serviceAccount: (namespace: string, name: string, signal?: AbortSignal) => c.get<ServiceAccountDocument>(`/iam/serviceaccounts/${enc(namespace)}/${enc(name)}`, undefined, signal),
      createServiceAccount: (sa: ServiceAccountDesired, signal?: AbortSignal) => c.request<ServiceAccountDocument>('POST', '/iam/serviceaccounts', { body: sa, signal }),
      replaceServiceAccount: (namespace: string, name: string, cmd: ReplaceServiceAccountCommand, signal?: AbortSignal) => c.request<ServiceAccountDocument>('PUT', `/iam/serviceaccounts/${enc(namespace)}/${enc(name)}`, { body: cmd, signal }),
      deleteServiceAccount: (namespace: string, name: string, review: ObjectReview, signal?: AbortSignal) => c.request<void>('DELETE', `/iam/serviceaccounts/${enc(namespace)}/${enc(name)}`, { body: review, signal }),
      /** The answer holds the token secret once: call it directly, never through a query or mutation cache. */
      createServiceAccountToken: (namespace: string, name: string, req: CreateServiceAccountTokenRequest, signal?: AbortSignal) => c.request<CreatedToken>('POST', `/iam/serviceaccounts/${enc(namespace)}/${enc(name)}/tokens`, { body: req, signal }),
      identityProvider: (name: string, signal?: AbortSignal) => c.get<IdentityProviderDocument>(`/iam/identityproviders/${enc(name)}`, undefined, signal),
      createIdentityProvider: (idp: IdentityProviderDesired, signal?: AbortSignal) => c.request<IdentityProviderDocument>('POST', '/iam/identityproviders', { body: idp, signal }),
      replaceIdentityProvider: (name: string, cmd: ReplaceIdentityProviderCommand, signal?: AbortSignal) => c.request<IdentityProviderDocument>('PUT', `/iam/identityproviders/${enc(name)}`, { body: cmd, signal }),
      deleteIdentityProvider: (name: string, review: ObjectReview, signal?: AbortSignal) => c.request<void>('DELETE', `/iam/identityproviders/${enc(name)}`, { body: review, signal }),
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
