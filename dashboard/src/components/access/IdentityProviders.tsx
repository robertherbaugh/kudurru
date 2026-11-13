// "Identity providers" on the Identity tab, with the reviewed commands of
// the complete SQLite runtime (D396): create, replace and delete
// IdentityProvider objects. The client secret is never entered or shown:
// spec.clientSecretRef names an environment variable or a file on every
// node (credentials.allowedEnvironment / credentials.allowedFiles), and the
// server refuses a value. A replace sends the whole object: the fields this
// form does not edit (claim mappings and rules, provisioning, allowed
// domains, audiences, labels) are carried over from the reviewed read.
// Providers of kudurru.yaml have no object and are changed there.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Code } from '@astryxdesign/core/Code';
import { Link } from '@astryxdesign/core/Link';
import { HStack } from '@astryxdesign/core/Stack';
import { proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useQuery } from '@tanstack/react-query';
import { useState } from 'react';
import { useApi, useCan } from '../../api/context';
import { useFeature } from '../../api/hooks';
import type { IamIdentityProvider, IdentityProviderDocument, IdentityProviderSpec } from '../../api/types';
import { FLOOR_LIMIT_TEXT, labelError, managerOf, operatorAnnotations } from '../../lib/iam';
import { ErrorBanner, useNotify } from '../Feedback';
import { EnumSelect, type EnumOption } from '../fields/EnumSelect';
import { cleanList, StringListField } from '../fields/StringListField';
import { TextField } from '../fields/TextField';
import { FormDialog } from '../FormDialog';
import { Panel } from '../layout/Page';
import { useIamCommand, useIamObjectReason, useReviewRead } from './iamCommand';

type Row = IamIdentityProvider & Record<string, unknown>;
const API = 'kudurru.io/v1alpha1';

const SECRET_SOURCES: EnumOption[] = [
  { value: 'env', label: 'Environment variable', help: 'A variable that credentials.allowedEnvironment allows, set on every node.' },
  { value: 'file', label: 'File', help: 'An absolute path that credentials.allowedFiles allows, present on every node.' },
];

export interface ProviderDraft {
  id: string;
  displayName: string;
  issuer: string;
  clientID: string;
  cliClientID: string;
  redirectURL: string;
  scopes: string[];
  secretSource: 'env' | 'file';
  secretName: string;
}

const EMPTY: ProviderDraft = { id: '', displayName: '', issuer: '', clientID: '', cliClientID: '', redirectURL: '', scopes: ['openid', 'email', 'profile'], secretSource: 'env', secretName: '' };

function draftOf(doc: IdentityProviderDocument): ProviderDraft {
  const ref = doc.spec.clientSecretRef;
  return {
    id: doc.metadata.name ?? '',
    displayName: doc.spec.displayName ?? '',
    issuer: doc.spec.issuer,
    clientID: doc.spec.clientID,
    cliClientID: doc.spec.cliClientID ?? '',
    redirectURL: doc.spec.redirectURL ?? '',
    scopes: doc.spec.scopes ?? [],
    secretSource: ref.file ? 'file' : 'env',
    secretName: ref.file ?? ref.env ?? '',
  };
}

/** The first problem of each field, empty when the draft can be sent. */
export function providerErrors(d: ProviderDraft, creating: boolean): Partial<Record<keyof ProviderDraft, string>> {
  const out: Partial<Record<keyof ProviderDraft, string>> = {};
  if (creating) {
    const e = labelError(d.id.trim(), 'provider id');
    if (e) out.id = e;
  }
  if (!/^https:\/\/[^\s/]+/.test(d.issuer.trim())) out.issuer = 'An https:// issuer URL, e.g. https://dex.example.com';
  if (!d.clientID.trim()) out.clientID = 'Enter the client ID';
  if (d.redirectURL.trim() && !/^https?:\/\/[^\s/]+/.test(d.redirectURL.trim())) out.redirectURL = 'A URL ending in /api/v1/auth/oidc/<id>/callback, or empty for the default';
  if (d.secretSource === 'env' && !/^[A-Za-z_][A-Za-z0-9_]*$/.test(d.secretName.trim())) out.secretName = 'An environment variable name such as DEX_CLIENT_SECRET';
  if (d.secretSource === 'file' && !d.secretName.trim().startsWith('/')) out.secretName = 'An absolute file path such as /run/secrets/dex-client-secret';
  return out;
}

