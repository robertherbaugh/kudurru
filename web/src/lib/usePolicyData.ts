// Data hooks of the policy, group and client pages: the objects every
// derivation needs (policies, groups, lists and the catalogue) and a
// policy's current state from GET /policies/{ns}/{name}/state (ux-plan
// C3), falling back to the state derived from the objects when the server
// does not serve that route yet (404/405/501 for an existing policy).
import { useQuery } from '@tanstack/react-query';
import { useMemo } from 'react';
import { ApiError } from '../api/client';
import { qk, useApi } from '../api/context';
import type { BlockList, ClientGroup, Policy, PolicyState } from '../api/types';
import { useCatalogueServices, useSafeSearchEngines } from '../components/ServiceGrid';
import { derivePolicyState, type DeriveContext } from './policies';
import { useUpstreamGroups } from './upstreams';

export function usePolicies() {
  const api = useApi();
  return useQuery({ queryKey: qk.objects('policies'), queryFn: () => api.objects.list<Policy>('policies') });
}

export function useGroups() {
  const api = useApi();
  return useQuery({ queryKey: qk.objects('clientgroups'), queryFn: () => api.objects.list<ClientGroup>('clientgroups') });
}

export function useLists() {
  const api = useApi();
  return useQuery({ queryKey: qk.objects('blocklists'), queryFn: () => api.objects.list<BlockList>('blocklists') });
}

/** Everything derivePolicyState reads; `ready` once the objects loaded. */
export function useDeriveContext(): { ctx: DeriveContext; ready: boolean; error: unknown } {
  const pols = usePolicies();
  const groups = useGroups();
  const lists = useLists();
  const services = useCatalogueServices();
  const engines = useSafeSearchEngines();
  const upstreams = useUpstreamGroups();
  const ctx = useMemo<DeriveContext>(
    () => ({
      policies: pols.data?.items ?? [],
      groups: groups.data?.items ?? [],
      lists: lists.data?.items ?? [],
      services: services.data?.items ?? [],
      engines: engines.data?.items ?? [],
      ...(upstreams.available ? { upstreamGroups: upstreams.names } : {}),
      now: new Date(),
    }),
    [pols.data, groups.data, lists.data, services.data, engines.data, upstreams.available, upstreams.names],
  );
  return { ctx, ready: !!pols.data && !!groups.data && !!lists.data, error: pols.error ?? groups.error ?? lists.error };
}

/** Whether an error means "this server has no state route" rather than a real failure. */
export function stateUnsupported(err: unknown): boolean {
  return err instanceof ApiError && (err.status === 404 || err.status === 405 || err.status === 501);
}

export interface PolicyStateResult {
  state: PolicyState | undefined;
  /** True when the state was derived from the objects (no statistics). */
  derived: boolean;
  isLoading: boolean;
  error: unknown;
}

export function usePolicyState(namespace: string, name: string, policy: Policy | undefined): PolicyStateResult {
  const api = useApi();
  const q = useQuery({
    queryKey: qk.policyState(namespace, name),
    queryFn: () => api.policies.state(namespace, name),
    enabled: !!policy,
    refetchInterval: 30_000,
  });
  const { ctx, ready } = useDeriveContext();
  const fallback = !!policy && q.isError && stateUnsupported(q.error);
  const derived = useMemo(() => (fallback && policy && ready ? derivePolicyState(policy, ctx) : undefined), [fallback, policy, ready, ctx]);
  if (q.data) return { state: q.data, derived: false, isLoading: false, error: null };
  if (fallback) return { state: derived, derived: true, isLoading: !derived, error: null };
  return { state: undefined, derived: false, isLoading: q.isLoading || !policy, error: q.error };
}
