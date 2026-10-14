// Data hooks of the policy, group and client pages: the objects every
// derivation needs (policies, groups, lists and the catalogue) and a
// policy's current state from GET /policies/{ns}/{name}/state (ux-plan
// C3), falling back to the state derived from the objects when the server
// does not serve that route yet (404/405/501 for an existing policy).
import { useQuery } from '@tanstack/react-query';
import { useMemo } from 'react';
import { ApiError } from '../api/client';
import { qk, useApi } from '../api/context';
import { useLicense } from '../api/hooks';
import type { BlockList, ClientGroup, Policy, PolicyState } from '../api/types';
import { useCatalogueCategories, useCatalogueServices, useSafeSearchEngines } from '../components/ServiceGrid';
import { policyLimitNotice } from './license';
import { DEFAULT_NAMESPACE, derivePolicyState, type DeriveContext } from './policies';
import { useUpstreamGroups } from './upstreams';

export function usePolicies() {
  const api = useApi();
  return useQuery({ queryKey: qk.objects('policies'), queryFn: () => api.objects.list<Policy>('policies') });
}

/**
 * Why no policy can be created now, or null: the license's policy limit (one
 * without a key, the seeded default) is reached. The server counts every
 * policy and answers 402; "New policy" is disabled before that.
 */
export function usePolicyLimit(policies: Policy[]): { title: string; description: string } | null {
  const limit = useLicense().data?.policyLimit ?? 0;
  return useMemo(() => (limit > 0 && policies.length >= limit ? policyLimitNotice(limit, policies.map((p) => p.metadata.name)) : null), [limit, policies]);
}

/**
 * Whether only the default policy can be created: without a valid license
 * key (policyLimit > 0) the server creates no Policy but default in its default namespace
 * (internal/license CommunityPolicyName), so the wizard names it for the user.
 */
export function useOnlyDefaultPolicy(): { namespace: string } | null {
  const l = useLicense().data;
  // The default policy lives in the server's default namespace: "default" in
  // Docker, the release namespace in Kubernetes (GET /license defaultNamespace).
  return l && l.policyLimit > 0 ? { namespace: l.defaultNamespace || DEFAULT_NAMESPACE } : null;
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
  const categories = useCatalogueCategories();
  const engines = useSafeSearchEngines();
  const upstreams = useUpstreamGroups();
  const ctx = useMemo<DeriveContext>(
    () => ({
      policies: pols.data?.items ?? [],
      groups: groups.data?.items ?? [],
      lists: lists.data?.items ?? [],
      services: services.data?.items ?? [],
      categories: categories.data?.items ?? [],
      engines: engines.data?.items ?? [],
      ...(upstreams.available ? { upstreamGroups: upstreams.names } : {}),
      now: new Date(),
    }),
    [pols.data, groups.data, lists.data, services.data, categories.data, engines.data, upstreams.available, upstreams.names],
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
