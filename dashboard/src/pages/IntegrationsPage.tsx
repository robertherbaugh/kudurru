// Integrations (/integrations): every Integration object of this node
// (GET /objects/integrations) with the host's status of its exact revision
// (GET /integrations?namespace=), rows linking to the instance's page. "New
// integration" opens the manifest-driven wizard (?new=1). The page asks for
// nothing without integrations.read; a node without the integration host
// (503) says so instead of showing an empty list. Rows the API may delete
// (not managed by a file, Kubernetes or the system, not already terminating)
// can be selected and deleted together with POST /integrations:batchDelete,
// one request per namespace, one at a time, after a confirmation that names
// each instance and its deletion policy, as on the other list pages.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Card } from '@astryxdesign/core/Card';
import { Link } from '@astryxdesign/core/Link';
import { List, ListItem } from '@astryxdesign/core/List';
import { HStack, StackItem, VStack } from '@astryxdesign/core/Stack';
import { pixel, proportional, Table, useTableSelection, useTableSelectionState, type TableColumn, type TablePlugin } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useMemo, useState, type ReactNode } from 'react';
import { ApiError } from '../api/client';
import { useApi, useCannotReason } from '../api/context';
import type { Integration, IntegrationSummary } from '../api/types';
import { ErrorBanner, Loading, useNotify } from '../components/Feedback';
import { FormDialog } from '../components/FormDialog';
import { ManagedBadge } from '../components/ManagedBadge';
import { Page, PageHeader } from '../components/layout/Page';
import { IntegrationWizardDialog } from '../components/wizards/IntegrationWizard';
import { formatRelative } from '../lib/format';
import { integrationHref, manifestFor, phaseVariant, REASON_TEXT } from '../lib/integrations';
import { MANAGED_BY, objectOwnership } from '../lib/managedBy';
import { useNewParam } from '../lib/router';
import { INTEGRATIONS_KEY, useCanReadIntegrations, useIntegrationManifests, useIntegrationObjects, useIntegrationSummaries } from '../lib/useIntegrations';

type Row = { obj: Integration; summary: IntegrationSummary | undefined; key: string } & Record<string, unknown>;

const rowAttrs: TablePlugin<Row> = {
  transformBodyRow: (props, r) => ({ ...props, htmlProps: { ...props.htmlProps, ...({ 'data-testid': `integration-${r.key}` } as Record<string, string>) } }),
};

/**
 * The phase the object's own Ready condition records for its current
 * generation (the host writes it), or undefined. On SQLite the host reports
 * its live status for the revision it captured. Since D384 it follows its
 * own status write, but only once it has captured the object again (at most
 * one capture interval, two seconds by default, and never for a revision it
 * skipped between two captures): until then GET /integrations answers
 * statusKnown false for a healthy instance, so the list falls back to the
 * written condition. On Kubernetes the server reads the same condition, so
 * the fallback changes nothing there.
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

/** Whether the API may delete an instance from the list: its manager allows it and it is not already terminating. */
export function batchDeletable(obj: Integration): boolean {
  return objectOwnership(obj).editable && !obj.metadata.deletionTimestamp;
}

/** Groups "namespace/name" keys by namespace, in order (one :batchDelete per namespace). */
export function byNamespace(keys: readonly string[]): Map<string, string[]> {
  const out = new Map<string, string[]>();
  for (const k of keys) {
    const i = k.indexOf('/');
    const ns = k.slice(0, i);
    out.set(ns, [...(out.get(ns) ?? []), k.slice(i + 1)]);
  }
  return out;
}

/** Whether an error is the 503 of a node that has no integration host. */
export function hostUnavailable(err: unknown): boolean {
  return err instanceof ApiError && err.status === 503 && err.key === 'unavailable';
}

export default function IntegrationsPage() {
  const api = useApi();
  const qc = useQueryClient();
  const notify = useNotify();
  const readable = useCanReadIntegrations();
  const cannot = useCannotReason();
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [confirm, setConfirm] = useState(false);
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
  const { selectionConfig } = useTableSelectionState({ data: rows, idKey: 'key', selectedKeys: selected, setSelectedKeys: setSelected, getIsItemEnabled: (r) => batchDeletable(r.obj) });
  const selection = useTableSelection({ ...selectionConfig, getRowLabel: (r: Row) => r.key });
  const chosen = rows.filter((r) => selected.has(r.key));

  const batchDelete = useMutation({
    mutationFn: async (keys: string[]) => {
      // One request at a time: on SQLite a request with several names keeps
      // its place only while no other change is committed.
      const success: string[] = [];
      const errors: { item: string; error: string }[] = [];
      for (const [ns, names] of byNamespace(keys)) {
        const r = await api.integrations.batchDelete(ns, names);
        success.push(...r.processed.success.map((x) => x.item));
        errors.push(...r.processed.errors);
      }
      return { success, errors };
    },
    onSuccess: (r) => {
      if (r.success.length) notify.success(`Deleted ${r.success.length} ${r.success.length === 1 ? 'integration' : 'integrations'}; one whose deletion policy is Delete stays terminating until its cleanup finishes`);
      for (const e of r.errors) notify.error(new Error(`${e.item}: ${e.error}`));
      setSelected(new Set());
      setConfirm(false);
      void qc.invalidateQueries({ queryKey: INTEGRATIONS_KEY });
    },
    onError: (err) => {
      notify.error(err, 'Batch delete failed');
      setConfirm(false);
      void qc.invalidateQueries({ queryKey: INTEGRATIONS_KEY });
    },
  });

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
  const deleteReason = cannot('integrations', 'delete');
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
      {chosen.length > 0 ? (
        <HStack gap={2} vAlign="center">
          <StackItem size="fill" />
          <Button label={`Delete ${chosen.length} selected`} variant="destructive" isDisabled={!!deleteReason} tooltip={deleteReason} onClick={() => setConfirm(true)} />
        </HStack>
      ) : null}
      {rows.length ? (
        <Card>
          <Table<Row> aria-label="Integrations" density="compact" verticalAlign="top" hasHover idKey="key" data={rows} columns={columns} plugins={{ selection, rowAttrs }} />
        </Card>
      ) : null}
      <FormDialog
        isOpen={confirm}
        onClose={() => setConfirm(false)}
        title={`Delete ${chosen.length} ${chosen.length === 1 ? 'integration' : 'integrations'}?`}
        width={520}
        footer={
          <>
            <Button label="Cancel" variant="ghost" onClick={() => setConfirm(false)} />
            <Button label="Delete" variant="destructive" isLoading={batchDelete.isPending} isDisabled={!!deleteReason || !chosen.length} onClick={() => batchDelete.mutate(chosen.map((r) => r.key))} />
          </>
        }
      >
        <List density="compact" listStyle="disc">
          {chosen.map((r) => {
            const policy = r.summary?.deletionPolicy ?? r.obj.spec.deletionPolicy ?? 'Retain';
            return <ListItem key={r.key} label={r.key} description={policy === 'Delete' ? 'Delete: stays terminating while the elected writer removes what it provably owns at the provider' : 'Retain: removed at once; what it published stays at the provider'} />;
          })}
        </List>
        <Text type="supporting" as="p">
          Each is deleted as it is now, without comparing the revision you see. This cannot be undone.
        </Text>
      </FormDialog>
      {manifests.data ? <IntegrationWizardDialog isOpen={isNew && !newReason && !unavailable} manifests={manifests.data} onClose={() => setNew(false)} /> : null}
    </Page>
  );
}
