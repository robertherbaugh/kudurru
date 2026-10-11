// A Go duration (`10m`, `24h`) picked from presets, with "Custom…" for
// anything else. Values are kept in the API's form; presets are matched by
// their parsed length so "10m0s" (what GET /settings answers) selects the
// "10 minutes" preset.
import { TextInput } from '@astryxdesign/core/TextInput';
import { useId, useState } from 'react';
import { parseDuration } from '../../lib/format';

export interface DurationPreset {
  value: string;
  label: string;
}

export const DURATION_PRESETS: DurationPreset[] = [
  { value: '30s', label: '30 seconds' },
  { value: '1m', label: '1 minute' },
  { value: '5m', label: '5 minutes' },
  { value: '10m', label: '10 minutes' },
  { value: '30m', label: '30 minutes' },
  { value: '1h', label: '1 hour' },
  { value: '6h', label: '6 hours' },
  { value: '12h', label: '12 hours' },
  { value: '24h', label: '1 day' },
  { value: '168h', label: '7 days' },
  { value: '720h', label: '30 days' },
];

const CUSTOM = '__custom__';

export interface DurationFieldProps {
  label: string;
  value: string;
  onChange: (v: string) => void;
  presets?: DurationPreset[];
  /** A first option meaning "not set" (inherit the default). */
  emptyLabel?: string;
  description?: string;
  isDisabled?: boolean;
  error?: string;
}

function same(a: string, b: string): boolean {
  if (a === b) return true;
  try {
    return parseDuration(a) === parseDuration(b);
  } catch {
    return false;
  }
}

export function DurationField({ label, value, onChange, presets = DURATION_PRESETS, emptyLabel, description, isDisabled, error }: DurationFieldProps) {
  const id = useId();
  const preset = value ? presets.find((p) => same(p.value, value)) : undefined;
  const [custom, setCustom] = useState(() => Boolean(value) && !preset);
  const selectValue = custom ? CUSTOM : value === '' ? '' : (preset?.value ?? CUSTOM);
  return (
    <div className="k-stack-sm">
      <div className="k-label">
        <label htmlFor={id}>{label}</label>
        {description ? (
          <span id={`${id}-d`} className="k-muted k-small">
            {description}
          </span>
        ) : null}
        <select
          id={id}
          className="k-native-select"
          value={selectValue}
          disabled={isDisabled}
          aria-describedby={description ? `${id}-d` : undefined}
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
      </div>
      {selectValue === CUSTOM ? (
        <TextInput
          label={`${label} (custom)`}
          isLabelHidden
          placeholder="e.g. 90s, 45m, 36h"
          value={value}
          onChange={onChange}
          isDisabled={isDisabled}
          status={error ? { type: 'error', message: error } : undefined}
        />
      ) : error ? (
        <span className="k-error-text">{error}</span>
      ) : null}
    </div>
  );
}
