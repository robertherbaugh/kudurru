// A provider's logo from the icons the binary embeds (C4: the public
// GET /catalogue/icons lists them with an optional dark-background
// variant, /catalogue/icons/{id}.svg serves them; homarr-labs/
// dashboard-icons, Apache-2.0), or a monogram when the server has no icon
// for it. Decorative: the provider's name is always next to it.
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { useApi } from '../api/context';
import { useAppearance } from '../lib/theme';

/** Icon id → the id of its dark-background variant ("" when none); empty when the server has no index. */
export function useIcons(): Map<string, string> {
  const api = useApi();
  const q = useQuery({
    queryKey: ['catalogue', 'icons'],
    queryFn: async () => {
      try {
        return (await api.catalogue.icons()).items.map((i) => [i.id, i.onDark ? i.onDark.replace(/^.*\//, '').replace(/\.svg$/, '') : ''] as const);
      } catch {
        return [];
      }
    },
    staleTime: Infinity,
  });
  return new Map(q.data ?? []);
}

/** The first two letters of the name's words: "Control D" → "CD", "Quad9" → "Q9". */
export function monogram(name: string): string {
  const words = name.split(/[\s-]+/).filter(Boolean);
  if (words.length > 1) return `${words[0]?.[0] ?? ''}${words[1]?.[0] ?? ''}`.toUpperCase();
  const w = words[0] ?? '?';
  return (w[0] ?? '?').toUpperCase() + (/\d/.exec(w)?.[0] ?? w[1] ?? '');
}

function useDark(): boolean {
  const { mode } = useAppearance();
  if (mode !== 'system') return mode === 'dark';
  return typeof window !== 'undefined' && !!window.matchMedia?.('(prefers-color-scheme: dark)').matches;
}

export function ProviderIcon({ name, ids }: { name: string; ids: string[] }) {
  const api = useApi();
  const icons = useIcons();
  const dark = useDark();
  const [broken, setBroken] = useState(false);
  const id = ids.find((i) => icons.has(i));
  const shown = id && dark && icons.get(id) ? (icons.get(id) as string) : id;
  return (
    <span className="k-provider-icon" aria-hidden="true" data-icon={shown && !broken ? shown : 'monogram'}>
      {shown && !broken ? <img src={api.catalogue.iconURL(shown)} alt="" width={28} height={28} onError={() => setBroken(true)} /> : monogram(name)}
    </span>
  );
}
