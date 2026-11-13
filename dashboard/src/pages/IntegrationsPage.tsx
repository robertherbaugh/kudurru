// Integrations (/integrations): every Integration object of this node
// (GET /objects/integrations) with the host's status of its exact revision
// (GET /integrations?namespace=), rows linking to the instance's page. "New
// integration" opens the manifest-driven wizard (?new=1). The page asks for
// nothing without integrations.read; a node without the integration host
// (503) says so instead of showing an empty list.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { Link } from '@astryxdesign/core/Link';
import { VStack } from '@astryxdesign/core/Stack';
import { pixel, proportional, Table, type TableColumn, type TablePlugin } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useMemo, type ReactNode } from 'react';
import { ApiError } from '../api/client';
import { useCannotReason } from '../api/context';
import type { Integration, IntegrationSummary } from '../api/types';
import { ErrorBanner, Loading } from '../components/Feedback';
import { ManagedBadge } from '../components/ManagedBadge';
import { Page, PageHeader } from '../components/layout/Page';
import { IntegrationWizardDialog } from '../components/wizards/IntegrationWizard';
import { formatRelative } from '../lib/format';
import { integrationHref, manifestFor, phaseVariant, REASON_TEXT } from '../lib/integrations';
import { MANAGED_BY } from '../lib/managedBy';
import { useNewParam } from '../lib/router';
import { useCanReadIntegrations, useIntegrationManifests, useIntegrationObjects, useIntegrationSummaries } from '../lib/useIntegrations';

type Row = { obj: Integration; summary: IntegrationSummary | undefined; key: string } & Record<string, unknown>;

const rowAttrs: TablePlugin<Row> = {
  transformBodyRow: (props, r) => ({ ...props, htmlProps: { ...props.htmlProps, ...({ 'data-testid': `integration-${r.key}` } as Record<string, string>) } }),
};

/**
 * The phase the object's own Ready condition records for its current
 * generation (the host writes it), or undefined. The host reports its live
 * status for the revision it captured; a status write gives the object a
 * new revision, so the list falls back to the written condition until the
 * host reports the stored one.
 */
export function objectPhase(obj: Integration | undefined): string | undefined {
  const ready = obj?.status?.conditions?.find((c) => c.type === 'Ready');
  if (!ready || (ready.observedGeneration !== undefined && obj?.metadata.generation !== undefined && ready.observedGeneration !== obj.metadata.generation)) return undefined;
  return ready.status === 'True' ? 'Ready' : ready.reason || 'Not ready';
}

/** The state badge and reason of one instance. */
export function IntegrationState({ summary, obj }: { summary: IntegrationSummary | undefined; obj?: Integration | undefined }) {
  if (!summary) return <Text type="supporting">No status</Text>;
  const fallback = summary.statusKnown ? undefined : objectPhase(obj);
  const label = summary.terminating ? 'Terminating' : summary.statusKnown ? summary.phase : (fallback ?? 'Pending');
  const variant = summary.statusKnown || summary.terminating || !fallback ? phaseVariant(summary) : fallback === 'Ready' ? 'success' : 'warning';
  const reason = summary.statusKnown || summary.terminating ? summary.reason : fallback ? '' : summary.reason;
  return (
    <VStack gap={0.5} align="start">
      <Badge variant={variant} label={label} />
      {reason ? <Text type="supporting">{REASON_TEXT[reason] ?? reason}</Text> : null}
    </VStack>
  );
}

/** Whether an error is the 503 of a node that has no integration host. */
export function hostUnavailable(err: unknown): boolean {
  return err instanceof ApiError && err.status === 503 && err.key === 'unavailable';
}

