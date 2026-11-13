// One Integration (/integrations/:ns/:name): its state from the host for
// the exact stored revision, the object's status, the reviewed commands
// and its configuration. Every command is bound to the UID and revision on
// screen (IntegrationReview): check; preview, whose digest a publishing
// sync must send back (the server refuses an expired or stale one with
// 409, shown as such); an observing sync; the provider's advertised
// actions; and, while the object is terminating, the release without
// cleanup (D367). An outcome the provider did not acknowledge is shown as
// uncertain and is never sent again. Configuration edits are whole-object
// PUTs bound to the stored resourceVersion (D400); credentials stay
// references.
import { AlertDialog } from '@astryxdesign/core/AlertDialog';
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { CodeBlock } from '@astryxdesign/core/CodeBlock';
import { Link } from '@astryxdesign/core/Link';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { pixel, proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useQueryClient } from '@tanstack/react-query';
import { useState, type ReactNode } from 'react';
import { ApiError } from '../api/client';
import { useApi, useCan, useCannotReason, usePrimaryOnly, useStaticMode } from '../api/context';
import type {
  Condition,
  Integration,
  IntegrationAction,
  IntegrationExecutionResult,
  IntegrationManifest,
  IntegrationManifests,
  IntegrationPlanOperation,
  IntegrationPreview,
  IntegrationRecord,
  IntegrationSummary,
} from '../api/types';
import type { PageProps } from '../routes';
import { errorText, ErrorBanner, Loading, useNotify } from '../components/Feedback';
import { EditableSection } from '../components/EditableSection';
import { ShapeField } from '../components/fields/ShapeField';
import { CredentialFields, SettingsFields } from '../components/integrations/IntegrationFields';
import { ManagedBadge } from '../components/ManagedBadge';
import { Page, PageHeader, Panel } from '../components/layout/Page';
import { formatDateTime, formatExact, formatRelative } from '../lib/format';
import {
  buildIntegration,
  commandErrorText,
  configError,
  credentialsError,
  describeRef,
  draftOf,
  humanize,
  manifestFor,
  needsSecrets,
  publishes,
  REASON_TEXT,
  schemaShape,
  type IntegrationCommand,
  type IntegrationDraft,
} from '../lib/integrations';
import { MANAGED_BY, objectOwnership, readOnlyReason } from '../lib/managedBy';
import { fromDraft, toDraft } from '../lib/shapes';
import { useNavigate } from '../lib/router';
import { toYAML } from '../lib/yaml';
import { INTEGRATIONS_KEY, useCanReadIntegrations, useIntegrationManifests, useIntegrationObject, useIntegrationSummary } from '../lib/useIntegrations';
import { hostUnavailable, IntegrationState } from './IntegrationsPage';

const LICENSE_REASON = 'Needs a license that covers integrations';
const TERMINATING_REASON = 'Refused while the Integration is terminating';

export default function IntegrationDetailPage({ params }: PageProps) {
  const namespace = params.ns ?? 'default';
  const name = params.name ?? '';
  const readable = useCanReadIntegrations();
  const manifests = useIntegrationManifests();
  const object = useIntegrationObject(namespace, name);
  const summary = useIntegrationSummary(namespace, name);
  const back = { href: '/integrations', label: 'Integrations' };

  if (!readable) {
    return (
      <Page>
        <PageHeader title={name} back={back} />
        <Banner status="warning" title="Not permitted" description="Viewing integrations needs integrations.read." />
      </Page>
    );
  }
  if (object.error instanceof ApiError && object.error.status === 404) {
    return (
      <Page>
        <PageHeader title={name} back={back} />
        <Banner status="info" title="This integration does not exist" description={`There is no Integration ${namespace}/${name}; it may have been deleted or released.`} />
      </Page>
    );
  }
  if (object.error) {
    return (
      <Page>
        <PageHeader title={name} back={back} />
        <ErrorBanner error={object.error} />
      </Page>
    );
  }
  if (!object.data) {
    return (
      <Page>
        <PageHeader title={name} back={back} />
        <Loading />
      </Page>
    );
  }
  return <IntegrationView obj={object.data} summary={summary.data} summaryError={summary.error} manifests={manifests.data} manifestsError={manifests.error} />;
}

