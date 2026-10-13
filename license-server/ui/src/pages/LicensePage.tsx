// /licenses/:id (licenses.read): one license for the licensor. The header
// with its state and actions (Renew, Withdraw, Email key), the current key
// panel, the facts, the Terms section (inline edit; "Sign new key" posts a
// change and warns that narrowing only takes effect once every earlier key
// has stopped working), the key history with a download per key, the
// non-commercial attestation and the recent audit events. A withdrawn
// license shows no key panel: who withdrew it, why, what the customer was
// told and how long installed keys keep working.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Grid } from '@astryxdesign/core/Grid';
import { Link } from '@astryxdesign/core/Link';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { pixel, proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { Mail } from 'lucide-react';
import { useState, type FormEvent } from 'react';
import type { ChangeBody, KeyRecord, LicenseDetail } from '@shared/api';
import { isFeature, PERSONAL_MAX_NODES, PLANS, planLabel, type Feature, type Plan } from '@shared/catalog';
import { useApi, useCan } from '../api/context';
import { useFieldErrors } from '../api/fieldErrors';
import { useCatalog, useCustomer, useLicense, useRouteMutation } from '../api/hooks';
import { RecentAudit } from '../components/AuditTable';
import { StateBadge } from '../components/badges';
import { InstallPanel, KeyNotices, KeyPanel } from '../components/keys';
import { EmailKeyDialog, keepsWorkingSentence, RenewDialog, WithdrawDialog } from '../components/LicenseDialogs';
import type { PageProps } from '../routes';
import { utcDay, utcDateTime } from '../lib/dates';
import { maxNodesFor, nameError, nodesError } from '../lib/validate';
import { KEY_REASON_LABEL, SOURCE_LABEL } from '../lib/words';
import { ErrorBanner, Loading } from '../ui/components/Feedback';
import { Page, PageHeader, Panel } from '../ui/components/layout/Page';
import { CustomerField, type CustomerChoice } from '../ui/fields/CustomerField';
import { featuresError, FeaturesField, featuresInWords } from '../ui/fields/FeaturesField';
import { NumberField } from '../ui/fields/NumberField';
import { SelectField } from '../ui/fields/SelectField';
import { TextField } from '../ui/fields/TextField';
import { formatExact } from '../ui/lib/format';

type KeyRow = KeyRecord & Record<string, unknown>;

export default function LicensePage({ params }: PageProps) {
  const id = params.id ?? '';
  const client = useApi();
  const can = useCan();
  const q = useLicense(id);
  const l = q.data;
  const customer = useCustomer(l?.customer.id ?? '', { enabled: can('customers.read') });
  const catalog = useCatalog();
  const [dialog, setDialog] = useState<'renew' | 'withdraw' | 'email' | null>(null);
  const live = l ? !l.withdrawn : false;
  const mail = catalog.data?.mail.configured ?? false;
  // Undefined while unknown (loading, or no customers.read): the dialogs then leave the address to the server.
  const contactEmail = customer.data ? customer.data.contactEmail : undefined;

  const keyColumns: TableColumn<KeyRow>[] = [
    { key: 'issuedAt', header: 'Signed (UTC)', width: pixel(170), renderCell: (k) => utcDay(k.issuedAt) },
    { key: 'reason', header: 'Reason', width: pixel(110), renderCell: (k) => KEY_REASON_LABEL[k.reason] },
    { key: 'expiresOn', header: 'Expires (UTC)', width: pixel(170), renderCell: (k) => utcDay(k.expiresOn) },
    { key: 'kid', header: 'Signing key', width: proportional(1, { minWidth: 140 }), renderCell: (k) => <Text type="code">{k.kid}</Text> },
    { key: 'issuedBy', header: 'Signed by', width: proportional(1, { minWidth: 160 }), renderCell: (k) => k.issuedBy.label },
    { key: 'supersededAt', header: 'Replaced', width: pixel(170), renderCell: (k) => (k.current ? <Text weight="semibold">Current key</Text> : utcDay(k.supersededAt)) },
    {
      key: 'download',
      header: 'Key',
      width: pixel(120),
      renderCell: (k) => (
        <Link href={client.href('licenseKeyDownload', { id, keyId: k.id })} label={`Download the key signed ${utcDay(k.issuedAt)}`}>
          Download
        </Link>
      ),
    },
  ];

  return (
    <Page>
      <PageHeader
        back={{ href: '/licenses', label: 'Licenses' }}
        title={l ? l.licensee : 'License'}
        meta={
          l ? (
            <>
              <StateBadge state={l.state} />
              <Text type="supporting">{planLabel(l.plan)}</Text>
              <Link href={`/customers/${encodeURIComponent(l.customer.id)}`}>{l.customer.name}</Link>
            </>
          ) : null
        }
        actions={
          l && live ? (
            <>
              {can('licenses.issue') ? (
                <Button
                  label="Email key"
                  variant="secondary"
                  icon={<Mail size={16} aria-hidden="true" />}
                  isDisabled={!mail}
                  tooltip={mail ? undefined : 'Mail is not configured; download the key instead'}
                  onClick={() => setDialog('email')}
                />
              ) : null}
              {can('licenses.renew') ? <Button label="Renew" variant="primary" onClick={() => setDialog('renew')} /> : null}
              {can('licenses.withdraw') ? <Button label="Withdraw" variant="destructive" onClick={() => setDialog('withdraw')} /> : null}
            </>
          ) : null
        }
      />
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isPending ? <Loading /> : null}
      {l ? (
        <>
          {l.withdrawn ? (
            <Banner
              status="warning"
              title={`Withdrawn on ${utcDay(l.withdrawn.at)} by ${l.withdrawn.by.label}. ${keepsWorkingSentence(l.withdrawn.keepsWorkingUntil)}`}
              description="The key is no longer offered to the customer, and the license cannot be renewed or changed."
              collapsible={false}
            >
              <VStack gap={1}>
                <Text as="p">Reason (internal): {l.withdrawn.reason}</Text>
                <Text as="p">Message to the customer: {l.withdrawn.customerMessage || 'none'}</Text>
              </VStack>
            </Banner>
          ) : (
            <KeyPanel
              title="Current key"
              token={l.currentKey.token}
              kid={l.currentKey.kid}
              issuedAt={l.currentKey.issuedAt}
              expiresOn={l.currentKey.expiresOn}
              plan={l.plan}
              downloadHref={client.href('licenseKeyDownload', { id: l.id, keyId: l.currentKey.id })}
            />
          )}
          <Grid columns={{ minWidth: 420, repeat: 'fit' }} gap={4}>
            <Panel title="Facts">
              <MetadataList>
                <MetadataListItem label="License ID">
                  <Text type="code">{l.id}</Text>
                </MetadataListItem>
                <MetadataListItem label="Customer">
                  <Link href={`/customers/${encodeURIComponent(l.customer.id)}`}>{l.customer.name}</Link>
                </MetadataListItem>
                <MetadataListItem label="Expires (UTC)">{utcDay(l.expiresOn)}</MetadataListItem>
                <MetadataListItem label="Keeps working until (UTC)">{utcDay(l.keepsWorkingUntil)}</MetadataListItem>
                {l.worksWithKudurru ? <MetadataListItem label="Works with">kudurru {l.worksWithKudurru} or later</MetadataListItem> : null}
                <MetadataListItem label="Source">{SOURCE_LABEL[l.source]}</MetadataListItem>
                {l.agreementRef ? <MetadataListItem label="Agreement reference">{l.agreementRef}</MetadataListItem> : null}
                <MetadataListItem label="Created">
                  {utcDay(l.createdAt)} by {l.createdBy.label}
                </MetadataListItem>
                {l.notes ? <MetadataListItem label="Notes">{l.notes}</MetadataListItem> : null}
              </MetadataList>
            </Panel>
            <TermsSection license={l} />
          </Grid>
          {l.attestation ? <AttestationPanel license={l} /> : null}
          <Panel title="Key history" description="Every key ever signed for this license, newest first. Each keeps working on nodes until its own expiry plus 30 days.">
            <Table<KeyRow> data={l.keys as KeyRow[]} columns={keyColumns} idKey="id" density="compact" />
            {/* The downloads hand out keys: with no key panel (withdrawn), the notices sit here (spec §0.7). */}
            {l.withdrawn ? <KeyNotices plan={l.plan} /> : null}
          </Panel>
          {live ? <InstallPanel expiresOn={l.currentKey.expiresOn} /> : null}
          <RecentAudit targetType="license" targetId={l.id} />
          {dialog === 'renew' ? <RenewDialog license={l} contactEmail={contactEmail} isOpen onClose={() => setDialog(null)} /> : null}
          {dialog === 'withdraw' ? <WithdrawDialog license={l} isOpen onClose={() => setDialog(null)} /> : null}
          {dialog === 'email' ? <EmailKeyDialog license={l} contactEmail={contactEmail} isOpen onClose={() => setDialog(null)} /> : null}
        </>
      ) : null}
    </Page>
  );
}

