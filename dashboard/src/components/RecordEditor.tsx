// The typed per-RR record editor (§10 principle 11): A, AAAA, CNAME, MX,
// SRV, TXT, CAA, HTTPS, SVCB, NS, PTR, TLSA, SSHFP, NAPTR and raw rdata.
// Each target is edited field by field (lib/rr), validated before
// submitting, and composed into the presentation-format target an
// external-dns Endpoint carries. The server's 400 is shown verbatim when
// it disagrees. "Served to" picks the endpoint's view (D403): the default
// data every client gets, public (published to sinks, never served), or a
// ClientGroup of the record set's namespace, with what the server refuses
// or bypasses for a group view said next to it.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { FieldStatus } from '@astryxdesign/core/FieldStatus';
import { Grid } from '@astryxdesign/core/Grid';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useState } from 'react';
import type { ClientGroup, Endpoint, ZoneRecord } from '../api/types';
import { compose, dnsNameToOwner, ownerError, ownerToDNSName, parse, RECORD_TYPES, typeSpec, validate, type Errors, type Values } from '../lib/rr';
import { errorText } from './Feedback';
import { EnumSelect, type EnumOption } from './fields/EnumSelect';
import { SecondsField } from './fields/SecondsField';
import { SelectField, type SelectOption } from './fields/SelectField';
import { SwitchField, TextField } from './fields/TextField';
import { DialogForm } from './FormDialog';

export interface RecordEditorProps {
  /** The zone's DNS name ("example.internal" or "example.internal."). */
  zoneName: string;
  initial?: ZoneRecord;
  /** Record set to write into (POST /zones/{zone}/records?recordSet=). */
  defaultRecordSet: string;
  onSubmit: (ep: Endpoint, recordSet: string) => Promise<void>;
  onCancel: () => void;
  /** The ClientGroups the session can read, for "Served to" (D403). */
  groups?: ClientGroup[] | undefined;
  /** The zone is DNSSEC-signed (spec.dnssec.enabled or signed records): group views are not served. */
  signed?: boolean | undefined;
  /**
   * The record list shows what this node serves (the complete SQLite
   * runtime): a public record, or a group view the server refuses, is
   * stored but never listed, so the editor says so before saving.
   */
  listsServed?: boolean | undefined;
}

/** "Served to" for the default data (no kudurru.view). */
export const VIEW_DEFAULT = '';
/** kudurru.view "public": published to integration sinks, never answered (D29). */
export const VIEW_PUBLIC = 'public';

/** Types a client-group view cannot hold (internal/zone, D403): leaf data only. */
const VIEW_REFUSED_TYPES = new Set(['NS', 'DNAME', 'SOA', 'DS', 'DNSKEY', 'CDS', 'CDNSKEY', 'RRSIG', 'NSEC', 'NSEC3', 'NSEC3PARAM']);

/** The namespace a record set reference ("ns/name") names; undefined for a bare name (the server's default). */
export function recordSetNamespace(ref: string): string | undefined {
  const i = ref.indexOf('/');
  return i > 0 ? ref.slice(0, i) : undefined;
}

/**
 * The "Served to" options: the default data, public, and each ClientGroup a
 * view may name, which are those of the record set's namespace (the server
 * resolves kudurru.view there, as Policy clientGroupRefs). For a bare record
 * set name (ns undefined) every namespace is offered, and choosing a group of another
 * namespace stores the record in that namespace (recordSetFor).
 */
export function viewOptions(groups: readonly ClientGroup[], ns: string | undefined, current: string): SelectOption[] {
  const out: SelectOption[] = [
    { value: VIEW_DEFAULT, label: 'Every client (default)', help: 'The default data: every client gets it unless its group has a view of this name and type.' },
    { value: VIEW_PUBLIC, label: 'Published only (public)', help: 'Published to integration sinks such as Cloudflare, never answered by this server.' },
  ];
  const seen = new Set<string>();
  for (const g of [...groups].sort((a, b) => (a.metadata.namespace ?? 'default').localeCompare(b.metadata.namespace ?? 'default') || a.metadata.name.localeCompare(b.metadata.name))) {
    const gns = g.metadata.namespace ?? 'default';
    if (ns !== undefined && gns !== ns) continue;
    const value = ns === undefined ? `${gns}/${g.metadata.name}` : g.metadata.name;
    seen.add(value);
    out.push({ value, label: `Client group ${gns === 'default' ? g.metadata.name : `${gns}/${g.metadata.name}`}`, help: `Served only to clients the identify stage places in ${g.metadata.name}; they get it instead of the default data of this name and type.` });
  }
  if (current && current !== VIEW_PUBLIC && !seen.has(current)) out.push({ value: current, label: `Client group ${current}`, help: 'No ClientGroup of this name is listed in the record set’s namespace; the view serves nobody until one exists.' });
  return out;
}