interface ViewProps {
  obj: Integration;
  summary: IntegrationSummary | undefined;
  summaryError: unknown;
  manifests: IntegrationManifests | undefined;
  manifestsError: unknown;
}

function IntegrationView({ obj, summary, summaryError, manifests, manifestsError }: ViewProps) {
  const api = useApi();
  const qc = useQueryClient();
  const notify = useNotify();
  const navigate = useNavigate();
  const can = useCan();
  const cannot = useCannotReason();
  const primaryOnly = usePrimaryOnly();
  const staticMode = useStaticMode();
  const namespace = obj.metadata.namespace ?? 'default';
  const name = obj.metadata.name;
  const manifest = manifestFor(manifests, obj.spec.type);
  const own = objectOwnership(obj);
  const terminating = summary?.terminating === true || !!obj.metadata.deletionTimestamp;
  const unavailable = hostUnavailable(summaryError) || hostUnavailable(manifestsError);
  const licensed = manifests?.licensed !== false;
  // The review every command is bound to: the host's status of the stored revision.
  const review = summary ? { uid: summary.uid, revision: summary.revision } : undefined;

  const [deleting, setDeleting] = useState(false);
  const [deleteBusy, setDeleteBusy] = useState(false);
  const [releasing, setReleasing] = useState(false);
  const [releaseBusy, setReleaseBusy] = useState(false);
  const [releaseError, setReleaseError] = useState<string | null>(null);

  const refresh = () => qc.invalidateQueries({ queryKey: INTEGRATIONS_KEY });

  const deleteReason = cannot('integrations', 'delete') ?? (!own.editable ? readOnlyReason(own) : undefined);
  const releaseReason = cannot('integrations', 'release') ?? (staticMode ? 'Read-only in static mode' : undefined) ?? primaryOnly('releaseIntegration');

  const remove = async () => {
    setDeleteBusy(true);
    try {
      await api.integrations.remove(namespace, name, obj.metadata.resourceVersion);
      setDeleting(false);
      await refresh();
      if (obj.spec.deletionPolicy === 'Delete') {
        notify.success(`Deleting ${name}: it stays terminating until its cleanup finishes`);
      } else {
        notify.success(`Deleted integration ${name}`);
        navigate('/integrations');
      }
    } catch (err) {
      setDeleting(false);
      notify.error(err, 'Could not delete the integration');
    } finally {
      setDeleteBusy(false);
    }
  };

  const release = async () => {
    if (!review) return;
    setReleaseBusy(true);
    setReleaseError(null);
    try {
      const r = await api.integrations.release(namespace, name, review);
      setReleasing(false);
      const kept = r.ledgerRows < 0 ? 'more ledger rows than one command counts' : `${formatExact(r.ledgerRows)} ledger rows and ${formatExact(r.operations)} operations`;
      notify.success(`Released ${name} without cleanup; retained ${kept}`);
      await refresh();
      navigate('/integrations');
    } catch (err) {
      setReleaseError(commandErrorText(err, 'release') ?? errorText(err));
    } finally {
      setReleaseBusy(false);
    }
  };

  const meta = (
    <>
      <IntegrationState summary={summary} />
      <ManagedBadge owner={obj.metadata.annotations?.[MANAGED_BY]} />
      {obj.spec.suspend ? <Badge variant="neutral" label="Suspended" /> : null}
      <Text type="supporting">{manifest?.displayName ?? obj.spec.type}</Text>
    </>
  );

  return (
    <Page>
      <PageHeader
        title={name}
        back={{ href: '/integrations', label: 'Integrations' }}
        meta={meta}
        description={`${namespace}/${name}`}
        actions={
          <Button
            label="Delete"
            variant="destructive"
            isDisabled={!!deleteReason || (terminating && !!obj.metadata.deletionTimestamp)}
            tooltip={deleteReason ?? (terminating ? 'Already being deleted' : undefined)}
            onClick={() => setDeleting(true)}
          />
        }
      />
      {unavailable ? <Banner status="info" title="Integrations are not available on this node" description="This node runs no integration host, so this Integration does not run here and its commands are refused." /> : null}
      {summaryError && !unavailable ? <ErrorBanner error={summaryError} title="Status unavailable" /> : null}
      {!licensed ? (
        <Banner status="warning" title="The license does not cover integrations" description="This instance is paused and makes no provider request; you can still change, delete or release it." endContent={<Link href="/license">View license</Link>} />
      ) : null}
      {manifests && !manifest ? <Banner status="warning" title="Unknown provider" description={`This build has no provider of type ${obj.spec.type}. The object is kept as it is; delete or release it, or run a build that has the provider.`} /> : null}
      {manifest && !manifest.supported ? <Banner status="warning" title="Not run on this node" description={REASON_TEXT[manifest.reason ?? ''] ?? manifest.reason ?? 'This node refuses this provider.'} /> : null}

      {terminating ? (
        <Panel title="Being deleted" data-testid="terminating">
          <Text>
            {obj.spec.deletionPolicy === 'Delete' || summary?.deletionPolicy === 'Delete'
              ? 'The elected writer removes the records this incarnation provably owns at the provider, then its local state, then the object. Check, preview, sync and actions are refused meanwhile.'
              : 'The object is held by a finalizer until it is released.'}
          </Text>
          <MetadataList>
            <MetadataListItem label="Requested">{summary?.deletionRequestedAt ? formatDateTime(summary.deletionRequestedAt) : obj.metadata.deletionTimestamp ? formatDateTime(obj.metadata.deletionTimestamp) : 'unknown'}</MetadataListItem>
            <MetadataListItem label="Reason">{summary?.statusKnown ? (REASON_TEXT[summary.reason] ?? summary.reason) : 'No status for this revision yet'}</MetadataListItem>
            <MetadataListItem label="Records left">{formatExact(summary?.remaining ?? 0)}</MetadataListItem>
            <MetadataListItem label="Blocking conflicts">{formatExact(summary?.blocked ?? 0)}</MetadataListItem>
            <MetadataListItem label="Uncertain writes">{formatExact(summary?.uncertain ?? 0)}</MetadataListItem>
          </MetadataList>
          <HStack gap={2}>
            <Button label="Release without cleanup" variant="secondary" isDisabled={!!releaseReason || !review} tooltip={releaseReason ?? (!review ? 'Waiting for the status of this revision' : undefined)} onClick={() => setReleasing(true)} />
          </HStack>
          {releaseError ? <Banner status="error" title="Not released" description={releaseError} /> : null}
        </Panel>
      ) : null}

      <StatusPanel obj={obj} summary={summary} />

      {manifest && !terminating ? (
        <CommandsPanel obj={obj} summary={summary} manifest={manifest} licenseReason={!licensed ? LICENSE_REASON : unavailable ? 'This node runs no integration host' : !manifest.supported ? 'This node does not run this provider' : undefined} />
      ) : null}
      {manifest && terminating ? (
        <Panel title="Commands">
          <Text type="supporting">{TERMINATING_REASON}.</Text>
        </Panel>
      ) : null}

      {manifest && manifests ? (
        <ConfigurationSection obj={obj} manifest={manifest} manifests={manifests} terminating={terminating} canWrite={can('integrations', 'write')} writeReason={cannot('integrations', 'write')} canSecrets={can('integrations', 'secrets')} />
      ) : null}

      <AlertDialog
        isOpen={deleting}
        onOpenChange={(open) => !open && setDeleting(false)}
        title={`Delete ${name}?`}
        description={
          obj.spec.deletionPolicy === 'Delete'
            ? 'Its deletion policy is Delete: the Integration stays terminating while the elected writer removes the records it provably owns at the provider and its local state, then it is removed.'
            : 'Its deletion policy is Retain: the Integration is removed at once and what it published stays at the provider.'
        }
        actionLabel="Delete integration"
        onAction={() => void remove()}
        isActionLoading={deleteBusy}
      />
      <AlertDialog
        isOpen={releasing}
        onOpenChange={(open) => !open && setReleasing(false)}
        title={`Release ${name} without cleanup?`}
        description="The object is removed without contacting the provider. What it published stays at the provider and its ledger stays in the store, as with Retain. A delete batch already sent may still complete."
        actionLabel="Release without cleanup"
        onAction={() => void release()}
        isActionLoading={releaseBusy}
      />
    </Page>
  );
}

