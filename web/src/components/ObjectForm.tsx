// The generic create/edit form for BlockList, ClientGroup and Policy,
// generated from the field descriptors in lib/objectSchemas (§10
// principle 6). Every object gets a comment and an enabled switch; objects
// owned by a file, Kubernetes or an integration open read-only with the
// owner named.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { CheckboxInput } from '@astryxdesign/core/CheckboxInput';
import { Switch } from '@astryxdesign/core/Switch';
import { TextArea } from '@astryxdesign/core/TextArea';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useQuery } from '@tanstack/react-query';
import { useId, useState, type ReactNode } from 'react';
import { qk, useApi } from '../api/context';
import type { AnyObject } from '../api/types';
import { objectComment, objectEnabled, objectOwnership, readOnlyReason, withMeta } from '../lib/managedBy';
import { durationError, nameError, type FieldDef, type KindSchema } from '../lib/objectSchemas';
import { errorText } from './Feedback';

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
      case 'refs':
        next = Array.isArray(v) ? (v as string[]).map((s) => s.trim()).filter(Boolean) : undefined;
        if (Array.isArray(next) && next.length === 0) next = undefined;
        break;
      case 'objects':
        next = Array.isArray(v) ? (v as Obj[]).map((item) => cleanSpec(f.fields, item ?? {})).filter((o) => Object.keys(o).length > 0) : undefined;
        if (Array.isArray(next) && next.length === 0 && f.key !== 'sources') next = undefined;
        break;
      case 'object':
        next = v && typeof v === 'object' ? cleanSpec(f.fields, v as Obj) : undefined;
        if (next && Object.keys(next as Obj).length === 0) next = undefined;
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
    <form
      className="k-form"
      noValidate
      onSubmit={(e) => {
        e.preventDefault();
        if (!readOnly) void submit();
      }}
    >
      {readOnly && own ? <Banner status="info" title={`Read-only: managed by ${own.owner}`} description={readOnlyReason(own)} /> : null}
      <div className="k-form-grid">
        <TextInput
          label="Name"
          value={name}
          onChange={setName}
          isReadOnly={!!initial}
          isRequired
          status={errors['metadata.name'] ? { type: 'error', message: errors['metadata.name'] } : undefined}
          description={initial ? undefined : 'Lower-case letters, digits and "-"'}
        />
        {schema.namespaced ? <TextInput label="Namespace" value={namespace} onChange={setNamespace} isReadOnly={!!initial} /> : null}
      </div>
      <TextInput label="Comment" value={comment} onChange={setComment} isDisabled={readOnly} isOptional />
      <Switch label="Enabled" value={enabled} onChange={setEnabled} isDisabled={readOnly} description="Disabled objects stay stored but take no effect." />
      <Fields fields={schema.fields} value={spec} onChange={setSpec} errors={errors} disabled={readOnly} />
      {serverError ? (
        <Banner status="error" title="The server rejected the change" description={serverError} />
      ) : null}
      {Object.keys(errors).length > 0 ? (
        <p className="k-error-text" role="alert">
          Fix the {Object.keys(errors).length} highlighted field{Object.keys(errors).length === 1 ? '' : 's'}.
        </p>
      ) : null}
      <div className="k-row">
        <span className="k-spacer" />
        <Button label={readOnly ? 'Close' : 'Cancel'} variant="ghost" onClick={onCancel} />
        {readOnly ? null : <Button type="submit" label={initial ? 'Save' : `Create ${schema.singular}`} variant="primary" isLoading={busy} />}
      </div>
    </form>
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

