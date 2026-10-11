// A Go duration (`10m`, `24h`) picked from presets, with "Custom…" for
// anything else. Values are kept in the API's form; presets are matched by
// their parsed length so "10m0s" (what GET /settings answers) selects the
// "10 minutes" preset. The settings schema's `presets` (C1) become options
// through durationPresets().
import { parseDuration } from '../../lib/format';
import { PresetField, type Preset } from './PresetField';

export type DurationPreset = Preset;

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

const UNITS: [number, string][] = [
  [86400, 'day'],
  [3600, 'hour'],
  [60, 'minute'],
  [1, 'second'],
];

/** Seconds as words: 90 → "90 seconds", 3600 → "1 hour", 604800 → "7 days". */
export function secondsLabel(s: number): string {
  if (s === 0) return '0';
  if (!Number.isInteger(s)) return `${Math.round(s * 1000)} ms`;
  for (const [n, unit] of UNITS) {
    if (s >= n && s % n === 0) {
      const v = s / n;
      return `${v} ${unit}${v === 1 ? '' : 's'}`;
    }
  }
  return `${s} seconds`;
}

/** A Go duration in words ("1800ms" → "1800 ms", "72h" → "3 days"); the input when it does not parse. */
export function durationLabel(v: string): string {
  const s = parseDuration(v);
  return Number.isNaN(s) ? v : secondsLabel(s);
}

/** Options for preset durations (C1 `presets`); "0s" reads `zeroLabel` when given. */
export function durationPresets(values: string[], zeroLabel?: string): DurationPreset[] {
  return values.map((v) => ({ value: v, label: parseDuration(v) === 0 ? (zeroLabel ?? '0 (off)') : durationLabel(v) }));
}

/** Whether two Go durations are the same length ("10m" and "10m0s"). */
export function sameDuration(a: string, b: string): boolean {
  if (a === b) return true;
  const x = parseDuration(a);
  const y = parseDuration(b);
  return !Number.isNaN(x) && x === y;
}

/** A user-facing error for a Go duration, or null. */
export function durationError(v: string, opts: { min?: number; allowEmpty?: boolean } = {}): string | null {
  const t = v.trim();
  if (!t) return opts.allowEmpty ? null : 'Required';
  const s = parseDuration(t);
  if (Number.isNaN(s)) return 'A duration such as 90s, 10m or 1h30m';
  if (opts.min !== undefined && s < opts.min) return `At least ${secondsLabel(opts.min)}`;
  return null;
}

export interface DurationFieldProps {
  label: string;
  isLabelHidden?: boolean | undefined;
  value: string;
  onChange: (v: string) => void;
  presets?: DurationPreset[];
  /** A first option meaning "not set" (inherit the default). */
  emptyLabel?: string | undefined;
  description?: string | undefined;
  isDisabled?: boolean | undefined;
  error?: string | undefined;
}

export function DurationField({ label, isLabelHidden, value, onChange, presets = DURATION_PRESETS, emptyLabel, description, isDisabled, error }: DurationFieldProps) {
  return (
    <PresetField
      label={label}
      isLabelHidden={isLabelHidden}
      value={value}
      onChange={onChange}
      presets={presets}
      same={sameDuration}
      emptyLabel={emptyLabel}
      description={description}
      isDisabled={isDisabled}
      error={error}
      customPlaceholder="e.g. 90s, 45m, 36h"
    />
  );
}