function AttestationPanel({ license: l }: { license: LicenseDetail }) {
  const a = l.attestation;
  if (!a) return null;
  return (
    <Panel title="Non-commercial attestation" description="The certification the personal key was issued on.">
      <MetadataList>
        <MetadataListItem label="Given">
          {utcDateTime(a.at)} ({a.source})
        </MetadataListItem>
        {a.statement ? <MetadataListItem label="Statement">{a.statement}</MetadataListItem> : null}
        {a.license ? <MetadataListItem label="License">{a.license}</MetadataListItem> : null}
        {a.email ? <MetadataListItem label="Email">{a.email}</MetadataListItem> : null}
        {a.ip ? <MetadataListItem label="Address">{a.ip}</MetadataListItem> : null}
        {a.userAgent ? <MetadataListItem label="Browser">{a.userAgent}</MetadataListItem> : null}
        {a.by ? (
          <MetadataListItem label="Attested by">
            <Link href={`/users/${encodeURIComponent(a.by)}`}>{a.by}</Link>
          </MetadataListItem>
        ) : null}
      </MetadataList>
    </Panel>
  );
}

interface TermsDraft {
  licensee: string;
  plan: Plan;
  nodes: string;
  /** null for all features. Sent only once touched, so a list this build cannot show in full is never rewritten. */
  features: Feature[] | null;
  /** Whether the features were changed (in the field, or by choosing Personal). */
  featuresTouched: boolean;
  customer: CustomerChoice | null;
}

