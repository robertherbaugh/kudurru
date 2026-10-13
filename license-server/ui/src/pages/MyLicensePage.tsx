// /my/licenses/:id (self.read): one license as its customer sees it. The
// current key (CodeBlock, Download license.key, Copy) with the notices, the
// facts with every date in UTC, how to install the key, renewal (a
// personal key inside its renew window) or the business mailto, and the
// people with access. A withdrawn license shows no key: only when it was
// withdrawn, how long installed keys keep working and the licensor's
// message.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Grid } from '@astryxdesign/core/Grid';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { HStack } from '@astryxdesign/core/Stack';
import { pixel, proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useState } from 'react';
import type { MyLicenseDetail } from '@shared/api';
import { planLabel } from '@shared/catalog';
import { useApi, useCan } from '../api/context';
import { useMyLicense, useRouteMutation } from '../api/hooks';
import { MemberStatusBadge, StateBadge } from '../components/badges';
import { InstallPanel, KeyNotices, KeyPanel } from '../components/keys';
import type { PageProps } from '../routes';
import { utcDay } from '../lib/dates';
import { businessMailto } from '../lib/words';
import { featuresInWords } from '../ui/fields/FeaturesField';
import { ErrorBanner, Loading, useNotify } from '../ui/components/Feedback';
import { DialogForm, FormDialog } from '../ui/components/FormDialog';
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

/** Renew (a personal key in its window) or, for business keys, the mailto. */
function RenewOrAsk({ license: l }: { license: MyLicenseDetail }) {
  const can = useCan();
  const [open, setOpen] = useState(false);
  if (l.withdrawn) return null;
  if (l.plan !== 'personal') {
    return <Button label="Ask about a renewal or a change" variant="secondary" href={businessMailto(l.id)} />;
  }
  if (!l.renewal || !can('self.request')) return null;
  return (
    <>
      <Button label="Renew" variant="primary" onClick={() => setOpen(true)} />
      <FormDialog isOpen={open} onClose={() => setOpen(false)} title="Renew your personal key" width={480}>
        <RenewForm license={l} until={l.renewal.expiresOn} onClose={() => setOpen(false)} />
      </FormDialog>
    </>
  );
}

function RenewForm({ license: l, until, onClose }: { license: MyLicenseDetail; until: string; onClose: () => void }) {
  const notify = useNotify();
  const m = useRouteMutation('myLicenseRenew');
  return (
    <DialogForm
      onSubmit={() =>
        m.mutate(
          { params: { id: l.id } },
          {
            onSuccess: (d) => {
              notify.success(`Renewed: the new key is valid through ${utcDay(d.expiresOn)}`);
              onClose();
            },
          },
        )
      }
      footer={
        <HStack gap={2}>
          <Button label="Cancel" variant="ghost" onClick={onClose} />
          <Button type="submit" label="Renew" variant="primary" isLoading={m.isPending} />
        </HStack>
      }
    >
      <Text as="p">
        A new key is signed, valid through {utcDay(until)}. Install it on your nodes; the current key keeps working until it expires and for 30 days after.
      </Text>
      {m.error ? <ErrorBanner error={m.error} title="The key was not renewed" /> : null}
    </DialogForm>
  );
}
