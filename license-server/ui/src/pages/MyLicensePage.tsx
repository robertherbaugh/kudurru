// /my/licenses/:id (self.read): one license as its customer sees it. The
// current key (CodeBlock, Download license.key, Copy) with the notices, the
// facts with every date in UTC, how to install the key, renewal (a
// personal key inside its renew window) or the business mailto, and the
// people with access. A withdrawn license shows no key: only when it was
// withdrawn, how long installed keys keep working and the licensor's
// message. Renewing a personal key needs the certification ticked again
// (POST /api/v1/me/licenses/:id/renew with noncommercial: true); while
// approval is required (Catalog.approvalRequired) it opens a request that
// a license admin approves, and the new key arrives by email.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Grid } from '@astryxdesign/core/Grid';
import { Link } from '@astryxdesign/core/Link';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { pixel, proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useState } from 'react';
import type { MyLicenseDetail, PersonalRequestResult } from '@shared/api';
import { ATTESTATION_STATEMENT, GRACE_DAYS, LICENSE_PAGE_URL, NONCOMMERCIAL_LICENSE_URL, planLabel } from '@shared/catalog';
import { ApiError } from '../api/client';
import { useApi, useCan, useMe } from '../api/context';
import { useFieldErrors } from '../api/fieldErrors';
import { useCatalog, useMyLicense, useRouteMutation } from '../api/hooks';
import { MemberStatusBadge, StateBadge } from '../components/badges';
import { InstallPanel, KeyNotices, KeyPanel } from '../components/keys';
import type { PageProps } from '../routes';
import { utcDay } from '../lib/dates';
import { businessMailto, outcomeWords } from '../lib/words';
import { featuresInWords } from '../ui/fields/FeaturesField';
import { ErrorBanner, Loading, useNotify } from '../ui/components/Feedback';
import { DialogForm, FormDialog } from '../ui/components/FormDialog';
import { CheckboxField } from '../ui/fields/TextField';
import { Page, PageHeader, Panel } from '../ui/components/layout/Page';
import { formatExact } from '../ui/lib/format';

type PersonRow = MyLicenseDetail['people'][number] & Record<string, unknown>;

const peopleColumns: TableColumn<PersonRow>[] = [
  { key: 'email', header: 'Email', width: proportional(1, { minWidth: 220 }) },
  { key: 'status', header: 'Access', width: pixel(140), renderCell: (p) => <MemberStatusBadge status={p.status} /> },
];

/** The withdrawn sentence (spec §0.8). */
export function withdrawnSentence(at: string, keepsWorkingUntil: string): string {
  return `Withdrawn on ${utcDay(at)}. Keys already installed keep working until ${utcDay(keepsWorkingUntil)}.`;
}

export default function MyLicensePage({ params }: PageProps) {
  const id = params.id ?? '';
  const client = useApi();
  const q = useMyLicense(id);
  const l = q.data;
  return (
    <Page>
      <PageHeader
        back={{ href: '/my/licenses', label: 'My licenses' }}
        title={l ? l.licensee : 'License'}
        meta={
          l ? (
            <>
              <StateBadge state={l.state} />
              <Text type="supporting">{planLabel(l.plan)}</Text>
            </>
          ) : null
        }
        actions={l ? <RenewOrAsk license={l} /> : null}
      />
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isPending ? <Loading /> : null}
      {l ? (
        <>
          {l.withdrawn ? (
            <Banner status="warning" title={withdrawnSentence(l.withdrawn.at, l.withdrawn.keepsWorkingUntil)} description={l.withdrawn.customerMessage || 'The licensor withdrew this license. Its key is no longer offered here.'} collapsible={false} />
          ) : null}
          {l.key ? (
            <KeyPanel token={l.key.token} kid={l.key.kid} issuedAt={l.key.issuedAt} expiresOn={l.key.expiresOn} plan={l.plan} downloadHref={client.href('myLicenseKey', { id: l.id })} />
          ) : null}
          <Grid columns={{ minWidth: 420, repeat: 'fit' }} gap={4}>
            <Panel title="License">
              <MetadataList>
                <MetadataListItem label="License ID">
                  <Text type="code">{l.id}</Text>
                </MetadataListItem>
                <MetadataListItem label="Licensee">{l.licensee}</MetadataListItem>
                <MetadataListItem label="Customer">{l.customer.name}</MetadataListItem>
                <MetadataListItem label="Plan">{planLabel(l.plan)}</MetadataListItem>
                <MetadataListItem label="Nodes">{formatExact(l.nodes)}</MetadataListItem>
                <MetadataListItem label="Features">{featuresInWords(l.features)}</MetadataListItem>
                <MetadataListItem label="Expires (UTC)">{utcDay(l.expiresOn)}</MetadataListItem>
                <MetadataListItem label="Keeps working until (UTC)">{utcDay(l.keepsWorkingUntil)}</MetadataListItem>
                {l.worksWithKudurru ? <MetadataListItem label="Works with">kudurru {l.worksWithKudurru} or later</MetadataListItem> : null}
              </MetadataList>
              <Text type="supporting" as="p">
                Nodes keep accepting a key for 30 days after it expires, so they can be given the next one in time.
              </Text>
              {!l.key ? <KeyNotices plan={l.plan} /> : null}
            </Panel>
            <Panel title="People with access" description="Everyone who sees this customer's licenses and keys. The licensor manages the list.">
              <Table<PersonRow> data={l.people} columns={peopleColumns} idKey="email" density="compact" />
            </Panel>
          </Grid>
          {l.key ? <InstallPanel expiresOn={l.key.expiresOn} /> : null}
        </>
      ) : null}
    </Page>
  );
}

