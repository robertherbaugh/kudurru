// React wiring for the API client: one ApiClient per app, the session from
// GET /auth/me (principal, permission matrix, CSRF token) and the node
// scope every stats/log call carries.
import { createContext, useCallback, useContext, useMemo, useRef, useState, type ReactNode } from 'react';
import { useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query';
import { ApiClient, ApiError } from './client';
import { endpoints, type Endpoints, type ScopeParams } from './endpoints';
import type { AuthStatus, ClusterInfo, Me } from './types';
import { browserLogoutContinuation } from './logout';
import { clusterRole, forwardedOperation, needsPrimary, NO_READ_BACK, permissionNeedsPrimary, primaryOnlyReason, statusRole, type ClusterRole } from '../lib/follower';

interface ApiContextValue {
  client: ApiClient;
  api: Endpoints;
  setCsrf: (csrf: string | undefined) => void;
}

const ApiContext = createContext<ApiContextValue | null>(null);

export function ApiProvider({
  baseUrl,
  onUnauthenticated,
  children,
}: {
  baseUrl: string;
  onUnauthenticated?: () => void;
  children: ReactNode;
}) {
  const csrf = useRef<string | undefined>(undefined);
  const unauth = useRef(onUnauthenticated);
  unauth.current = onUnauthenticated;
  const queryClient = useQueryClient();
  const qc = useRef(queryClient);
  qc.current = queryClient;
  const value = useMemo(() => {
    const client = new ApiClient({
      baseUrl,
      csrf: () => csrf.current,
      onUnauthenticated: () => unauth.current?.(),
      // Every write the dashboard sends passes here, however a page issues
      // it (useMutation or a plain await), so the follower read-back and the
      // role refresh do not depend on the calling component (D368).
      onMutation: (err, request) => afterMutation(qc.current, err, request),
    });
    return {
      client,
      api: endpoints(client),
      setCsrf: (v: string | undefined) => {
        csrf.current = v;
      },
    };
  }, [baseUrl]);
  return <ApiContext.Provider value={value}>{children}</ApiContext.Provider>;
}

export function useApiContext(): ApiContextValue {
  const v = useContext(ApiContext);
  if (!v) throw new Error('useApi outside ApiProvider');
  return v;
}

export function useApi(): Endpoints {
  return useApiContext().api;
}

/** Query keys in one place so mutations invalidate the right caches. */
export const qk = {
  me: ['auth', 'me'] as const,
  /** Why the last sign-out could not be confirmed; shown on the login page. */
  signOutNotice: ['auth', 'signOutNotice'] as const,
  authStatus: ['auth', 'status'] as const,
  tokens: ['auth', 'tokens'] as const,
  objects: (kind: string) => ['objects', kind] as const,
  policyState: (namespace: string, name: string) => ['policies', 'state', namespace, name] as const,
  clients: (...rest: unknown[]) => ['clients', ...rest] as const,
  zones: ['zones'] as const,
  zone: (z: string) => ['zones', z] as const,
  records: (z: string) => ['zones', z, 'records'] as const,
  blocking: ['blocking'] as const,
  stats: (what: string, ...rest: unknown[]) => ['stats', what, ...rest] as const,
  queries: (...rest: unknown[]) => ['queries', ...rest] as const,
  settings: ['settings'] as const,
  nodeConfig: ['node', 'config'] as const,
  schema: ['settings', 'schema'] as const,
  nodes: ['nodes'] as const,
  messages: ['messages'] as const,
  version: ['version'] as const,
  cluster: ['cluster'] as const,
  dhcp: (...rest: unknown[]) => ['dhcp', ...rest] as const,
  license: ['license'] as const,
  audit: (...rest: unknown[]) => ['audit', ...rest] as const,
  unblock: (...rest: unknown[]) => ['unblockrequests', ...rest] as const,
  /** Allowlist, Denylist and /domains pages. */
  domains: (...rest: unknown[]) => ['domains', ...rest] as const,
};

/** The signed-in session; `data` is undefined while signed out. */
export function useSession() {
  const { api, setCsrf } = useApiContext();
  const q = useQuery({
    queryKey: qk.me,
    queryFn: async ({ signal }) => {
      try {
        const me = await api.auth.me();
        // Query cancellation also retires this read's CSRF side effect, even
        // when its HTTP response arrives after session cleanup.
        signal.throwIfAborted();
        setCsrf(me.csrf);
        return me;
      } catch (err) {
        if (signal.aborted) throw err;
        if (err instanceof ApiError && err.status === 401) {
          setCsrf(undefined);
          return null;
        }
        throw err;
      }
    },
    staleTime: 60_000,
    retry: false,
  });
  return q;
}

/** Whether the session may perform verb on resource, at cluster scope or,
 * with `on`, on that zone (a zone or zone-group grant of /auth/me counts). On a
 * static host and on an enrolled follower it also says whether this node
 * carries the change out (a follower refuses a permission whose every
 * operation needs the primary); the API independently decides each request. */
export function useCan(): (resource: string, verb: string, on?: ObjectScope) => boolean {
  const { data } = useSession();
  const readOnly = useStaticMode();
  const role = useClusterRole();
  return useCallback(
    (resource: string, verb: string, on?: ObjectScope) => {
      if (!data) return false;
      if (readOnly && STATIC_VERBS.includes(verb)) return false;
      if (permissionNeedsPrimary(role, resource, verb)) return false;
      return can(data, resource, verb, on);
    },
    [data, readOnly, role],
  );
}

const STATIC_VERBS = ['write', 'delete', 'decide', 'import', 'sync', 'act'];

/**
 * Why useCan refuses resource.verb, for a disabled control's tooltip:
 * undefined when it is allowed.
 */
export function useCannotReason(): (resource: string, verb: string, on?: ObjectScope) => string | undefined {
  const { data } = useSession();
  const readOnly = useStaticMode();
  const role = useClusterRole();
  return useCallback(
    (resource: string, verb: string, on?: ObjectScope) => {
      if (!data || !can(data, resource, verb, on)) return on?.zone ? `Needs ${resource}.${verb} on zone ${on.zone}` : `Needs ${resource}.${verb}`;
      if (readOnly && STATIC_VERBS.includes(verb)) return 'Read-only in static mode';
      if (permissionNeedsPrimary(role, resource, verb)) return primaryOnlyReason(role.primary);
      return undefined;
    },
    [data, readOnly, role],
  );
}

/** The follower half of useCannotReason: the tooltip when only the primary
 * carries resource.verb out, undefined otherwise (also without the
 * permission, so a control can keep its own wording for that case). */
export function usePrimaryOnlyPermission(): (resource: string, verb: string) => string | undefined {
  const role = useClusterRole();
  return useCallback((resource: string, verb: string) => (permissionNeedsPrimary(role, resource, verb) ? primaryOnlyReason(role.primary) : undefined), [role]);
}

/** How often the capability hooks reread the node's role, so a tab left on
 * one page follows a promotion or a demotion. */
export const ROLE_POLL_MS = 30_000;

function refusedRead(query: { state: { error: unknown } }): boolean {
  return query.state.error instanceof ApiError && query.state.error.status === 403;
}

/**
 * GET /cluster, for the capability hooks and every page that shows it (use
 * this, not a useQuery of its own on qk.cluster, so each observer carries the
 * same guards). Only a session that holds cluster.read sends it: the server
 * audits every refused read as a denial, so a session without the permission
 * takes the role from the public GET /auth/status instead (useStatusRole). A
 * 403 (a permission withdrawn during the session) ends the poll and is not
 * retried when another view mounts or the window regains focus; the next
 * /auth/me read then disables the query.
 */
export function useClusterInfo() {
  const api = useApi();
  const { data: session } = useSession();
  return useQuery({
    queryKey: qk.cluster,
    queryFn: () => api.cluster.info(),
    enabled: !!session && can(session, 'cluster', 'read'),
    staleTime: 60_000,
    refetchInterval: (query) => (refusedRead(query) ? false : ROLE_POLL_MS),
    retryOnMount: (query) => !refusedRead(query),
    refetchOnWindowFocus: (query) => !refusedRead(query),
    retry: false,
  });
}

/** The public GET /auth/status, for a session that cannot read GET /cluster
 * (cluster.read is the admin role's only): a follower names itself and its
 * primary there. */
function useStatusRole(enabled: boolean) {
  const api = useApi();
  return useQuery<AuthStatus>({ queryKey: qk.authStatus, queryFn: () => api.auth.status(), enabled, staleTime: 60_000, refetchInterval: ROLE_POLL_MS, retry: false });
}

/** Host mode is a UI capability; the API independently rejects each mutation. */
export function useStaticMode(): boolean {
  return useClusterInfo().data?.readOnly === true;
}

/** This node's replication role, its primary and what a follower forwards
 * (D368), from GET /cluster; for a session without cluster.read (or when
 * /cluster fails) from GET /auth/status and the lists of the OpenAPI document
 * this dashboard was built with. Until one of them answers, nothing is gated
 * and the server decides. */
export function useClusterRole(): ClusterRole {
  return useRoleAnswer().role;
}

/** Whether GET /cluster or the GET /auth/status fallback has answered (also
 * with an error), so the role useClusterRole reports is the node's: a control
 * that opens by itself (a ?add= deep link) waits for it instead of opening
 * before a follower is known to refuse it. */
export function useClusterRoleKnown(): boolean {
  return useRoleAnswer().known;
}

function useRoleAnswer(): { role: ClusterRole; known: boolean } {
  const { data: session } = useSession();
  const cluster = useClusterInfo();
  const fallback = !!session && cluster.data === undefined && (cluster.isError || !can(session, 'cluster', 'read'));
  const status = useStatusRole(fallback);
  const info = cluster.data;
  const statusData = fallback ? status.data : undefined;
  const role = useMemo(() => (info ? clusterRole(info) : statusRole(statusData)), [info, statusData]);
  return { role, known: info !== undefined || (fallback && (status.data !== undefined || status.isError)) };
}

/** The node's role as the cached answers describe it (GET /cluster, else
 * GET /auth/status). */
function cachedRole(client: QueryClient): ClusterRole {
  const info = client.getQueryData<ClusterInfo>(qk.cluster);
  return info ? clusterRole(info) : statusRole(client.getQueryData<AuthStatus>(qk.authStatus));
}

/**
 * Called by the API client after every POST, PUT, PATCH and DELETE that got
 * an answer: a forwarded change on a follower schedules the read-back; a
 * primary_required refusal rereads the node's role at once, since the
 * dashboard may still describe it as a primary (the dashboard sends no
 * promotion; ROLE_POLL_MS follows one made with the CLI).
 */
export function afterMutation(client: QueryClient, err: ApiError | undefined, request: { method: string; path: string }): void {
  if (!err) {
    const op = forwardedOperation(request.method, request.path);
    if (op !== undefined && !NO_READ_BACK.has(op)) scheduleFollowerRefetch(client, op);
    return;
  }
  if (err.key === 'primary_required') {
    void client.invalidateQueries({ queryKey: qk.cluster, exact: true });
    void client.invalidateQueries({ queryKey: qk.authStatus, exact: true });
  }
}

/** How long after a change on a follower the dashboard reads again: the
 * follower installs its primary's commits at its next pull (every 5 s by
 * default, internal/cluster/replica/coordinator.go), so the immediate refetch
 * of a forwarded write can still show the old state. */
export const FOLLOWER_SYNC_MS = 6_000;

const followerTimers = new WeakMap<QueryClient, ReturnType<typeof setTimeout>>();

/**
 * Called for every successful forwarded change (afterMutation, with its
 * operationId): when the cached role says this node is an enrolled follower
 * that forwards the operation, refetch the active queries once more after
 * FOLLOWER_SYNC_MS (one timer per client; a later change restarts it). A
 * write the node runs itself, a read sent as POST (a DNS query) and every
 * write on a primary schedule nothing: the immediate refetch already sees
 * them.
 */
export function scheduleFollowerRefetch(client: QueryClient, operationId: string): void {
  const role = cachedRole(client);
  if (!role.follower || !role.forwarded.has(operationId)) return;
  const pending = followerTimers.get(client);
  if (pending !== undefined) clearTimeout(pending);
  followerTimers.set(
    client,
    setTimeout(() => {
      followerTimers.delete(client);
      void client.invalidateQueries({ predicate: (q) => !sameKey(q.queryKey, qk.me) && !sameKey(q.queryKey, qk.signOutNotice) && !isReview(q.queryKey) });
    }, FOLLOWER_SYNC_MS),
  );
}

/**
 * For an action that one API operation carries out: the tooltip sentence
 * when this node is a follower that answers it with 503 primary_required,
 * undefined when the node runs or forwards it.
 */
export function usePrimaryOnly(): (operationId: string) => string | undefined {
  const role = useClusterRole();
  return useCallback((operationId: string) => (needsPrimary(role, operationId) ? primaryOnlyReason(role.primary) : undefined), [role]);
}

/** The zone an action targets, for scoped grants: the zone's object name
 * and its zone group (GET /zones zoneGroup, the kudurru.io/zonegroup label). */
export interface ObjectScope {
  zone?: string | undefined;
  zoneGroup?: string | undefined;
}

function grants(perms: Record<string, string[]> | undefined, resource: string, verb: string): boolean {
  const verbs = perms?.[resource] ?? perms?.['*'];
  return !!verbs && (verbs.includes(verb) || verbs.includes('*'));
}

/** Whether me holds verb on resource at cluster scope or, with `on`, through
 * a grant at `zone:<zone>` or `zonegroup:<group>` (D385: a zone-group grant
 * covers every zone labeled with the group and their record sets). The
 * server decides each request; this only keeps the controls honest. */
export function can(me: Me, resource: string, verb: string, on?: ObjectScope): boolean {
  if (grants(me.permissions, resource, verb)) return true;
  if (!on) return false;
  const scoped = me.scopedPermissions ?? {};
  if (on.zone && grants(scoped[`zone:${on.zone}`], resource, verb)) return true;
  return !!on.zoneGroup && grants(scoped[`zonegroup:${on.zoneGroup}`], resource, verb);
}

/** Whether me holds verb on resource at cluster scope or at any of its scopes
 * (own tokens: a zone-admin holds tokens.own at its zone only). */
export function canAnyScope(me: Me, resource: string, verb: string): boolean {
  if (can(me, resource, verb)) return true;
  return Object.values(me.scopedPermissions ?? {}).some((perms) => grants(perms, resource, verb));
}

/** GET /auth/status capabilities (D387): whether this driver offers
 * authenticator (TOTP) management and the local-user commands. Until the
 * answer arrives, and from a server that does not report them, both count
 * as offered and the server decides. */
export function useAuthCapabilities(): { factorManagement: boolean; localUserCommands: boolean } {
  const api = useApi();
  const q = useQuery<AuthStatus>({ queryKey: qk.authStatus, queryFn: () => api.auth.status(), staleTime: 60_000, retry: false });
  const caps = q.data?.capabilities;
  return { factorManagement: caps?.factorManagement !== false, localUserCommands: caps?.localUserCommands !== false };
}

/**
 * Signs out of the browser session. Once the server has answered, the local
 * session ends (the server clears the cookie also when it cannot revoke the
 * session, as an enrolled follower does when its primary is unreachable);
 * when it refused or could not confirm the sign-out, its explanation is kept
 * for the login page, because the session may stay valid until it expires.
 * When the request never reached the server, nothing cleared the cookie, so
 * the session is kept and the promise rejects for the caller to show.
 */
export function useSignOut(): () => Promise<void> {
  const { api, client, setCsrf } = useApiContext();
  const qc = useQueryClient();
  return useCallback(async () => {
    let continuation: string | undefined;
    let failure: unknown;
    try {
      const result = await api.auth.browserLogout();
      continuation = browserLogoutContinuation(client.baseUrl, result?.continuePath, window.location.origin);
    } catch (err) {
      // status 0: the request never reached the server (ApiClient.request).
      if (err instanceof ApiError && err.status === 0) throw err;
      failure = err;
    }
    await qc.cancelQueries();
    setCsrf(undefined);
    qc.setQueryData(qk.signOutNotice, failure === undefined ? null : signOutNotice(failure));
    // Signed out first, so the session gate renders the login page; then
    // drop every other cached answer. qc.clear() would also remove the
    // null session and leave the gate on stale data.
    qc.setQueryData(qk.me, null);
    qc.removeQueries({ predicate: (q) => !sameKey(q.queryKey, qk.me) && !sameKey(q.queryKey, qk.signOutNotice) });
    if (continuation) window.location.assign(continuation);
  }, [api, client, qc, setCsrf]);
}

/** A wizard's operator review (['cluster', 'enrollment', 'review']): read
 * once per opening, never refetched behind the operator's back. */
function isReview(key: readonly unknown[]): boolean {
  return key[0] === 'cluster' && key[2] === 'review';
}

function sameKey(a: readonly unknown[], b: readonly unknown[]): boolean {
  return a.length === b.length && a.every((v, i) => v === b[i]);
}

function signOutNotice(err: unknown): string {
  const reason = err instanceof ApiError ? err.describe() : err instanceof Error ? err.message : String(err);
  const sentence = `${reason.charAt(0).toUpperCase()}${reason.slice(1).replace(/\.$/, '')}.`;
  // A follower's answer already says what became of the session.
  if (/valid until it expires/.test(reason)) return sentence;
  return `${sentence} The dashboard signed out, but the server did not confirm that the session ended; it may stay valid until it expires.`;
}

/** The notice useSignOut left for the login page, null when there is none. */
export function useSignOutNotice(): string | null {
  const q = useQuery<string | null>({ queryKey: qk.signOutNotice, queryFn: () => null, enabled: false, staleTime: Infinity });
  return q.data ?? null;
}

// ---------------------------------------------------------------------------
// Node scope (§10 principle 9): "all nodes" or one node, on every stats and
// log page. Single node today; the parameter is already sent.

interface ScopeContextValue extends ScopeParams {
  setScope: (s: ScopeParams) => void;
}

const ScopeContext = createContext<ScopeContextValue | null>(null);
const SCOPE_KEY = 'kudurru.scope';

export function ScopeProvider({ children }: { children: ReactNode }) {
  const [scope, setScopeState] = useState<ScopeParams>(() => {
    try {
      const raw = window.localStorage.getItem(SCOPE_KEY);
      if (raw) {
        const v = JSON.parse(raw) as ScopeParams;
        if (v.scope === 'node' && v.node) return { scope: 'node', node: v.node };
      }
    } catch {
      // ignore
    }
    return { scope: 'cluster' };
  });
  const setScope = useCallback((s: ScopeParams) => {
    setScopeState(s);
    try {
      window.localStorage.setItem(SCOPE_KEY, JSON.stringify(s));
    } catch {
      // ignore
    }
  }, []);
  const value = useMemo(() => ({ ...scope, setScope }), [scope, setScope]);
  return <ScopeContext.Provider value={value}>{children}</ScopeContext.Provider>;
}

export function useScope(): ScopeContextValue {
  const v = useContext(ScopeContext);
  if (!v) throw new Error('useScope outside ScopeProvider');
  return v;
}

/** The scope as a plain value for query keys and endpoint calls. */
export function useScopeParams(): ScopeParams {
  const { scope, node } = useScope();
  return useMemo(() => (scope === 'node' && node ? { scope, node } : { scope: 'cluster' }), [scope, node]);
}
