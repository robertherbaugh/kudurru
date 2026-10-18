// A labelled group of fields (what a <fieldset> with a <legend> was): a
// bordered box named by its label, the label on top (or only for screen
// readers), the fields under it. Every control inside gets its own
// isDisabled, so the group needs no native `disabled`.
import { Card } from '@astryxdesign/core/Card';
import { Text } from '@astryxdesign/core/Text';
import { VStack } from '@astryxdesign/core/VStack';
import { useId, type ReactNode } from 'react';

export interface FieldGroupProps {
  label: string;
  /** Name the group for screen readers only. */
  isLabelHidden?: boolean | undefined;
  /** The id of help text that describes the whole group. */
  describedBy?: string | undefined;
  children: ReactNode;
  'data-testid'?: string;
}

export function FieldGroup({ label, isLabelHidden, describedBy, children, ...rest }: FieldGroupProps) {
  const id = useId();
  return (
    <Card
      padding={3}
      role="group"
      aria-label={isLabelHidden ? label : undefined}
      aria-labelledby={isLabelHidden ? undefined : id}
      aria-describedby={describedBy}
      {...rest}
    >
      <VStack gap={2}>
        {isLabelHidden ? null : (
          <Text id={id} weight="semibold" size="sm">
            {label}
          </Text>
        )}
        {children}
      </VStack>
    </Card>
  );
}
