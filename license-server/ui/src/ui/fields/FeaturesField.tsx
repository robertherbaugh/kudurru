// What a key unlocks (design spec §0.3, §7): two choices, "All features,
// including ones added later" (the key carries no features claim) or
// "Only these features" with a list of the catalog's features. An explicit
// list is never empty: an empty one would mean every feature to a node
// while reading as none here, so the field reports it and featuresError
// blocks the form. Ticking every feature shows that the list is fixed
// (features added in later releases are not included).
//
// The value is the wire's: null for all features, otherwise the list ([]
// only while the person is choosing, and then with an error).
import { Text } from '@astryxdesign/core/Text';
import { VStack } from '@astryxdesign/core/VStack';
import type { ReactNode } from 'react';
import { ALL_FEATURES_LABEL, FEATURES, type Feature } from '@shared/catalog';
import { ChoiceCards } from './ChoiceCards';

/** The note when every feature is ticked. */
export const FIXED_LIST_NOTE = 'This is a fixed list: features added in later releases are not included';

/** The error of an empty explicit list. */
export const EMPTY_LIST_ERROR = 'Choose at least one feature, or all features';

/** The error of a features value, or null (null and a non-empty list are fine). */
export function featuresError(value: readonly Feature[] | null): string | null {
  return value !== null && value.length === 0 ? EMPTY_LIST_ERROR : null;
}

/** The features in words: the all-features label, or the labels in catalog order. */
export function featuresInWords(value: readonly string[] | null): string {
  if (value === null) return ALL_FEATURES_LABEL;
  const known = FEATURES.filter((f) => value.includes(f.key)).map((f) => f.label);
  const unknown = value.filter((v) => !FEATURES.some((f) => f.key === v));
  return [...known, ...unknown].join(', ');
}

export interface FeaturesFieldProps {
  value: readonly Feature[] | null;
  onChange: (v: Feature[] | null) => void;
  /** Only "all features" can be chosen (a personal key), with this reason under it. */
  lockedAllReason?: string | undefined;
  /** Show the empty-list error even before the list was touched (after a failed Next). */
  showErrors?: boolean | undefined;
  help?: ReactNode;
  /** An error for the whole field (the server's 400 on `features`). */
  error?: string | undefined;
}

export function FeaturesField({ value, onChange, lockedAllReason, showErrors, help, error }: FeaturesFieldProps) {
  const mode = value === null ? 'all' : 'only';
  const list = value ?? [];
  const allTicked = value !== null && FEATURES.every((f) => list.includes(f.key));
  const err = value !== null && (showErrors || list.length === 0) ? (featuresError(value) ?? undefined) : undefined;
  return (
    <VStack gap={3}>
      <ChoiceCards
        label="Features"
        value={mode}
        onChange={(v) => onChange(v === 'all' ? null : [])}
        help={lockedAllReason ?? help}
        error={error}
        choices={[
          { value: 'all', label: ALL_FEATURES_LABEL, description: 'The key carries no features list, so nodes grant every feature their release knows, including later ones.' },
          {
            value: 'only',
            label: 'Only these features',
            description: 'The key names its features; nodes grant those and nothing added later.',
            isDisabled: lockedAllReason !== undefined,
          },
        ]}
      />
      {mode === 'only' ? (
        <VStack gap={1}>
          <ChoiceCards
            label="Included features"
            multiple
            hasDividers
            value={[...list]}
            onChange={(v) => onChange(FEATURES.filter((f) => v.includes(f.key)).map((f) => f.key))}
            choices={FEATURES.map((f) => ({ value: f.key, label: f.label, description: f.unlocks }))}
            error={err}
          />
          {allTicked ? (
            <Text type="supporting" display="block" data-testid="features-fixed-note">
              {FIXED_LIST_NOTE}
            </Text>
          ) : null}
        </VStack>
      ) : null}
    </VStack>
  );
}