function StatusPanel({ obj, summary }: { obj: Integration; summary: IntegrationSummary | undefined }) {
  const st = obj.status;
  const conditions = st?.conditions ?? [];
  const columns: TableColumn<Condition & Record<string, unknown>>[] = [
    { key: 'type', header: 'Condition', width: pixel(140), renderCell: (c) => <Text>{c.type}</Text> },
    { key: 'status', header: 'Status', width: pixel(90), renderCell: (c) => <Text>{c.status}</Text> },
    { key: 'reason', header: 'Reason', width: proportional(1, { minWidth: 120 }), renderCell: (c) => <Text type="supporting">{c.reason ?? ''}</Text> },
    { key: 'message', header: 'Message', width: proportional(2, { minWidth: 160 }), renderCell: (c) => <Text type="supporting">{c.message ?? ''}</Text> },
  ];
  return (
    <Panel title="Status" data-testid="integration-status">
      <MetadataList columns="multi">
        <MetadataListItem label="Phase">{summary ? (summary.statusKnown ? summary.phase : 'No status for this revision yet') : 'unknown'}</MetadataListItem>
        <MetadataListItem label="Reason">{summary?.reason ? (REASON_TEXT[summary.reason] ?? summary.reason) : 'none'}</MetadataListItem>
        <MetadataListItem label="Lease held here">{summary ? (summary.leader ? 'yes' : 'no') : 'unknown'}</MetadataListItem>
        <MetadataListItem label="Last sync">{summary?.lastSync ? `${formatRelative(summary.lastSync)} (${formatDateTime(summary.lastSync)})` : 'never'}</MetadataListItem>
        <MetadataListItem label="Records managed">{formatExact(st?.records?.managed ?? 0)}</MetadataListItem>
        <MetadataListItem label="Drift">{formatExact(st?.records?.drift ?? 0)}</MetadataListItem>
        <MetadataListItem label="Conflicts">{formatExact(st?.records?.conflicts ?? 0)}</MetadataListItem>
        <MetadataListItem label="Clients">{st?.clients ? <Link href="/clients">{formatExact(st.clients)}</Link> : '0'}</MetadataListItem>
        <MetadataListItem label="Revision">
          <Text type="code">{obj.metadata.resourceVersion ?? ''}</Text>
        </MetadataListItem>
      </MetadataList>
      {conditions.length ? <Table aria-label="Conditions" density="compact" verticalAlign="top" idKey={(c) => c.type} data={conditions as (Condition & Record<string, unknown>)[]} columns={columns} /> : null}
    </Panel>
  );
}

