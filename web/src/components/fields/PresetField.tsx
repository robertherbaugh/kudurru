// A value picked from presets with "Custom…" for anything else: the shared
// body of DurationField (Go durations) and SecondsField (TTLs in seconds).
// `same` decides which preset the current value is, so "10m0s" (what
// GET /settings answers) selects the "10 minutes" preset.
import { TextInput } from '@astryxdesign/core/TextInput';
import { useId, useState } from 'react';

export interface Preset {
  value: string;
  label: string;
}

const CUSTOM = '__custom__';

export interface PresetFieldProps {
  label: string;
  isLabelHidden?: boolean | undefined;
  value: string;
  onChange: (v: string) => void;
  presets: Preset[];
  /** Whether two values mean the same thing (default: string equality). */
  same?: (a: string, b: string) => boolean;
  /** A first option meaning "not set" (inherit the default). */
  emptyLabel?: string | undefined;
  description?: string | undefined;
  isDisabled?: boolean | undefined;
  error?: string | undefined;
  /** Placeholder of the custom input, with examples. */
  customPlaceholder: string;
  /** Shown after the custom input (e.g. "seconds"). */
  customUnit?: string;
}

export function PresetField({
  label,
  isLabelHidden,
  value,
  onChange,
  presets,
  same = (a, b) => a === b,
  emptyLabel,
  description,
  isDisabled,
  error,
  customPlaceholder,
  customUnit,
}: PresetFieldProps) {
  const id = useId();
  const preset = value ? presets.find((p) => same(p.value, value)) : undefined;
  const [custom, setCustom] = useState(() => Boolean(value) && !preset);
  const selectValue = custom ? CUSTOM : value === '' ? (emptyLabel !== undefined ? '' : CUSTOM) : (preset?.value ?? CUSTOM);
  return (
    <div className="k-stack-sm">
      <label className="k-label" htmlFor={id}>
        {isLabelHidden ? <span className="k-visually-hidden">{label}</span> : label}
        {description ? (
          <span id={`${id}-d`} className="k-muted k-small">
            {description}
          </span>
        ) : null}
        <select
          id={id}
          className="k-native-select"
          aria-label={label}
          aria-describedby={description ? `${id}-d` : undefined}
          value={selectValue}
          disabled={isDisabled}
          aria-invalid={error ? true : undefined}
          onChange={(e) => {
            const v = e.target.value;
            if (v === CUSTOM) {
              setCustom(true);
              return;
            }
            setCustom(false);
            onChange(v);
          }}
        >
          {emptyLabel !== undefined ? <option value="">{emptyLabel}</option> : null}
          {presets.map((p) => (
            <option key={p.value} value={p.value}>
              {p.label}
            </option>
          ))}
          <option value={CUSTOM}>Custom…</option>
        </select>
      </label>
      {selectValue === CUSTOM ? (
        <span className="k-row">
          <TextInput
            label={`${label} (custom)`}
            isLabelHidden
            placeholder={customPlaceholder}
            value={value}
            onChange={onChange}
            isDisabled={isDisabled}
            status={error ? { type: 'error', message: error } : undefined}
          />
          {customUnit ? <span className="k-muted k-small">{customUnit}</span> : null}
        </span>
      ) : error ? (
        <span className="k-error-text">{error}</span>
      ) : null}
    </div>
  );
}