/** The spec a draft sends; `base` carries the fields the form does not edit. */
export function providerSpec(d: ProviderDraft, base?: IdentityProviderSpec): IdentityProviderSpec {
  const spec: IdentityProviderSpec = { ...(base ?? {}), issuer: d.issuer.trim(), clientID: d.clientID.trim(), clientSecretRef: d.secretSource === 'file' ? { file: d.secretName.trim() } : { env: d.secretName.trim() } };
  const opt = (k: 'displayName' | 'cliClientID' | 'redirectURL', v: string) => {
    if (v.trim()) spec[k] = v.trim();
    else delete spec[k];
  };
  opt('displayName', d.displayName);
  opt('cliClientID', d.cliClientID);
  opt('redirectURL', d.redirectURL);
  const scopes = cleanList(d.scopes);
  if (scopes.length) spec.scopes = scopes;
  else delete spec.scopes;
  return spec;
}

export function IdentityProvidersPanel() {
  const api = useApi();
  const can = useCan();
  const reason = useIamObjectReason();
  const oidc = useFeature('oidc');
  const idps = useQuery({ queryKey: ['iam', 'idp'], queryFn: () => api.iam.identityProviders(), enabled: can('identityproviders', 'read') });
  const [creating, setCreating] = useState(false);
  const [selected, setSelected] = useState<{ p: IamIdentityProvider; action: 'edit' | 'delete' } | null>(null);
  const writeReason = reason('identityproviders', 'write');
  const deleteReason = reason('identityproviders', 'delete');
  const columns: TableColumn<Row>[] = [
    {
      key: 'id',
      header: 'Provider',
      width: proportional(1.5, { minWidth: 160 }),
      renderCell: (p) => (
        <HStack gap={1} vAlign="center" wrap="wrap">
          <Text weight="semibold">{p.displayName || p.id}</Text>
          <Text type="code" size="sm" color="secondary">
            {p.id}
          </Text>
        </HStack>
      ),
    },
    {
      key: 'issuer',
      header: 'Issuer',
      width: proportional(2, { minWidth: 160 }),
      renderCell: (p) => (
        <Text type="code" size="sm">
          {p.issuer}
        </Text>
      ),
    },
    {
      key: 'clientID',
      header: 'Client ID',
      width: proportional(1, { minWidth: 120 }),
      renderCell: (p) => (
        <Text type="code" size="sm">
          {p.clientID}
        </Text>
      ),
    },
    { key: 'source', header: 'Source', width: proportional(1, { minWidth: 100 }), renderCell: (p) => <Text size="sm">{p.source === 'config' ? 'kudurru.yaml' : (p.source ?? '–')}</Text> },
    {
      key: 'actions',
      header: 'Actions',
      width: proportional(1.5, { minWidth: 180 }),
      renderCell: (p) => {
        const row = p.source === 'config' ? 'Declared in kudurru.yaml: change it there' : undefined;
        const w = row ?? writeReason;
        const d = row ?? deleteReason;
        return (
          <HStack gap={1} wrap="wrap">
            <Button label={`Edit ${p.id}`} size="sm" variant="ghost" isDisabled={!!w} tooltip={w} onClick={() => setSelected({ p: { ...p }, action: 'edit' })}>
              Edit…
            </Button>
            <Button label={`Delete ${p.id}`} size="sm" variant="ghost" isDisabled={!!d} tooltip={d} onClick={() => setSelected({ p: { ...p }, action: 'delete' })}>
              Delete…
            </Button>
          </HStack>
        );
      },
    },
  ];
  return (
    <Panel
      title="Identity providers"
      headingId="iam-idp"
      actions={<Button label="New identity provider" variant="secondary" size="sm" isDisabled={!!writeReason} tooltip={writeReason} onClick={() => setCreating(true)} />}
      description="OpenID Connect providers for single sign-on. A provider shows on the sign-in page after its discovery succeeds and the license allows single sign-on."
    >
      {idps.error ? <ErrorBanner error={idps.error} /> : null}
      {!oidc.allowed && idps.data?.items.length ? (
        <Banner
          data-testid="oidc-locked"
          status="warning"
          title="Single sign-on needs a license key"
          description="Without one these identity providers are configured but switched off. Local sign-in keeps working."
          endContent={<Link href="/license">View license</Link>}
        />
      ) : null}
      {idps.data?.diagnoses?.length ? <Banner status="warning" title="Some entries were ignored" description={idps.data.diagnoses.join('; ')} /> : null}
      {idps.data?.items.length ? (
        <Table<Row> aria-label="Identity providers" density="compact" idKey="id" data={idps.data.items as Row[]} columns={columns} />
      ) : (
        <Text type="supporting" as="p">
          {idps.isLoading ? 'Loading…' : 'No identity providers.'}
        </Text>
      )}
      {creating ? <ProviderDialog onClose={() => setCreating(false)} /> : null}
      {selected ? <ReviewedProviderDialog id={selected.p.id} action={selected.action} onClose={() => setSelected(null)} /> : null}
    </Panel>
  );
}