// ---------------------------------------------------------------------------
// Reviewed commands

interface CommandsProps {
  obj: Integration;
  summary: IntegrationSummary | undefined;
  manifest: IntegrationManifest;
  /** Set when nothing can run (no license, no host). */
  licenseReason: string | undefined;
}

interface Outcome {
  cmd: IntegrationCommand;
  ok?: IntegrationExecutionResult;
  error?: string;
  uncertain?: boolean;
}

function CommandsPanel({ obj, summary, manifest, licenseReason }: CommandsProps) {
  const api = useApi();
  const qc = useQueryClient();
  const cannot = useCannotReason();
  const primaryOnly = usePrimaryOnly();
  const namespace = obj.metadata.namespace ?? 'default';
  const name = obj.metadata.name;
  const review = summary ? { uid: summary.uid, revision: summary.revision } : undefined;
  const reviewKey = review ? `${review.uid}@${review.revision}` : '';
  const publisher = publishes(manifest, obj.spec);
  const [preview, setPreview] = useState<{ key: string; plan: IntegrationPreview } | null>(null);
  const [outcome, setOutcome] = useState<(Outcome & { key: string }) | null>(null);
  // A sync whose outcome is unknown is never sent again for the same review.
  const [uncertainKey, setUncertainKey] = useState<string | null>(null);
  const [busy, setBusy] = useState<IntegrationCommand | null>(null);
  const currentPreview = preview && preview.key === reviewKey ? preview.plan : null;
  // The last outcome stays in view after the review it ran on changed (a
  // refusal of a stale review rereads the status, which changes the key).
  const shown = outcome;

  const noReview = !review ? 'Waiting for the status of this revision' : undefined;
  const checkReason = licenseReason ?? cannot('integrations', 'check') ?? noReview;
  const previewReason = licenseReason ?? cannot('integrations', 'read') ?? primaryOnly('previewIntegration') ?? noReview;
  const syncBase = licenseReason ?? cannot('integrations', 'sync') ?? noReview;
  const syncReason =
    syncBase ??
    (uncertainKey === reviewKey ? 'The last sync of this revision has an unknown outcome and is not sent again; check the provider, then preview again' : undefined) ??
    (publisher && !currentPreview ? 'Preview first: a publishing sync applies exactly the reviewed plan' : undefined);

  const run = async (cmd: IntegrationCommand, call: () => Promise<IntegrationExecutionResult>, after?: (r: IntegrationExecutionResult) => void) => {
    setBusy(cmd);
    try {
      const r = await call();
      setOutcome({ key: reviewKey, cmd, ok: r });
      after?.(r);
    } catch (err) {
      const uncertain = err instanceof ApiError && err.key === 'integration_uncertain';
      setOutcome({ key: reviewKey, cmd, error: commandErrorText(err, cmd) ?? errorText(err), uncertain });
      if (cmd === 'sync') {
        // A refused or uncertain sync consumed the reviewed plan.
        setPreview(null);
        if (uncertain) setUncertainKey(reviewKey);
      }
    } finally {
      setBusy(null);
      void qc.invalidateQueries({ queryKey: INTEGRATIONS_KEY });
    }
  };

  const check = () => review && run('check', () => api.integrations.check(namespace, name, review));
  const doPreview = () =>
    review &&
    run(
      'preview',
      () => api.integrations.preview(namespace, name, review),
      (r) => {
        if (r.preview) setPreview({ key: reviewKey, plan: r.preview });
        setUncertainKey(null);
      },
    );
  const sync = () => {
    if (!review) return;
    const body = publisher && currentPreview ? { ...review, mode: 'publish' as const, previewDigest: currentPreview.digest } : { ...review, mode: 'observe' as const };
    void run('sync', () => api.integrations.sync(namespace, name, body), () => setPreview(null));
  };

  return (
    <Panel
      title="Commands"
      description={
        publisher
          ? 'Check proves the endpoint and the credential. Preview computes the plan this instance would apply; Publish applies exactly that plan, identified by its digest, or nothing.'
          : 'Check proves the endpoint and the credential. Sync runs the instance once now and reports what it observed.'
      }
      data-testid="integration-commands"
    >
      <HStack gap={2} wrap="wrap">
        <Button label="Check" variant="secondary" isLoading={busy === 'check'} isDisabled={!!checkReason || busy !== null} tooltip={checkReason} onClick={() => void check()} />
        {publisher ? <Button label="Preview" variant="secondary" isLoading={busy === 'preview'} isDisabled={!!previewReason || busy !== null} tooltip={previewReason} onClick={() => void doPreview()} /> : null}
        <Button
          label={publisher ? 'Publish this plan' : 'Sync now'}
          variant="primary"
          isLoading={busy === 'sync'}
          isDisabled={!!syncReason || busy !== null}
          tooltip={syncReason}
          onClick={sync}
        />
      </HStack>
      {currentPreview ? <PreviewPlan plan={currentPreview} /> : null}
      {shown ? <OutcomeBanner outcome={shown} /> : null}
      {manifest.capabilities.includes('Actions') ? <ActionsSection obj={obj} review={review} licenseReason={licenseReason} /> : null}
    </Panel>
  );
}

