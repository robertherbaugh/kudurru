// A list of strings edited row by row (settings string[] keys, server
// lists, ACLs, name servers): one input per entry, add and remove buttons,
// an inline error per row from `itemError`, and "Paste many" for a block of
// lines. Empty rows are dropped by cleanList() before saving.
import { Button } from '@astryxdesign/core/Button';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useId, useState } from 'react';
import type { EnumOption } from './EnumSelect';

/** Trims entries and drops empty ones. */
export function cleanList(v: string[]): string[] {
  return v.map((s) => s.trim()).filter(Boolean);
}

/** The first row error as "Line n (value): message", or null. */
export function listError(v: string[], itemError?: (s: string) => string | null): string | null {
  if (!itemError) return null;
  const list = cleanList(v);
  for (let i = 0; i < list.length; i++) {
    const e = itemError(list[i] as string);
    if (e) return `Line ${i + 1} (${list[i]}): ${e}`;
  }
  return null;
}

export interface StringListFieldProps {
  label: string;
  isLabelHidden?: boolean | undefined;
  value: string[];
  onChange: (v: string[]) => void;
  description?: string | undefined;
  placeholder?: string | undefined;
  /** A closed set: each row is a select of these. */
  options?: EnumOption[] | undefined;
  itemError?: ((s: string) => string | null) | undefined;
  addLabel?: string;
  isDisabled?: boolean | undefined;
  /** An error for the whole list (e.g. "at least one"). */
  error?: string | undefined;
  /** Offer a textarea for pasting many entries at once. */
  allowPaste?: boolean;
}

export function StringListField({
  label,
  isLabelHidden,
  value,
  onChange,
  description,
  placeholder,
  options,
  itemError,
  addLabel = 'Add',
  isDisabled,
  error,
  allowPaste = !options,
}: StringListFieldProps) {
  const id = useId();
  const [paste, setPaste] = useState<string | null>(null);
  const set = (i: number, v: string) => onChange(value.map((x, j) => (j === i ? v : x)));
  return (
    <fieldset className="k-fieldset k-list-field" aria-describedby={description ? `${id}-d` : undefined} disabled={isDisabled}>
      <legend className={isLabelHidden ? 'k-visually-hidden' : undefined}>{label}</legend>
      {description ? (
        <span id={`${id}-d`} className="k-muted k-small">
          {description}
        </span>
      ) : null}
      {value.length === 0 ? <span className="k-muted k-small">None.</span> : null}
      {value.map((v, i) => {
        const e = v.trim() && itemError ? itemError(v.trim()) : null;
        const rowLabel = `${label} ${i + 1}`;
        return (
          <div key={i} className="k-array-item" data-testid="list-row">
            {options ? (
              <select className="k-native-select" aria-label={rowLabel} value={v} disabled={isDisabled} onChange={(ev) => set(i, ev.target.value)}>
                {(options.some((o) => o.value === v) ? options : [...options, { value: v, label: v || 'Choose…' }]).map((o) => (
                  <option key={o.value} value={o.value}>
                    {o.label}
                  </option>
                ))}
              </select>
            ) : (
              <TextInput
                label={rowLabel}
                isLabelHidden
                value={v}
                onChange={(nv) => set(i, nv)}
                placeholder={placeholder}
                isDisabled={isDisabled}
                status={e ? { type: 'error', message: e } : undefined}
              />
            )}
            <Button label={`Remove ${rowLabel}`} size="sm" variant="ghost" isDisabled={isDisabled} onClick={() => onChange(value.filter((_, j) => j !== i))} />
          </div>
        );
      })}
      {paste !== null ? (
        <div className="k-stack-sm">
          <label className="k-label" htmlFor={`${id}-paste`}>
            Paste entries, one per line
            <textarea id={`${id}-paste`} className="k-native-input k-mono" rows={5} value={paste} onChange={(e) => setPaste(e.target.value)} spellCheck={false} />
          </label>
          <span className="k-row">
            <Button
              label="Add these"
              size="sm"
              variant="secondary"
              onClick={() => {
                onChange([...cleanList(value), ...cleanList(paste.split(/[\r\n,]+/))]);
                setPaste(null);
              }}
            />
            <Button label="Cancel" size="sm" variant="ghost" onClick={() => setPaste(null)} />
          </span>
        </div>
      ) : null}
      <span className="k-row">
        <Button label={addLabel} size="sm" variant="secondary" isDisabled={isDisabled} onClick={() => onChange([...value, options?.[0]?.value ?? ''])} />
        {allowPaste && paste === null ? <Button label="Paste many" size="sm" variant="ghost" isDisabled={isDisabled} onClick={() => setPaste('')} /> : null}
      </span>
      {error ? <span className="k-error-text">{error}</span> : null}
    </fieldset>
  );
}
