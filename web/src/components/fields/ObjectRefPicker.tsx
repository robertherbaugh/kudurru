// Picks existing objects instead of typing their names: client groups,
// block lists, policies or zones, read from GET /objects/{kind}. `multiple`
// renders an Astryx CheckboxList, or a MultiSelector with search once there
// are more than seven; otherwise a Selector (searchable when long). Values are "namespace/name" for namespaced kinds and "name" for
// zones; with `sameNamespace` they are bare names of objects in
// `namespace`, the form Policy references take (clientGroupRefs,
// blockListRefs: names in the policy's own namespace). A name that no
// longer exists stays listed (marked missing) so editing never drops it
// silently.
import { useQuery } from '@tanstack/react-query';
import { Badge } from '@astryxdesign/core/Badge';
import { CheckboxList, CheckboxListItem } from '@astryxdesign/core/CheckboxList';
import { MultiSelector } from '@astryxdesign/core/MultiSelector';
import { useMemo } from 'react';
import { qk, useApi } from '../../api/context';
import type { AnyObject, ObjectKind } from '../../api/types';
import { FieldShell } from './FieldHelp';
import { SelectField, type SelectOption } from './SelectField';

export interface ObjectRefPickerProps {
  kind: Exclude<ObjectKind, 'dnsrecordsets'>;
  label: string;
  /** Help under the field. */
  description?: string;
  /** The namespace of `sameNamespace` refs; default "default". */
  namespace?: string;
  /** Offer only objects of `namespace` and use their bare names as values. */
  sameNamespace?: boolean;
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

/** Up to this many choices show as a checkbox list; more get a MultiSelector with search. */
const CHECKLIST_MAX = 7;

export function ObjectRefPicker({ kind, label, description, namespace = 'default', sameNamespace, multiple, value, onChange, emptyLabel, filter, detail, isDisabled, error }: ObjectRefPickerProps) {
  const api = useApi();
  const list = useQuery({ queryKey: qk.objects(kind), queryFn: () => api.objects.list<AnyObject>(kind) });
  const items = useMemo(
    () => (list.data?.items ?? []).filter((o) => (!filter || filter(o)) && (!sameNamespace || (o.metadata.namespace ?? 'default') === namespace)),
    [list.data, filter, sameNamespace, namespace],
  );
  const keyOf = (o: AnyObject) => (sameNamespace ? o.metadata.name : refOf(o));
  const known = new Set(items.map(keyOf));
  const missing = value.filter((v) => !known.has(v));
  const state = list.isLoading ? 'Loading…' : !items.length && !missing.length ? 'None exist yet.' : '';
  const help =
    description || state ? (
      <>
        {description}
        {description && state ? ' ' : ''}
        {state}
      </>
    ) : undefined;

  if (!multiple) {
    const options: SelectOption[] = [
      ...items.map((o) => ({ value: keyOf(o), label: keyOf(o), detail: detail?.(o) })),
      ...missing.map((m) => ({ value: m, label: `${m} (missing)` })),
    ];
    return (
      <SelectField
        label={label}
        value={value[0] ?? ''}
        options={options}
        emptyLabel={emptyLabel ?? (value[0] ? undefined : 'Choose…')}
        onChange={(v) => onChange(v ? [v] : [])}
        isDisabled={isDisabled}
        isLoading={list.isLoading}
        hasSearch={options.length > 8}
        help={help}
        error={error}
      />
    );
  }

  if (items.length + missing.length > CHECKLIST_MAX) {
    return (
      <FieldShell help={help} error={error}>
        {(status) => (
          <MultiSelector
            label={label}
            value={value}
            onChange={(v) => onChange(v)}
            options={[
              ...items.map((o) => ({ value: keyOf(o), label: keyOf(o), description: detail?.(o) })),
              ...missing.map((m) => ({ value: m, label: `${m} (missing)` })),
            ]}
            hasSearch
            searchPlaceholder={`Filter ${label.toLowerCase()}`}
            triggerDisplay="badges"
            placeholder="None"
            isDisabled={isDisabled}
            isLoading={list.isLoading}
            status={status}
          />
        )}
      </FieldShell>
    );
  }

  return (
    <FieldShell help={help} error={error} describes="group">
      {(status) => (
        <CheckboxList label={label} value={value} onChange={onChange} isDisabled={isDisabled} status={status}>
          {items.map((o) => (
            <CheckboxListItem key={keyOf(o)} value={keyOf(o)} label={keyOf(o)} description={detail?.(o)} />
          ))}
          {missing.map((m) => (
            <CheckboxListItem key={m} value={m} label={m} endContent={<Badge variant="error" label="missing" />} />
          ))}
        </CheckboxList>
      )}
    </FieldShell>
  );
}
