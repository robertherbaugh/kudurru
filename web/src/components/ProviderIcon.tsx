// A provider's logo (upstream presets) by icon id: the first of `ids` the
// server's icon index (C4: the public GET /catalogue/icons, with an
// optional dark-background variant; homarr-labs/dashboard-icons,
// Apache-2.0) has, drawn by ServiceIcon, the one logo/monogram component;
// a monogram when the server has none. Decorative: the provider's name is
// always next to it.
import { useQuery } from '@tanstack/react-query';
import { useApi } from '../api/context';
import { ServiceIcon } from './ServiceIcon';

export { monogram } from './ServiceIcon';

/** Icon id → its path and dark-background variant path; empty when the server has no index. */
export function useIcons(): Map<string, { path: string; onDark?: string }> {
  const api = useApi();
  const q = useQuery({
    queryKey: ['catalogue', 'icons'],
    queryFn: async () => {
      try {
        return (await api.catalogue.icons()).items.map((i) => [i.id, { path: i.path, ...(i.onDark ? { onDark: i.onDark } : {}) }] as const);
      } catch {
        return [];
      }
    },
    staleTime: Infinity,
  });
  return new Map(q.data ?? []);
}

export function ProviderIcon({ name, ids }: { name: string; ids: string[] }) {
  const icons = useIcons();
  const id = ids.find((i) => icons.has(i));
  const e = id ? icons.get(id) : undefined;
  return (
    <span className="k-provider-icon" aria-hidden="true">
      <ServiceIcon id={id ?? ids[0] ?? name.toLowerCase()} name={name} icon={e?.path} iconOnDark={e?.onDark} size={28} />
    </span>
  );
}
