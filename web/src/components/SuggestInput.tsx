// A text filter with server-side suggestions (GET /queries/suggestions),
// following the WAI-ARIA combobox pattern: arrow keys move through the
// listbox, Enter picks, Escape closes. Wildcards ("*.example.com") are
// passed through to the API untouched.
import { useQuery } from '@tanstack/react-query';
import { useEffect, useId, useState, type KeyboardEvent } from 'react';
import { useApi } from '../api/context';
import type { SuggestionField } from '../api/types';

export interface SuggestInputProps {
  label: string;
  field: SuggestionField;
  value: string;
  /** Called with the committed value (Enter, pick, blur). */
  onCommit: (value: string) => void;
  placeholder?: string;
}

function useDebounced<T>(value: T, ms: number): T {
  const [v, setV] = useState(value);
  useEffect(() => {
    const id = window.setTimeout(() => setV(value), ms);
    return () => window.clearTimeout(id);
  }, [value, ms]);
  return v;
}

export function SuggestInput({ label, field, value, onCommit, placeholder }: SuggestInputProps) {
  const api = useApi();
  const id = useId();
  const listId = `${id}-list`;
  const [text, setText] = useState(value);
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(-1);
  const prefix = useDebounced(text.replace(/\*/g, ''), 200);

  useEffect(() => setText(value), [value]);

  const q = useQuery({
    queryKey: ['suggestions', field, prefix],
    queryFn: ({ signal }) => api.queries.suggestions(field, prefix, signal),
    enabled: open && prefix.length >= 1,
    staleTime: 30_000,
  });
  const options = open ? (q.data?.values ?? []).filter((v) => v !== text).slice(0, 10) : [];
  const expanded = options.length > 0;

  const commit = (v: string) => {
    setOpen(false);
    setActive(-1);
    setText(v);
    if (v !== value) onCommit(v);
  };

  const onKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      setOpen(true);
      setActive((a) => Math.min(options.length - 1, a + 1));
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      setActive((a) => Math.max(-1, a - 1));
    } else if (e.key === 'Enter') {
      e.preventDefault();
      commit(active >= 0 && options[active] !== undefined ? options[active] : text.trim());
    } else if (e.key === 'Escape') {
      if (expanded) {
        e.preventDefault();
        setOpen(false);
        setActive(-1);
      }
    }
  };

  return (
    <div className="k-suggest k-label">
      <label htmlFor={id}>{label}</label>
      <input
        id={id}
        className="k-native-input"
        type="search"
        role="combobox"
        aria-autocomplete="list"
        aria-expanded={expanded}
        aria-controls={listId}
        aria-activedescendant={active >= 0 ? `${listId}-${active}` : undefined}
        autoComplete="off"
        spellCheck={false}
        placeholder={placeholder}
        value={text}
        onChange={(e) => {
          setText(e.target.value);
          setOpen(true);
          setActive(-1);
        }}
        onFocus={() => setOpen(true)}
        onBlur={() => {
          // Let a click on an option land before closing.
          window.setTimeout(() => {
            setOpen(false);
            if (text.trim() !== value) onCommit(text.trim());
          }, 120);
        }}
        onKeyDown={onKeyDown}
      />
      <ul id={listId} role="listbox" aria-label={`${label} suggestions`} hidden={!expanded}>
        {options.map((o, i) => (
          <li
            key={o}
            id={`${listId}-${i}`}
            role="option"
            aria-selected={i === active}
            onMouseDown={(e) => {
              e.preventDefault();
              commit(o);
            }}
          >
            {o}
          </li>
        ))}
      </ul>
    </div>
  );
}