function statusOf(errors: FieldErrors, path: string) {
  const m = errors[path];
  return m ? { type: 'error' as const, message: m } : undefined;
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
  const id = useId();
  switch (def.type) {
    case 'text':
    case 'duration':
      return (
        <TextInput
          label={def.label}
          description={def.help}
          placeholder={def.placeholder}
          value={typeof value === 'string' ? value : ''}
          onChange={(v) => onChange(v)}
          isDisabled={disabled}
          isRequired={def.type === 'text' && def.required}
          status={statusOf(errors, path)}
        />
      );
    case 'number':
      return (
        <TextInput
          label={def.label}
          description={def.help}
          value={typeof value === 'number' ? String(value) : typeof value === 'string' ? value : ''}
          onChange={(v) => onChange(v.trim() === '' ? undefined : /^-?\d+$/.test(v.trim()) ? Number(v) : v)}
          isDisabled={disabled}
          status={statusOf(errors, path)}
        />
      );
    case 'bool':
      return <Switch label={def.label} description={def.help} value={value === true} onChange={(v) => onChange(v)} isDisabled={disabled} />;
    case 'enum':
      return (
        <label className="k-label" htmlFor={id}>
          {def.label}
          <select id={id} className="k-native-select" value={typeof value === 'string' ? value : ''} disabled={disabled} onChange={(e) => onChange(e.target.value)}>
            {!def.required && !def.options.some((o) => o.value === '') ? <option value="">Not set</option> : null}
            {def.options.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
          {def.help ? <span className="k-muted k-small">{def.help}</span> : null}
          {errors[path] ? <span className="k-error-text">{errors[path]}</span> : null}
        </label>
      );
    case 'strings':
      return (
        <TextArea
          label={def.label}
          description={def.help}
          placeholder={def.placeholder}
          rows={4}
          value={Array.isArray(value) ? (value as string[]).join('\n') : ''}
          onChange={(v) => onChange(v.split(/\r?\n/))}
          isDisabled={disabled}
          status={statusOf(errors, path)}
          hasSpellCheck={false}
        />
      );
    case 'refs':
      return <RefsField def={def} value={Array.isArray(value) ? (value as string[]) : []} onChange={onChange} disabled={disabled} />;
    case 'object': {
      const obj = value && typeof value === 'object' ? (value as Obj) : {};
      return (
        <fieldset className="k-fieldset">
          <legend>{def.label}</legend>
          {def.help ? <p className="k-muted k-small">{def.help}</p> : null}
          <div className="k-form-grid">
            <Fields fields={def.fields} value={obj} onChange={(v) => onChange(v)} errors={errors} disabled={disabled} prefix={path} />
          </div>
        </fieldset>
      );
    }
    case 'objects':
      return <ObjectsField def={def} path={path} value={Array.isArray(value) ? (value as Obj[]) : []} onChange={onChange} errors={errors} disabled={disabled} />;
  }
}

function RefsField({ def, value, onChange, disabled }: { def: Extract<FieldDef, { type: 'refs' }>; value: string[]; onChange: (v: unknown) => void; disabled: boolean }) {
  const api = useApi();
  const q = useQuery({ queryKey: qk.objects(def.refKind), queryFn: () => api.objects.list(def.refKind) });
  const names = (q.data?.items ?? []).filter((o) => !def.filter || def.filter(o)).map((o) => o.metadata.name);
  for (const v of value) if (!names.includes(v)) names.push(v);
  return (
    <fieldset className="k-fieldset">
      <legend>{def.label}</legend>
      {def.help ? <p className="k-muted k-small">{def.help}</p> : null}
      {names.length === 0 ? <p className="k-muted k-small">{q.isLoading ? 'Loading…' : 'Nothing to choose yet.'}</p> : null}
      <div className="k-check-grid">
        {names.map((n) => (
          <CheckboxInput
            key={n}
            label={n}
            value={value.includes(n)}
            isDisabled={disabled}
            onChange={(on) => onChange(on ? [...value, n] : value.filter((x) => x !== n))}
          />
        ))}
      </div>
    </fieldset>
  );
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
    // Multi-add: one item per line, into the first text field (IP for
    // clients) or into "cidr" when the line has a prefix length.
    const lines = bulk.split(/[\r\n,]+/).map((s) => s.trim()).filter(Boolean);
    const items = lines.map((l) => {
      if (def.key === 'clients') {
        if (l.includes('/')) return { cidr: l };
        if (/^([0-9a-f]{2}[:-]){5}[0-9a-f]{2}$/i.test(l)) return { mac: l };
        return { ip: l };
      }
      return { [firstText?.key ?? 'name']: l };
    });
    onChange([...value, ...items]);
    setBulk('');
    setShowBulk(false);
  };
  return (
    <fieldset className="k-fieldset">
      <legend>{def.label}</legend>
      {def.help ? <p className="k-muted k-small">{def.help}</p> : null}
      {value.length === 0 ? <p className="k-muted k-small">No {def.itemLabel}s yet.</p> : null}
      {value.map((item, i) => (
        <details key={i} className="k-array-entry" open={value.length <= 3 || Object.keys(errors).some((k) => k.startsWith(`${path}.${i}.`))}>
          <summary>
            <span className="k-mono">{def.summary ? def.summary(item ?? {}) : `${def.itemLabel} ${i + 1}`}</span>
          </summary>
          <div className="k-form-grid">
            <Fields fields={def.fields} value={item ?? {}} onChange={(v) => onChange(value.map((x, j) => (j === i ? v : x)))} errors={errors} disabled={disabled} prefix={`${path}.${i}`} />
          </div>
          {disabled ? null : (
            <Button label={`Remove ${def.itemLabel}`} variant="ghost" size="sm" onClick={() => onChange(value.filter((_, j) => j !== i))} />
          )}
        </details>
      ))}
      {disabled ? null : (
        <div className="k-row">
          <Button label={`Add ${def.itemLabel}`} variant="secondary" size="sm" onClick={() => onChange([...value, {}])} />
          <Button label="Add many" variant="ghost" size="sm" onClick={() => setShowBulk((s) => !s)} />
        </div>
      )}
      {showBulk ? (
        <div className="k-stack">
          <TextArea label={`Add many ${def.itemLabel}s`} description="One per line or comma separated" value={bulk} onChange={setBulk} rows={4} hasSpellCheck={false} />
          <div className="k-row">
            <Button label={`Add ${bulk.split(/[\r\n,]+/).filter((s) => s.trim()).length || ''} ${def.itemLabel}s`.replace('  ', ' ')} size="sm" variant="primary" onClick={addMany} />
          </div>
        </div>
      ) : null}
    </fieldset>
  );
}
