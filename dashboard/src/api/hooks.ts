// Shared TanStack Query hooks. Stats and log queries carry the node scope
// in their keys, so switching "all nodes" / one node refetches everything.
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { qk, useApi, useCan, useScopeParams } from './context';
import type { BlockingState, LicenseInfo } from './types';
import type { UnblockApproval, UnblockDecisionResult, UnblockDenial, UnblockRequest, UnblockRequestList } from './types';

/** GET /stats/summary feeds the tiles and the sidebar (§10 principle 1). */
export function useSummary(window = '24h') {
  const api = useApi();
  const scope = useScopeParams();
  return useQuery({
    queryKey: qk.stats('summary', scope, window),
    queryFn: () => api.stats.summary(scope, window),
    refetchInterval: 10_000,
  });
}

export function useBlocking() {
  const api = useApi();
  return useQuery({ queryKey: qk.blocking, queryFn: () => api.runtime.blocking(), refetchInterval: 15_000 });
}

/** Toggles blocking; `timer` seconds or `until` (RFC 3339) when disabling. */
export function useSetBlocking() {
  const api = useApi();
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: { blocking: boolean; timer?: number; until?: string }) =>
      v.until && !v.blocking ? api.runtime.setBlockingUntil(v.until) : api.runtime.setBlocking(v.blocking, v.timer),
    onSuccess: (state: BlockingState) => {
      qc.setQueryData(qk.blocking, state);
    },
  });
}

export function useNodes() {
  const api = useApi();
  return useQuery({ queryKey: qk.nodes, queryFn: () => api.cluster.nodes(), refetchInterval: 15_000 });
}

export function useMessages() {
  const api = useApi();
  return useQuery({ queryKey: qk.messages, queryFn: () => api.cluster.messages(), refetchInterval: 60_000 });
}

export function useVersion() {
  const api = useApi();
  return useQuery({ queryKey: qk.version, queryFn: () => api.cluster.version(), staleTime: Infinity });
}

// License -------------------------------------------------------------------

/** GET /license: the node's effective license (needs settings.read). */
export function useLicense() {
  const api = useApi();
  return useQuery({ queryKey: qk.license, queryFn: () => api.license.get(), staleTime: 60_000, refetchInterval: 5 * 60_000 });
}

/**
 * Whether a licensed feature (lib/license FEATURES) is on. While the license
 * loads, `ready` is false so callers hold their requests back; when the
 * license cannot be read (no settings.read) the feature counts as on and the
 * server's own 402 decides, so a user is never locked out by a failed lookup.
 */
export function useFeature(feature: string): { allowed: boolean; ready: boolean; license: LicenseInfo | undefined } {
  const q = useLicense();
  if (q.data) return { allowed: q.data.features.includes(feature), ready: true, license: q.data };
  return { allowed: true, ready: !q.isPending, license: undefined };
}

// Unblock requests ----------------------------------------------------------

/**
 * The number of pending unblock requests (the counts of
 * GET /unblockrequests?state=Pending&limit=1), for the navigation badge and
 * the dashboard notice. Not asked without unblockrequests.read; a 403 leaves
 * `data` undefined, so callers simply show nothing.
 */
export function usePendingUnblockCount() {
  const api = useApi();
  const can = useCan();
  const requests = useFeature('requests');
  return useQuery({
    queryKey: qk.unblock('pending'),
    queryFn: () => api.unblock.list({ state: 'Pending', limit: 1 }),
    select: (l: UnblockRequestList) => l.counts.pending,
    enabled: can('unblockrequests', 'read') && requests.ready && requests.allowed,
    refetchInterval: 60_000,
  });
}

/** Refetches every unblock query and the lists and policies a decision may change. */
function useUnblockChanged() {
  const qc = useQueryClient();
  return (r?: UnblockRequest) => {
    if (r) qc.setQueryData(qk.unblock('get', r.namespace, r.name), r);
    void qc.invalidateQueries({ queryKey: qk.unblock() });
    void qc.invalidateQueries({ queryKey: qk.objects('blocklists') });
    void qc.invalidateQueries({ queryKey: qk.objects('policies') });
  };
}

export interface UnblockTarget {
  namespace: string;
  name: string;
}

export function useApproveUnblock() {
  const api = useApi();
  const changed = useUnblockChanged();
  return useMutation({
    mutationFn: (v: UnblockTarget & { body: UnblockApproval }) => api.unblock.approve(v.namespace, v.name, v.body),
    onSuccess: (res: UnblockDecisionResult) => changed(res.request),
  });
}

export function useDenyUnblock() {
  const api = useApi();
  const changed = useUnblockChanged();
  return useMutation({
    mutationFn: (v: UnblockTarget & { body: UnblockDenial }) => api.unblock.deny(v.namespace, v.name, v.body),
    onSuccess: (res: UnblockDecisionResult) => changed(res.request),
  });
}

export function useDeleteUnblock() {
  const api = useApi();
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (v: UnblockTarget) => api.unblock.delete(v.namespace, v.name),
    onSuccess: (_r, v) => {
      // Not the deleted request itself: refetching it would answer 404.
      qc.removeQueries({ queryKey: qk.unblock('get', v.namespace, v.name) });
      void qc.invalidateQueries({ queryKey: qk.unblock('list') });
      void qc.invalidateQueries({ queryKey: qk.unblock('pending') });
    },
  });
}
