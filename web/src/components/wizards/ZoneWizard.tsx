// New zone, in a WizardDialog over the Zones page (?new=1; docs/m3/ui.md
// "Pages, wizards and dialogs"): type (primary or forward, with what each
// does) → apex → the type's settings (primary: name servers, default TTL,
// SOA timers, who may query; forward: the upstream group) → review → PUT
// /objects/dnszones/{name}, then the dialog closes and the zone's page
// opens. Every value is a typed control; empty optional values keep the
// CRD defaults and are left out of the object.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Text } from '@astryxdesign/core/Text';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { qk, useApi, useCan } from '../../api/context';
import type { AccessMode, DNSZone, DNSZoneSpec } from '../../api/types';
import { errorText, useNotify } from '../Feedback';
import { DialogForm } from '../FormDialog';
import { ChoiceCards } from '../fields/ChoiceCards';
import { secondsLabel } from '../fields/DurationField';
import { EnumSelect } from '../fields/EnumSelect';
import { formatError } from '../fields/formats';
import { SecondsField, secondsError } from '../fields/SecondsField';
import { cleanList, listError, StringListField } from '../fields/StringListField';
import { UpstreamGroupSelect } from '../UpstreamGroupSelect';
import { Wizard, WizardDialog, type WizardStep } from '../Wizard';
import { useNavigate } from '../../lib/router';
import { hostError } from '../../lib/rr';
import { DEFAULT_GROUP } from '../../lib/upstreams';
import { aclEntryError, apexError, QUERY_ACCESS, rnameOf, SOA_DEFAULTS, SOA_PRESETS, usesACL, zoneObjectName } from '../../lib/zones';

type Kind = 'primary' | 'forward';

interface Draft {
  type: Kind;
  apex: string;
  nameservers: string[];
  defaultTTL: string;
  mname: string;
  rname: string;
  refresh: string;
  retry: string;
  expire: string;
  minimum: string;
  queryAccess: AccessMode;
  queryACL: string[];
  upstreamGroup: string;
  dnssec: '' | 'on' | 'off';
}

const EMPTY: Draft = {
  type: 'primary',
  apex: '',
  nameservers: [],
  defaultTTL: '',
  mname: '',
  rname: '',
  refresh: '',
  retry: '',
  expire: '',
  minimum: '',
  queryAccess: 'allow',
  queryACL: [],
  upstreamGroup: '',
  dnssec: '',
};

const DNSSEC = [
  { value: '', label: 'Default', help: 'As the upstream group does.' },
  { value: 'on', label: 'Validate', help: 'Answers that fail DNSSEC validation become SERVFAIL.' },
  { value: 'off', label: 'Do not validate' },
];

