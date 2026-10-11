// A provider's logo from the icons the binary embeds (C4: GET
// /catalogue/icons lists them, /catalogue/icons/{id}.svg serves them,
// taken from homarr-labs/dashboard-icons), or a monogram when the server
// has no icon for it (or no icon index at all). Decorative: the provider's
// name is always next to it.
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { useApi, useCan } from '../api/context';
import { catalogueServicesKey } from './ServicesField';

/**
 * The icon ids this server embeds; empty when it has none. The index is
 * only asked for when the catalogue's services carry icons (C4), so a
 * server without them never answers a 404 the browser would log.
 */
export function useIconIds(): Set<string> {
  const api = useApi();
  const can = useCan();
  const services = useQuery({ queryKey: catalogueServicesKey, queryFn: () => api.catalogue.services(), staleTime: 5 * 60_000, enabled: can('policies', 'read') });
  const hasIcons = !!services.data?.items.some((s) => (s as { icon?: string }).icon);
  const q = useQuery({
    queryKey: ['catalogue', 'icons'],
    enabled: hasIcons,
    queryFn: async () => {
      try {
        return (await api.catalogue.icons()).items.map((i) => i.id);
      } catch {
        return [];
      }
    },
    staleTime: Infinity,
  });
  return new Set(q.data ?? []);
}

/** The first two letters of the name's words: "Control D" → "CD", "Quad9" → "Q9". */
export function monogram(name: string): string {
  const words = name.split(/[\s-]+/).filter(Boolean);
  if (words.length > 1) return `${words[0]?.[0] ?? ''}${words[1]?.[0] ?? ''}`.toUpperCase();
  const w = words[0] ?? '?';
  return (w[0] ?? '?').toUpperCase() + (/\d/.exec(w)?.[0] ?? w[1] ?? '');
}

export function ProviderIcon({ name, ids }: { name: string; ids: string[] }) {
  const api = useApi();
  const known = useIconIds();
  const [broken, setBroken] = useState(false);
  const id = ids.find((i) => known.has(i));
  return (
    <span className="k-provider-icon" aria-hidden="true" data-icon={id && !broken ? id : 'monogram'}>
      {id && !broken ? <img src={api.catalogue.iconURL(id)} alt="" width={28} height={28} onError={() => setBroken(true)} /> : monogram(name)}
    </span>
  );
}
