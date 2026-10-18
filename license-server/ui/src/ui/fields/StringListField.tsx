// Copied from dashboard/src/components/fields/StringListField.tsx at 9046417; keep in step by hand.
// Changes: `maxItems` (Add is disabled at the limit, with the reason as its
// tooltip) and `type` (the rows' input type, e.g. email).
// A list of strings edited row by row (settings string[] keys, server
// lists, ACLs, name servers): one input per entry, add and remove buttons,
// an inline error per row from `itemError`, and "Paste many" for a block of
// lines. Empty rows are dropped by cleanList() before saving. The help
// sits under the rows and buttons.
import { Button } from '@astryxdesign/core/Button';
import { FieldStatus } from '@astryxdesign/core/FieldStatus';
import { HStack } from '@astryxdesign/core/HStack';
import { StackItem } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { VStack } from '@astryxdesign/core/VStack';
import { Trash2 } from 'lucide-react';
import { useId, useState } from 'react';
import type { EnumOption } from './EnumSelect';
import { FieldGroup } from './FieldGroup';
import { SelectField } from './SelectField';
import { TextAreaField, TextField } from './TextField';

/** Trims entries and drops empty ones. */
export function cleanList(v: string[]): string[] {
  return v.map((s) => s.trim()).filter(Boolean);
}

/** The first row error as "Line n (value): message", or null. */
export function listError(v: string[], itemError?: (s: string) => string | null): string | null {
  if (!itemError) return null;
  const list = cleanList(v);
  for (let i = 0; i < list.length; i++) {
    const e = itemError(list[i] as string);
    if (e) return `Line ${i + 1} (${list[i]}): ${e}`;
  }
  return null;
}

export interface StringListFieldProps {
  label: string;
  isLabelHidden?: boolean | undefined;
  value: string[];
  onChange: (v: string[]) => void;
  description?: string | undefined;
  placeholder?: string | undefined;
  /** A closed set: each row is a select of these. */
  options?: EnumOption[] | undefined;
  itemError?: ((s: string) => string | null) | undefined;
  addLabel?: string;
  isDisabled?: boolean | undefined;
  /** An error for the whole list (e.g. "at least one"). */
  error?: string | undefined;
  /** Offer a textarea for pasting many entries at once. */
  allowPaste?: boolean;
  /** The most rows; Add is disabled at the limit. */
  maxItems?: number | undefined;
  /** The rows' input type. */
  type?: 'text' | 'email' | undefined;
}

export function StringListField({
  label,
  isLabelHidden,
  value,
  onChange,
  description,
  placeholder,
  options,
  itemError,
  addLabel = 'Add',
  isDisabled,
  error,
  allowPaste = !options,
  maxItems,
  type,
}: StringListFieldProps) {
  const full = maxItems !== undefined && value.length >= maxItems;
  const id = useId();
  const [paste, setPaste] = useState<string | null>(null);
  const set = (i: number, v: string) => onChange(value.map((x, j) => (j === i ? v : x)));
  return (
    <FieldGroup label={label} isLabelHidden={isLabelHidden} describedBy={description ? `${id}-d` : undefined}>
      {value.length === 0 ? <Text type="supporting">None.</Text> : null}
      {value.map((v, i) => {
        const e = v.trim() && itemError ? itemError(v.trim()) : null;
        const rowLabel = `${label} ${i + 1}`;
        return (
          <HStack key={i} gap={2} vAlign="start" data-testid="list-row">
            <StackItem size="fill">
              {options ? (
                <SelectField label={rowLabel} isLabelHidden value={v} options={options} placeholder="Choose…" isDisabled={isDisabled} onChange={(nv) => set(i, nv)} />
              ) : (
                <TextField label={rowLabel} isLabelHidden type={type} value={v} onChange={(nv) => set(i, nv)} placeholder={placeholder} isDisabled={isDisabled} error={e ?? undefined} />
              )}
            </StackItem>
            {/* Icon-only, like ShapeField's: "Remove <label> <n>" as text is cut off in narrow cards. */}
            <Button
              label={`Remove ${rowLabel}`}
              tooltip={`Remove ${rowLabel}`}
              size="md"
              variant="ghost"
              isIconOnly
              icon={<Trash2 size={16} aria-hidden="true" />}
              isDisabled={isDisabled}
              onClick={() => onChange(value.filter((_, j) => j !== i))}
            />
          </HStack>
        );
      })}
      {paste !== null ? (
        <VStack gap={2}>
          <TextAreaField label="Paste entries, one per line" mono rows={5} value={paste} onChange={setPaste} hasSpellCheck={false} />
          <HStack gap={2}>
            <Button
              label="Add these"
              size="sm"
              variant="secondary"
              onClick={() => {
                onChange([...cleanList(value), ...cleanList(paste.split(/[\r\n,]+/))]);
                setPaste(null);
              }}
            />
            <Button label="Cancel" size="sm" variant="ghost" onClick={() => setPaste(null)} />
          </HStack>
        </VStack>
      ) : null}
      <HStack gap={2}>
        <Button
          label={addLabel}
          size="sm"
          variant="secondary"
          isDisabled={isDisabled || full}
          tooltip={full ? `At most ${maxItems}` : undefined}
          onClick={() => onChange([...value, options?.[0]?.value ?? ''])}
        />
        {allowPaste && paste === null ? <Button label="Paste many" size="sm" variant="ghost" isDisabled={isDisabled || full} onClick={() => setPaste('')} /> : null}
      </HStack>
      {description ? (
        <Text type="supporting" display="block" id={`${id}-d`}>
          {description}
        </Text>
      ) : null}
      {error ? <FieldStatus type="error" message={error} variant="detached" /> : null}
    </FieldGroup>
  );
}
