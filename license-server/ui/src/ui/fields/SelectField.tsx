// Copied from dashboard/src/components/fields/SelectField.tsx at dd3dcb0; keep in step by hand.
// Changes: none.
// A closed choice as an Astryx Selector with the help under it: the
// field's own help, then the selected option's help, so a choice explains
// itself without a paragraph between the label and the control. A value
// that is not among the options stays listed (shown as itself) so editing
// never drops it silently; `emptyLabel` adds a first option for "not set".
// Long lists get the Selector's search. The trigger carries the value as
// data-value (tests and e2e read it).
import { Selector, type SelectorOptionData } from '@astryxdesign/core/Selector';
import { Text } from '@astryxdesign/core/Text';
import type { ReactNode } from 'react';
import { FieldShell } from './FieldHelp';

export interface SelectOption {
  value: string;
  label: string;
  /** Shown under the field while this option is selected. */
  help?: string | undefined;
  /** Secondary text in the list (e.g. "2,852 entries"). */
  detail?: string | undefined;
  /** Listed but not selectable (e.g. a scope the object cannot use). */
  disabled?: boolean | undefined;
}

export interface SelectFieldProps {
  label: string;
  isLabelHidden?: boolean | undefined;
  value: string;
  options: SelectOption[];
  onChange: (v: string) => void;
  /** A first option meaning "not set" (inherit), e.g. "Default (from settings)". */
  emptyLabel?: string | undefined;
  /** Shown on the trigger while nothing is chosen. */
  placeholder?: string | undefined;
  help?: ReactNode;
  error?: string | undefined;
  isDisabled?: boolean | undefined;
  isLoading?: boolean | undefined;
  isRequired?: boolean | undefined;
  isOptional?: boolean | undefined;
  /** Offer a search box (default: more than 12 options). */
  hasSearch?: boolean | undefined;
  /** Field width (default: the column). */
  width?: string | number | undefined;
  size?: 'sm' | 'md' | 'lg' | undefined;
  'data-testid'?: string | undefined;
}

/** The options as the Selector takes them, with an unknown value kept and the empty option first. */
export function selectorOptions(value: string, options: SelectOption[], emptyLabel?: string): SelectorOptionData[] {
  const out: SelectorOptionData[] = [];
  if (emptyLabel !== undefined) out.push({ value: '', label: emptyLabel });
  for (const o of options) {
    const d: SelectorOptionData = { value: o.value, label: o.label };
    if (o.detail) d.description = o.detail;
    if (o.disabled) d.disabled = true;
    out.push(d);
  }
  if (value && !options.some((o) => o.value === value)) out.push({ value, label: value });
  return out;
}

export function SelectField({
  label,
  isLabelHidden,
  value,
  options,
  onChange,
  emptyLabel,
  placeholder,
  help,
  error,
  isDisabled,
  isLoading,
  isRequired,
  isOptional,
  hasSearch,
  width,
  size,
  'data-testid': testId,
}: SelectFieldProps) {
  const selected = options.find((o) => o.value === value);
  const optionHelp = selected?.help;
  const all = selectorOptions(value, options, emptyLabel);
  const foot =
    help && optionHelp ? (
      <>
        {help}{' '}
        <Text type="inherit" color="inherit">
          {optionHelp}
        </Text>
      </>
    ) : (
      (help ?? optionHelp)
    );
  return (
    <FieldShell help={foot} error={error} width={width}>
      {(status) => (
        <Selector
          label={label}
          isLabelHidden={isLabelHidden}
          value={value}
          options={all}
          onChange={onChange}
          placeholder={placeholder ?? 'Choose…'}
          isDisabled={isDisabled}
          isLoading={isLoading}
          isRequired={isRequired}
          isOptional={isOptional}
          hasSearch={hasSearch ?? all.length > 12}
          status={status}
          size={size}
          data-value={value}
          data-testid={testId}
        />
      )}
    </FieldShell>
  );
}