function OutcomeBanner({ outcome }: { outcome: Outcome }) {
  const what = { check: 'Check', preview: 'Preview', sync: 'Sync', act: 'Action', actions: 'Actions', release: 'Release' }[outcome.cmd];
  if (outcome.error) {
    return <Banner status={outcome.uncertain ? 'warning' : 'error'} title={outcome.uncertain ? `${what}: outcome unknown` : `${what} refused`} description={outcome.error} data-testid="command-outcome" />;
  }
  const r = outcome.ok;
  if (!r) return null;
  let description: string;
  switch (outcome.cmd) {
    case 'check':
      description = `Reachable: ${r.reachable ? 'yes' : 'no'}. Credential accepted: ${r.authenticated ? 'yes' : 'no'}.`;
      break;
    case 'preview':
      description = r.preview ? `${formatExact(r.preview.operations.length)} changes, ${formatExact(r.preview.conflicts.length)} conflicts, ${formatExact(r.preview.unchanged)} unchanged.` : 'The provider returned no plan.';
      break;
    default:
      description = `Applied ${formatExact(r.applied)}, unchanged ${formatExact(r.unchanged)}, pending ${formatExact(r.pending)}, conflicts ${formatExact(r.conflicts)}${r.clients ? `, clients ${formatExact(r.clients)}` : ''}.${r.sourceCommitted ? ' Inventory committed.' : ''}`;
  }
  return <Banner status="success" title={`${what} completed`} description={description} data-testid="command-outcome" />;
}

