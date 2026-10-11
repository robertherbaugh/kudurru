// A whole number with optional bounds and unit (settings `min`/`max`, ECS
// prefixes, percentages, TTLs): an Astryx NumberInput with the help and
// the allowed range under it. The form keeps the value as text ("" = not
// set) and gets every keystroke (the input event), so a Save button can
// appear while typing and an out-of-range number is reported inline
// rather than silently clamped: the bounds are checked here and not passed
// to NumberInput, which would clamp on blur.
import { NumberInput } from '@astryxdesign/core/NumberInput';
import type { ReactNode } from 'react';
import { FieldShell } from './FieldHelp';

/** A user-facing error for a whole number within bounds, or null. */
export function numberError(text: string, opts: { min?: number | undefined; max?: number | undefined; optional?: boolean } = {}): string | null {
  const t = text.trim();
  if (!t) return opts.optional ? null : 'Required';
  if (!/^-?\d+$/.test(t)) return 'Must be a whole number';
  const n = Number(t);
  const { min, max } = opts;
  if (min !== undefined && max !== undefined && (n < min || n > max)) return `Between ${min} and ${max}`;
  if (min !== undefined && n < min) return `At least ${min}`;
  if (max !== undefined && n > max) return `At most ${max}`;
  return null;
}

/** "0–32", "≥ 1", "≤ 100" or "". */
export function rangeText(min?: number, max?: number): string {
  return min !== undefined && max !== undefined ? `${min}–${max}` : min !== undefined ? `≥ ${min}` : max !== undefined ? `≤ ${max}` : '';
}

export interface NumberFieldProps {
  label: string;
  isLabelHidden?: boolean | undefined;
  value: string;
  onChange: (v: string) => void;
  min?: number | undefined;
  max?: number | undefined;
  /** Shown inside the input after the number ("seconds", "%"). */
  unit?: string | undefined;
  help?: ReactNode;
  /** The old name of `help` (also shown under the control). */
  description?: ReactNode;
  placeholder?: string | undefined;
  isDisabled?: boolean | undefined;
  /** Shown instead of the bounds check (e.g. the server's message). */
  error?: string | undefined;
  optional?: boolean | undefined;
  /** Allow fractions (default: whole numbers). */
  allowFraction?: boolean | undefined;
  onEnter?: (() => void) | undefined;
  width?: string | number | undefined;
}

export function NumberField({ label, isLabelHidden, value, onChange, min, max, unit, help: helpProp, description, placeholder, isDisabled, error, optional, allowFraction, onEnter, width }: NumberFieldProps) {
  const help = helpProp ?? description;
  const t = value.trim();
  const msg = error ?? (t && !allowFraction ? numberError(value, { min, max }) : null) ?? undefined;
  const range = rangeText(min, max);
  const n = t === '' ? null : Number(t);
  const foot =
    help || range ? (
      <>
        {help}
        {help && range ? ' ' : ''}
        {range ? `Range ${range}.` : ''}
      </>
    ) : undefined;
  return (
    <FieldShell help={foot} error={msg} width={width}>
      {(status) => (
        <NumberInput
          label={label}
          isLabelHidden={isLabelHidden}
          isOptional={optional}
          value={n === null || Number.isNaN(n) ? null : n}
          hasClear
          onChange={(v) => onChange(v === null ? '' : String(v))}
          onInput={(e) => onChange((e.target as HTMLInputElement).value)}
          units={unit ?? null}
          isIntegerOnly={!allowFraction}
          isWheelEnabled={false}
          placeholder={placeholder}
          isDisabled={isDisabled}
          status={status}
          onEnter={onEnter}
        />
      )}
    </FieldShell>
  );
}
