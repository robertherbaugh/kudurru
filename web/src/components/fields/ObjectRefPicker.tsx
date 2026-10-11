// Picks existing objects instead of typing their names: client groups,
// block lists, policies or zones, read from GET /objects/{kind}. `multiple`
// renders a checkbox list (with a filter once the list is long); otherwise a
// select. Values are "namespace/name" for namespaced kinds and "name" for
// zones, the form the object references use. A name that no longer exists
// stays listed (marked missing) so editing never drops it silently.
import { useQuery } from '@tanstack/react-query';
import { useId, useMemo, useState } from 'react';
import { qk, useApi } from '../../api/context';
import type { AnyObject, ObjectKind } from '../../api/types';

export interface ObjectRefPickerProps {
  kind: Exclude<ObjectKind, 'dnsrecordsets'>;
  label: string;
  description?: string;
  /** Namespace used to shorten same-namespace refs; default "default". */
  namespace?: string;
  multiple?: boolean;
  value: string[];
  onChange: (v: string[]) => void;
  /** Single mode: a first option meaning "none". */
  emptyLabel?: string;
  /** Hide objects (e.g. only block-mode lists for a "Block lists" field). */
  filter?: (o: AnyObject) => boolean;
  /** Secondary text per option (e.g. "2,852 entries"). */
  detail?: (o: AnyObject) => string | undefined;
  isDisabled?: boolean;
  error?: string;
}

export function refOf(o: AnyObject): string {
  return o.metadata.namespace ? `${o.metadata.namespace}/${o.metadata.name}` : o.metadata.name;
}

export function ObjectRefPicker({ kind, label, description, multiple, value, onChange, emptyLabel, filter, detail, isDisabled, error }: ObjectRefPickerProps) {
  const api = useApi();
  const id = useId();
  const [q, setQ] = useState('');
  const list = useQuery({ queryKey: qk.objects(kind), queryFn: () => api.objects.list<AnyObject>(kind) });
  const items = useMemo(() => (list.data?.items ?? []).filter((o) => !filter || filter(o)), [list.data, filter]);
  const known = new Set(items.map(refOf));
  const missing = value.filter((v) => !known.has(v));

  if (!multiple) {
    return (
      <label className="k-label" htmlFor={id}>
        {label}
        {description ? <span className="k-muted k-small">{description}</span> : null}
        <select id={id} className="k-native-select" value={value[0] ?? ''} disabled={isDisabled || list.isLoading} onChange={(e) => onChange(e.target.value ? [e.target.value] : [])}>
          {emptyLabel !== undefined || !value[0] ? <option value="">{emptyLabel ?? 'Choose…'}</option> : null}
          {items.map((o) => (
            <option key={refOf(o)} value={refOf(o)}>
              {refOf(o)}
              {detail?.(o) ? ` · ${detail(o)}` : ''}
            </option>
          ))}
          {missing.map((m) => (
            <option key={m} value={m}>
              {m} (missing)
            </option>
          ))}
        </select>
        {error ? <span className="k-error-text">{error}</span> : null}
      </label>
    );
  }

  const shown = q ? items.filter((o) => refOf(o).toLowerCase().includes(q.toLowerCase())) : items;
  const toggle = (ref: string, on: boolean) => onChange(on ? [...value, ref] : value.filter((v) => v !== ref));
  return (
    <fieldset className="k-fieldset" aria-describedby={description ? `${id}-d` : undefined} disabled={isDisabled}>
      <legend className="k-label">{label}</legend>
      {description ? (
        <span id={`${id}-d`} className="k-muted k-small">
          {description}
        </span>
      ) : null}
      {items.length > 8 ? <input className="k-native-input" type="search" placeholder={`Filter ${label.toLowerCase()}`} value={q} onChange={(e) => setQ(e.target.value)} aria-label={`Filter ${label}`} /> : null}
      {list.isLoading ? <span className="k-muted k-small">Loading…</span> : null}
      {!list.isLoading && !items.length && !missing.length ? <span className="k-muted k-small">None exist yet.</span> : null}
      <ul className="k-checklist">
        {shown.map((o) => {
          const ref = refOf(o);
          return (
            <li key={ref}>
              <label>
                <input type="checkbox" checked={value.includes(ref)} onChange={(e) => toggle(ref, e.target.checked)} /> <span className="k-mono">{ref}</span>
                {detail?.(o) ? <span className="k-muted k-small"> · {detail(o)}</span> : null}
              </label>
            </li>
          );
        })}
        {missing.map((m) => (
          <li key={m}>
            <label>
              <input type="checkbox" checked onChange={() => toggle(m, false)} /> <span className="k-mono">{m}</span> <span className="k-error-text">missing</span>
            </label>
          </li>
        ))}
      </ul>
      {error ? <span className="k-error-text">{error}</span> : null}
    </fieldset>
  );
}