function recordText(r: IntegrationRecord | undefined): string {
  return r ? `${r.type} ${r.targets.join(', ')} (TTL ${r.ttl})` : '';
}

type OpRow = IntegrationPlanOperation & { id: string } & Record<string, unknown>;

/** The reviewed plan of a preview and the digest a publishing sync sends back. */
function PreviewPlan({ plan }: { plan: IntegrationPreview }) {
  const rows: OpRow[] = plan.operations.map((o, i) => ({ ...o, id: `${i}` }));
  const columns: TableColumn<OpRow>[] = [
    { key: 'action', header: 'Change', width: pixel(100), renderCell: (o) => <Text>{o.action}</Text> },
    { key: 'name', header: 'Name', width: proportional(1.4, { minWidth: 160 }), renderCell: (o) => <Text type="code">{`${o.name} ${o.type}`}</Text> },
    { key: 'before', header: 'Before', width: proportional(1.2, { minWidth: 120 }), renderCell: (o) => <Text type="supporting">{recordText(o.before)}</Text> },
    { key: 'after', header: 'After', width: proportional(1.2, { minWidth: 120 }), renderCell: (o) => <Text type="supporting">{recordText(o.after)}</Text> },
    { key: 'domains', header: 'Destination', width: proportional(1, { minWidth: 120 }), renderCell: (o) => <Text type="supporting">{`${o.domains.join(', ')} (${o.policy})`}</Text> },
  ];
  return (
    <VStack gap={2} data-testid="integration-preview">
      <Text>
        Plan digest <Text type="code">{plan.digest}</Text>: {formatExact(plan.operations.length)} changes, {formatExact(plan.unchanged)} unchanged.
      </Text>
      {rows.length ? <Table<OpRow> aria-label="Planned changes" density="compact" verticalAlign="top" idKey={(o) => o.id} data={rows} columns={columns} /> : <Text type="supporting">Nothing to change.</Text>}
      {plan.conflicts.length ? (
        <Banner
          status="warning"
          title={`${formatExact(plan.conflicts.length)} records are left alone`}
          description={plan.conflicts.map((c) => `${c.name} ${c.type}: ${c.reason}`).join('; ')}
        />
      ) : null}
    </VStack>
  );
}

