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
  isLabelHidden?: boolean | undefined;
  value: string;
  options: EnumOption[];
  onChange: (v: string) => void;
  /** A first option meaning "not set" (inherit), e.g. "Default (from settings)". */
  emptyLabel?: string;
  description?: string | undefined;
  isDisabled?: boolean | undefined;
  error?: string | undefined;
}

export function EnumSelect({ label, isLabelHidden, value, options, onChange, emptyLabel, description, isDisabled, error }: EnumSelectProps) {
  const id = useId();
  const all = value && !options.some((o) => o.value === value) ? [...options, { value, label: value }] : options;
  const selected = all.find((o) => o.value === value);
  return (
    <label className="k-label" htmlFor={id}>
      {isLabelHidden ? <span className="k-visually-hidden">{label}</span> : label}
      {description ? (
        <span id={`${id}-d`} className="k-muted k-small">
          {description}
        </span>
      ) : null}
      {/* Named by the label alone; description and help describe it. */}
      <select
        id={id}
        className="k-native-select"
        value={value}
        disabled={isDisabled}
        aria-label={label}
        aria-describedby={[description ? `${id}-d` : '', error || selected?.help ? `${id}-h` : ''].filter(Boolean).join(' ') || undefined}
        aria-invalid={error ? true : undefined}
        onChange={(e) => onChange(e.target.value)}
      >
        {emptyLabel !== undefined ? <option value="">{emptyLabel}</option> : null}
        {all.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
      {error ? (
        <span id={`${id}-h`} className="k-error-text">
          {error}
        </span>
      ) : selected?.help ? (
        <span id={`${id}-h`} className="k-muted k-small">
          {selected.help}
        </span>
      ) : null}
    </label>
  );
}
