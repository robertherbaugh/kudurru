// The reads of the Integrations pages. Nothing is asked without
// integrations.read (the server would audit a refused read); status is
// reread while it settles. Every key starts with INTEGRATIONS_KEY so a
// write invalidates them together.
import { useQueries, useQuery } from '@tanstack/react-query';
import { useApi, useSession, can } from '../api/context';
import type { IntegrationSummary } from '../api/types';

export const INTEGRATIONS_KEY = ['integrations'] as const;
export const STATUS_POLL_MS = 10_000;

export function useCanReadIntegrations(): boolean {
  const { data } = useSession();
  return !!data && can(data, 'integrations', 'read');
}

export function useIntegrationManifests() {
  const api = useApi();
  const enabled = useCanReadIntegrations();
  return useQuery({ queryKey: [...INTEGRATIONS_KEY, 'manifests'], queryFn: () => api.integrations.manifests(), enabled, staleTime: 60_000, retry: false });
}

export function useIntegrationObjects() {
  const api = useApi();
  const enabled = useCanReadIntegrations();
  return useQuery({ queryKey: [...INTEGRATIONS_KEY, 'objects'], queryFn: () => api.integrations.objects(), enabled, refetchInterval: STATUS_POLL_MS, retry: false });
}

/** GET /integrations for each namespace, merged by namespace/name. */
export function useIntegrationSummaries(namespaces: string[]) {
  const api = useApi();
  const enabled = useCanReadIntegrations();
  return useQueries({
    queries: namespaces.map((ns) => ({
      queryKey: [...INTEGRATIONS_KEY, 'summaries', ns],
      queryFn: () => api.integrations.list(ns),
      enabled,
      refetchInterval: STATUS_POLL_MS,
      retry: false,
    })),
    combine: (qs) => {
      const byKey = new Map<string, IntegrationSummary>();
      for (const q of qs) for (const s of q.data?.items ?? []) byKey.set(`${s.namespace}/${s.name}`, s);
      return { byKey, error: qs.find((q) => q.error)?.error ?? null, isLoading: qs.some((q) => q.isLoading) };
    },
  });
}

export function useIntegrationSummary(namespace: string, name: string) {
  const api = useApi();
  const enabled = useCanReadIntegrations();
  return useQuery({ queryKey: [...INTEGRATIONS_KEY, 'summary', namespace, name], queryFn: () => api.integrations.get(namespace, name), enabled, refetchInterval: STATUS_POLL_MS, retry: false });
}

export function useIntegrationObject(namespace: string, name: string) {
  const api = useApi();
  const enabled = useCanReadIntegrations();
  return useQuery({ queryKey: [...INTEGRATIONS_KEY, 'object', namespace, name], queryFn: () => api.integrations.object(namespace, name), enabled, refetchInterval: STATUS_POLL_MS, retry: false });
}