function ActionsSection({ obj, review, licenseReason }: { obj: Integration; review: { uid: string; revision: string } | undefined; licenseReason: string | undefined }) {
  const api = useApi();
  const cannot = useCannotReason();
  const primaryOnly = usePrimaryOnly();
  const namespace = obj.metadata.namespace ?? 'default';
  const name = obj.metadata.name;
  const [actions, setActions] = useState<{ key: string; items: IntegrationAction[] } | null>(null);
  const [params, setParams] = useState<Record<string, unknown>>({});
  const [busy, setBusy] = useState<string | null>(null);
  const [result, setResult] = useState<{ ok: boolean; uncertain?: boolean; text: string } | null>(null);
  const key = review ? `${review.uid}@${review.revision}` : '';
  const listed = actions && actions.key === key ? actions.items : null;
  // The actions read runs against the provider on the primary, like act.
  const listReason = licenseReason ?? cannot('integrations', 'read') ?? primaryOnly('invokeIntegrationAction') ?? (!review ? 'Waiting for the status of this revision' : undefined);
  const actReason = licenseReason ?? cannot('integrations', 'act');

  const load = async () => {
    if (!review) return;
    setBusy('*');
    setResult(null);
    try {
      const r = await api.integrations.actions(namespace, name, review);
      setActions({ key, items: r.actions ?? [] });
    } catch (err) {
      setResult({ ok: false, text: commandErrorText(err, 'actions') ?? errorText(err) });
    } finally {
      setBusy(null);
    }
  };
  const invoke = async (a: IntegrationAction, shape: ReturnType<typeof schemaShape>) => {
    if (!review) return;
    setBusy(a.name);
    setResult(null);
    try {
      const value = shape ? fromDraft(shape, params[a.name] ?? toDraft(shape, {})) : {};
      await api.integrations.act(namespace, name, a.name, { ...review, params: value ?? {} });
      setResult({ ok: true, text: `${a.name} completed.` });
    } catch (err) {
      setResult({ ok: false, uncertain: err instanceof ApiError && err.key === 'integration_uncertain', text: commandErrorText(err, 'act') ?? errorText(err) });
    } finally {
      setBusy(null);
    }
  };

  return (
    <VStack gap={2} data-testid="integration-actions">
      <HStack gap={2} vAlign="center">
        <Text weight="semibold">Actions</Text>
        <Button label="Load actions" size="sm" variant="secondary" isLoading={busy === '*'} isDisabled={!!listReason || busy !== null} tooltip={listReason} onClick={() => void load()} />
      </HStack>
      {listed && !listed.length ? <Text type="supporting">The provider advertises no actions.</Text> : null}
      {(listed ?? []).map((a) => {
        const shape = a.params && typeof a.params === 'object' ? schemaShape(a.params, `params.${a.name}`, new Set()) : null;
        return (
          <VStack key={a.name} gap={1}>
            {shape ? <ShapeField label={`${humanize(a.name)} parameters`} shape={shape} value={params[a.name] ?? toDraft(shape, {})} onChange={(v) => setParams({ ...params, [a.name]: v })} /> : null}
            <HStack gap={2}>
              <Button label={`Run ${a.name}`} size="sm" variant="secondary" isLoading={busy === a.name} isDisabled={!!actReason || busy !== null} tooltip={actReason} onClick={() => void invoke(a, shape)} />
            </HStack>
          </VStack>
        );
      })}
      {result ? <Banner status={result.ok ? 'success' : result.uncertain ? 'warning' : 'error'} title={result.ok ? 'Action completed' : result.uncertain ? 'Action: outcome unknown' : 'Action refused'} description={result.text} /> : null}
    </VStack>
  );
}

