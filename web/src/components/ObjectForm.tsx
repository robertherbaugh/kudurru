// The generic create/edit form for BlockList, generated from the field
// descriptors in lib/objectSchemas (§10 principle 6). ClientGroups and
// Policies have their own pages and wizards. Every object gets a comment and an enabled switch; objects
// owned by a file, Kubernetes or an integration open read-only with the
// owner named.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { Collapsible } from '@astryxdesign/core/Collapsible';
import { FieldStatus } from '@astryxdesign/core/FieldStatus';
import { Grid } from '@astryxdesign/core/Grid';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useState, type ReactNode } from 'react';
import type { AnyObject } from '../api/types';
import { objectComment, objectEnabled, objectOwnership, readOnlyReason, withMeta } from '../lib/managedBy';
import { durationError, nameError, type FieldDef, type KindSchema } from '../lib/objectSchemas';
import { errorText } from './Feedback';
import { FieldGroup } from './fields/FieldGroup';
import { NumberField } from './fields/NumberField';
import { SelectField } from './fields/SelectField';
import { SwitchField, TextAreaField, TextField } from './fields/TextField';
import { DialogForm } from './FormDialog';

type Obj = Record<string, unknown>;
export type FieldErrors = Record<string, string>;

/** Validates spec values against the descriptors; keys are dotted paths. */
export function validateFields(fields: FieldDef[], value: Obj, prefix = ''): FieldErrors {
  const errs: FieldErrors = {};
  for (const f of fields) {
    const path = prefix ? `${prefix}.${f.key}` : f.key;
    const v = value[f.key];
    switch (f.type) {
      case 'text': {
        const s = typeof v === 'string' ? v.trim() : '';
        if (f.required && !s) errs[path] = 'Required';
        else if (s && f.validate) {
          const e = f.validate(s);
          if (e) errs[path] = e;
        }
        break;
      }
      case 'duration': {
        const e = durationError(typeof v === 'string' ? v.trim() : '');
        if (e) errs[path] = e;
        break;
      }
      case 'enum':
        if (f.required && !v) errs[path] = 'Required';
        break;
      case 'number':
        if (v !== undefined && v !== '' && (typeof v !== 'number' || Number.isNaN(v))) errs[path] = 'Must be a number';
        else if (typeof v === 'number' && ((f.min !== undefined && v < f.min) || (f.max !== undefined && v > f.max))) {
          errs[path] = `Between ${f.min ?? '-∞'} and ${f.max ?? '∞'}`;
        }
        break;
      case 'strings':
        if (Array.isArray(v) && f.validate) {
          const bad = (v as string[]).map((s, i) => [i, s.trim()] as const).filter(([, s]) => s && f.validate?.(s));
          if (bad.length) {
            const [i, s] = bad[0] as readonly [number, string];
            errs[path] = `Line ${i + 1}: ${f.validate(s)}${bad.length > 1 ? ` (and ${bad.length - 1} more)` : ''}`;
          }
        }
        break;
      case 'objects':
        if (Array.isArray(v)) {
          (v as Obj[]).forEach((item, i) => Object.assign(errs, validateFields(f.fields, item ?? {}, `${path}.${i}`)));
        }
        break;
      case 'object':
        if (v && typeof v === 'object') Object.assign(errs, validateFields(f.fields, v as Obj, path));
        break;
      default:
        break;
    }
  }
  return errs;
}

