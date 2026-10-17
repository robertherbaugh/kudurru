// One control per C1 shape, recursively (Astryx controls, help under
// each control): Switch for booleans, a bounded NumberInput, DurationField with the key's presets, a size with a unit,
// EnumSelect for closed sets, a picker for references, a format-checked
// text input, a row editor for string lists, and rows of fieldsets for
// lists of objects and maps. It edits drafts (lib/shapes.ts), never JSON.
import { Button } from '@astryxdesign/core/Button';
import { Grid } from '@astryxdesign/core/Grid';
import { HStack, StackItem, VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { Trash2 } from 'lucide-react';
import type { ObjectKind, SettingRef } from '../../api/types';
import { emptyDraft, isMapDraft, keyError, str, type MapDraft } from '../../lib/shapes';
import type { UiShape } from '../../lib/settingsMeta';
import { UpstreamGroupSelect } from '../UpstreamGroupSelect';
import { BytesField } from './BytesField';
import { DURATION_PRESETS, DurationField, durationPresets } from './DurationField';
import { EnumSelect } from './EnumSelect';
import { FieldGroup } from './FieldGroup';
import { FORMAT_EXAMPLES, formatError } from './formats';
import { NumberField } from './NumberField';
import { ObjectRefPicker } from './ObjectRefPicker';
import { StringListField } from './StringListField';
import { SwitchField, TextField } from './TextField';

const REF_KINDS: Record<Exclude<SettingRef, 'upstreamGroup'>, Exclude<ObjectKind, 'dnsrecordsets'>> = {
  clientGroup: 'clientgroups',
  blockList: 'blocklists',
  policy: 'policies',
  zone: 'dnszones',
};

export interface ShapeFieldProps {
  label: string;
  isLabelHidden?: boolean;
  description?: string | undefined;
  shape: UiShape;
  value: unknown;
  onChange: (v: unknown) => void;
  isDisabled?: boolean | undefined;
  /** The key's default, shown as the placeholder of empty text inputs. */
  placeholder?: string | undefined;
}

/** Shapes drawn as a group of fields (they take a row's full width). */
function isGroup(s: UiShape): boolean {
  return s.type === 'object' || s.type === 'list' || s.type === 'map' || s.type === 'string[]';
}

/** A row's remove button: an icon, named "Remove <row>" (the name is its tooltip too). */
function RemoveButton({ label, isDisabled, onClick }: { label: string; isDisabled?: boolean | undefined; onClick: () => void }) {
  return <Button label={label} tooltip={label} size="sm" variant="ghost" isIconOnly icon={<Trash2 size={16} aria-hidden="true" />} isDisabled={isDisabled} onClick={onClick} />;
}

/** Help under a group of fields. */
function Help({ text }: { text?: string | undefined }) {
  return text ? (
    <Text type="supporting" display="block">
      {text}
    </Text>
  ) : null;
}

export function ShapeField({ label, isLabelHidden, description, shape, value, onChange, isDisabled, placeholder }: ShapeFieldProps) {
  switch (shape.type) {
    case 'bool':
      return <SwitchField label={label} isLabelHidden={isLabelHidden} help={description} value={value === true} isDisabled={isDisabled} onChange={onChange} />;
    case 'int':
      return (
        <NumberField
          label={label}
          isLabelHidden={isLabelHidden}
          help={description}
          value={str(value)}
          onChange={onChange}
          min={shape.min}
          max={shape.max}
          placeholder={placeholder}
          isDisabled={isDisabled}
        />
      );
    case 'duration':
      return (
        <DurationField
          label={label}
          isLabelHidden={isLabelHidden}
          description={description}
          value={str(value)}
          onChange={onChange}
          presets={shape.presets?.length ? durationPresets(shape.presets) : DURATION_PRESETS}
          isDisabled={isDisabled}
        />
      );
    case 'bytes':
      return <BytesField label={label} isLabelHidden={isLabelHidden} description={description} value={value} onChange={onChange} isDisabled={isDisabled} />;
    case 'string[]':
      return (
        <StringListField
          label={label}
          isLabelHidden={isLabelHidden}
          description={description}
          value={Array.isArray(value) ? (value as string[]) : []}
          onChange={onChange}
          options={shape.items?.enum ?? undefined}
          placeholder={shape.items?.format ? `e.g. ${FORMAT_EXAMPLES[shape.items.format]}` : undefined}
          itemError={shape.items?.format ? (v) => formatError(shape.items?.format, v) : undefined}
          isDisabled={isDisabled}
        />
      );
    case 'list':
      return <ListRows label={label} isLabelHidden={isLabelHidden} description={description} shape={shape} value={value} onChange={onChange} isDisabled={isDisabled} />;
    case 'map':
      return <MapRows label={label} isLabelHidden={isLabelHidden} description={description} shape={shape} value={value} onChange={onChange} isDisabled={isDisabled} />;
    case 'object':
      return (
        <FieldGroup label={label} isLabelHidden={isLabelHidden}>
          <Grid columns={{ minWidth: 220, repeat: 'fit' }} gap={3} align="start">
            {(shape.fields ?? []).map((f) => {
              const o = (value && typeof value === 'object' ? value : {}) as Record<string, unknown>;
              return (
                <ShapeField
                  key={f.name}
                  label={f.label ?? f.name}
                  description={f.help}
                  shape={f}
                  value={o[f.name]}
                  onChange={(v) => onChange({ ...o, [f.name]: v })}
                  isDisabled={isDisabled}
                />
              );
            })}
          </Grid>
          <Help text={description} />
        </FieldGroup>
      );
    default:
      return <StringField label={label} isLabelHidden={isLabelHidden} description={description} shape={shape} value={value} onChange={onChange} isDisabled={isDisabled} placeholder={placeholder} />;
  }
}

function StringField({ label, isLabelHidden, description, shape, value, onChange, isDisabled, placeholder }: ShapeFieldProps) {
  const v = str(value);
  if (shape.enum?.length) {
    return <EnumSelect label={label} isLabelHidden={isLabelHidden} description={description} value={v} options={shape.enum} onChange={onChange} isDisabled={isDisabled} />;
  }
  if (shape.ref === 'upstreamGroup') {
    return <UpstreamGroupSelect label={label} value={v} onChange={onChange} allowEmpty={false} isDisabled={isDisabled} help={description} />;
  }
  if (shape.ref) {
    return (
      <ObjectRefPicker
        kind={REF_KINDS[shape.ref]}
        label={label}
        description={description}
        value={v ? [v] : []}
        onChange={(x) => onChange(x[0] ?? '')}
        isDisabled={isDisabled}
      />
    );
  }
  const err = formatError(shape.format, v);
  return (
    <TextField
      label={label}
      isLabelHidden={isLabelHidden}
      help={description}
      value={v}
      onChange={onChange}
      isDisabled={isDisabled}
      placeholder={placeholder || (shape.format ? `e.g. ${FORMAT_EXAMPLES[shape.format]}` : undefined)}
      error={err ?? undefined}
    />
  );
}

function ListRows({ label, isLabelHidden, description, shape, value, onChange, isDisabled }: Omit<ShapeFieldProps, 'placeholder'>) {
  const items: unknown[] = Array.isArray(value) ? (value as unknown[]) : [];
  const item = shape.items as UiShape;
  // A group of fields per entry takes the full width, with Remove under it.
  const wide = isGroup(item);
  return (
    <FieldGroup label={label} isLabelHidden={isLabelHidden}>
      {items.length === 0 ? <Text type="supporting">None.</Text> : null}
      {items.map((x, i) => {
        const field = <ShapeField label={`${label} ${i + 1}`} shape={item} value={x} onChange={(v) => onChange(items.map((y, j) => (j === i ? v : y)))} isDisabled={isDisabled} />;
        const remove = <RemoveButton label={`Remove ${label} ${i + 1}`} isDisabled={isDisabled} onClick={() => onChange(items.filter((_, j) => j !== i))} />;
        return wide ? (
          <VStack key={i} gap={2} data-testid="list-row">
            {field}
            <HStack gap={2}>{remove}</HStack>
          </VStack>
        ) : (
          <HStack key={i} gap={2} vAlign="end" data-testid="list-row">
            <StackItem size="fill">{field}</StackItem>
            {remove}
          </HStack>
        );
      })}
      <HStack gap={2}>
        <Button label="Add" size="sm" variant="secondary" isDisabled={isDisabled} onClick={() => onChange([...items, emptyDraft(item)])} />
      </HStack>
      <Help text={description} />
    </FieldGroup>
  );
}

function MapRows({ label, isLabelHidden, description, shape, value, onChange, isDisabled }: Omit<ShapeFieldProps, 'placeholder'>) {
  const rows = isMapDraft(value) ? value.__rows : [];
  const vshape = shape.values as UiShape;
  const keyLabel = shape.keyLabel ?? 'Key';
  const keyErr = (k: string) => (k.trim() && shape.keys ? keyError(shape.keys, k.trim()) : null);
  const emit = (r: [string, unknown][]) => onChange({ __rows: r } satisfies MapDraft);
  const counts = new Map<string, number>();
  for (const [k] of rows) counts.set(k.trim(), (counts.get(k.trim()) ?? 0) + 1);
  return (
    <FieldGroup label={label} isLabelHidden={isLabelHidden}>
      {rows.length === 0 ? <Text type="supporting">None.</Text> : null}
      {rows.map(([k, v], i) => {
        const dup = k.trim() && (counts.get(k.trim()) ?? 0) > 1;
        const keyField =
          shape.keys?.type === 'int' ? (
            <NumberField
              label={keyLabel}
              isLabelHidden={i > 0}
              value={k}
              placeholder={shape.keyPlaceholder}
              min={shape.keys.min}
              max={shape.keys.max}
              onChange={(nk) => emit(rows.map((r, j) => (j === i ? [nk, r[1]] : r)))}
              isDisabled={isDisabled}
              error={dup ? 'Listed twice' : undefined}
            />
          ) : (
            <TextField
              label={keyLabel}
              isLabelHidden={i > 0}
              value={k}
              placeholder={shape.keyPlaceholder}
              onChange={(nk) => emit(rows.map((r, j) => (j === i ? [nk, r[1]] : r)))}
              isDisabled={isDisabled}
              error={dup ? 'Listed twice' : (keyErr(k) ?? undefined)}
            />
          );
        const valueField = (
          <ShapeField
            label={vshape.type === 'object' ? (k || `${keyLabel} ${i + 1}`) : 'Value'}
            isLabelHidden={vshape.type !== 'object' && i > 0}
            shape={vshape}
            value={v}
            onChange={(nv) => emit(rows.map((r, j) => (j === i ? [r[0], nv] : r)))}
            isDisabled={isDisabled}
          />
        );
        const remove = <RemoveButton label={`Remove ${k || `row ${i + 1}`}`} isDisabled={isDisabled} onClick={() => emit(rows.filter((_, j) => j !== i))} />;
        // A group of fields per value takes the full width under its key.
        return isGroup(vshape) ? (
          <VStack key={i} gap={2} data-testid="map-row">
            <HStack gap={2} vAlign="end">
              <StackItem size="fill">{keyField}</StackItem>
              {remove}
            </HStack>
            {valueField}
          </VStack>
        ) : (
          // Key and value side by side where there is room, stacked in a narrow column.
          <HStack key={i} gap={2} vAlign="end" data-testid="map-row">
            <StackItem size="fill">
              <Grid columns={{ minWidth: 200, repeat: 'fit' }} gap={2} align="start">
                {keyField}
                {valueField}
              </Grid>
            </StackItem>
            {remove}
          </HStack>
        );
      })}
      <HStack gap={2}>
        <Button label="Add" size="sm" variant="secondary" isDisabled={isDisabled} onClick={() => emit([...rows, ['', emptyDraft(vshape)]])} />
      </HStack>
      <Help text={description} />
    </FieldGroup>
  );
}