export default function IntegrationsPage() {
  const readable = useCanReadIntegrations();
  const cannot = useCannotReason();
  const manifests = useIntegrationManifests();
  const objects = useIntegrationObjects();
  const [isNew, setNew] = useNewParam();
  const namespaces = useMemo(() => {
    const set = new Set((objects.data?.items ?? []).map((o) => o.metadata.namespace ?? 'default'));
    if (manifests.data?.namespace) set.add(manifests.data.namespace);
    return [...set].sort();
  }, [objects.data, manifests.data]);
  const summaries = useIntegrationSummaries(namespaces);

  const rows: Row[] = useMemo(
    () =>
      (objects.data?.items ?? [])
        .map((obj) => {
          const key = `${obj.metadata.namespace ?? 'default'}/${obj.metadata.name}`;
          return { obj, key, summary: summaries.byKey.get(key) };
        })
        .sort((a, b) => a.key.localeCompare(b.key)),
    [objects.data, summaries.byKey],
  );

  const header = (actions?: ReactNode) => (
    <PageHeader
      title="Integrations"
      description="Providers this node runs in the background: publish DNS records (Cloudflare, a webhook peer), read client inventory (UniFi, a webhook peer) and offer actions. Each runs on the elected writer with its own lease."
      actions={actions}
    />
  );

  if (!readable) {
    return (
      <Page>
        {header()}
        <Banner status="warning" title="Not permitted" description="Viewing integrations needs integrations.read." />
      </Page>
    );
  }

  const unavailable = hostUnavailable(manifests.error) || hostUnavailable(summaries.error);
  const writeReason = cannot('integrations', 'write');
  const newReason = !manifests.data ? 'Loading the provider manifests' : !manifests.data.licensed ? 'A new Integration needs a license that covers integrations' : writeReason;

  const columns: TableColumn<Row>[] = [
    {
      key: 'name',
      header: 'Name',
      width: proportional(1.4, { minWidth: 160 }),
      renderCell: (r) => (
        <Link href={integrationHref(r.obj.metadata.namespace ?? 'default', r.obj.metadata.name)} color="inherit" weight="semibold" hasUnderline>
          <Text type="code" color="inherit">
            {r.key}
          </Text>
        </Link>
      ),
    },
    { key: 'type', header: 'Provider', width: proportional(1, { minWidth: 110 }), renderCell: (r) => <Text>{manifestFor(manifests.data, r.obj.spec.type)?.displayName ?? r.obj.spec.type}</Text> },
    { key: 'state', header: 'State', width: proportional(1.6, { minWidth: 160 }), renderCell: (r) => <IntegrationState summary={r.summary} obj={r.obj} /> },
    {
      key: 'policy',
      header: 'When deleted',
      width: pixel(120),
      renderCell: (r) => <Text type="supporting">{r.summary?.deletionPolicy ?? r.obj.spec.deletionPolicy ?? 'Retain'}</Text>,
    },
    {
      key: 'lastSync',
      header: 'Last sync',
      width: pixel(130),
      renderCell: (r) => <Text type="supporting">{r.summary?.lastSync ? formatRelative(r.summary.lastSync) : 'never'}</Text>,
    },
    { key: 'managed', header: 'Managed by', width: pixel(140), renderCell: (r) => <ManagedBadge owner={r.obj.metadata.annotations?.[MANAGED_BY]} inColumn /> },
  ];

  return (
    <Page>
      {header(<Button label="New integration" variant="primary" isDisabled={!!newReason || unavailable} tooltip={unavailable ? 'This node runs no integration host' : newReason} onClick={() => setNew(true)} />)}
      {unavailable ? (
        <Banner
          status="info"
          title="Integrations are not available on this node"
          description="This node runs no integration host (it needs the complete SQLite runtime or the Kubernetes host and a license). The objects below are stored, but nothing runs them here."
        />
      ) : null}
      {manifests.data && !manifests.data.licensed ? (
        <Banner
          status="warning"
          title="The license does not cover integrations"
          description="Existing integrations are paused and make no provider request; you can still change or remove them. A new Integration needs a license that covers integrations."
          endContent={<Link href="/license">View license</Link>}
        />
      ) : null}
      {manifests.data?.driver === 'kube' && manifests.data.namespace ? (
        <Text type="supporting">On Kubernetes only Integrations in the namespace {manifests.data.namespace} run; others are refused on their object.</Text>
      ) : null}
      {objects.error ? <ErrorBanner error={objects.error} /> : null}
      {summaries.error && !unavailable ? <ErrorBanner error={summaries.error} title="Status unavailable" /> : null}
      {objects.isLoading ? <Loading /> : null}
      {objects.data && !rows.length ? <Banner status="info" title="No integrations yet" description="Create one with New integration, write one to the objects directory, or apply one with kudurru apply." /> : null}
      {rows.length ? (
        <Card>
          <Table<Row> aria-label="Integrations" density="compact" verticalAlign="top" hasHover idKey={(r) => r.key} data={rows} columns={columns} plugins={{ rowAttrs }} />
        </Card>
      ) : null}
      {manifests.data ? <IntegrationWizardDialog isOpen={isNew && !newReason && !unavailable} manifests={manifests.data} onClose={() => setNew(false)} /> : null}
    </Page>
  );
}