// ---------------------------------------------------------------------------
// Configuration

interface ConfigProps {
  obj: Integration;
  manifest: IntegrationManifest;
  manifests: IntegrationManifests;
  terminating: boolean;
  canWrite: boolean;
  writeReason: string | undefined;
  canSecrets: boolean;
}

function ConfigurationSection({ obj, manifest, manifests, terminating, canWrite, writeReason, canSecrets }: ConfigProps) {
  const api = useApi();
  const qc = useQueryClient();
  const notify = useNotify();
  const own = objectOwnership(obj);
  const bound = Object.keys(obj.spec.credentials ?? {}).length > 0;
  const readOnly = !own.editable ? readOnlyReason(own) : !canWrite ? (writeReason ?? 'Needs integrations.write') : terminating ? 'A terminating Integration is not edited; release it or wait for its cleanup' : undefined;
  const block = obj.spec[manifest.configField];

  const view: ReactNode = (
    <VStack gap={2}>
      <CodeBlock code={toYAML(block ?? {})} language="yaml" title={`spec.${manifest.configField}`} width="100%" maxHeight="20rem" />
      <MetadataList>
        {Object.keys(obj.spec.credentials ?? {}).length || manifest.credentials.length
          ? [...new Set([...manifest.credentials.map((c) => c.name), ...Object.keys(obj.spec.credentials ?? {})])].map((k) => (
              <MetadataListItem key={k} label={humanize(k)}>
                {describeRef(obj.spec.credentials?.[k])}
              </MetadataListItem>
            ))
          : null}
        <MetadataListItem label="Resync">{obj.spec.resync || 'provider default'}</MetadataListItem>
        <MetadataListItem label="When deleted">{obj.spec.deletionPolicy ?? 'Retain'}</MetadataListItem>
      </MetadataList>
    </VStack>
  );

  return (
    <EditableSection<IntegrationDraft>
      title="Configuration"
      testId="integration-config"
      value={draftOf(obj, manifest)}
      view={view}
      readOnlyReason={readOnly}
      edit={(draft, setDraft, showErrors) => (
        <VStack gap={4}>
          <SettingsFields manifest={manifest} draft={draft} onChange={setDraft} destinationLockReason={bound && !canSecrets ? 'Credentials are bound: changing where they are sent needs integrations.secrets.' : undefined} />
          <CredentialFields manifest={manifest} manifests={manifests} draft={draft} onChange={setDraft} showErrors={showErrors} disabledReason={!canSecrets ? 'Changing a credential reference needs integrations.secrets.' : undefined} />
        </VStack>
      )}
      validate={(draft) => {
        const e = configError(manifest, draft) ?? credentialsError(manifest, draft, manifests.credentialSources);
        if (e) return e;
        const next = buildIntegration(manifest, draft, obj);
        if (!canSecrets && needsSecrets(obj.spec, next.spec)) return 'This change binds or moves a credential and needs integrations.secrets.';
        return null;
      }}
      onSave={async (draft) => {
        await api.integrations.replace(buildIntegration(manifest, draft, obj));
        await qc.invalidateQueries({ queryKey: INTEGRATIONS_KEY });
        notify.success(`Saved ${obj.metadata.name}`);
      }}
    />
  );
}