/** The kudurru.view to send for a "Served to" value ("ns/name" options carry the group's namespace). */
export function viewName(value: string): string {
  const i = value.indexOf('/');
  return i >= 0 ? value.slice(i + 1) : value;
}

/** The record set to write: a bare name moves into the chosen group's namespace. */
export function recordSetFor(recordSet: string, value: string): string {
  const i = value.indexOf('/');
  if (i < 0 || recordSetNamespace(recordSet) !== undefined) return recordSet;
  return `${value.slice(0, i)}/${recordSet}`;
}

/** The namespace of a "Served to" value: "ns/name" names it, a bare group name is the record set's (undefined when that is bare too). */
function viewNamespace(value: string, recordSet: string): string | undefined {
  const i = value.indexOf('/');
  return i >= 0 ? value.slice(0, i) : recordSetNamespace(recordSet);
}

/**
 * The "Served to" value after the record set changes from `from` to `to`, so
 * the view keeps naming the same ClientGroup: a group chosen while the record
 * set was bare ("ns/name") becomes its bare name once the record set names
 * that namespace, and a bare group name gains its namespace when the record
 * set loses it. A group of another namespace than the new record set's is
 * kept as it is; validation refuses it (viewNamespaceError).
 */
export function remapView(value: string, from: string, to: string): string {
  if (!isGroupView(value)) return value;
  const gns = viewNamespace(value, from);
  if (gns === undefined) return value;
  const ns = recordSetNamespace(to);
  if (ns === undefined) return `${gns}/${viewName(value)}`;
  return ns === gns ? viewName(value) : value;
}

/** Why the view's ClientGroup cannot serve this record set (another namespace), or undefined. */
export function viewNamespaceError(value: string, recordSet: string): string | undefined {
  if (!isGroupView(value) || !value.includes('/')) return undefined;
  const gns = value.slice(0, value.indexOf('/'));
  const ns = recordSetNamespace(recordSet);
  if (ns === undefined || ns === gns) return undefined;
  return `Client group ${value} is in namespace ${gns}, but record set ${recordSet} is in ${ns}: a view names a group of its record set’s namespace. Choose a group of ${ns}, or a record set in ${gns}.`;
}

/** Whether the value names a client group (not the default data or public). */
export function isGroupView(value: string): boolean {
  return value !== VIEW_DEFAULT && value !== VIEW_PUBLIC;
}

/** Why a client-group view cannot hold this record, or undefined. */
export function viewError(value: string, recordType: string, owner: string): string | undefined {
  if (!isGroupView(value)) return undefined;
  const t = recordType.toUpperCase();
  if (VIEW_REFUSED_TYPES.has(t)) return `A client-group view holds leaf data only; ${t} records stay in the default data`;
  if (t === 'CNAME' && (owner.trim() === '@' || owner.trim() === '')) return 'A client-group view cannot hold a CNAME at the zone apex';
  return undefined;
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

/** Fields side by side while each keeps 220 px (structural width). */
const FORM_GRID = { minWidth: 220, repeat: 'fit' } as const;

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
  view?: string;
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
  return !!e.owner || !!e.ttl || !!e.view || e.targets.some((t) => Object.keys(t).length > 0);
}

