// Picks an existing customer: an Astryx Typeahead searching
// GET /api/v1/customers?q= (name, contact email and member emails) as the
// admin types, the first customers offered on focus, the help and the
// error under it (FieldHelp). `kind` limits the choice (an approval only
// goes to a person customer). The value is the chosen customer or null.
import { Typeahead, TypeaheadItem } from '@astryxdesign/core/Typeahead';
import type { SearchableItem, SearchSource } from '@astryxdesign/core/Typeahead';
import { useMemo, type ReactNode } from 'react';
import type { Customer, CustomerKind } from '@shared/api';
import { useApi } from '../../api/context';
import { FieldShell } from './FieldHelp';

/** A customer as the field holds it. */
export interface CustomerChoice {
  id: string;
  name: string;
  kind: CustomerKind;
  contactEmail: string | null;
}

type Item = SearchableItem<CustomerChoice>;

function toItem(c: CustomerChoice): Item {
  return { id: c.id, label: c.name, auxiliaryData: c };
}

export function customerChoice(c: Pick<Customer, 'id' | 'name' | 'kind' | 'contactEmail'>): CustomerChoice {
  return { id: c.id, name: c.name, kind: c.kind, contactEmail: c.contactEmail };
}

const KIND_LABEL: Record<CustomerKind, string> = { person: 'Person', organization: 'Organisation' };

export interface CustomerFieldProps {
  label?: string;
  value: CustomerChoice | null;
  onChange: (c: CustomerChoice | null) => void;
  /** Offer only customers of this kind. */
  kind?: CustomerKind | undefined;
  help?: ReactNode;
  error?: string | undefined;
  isRequired?: boolean | undefined;
  isDisabled?: boolean | undefined;
}

export function CustomerField({ label = 'Customer', value, onChange, kind, help, error, isRequired, isDisabled }: CustomerFieldProps) {
  const client = useApi();
  const source = useMemo<SearchSource<Item>>(() => {
    let controller: AbortController | null = null;
    const find = async (q: string): Promise<Item[]> => {
      controller?.abort();
      const own = new AbortController();
      controller = own;
      try {
        const page = await client.call('customersList', { query: { q: q.trim() || undefined, limit: 20 }, signal: own.signal });
        return page.items
          .filter((c) => !kind || c.kind === kind)
          .slice(0, 10)
          .map((c) => toItem(customerChoice(c)));
      } catch (err) {
        if (err instanceof DOMException && err.name === 'AbortError') return [];
        throw err;
      }
    };
    return {
      search: find,
      bootstrap: () => find(''),
      cancel: () => controller?.abort(),
    };
  }, [client, kind]);
  return (
    <FieldShell help={help} error={error}>
      {(status) => (
        <Typeahead<Item>
          label={label}
          searchSource={source}
          value={value ? toItem(value) : null}
          onChange={(item) => onChange(item?.auxiliaryData ?? null)}
          placeholder="Search by name or email"
          hasEntriesOnFocus
          debounceMs={200}
          isRequired={isRequired}
          isDisabled={isDisabled}
          status={status}
          emptySearchText="No customer matches"
          renderItem={(item) => (
            <TypeaheadItem
              item={item}
              description={[item.auxiliaryData ? KIND_LABEL[item.auxiliaryData.kind] : null, item.auxiliaryData?.contactEmail ?? null].filter(Boolean).join(' · ')}
            />
          )}
        />
      )}
    </FieldShell>
  );
}
