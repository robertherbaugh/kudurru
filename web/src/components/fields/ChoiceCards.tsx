// A closed choice where each option needs a sentence, for the first step
// of a wizard (zone type, upstream provider): an Astryx RadioList with the
// sentence under each option's label and an optional icon before it, so
// arrows move between options and the choice is announced like any radio
// group. Options carry data-choice for tests.
import { FieldStatus } from '@astryxdesign/core/FieldStatus';
import { RadioList, RadioListItem } from '@astryxdesign/core/RadioList';
import { VStack } from '@astryxdesign/core/VStack';
import type { ReactNode } from 'react';

export interface Choice {
  value: string;
  label: string;
  description?: ReactNode;
  /** Shown before the label (a provider icon). */
  icon?: ReactNode;
  isDisabled?: boolean;
}

export interface ChoiceCardsProps {
  label: string;
  isLabelHidden?: boolean;
  value: string;
  choices: Choice[];
  onChange: (v: string) => void;
  error?: string | undefined;
}

export function ChoiceCards({ label, isLabelHidden, value, choices, onChange, error }: ChoiceCardsProps) {
  return (
    <VStack gap={1}>
      <RadioList label={label} isLabelHidden={isLabelHidden} value={value} onChange={onChange} status={error ? { type: 'error' } : undefined}>
        {choices.map((c) => (
          <RadioListItem key={c.value} value={c.value} label={c.label} description={c.description} startContent={c.icon} isDisabled={c.isDisabled} data-choice={c.value} />
        ))}
      </RadioList>
      {error ? <FieldStatus type="error" message={error} variant="detached" /> : null}
    </VStack>
  );
}