/** Features the license names that this build does not know (an imported licensegen key may carry them). */
export function unknownFeatures(l: Pick<LicenseDetail, 'features'>): string[] {
  return l.features === null ? [] : l.features.filter((f) => !isFeature(f));
}

/** The change body: only what differs from the license (spec §6 Change). */
export function changeBody(l: LicenseDetail, d: TermsDraft): ChangeBody {
  const body: ChangeBody = {};
  if (d.licensee.trim() !== l.licensee) body.licensee = d.licensee.trim();
  if (d.plan !== l.plan) body.plan = d.plan;
  if (Number(d.nodes) !== l.nodes) body.nodes = Number(d.nodes);
  if (d.featuresTouched) {
    const before = l.features === null ? null : [...l.features].sort().join(',');
    const after = d.features === null ? null : [...d.features].sort().join(',');
    if (before !== after) body.features = d.features;
  }
  if (d.customer && d.customer.id !== l.customer.id) body.customerId = d.customer.id;
  return body;
}

/** Whether a change narrows what installed keys grant (fewer nodes, fewer features, another plan). */
export function narrows(l: LicenseDetail, d: TermsDraft): boolean {
  if (Number(d.nodes) < l.nodes || d.plan !== l.plan) return true;
  if (!d.featuresTouched || d.features === null) return false;
  if (l.features === null) return true;
  return l.features.some((f) => !d.features?.includes(f as Feature));
}

/** The body fields the Terms controls show (a 400 names them inline). */
const TERMS_FIELDS = ['licensee', 'plan', 'nodes', 'features', 'customerId'];

