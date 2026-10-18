// Copied from dashboard/src/components/fields/PresetField.tsx at dd3dcb0; keep in step by hand.
// Changes: none.
// A value picked from presets with "Custom…" for anything else: the shared
// body of DurationField (Go durations) and SecondsField (TTLs in seconds).
// `same` decides which preset the current value is, so "10m0s" (what
// GET /settings answers) selects the "10 minutes" preset. The help sits
// under the custom input when there is one, else under the Selector.
import { VStack } from '@astryxdesign/core/VStack';
import { useState, type ReactNode } from 'react';
import { SelectField } from './SelectField';
import { TextField } from './TextField';

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
  help?: ReactNode;
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
  help,
  isDisabled,
  error,
  customPlaceholder,
  customUnit,
}: PresetFieldProps) {
  const preset = value ? presets.find((p) => same(p.value, value)) : undefined;
  const [custom, setCustom] = useState(() => Boolean(value) && !preset);
  const selectValue = custom ? CUSTOM : value === '' ? (emptyLabel !== undefined ? '' : CUSTOM) : (preset?.value ?? CUSTOM);
  const isCustom = selectValue === CUSTOM;
  return (
    <VStack gap={2}>
      <SelectField
        label={label}
        isLabelHidden={isLabelHidden}
        value={selectValue}
        options={[...presets, { value: CUSTOM, label: 'Custom…' }]}
        emptyLabel={emptyLabel}
        isDisabled={isDisabled}
        help={isCustom ? undefined : help}
        error={isCustom ? undefined : error}
        onChange={(v) => {
          if (v === CUSTOM) {
            setCustom(true);
            return;
          }
          setCustom(false);
          onChange(v);
        }}
      />
      {isCustom ? (
        <TextField
          label={`${label} (custom)`}
          isLabelHidden
          placeholder={customPlaceholder}
          value={value}
          onChange={onChange}
          isDisabled={isDisabled}
          help={
            customUnit || help ? (
              <>
                {customUnit ? `In ${customUnit}.` : ''}
                {customUnit && help ? ' ' : ''}
                {help}
              </>
            ) : undefined
          }
          error={error}
        />
      ) : null}
    </VStack>
  );
}
