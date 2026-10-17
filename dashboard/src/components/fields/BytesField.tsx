// A size (querylog.retention.maxDBBytes): a NumberInput and a unit Selector. The
// value is the API's form, "10GiB" or a plain byte count; 0 usually means
// "no cap".
import { HStack } from '@astryxdesign/core/HStack';
import { NumberInput } from '@astryxdesign/core/NumberInput';
import { Selector } from '@astryxdesign/core/Selector';
import { StackItem } from '@astryxdesign/core/Stack';
import { FieldShell } from './FieldHelp';

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
  /** Help under the field. */
  description?: string | undefined;
  isDisabled?: boolean | undefined;
  error?: string | undefined;
}

export function BytesField({ label, isLabelHidden, value, onChange, description, isDisabled, error }: BytesFieldProps) {
  const { n, unit } = splitBytes(value);
  const num = n.trim() === '' ? null : Number(n);
  return (
    <FieldShell help={description} error={error}>
      {(status) => (
        <HStack gap={2} vAlign="end">
          <StackItem size="fill">
            <NumberInput
              label={label}
              isLabelHidden={isLabelHidden}
              value={num === null || Number.isNaN(num) ? null : num}
              hasClear
              min={0}
              isWheelEnabled={false}
              isDisabled={isDisabled}
              status={status}
              onChange={(v) => onChange(joinBytes(v === null ? '' : String(v), unit))}
            />
          </StackItem>
          <Selector
            label={`${label} unit`}
            isLabelHidden
            value={unit}
            options={[...BYTE_UNITS]}
            isDisabled={isDisabled}
            data-value={unit}
            onChange={(u) => onChange(joinBytes(n, u as ByteUnit))}
          />
        </HStack>
      )}
    </FieldShell>
  );
}