function ReviewedProviderDialog({ id, action, onClose }: { id: string; action: 'edit' | 'delete'; onClose: () => void }) {
  const api = useApi();
  const review = useReviewRead((signal) => api.iam.identityProvider(id, signal));
  const title = action === 'edit' ? `Edit identity provider ${id}` : `Delete identity provider ${id}?`;
  if (!review.data) {
    return (
      <FormDialog isOpen onClose={onClose} title={title} width={520} footer={<Button label="Close" variant="ghost" onClick={onClose} />}>
        {review.error ? <ErrorBanner error={review.error} title="The provider could not be read" /> : <Text>Reading the provider…</Text>}
      </FormDialog>
    );
  }
  const manager = managerOf(review.data.metadata);
  if (manager !== 'api') {
    return (
      <FormDialog isOpen onClose={onClose} title={title} width={520} footer={<Button label="Close" variant="ghost" onClick={onClose} />}>
        <Banner status="info" title="Managed elsewhere" description={`This provider belongs to ${manager || 'no API manager'}. Change it where it is declared; the dashboard changes and deletes only providers it created through the API.`} />
      </FormDialog>
    );
  }
  return action === 'edit' ? <ProviderDialog doc={review.data} onClose={onClose} /> : <DeleteProviderDialog doc={review.data} title={title} onClose={onClose} />;
}

const ENDS_SESSIONS = 'Changing or deleting a provider ends every session and token that came from it.';

