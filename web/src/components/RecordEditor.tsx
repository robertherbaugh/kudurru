// The typed per-RR record editor (§10 principle 11): A, AAAA, CNAME, MX,
// SRV, TXT, CAA, HTTPS, SVCB, NS, PTR, TLSA, SSHFP, NAPTR and raw rdata.
// Each target is edited field by field (lib/rr), validated before
// submitting, and composed into the presentation-format target an
// external-dns Endpoint carries. The server's 400 is shown verbatim when
// it disagrees.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Switch } from '@astryxdesign/core/Switch';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useId, useState } from 'react';
import type { Endpoint, ZoneRecord } from '../api/types';
import { compose, dnsNameToOwner, ownerError, ownerToDNSName, parse, RECORD_TYPES, typeSpec, validate, type Errors, type Values } from '../lib/rr';
import { errorText } from './Feedback';
import { DialogForm } from './FormDialog';

export interface RecordEditorProps {
  /** The zone's DNS name ("example.internal" or "example.internal."). */
  zoneName: string;
  initial?: ZoneRecord;
  /** Record set to write into (POST /zones/{zone}/records?recordSet=). */
  defaultRecordSet: string;
  onSubmit: (ep: Endpoint, recordSet: string) => Promise<void>;
  onCancel: () => void;
}

interface Draft {
  type: string;
  targets: Values[];
}

/** Turns an existing record into editor state (raw when a target does not parse). */
export function draftFrom(rec: ZoneRecord | undefined): Draft {
  if (!rec) return { type: 'A', targets: [{}] };
  const parsed = rec.targets.map((t) => parse(rec.recordType, t));
  if (parsed.some((p) => p.type === 'raw')) {
    return { type: 'raw', targets: rec.targets.map((t) => ({ rrtype: rec.recordType, rdata: t })) };
  }
  return { type: parsed[0]?.type ?? rec.recordType, targets: parsed.map((p) => p.values) };
}

export interface EditorErrors {
  owner?: string;
  ttl?: string;
  targets: Errors[];
}

/** All validation of the editor in one place (exported for tests). */
export function validateRecord(zoneName: string, owner: string, ttl: string, d: Draft): EditorErrors {
  const out: EditorErrors = { targets: d.targets.map((t) => validate(d.type, t)) };
  const oe = ownerError(owner, zoneName);
  if (oe) out.owner = oe;
  if (ttl.trim() && (!/^\d+$/.test(ttl.trim()) || Number(ttl) > 2147483647)) out.ttl = 'Seconds, 0 to 2147483647';
  if (d.type === 'raw') {
    const types = new Set(d.targets.map((t) => (t.rrtype ?? '').trim().toUpperCase()));
    if (types.size > 1) out.targets = out.targets.map((e) => ({ ...e, rrtype: 'All targets of one record need the same type' }));
  }
  if (d.type === 'CNAME' && d.targets.length > 1) out.targets = out.targets.map((e, i) => (i > 0 ? { ...e, target: 'A CNAME has exactly one target' } : e));
  return out;
}

export function hasErrors(e: EditorErrors): boolean {
  return !!e.owner || !!e.ttl || e.targets.some((t) => Object.keys(t).length > 0);
}

