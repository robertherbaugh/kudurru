// A strongly typed choice: a native select over a fixed set of values
// (settings schema `enum`, Policy blockMode, BlockList mode, ...). The
// option's help text shows under the field for the selected value, so the
// choice explains itself without a paragraph of help above it.
import { useId } from 'react';

export interface EnumOption {
  value: string;
  label: string;
  help?: string;
}

export interface EnumSelectProps {
  label: string;
  value: string;
  options: EnumOption[];
  onChange: (v: string) => void;
  /** A first option meaning "not set" (inherit), e.g. "Default (from settings)". */
  emptyLabel?: string;
  description?: string;
  isDisabled?: boolean;
  error?: string;
}

export function EnumSelect({ label, value, options, onChange, emptyLabel, description, isDisabled, error }: EnumSelectProps) {
  const id = useId();
  const all = value && !options.some((o) => o.value === value) ? [...options, { value, label: value }] : options;
  const selected = all.find((o) => o.value === value);
  const hint = error ?? selected?.help;
  const describedBy = [description ? `${id}-d` : '', hint ? `${id}-h` : ''].filter(Boolean).join(' ') || undefined;
  // The label names the select; description and help describe it, so a
  // screen reader (and getByRole name) hears the label alone.
  return (
    <div className="k-label">
      <label htmlFor={id}>{label}</label>
      {description ? (
        <span id={`${id}-d`} className="k-muted k-small">
          {description}
        </span>
      ) : null}
      <select id={id} className="k-native-select" value={value} disabled={isDisabled} aria-invalid={error ? true : undefined} aria-describedby={describedBy} onChange={(e) => onChange(e.target.value)}>
        {emptyLabel !== undefined ? <option value="">{emptyLabel}</option> : null}
        {all.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
      {hint ? (
        <span id={`${id}-h`} className={error ? 'k-error-text' : 'k-muted k-small'}>
          {hint}
        </span>
      ) : null}
    </div>
  );
}
