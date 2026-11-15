import { DateTimeInput, type ISODateTimeString } from '@astryxdesign/core/DateTimeInput';
import type { ReactNode } from 'react';
import { FieldShell } from './FieldHelp';
import { formatError } from './formats';

/** Render absolute instants in UTC; never reinterpret a stored expiry in the browser's time zone. */
export function utcInput(value: string): ISODateTimeString | undefined {
  if (!value || formatError('date-time', value)) return undefined;
  return new Date(value).toISOString().slice(0, 19) as ISODateTimeString;
}

export function DateTimeField({ label, isLabelHidden, value, onChange, help, isDisabled }: {
  label: string;
  isLabelHidden?: boolean | undefined;
  value: string;
  onChange: (value: string) => void;
  help?: ReactNode;
  isDisabled?: boolean | undefined;
}) {
  return (
    <FieldShell help={help} error={formatError('date-time', value) ?? undefined}>
      {(status) => (
        <DateTimeInput
          label={`${label} (UTC)`}
          isLabelHidden={isLabelHidden}
          value={utcInput(value)}
          onChange={(v) => onChange(v ? `${v.length === 16 ? `${v}:00` : v}Z` : '')}
          hasSeconds
          hourFormat="24h"
          hasClear
          isDisabled={isDisabled}
          status={status}
        />
      )}
    </FieldShell>
  );
}
