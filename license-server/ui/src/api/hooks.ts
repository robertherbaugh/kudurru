// TanStack Query hooks for the portal's routes. Every hook names a route
// of shared/routes.ts and is typed by shared/api.ts (ResponseOf for the
// answer, BodyOf for a mutation's body), so pages never spell a URL or a
// wire type. Query keys are [routeId, params, query]; a successful
// mutation refetches what is on screen (everything but the session, the
// catalog and the sign-in config), which keeps lists, detail pages, counts
// and the audit trail in step without a hand-kept invalidation map.
import { useInfiniteQuery, useMutation, useQuery, useQueryClient, type UseQueryOptions } from '@tanstack/react-query';
import type { AuditQuery, CustomerListQuery, LicenseListQuery, RequestListQuery, ResponseOf, UserListQuery } from '@shared/api';
import type { RouteId } from '@shared/routes';
import type { ApiClient, CallArgs } from './client';
import { SESSION_KEY, useApi, useCan } from './context';

/** client.call for a route id known only as a type parameter (the rest-tuple overload cannot be resolved generically). */
function callRoute<Id extends RouteId>(client: ApiClient, id: Id, args: CallArgs<Id>): Promise<ResponseOf<Id>> {
  const call = client.call.bind(client) as (id: Id, args: CallArgs<Id>) => Promise<ResponseOf<Id>>;
  return call(id, args);
}

/** The query key of a route call. */
export function routeKey(id: RouteId, params?: object | null, query?: object | null): readonly unknown[] {
  return [id, params ?? null, query ?? null];
}

type QueryOpts<Id extends RouteId> = Omit<UseQueryOptions<ResponseOf<Id>, Error, ResponseOf<Id>, readonly unknown[]>, 'queryKey' | 'queryFn'>;

/** A GET (or a read-only POST such as the invite preview) as a query. */
export function useRouteQuery<Id extends RouteId>(id: Id, args: CallArgs<Id>, opts: QueryOpts<Id> = {}) {
  const client = useApi();
  const a = args as { params?: object; query?: object; body?: object };
  return useQuery({
    queryKey: routeKey(id, a.params, a.query ?? a.body),
    queryFn: ({ signal }) => callRoute(client, id, { ...args, signal }),
    ...opts,
  });
}

/** Queries that a mutation never makes stale. */
const STABLE: ReadonlySet<unknown> = new Set<unknown>([SESSION_KEY[0], 'catalog', 'authConfig', 'myInvitePreview']);

export interface RouteMutationOptions {
  /** Also refetch the session (a change to the signed-in person's own roles). */
  refreshSession?: boolean;
}

/** A mutating route call; on success everything on screen refetches. */
export function useRouteMutation<Id extends RouteId>(id: Id, opts: RouteMutationOptions = {}) {
  const client = useApi();
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (args: CallArgs<Id>) => callRoute(client, id, args),
    onSuccess: () => {
      // Not awaited: the caller's onSuccess (close, navigate, show the invite link) runs at once.
      void qc.invalidateQueries({ predicate: (q) => !STABLE.has(q.queryKey[0]) || (opts.refreshSession === true && q.queryKey[0] === SESSION_KEY[0]) });
    },
  });
}

// --- the session's shared data -------------------------------------------------

/** GET /api/v1/auth/config: the sign-in button's label and dev mode (anonymous). */
export function useAuthConfig() {
  return useRouteQuery('authConfig', {}, { staleTime: 5 * 60_000, retry: 1 });
}

/** GET /api/v1/catalog: plans, features, policy, signing and mail state, today (UTC). */
export function useCatalog() {
  return useRouteQuery('catalog', {}, { staleTime: 5 * 60_000 });
}

// --- the signed-in person's own licenses ---------------------------------------

export function useMyLicenses() {
  return useRouteQuery('myLicenses', {});
}

export function useMyLicense(id: string) {
  return useRouteQuery('myLicense', { params: { id } });
}

export function useMyRequests() {
  return useRouteQuery('myRequests', {});
}

/** POST /api/v1/me/invites/preview: names the customer before the person accepts (changes nothing). */
export function useInvitePreview(token: string) {
  return useRouteQuery('myInvitePreview', { body: { token } }, { retry: false, staleTime: Infinity });
}

// --- the one-click approval link (anonymous) -----------------------------------

/**
 * POST /api/v1/public/approvals/preview: the request and the terms behind
 * an approval link (changes nothing; deciding is a separate POST from the
 * page). Asked once: a refetch on focus would only spend the per-address
 * rate limit, and the page shows the decision it made itself.
 */
export function useApprovalPreview(token: string) {
  return useRouteQuery('approvalPreview', { body: { token } }, { retry: false, staleTime: Infinity, refetchOnWindowFocus: false, enabled: token !== '' });
}

// --- licenses, customers, requests (admin) -------------------------------------

export function useLicenses(query: LicenseListQuery, opts: QueryOpts<'licensesList'> = {}) {
  return useRouteQuery('licensesList', { query }, { placeholderData: (prev) => prev, ...opts });
}

export function useLicense(id: string) {
  return useRouteQuery('licenseGet', { params: { id } });
}

export function useCustomers(query: CustomerListQuery, opts: QueryOpts<'customersList'> = {}) {
  return useRouteQuery('customersList', { query }, { placeholderData: (prev) => prev, ...opts });
}

export function useCustomer(id: string, opts: QueryOpts<'customerGet'> = {}) {
  return useRouteQuery('customerGet', { params: { id } }, { ...opts, enabled: Boolean(id) && opts.enabled !== false });
}

export function useRequests(query: RequestListQuery) {
  return useRouteQuery('requestsList', { query });
}

/** The number of open requests for the navigation badge; nothing is asked without requests.read. */
export function useOpenRequestCount() {
  const can = useCan();
  return useRouteQuery('requestsList', { query: { status: 'open' } }, { enabled: can('requests.read'), refetchInterval: 60_000 });
}

export function useBlockedEmails(enabled = true) {
  return useRouteQuery('blockedEmailsList', {}, { enabled });
}

// --- users, audit, system -------------------------------------------------------

export function useUsers(query: UserListQuery) {
  return useRouteQuery('usersList', { query }, { placeholderData: (prev) => prev });
}

export function useUser(id: string) {
  return useRouteQuery('userGet', { params: { id } });
}

/** GET /api/v1/audit, newest first, one page at a time ("Load older events" follows `next`). */
export function useAuditPages(query: Omit<AuditQuery, 'before'>, enabled = true) {
  const client = useApi();
  return useInfiniteQuery({
    queryKey: routeKey('auditList', null, query),
    queryFn: ({ pageParam, signal }) => client.call('auditList', { query: { ...query, ...(pageParam ? { before: pageParam } : {}) }, signal }),
    initialPageParam: '',
    getNextPageParam: (last) => last.next,
    enabled,
  });
}

export function useSystem() {
  return useRouteQuery('system', {});
}