function TermsSection({ license: l }: { license: LicenseDetail }) {
  const can = useCan();
  const m = useRouteMutation('licenseChange');
  const fe = useFieldErrors(m.error, TERMS_FIELDS);
  const [draft, setDraft] = useState<TermsDraft | null>(null);
  const [showErrors, setShowErrors] = useState(false);
  const readOnly = l.withdrawn ? 'A withdrawn license cannot be changed' : !can('licenses.change') ? 'Needs licenses.change' : null;
  const unknown = unknownFeatures(l);
  const knownFeatures = l.features === null ? null : l.features.filter(isFeature);

  const start = () => {
    m.reset();
    setShowErrors(false);
    setDraft({
      licensee: l.licensee,
      plan: l.plan,
      nodes: String(l.nodes),
      features: knownFeatures,
      featuresTouched: false,
      customer: { id: l.customer.id, name: l.customer.name, kind: l.customer.kind, contactEmail: null },
    });
  };
  const set = (patch: Partial<TermsDraft>) => setDraft((d) => (d ? { ...d, ...patch } : d));

  const problems = (d: TermsDraft) =>
    nameError(d.licensee, 'the licensee') ?? nodesError(d.nodes, d.plan) ?? (d.featuresTouched ? featuresError(d.features) : null) ?? (d.customer ? null : 'Choose a customer');

  const choosePlan = (d: TermsDraft, p: Plan) => {
    fe.edited('plan');
    if (p === 'personal') {
      // A personal key always carries every feature.
      set({ plan: p, nodes: String(Math.min(Number(d.nodes) || PERSONAL_MAX_NODES, PERSONAL_MAX_NODES)), features: null, featuresTouched: true });
    } else if (d.plan === 'personal' && l.plan !== 'personal') {
      // Back from Personal: the license's own features again, untouched.
      set({ plan: p, features: knownFeatures, featuresTouched: false });
    } else set({ plan: p });
  };

  const save = async (e: FormEvent) => {
    e.preventDefault();
    if (!draft) return;
    setShowErrors(true);
    if (problems(draft)) return;
    const body = changeBody(l, draft);
    if (!Object.keys(body).length) {
      setDraft(null);
      return;
    }
    try {
      await m.mutateAsync({ params: { id: l.id }, body });
      setDraft(null);
    } catch {
      // Shown in the section (the fields it names, else the banner).
    }
  };

  const view = (
    <MetadataList>
      <MetadataListItem label="Licensee">{l.licensee}</MetadataListItem>
      <MetadataListItem label="Plan">{planLabel(l.plan)}</MetadataListItem>
      <MetadataListItem label="Nodes">{formatExact(l.nodes)}</MetadataListItem>
      <MetadataListItem label="Features">{featuresInWords(l.features)}</MetadataListItem>
    </MetadataList>
  );

  return (
    <Panel
      title="Terms"
      description="What the current key says. Changing them signs a new key; the expiry stays (renew to move it)."
      actions={!draft ? <Button label="Edit terms" size="sm" variant="secondary" isDisabled={Boolean(readOnly)} tooltip={readOnly ?? undefined} onClick={start}>Edit</Button> : null}
    >
      {!draft ? (
        view
      ) : (
        <VStack as="form" gap={3} {...formAttrs} onSubmit={(e: FormEvent) => void save(e)}>
          <TextField
            label="Licensee"
            value={draft.licensee}
            onChange={(v) => {
              set({ licensee: v });
              fe.edited('licensee');
            }}
            isRequired
            error={(showErrors ? nameError(draft.licensee, 'the licensee') : null) ?? fe.field('licensee')}
          />
          <SelectField
            label="Plan"
            value={draft.plan}
            options={PLANS.map((p) => ({ value: p.key, label: p.label, help: p.description }))}
            onChange={(v) => choosePlan(draft, v as Plan)}
            error={fe.field('plan')}
          />
          <NumberField
            label="Nodes"
            value={draft.nodes}
            onChange={(v) => {
              set({ nodes: v });
              fe.edited('nodes');
            }}
            min={1}
            max={maxNodesFor(draft.plan)}
            error={fe.field('nodes')}
          />
          {unknown.length && draft.plan !== 'personal' ? (
            <VStack gap={1} data-testid="features-kept">
              <MetadataList>
                <MetadataListItem label="Features">{featuresInWords(l.features)}</MetadataListItem>
              </MetadataList>
              <Text type="supporting" display="block">
                {`The key names ${unknown.length === 1 ? 'a feature' : 'features'} this portal does not know (${unknown.join(', ')}), so the features stay exactly as they are. To grant other features, issue a new license.`}
              </Text>
            </VStack>
          ) : (
            <FeaturesField
              value={draft.features}
              onChange={(v) => {
                set({ features: v, featuresTouched: true });
                fe.edited('features');
              }}
              showErrors={showErrors && draft.featuresTouched}
              lockedAllReason={draft.plan === 'personal' ? 'A personal key always carries every feature.' : undefined}
              error={fe.field('features')}
            />
          )}
          <CustomerField
            label="Customer"
            value={draft.customer}
            onChange={(c) => {
              set({ customer: c });
              fe.edited('customerId');
            }}
            help={draft.customer && draft.customer.id !== l.customer.id ? `Moves the license: members of ${l.customer.name} lose access to it in the portal.` : 'To move the license, choose another customer.'}
            error={(showErrors && !draft.customer ? 'Choose a customer' : null) ?? fe.field('customerId')}
          />
          {narrows(l, draft) ? (
            <Banner
              status="warning"
              title="Narrowing takes effect late"
              description={`Keys already installed keep their terms: fewer nodes, fewer features or another plan only apply everywhere once every earlier key stops working, on ${utcDay(l.keepsWorkingUntil)}.`}
              collapsible={false}
            />
          ) : null}
          {fe.summary ? <Banner status="error" title="No new key was signed" description={fe.summary} /> : null}
          <HStack gap={2} wrap="wrap">
            <Button type="submit" label="Sign new key" variant="primary" isLoading={m.isPending} />
            <Button label="Cancel" variant="ghost" onClick={() => setDraft(null)} />
          </HStack>
        </VStack>
      )}
    </Panel>
  );
}

/** <form> attributes the Astryx stack props do not type (they reach the element). */
const formAttrs: object = { noValidate: true };
