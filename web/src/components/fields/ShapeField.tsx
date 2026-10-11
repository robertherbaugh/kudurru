// One control per C1 shape, recursively: Switch for booleans, a bounded
// number input, DurationField with the key's presets, a size with a unit,
// EnumSelect for closed sets, a picker for references, a format-checked
// text input, a row editor for string lists, and rows of fieldsets for
// lists of objects and maps. It edits drafts (lib/shapes.ts), never JSON.
import { Button } from '@astryxdesign/core/Button';
import { Switch } from '@astryxdesign/core/Switch';
import { TextInput } from '@astryxdesign/core/TextInput';
import type { ObjectKind, SettingRef } from '../../api/types';
import { emptyDraft, isMapDraft, keyError, str, type MapDraft } from '../../lib/shapes';
import type { UiShape } from '../../lib/settingsMeta';
import { UpstreamGroupSelect } from '../UpstreamGroupSelect';
import { BytesField } from './BytesField';
import { DURATION_PRESETS, DurationField, durationPresets } from './DurationField';
import { EnumSelect } from './EnumSelect';
import { FORMAT_EXAMPLES, formatError } from './formats';
import { NumberField } from './NumberField';
import { ObjectRefPicker } from './ObjectRefPicker';
import { StringListField } from './StringListField';

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

export function ShapeField({ label, isLabelHidden, description, shape, value, onChange, isDisabled, placeholder }: ShapeFieldProps) {
  switch (shape.type) {
    case 'bool':
      return <Switch label={label} isLabelHidden={isLabelHidden} description={description} value={value === true} isDisabled={isDisabled} onChange={onChange} />;
    case 'int':
      return (
        <NumberField
          label={label}
          isLabelHidden={isLabelHidden}
          description={description}
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
        <fieldset className="k-fieldset" disabled={isDisabled}>
          <legend className={isLabelHidden ? 'k-visually-hidden' : undefined}>{label}</legend>
          {description ? <span className="k-muted k-small">{description}</span> : null}
          <div className="k-form-grid">
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
          </div>
        </fieldset>
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
    <TextInput
      label={label}
      isLabelHidden={isLabelHidden}
      description={description}
      value={v}
      onChange={onChange}
      isDisabled={isDisabled}
      placeholder={placeholder || (shape.format ? `e.g. ${FORMAT_EXAMPLES[shape.format]}` : undefined)}
      status={err ? { type: 'error', message: err } : undefined}
    />
  );
}

function ListRows({ label, isLabelHidden, description, shape, value, onChange, isDisabled }: Omit<ShapeFieldProps, 'placeholder'>) {
  const items: unknown[] = Array.isArray(value) ? (value as unknown[]) : [];
  const item = shape.items as UiShape;
  return (
    <fieldset className="k-fieldset" disabled={isDisabled}>
      <legend className={isLabelHidden ? 'k-visually-hidden' : undefined}>{label}</legend>
      {description ? <span className="k-muted k-small">{description}</span> : null}
      {items.length === 0 ? <span className="k-muted k-small">None.</span> : null}
      {items.map((x, i) => (
        <div key={i} className="k-array-item" data-testid="list-row">
          <ShapeField label={`${label} ${i + 1}`} shape={item} value={x} onChange={(v) => onChange(items.map((y, j) => (j === i ? v : y)))} isDisabled={isDisabled} />
          <Button label={`Remove ${label} ${i + 1}`} size="sm" variant="ghost" isDisabled={isDisabled} onClick={() => onChange(items.filter((_, j) => j !== i))} />
        </div>
      ))}
      <span className="k-row">
        <Button label="Add" size="sm" variant="secondary" isDisabled={isDisabled} onClick={() => onChange([...items, emptyDraft(item)])} />
      </span>
    </fieldset>
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
    <fieldset className="k-fieldset" disabled={isDisabled}>
      <legend className={isLabelHidden ? 'k-visually-hidden' : undefined}>{label}</legend>
      {description ? <span className="k-muted k-small">{description}</span> : null}
      {rows.length === 0 ? <span className="k-muted k-small">None.</span> : null}
      {rows.map(([k, v], i) => {
        const dup = k.trim() && (counts.get(k.trim()) ?? 0) > 1;
        return (
          <div key={i} className="k-map-row" data-testid="map-row">
            {shape.keys?.type === 'int' ? (
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
              <TextInput
                label={keyLabel}
                isLabelHidden={i > 0}
                value={k}
                placeholder={shape.keyPlaceholder}
                onChange={(nk) => emit(rows.map((r, j) => (j === i ? [nk, r[1]] : r)))}
                isDisabled={isDisabled}
                status={dup ? { type: 'error', message: 'Listed twice' } : keyErr(k) ? { type: 'error', message: keyErr(k) ?? '' } : undefined}
              />
            )}
            <ShapeField
              label={vshape.type === 'object' ? (k || `${keyLabel} ${i + 1}`) : 'Value'}
              isLabelHidden={vshape.type !== 'object' && i > 0}
              shape={vshape}
              value={v}
              onChange={(nv) => emit(rows.map((r, j) => (j === i ? [r[0], nv] : r)))}
              isDisabled={isDisabled}
            />
            <Button label={`Remove ${k || `row ${i + 1}`}`} size="sm" variant="ghost" isDisabled={isDisabled} onClick={() => emit(rows.filter((_, j) => j !== i))} />
          </div>
        );
      })}
      <span className="k-row">
        <Button label="Add" size="sm" variant="secondary" isDisabled={isDisabled} onClick={() => emit([...rows, ['', emptyDraft(vshape)]])} />
      </span>
    </fieldset>
  );
}
