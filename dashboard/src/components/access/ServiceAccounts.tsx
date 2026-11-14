// "Service accounts" on the Identity tab, with the reviewed commands of the
// complete SQLite runtime (D396): create, edit (description, disabled),
// delete, and issue a token. Every replace, delete and token issuance first
// reads the account (GET /iam/serviceaccounts/{namespace}/{name}) and sends
// that read's exact UID (and resourceVersion), once. An issued token's
// secret exists only in the 201 answer: the dialog shows it once, keeps it
// only in its own state (never in a query or mutation cache, never logged)
// and drops it when it closes. Accounts of other managers (managedBy other
// than api in the list) have their change actions disabled with the reason,
// the dialog says so again after its fresh read, and the server refuses them.
// "Tokens…" lists the account's tokens (GET /auth/tokens?all=true, needs
// tokens.read) and revokes one (DELETE /auth/tokens/{id}, tokens.delete)
// once per decision, like every other IAM command.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Code } from '@astryxdesign/core/Code';
import { CodeBlock } from '@astryxdesign/core/CodeBlock';
import { HStack } from '@astryxdesign/core/Stack';
import { pixel, proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { useApi, useCan, useCannotReason } from '../../api/context';
import type { ApiToken, CreatedToken, IamServiceAccount, ServiceAccountDocument } from '../../api/types';
import { formatDateTime, formatRelative } from '../../lib/format';
import { accountManagerReason, FLOOR_LIMIT_TEXT, labelError, managerOf, operatorAnnotations, subdomainError } from '../../lib/iam';
import { ROLES, SCOPE_KINDS, scopeString } from '../../lib/rbac';
import { zoneGroupError } from '../../lib/zones';
import { ErrorBanner, useNotify } from '../Feedback';
import { DurationField, durationPresets } from '../fields/DurationField';
import { EnumSelect } from '../fields/EnumSelect';
import { ObjectRefPicker } from '../fields/ObjectRefPicker';
import { SwitchField, TextAreaField, TextField } from '../fields/TextField';
import { FormDialog } from '../FormDialog';
import { Panel } from '../layout/Page';
import { useIamCommand, useIamObjectReason, useReviewRead } from './iamCommand';

type Row = IamServiceAccount & Record<string, unknown>;
type Action = 'edit' | 'token' | 'delete' | 'tokens';

const TOKEN_LIFETIMES = durationPresets(['24h', '168h', '720h', '2160h', '8760h']);
const API = 'kudurru.io/v1alpha1';

const saName = (s: { namespace?: string | undefined; name?: string | undefined }) => `${s.namespace ?? ''}/${s.name ?? ''}`;

export function ServiceAccountsPanel() {
  const api = useApi();
  const can = useCan();
  const cannot = useCannotReason();
  const reason = useIamObjectReason();
  const sas = useQuery({ queryKey: ['iam', 'sa'], queryFn: () => api.iam.serviceAccounts(), enabled: can('serviceaccounts', 'read') });
  const [creating, setCreating] = useState(false);
  const [selected, setSelected] = useState<{ sa: IamServiceAccount; action: Action } | null>(null);
  const writeReason = reason('serviceaccounts', 'write');
  const deleteReason = reason('serviceaccounts', 'delete');
  const tokenReason = writeReason ?? reason('tokens', 'write');
  const columns: TableColumn<Row>[] = [
    {
      key: 'name',
      header: 'Service account',
      width: proportional(2, { minWidth: 200 }),
      renderCell: (s) => (
        <HStack gap={1} vAlign="center" wrap="wrap">
          <Text type="code">{saName(s)}</Text>
          {s.builtinAdmin ? <Badge variant="blue" label="admin" /> : null}
          {s.disabled ? <Badge variant="warning" label="disabled" /> : null}
        </HStack>
      ),
    },
    { key: 'description', header: 'Description', width: proportional(2, { minWidth: 160 }), renderCell: (s) => <Text size="sm">{s.description || '–'}</Text> },
    { key: 'tokens', header: 'Tokens', align: 'end', width: pixel(80), renderCell: (s) => <Text hasTabularNumbers>{s.tokens}</Text> },
    {
      key: 'actions',
      header: 'Actions',
      width: proportional(2, { minWidth: 260 }),
      renderCell: (s) => {
        const row = s.builtinAdmin ? 'The bootstrap account is not changed or given tokens here' : accountManagerReason(s.managedBy);
        const list = cannot('tokens', 'read');
        const w = row ?? writeReason;
        const t = row ?? (s.disabled ? 'Enable the account before issuing a token' : tokenReason);
        const d = row ?? deleteReason;
        const choose = (action: Action) => setSelected({ sa: { ...s }, action });
        return (
          <HStack gap={1} wrap="wrap">
            <Button label={`Edit ${saName(s)}`} size="sm" variant="ghost" isDisabled={!!w} tooltip={w} onClick={() => choose('edit')}>
              Edit…
            </Button>
            <Button label={`New token for ${saName(s)}`} size="sm" variant="ghost" isDisabled={!!t} tooltip={t} onClick={() => choose('token')}>
              New token…
            </Button>
            <Button label={`Tokens of ${saName(s)}`} size="sm" variant="ghost" isDisabled={!!list} tooltip={list} onClick={() => choose('tokens')}>
              Tokens…
            </Button>
            <Button label={`Delete ${saName(s)}`} size="sm" variant="ghost" isDisabled={!!d} tooltip={d} onClick={() => choose('delete')}>
              Delete…
            </Button>
          </HStack>
        );
      },
    },
  ];
  return (
    <Panel
      title="Service accounts"
      headingId="iam-sa"
      actions={<Button label="New service account" variant="secondary" size="sm" isDisabled={!!writeReason} tooltip={writeReason} onClick={() => setCreating(true)} />}
      description="Accounts for programs. They have no password and sign in with API tokens; a role binding with a ServiceAccount subject gives them a role."
    >
      {sas.error ? <ErrorBanner error={sas.error} /> : null}
      {sas.data?.items.length ? (
        <Table<Row> aria-label="Service accounts" density="compact" idKey={(s) => saName(s)} data={sas.data.items as Row[]} columns={columns} />
      ) : (
        <Text type="supporting" as="p">
          {sas.isLoading ? 'Loading…' : 'No service accounts.'}
        </Text>
      )}
      {creating ? <CreateServiceAccountDialog onClose={() => setCreating(false)} /> : null}
      {selected?.action === 'tokens' ? <ServiceAccountTokensDialog sa={selected.sa} onClose={() => setSelected(null)} /> : null}
      {selected && selected.action !== 'tokens' ? <ReviewedServiceAccountDialog sa={selected.sa} action={selected.action} onClose={() => setSelected(null)} /> : null}
    </Panel>
  );
}

function CreateServiceAccountDialog({ onClose }: { onClose: () => void }) {
  const api = useApi();
  const notify = useNotify();
  const cmd = useIamCommand('service account', { retryAfterRefusal: true });
  const [namespace, setNamespace] = useState('default');
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [touched, setTouched] = useState(false);
  const nsErr = labelError(namespace.trim(), 'namespace');
  const nameErr = subdomainError(name.trim());
  const descErr = description.length > 2048 ? 'At most 2048 characters' : undefined;
  const submit = async () => {
    setTouched(true);
    if (nsErr || nameErr || descErr) return;
    const r = await cmd.run((signal) =>
      api.iam.createServiceAccount({ apiVersion: API, kind: 'ServiceAccount', metadata: { namespace: namespace.trim(), name: name.trim() }, spec: description.trim() ? { description: description.trim() } : {} }, signal),
    );
    if (r.ok) {
      notify.success(`Created service account ${saName(r.value.metadata)}`);
      onClose();
    }
  };
  return (
    <FormDialog
      isOpen
      onClose={onClose}
      title="New service account"
      width={480}
      onSubmit={() => void submit()}
      footer={
        <>
          <Button label={cmd.locked ? 'Close' : 'Cancel'} variant="ghost" onClick={onClose} />
          <Button type="submit" label="Create service account" variant="primary" isLoading={cmd.pending} isDisabled={cmd.locked} />
        </>
      }
    >
      <TextField label="Namespace" value={namespace} onChange={setNamespace} mono isRequired error={touched ? (nsErr ?? undefined) : undefined} />
      <TextField label="Name" value={name} onChange={setName} mono isRequired placeholder="backup-robot" error={touched ? (nameErr ?? undefined) : undefined} />
      <TextAreaField label="Description" value={description} onChange={setDescription} rows={2} error={descErr} />
      <Text type="supporting" as="p">
        No token is issued with the account: use New token… on its row afterwards.
      </Text>
      {cmd.outcome}
    </FormDialog>
  );
}

type TokenRow = ApiToken & Record<string, unknown>;

/** Whether a token belongs to this service account (owner kind, namespace and name). */
export function ownedBy(t: ApiToken, sa: Pick<IamServiceAccount, 'namespace' | 'name'>): boolean {
  return t.owner.kind === 'ServiceAccount' && t.owner.namespace === sa.namespace && t.owner.name === sa.name;
}

/**
 * The account's tokens and their revocation. The list is GET
 * /auth/tokens?all=true filtered to the account; Revoke asks first in the
 * dialog (no second dialog), then sends one DELETE for that token ID. A
 * refusal or an unknown outcome locks the dialog: close it, read the list
 * again and decide again.
 */
function ServiceAccountTokensDialog({ sa, onClose }: { sa: IamServiceAccount; onClose: () => void }) {
  const api = useApi();
  const notify = useNotify();
  const cannot = useCannotReason();
  const revokeReason = cannot('tokens', 'delete');
  const tokens = useQuery({ queryKey: ['iam', 'tokens', 'all'], queryFn: ({ signal }) => api.auth.allTokens(signal) });
  const [confirm, setConfirm] = useState<ApiToken | null>(null);
  const cmd = useIamCommand('token revocation', { floor: false });
  const rows = (tokens.data?.items ?? []).filter((t) => ownedBy(t, sa));
  const revoke = async (t: ApiToken) => {
    const r = await cmd.run((signal) => api.auth.deleteToken(t.id, signal));
    if (r.ok) {
      notify.success(`Revoked token ${t.description || t.id} of ${saName(sa)}`);
      setConfirm(null);
    }
  };
  const columns: TableColumn<TokenRow>[] = [
    {
      key: 'description',
      header: 'Token',
      width: proportional(2, { minWidth: 160 }),
      renderCell: (t) => (
        <Text type="inherit">
          {t.description ? <Text type="inherit">{t.description}</Text> : <Text color="secondary">(no description)</Text>}{' '}
          <Text type="code" size="sm" color="secondary">
            kdr_{t.id}_…
          </Text>
        </Text>
      ),
    },
    { key: 'scopes', header: 'Limited to', width: proportional(1.5, { minWidth: 110 }), renderCell: (t) => <Text size="sm">{t.scopes?.length ? t.scopes.join(', ') : 'the account’s bindings'}</Text> },
    { key: 'expiresAt', header: 'Expires', width: proportional(1, { minWidth: 100 }), renderCell: (t) => <Text size="sm">{t.expiresAt ? formatDateTime(t.expiresAt) : 'never'}</Text> },
    { key: 'lastUsedAt', header: 'Last used', width: proportional(1, { minWidth: 100 }), renderCell: (t) => <Text size="sm">{t.lastUsedAt ? formatRelative(t.lastUsedAt) : 'never'}</Text> },
    {
      key: 'actions',
      header: 'Actions',
      width: pixel(110),
      renderCell: (t) => (
        <Button label={`Revoke ${t.description || t.id}`} size="sm" variant="ghost" isDisabled={!!revokeReason || cmd.locked || cmd.pending} tooltip={revokeReason} onClick={() => setConfirm(t)}>
          Revoke…
        </Button>
      ),
    },
  ];
  return (
    <FormDialog
      isOpen
      onClose={onClose}
      title={`Tokens of ${saName(sa)}`}
      width={760}
      onSubmit={() => {
        if (confirm) void revoke(confirm);
      }}
      footer={
        confirm && !cmd.locked ? (
          <>
            <Button label="Keep it" variant="ghost" onClick={() => setConfirm(null)} isDisabled={cmd.pending} />
            <Button type="submit" label="Revoke token" variant="destructive" isLoading={cmd.pending} />
          </>
        ) : (
          <Button label="Close" variant="ghost" onClick={onClose} />
        )
      }
    >
      {tokens.error ? <ErrorBanner error={tokens.error} title="The tokens could not be read" /> : null}
      {tokens.data && !rows.length ? <Text type="supporting" as="p">This account has no tokens.</Text> : null}
      {rows.length ? <Table<TokenRow> aria-label={`Tokens of ${saName(sa)}`} density="compact" idKey="id" data={rows as TokenRow[]} columns={columns} /> : null}
      {tokens.isLoading ? <Text>Reading the tokens…</Text> : null}
      {confirm && !cmd.locked ? (
        <Banner
          status="warning"
          title={`Revoke token ${confirm.description || confirm.id}?`}
          description={`Programs that use kdr_${confirm.id}_… are refused from the next request on. A revoked token cannot be restored; issue a new one with New token… instead.`}
        />
      ) : null}
      {cmd.outcome}
    </FormDialog>
  );
}

/** Edit, delete or token issuance: the account read again first, then one command bound to that read. */
function ReviewedServiceAccountDialog({ sa, action, onClose }: { sa: IamServiceAccount; action: Action; onClose: () => void }) {
  const api = useApi();
  const review = useReviewRead((signal) => api.iam.serviceAccount(sa.namespace, sa.name, signal));
  const title = action === 'edit' ? `Edit ${saName(sa)}` : action === 'token' ? `New token for ${saName(sa)}` : `Delete ${saName(sa)}?`;
  if (!review.data) {
    return (
      <FormDialog isOpen onClose={onClose} title={title} width={520} footer={<Button label="Close" variant="ghost" onClick={onClose} />}>
        {review.error ? <ErrorBanner error={review.error} title="The account could not be read" /> : <Text>Reading the account…</Text>}
      </FormDialog>
    );
  }
  const doc = review.data;
  const manager = managerOf(doc.metadata);
  if (manager !== 'api') {
    return (
      <FormDialog isOpen onClose={onClose} title={title} width={520} footer={<Button label="Close" variant="ghost" onClick={onClose} />}>
        <Banner status="info" title="Managed elsewhere" description={`This account belongs to ${manager || 'no API manager'}. Change it where it is declared; the dashboard changes, deletes and issues tokens only for accounts it created through the API.`} />
      </FormDialog>
    );
  }
  if (action === 'edit') return <EditServiceAccountDialog doc={doc} title={title} onClose={onClose} />;
  if (action === 'token') return <TokenDialog doc={doc} title={title} onClose={onClose} />;
  return <DeleteServiceAccountDialog doc={doc} title={title} onClose={onClose} />;
}

function Reviewed({ doc }: { doc: ServiceAccountDocument }) {
  return (
    <Text as="p" type="supporting">
      UID <Code size="inherit">{doc.metadata.uid}</Code>, resourceVersion <Code size="inherit">{doc.metadata.resourceVersion}</Code>, read just now.
    </Text>
  );
}

function EditServiceAccountDialog({ doc, title, onClose }: { doc: ServiceAccountDocument; title: string; onClose: () => void }) {
  const api = useApi();
  const notify = useNotify();
  const cmd = useIamCommand('service account');
  const [description, setDescription] = useState(doc.spec.description ?? '');
  const [disabled, setDisabled] = useState(!!doc.spec.disabled);
  const changed = description !== (doc.spec.description ?? '') || disabled !== !!doc.spec.disabled;
  const disabling = disabled && !doc.spec.disabled;
  const descErr = description.length > 2048 ? 'At most 2048 characters' : undefined;
  const submit = async () => {
    if (!changed || descErr) return;
    const annotations = operatorAnnotations(doc.metadata);
    const r = await cmd.run((signal) =>
      api.iam.replaceServiceAccount(
        doc.metadata.namespace ?? '',
        doc.metadata.name ?? '',
        {
          uid: doc.metadata.uid ?? '',
          resourceVersion: doc.metadata.resourceVersion ?? '',
          serviceAccount: {
            apiVersion: API,
            kind: 'ServiceAccount',
            metadata: { namespace: doc.metadata.namespace ?? '', name: doc.metadata.name ?? '', ...(doc.metadata.labels ? { labels: doc.metadata.labels } : {}), ...(annotations ? { annotations } : {}) },
            spec: { ...(description ? { description } : {}), ...(disabled ? { disabled: true } : {}) },
          },
        },
        signal,
      ),
    );
    if (r.ok) {
      notify.success(`Updated service account ${saName(doc.metadata)}`);
      onClose();
    }
  };
  return (
    <FormDialog
      isOpen
      onClose={onClose}
      title={title}
      width={520}
      onSubmit={() => void submit()}
      footer={
        <>
          <Button label={cmd.locked ? 'Close' : 'Cancel'} variant="ghost" onClick={onClose} />
          <Button type="submit" label="Save changes" variant={disabling ? 'destructive' : 'primary'} isLoading={cmd.pending} isDisabled={!changed || cmd.locked} />
        </>
      }
    >
      <Reviewed doc={doc} />
      <TextAreaField label="Description" value={description} onChange={setDescription} rows={2} error={descErr} />
      <SwitchField label="Disabled" value={disabled} onChange={setDisabled} help="A disabled account’s tokens are refused." />
      {disabling ? <Banner status="warning" title="Disabling deletes every token of the account" description={`They are deleted in the same commit, and enabling it again restores none. ${FLOOR_LIMIT_TEXT}`} /> : null}
      {cmd.outcome}
    </FormDialog>
  );
}

function DeleteServiceAccountDialog({ doc, title, onClose }: { doc: ServiceAccountDocument; title: string; onClose: () => void }) {
  const api = useApi();
  const notify = useNotify();
  const cmd = useIamCommand('service account');
  const submit = async () => {
    const r = await cmd.run((signal) => api.iam.deleteServiceAccount(doc.metadata.namespace ?? '', doc.metadata.name ?? '', { uid: doc.metadata.uid ?? '', resourceVersion: doc.metadata.resourceVersion ?? '' }, signal));
    if (r.ok) {
      notify.success(`Deleted service account ${saName(doc.metadata)}`);
      onClose();
    }
  };
  return (
    <FormDialog
      isOpen
      onClose={onClose}
      title={title}
      width={480}
      onSubmit={() => void submit()}
      footer={
        <>
          <Button label={cmd.locked ? 'Close' : 'Cancel'} variant="ghost" onClick={onClose} />
          <Button type="submit" label="Delete service account" variant="destructive" isLoading={cmd.pending} isDisabled={cmd.locked} />
        </>
      }
    >
      <Reviewed doc={doc} />
      <Text as="p">The account and all of its tokens are deleted in one commit, and the tokens stay refused after a restart. A later account with the same name gets a new UID and inherits nothing. Role bindings that name it stay until you delete them.</Text>
      <Banner status="warning" title="Permanent revocation record" description={`${FLOOR_LIMIT_TEXT}`} />
      {cmd.outcome}
    </FormDialog>
  );
}

/**
 * Issues one token for the reviewed account. The answer's secret lives
 * only in `created`, this component's state: it is shown once and dropped
 * with the dialog.
 */
function TokenDialog({ doc, title, onClose }: { doc: ServiceAccountDocument; title: string; onClose: () => void }) {
  const api = useApi();
  const cmd = useIamCommand('token');
  const [description, setDescription] = useState('');
  const [expires, setExpires] = useState('720h');
  const [role, setRole] = useState('');
  const [scopeKind, setScopeKind] = useState('cluster');
  const [scopeZone, setScopeZone] = useState('');
  const [created, setCreated] = useState<CreatedToken | null>(null);
  const scope = role ? scopeString(role, scopeKind, scopeZone) : '';
  const scopeErr = role && scopeKind === 'zonegroup' ? zoneGroupError(scopeZone) : role && scopeKind === 'zone' && !scopeZone ? 'Pick a zone' : null;
  const submit = async () => {
    if (created || scopeErr) return;
    const r = await cmd.run((signal) =>
      api.iam.createServiceAccountToken(
        doc.metadata.namespace ?? '',
        doc.metadata.name ?? '',
        { uid: doc.metadata.uid ?? '', ...(description.trim() ? { description: description.trim() } : {}), ...(expires.trim() ? { expiresIn: expires.trim() } : {}), ...(scope ? { scopes: [scope] } : {}) },
        signal,
      ),
    );
    if (r.ok) setCreated(r.value);
  };
  if (created) {
    return (
      <FormDialog isOpen onClose={onClose} isDirty title={title} width={560} footer={<Button label="Done" variant="primary" onClick={onClose} />}>
        <Banner status="success" title={`Token ${created.description || created.id} created`} description="Copy it now; it is not shown again. kudurru keeps only its hash, and this dialog forgets it when it closes." collapsible={false}>
          <CodeBlock data-testid="new-sa-token" code={created.token} width="100%" />
        </Banner>
        <Text type="supporting" as="p">
          {created.expiresAt ? `It expires ${new Date(created.expiresAt).toLocaleString()}.` : 'It does not expire.'} Use it as <Code size="inherit">Authorization: Bearer kdr_…</Code>. Tokens… on the account’s row lists and revokes its tokens.
        </Text>
      </FormDialog>
    );
  }
  return (
    <FormDialog
      isOpen
      onClose={onClose}
      title={title}
      width={560}
      onSubmit={() => void submit()}
      footer={
        <>
          <Button label={cmd.locked ? 'Close' : 'Cancel'} variant="ghost" onClick={onClose} />
          <Button type="submit" label="Issue token" variant="primary" isLoading={cmd.pending} isDisabled={cmd.locked || !!scopeErr} />
        </>
      }
    >
      <Reviewed doc={doc} />
      <TextField label="Description" value={description} onChange={setDescription} placeholder="nightly backup" />
      <DurationField label="Expires in" value={expires} onChange={setExpires} presets={TOKEN_LIFETIMES} emptyLabel="Server maximum" />
      <EnumSelect label="Limit to role" value={role} options={ROLES} onChange={setRole} emptyLabel="The account’s bindings" />
      {role ? (
        <EnumSelect
          label="For"
          value={scopeKind}
          options={SCOPE_KINDS}
          onChange={(k) => {
            setScopeKind(k);
            setScopeZone('');
          }}
        />
      ) : null}
      {role && scopeKind === 'zone' ? <ObjectRefPicker kind="dnszones" label="Zone" value={scopeZone ? [scopeZone] : []} onChange={(v) => setScopeZone(v[0] ?? '')} /> : null}
      {role && scopeKind === 'zonegroup' ? <TextField label="Zone group" value={scopeZone} onChange={setScopeZone} mono placeholder="prod" error={scopeZone ? (scopeErr ?? undefined) : undefined} /> : null}
      <Text type="supporting" as="p">
        The token acts as the account with its role bindings, or narrower with a role limit. It expires no later than the API token you are signed in with, and within auth.tokens.maxLifetime. Issuing needs serviceaccounts.write and tokens.write. The secret is shown once in this dialog.
      </Text>
      {cmd.outcome}
    </FormDialog>
  );
}
