// React wiring for the API client, after dashboard/src/api/context.tsx: one
// ApiClient per app, the session from GET /api/v1/auth/me (the user, the
// effective roles, the permissions that drive navigation and actions, the
// CSRF token, dev mode) and sign-out. The CSRF token lives in a ref the
// client reads on every mutating request, so it never sits in a query key.
import { createContext, useCallback, useContext, useMemo, useRef, type ReactNode } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import type { Me } from '@shared/api';
import { hasPermission, type Permission } from '@shared/permissions';
import { ApiClient, ApiError } from './client';

interface ApiContextValue {
  client: ApiClient;
  setCsrf: (csrf: string | undefined) => void;
}

const ApiContext = createContext<ApiContextValue | null>(null);

export function ApiProvider({ baseUrl, onUnauthenticated, children }: { baseUrl: string; onUnauthenticated?: () => void; children: ReactNode }) {
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

/** The app's API client. */
export function useApi(): ApiClient {
  return useApiContext().client;
}

/** The query key of the session (GET /api/v1/auth/me). */
export const SESSION_KEY = ['authMe'] as const;

/** The signed-in session; `data` is null while signed out and undefined while it loads. */
export function useSession() {
  const { client, setCsrf } = useApiContext();
  return useQuery({
    queryKey: SESSION_KEY,
    queryFn: async (): Promise<Me | null> => {
      try {
        const me = await client.call('authMe');
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
}

/** The signed-in person (inside the session gate only). */
export function useMe(): Me {
  const { data } = useSession();
  if (!data) throw new Error('useMe outside the session gate');
  return data;
}

/** Whether the session holds a permission (Me.permissions, shared/permissions.ts). */
export function useCan(): (p: Permission) => boolean {
  const { data } = useSession();
  return useCallback((p: Permission) => (data ? hasPermission(data.permissions, p) : false), [data]);
}

/** POST /auth/logout, then the gate shows the sign-in page and every cached answer is dropped. */
export function useSignOut(): () => Promise<void> {
  const { client, setCsrf } = useApiContext();
  const qc = useQueryClient();
  return useCallback(async () => {
    try {
      await client.call('authLogout');
    } catch {
      // The session may already be gone; signing out locally still holds.
    } finally {
      setCsrf(undefined);
      // Signed out first, so the gate renders the sign-in page; then drop
      // every other cached answer (another person may sign in next).
      qc.setQueryData(SESSION_KEY, null);
      qc.removeQueries({ predicate: (q) => q.queryKey[0] !== SESSION_KEY[0] });
    }
  }, [client, qc, setCsrf]);
}
