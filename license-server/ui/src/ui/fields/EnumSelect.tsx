// Copied from web/src/components/fields/EnumSelect.tsx at dd3dcb0; keep in step by hand.
// Changes: none.
// A strongly typed choice over a fixed set of values (settings schema
// `enum`, Policy blockMode, BlockList mode, ...): SelectField under its
// historical name. The option's help shows under the field for the
// selected value, so the choice explains itself.
import type { ReactNode } from 'react';
import { SelectField, type SelectOption } from './SelectField';

export type EnumOption = SelectOption;

export interface EnumSelectProps {
  label: string;
  isLabelHidden?: boolean | undefined;
  value: string;
  options: EnumOption[];
  onChange: (v: string) => void;
  /** A first option meaning "not set" (inherit), e.g. "Default (from settings)". */
  emptyLabel?: string | undefined;
  /** Help under the field, before the selected option's help. */
  description?: ReactNode;
  isDisabled?: boolean | undefined;
  error?: string | undefined;
  'data-testid'?: string | undefined;
}

export function EnumSelect({ description, ...rest }: EnumSelectProps) {
  return <SelectField {...rest} help={description} />;
}
