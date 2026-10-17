// A whole number of seconds (DNS TTLs, SOA timers) picked from presets,
// with "Custom…" for any other value. The value is the decimal text the
// form keeps ("" = not set); presets show as words ("5 minutes (300)").
import { secondsLabel } from './DurationField';
import { PresetField, type Preset } from './PresetField';

/** Record TTLs. */
export const TTL_PRESETS: number[] = [60, 300, 900, 3600, 14400, 86400];

/** Options for second presets. */
export function secondPresets(values: number[]): Preset[] {
  return values.map((s) => ({ value: String(s), label: `${secondsLabel(s)} (${s})` }));
}

/** A user-facing error for a seconds value, or null ("" is allowed when optional). */
export function secondsError(v: string, opts: { min?: number; max?: number; optional?: boolean } = {}): string | null {
  const t = v.trim();
  if (!t) return opts.optional ? null : 'Required';
  const min = opts.min ?? 0;
  const max = opts.max ?? 2147483647;
  if (!/^\d+$/.test(t) || Number(t) < min || Number(t) > max) return `Seconds, ${min} to ${max}`;
  return null;
}

export interface SecondsFieldProps {
  label: string;
  value: string;
  onChange: (v: string) => void;
  presets?: number[];
  emptyLabel?: string | undefined;
  /** Help under the field. */
  description?: string | undefined;
  isDisabled?: boolean | undefined;
  error?: string | undefined;
}

export function SecondsField({ label, value, onChange, presets = TTL_PRESETS, emptyLabel, description, isDisabled, error }: SecondsFieldProps) {
  return (
    <PresetField
      label={label}
      value={value}
      onChange={onChange}
      presets={secondPresets(presets)}
      same={(a, b) => a.trim() !== '' && Number(a) === Number(b)}
      emptyLabel={emptyLabel}
      help={description}
      isDisabled={isDisabled}
      error={error}
      customPlaceholder="e.g. 120"
      customUnit="seconds"
    />
  );
}