/** Drops empty strings, blank lines, empty arrays and empty objects. */
export function cleanSpec(fields: FieldDef[], value: Obj): Obj {
  const out: Obj = { ...value };
  for (const f of fields) {
    const v = value[f.key];
    let next: unknown = v;
    switch (f.type) {
      case 'text':
      case 'duration':
      case 'enum':
        next = typeof v === 'string' ? v.trim() : v;
        if (next === '') next = undefined;
        break;
      case 'number':
        next = typeof v === 'number' && !Number.isNaN(v) ? v : undefined;
        break;
      case 'strings':
        next = Array.isArray(v) ? (v as string[]).map((s) => s.trim()).filter(Boolean) : undefined;
        if (Array.isArray(next) && next.length === 0) next = undefined;
        break;
      case 'objects':
        next = Array.isArray(v) ? (v as Obj[]).map((item) => cleanSpec(f.fields, item ?? {})).filter((o) => Object.keys(o).length > 0) : undefined;
        if (Array.isArray(next) && next.length === 0 && f.key !== 'sources') next = undefined;
        break;
      case 'object':
        next = v && typeof v === 'object' ? cleanSpec(f.fields, v as Obj) : undefined;
        if (next && Object.keys(next).length === 0) next = undefined;
        break;
      case 'bool':
        next = typeof v === 'boolean' ? v : undefined;
        break;
    }
    if (next === undefined) delete out[f.key];
    else out[f.key] = next;
  }
  return out;
}

export interface ObjectFormProps {
  schema: KindSchema;
  /** The object to edit; undefined creates a new one. */
  initial?: AnyObject;
  defaultNamespace?: string;
  onSubmit: (obj: AnyObject) => Promise<void>;
  onCancel: () => void;
}