/** The DNSZone object of a finished draft (exported for tests). */
export function zoneFromDraft(d: Draft): DNSZone {
  const name = zoneObjectName(d.apex);
  const num = (v: string) => (v.trim() ? Number(v) : undefined);
  const spec: DNSZoneSpec = { zoneName: `${name}.`, type: d.type };
  if (d.type === 'primary') {
    const ns = cleanList(d.nameservers);
    if (ns.length) spec.nameservers = ns;
    if (num(d.defaultTTL) !== undefined) spec.defaultTTL = num(d.defaultTTL);
    const soa = Object.fromEntries(
      Object.entries({
        mname: d.mname.trim() || undefined,
        rname: d.rname.trim() ? rnameOf(d.rname) : undefined,
        refresh: num(d.refresh),
        retry: num(d.retry),
        expire: num(d.expire),
        minimum: num(d.minimum),
      }).filter(([, v]) => v !== undefined),
    );
    if (Object.keys(soa).length) spec.soa = soa;
    if (d.queryAccess !== 'allow' || cleanList(d.queryACL).length) {
      spec.policy = { queryAccess: d.queryAccess, ...(usesACL(d.queryAccess) ? { queryACL: cleanList(d.queryACL) } : {}) };
    }
  } else {
    spec.forward = {
      ...(d.upstreamGroup ? { upstreamGroup: d.upstreamGroup } : {}),
      ...(d.dnssec ? { dnssecValidation: d.dnssec === 'on' } : {}),
    };
  }
  return { apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSZone', metadata: { name }, spec };
}

const cidr = (v: string) => formatError('cidr', v);

export interface ZoneWizardProps {
  /** Closes the dialog without asking (after the zone was created). */
  onClose: () => void;
}

export function ZoneWizardDialog({ isOpen, onClose }: ZoneWizardProps & { isOpen: boolean }) {
  const can = useCan();
  return (
    <WizardDialog
      isOpen={isOpen}
      onClose={onClose}
      title="New zone"
      subtitle="To bring records from another server, use Import zone file on the Zones page instead."
      what="zone"
    >
      {can('zones', 'write') ? (
        <ZoneWizard onClose={onClose} />
      ) : (
        <DialogForm footer={<Button label="Close" variant="secondary" onClick={onClose} />}>
          <Banner status="info" title="Not permitted" description="Creating zones needs the zones.write permission." />
        </DialogForm>
      )}
    </WizardDialog>
  );
}

function ZoneWizard({ onClose }: ZoneWizardProps) {
  const api = useApi();
  const qc = useQueryClient();
  const notify = useNotify();
  const navigate = useNavigate();
  const [d, setD] = useState<Draft>(EMPTY);
  const set = <K extends keyof Draft>(k: K, v: Draft[K]) => setD((x) => ({ ...x, [k]: v }));
  const zones = useQuery({ queryKey: qk.zones, queryFn: () => api.zones.list() });
  // Object names and apexes: a second zone for the same apex is refused too.
  const existing = (zones.data?.items ?? []).flatMap((z) => [z.name, z.zoneName.replace(/\.$/, '').toLowerCase()]);
  const name = zoneObjectName(d.apex);
  const create = useMutation({
    mutationFn: () => api.objects.put('dnszones', zoneFromDraft(d)),
    onSuccess: () => {
      notify.success(`Created zone ${name}`);
      void qc.invalidateQueries({ queryKey: qk.zones });
      onClose();
      navigate(`/zones/${encodeURIComponent(name)}`);
    },
  });

  const secs = (k: 'refresh' | 'retry' | 'expire' | 'minimum' | 'defaultTTL', label: string, description?: string) => (
    <SecondsField
      label={label}
      description={description}
      value={d[k]}
      onChange={(v) => set(k, v)}
      presets={[...SOA_PRESETS[k]]}
      emptyLabel={`Default (${secondsLabel(SOA_DEFAULTS[k])})`}
      error={secondsError(d[k], { optional: true }) ?? undefined}
    />
  );

  const steps: WizardStep[] = [
    {
      id: 'type',
      label: 'Type',
      description: 'What kudurru does with queries for names in the zone.',
      content: (
        <>
          <ChoiceCards
            label="Zone type"
            value={d.type}
            onChange={(v) => set('type', v as Kind)}
            choices={[
              {
                value: 'primary',
                label: 'Primary',
                description: 'kudurru is authoritative: you keep the records here (home.arpa, lab.example) and it answers from them, NOTIFYing secondaries.',
              },
              {
                value: 'forward',
                label: 'Forward',
                description: 'Queries for the zone go to an upstream group, e.g. a corporate or Active Directory DNS for corp.example.',
              },
            ]}
          />
          <p className="k-muted k-small">Secondary, stub and catalog zones are declared in YAML (`kudurru apply`) in this release.</p>
        </>
      ),
    },
    {
      id: 'apex',
      label: 'Apex',
      description: 'The name the zone starts at; every name below it belongs to the zone.',
      validate: () => apexError(d.apex, existing),
      content: (
        <>
          <TextInput
            label="Zone name"
            placeholder={d.type === 'primary' ? 'home.arpa' : 'corp.example'}
            description="A domain name such as home.arpa or lab.example.com; a trailing dot is optional"
            value={d.apex}
            onChange={(v) => set('apex', v)}
            isRequired
            status={d.apex.trim() && apexError(d.apex, existing) ? { type: 'error', message: apexError(d.apex, existing) ?? '' } : undefined}
          />
          {name && !apexError(d.apex, existing) ? (
            <p className="k-small k-muted">
              Object <code>dnszones/{name}</code>, zone <code>{name}.</code>
            </p>
          ) : null}
        </>
      ),
    },
    d.type === 'primary'
      ? {
          id: 'primary',
          label: 'Records and access',
          description: 'Everything here has a sensible default; change only what you need.',
          validate: () =>
            listError(d.nameservers, (s) => hostError(s)) ??
            (d.mname.trim() ? hostError(d.mname) && `Primary name server: ${hostError(d.mname)}` : null) ??
            (d.rname.trim() && !d.rname.includes('@') && hostError(d.rname) ? `Responsible mailbox: ${hostError(d.rname)}` : null) ??
            (['defaultTTL', 'refresh', 'retry', 'expire', 'minimum'] as const).map((k) => secondsError(d[k], { optional: true })).find(Boolean) ??
            (usesACL(d.queryAccess) && !cleanList(d.queryACL).length ? 'The ACL needs at least one entry for this access mode' : null) ??
            (usesACL(d.queryAccess) ? listError(d.queryACL, (s) => aclEntryError(s, cidr)) : null),
          content: (
            <>
              <StringListField
                label="Name servers"
                description="The zone's NS records; none uses this server's name"
                value={d.nameservers}
                onChange={(v) => set('nameservers', v)}
                placeholder="ns1.example.com"
                itemError={(s) => hostError(s)}
                addLabel="Add name server"
              />
              <div className="k-form-grid">{secs('defaultTTL', 'Default TTL', 'Records without their own TTL')}</div>
              <fieldset className="k-fieldset">
                <legend>SOA</legend>
                <div className="k-form-grid">
                  <TextInput
                    label="Primary name server"
                    isOptional
                    placeholder="the first name server"
                    value={d.mname}
                    onChange={(v) => set('mname', v)}
                    status={d.mname.trim() && hostError(d.mname) ? { type: 'error', message: hostError(d.mname) ?? '' } : undefined}
                  />
                  <TextInput
                    label="Responsible mailbox"
                    isOptional
                    placeholder="hostmaster@example.com"
                    description="An e-mail address; stored in SOA form"
                    value={d.rname}
                    onChange={(v) => set('rname', v)}
                  />
                  {secs('refresh', 'Refresh', 'How often secondaries check the serial')}
                  {secs('retry', 'Retry', 'After a failed refresh')}
                  {secs('expire', 'Expire', 'Secondaries stop answering after this without the primary')}
                  {secs('minimum', 'Negative TTL', 'How long resolvers cache "no such name"')}
                </div>
              </fieldset>
              <EnumSelect label="Who may query" value={d.queryAccess} options={QUERY_ACCESS} onChange={(v) => set('queryAccess', v as AccessMode)} />
              {usesACL(d.queryAccess) ? (
                <StringListField
                  label="Query ACL"
                  description='Addresses or CIDRs in order, first match wins; "!" in front denies (!192.168.1.66)'
                  value={d.queryACL}
                  onChange={(v) => set('queryACL', v)}
                  placeholder="192.168.1.0/24"
                  itemError={(s) => aclEntryError(s, cidr)}
                  addLabel="Add entry"
                />
              ) : null}
            </>
          ),
        }
      : {
          id: 'forward',
          label: 'Upstream',
          description: 'Where queries for the zone are sent. Answers are cached like any forwarded answer.',
          content: (
            <>
              <UpstreamGroupSelect value={d.upstreamGroup} onChange={(v) => set('upstreamGroup', v)} help="The servers of this group answer for the zone." />
              <Text type="supporting" as="p">
                Need a group for this zone? Create one on the Upstreams page first; it takes effect without a restart.
              </Text>
              <EnumSelect label="DNSSEC validation" value={d.dnssec} options={DNSSEC} onChange={(v) => set('dnssec', v as Draft['dnssec'])} />
            </>
          ),
        },
    {
      id: 'review',
      label: 'Review',
      content: <Review d={d} />,
    },
  ];

  return (
    <Wizard
      label="New zone"
      steps={steps}
      finishLabel="Create zone"
      isFinishing={create.isPending}
      onFinish={() => create.mutate()}
      isDirty={d !== EMPTY}
      error={create.error ? <Banner status="error" title="The zone could not be created" description={errorText(create.error)} /> : null}
    />
  );
}

function Review({ d }: { d: Draft }) {
  const z = zoneFromDraft(d);
  const s = z.spec;
  const dflt = (v: number | undefined, def: number) => (v === undefined ? `default (${secondsLabel(def)})` : `${secondsLabel(v)} (${v})`);
  return (
    <dl className="k-review" data-testid="zone-review">
      <dt>Zone</dt>
      <dd className="k-mono">{s.zoneName}</dd>
      <dt>Type</dt>
      <dd>{s.type === 'primary' ? 'Primary (records served here)' : 'Forward (to an upstream group)'}</dd>
      {s.type === 'primary' ? (
        <>
          <dt>Name servers</dt>
          <dd>{s.nameservers?.join(', ') ?? 'this server'}</dd>
          <dt>Default TTL</dt>
          <dd>{dflt(s.defaultTTL, SOA_DEFAULTS.defaultTTL)}</dd>
          <dt>SOA</dt>
          <dd>
            refresh {dflt(s.soa?.refresh, SOA_DEFAULTS.refresh)}, retry {dflt(s.soa?.retry, SOA_DEFAULTS.retry)}, expire {dflt(s.soa?.expire, SOA_DEFAULTS.expire)},
            negative TTL {dflt(s.soa?.minimum, SOA_DEFAULTS.minimum)}
            {s.soa?.rname ? `, mailbox ${s.soa.rname}` : ''}
          </dd>
          <dt>Who may query</dt>
          <dd>
            {QUERY_ACCESS.find((q) => q.value === (s.policy?.queryAccess ?? 'allow'))?.label}
            {s.policy?.queryACL?.length ? `: ${s.policy.queryACL.join(', ')}` : ''}
          </dd>
        </>
      ) : (
        <>
          <dt>Upstream group</dt>
          <dd className="k-mono">{s.forward?.upstreamGroup ?? `${DEFAULT_GROUP} (not set)`}</dd>
          <dt>DNSSEC validation</dt>
          <dd>{DNSSEC.find((x) => x.value === d.dnssec)?.label}</dd>
        </>
      )}
    </dl>
  );
}
