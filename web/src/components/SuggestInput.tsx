// A text filter with server-side suggestions (GET /queries/suggestions) on
// an Astryx Typeahead. The typed text is always the first result ("Filter
// by …"), so Enter commits exactly what was typed, wildcards
// ("*.example.com") included; the arrows pick a suggestion instead. Leaving
// the field commits the typed text too. A committed value shows as the
// Typeahead's token; its clear button removes the filter.
import { Typeahead, type SearchableItem, type SearchSource } from '@astryxdesign/core/Typeahead';
import { useMemo, useRef, type KeyboardEvent } from 'react';
import { useApi } from '../api/context';
import type { SuggestionField } from '../api/types';

export interface SuggestInputProps {
  label: string;
  field: SuggestionField;
  value: string;
  /** Called with the committed value (Enter, pick, blur, clear). */
  onCommit: (value: string) => void;
  placeholder?: string;
}

export function SuggestInput({ label, field, value, onCommit, placeholder }: SuggestInputProps) {
  const api = useApi();
  const query = useRef('');
  const source = useMemo<SearchSource<SearchableItem>>(
    () => ({
      bootstrap: () => [],
      search: async (q: string) => {
        const t = q.trim();
        if (!t) return [];
        const prefix = t.replace(/\*/g, '');
        const values = prefix ? ((await api.queries.suggestions(field, prefix).catch(() => ({ values: [] as string[] }))).values ?? []) : [];
        return [{ id: `=${t}`, label: t, element: <>Filter by “{t}”</> }, ...values.filter((v) => v !== t).slice(0, 10).map((v) => ({ id: v, label: v }))];
      },
    }),
    [api, field],
  );

  const commit = (v: string) => {
    query.current = '';
    if (v !== value) onCommit(v);
  };

  return (
    <Typeahead
      label={label}
      value={value ? { id: `=${value}`, label: value } : null}
      onChange={(item) => commit(item?.label ?? '')}
      searchSource={source}
      onChangeQuery={(q) => {
        query.current = q;
      }}
      placeholder={placeholder}
      debounceMs={200}
      minQueryLength={1}
      onKeyDown={(e: KeyboardEvent<HTMLDivElement>) => {
        // The Typeahead picked the highlighted result first; otherwise
        // (suggestions still loading) commit the text as typed.
        if (e.key === 'Enter' && query.current.trim()) commit(query.current.trim());
      }}
      onBlur={(e) => {
        if (e.currentTarget.contains(e.relatedTarget)) return;
        if (query.current.trim()) commit(query.current.trim());
      }}
    />
  );
}
