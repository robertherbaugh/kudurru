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
import { useState } from 'react';
import type { Endpoint, ZoneRecord } from '../api/types';
import { compose, dnsNameToOwner, ownerError, ownerToDNSName, parse, RECORD_TYPES, typeSpec, validate, type Errors, type Values } from '../lib/rr';
import { errorText } from './Feedback';
import { EnumSelect, type EnumOption } from './fields/EnumSelect';
import { SecondsField } from './fields/SecondsField';
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

/** What each record type is for, under the Type select. */
const TYPE_HELP: Record<string, string> = {
  A: 'Points a name at an IPv4 address.',
  AAAA: 'Points a name at an IPv6 address.',
  CNAME: 'Makes the name an alias of another; it cannot have other records.',
  MX: 'Where mail for the name is delivered, lowest preference first.',
  SRV: 'Where a service (_sip._udp, _ldap._tcp) runs: host, port, priority and weight.',
  TXT: 'Free text: SPF, DKIM, site verification.',
  CAA: 'Which certificate authorities may issue for the name.',
  HTTPS: 'Connection hints for browsers (ALPN, ports, ECH).',
  SVCB: 'Service binding for other protocols (DoT, DoH discovery).',
  NS: 'Delegates the name to other name servers.',
  PTR: 'Reverse lookup: the name of an address.',
  TLSA: 'DANE: the certificate a TLS service must present.',
  SSHFP: 'SSH host key fingerprints.',
  NAPTR: 'Rewrite rules (ENUM, SIP).',
  raw: 'Any other type as presentation-format rdata.',
};

const TYPE_OPTIONS: EnumOption[] = RECORD_TYPES.map((t) => ({ value: t.type, label: t.label, help: TYPE_HELP[t.type] }));

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
        <EnumSelect
          label="Type"
          value={draft.type}
          options={TYPE_OPTIONS}
          isDisabled={!!initial}
          onChange={(v) => {
            setDraft({ type: v, targets: [{}] });
            setErrors(null);
          }}
        />
        <SecondsField label="TTL" value={ttl} onChange={setTTL} emptyLabel="Zone default" error={shown.ttl} />
      </div>

      {draft.targets.map((t, i) => (
        <fieldset key={i} className="k-fieldset" aria-label={`Target ${i + 1}`}>
          <legend>
            {spec.single ? 'Target' : `Target ${i + 1}`}
          </legend>
          <div className="k-form-grid">
            {spec.fields.map((f) =>
              f.kind === 'enum' && f.options && f.options.length > 0 ? (
                <EnumSelect
                  key={f.key}
                  label={f.label}
                  options={f.options.map((o) => ({ value: o, label: o }))}
                  emptyLabel="Choose…"
                  value={t[f.key] ?? ''}
                  error={shown.targets[i]?.[f.key]}
                  onChange={(v) => setTarget(i, f.key, v)}
                />
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

