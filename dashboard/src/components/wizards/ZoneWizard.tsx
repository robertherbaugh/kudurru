// New zone, in a WizardDialog over the Zones page (?new=1; dev-docs/milestones/m3/ui.md
// "Pages, wizards and dialogs"): type (primary, forward or secondary)
// does) → apex → the type's settings (primary: name servers, default TTL,
// SOA timers, who may query; forward: the upstream group) → review → PUT
// /objects/dnszones/{name}, then the dialog closes and the zone's page
// opens. Every value is a typed control; empty optional values keep the
// CRD defaults and are left out of the object.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { CheckboxInput } from '@astryxdesign/core/CheckboxInput';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { Grid } from '@astryxdesign/core/Grid';
import { Heading } from '@astryxdesign/core/Heading';
import { VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useId, useState } from 'react';
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
import { TextField } from '../fields/TextField';
import { UpstreamGroupSelect } from '../UpstreamGroupSelect';
import { Wizard, WizardDialog, type WizardStep } from '../Wizard';
import { useNavigate } from '../../lib/router';
import { hostError } from '../../lib/rr';
import { DEFAULT_GROUP } from '../../lib/upstreams';
import { aclEntryError, apexError, primaryEndpointError, QUERY_ACCESS, rnameOf, SOA_DEFAULTS, SOA_PRESETS, tsigKeyNameError, usesACL, zoneObjectName } from '../../lib/zones';

type Kind = 'primary' | 'forward' | 'secondary';

export interface Draft {
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
  fallback: boolean;
  fallbackGroup: string;
  primaries: string[];
  tsigKeyRef: string;
}

export const EMPTY: Draft = {
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
  fallback: false,
  fallbackGroup: '',
  primaries: [],
  tsigKeyRef: '',
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
    if (d.fallback) spec.fallback = d.fallbackGroup ? { upstreamGroup: d.fallbackGroup } : {};
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
  } else if (d.type === 'secondary') {
    spec.secondary = {
      primaries: cleanList(d.primaries),
      transferProtocol: 'tcp',
      ...(d.tsigKeyRef.trim() ? { tsigKeyRef: d.tsigKeyRef.trim() } : {}),
    };
  } else {
    spec.forward = {
      ...(d.upstreamGroup ? { upstreamGroup: d.upstreamGroup } : {}),
      ...(d.dnssec ? { dnssecValidation: d.dnssec === 'on' } : {}),
    };
  }
  return { apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSZone', metadata: { name }, spec };
}

const cidr = (v: string) => formatError('cidr', v);

/** Sets one field of a draft. */
export type SetDraft = <K extends keyof Draft>(k: K, v: Draft[K]) => void;

/** The first problem of a primary zone's settings, or null. */
export function primaryError(d: Draft): string | null {
  return (
    listError(d.nameservers, (s) => hostError(s)) ??
            (d.mname.trim() ? hostError(d.mname) && `Primary name server: ${hostError(d.mname)}` : null) ??
            (d.rname.trim() && !d.rname.includes('@') && hostError(d.rname) ? `Responsible mailbox: ${hostError(d.rname)}` : null) ??
            (['defaultTTL', 'refresh', 'retry', 'expire', 'minimum'] as const).map((k) => secondsError(d[k], { optional: true })).find(Boolean) ??
            (usesACL(d.queryAccess) && !cleanList(d.queryACL).length ? 'The ACL needs at least one entry for this access mode' : null) ??
            (usesACL(d.queryAccess) ? listError(d.queryACL, (s) => aclEntryError(s, cidr)) : null)
  );
}