export function ObjectForm({ schema, initial, defaultNamespace = 'default', onSubmit, onCancel }: ObjectFormProps) {
  const own = initial ? objectOwnership(initial) : undefined;
  const readOnly = !!own && !own.editable;
  const [name, setName] = useState(initial?.metadata.name ?? '');
  const [namespace, setNamespace] = useState(initial?.metadata.namespace ?? defaultNamespace);
  const [comment, setComment] = useState(initial ? objectComment(initial) : '');
  const [enabled, setEnabled] = useState(initial ? objectEnabled(initial) : true);
  const [spec, setSpec] = useState<Obj>(() => structuredClone((initial?.spec as Obj | undefined) ?? schema.empty()));
  const [errors, setErrors] = useState<FieldErrors>({});
  const [serverError, setServerError] = useState<string | undefined>();
  const [busy, setBusy] = useState(false);

  const submit = async () => {
    const errs = validateFields(schema.fields, spec);
    if (!initial) {
      const ne = nameError(name);
      if (ne) errs['metadata.name'] = ne;
    }
    setErrors(errs);
    if (Object.keys(errs).length) return;
    const base = {
      apiVersion: 'kudurru.io/v1alpha1',
      kind: schema.objectKind,
      metadata: initial
        ? { ...initial.metadata }
        : { name, ...(schema.namespaced ? { namespace: namespace || defaultNamespace } : {}) },
      spec: cleanSpec(schema.fields, spec),
    } as unknown as AnyObject;
    setBusy(true);
    setServerError(undefined);
    try {
      await onSubmit(withMeta(base, comment, enabled));
    } catch (err) {
      setServerError(errorText(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <DialogForm
      gap={3}
      noValidate
      onSubmit={() => {
        if (!readOnly) void submit();
      }}
      footer={
        <>
          <Button label={readOnly ? 'Close' : 'Cancel'} variant="ghost" onClick={onCancel} />
          {readOnly ? null : <Button type="submit" label={initial ? 'Save' : `Create ${schema.singular}`} variant="primary" isLoading={busy} />}
        </>
      }
    >
      {readOnly && own ? <Banner status="info" title={`Read-only: managed by ${own.owner}`} description={readOnlyReason(own)} /> : null}
      <FormGrid>
        <TextField
          label="Name"
          value={name}
          onChange={setName}
          isReadOnly={!!initial}
          isRequired
          error={errors['metadata.name']}
          help={initial ? undefined : 'Lower-case letters, digits and "-"'}
        />
        {schema.namespaced ? <TextField label="Namespace" value={namespace} onChange={setNamespace} isReadOnly={!!initial} /> : null}
      </FormGrid>
      <TextField label="Comment" value={comment} onChange={setComment} isDisabled={readOnly} isOptional />
      <SwitchField label="Enabled" value={enabled} onChange={setEnabled} isDisabled={readOnly} help="Disabled objects stay stored but take no effect." />
      <Fields fields={schema.fields} value={spec} onChange={setSpec} errors={errors} disabled={readOnly} />
      {serverError ? (
        <Banner status="error" title="The server rejected the change" description={serverError} />
      ) : null}
      {Object.keys(errors).length > 0 ? (
        <FieldStatus
          type="error"
          variant="detached"
          role="alert"
          message={`Fix the ${Object.keys(errors).length} highlighted field${Object.keys(errors).length === 1 ? '' : 's'}.`}
        />
      ) : null}
    </DialogForm>
  );
}

interface FieldsProps {
  fields: FieldDef[];
  value: Obj;
  onChange: (v: Obj) => void;
  errors: FieldErrors;
  disabled: boolean;
  prefix?: string;
}

function Fields({ fields, value, onChange, errors, disabled, prefix = '' }: FieldsProps) {
  return (
    <>
      {fields.map((f) => (
        <Field
          key={f.key}
          def={f}
          path={prefix ? `${prefix}.${f.key}` : f.key}
          value={value[f.key]}
          onChange={(v) => onChange({ ...value, [f.key]: v })}
          errors={errors}
          disabled={disabled}
        />
      ))}
    </>
  );
}

function Field({
  def,
  path,
  value,
  onChange,
  errors,
  disabled,
}: {
  def: FieldDef;
  path: string;
  value: unknown;
  onChange: (v: unknown) => void;
  errors: FieldErrors;
  disabled: boolean;
}): ReactNode {
  switch (def.type) {
    case 'text':
    case 'duration':
      return (
        <TextField
          label={def.label}
          help={def.help}
          placeholder={def.placeholder}
          value={typeof value === 'string' ? value : ''}
          onChange={(v) => onChange(v)}
          isDisabled={disabled}
          isRequired={def.type === 'text' && def.required}
          error={errors[path]}
        />
      );
    case 'number':
      return (
        <NumberField
          label={def.label}
          help={def.help}
          min={def.min}
          max={def.max}
          optional
          value={typeof value === 'number' ? String(value) : typeof value === 'string' ? value : ''}
          onChange={(v) => onChange(v.trim() === '' ? undefined : /^-?\d+$/.test(v.trim()) ? Number(v) : v)}
          isDisabled={disabled}
          error={errors[path]}
        />
      );
    case 'bool':
      return <SwitchField label={def.label} help={def.help} value={value === true} onChange={(v) => onChange(v)} isDisabled={disabled} />;
    case 'enum':
      return (
        <SelectField
          label={def.label}
          value={typeof value === 'string' ? value : ''}
          options={def.options}
          emptyLabel={!def.required && !def.options.some((o) => o.value === '') ? 'Not set' : undefined}
          onChange={(v) => onChange(v)}
          isDisabled={disabled}
          help={def.help}
          error={errors[path]}
        />
      );
    case 'strings':
      return (
        <TextAreaField
          label={def.label}
          help={def.help}
          placeholder={def.placeholder}
          rows={4}
          value={Array.isArray(value) ? (value as string[]).join('\n') : ''}
          onChange={(v) => onChange(v.split(/\r?\n/))}
          isDisabled={disabled}
          error={errors[path]}
          hasSpellCheck={false}
        />
      );
    case 'object': {
      const obj = value && typeof value === 'object' ? (value as Obj) : {};
      return (
        <FieldGroup label={def.label}>
          <Help text={def.help} />
          <FormGrid>
            <Fields fields={def.fields} value={obj} onChange={(v) => onChange(v)} errors={errors} disabled={disabled} prefix={path} />
          </FormGrid>
        </FieldGroup>
      );
    }
    case 'objects':
      return <ObjectsField def={def} path={path} value={Array.isArray(value) ? (value as Obj[]) : []} onChange={onChange} errors={errors} disabled={disabled} />;
  }
}

function ObjectsField({
  def,
  path,
  value,
  onChange,
  errors,
  disabled,
}: {
  def: Extract<FieldDef, { type: 'objects' }>;
  path: string;
  value: Obj[];
  onChange: (v: unknown) => void;
  errors: FieldErrors;
  disabled: boolean;
}) {
  const [bulk, setBulk] = useState('');
  const [showBulk, setShowBulk] = useState(false);
  const firstText = def.fields.find((f) => f.type === 'text');
  const addMany = () => {
    // Multi-add: one item per line, into the first text field.
    const lines = bulk.split(/[\r\n,]+/).map((s) => s.trim()).filter(Boolean);
    const items = lines.map((l) => ({ [firstText?.key ?? 'name']: l }));
    onChange([...value, ...items]);
    setBulk('');
    setShowBulk(false);
  };
  return (
    <FieldGroup label={def.label}>
      <Help text={def.help} />
      {value.length === 0 ? <Text type="supporting">No {def.itemLabel}s yet.</Text> : null}
      {value.map((item, i) => (
        <Entry
          key={i}
          title={def.summary ? def.summary(item ?? {}) : `${def.itemLabel} ${i + 1}`}
          defaultIsOpen={value.length <= 3}
          hasErrors={Object.keys(errors).some((k) => k.startsWith(`${path}.${i}.`))}
        >
          <FormGrid>
            <Fields fields={def.fields} value={item ?? {}} onChange={(v) => onChange(value.map((x, j) => (j === i ? v : x)))} errors={errors} disabled={disabled} prefix={`${path}.${i}`} />
          </FormGrid>
          {disabled ? null : (
            <HStack gap={2}>
              <Button label={`Remove ${def.itemLabel}`} variant="ghost" size="sm" onClick={() => onChange(value.filter((_, j) => j !== i))} />
            </HStack>
          )}
        </Entry>
      ))}
      {disabled ? null : (
        <HStack gap={2} vAlign="center" wrap="wrap">
          <Button label={`Add ${def.itemLabel}`} variant="secondary" size="sm" onClick={() => onChange([...value, {}])} />
          <Button label="Add many" variant="ghost" size="sm" onClick={() => setShowBulk((s) => !s)} />
        </HStack>
      )}
      {showBulk ? (
        <VStack gap={2}>
          <TextAreaField label={`Add many ${def.itemLabel}s`} help="One per line or comma separated" value={bulk} onChange={setBulk} rows={4} hasSpellCheck={false} />
          <HStack gap={2} vAlign="center" wrap="wrap">
            <Button label={`Add ${bulk.split(/[\r\n,]+/).filter((s) => s.trim()).length || ''} ${def.itemLabel}s`.replace('  ', ' ')} size="sm" variant="primary" onClick={addMany} />
          </HStack>
        </VStack>
      ) : null}
    </FieldGroup>
  );
}

/** The fields of a form in columns (at least 220 px each). */
function FormGrid({ children }: { children: ReactNode }) {
  return (
    <Grid columns={{ minWidth: 220, repeat: 'fit' }} gap={3} align="start">
      {children}
    </Grid>
  );
}

/** Help under a group's label. */
function Help({ text }: { text?: string | undefined }) {
  return text ? <Text type="supporting">{text}</Text> : null;
}

/** One entry of a list of objects: collapsible, forced open while it has an error. */
function Entry({ title, defaultIsOpen, hasErrors, children }: { title: string; defaultIsOpen: boolean; hasErrors: boolean; children: ReactNode }) {
  const [open, setOpen] = useState(defaultIsOpen);
  return (
    <Card padding={2}>
      <Collapsible trigger={<Text type="code">{title}</Text>} isOpen={open || hasErrors} onOpenChange={setOpen}>
        <VStack gap={2}>{children}</VStack>
      </Collapsible>
    </Card>
  );
}
