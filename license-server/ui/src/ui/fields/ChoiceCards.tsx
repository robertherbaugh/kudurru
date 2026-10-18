// Copied from dashboard/src/components/fields/ChoiceCards.tsx at dd3dcb0; keep in step by hand.
// Changes: none.
// Options where each one needs a sentence: the first step of a wizard
// (zone type, upstream provider, list type) and opt-in lists such as the
// list wizard's well-known lists. One choice is an Astryx RadioList, several
// (`multiple`) a CheckboxList, both with the sentence under each option's
// label, an optional icon before it (radios) and content after it (a
// license badge), so arrows or Tab move between options and the choice is
// announced like any radio or checkbox group. Help sits under the options
// (the dashboard rule, see FieldHelp) and describes the group. Options
// carry data-choice for tests.
import { CheckboxList, CheckboxListItem } from '@astryxdesign/core/CheckboxList';
import { RadioList, RadioListItem } from '@astryxdesign/core/RadioList';
import type { ReactNode } from 'react';
import { FieldShell } from './FieldHelp';

export interface Choice {
  value: string;
  label: string;
  description?: ReactNode;
  /** Shown before the label (a provider icon); one-choice cards only. */
  icon?: ReactNode;
  /** Shown after the label (a license or size badge). */
  endContent?: ReactNode;
  isDisabled?: boolean;
}

interface CommonProps {
  label: string;
  isLabelHidden?: boolean;
  choices: Choice[];
  error?: string | undefined;
  /** Supporting text under the options. */
  help?: ReactNode;
}

export interface SingleChoiceProps extends CommonProps {
  multiple?: false;
  value: string;
  onChange: (v: string) => void;
}

export interface MultipleChoiceProps extends CommonProps {
  multiple: true;
  value: string[];
  onChange: (v: string[]) => void;
  /** Lines between the options (many options with long descriptions). */
  hasDividers?: boolean;
}

export type ChoiceCardsProps = SingleChoiceProps | MultipleChoiceProps;

export function ChoiceCards(props: ChoiceCardsProps) {
  const { label, isLabelHidden, choices, error, help } = props;
  return (
    <FieldShell help={help} error={error} describes="group">
      {(status) =>
        props.multiple ? (
          <CheckboxList label={label} isLabelHidden={isLabelHidden} value={props.value} onChange={props.onChange} hasDividers={props.hasDividers} status={status}>
            {choices.map((c) => (
              <CheckboxListItem key={c.value} value={c.value} label={c.label} description={c.description} endContent={c.endContent} isDisabled={c.isDisabled} data-choice={c.value} />
            ))}
          </CheckboxList>
        ) : (
          <RadioList label={label} isLabelHidden={isLabelHidden} value={props.value} onChange={props.onChange} status={status}>
            {choices.map((c) => (
              <RadioListItem key={c.value} value={c.value} label={c.label} description={c.description} startContent={c.icon} endContent={c.endContent} isDisabled={c.isDisabled} data-choice={c.value} />
            ))}
          </RadioList>
        )
      }
    </FieldShell>
  );
}
