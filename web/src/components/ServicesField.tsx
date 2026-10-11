// The Policy form's blocked-services picker: the services of the
// blocked-services catalogue (GET /catalogue/services, D60) as checkboxes
// grouped by category. IDs the catalogue in service does not have stay
// listed so saving the form never drops them silently.
import { CheckboxInput } from '@astryxdesign/core/CheckboxInput';
import { useQuery } from '@tanstack/react-query';
import { useApi } from '../api/context';
import type { CatalogueService } from '../api/types';
import { errorText } from './Feedback';

/** Query key of GET /catalogue/services. */
export const catalogueServicesKey = ['catalogue', 'services'] as const;

export interface ServicesFieldProps {
  label: string;
  help?: string | undefined;
  value: string[];
  onChange: (v: unknown) => void;
  disabled: boolean;
}

/** Groups services by category, categories and services sorted by name. */
export function groupServices(items: CatalogueService[]): [string, CatalogueService[]][] {
  const groups = new Map<string, CatalogueService[]>();
  for (const s of items) groups.set(s.category, [...(groups.get(s.category) ?? []), s]);
  return [...groups.entries()]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([c, list]) => [c, [...list].sort((a, b) => a.name.localeCompare(b.name))]);
}

export function ServicesField({ label, help, value, onChange, disabled }: ServicesFieldProps) {
  const api = useApi();
  const q = useQuery({ queryKey: catalogueServicesKey, queryFn: () => api.catalogue.services(), staleTime: 5 * 60_000 });
  const items = q.data?.items ?? [];
  const known = new Set(items.map((s) => s.id));
  const unknown = q.data ? value.filter((v) => !known.has(v)) : [];
  const toggle = (id: string, on: boolean) => onChange(on ? [...value, id] : value.filter((x) => x !== id));
  return (
    <fieldset className="k-fieldset">
      <legend>
        {label}
        {value.length ? <span className="k-muted k-small"> ({value.length} selected)</span> : null}
      </legend>
      {help ? <p className="k-muted k-small">{help}</p> : null}
      {q.isLoading ? <p className="k-muted k-small">Loading…</p> : null}
      {q.isError ? <p className="k-error-text">Could not load the catalogue: {errorText(q.error)}</p> : null}
      {groupServices(items).map(([category, services]) => (
        <div key={category} className="k-stack">
          <span className="k-muted k-small">{category}</span>
          <div className="k-check-grid">
            {services.map((s) => (
              <CheckboxInput
                key={s.id}
                label={s.name}
                description={`${s.rules} domain${s.rules === 1 ? '' : 's'}`}
                value={value.includes(s.id)}
                isDisabled={disabled}
                onChange={(on) => toggle(s.id, on)}
              />
            ))}
          </div>
        </div>
      ))}
      {unknown.length ? (
        <div className="k-stack">
          <span className="k-muted k-small">Not in the catalogue (ignored until it is)</span>
          <div className="k-check-grid">
            {unknown.map((id) => (
              <CheckboxInput key={id} label={id} value isDisabled={disabled} onChange={(on) => toggle(id, on)} />
            ))}
          </div>
        </div>
      ) : null}
    </fieldset>
  );
}