export function RecordEditor({ zoneName, initial, defaultRecordSet, onSubmit, onCancel }: RecordEditorProps) {
  const typeId = useId();
  const [owner, setOwner] = useState(initial ? dnsNameToOwner(initial.dnsName, zoneName) : '');
  const [ttl, setTTL] = useState(initial?.recordTTL !== undefined ? String(initial.recordTTL) : '');
  const [draft, setDraft] = useState<Draft>(() => draftFrom(initial));
  const [comment, setComment] = useState(initial?.kudurru?.comment ?? '');
  const [disabled, setDisabled] = useState(initial?.kudurru?.disabled ?? false);
  const [recordSet, setRecordSet] = useState(initial?.recordSet ?? defaultRecordSet);
  const [errors, setErrors] = useState<EditorErrors | null>(null);
  const [serverError, setServerError] = useState<string | undefined>();
  const [busy, setBusy] = useState(false);
  const spec = typeSpec(draft.type);
  const shown = errors ?? { targets: [] };

  const setTarget = (i: number, key: string, v: string) =>
    setDraft((d) => ({ ...d, targets: d.targets.map((t, j) => (j === i ? { ...t, [key]: v } : t)) }));

  const submit = async () => {
    const e = validateRecord(zoneName, owner, ttl, draft);
    setErrors(e);
    if (hasErrors(e)) return;
    const composed = draft.targets.map((t) => compose(draft.type, t));
    const ext = { ...(comment.trim() ? { comment: comment.trim() } : {}), ...(disabled ? { disabled: true } : {}) };
    const ep: Endpoint = {
      dnsName: ownerToDNSName(owner, zoneName),
      recordType: composed[0]?.recordType ?? draft.type,
      targets: composed.map((c) => c.target),
      ...(ttl.trim() ? { recordTTL: Number(ttl) } : {}),
      ...(initial?.setIdentifier ? { setIdentifier: initial.setIdentifier } : {}),
      ...(Object.keys(ext).length ? { kudurru: ext } : {}),
    };
    setBusy(true);
    setServerError(undefined);
    try {
      await onSubmit(ep, recordSet.trim() || defaultRecordSet);
    } catch (err) {
      setServerError(errorText(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <DialogForm
      className="k-form"
      noValidate
      aria-label="Record"
      onSubmit={() => void submit()}
      footer={
        <>
          <Button label="Cancel" variant="ghost" onClick={onCancel} />
          <Button type="submit" label={initial ? 'Save record' : 'Add record'} variant="primary" isLoading={busy} />
        </>
      }
    >
      <div className="k-form-grid">
        <TextInput
          label="Name"
          description={`Relative to ${zoneName.replace(/\.$/, '')}; @ is the apex, a trailing dot makes it absolute`}
          placeholder="www"
          value={owner}
          onChange={setOwner}
          isReadOnly={!!initial}
          status={shown.owner ? { type: 'error', message: shown.owner } : undefined}
        />
        <label className="k-label" htmlFor={typeId}>
          Type
          <select
            id={typeId}
            className="k-native-select"
            value={draft.type}
            disabled={!!initial}
            onChange={(e) => {
              setDraft({ type: e.target.value, targets: [{}] });
              setErrors(null);
            }}
          >
            {RECORD_TYPES.map((t) => (
              <option key={t.type} value={t.type}>
                {t.label}
              </option>
            ))}
          </select>
        </label>
        <TextInput
          label="TTL (seconds)"
          placeholder="zone default"
          value={ttl}
          onChange={setTTL}
          isOptional
          status={shown.ttl ? { type: 'error', message: shown.ttl } : undefined}
        />
      </div>

      {draft.targets.map((t, i) => (
        <fieldset key={i} className="k-fieldset" aria-label={`Target ${i + 1}`}>
          <legend>
            {spec.single ? 'Target' : `Target ${i + 1}`}
          </legend>
          <div className="k-form-grid">
            {spec.fields.map((f) =>
              f.kind === 'enum' && f.options && f.options.length > 0 ? (
                <EnumField key={f.key} label={f.label} options={f.options} value={t[f.key] ?? ''} error={shown.targets[i]?.[f.key]} onChange={(v) => setTarget(i, f.key, v)} />
              ) : (
                <TextInput
                  key={f.key}
                  label={f.label}
                  description={f.help}
                  placeholder={f.placeholder}
                  value={t[f.key] ?? ''}
                  onChange={(v) => setTarget(i, f.key, v)}
                  isOptional={f.optional}
                  status={shown.targets[i]?.[f.key] ? { type: 'error', message: shown.targets[i]?.[f.key] as string } : undefined}
                />
              ),
            )}
          </div>
          {draft.targets.length > 1 ? (
            <Button label={`Remove target ${i + 1}`} size="sm" variant="ghost" onClick={() => setDraft((d) => ({ ...d, targets: d.targets.filter((_, j) => j !== i) }))} />
          ) : null}
        </fieldset>
      ))}
      {spec.single ? null : (
        <div>
          <Button label="Add target" size="sm" variant="secondary" onClick={() => setDraft((d) => ({ ...d, targets: [...d.targets, {}] }))} />
        </div>
      )}

      <div className="k-form-grid">
        <TextInput label="Comment" value={comment} onChange={setComment} isOptional />
        <TextInput label="Record set" description="The DNSRecordSet object this record is stored in" value={recordSet} onChange={setRecordSet} isReadOnly={!!initial} />
      </div>
      <Switch label="Disabled" description="Keep the record but stop serving it" value={disabled} onChange={setDisabled} />

      {serverError ? <Banner status="error" title="The server rejected the record" description={serverError} /> : null}
      {errors && hasErrors(errors) ? (
        <p className="k-error-text" role="alert">
          Fix the highlighted fields before saving.
        </p>
      ) : null}
    </DialogForm>
  );
}

function EnumField({ label, options, value, error, onChange }: { label: string; options: string[]; value: string; error?: string; onChange: (v: string) => void }) {
  const id = useId();
  return (
    <label className="k-label" htmlFor={id}>
      {label}
      <select id={id} className="k-native-select" value={value} onChange={(e) => onChange(e.target.value)} aria-invalid={!!error}>
        <option value="">Choose…</option>
        {options.map((o) => (
          <option key={o} value={o}>
            {o}
          </option>
        ))}
      </select>
      {error ? <span className="k-error-text">{error}</span> : null}
    </label>
  );
}