/** Create (no doc) or replace (doc: the reviewed read). */
function ProviderDialog({ doc, onClose }: { doc?: IdentityProviderDocument; onClose: () => void }) {
  const api = useApi();
  const notify = useNotify();
  const cmd = useIamCommand('identity provider', { retryAfterRefusal: !doc });
  const [d, setD] = useState<ProviderDraft>(doc ? draftOf(doc) : EMPTY);
  const [touched, setTouched] = useState(false);
  const errors = providerErrors(d, !doc);
  const shown = (k: keyof ProviderDraft) => (touched ? errors[k] : undefined);
  const update = (patch: Partial<ProviderDraft>) => setD((prev) => ({ ...prev, ...patch }));
  const id = doc?.metadata.name ?? d.id.trim();
  const submit = async () => {
    setTouched(true);
    if (Object.keys(errors).length) return;
    const r = await cmd.run((signal) => {
      if (!doc) return api.iam.createIdentityProvider({ apiVersion: API, kind: 'IdentityProvider', metadata: { name: id }, spec: providerSpec(d) }, signal);
      const annotations = operatorAnnotations(doc.metadata);
      return api.iam.replaceIdentityProvider(
        id,
        {
          uid: doc.metadata.uid ?? '',
          resourceVersion: doc.metadata.resourceVersion ?? '',
          identityProvider: { apiVersion: API, kind: 'IdentityProvider', metadata: { name: id, ...(doc.metadata.labels ? { labels: doc.metadata.labels } : {}), ...(annotations ? { annotations } : {}) }, spec: providerSpec(d, doc.spec) },
        },
        signal,
      );
    });
    if (r.ok) {
      notify.success(`${doc ? 'Updated' : 'Created'} identity provider ${id}`);
      onClose();
    }
  };
  return (
    <FormDialog
      isOpen
      onClose={onClose}
      title={doc ? `Edit identity provider ${id}` : 'New identity provider'}
      width={600}
      onSubmit={() => void submit()}
      footer={
        <>
          <Button label={cmd.locked ? 'Close' : 'Cancel'} variant="ghost" onClick={onClose} />
          <Button type="submit" label={doc ? 'Save changes' : 'Create provider'} variant="primary" isLoading={cmd.pending} isDisabled={cmd.locked} />
        </>
      }
    >
      {doc ? (
        <Text as="p" type="supporting">
          UID <Code size="inherit">{doc.metadata.uid}</Code>, resourceVersion <Code size="inherit">{doc.metadata.resourceVersion}</Code>, read just now. Claim mappings, claim rules, provisioning, allowed domains and audiences are kept as stored.
        </Text>
      ) : (
        <TextField label="Provider ID" value={d.id} onChange={(v) => update({ id: v })} mono isRequired placeholder="dex" error={shown('id')} help="A DNS label. It appears in the callback URL and in user names; an id of kudurru.yaml cannot be used." />
      )}
      <TextField label="Display name" value={d.displayName} onChange={(v) => update({ displayName: v })} placeholder="Corporate SSO" help="The label of the sign-in button." />
      <TextField label="Issuer" value={d.issuer} onChange={(v) => update({ issuer: v })} mono isRequired placeholder="https://dex.example.com" error={shown('issuer')} />
      <TextField label="Client ID" value={d.clientID} onChange={(v) => update({ clientID: v })} mono isRequired error={shown('clientID')} />
      <EnumSelect label="Client secret from" value={d.secretSource} options={SECRET_SOURCES} onChange={(v) => update({ secretSource: v as 'env' | 'file', secretName: '' })} />
      <TextField
        label={d.secretSource === 'file' ? 'Secret file path' : 'Secret environment variable'}
        value={d.secretName}
        onChange={(v) => update({ secretName: v })}
        mono
        isRequired
        autoComplete="off"
        placeholder={d.secretSource === 'file' ? '/run/secrets/dex-client-secret' : 'DEX_CLIENT_SECRET'}
        error={shown('secretName')}
        help="A reference, never the secret itself: kudurru reads the value on each node and never returns or logs it."
      />
      <TextField label="CLI client ID" value={d.cliClientID} onChange={(v) => update({ cliClientID: v })} mono help="Optional: a public client for kudurru login --provider." />
      <TextField label="Redirect URL" value={d.redirectURL} onChange={(v) => update({ redirectURL: v })} mono error={shown('redirectURL')} help="Optional: empty uses this server’s /api/v1/auth/oidc/<id>/callback." />
      <StringListField label="Scopes" value={d.scopes} onChange={(v) => update({ scopes: v })} addLabel="Add scope" />
      {doc ? <Banner status="warning" title="Saving signs out this provider’s users" description={`${ENDS_SESSIONS} ${FLOOR_LIMIT_TEXT}`} /> : null}
      {cmd.outcome}
    </FormDialog>
  );
}

function DeleteProviderDialog({ doc, title, onClose }: { doc: IdentityProviderDocument; title: string; onClose: () => void }) {
  const api = useApi();
  const notify = useNotify();
  const cmd = useIamCommand('identity provider');
  const id = doc.metadata.name ?? '';
  const submit = async () => {
    const r = await cmd.run((signal) => api.iam.deleteIdentityProvider(id, { uid: doc.metadata.uid ?? '', resourceVersion: doc.metadata.resourceVersion ?? '' }, signal));
    if (r.ok) {
      notify.success(`Deleted identity provider ${id}`);
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
          <Button type="submit" label="Delete provider" variant="destructive" isLoading={cmd.pending} isDisabled={cmd.locked} />
        </>
      }
    >
      <Text as="p" type="supporting">
        UID <Code size="inherit">{doc.metadata.uid}</Code>, resourceVersion <Code size="inherit">{doc.metadata.resourceVersion}</Code>, read just now.
      </Text>
      <Text as="p">The provider leaves the sign-in page. {ENDS_SESSIONS}</Text>
      <Banner status="warning" title="Permanent revocation record" description={`${FLOOR_LIMIT_TEXT}`} />
      {cmd.outcome}
    </FormDialog>
  );
}
