// License (/license): what this node runs with (GET /license) and what the
// key unlocks. kudurru is source-available; personal use is free and a key
// is only needed for more than one node and the licensed features. The page
// is read-only: a key is put in place with the environment, a file or the
// command line (docs/architecture/licensing.md), not typed into the browser.
import { Badge } from '@astryxdesign/core/Badge';
import { Grid } from '@astryxdesign/core/Grid';
import { Link } from '@astryxdesign/core/Link';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { VStack } from '@astryxdesign/core/Stack';
import { pixel, proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { Check, Lock } from 'lucide-react';
import { useLicense } from '../api/hooks';
import { ErrorBanner, Loading } from '../components/Feedback';
import { Page, PageHeader, Panel } from '../components/layout/Page';
import { formatExact } from '../lib/format';
import { FEATURES, LICENSE_CONTACT, LICENSE_PAGE_URL, LICENSE_TEXT_URL, LICENSING_URL, planLabel, policyLimitLabel, stateLabel, stateTone, type FeatureInfo } from '../lib/license';

type Row = FeatureInfo & { included: boolean } & Record<string, unknown>;

function day(iso: string | undefined): string {
  if (!iso) return '–';
  const t = Date.parse(iso);
  return Number.isNaN(t) ? iso : new Date(t).toLocaleDateString(undefined, { year: 'numeric', month: 'long', day: 'numeric' });
}

export default function LicensePage() {
  const q = useLicense();
  const l = q.data;

  const columns: TableColumn<Row>[] = [
    {
      key: 'label',
      header: 'Feature',
      width: proportional(2, { minWidth: 180 }),
      renderCell: (f) => <Text weight="semibold">{f.label}</Text>,
    },
    { key: 'unlocks', header: 'What the key unlocks', width: proportional(5, { minWidth: 260 }), renderCell: (f) => <Text color="secondary">{f.unlocks}</Text> },
    {
      key: 'included',
      header: 'On this node',
      width: pixel(150),
      renderCell: (f) =>
        f.included ? (
          <Text weight="semibold" data-testid={`feature-${f.key}`}>
            <Check size={14} aria-hidden="true" /> Included
          </Text>
        ) : (
          <Text color="secondary" data-testid={`feature-${f.key}`}>
            <Lock size={14} aria-hidden="true" /> Needs a key
          </Text>
        ),
    },
  ];

  return (
    <Page>
      <PageHeader
        title="License"
        meta={l ? <Badge variant={stateTone(l.state)} label={stateLabel(l.state)} /> : null}
        description="kudurru is source-available: personal use is free and a business needs an agreement. A key unlocks more nodes, more policies and the features below. DNS answering never depends on a key."
      />
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isLoading ? <Loading /> : null}
      {l ? (
        <>
          <Grid columns={{ minWidth: 420, repeat: 'fit' }} gap={4}>
            <Panel title="This node" headingId="license-node-title">
              <MetadataList columns="multi">
                <MetadataListItem label="State">{stateLabel(l.state)}</MetadataListItem>
                {l.plan ? <MetadataListItem label="Plan">{planLabel(l.plan)}</MetadataListItem> : null}
                {l.licensee ? <MetadataListItem label="Licensee">{l.licensee}</MetadataListItem> : null}
                {l.id ? (
                  <MetadataListItem label="License ID">
                    <Text type="code" size="sm">
                      {l.id}
                    </Text>
                  </MetadataListItem>
                ) : null}
                {l.source ? <MetadataListItem label="Read from">{l.source}</MetadataListItem> : null}
                {l.expiresAt ? <MetadataListItem label="Expires">{day(l.expiresAt)}</MetadataListItem> : null}
                {l.state === 'grace' && l.graceEndsAt ? <MetadataListItem label="Grace period ends">{day(l.graceEndsAt)}</MetadataListItem> : null}
              </MetadataList>
              {l.error ? (
                <Text color="primary" as="p">
                  {l.error}
                </Text>
              ) : null}
            </Panel>
            <Panel title="Limits" headingId="license-limits-title">
              <MetadataList columns="multi">
                <MetadataListItem label="Nodes">
                  <Text type="inherit" data-testid="license-nodes">
                    {formatExact(l.nodesInUse)} of {formatExact(l.nodes)}
                    {l.overLimit ? ' (over the limit: configuration changes are refused)' : ''}
                  </Text>
                </MetadataListItem>
                <MetadataListItem label="Policies">{policyLimitLabel(l.policyLimit)}</MetadataListItem>
              </MetadataList>
              <Text type="supporting" as="p">
                Limits only narrow configuration changes. Queries are answered the same with any key, with none, and with an expired one.
              </Text>
            </Panel>
          </Grid>
          <Panel title="Features" headingId="license-features-title">
            <Table<Row>
              aria-label="Licensed features"
              density="compact"
              idKey="key"
              data={FEATURES.map((f) => ({ ...f, included: l.features.includes(f.key) }))}
              columns={columns}
            />
          </Panel>
        </>
      ) : null}
      <Panel title="Add or change the key" headingId="license-key-title">
        <VStack gap={2}>
          <Text as="p">
            Put the key where the node can read it, then wait up to a minute (no restart). The first match wins:
          </Text>
          <VStack gap={1}>
            <Text type="code">kudurru serve --license-key &lt;key&gt;</Text>
            <Text type="code">KUDURRU_LICENSE_KEY=&lt;key&gt;</Text>
            <Text type="code">KUDURRU_LICENSE_KEY_FILE=/run/secrets/license_key</Text>
            <Text type="code">&lt;node.dataDir&gt;/license.key</Text>
          </VStack>
          <Text as="p">
            <Text type="code">kudurru license status</Text> shows what a node would run with and <Text type="code">kudurru license verify &lt;key&gt;</Text> checks a key without starting anything. In Helm, set{' '}
            <Text type="code">license.existingSecret</Text>.
          </Text>
          <Text as="p">
            A free personal key covers up to 3 nodes for non-commercial use; write to <Link href={`mailto:${LICENSE_CONTACT}`}>{LICENSE_CONTACT}</Link> for one, or for a commercial agreement. What each license includes is on{' '}
            <Link href={LICENSE_PAGE_URL}>kudurru.io/license</Link>, the rules are in the <Link href={LICENSING_URL}>licensing docs</Link> and the license is the{' '}
            <Link href={LICENSE_TEXT_URL}>PolyForm Noncommercial License 1.0.0</Link>.
          </Text>
        </VStack>
      </Panel>
    </Page>
  );
}
