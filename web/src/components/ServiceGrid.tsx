// The blocked-services picker: every service of the catalogue
// (GET /catalogue/services, D60) as a logo tile, ordered by category; a
// tile is a toggle button (aria-pressed). IDs the catalogue in service does
// not have stay listed so saving never drops them silently. The
// safe-search engines get the same logos, read-only (EngineList).
import { useQuery } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { useApi } from '../api/context';
import type { CatalogueService, SafeSearchEngine } from '../api/types';
import { errorText } from './Feedback';
import { ServiceIcon } from './ServiceIcon';

/** Query keys of the catalogue (shared with the policy pages). */
export const catalogueServicesKey = ['catalogue', 'services'] as const;
export const catalogueSafeSearchKey = ['catalogue', 'safesearch'] as const;

export function useCatalogueServices() {
  const api = useApi();
  return useQuery({ queryKey: catalogueServicesKey, queryFn: () => api.catalogue.services(), staleTime: 5 * 60_000 });
}

export function useSafeSearchEngines() {
  const api = useApi();
  return useQuery({ queryKey: catalogueSafeSearchKey, queryFn: () => api.catalogue.safeSearch(), staleTime: 5 * 60_000 });
}

/** Groups services by category, categories and services sorted by name. */
export function groupServices(items: CatalogueService[]): [string, CatalogueService[]][] {
  const groups = new Map<string, CatalogueService[]>();
  for (const s of items) groups.set(s.category, [...(groups.get(s.category) ?? []), s]);
  return [...groups.entries()]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([c, list]) => [c, [...list].sort((a, b) => a.name.localeCompare(b.name))]);
}

const CATEGORY: Record<string, string> = { ai: 'AI', video: 'Video', social: 'Social', messaging: 'Messaging', gaming: 'Gaming', music: 'Music', dating: 'Dating' };

function categoryLabel(c: string): string {
  return CATEGORY[c] ?? c.slice(0, 1).toUpperCase() + c.slice(1);
}

export interface ServiceGridProps {
  label: string;
  value: string[];
  onChange: (ids: string[]) => void;
  isDisabled?: boolean;
  description?: string;
  /** The surrounding section already names the field. */
  isLabelHidden?: boolean;
}

export function ServiceGrid({ label, value, onChange, isDisabled, description, isLabelHidden }: ServiceGridProps) {
  const q = useCatalogueServices();
  const [filter, setFilter] = useState('');
  const items = useMemo(() => q.data?.items ?? [], [q.data]);
  const shown = useMemo(() => {
    const f = filter.trim().toLowerCase();
    return f ? items.filter((s) => s.name.toLowerCase().includes(f) || s.id.includes(f) || s.category.includes(f)) : items;
  }, [items, filter]);
  const known = new Set(items.map((s) => s.id));
  const unknown = q.data ? value.filter((v) => !known.has(v)) : [];
  const toggle = (id: string) => onChange(value.includes(id) ? value.filter((x) => x !== id) : [...value, id]);
  return (
    <fieldset className="k-fieldset" disabled={isDisabled}>
      <legend className={isLabelHidden ? 'k-visually-hidden' : undefined}>
        {label}
        <span className="k-muted k-small"> ({value.length} selected)</span>
      </legend>
      {description ? <p className="k-muted k-small">{description}</p> : null}
      {items.length > 12 ? (
        <input className="k-native-input k-search" type="search" placeholder="Filter services" aria-label="Filter services" value={filter} onChange={(e) => setFilter(e.target.value)} />
      ) : null}
      {q.isLoading ? <p className="k-muted k-small">Loading the catalogue…</p> : null}
      {q.isError ? <p className="k-error-text">Could not load the catalogue: {errorText(q.error)}</p> : null}
      <div className="k-logo-grid" role="group" aria-label={`${label}: catalogue`}>
        {groupServices(shown).flatMap(([category, services]) =>
          services.map((s) => {
            const on = value.includes(s.id);
            return (
              <button
                key={s.id}
                type="button"
                className="k-logo-tile"
                aria-pressed={on}
                disabled={isDisabled}
                onClick={() => toggle(s.id)}
                title={`${s.name}: ${s.rules} rule${s.rules === 1 ? '' : 's'} (${s.domains.slice(0, 3).join(', ')}${s.domains.length > 3 ? ', …' : ''})`}
              >
                <ServiceIcon id={s.id} name={s.name} icon={s.icon} iconOnDark={s.iconOnDark} />
                <span className="k-logo-name">{s.name}</span>
                <span className="k-logo-meta">{on ? 'Blocked' : `${categoryLabel(category)} · ${s.rules} rule${s.rules === 1 ? '' : 's'}`}</span>
              </button>
            );
          }),
        )}
      </div>
      {filter && shown.length === 0 ? <p className="k-muted k-small">No service matches "{filter}".</p> : null}
      {unknown.length ? (
        <div className="k-stack">
          <span className="k-muted k-small">Not in the catalogue (ignored until it is)</span>
          <div className="k-logo-grid">
            {unknown.map((id) => (
              <button key={id} type="button" className="k-logo-tile" aria-pressed disabled={isDisabled} onClick={() => toggle(id)}>
                <ServiceIcon id={id} name={id} />
                <span className="k-logo-name">{id}</span>
                <span className="k-logo-meta k-error-text">unknown</span>
              </button>
            ))}
          </div>
        </div>
      ) : null}
    </fieldset>
  );
}

/** The engines safe search rewrites, with their logos. */
export function EngineList({ engines, mode }: { engines: (Pick<SafeSearchEngine, 'id' | 'name' | 'icon' | 'iconOnDark'> & Partial<Pick<SafeSearchEngine, 'target' | 'moderateTarget'>>)[]; mode?: string }) {
  return (
    <ul className="k-logo-list" aria-label="Safe-search engines">
      {engines.map((e) => (
        <li key={e.id}>
          <ServiceIcon id={e.id} name={e.name} icon={e.icon} iconOnDark={e.iconOnDark} size={24} />
          <span>{e.name}</span>
          {mode === 'moderate' && e.moderateTarget ? <span className="k-muted k-small">moderate</span> : e.target ? <span className="k-muted k-small">strict</span> : null}
        </li>
      ))}
    </ul>
  );
}