export function RecordEditor({ zoneName, initial, defaultRecordSet, onSubmit, onCancel, groups = [], signed = false, listsServed = false }: RecordEditorProps) {
  const [owner, setOwner] = useState(initial ? dnsNameToOwner(initial.dnsName, zoneName) : '');
  const [ttl, setTTL] = useState(initial?.recordTTL !== undefined ? String(initial.recordTTL) : '');
  const [draft, setDraft] = useState<Draft>(() => draftFrom(initial));
  const [comment, setComment] = useState(initial?.kudurru?.comment ?? '');
  const [disabled, setDisabled] = useState(initial?.kudurru?.disabled ?? false);
  const [recordSet, setRecordSet] = useState(initial?.recordSet ?? defaultRecordSet);
  const [view, setView] = useState(initial?.kudurru?.view ?? VIEW_DEFAULT);
  const [errors, setErrors] = useState<EditorErrors | null>(null);
  const [serverError, setServerError] = useState<string | undefined>();
  const [busy, setBusy] = useState(false);
  const spec = typeSpec(draft.type);
  const shown = errors ?? { targets: [] };

  const setTarget = (i: number, key: string, v: string) =>
    setDraft((d) => ({ ...d, targets: d.targets.map((t, j) => (j === i ? { ...t, [key]: v } : t)) }));

  const submit = async () => {
    const e = validateRecord(zoneName, owner, ttl, draft);
    const composed = draft.targets.map((t) => compose(draft.type, t));
    const ve = viewError(view, composed[0]?.recordType ?? draft.type, owner) ?? viewNamespaceError(view, recordSet.trim() || defaultRecordSet);
    if (ve) e.view = ve;
    setErrors(e);
    if (hasErrors(e)) return;
    const ext = { ...(comment.trim() ? { comment: comment.trim() } : {}), ...(disabled ? { disabled: true } : {}), ...(view ? { view: viewName(view) } : {}) };
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
      await onSubmit(ep, recordSetFor(recordSet.trim() || defaultRecordSet, view));
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
      aria-label="Record"
      onSubmit={() => void submit()}
      footer={
        <>
          <Button label="Cancel" variant="ghost" onClick={onCancel} />
          <Button type="submit" label={initial ? 'Save record' : 'Add record'} variant="primary" isLoading={busy} />
        </>
      }
    >
      <Grid columns={FORM_GRID} gap={3} align="start">
        <TextField
          label="Name"
          help={`Relative to ${zoneName.replace(/\.$/, '')}; @ is the apex, a trailing dot makes it absolute`}
          placeholder="www"
          value={owner}
          onChange={setOwner}
          isReadOnly={!!initial}
          error={shown.owner}
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
      </Grid>

      {draft.targets.map((t, i) => (
        <Card key={i} padding={3} role="group" aria-label={`Target ${i + 1}`}>
          <VStack gap={2}>
            <Text type="label" weight="semibold">
              {spec.single ? 'Target' : `Target ${i + 1}`}
            </Text>
            <Grid columns={FORM_GRID} gap={3} align="start">
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
                  <TextField
                    key={f.key}
                    label={f.label}
                    help={f.help}
                    placeholder={f.placeholder}
                    value={t[f.key] ?? ''}
                    onChange={(v) => setTarget(i, f.key, v)}
                    isOptional={f.optional}
                    error={shown.targets[i]?.[f.key]}
                  />
                ),
              )}
            </Grid>
            {draft.targets.length > 1 ? (
              <HStack gap={2}>
                <Button label={`Remove target ${i + 1}`} size="sm" variant="ghost" onClick={() => setDraft((d) => ({ ...d, targets: d.targets.filter((_, j) => j !== i) }))} />
              </HStack>
            ) : null}
          </VStack>
        </Card>
      ))}
      {spec.single ? null : (
        <HStack gap={2}>
          <Button label="Add target" size="sm" variant="secondary" onClick={() => setDraft((d) => ({ ...d, targets: [...d.targets, {}] }))} />
        </HStack>
      )}

      <Grid columns={FORM_GRID} gap={3} align="start">
        <TextField label="Comment" value={comment} onChange={setComment} isOptional />
        <TextField
          label="Record set"
          help="The DNSRecordSet object this record is stored in"
          value={recordSet}
          onChange={(v) => {
            setView((cur) => remapView(cur, recordSet, v));
            setRecordSet(v);
          }}
          isReadOnly={!!initial}
        />
      </Grid>
      <SelectField
        label="Served to"
        value={view}
        options={viewOptions(groups, initial ? (initial.namespace ?? recordSetNamespace(initial.recordSet) ?? 'default') : recordSetNamespace(recordSet), view)}
        onChange={setView}
        isDisabled={!!initial}
        help={initial ? 'The view is part of the record’s identity; add the record again to serve it to another group.' : 'Who gets this record. A client-group view replaces the default data of the same name and type for that group only.'}
        error={shown.view}
      />
      {isGroupView(view) ? (
        <Banner
          status={signed ? 'warning' : 'info'}
          title={signed ? 'This zone is signed: client-group views are not served' : 'How a client-group view is served'}
          description={
            (signed ? 'Views are not served in a DNSSEC-signed zone: the server refuses the endpoint as Invalid and the group gets the default data. ' : 'Views are not served if the zone is DNSSEC-signed (the endpoint is refused as Invalid). ') +
            'Zone transfers (AXFR, IXFR, NOTIFY), the BIND export, RFC 2136 UPDATE and integration sinks carry the default data only, so a secondary never receives the view. ' +
            'While any client-group view exists the dnsdist edge bypasses its authoritative and resolver packet caches (zone-client-view), because those caches cannot tell groups apart. ' +
            'A zone serves at most 16 views, and a view may only replace default data of its own namespace.' +
            (listsServed ? ' This server lists only the records it serves, so a view it refuses is stored but does not appear in the record list.' : '')
          }
        />
      ) : null}
      {view === VIEW_PUBLIC && listsServed ? (
        <Banner
          status="warning"
          title="A public record is stored but not listed here"
          description="This server lists the records it serves, and a public record is never served: once saved it is published to integration sinks but does not appear in the record list, so this page cannot edit or delete it. The DNSRecordSet download under Tools shows it, and the records API removes it (DELETE /zones/{zone}/records/{name}/{type}?view=public)."
        />
      ) : null}
      <SwitchField label="Disabled" help="Keep the record but stop serving it" value={disabled} onChange={setDisabled} />

      {serverError ? <Banner status="error" title="The server rejected the record" description={serverError} /> : null}
      {errors && hasErrors(errors) ? <FieldStatus type="error" variant="detached" message="Fix the highlighted fields before saving." /> : null}
    </DialogForm>
  );
}

