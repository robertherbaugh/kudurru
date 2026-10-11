// Shared TanStack Query hooks. Stats and log queries carry the node scope
// in their keys, so switching "all nodes" / one node refetches everything.
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { qk, useApi, useScopeParams } from './context';
import type { BlockingState } from './types';

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
