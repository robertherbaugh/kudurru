// A key's expiry day: an Astryx DateInput showing the day as YYYY-MM-DD
// (format system_date) with min and max, optional preset buttons under it
// ("1 year", "2 years"), then the help and the error (FieldHelp). Values
// are 'YYYY-MM-DD' strings, the wire's expiresOn: the last UTC day the key
// is valid; the range error and the presets' tooltips name days the way
// every page does (lib/dates utcDay: 'YYYY-MM-DD (UTC)'). The DateInput's
// calendar is the Astryx popover everywhere (no native picker), so what is
// chosen is exactly the day shown.
import type { ISODateString } from '@astryxdesign/core/Calendar';
import { Button } from '@astryxdesign/core/Button';
import { DateInput } from '@astryxdesign/core/DateInput';
import { HStack } from '@astryxdesign/core/Stack';
import type { ReactNode } from 'react';
import { isExpiresOn, type ExpiresOn } from '@shared/dates';
import { utcDay } from '../../lib/dates';
import { FieldShell } from './FieldHelp';

export interface DatePreset {
  label: string;
  value: ExpiresOn;
}

/** A user-facing error for an expiry day within bounds ('YYYY-MM-DD' strings compare in calendar order), or null. */
export function dateError(value: string, opts: { min?: string | undefined; max?: string | undefined; optional?: boolean } = {}): string | null {
  if (!value) return opts.optional ? null : 'Choose a day';
  if (!isExpiresOn(value)) return 'Not a valid day (YYYY-MM-DD)';
  if (opts.min && value < opts.min) return `On or after ${utcDay(opts.min)}`;
  if (opts.max && value > opts.max) return `On or before ${utcDay(opts.max)}`;
  return null;
}

export interface DateFieldProps {
  label: string;
  value: string;
  onChange: (v: string) => void;
  min?: string | undefined;
  max?: string | undefined;
  /** Buttons under the control that set a day (the term presets). */
  presets?: DatePreset[] | undefined;
  help?: ReactNode;
  error?: string | undefined;
  isRequired?: boolean | undefined;
  isOptional?: boolean | undefined;
  isDisabled?: boolean | undefined;
  width?: string | number | undefined;
  'data-testid'?: string | undefined;
}

export function DateField({ label, value, onChange, min, max, presets, help, error, isRequired, isOptional, isDisabled, width, 'data-testid': testId }: DateFieldProps) {
  return (
    <FieldShell help={help} error={error} width={width} data-testid={testId}>
      {(status) => (
        <>
          <DateInput
            label={label}
            value={isExpiresOn(value) ? (value as ISODateString) : undefined}
            onChange={(v) => onChange(v ?? '')}
            min={min && isExpiresOn(min) ? (min as ISODateString) : undefined}
            max={max && isExpiresOn(max) ? (max as ISODateString) : undefined}
            format="system_date"
            presentation="popover"
            placeholder="YYYY-MM-DD"
            isRequired={isRequired}
            isOptional={isOptional}
            isDisabled={isDisabled}
            status={status}
          />
          {presets && presets.length ? (
            <HStack gap={1} wrap="wrap" role="group" aria-label={`${label}: presets`}>
              {presets.map((p) => (
                <Button
                  key={p.label}
                  label={p.label}
                  size="sm"
                  variant={p.value === value ? 'primary' : 'secondary'}
                  isDisabled={isDisabled}
                  tooltip={utcDay(p.value)}
                  onClick={() => onChange(p.value)}
                />
              ))}
            </HStack>
          ) : null}
        </>
      )}
    </FieldShell>
  );
}
