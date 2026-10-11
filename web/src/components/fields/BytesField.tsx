// A size (querylog.retention.maxDBBytes): a number and a unit select. The
// value is the API's form, "10GiB" or a plain byte count; 0 usually means
// "no cap".
import { useId } from 'react';

export const BYTE_UNITS = ['B', 'KB', 'MB', 'GB', 'TB', 'KiB', 'MiB', 'GiB', 'TiB'] as const;
export type ByteUnit = (typeof BYTE_UNITS)[number];

/** Splits "10GiB", "500 MB" or 1048576 into number text and unit. */
export function splitBytes(v: unknown): { n: string; unit: ByteUnit } {
  if (typeof v === 'number') return { n: String(v), unit: 'B' };
  const m = /^\s*(\d+(?:\.\d+)?)\s*([KMGT]i?B|B)?\s*$/i.exec(typeof v === 'string' ? v : '');
  if (!m) return { n: typeof v === 'string' ? v : '', unit: 'GiB' };
  const unit = BYTE_UNITS.find((u) => u.toLowerCase() === (m[2] ?? 'B').toLowerCase()) ?? 'B';
  return { n: m[1] ?? '', unit };
}

/** Joins number text and unit into the API value (bytes as a number). */
export function joinBytes(n: string, unit: ByteUnit): string | number {
  const t = n.trim();
  if (unit === 'B' && /^\d+$/.test(t)) return Number(t);
  return `${t}${unit}`;
}

/** A user-facing error for a size, or null. */
export function bytesError(v: unknown): string | null {
  if (typeof v === 'number') return Number.isInteger(v) && v >= 0 ? null : 'A whole number of bytes';
  return typeof v === 'string' && /^\d+(\.\d+)?\s*([KMGT]i?B|B)?$/i.test(v.trim()) ? null : 'A size such as 500MB or 10GiB';
}

/** Whether two sizes are the same value ("10GiB" and "10 GiB"). */
export function sameBytes(a: unknown, b: unknown): boolean {
  const x = splitBytes(a);
  const y = splitBytes(b);
  return Number(x.n) === Number(y.n) && (x.unit === y.unit || Number(x.n) === 0);
}

export interface BytesFieldProps {
  label: string;
  isLabelHidden?: boolean | undefined;
  value: unknown;
  onChange: (v: string | number) => void;
  description?: string | undefined;
  isDisabled?: boolean | undefined;
  error?: string | undefined;
}

export function BytesField({ label, isLabelHidden, value, onChange, description, isDisabled, error }: BytesFieldProps) {
  const id = useId();
  const { n, unit } = splitBytes(value);
  return (
    <div className="k-stack-sm">
      <label className="k-label" htmlFor={id}>
        {isLabelHidden ? <span className="k-visually-hidden">{label}</span> : label}
        {description ? <span className="k-muted k-small">{description}</span> : null}
      </label>
      <span className="k-row">
        <input
          id={id}
          className="k-native-input k-number"
          type="number"
          min={0}
          step="any"
          value={n}
          disabled={isDisabled}
          aria-invalid={error ? true : undefined}
          onChange={(e) => onChange(joinBytes(e.target.value, unit))}
        />
        <select className="k-native-select k-unit" aria-label={`${label} unit`} value={unit} disabled={isDisabled} onChange={(e) => onChange(joinBytes(n, e.target.value as ByteUnit))}>
          {BYTE_UNITS.map((u) => (
            <option key={u} value={u}>
              {u}
            </option>
          ))}
        </select>
      </span>
      {error ? <span className="k-error-text">{error}</span> : null}
    </div>
  );
}