/** Name servers, default TTL, SOA and who may query (primary zones). */
export function PrimaryFields({ d, set }: { d: Draft; set: SetDraft }) {
  const soaId = useId();
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

  return (
    <>
      <CheckboxInput
        label="Resolve missing names publicly"
        description="Use local records first, then resolve names absent from this zone. Missing record types at a local name stay local."
        value={d.fallback}
        onChange={(v) => set('fallback', v)}
      />
      {d.fallback ? (
        <>
          <UpstreamGroupSelect
            label="Fallback upstream group"
            value={d.fallbackGroup}
            onChange={(v) => set('fallbackGroup', v)}
            emptyLabel="Normal resolver routing"
            help="Choose a forwarding or recursive group, or follow client policy and conditional forwarding."
          />
          <Text type="supporting">An apex without a local record also resolves publicly for ordinary queries. SOA and NS stay local. Zone transfers and NOTIFY are unavailable with fallback.</Text>
        </>
      ) : null}
      <StringListField
        label="Name servers"
        description="The zone's NS records; none uses this server's name"
        value={d.nameservers}
        onChange={(v) => set('nameservers', v)}
        placeholder="ns1.example.com"
        itemError={(s) => hostError(s)}
        addLabel="Add name server"
      />
      <Grid columns={2} gap={4} align="start">
        {secs('defaultTTL', 'Default TTL', 'Records without their own TTL')}
      </Grid>
      <VStack gap={2} role="group" aria-labelledby={soaId}>
        <Heading level={4} id={soaId}>
          SOA
        </Heading>
        <Grid columns={2} gap={4} align="start">
          <TextField
            label="Primary name server"
            isOptional
            mono
            placeholder="the first name server"
            value={d.mname}
            onChange={(v) => set('mname', v)}
            error={d.mname.trim() ? (hostError(d.mname) ?? undefined) : undefined}
          />
          <TextField label="Responsible mailbox" isOptional placeholder="hostmaster@example.com" value={d.rname} onChange={(v) => set('rname', v)} help="An e-mail address; stored in SOA form" />
          {secs('refresh', 'Refresh', 'How often secondaries check the serial')}
          {secs('retry', 'Retry', 'After a failed refresh')}
          {secs('expire', 'Expire', 'Secondaries stop answering after this without the primary')}
          {secs('minimum', 'Negative TTL', 'How long resolvers cache "no such name"')}
        </Grid>
      </VStack>
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
  );
}

/** Upstream group and DNSSEC validation (forward zones). */
export function ForwardFields({ d, set }: { d: Draft; set: SetDraft }) {
  return (
    <>
      <UpstreamGroupSelect value={d.upstreamGroup} onChange={(v) => set('upstreamGroup', v)} help="The servers of this group answer for the zone." />
      <Text type="supporting">
        Need a group for this zone? Create one on the Upstreams page first; it takes effect without a restart.
      </Text>
      <EnumSelect label="DNSSEC validation" value={d.dnssec} options={DNSSEC} onChange={(v) => set('dnssec', v as Draft['dnssec'])} />
    </>
  );
}

export function secondaryError(d: Draft): string | null {
  return (
    (!cleanList(d.primaries).length ? 'At least one primary server is required' : null) ??
    listError(d.primaries, primaryEndpointError) ??
    tsigKeyNameError(d.tsigKeyRef)
  );
}

/** Inbound AXFR configuration, shared by creation and settings editing. */
export function SecondaryFields({ d, set }: { d: Draft; set: SetDraft }) {
  return (
    <>
      <StringListField
        label="Primary servers"
        description="Hostnames or IP addresses, with an optional port (default 53). Use [2001:db8::1]:5353 for IPv6 with a port. Servers are tried in order."
        value={d.primaries}
        onChange={(v) => set('primaries', v)}
        placeholder="ns1.example.com:5353"
        itemError={primaryEndpointError}
        addLabel="Add primary server"
      />
      <TextField
        label="TSIG key name"
        isOptional
        mono
        value={d.tsigKeyRef}
        onChange={(v) => set('tsigKeyRef', v)}
        placeholder="transfer-key.example."
        help="Optional key name configured on every node and the primary. Leave empty for unsigned transfers."
        error={tsigKeyNameError(d.tsigKeyRef) ?? undefined}
      />
      <Text type="supporting">Transfers use AXFR over TCP. Refresh, retry, expiry and record TTLs come from the primary's SOA and records.</Text>
    </>
  );
}

