// A whole number with optional bounds (settings `min`/`max`, ECS prefixes,
// percentages): a native number input that reports out-of-range values
// inline. The value stays text so a half-typed number is not lost.
import { useId } from 'react';

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

export interface NumberFieldProps {
  label: string;
  isLabelHidden?: boolean;
  value: string;
  onChange: (v: string) => void;
  min?: number | undefined;
  max?: number | undefined;
  description?: string | undefined;
  placeholder?: string | undefined;
  isDisabled?: boolean | undefined;
  /** Shown instead of the bounds check (e.g. the server's message). */
  error?: string | undefined;
  optional?: boolean;
}

export function NumberField({ label, isLabelHidden, value, onChange, min, max, description, placeholder, isDisabled, error, optional }: NumberFieldProps) {
  const id = useId();
  const msg = error ?? (value.trim() ? numberError(value, { min, max }) : null) ?? undefined;
  const range = min !== undefined && max !== undefined ? `${min}–${max}` : min !== undefined ? `≥ ${min}` : max !== undefined ? `≤ ${max}` : '';
  return (
    <label className="k-label" htmlFor={id}>
      <span className={isLabelHidden ? 'k-visually-hidden' : undefined}>
        {label}
        {optional ? <span className="k-muted k-small"> (optional)</span> : null}
      </span>
      {description ? (
        <span id={`${id}-d`} className="k-muted k-small">
          {description}
        </span>
      ) : null}
      <span className="k-row">
        <input
          id={id}
          className="k-native-input k-number"
          type="number"
          aria-label={label}
          aria-describedby={description ? `${id}-d` : undefined}
          inputMode="numeric"
          step={1}
          min={min}
          max={max}
          value={value}
          placeholder={placeholder}
          disabled={isDisabled}
          aria-invalid={msg ? true : undefined}
          onChange={(e) => onChange(e.target.value)}
        />
        {range ? <span className="k-muted k-small">{range}</span> : null}
      </span>
      {msg ? <span className="k-error-text">{msg}</span> : null}
    </label>
  );
}