/** Renew (a personal key in its window; a request while approval is required) or, for business keys, the mailto. */
function RenewOrAsk({ license: l }: { license: MyLicenseDetail }) {
  const can = useCan();
  const catalog = useCatalog();
  const [open, setOpen] = useState(false);
  if (l.withdrawn) return null;
  if (l.plan !== 'personal') {
    return <Button label="Ask about a renewal or a change" variant="secondary" href={businessMailto(l.id)} />;
  }
  if (!l.renewal || !can('self.request')) return null;
  // Approval is the default; only a catalog that says otherwise shows the immediate Renew.
  const approval = catalog.data?.approvalRequired !== false;
  return (
    <>
      <Button label={approval ? 'Request renewal' : 'Renew'} variant="primary" onClick={() => setOpen(true)} />
      <FormDialog isOpen={open} onClose={() => setOpen(false)} title={approval ? 'Request a renewal of your personal key' : 'Renew your personal key'} width={560}>
        <RenewForm license={l} until={l.renewal.expiresOn} approval={approval} onClose={() => setOpen(false)} />
      </FormDialog>
    </>
  );
}

/** The body field the certification checkbox shows (a 400 names it inline). */
const RENEW_FIELDS = ['noncommercial'];

function RenewForm({ license: l, until, approval, onClose }: { license: MyLicenseDetail; until: string; approval: boolean; onClose: () => void }) {
  const me = useMe();
  const notify = useNotify();
  const m = useRouteMutation('myLicenseRenew');
  const [certified, setCertified] = useState(false);
  const [showErrors, setShowErrors] = useState(false);
  const unverifiedAnswer = m.error instanceof ApiError && m.error.key === 'email_unverified';
  const fe = useFieldErrors(unverifiedAnswer ? null : m.error, RENEW_FIELDS);
  const verified = me.user.emailVerified && Boolean(me.user.email);
  const certError = certified ? null : `Tick the certification to ${approval ? 'request a renewal' : 'renew your personal key'}`;
  const action = approval ? 'Request renewal' : 'Renew';

  if (m.data && m.data.outcome !== 'renewed') return <RenewOutcome result={m.data} email={me.user.email} onClose={onClose} />;

  return (
    <DialogForm
      onSubmit={() => {
        setShowErrors(true);
        if (certError || !verified) return;
        m.mutate(
          { params: { id: l.id }, body: { noncommercial: true } },
          {
            onSuccess: (d) => {
              if (d.outcome !== 'renewed') return;
              notify.success(`Renewed: the new key is valid through ${utcDay(until)}`);
              onClose();
            },
          },
        );
      }}
      noValidate
      gap={3}
      footer={
        <>
          <Button label="Cancel" variant="ghost" onClick={onClose} />
          <Button type="submit" label={action} variant="primary" isLoading={m.isPending} isDisabled={!verified} tooltip={verified ? undefined : 'Needs a verified email address'} />
        </>
      }
    >
      <Text as="p">
        {approval
          ? `Once a license admin approves the renewal, the new key arrives by email at ${me.user.email ?? 'your verified address'} and shows here, valid through ${utcDay(until)}.`
          : `A new key is signed, valid through ${utcDay(until)}, mailed to ${me.user.email ?? 'your verified address'} and shown here.`}{' '}
        Install it on your nodes; the current key keeps working until it expires and for {GRACE_DAYS} days after.
      </Text>
      {!verified || unverifiedAnswer ? (
        <Banner
          status="warning"
          title="Your sign-in did not share a verified email address"
          description="The portal mails keys only to an address your identity provider verified. Ask its administrator, or use the public form at kudurru.io/license."
          endContent={
            <Link href={LICENSE_PAGE_URL} isExternalLink>
              Public form
            </Link>
          }
        />
      ) : null}
      <CheckboxField
        label={ATTESTATION_STATEMENT}
        value={certified}
        onChange={(v) => {
          setCertified(v);
          fe.edited('noncommercial');
        }}
        error={(showErrors ? certError : null) ?? fe.field('noncommercial')}
      />
      <Text type="supporting" as="p">
        <Link href={NONCOMMERCIAL_LICENSE_URL} isExternalLink>
          Read the Kudurru Community License
        </Link>
      </Text>
      {fe.summary ? <Banner status="error" title={approval ? 'The renewal was not requested' : 'The key was not renewed'} description={fe.summary} /> : null}
    </DialogForm>
  );
}

/** What a renewal led to when no key was signed at once: usually a request waiting for approval. */
function RenewOutcome({ result, email, onClose }: { result: PersonalRequestResult; email: string | null; onClose: () => void }) {
  const w = outcomeWords(result.outcome, email, 'portal');
  return (
    <DialogForm footer={<Button label="Done" variant="primary" onClick={onClose} />}>
      <Banner status={result.outcome === 'pending_review' ? 'info' : 'success'} title={w.title} description={w.body} />
    </DialogForm>
  );
}