/** The draft of an existing zone's settings (the zone page's editor). */
export function draftFromZone(spec: DNSZoneSpec): Draft {
  const str = (v: number | undefined) => (v === undefined ? '' : String(v));
  return {
    ...EMPTY,
    type: spec.type === 'forward' || spec.type === 'secondary' ? spec.type : 'primary',
    apex: spec.zoneName ?? '',
    nameservers: spec.nameservers ?? [],
    defaultTTL: str(spec.defaultTTL),
    mname: spec.soa?.mname ?? '',
    rname: spec.soa?.rname ?? '',
    refresh: str(spec.soa?.refresh),
    retry: str(spec.soa?.retry),
    expire: str(spec.soa?.expire),
    minimum: str(spec.soa?.minimum),
    queryAccess: spec.policy?.queryAccess ?? 'allow',
    queryACL: spec.policy?.queryACL ?? [],
    upstreamGroup: spec.forward?.upstreamGroup ?? '',
    dnssec: spec.forward?.dnssecValidation === undefined ? '' : spec.forward.dnssecValidation ? 'on' : 'off',
    fallback: spec.fallback !== undefined,
    fallbackGroup: spec.fallback?.upstreamGroup ?? '',
    primaries: spec.secondary?.primaries ?? [],
    tsigKeyRef: spec.secondary?.tsigKeyRef ?? '',
  };
}

/**
 * spec with the draft's settings applied: the fields the editor shows are
 * replaced (empty ones removed, so the CRD defaults apply again) and every
 * other field of the zone is kept as it was.
 */
