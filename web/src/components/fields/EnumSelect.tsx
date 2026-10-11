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
  return (
    <label className="k-label" htmlFor={id}>
      {label}
      {description ? <span className="k-muted k-small">{description}</span> : null}
      <select id={id} className="k-native-select" value={value} disabled={isDisabled} aria-invalid={error ? true : undefined} onChange={(e) => onChange(e.target.value)}>
        {emptyLabel !== undefined ? <option value="">{emptyLabel}</option> : null}
        {all.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
      {error ? <span className="k-error-text">{error}</span> : selected?.help ? <span className="k-muted k-small">{selected.help}</span> : null}
    </label>
  );
}
