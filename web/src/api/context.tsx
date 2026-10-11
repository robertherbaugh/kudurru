// React wiring for the API client: one ApiClient per app, the session from
// GET /auth/me (principal, permission matrix, CSRF token) and the node
// scope every stats/log call carries.
import { createContext, useCallback, useContext, useMemo, useRef, useState, type ReactNode } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { ApiClient, ApiError } from './client';
import { endpoints, type Endpoints, type ScopeParams } from './endpoints';
import type { Me } from './types';

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
  const value = useMemo(() => {
    const client = new ApiClient({
      baseUrl,
      csrf: () => csrf.current,
      onUnauthenticated: () => unauth.current?.(),
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
  authStatus: ['auth', 'status'] as const,
  tokens: ['auth', 'tokens'] as const,
  objects: (kind: string) => ['objects', kind] as const,
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
  audit: (...rest: unknown[]) => ['audit', ...rest] as const,
};

/** The signed-in session; `data` is undefined while signed out. */
export function useSession() {
  const { api, setCsrf } = useApiContext();
  const q = useQuery({
    queryKey: qk.me,
    queryFn: async () => {
      try {
        const me = await api.auth.me();
        setCsrf(me.csrf);
        return me;
      } catch (err) {
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

/** Whether the session may perform verb on resource (cluster scope). */
export function useCan(): (resource: string, verb: string) => boolean {
  const { data } = useSession();
  return useCallback(
    (resource: string, verb: string) => {
      if (!data) return false;
      return can(data, resource, verb);
    },
    [data],
  );
}

export function can(me: Me, resource: string, verb: string): boolean {
  const verbs = me.permissions[resource] ?? me.permissions['*'];
  return !!verbs && (verbs.includes(verb) || verbs.includes('*'));
}

export function useSignOut(): () => Promise<void> {
  const { api, setCsrf } = useApiContext();
  const qc = useQueryClient();
  return useCallback(async () => {
    try {
      await api.auth.logout();
    } finally {
      setCsrf(undefined);
      // Signed out first, so the session gate renders the login page; then
      // drop every other cached answer. qc.clear() would also remove the
      // null session and leave the gate on stale data.
      qc.setQueryData(qk.me, null);
      qc.removeQueries({ predicate: (q) => q.queryKey[0] !== qk.me[0] || q.queryKey[1] !== qk.me[1] });
    }
  }, [api, qc, setCsrf]);
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