export function applyDraft(spec: DNSZoneSpec, d: Draft): DNSZoneSpec {
  const built = zoneFromDraft({ ...d, apex: spec.zoneName ?? d.apex }).spec;
  const next: DNSZoneSpec = { ...spec };
  if (d.type === 'primary') {
    delete next.fallback;
    if (built.fallback) next.fallback = built.fallback;
    delete next.nameservers;
    delete next.defaultTTL;
    delete next.soa;
    if (built.nameservers) next.nameservers = built.nameservers;
    if (built.defaultTTL !== undefined) next.defaultTTL = built.defaultTTL;
    if (built.soa) next.soa = built.soa;
    const policy = { ...spec.policy };
    delete policy.queryAccess;
    delete policy.queryACL;
    if (built.policy) Object.assign(policy, built.policy);
    if (Object.keys(policy).length) next.policy = policy;
    else delete next.policy;
  } else if (d.type === 'secondary') {
    next.secondary = { ...spec.secondary, primaries: built.secondary?.primaries ?? [] };
    if (built.secondary?.tsigKeyRef) next.secondary.tsigKeyRef = built.secondary.tsigKeyRef;
    else delete next.secondary.tsigKeyRef;
  } else {
    next.forward = { ...spec.forward, ...built.forward };
    if (!built.forward?.upstreamGroup) delete next.forward.upstreamGroup;
    if (built.forward?.dnssecValidation === undefined) delete next.forward.dnssecValidation;
  }
  return next;
}

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
      subtitle="Create records here, forward queries, or keep a secondary copy from a primary server."
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
                value: 'secondary',
                label: 'Secondary',
                description: 'Each kudurru node transfers a read-only copy from a primary over TCP and refreshes it using the primary’s SOA timers.',
              },
              {
                value: 'forward',
                label: 'Forward',
                description: 'Queries for the zone go to an upstream group, e.g. a corporate or Active Directory DNS for corp.example.',
              },
            ]}
          />
          <Text type="supporting">Stub and catalog zones are reserved for a later release.</Text>
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
          <TextField
            label="Zone name"
            mono
            placeholder={d.type === 'primary' ? 'home.arpa' : 'corp.example'}
            value={d.apex}
            onChange={(v) => set('apex', v)}
            isRequired
            help="A domain name such as home.arpa or lab.example.com; a trailing dot is optional"
            error={d.apex.trim() ? (apexError(d.apex, existing) ?? undefined) : undefined}
          />
          {name && !apexError(d.apex, existing) ? (
            <Text type="supporting">
              Object <Text type="code">dnszones/{name}</Text>, zone <Text type="code">{name}.</Text>
            </Text>
          ) : null}
        </>
      ),
    },
    d.type === 'primary'
      ? {
          id: 'primary',
          label: 'Records and access',
          description: 'Everything here has a sensible default; change only what you need.',
          validate: () => primaryError(d),
          content: <PrimaryFields d={d} set={set} />,
        }
      : d.type === 'secondary' ? {
          id: 'secondary',
          label: 'Primary servers',
          description: 'Where each node obtains its secondary copy of the zone.',
          validate: () => secondaryError(d),
          content: <SecondaryFields d={d} set={set} />,
        } : {
          id: 'forward',
          label: 'Upstream',
          description: 'Where queries for the zone are sent. Answers are cached like any forwarded answer.',
          content: <ForwardFields d={d} set={set} />,
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
    <MetadataList data-testid="zone-review">
      <MetadataListItem label="Zone">
        <Text type="code">{s.zoneName}</Text>
      </MetadataListItem>
      <MetadataListItem label="Type">{s.type === 'primary' ? 'Primary (records served here)' : s.type === 'secondary' ? 'Secondary (transferred from a primary)' : 'Forward (to an upstream group)'}</MetadataListItem>
      {s.type === 'primary' ? (
        <>
          <MetadataListItem label="Missing names">{s.fallback ? `Resolve through ${s.fallback.upstreamGroup || 'normal resolver routing'}` : 'Answer locally (NXDOMAIN)'}</MetadataListItem>
          <MetadataListItem label="Name servers">{s.nameservers?.join(', ') ?? 'this server'}</MetadataListItem>
          <MetadataListItem label="Default TTL">{dflt(s.defaultTTL, SOA_DEFAULTS.defaultTTL)}</MetadataListItem>
          <MetadataListItem label="SOA">
            refresh {dflt(s.soa?.refresh, SOA_DEFAULTS.refresh)}, retry {dflt(s.soa?.retry, SOA_DEFAULTS.retry)}, expire {dflt(s.soa?.expire, SOA_DEFAULTS.expire)},
            negative TTL {dflt(s.soa?.minimum, SOA_DEFAULTS.minimum)}
            {s.soa?.rname ? `, mailbox ${s.soa.rname}` : ''}
          </MetadataListItem>
          <MetadataListItem label="Who may query">
            {QUERY_ACCESS.find((q) => q.value === (s.policy?.queryAccess ?? 'allow'))?.label}
            {s.policy?.queryACL?.length ? `: ${s.policy.queryACL.join(', ')}` : ''}
          </MetadataListItem>
        </>
      ) : s.type === 'secondary' ? (
        <>
          <MetadataListItem label="Primary servers">{s.secondary?.primaries.join(', ')}</MetadataListItem>
          <MetadataListItem label="Transfer">AXFR over TCP; default port 53</MetadataListItem>
          <MetadataListItem label="TSIG key name">{s.secondary?.tsigKeyRef ?? 'None (unsigned transfers)'}</MetadataListItem>
          <MetadataListItem label="SOA timers">From the primary</MetadataListItem>
        </>
      ) : (
        <>
          <MetadataListItem label="Upstream group">
            <Text type="code">{s.forward?.upstreamGroup ?? `${DEFAULT_GROUP} (not set)`}</Text>
          </MetadataListItem>
          <MetadataListItem label="DNSSEC validation">{DNSSEC.find((x) => x.value === d.dnssec)?.label}</MetadataListItem>
        </>
      )}
    </MetadataList>
  );
}
