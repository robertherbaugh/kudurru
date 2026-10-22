// Access: the signed-in principal, its roles and permissions, its
// own API tokens (the kdr_ secret is shown once), the read-only IAM views
// (the write API is v0.2; IAM is declared in YAML today).
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Code } from '@astryxdesign/core/Code';
import { CodeBlock } from '@astryxdesign/core/CodeBlock';
import { Collapsible } from '@astryxdesign/core/Collapsible';
import { Link } from '@astryxdesign/core/Link';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Tab, TabList } from '@astryxdesign/core/TabList';
import { pixel, proportional, Table, TableBody, TableCell, TableHeaderCell, TableRow, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { Tooltip } from '@astryxdesign/core/Tooltip';
import { VisuallyHidden } from '@astryxdesign/core/VisuallyHidden';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useState } from 'react';
import { canAnyScope, qk, useApi, useCan, useSession } from '../api/context';
import { useFeature } from '../api/hooks';
import type { ApiToken, CreatedToken, IamIdentityProvider, IamRoleBinding, IamServiceAccount, IamUser, RbacScope } from '../api/types';
import { AuthenticatorPanel, RecoverAuthenticatorButton } from '../components/access/Authenticators';
import { SessionsPanel } from '../components/access/Sessions';
import { ChangePasswordButton, principalName } from '../components/AccountMenu';
import { ErrorBanner, useNotify } from '../components/Feedback';
import { DurationField, durationPresets } from '../components/fields/DurationField';
import { EnumSelect, type EnumOption } from '../components/fields/EnumSelect';
import { ObjectRefPicker } from '../components/fields/ObjectRefPicker';
import { TextField } from '../components/fields/TextField';
import { Page, PageHeader, Panel } from '../components/layout/Page';
import { formatDateTime, formatRelative } from '../lib/format';
import { useNavigate, useSearchParams } from '../lib/router';

/** A relative time with the exact instant in its tooltip. */
function Relative({ at }: { at: string }) {
  return (
    <Tooltip content={at}>
      <Text type="inherit">{formatRelative(at)}</Text>
    </Tooltip>
  );
}

function Me() {
  const { data } = useSession();
  if (!data) return null;
  return (
    <Panel title={`Signed in as ${principalName(data)}`} headingId="me-title" actions={<ChangePasswordButton />}>
      <Text type="supporting" color="primary" as="p">
        {data.principal.kind} <Code size="inherit">{data.principal.id}</Code>
        {data.principal.groups?.length ? `, groups ${data.principal.groups.join(', ')}` : ''}
      </Text>
      <HStack gap={2} vAlign="center" wrap="wrap" role="group" aria-label="Roles">
        {data.roles.length ? data.roles.map((r) => <Badge key={r} variant="blue" label={r} />) : <Text type="supporting">No roles reported for this session.</Text>}
      </HStack>
      <Collapsible trigger={<Text>Permissions</Text>} defaultIsOpen={false} chevronPosition="start">
        <VStack paddingBlockStart={2}>
          <Table density="compact" aria-label="Permissions">
            <TableBody>
              {Object.entries(data.permissions).map(([res, verbs]) => (
                <TableRow key={res}>
                  <TableHeaderCell scope="row">{res}</TableHeaderCell>
                  <TableCell>{(verbs ?? []).join(', ')}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </VStack>
      </Collapsible>
    </Panel>
  );
}

/** Token lifetimes offered; "" asks for the server maximum (auth.tokens.maxLifetime). */
const TOKEN_LIFETIMES = durationPresets(['24h', '168h', '720h', '2160h', '8760h']);

/** The roles of internal/rbac (the rbac.bindings role enum of the settings schema). */
export const ROLES: EnumOption[] = [
  { value: 'viewer', label: 'Viewer', help: 'Read zones, records, lists, policies, stats and settings.' },
  { value: 'auditor', label: 'Auditor', help: 'Viewer plus the query log and the audit log.' },
  { value: 'operator', label: 'Operator', help: 'Manage records, lists, policies, groups, clients and blocking; flush the cache.' },
  { value: 'zone-admin', label: 'Zone admin', help: 'Manage zones and their records, transfers and signing.' },
  { value: 'admin', label: 'Admin', help: 'Everything.' },
];

const SCOPE_KINDS: EnumOption[] = [
  { value: 'cluster', label: 'Everything', help: 'The role applies to every object.' },
  { value: 'zone', label: 'One zone', help: 'The role applies to one zone and its records.' },
];

/** The scope string of a token restricted to a role: "viewer" or "zone-admin@zone:home-arpa". */
export function scopeString(role: string, kind: string, name: string): string {
  if (!role) return '';
  return kind === 'zone' && name.trim() ? `${role}@zone:${name.trim()}` : role;
}

/** Scopes typed as "viewer, zone-admin@zone:home-example-com". */
export function parseScopes(s: string): string[] {
  return s
    .split(/[\s,]+/)
    .map((x) => x.trim())
    .filter(Boolean);
}

type TokenRow = ApiToken & Record<string, unknown>;

function Tokens() {
  const api = useApi();
  const qc = useQueryClient();
  const notify = useNotify();
  const can = useCan();
  const { data: session } = useSession();
  const [description, setDescription] = useState('');
  const [expires, setExpires] = useState('720h');
  const [role, setRole] = useState('');
  const [scopeKind, setScopeKind] = useState('cluster');
  const [scopeZone, setScopeZone] = useState('');
  const scopes = scopeString(role, scopeKind, scopeZone);
  const [created, setCreated] = useState<CreatedToken | null>(null);
  const q = useQuery({ queryKey: qk.tokens, queryFn: () => api.auth.tokens() });
  const create = useMutation({
    mutationFn: () => {
      const sc = parseScopes(scopes);
      return api.auth.createToken({
        description: description.trim(),
        ...(expires.trim() ? { expiresIn: expires.trim() } : {}),
        ...(sc.length ? { scopes: sc } : {}),
      });
    },
    onSuccess: (t) => {
      setCreated(t);
      setDescription('');
      setRole('');
      void qc.invalidateQueries({ queryKey: qk.tokens });
    },
    onError: (err) => notify.error(err, 'Could not create the token'),
  });
  const del = useMutation({
    mutationFn: (id: string) => api.auth.deleteToken(id),
    onSuccess: () => {
      notify.success('Token revoked');
      void qc.invalidateQueries({ queryKey: qk.tokens });
    },
    onError: (err) => notify.error(err, 'Could not revoke the token'),
  });
  const allowed = (!!session && canAnyScope(session, 'tokens', 'own')) || can('tokens', 'write');
  const columns: TableColumn<TokenRow>[] = [
    {
      key: 'description',
      header: 'Description',
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
    {
      key: 'scopes',
      header: 'Scopes',
      width: proportional(1.5, { minWidth: 120 }),
      renderCell: (t) =>
        t.scopes?.length ? (
          <HStack gap={1} vAlign="center" wrap="wrap">
            {t.scopes.map((s) => (
              <Badge key={s} variant="neutral" label={s} />
            ))}
          </HStack>
        ) : (
          <Text type="supporting">all your roles</Text>
        ),
    },
    { key: 'createdAt', header: 'Created', width: proportional(1.5, { minWidth: 150 }), renderCell: (t) => <Text>{formatDateTime(t.createdAt)}</Text> },
    { key: 'expiresAt', header: 'Expires', width: proportional(1, { minWidth: 90 }), renderCell: (t) => (t.expiresAt ? <Relative at={t.expiresAt} /> : <Text>never</Text>) },
    {
      key: 'lastUsedAt',
      header: 'Last used',
      width: proportional(1.5, { minWidth: 120 }),
      renderCell: (t) => (
        <Text>
          {t.lastUsedAt ? formatRelative(t.lastUsedAt) : 'never'}
          {t.lastUsedIP ? <Text color="secondary"> from {t.lastUsedIP}</Text> : null}
        </Text>
      ),
    },
    {
      key: 'actions',
      header: <VisuallyHidden>Actions</VisuallyHidden>,
      width: pixel(128),
      renderCell: (t) => <Button label="Revoke" size="sm" variant="ghost" onClick={() => del.mutate(t.id)} />,
    },
  ];
  return (
    <Panel
      title="API tokens"
      headingId="tok-title"
      description={
        <>
          Tokens act as you with your roles, or with the narrower scopes you give them. Use them as <Code size="inherit">Authorization: Bearer kdr_…</Code> or with{' '}
          <Code size="inherit">kudurru login --token</Code>.
        </>
      }
    >
      <VStack
        as="form"
        gap={2}
        onSubmit={(e) => {
          e.preventDefault();
          if (description.trim()) create.mutate();
        }}
      >
        <HStack gap={2} wrap="wrap" align="start">
          <TextField label="Description" value={description} onChange={setDescription} placeholder="ci-deploy" />
          <DurationField label="Expires in" value={expires} onChange={setExpires} presets={TOKEN_LIFETIMES} emptyLabel="Server maximum" />
          <EnumSelect label="Limit to role" value={role} options={ROLES} onChange={setRole} emptyLabel="All my roles" />
          {role ? <EnumSelect label="For" value={scopeKind} options={SCOPE_KINDS} onChange={setScopeKind} /> : null}
          {role && scopeKind === 'zone' ? (
            <ObjectRefPicker kind="dnszones" label="Zone" value={scopeZone ? [scopeZone] : []} onChange={(v) => setScopeZone(v[0] ?? '')} />
          ) : null}
        </HStack>
        <HStack gap={2}>
          <Button type="submit" label="Create token" variant="primary" isDisabled={!allowed || !description.trim()} isLoading={create.isPending} />
        </HStack>
      </VStack>
      {created ? (
        <Banner
          status="success"
          title={`Token ${created.description || created.id} created`}
          description="Copy it now; it is not shown again."
          isDismissable
          onDismiss={() => setCreated(null)}
          collapsible={false}
        >
          <CodeBlock data-testid="new-token" code={created.token} width="100%" />
        </Banner>
      ) : null}
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.data ? (
        <Table<TokenRow>
          aria-label="Your API tokens"
          density="compact"
          idKey="id"
          data={q.data.items as TokenRow[]}
          columns={columns}
          emptyState={<Text color="secondary">No tokens.</Text>}
        />
      ) : null}
    </Panel>
  );
}

/** "cluster" or "zone home-arpa". */
export function scopeText(s: RbacScope | undefined): string {
  if (!s || !s.kind || s.kind === 'cluster') return 'cluster';
  return s.name ? `${s.kind} ${s.name}` : s.kind;
}

function bindingName(b: IamRoleBinding): string {
  return b.namespace ? `${b.namespace}/${b.name}` : b.name;
}

function Diagnoses({ items }: { items: string[] | undefined }) {
  if (!items?.length) return null;
  return <Banner status="warning" title="Some entries were ignored" description={items.join('; ')} />;
}

type UserRow = IamUser & Record<string, unknown>;
type ServiceAccountRow = IamServiceAccount & Record<string, unknown>;
type BindingRow = IamRoleBinding & Record<string, unknown>;
type ProviderRow = IamIdentityProvider & Record<string, unknown>;

const userColumns: TableColumn<UserRow>[] = [
  {
    key: 'name',
    header: 'User',
    width: proportional(2, { minWidth: 180 }),
    renderCell: (u) => (
      <HStack gap={1} vAlign="center" wrap="wrap">
        <Text weight="semibold">{u.displayName || u.name}</Text>
        {u.displayName ? (
          <Text type="code" size="sm" color="secondary">
            {u.name}
          </Text>
        ) : null}
        {u.builtinAdmin ? <Badge variant="blue" label="admin" /> : null}
        {u.disabled ? <Badge variant="warning" label="disabled" /> : null}
        {u.mustChangePassword ? <Badge variant="orange" label="must change password" /> : null}
      </HStack>
    ),
  },
  { key: 'groups', header: 'Groups', width: proportional(1.5, { minWidth: 120 }), renderCell: (u) => <Text size="sm">{u.groups?.length ? u.groups.join(', ') : '–'}</Text> },
  { key: 'lastLoginAt', header: 'Last sign-in', width: proportional(1, { minWidth: 110 }), renderCell: (u) => <Text size="sm">{u.lastLoginAt ? formatRelative(u.lastLoginAt) : 'never'}</Text> },
  {
    key: 'passwordChangedAt',
    header: 'Password changed',
    width: proportional(1.5, { minWidth: 192 }),
    renderCell: (u) => <Text size="sm">{u.passwordChangedAt ? formatRelative(u.passwordChangedAt) : '–'}</Text>,
  },
];

const serviceAccountColumns: TableColumn<ServiceAccountRow>[] = [
  {
    key: 'name',
    header: 'Service account',
    width: proportional(2, { minWidth: 200 }),
    renderCell: (s) => (
      <HStack gap={1} vAlign="center" wrap="wrap">
        <Text type="code">
          {s.namespace}/{s.name}
        </Text>
        {s.builtinAdmin ? <Badge variant="blue" label="admin" /> : null}
        {s.disabled ? <Badge variant="warning" label="disabled" /> : null}
      </HStack>
    ),
  },
  { key: 'description', header: 'Description', width: proportional(2, { minWidth: 160 }), renderCell: (s) => <Text size="sm">{s.description || '–'}</Text> },
  { key: 'tokens', header: 'Tokens', align: 'end', width: pixel(96), renderCell: (s) => <Text hasTabularNumbers>{s.tokens}</Text> },
];

const bindingColumns: TableColumn<BindingRow>[] = [
  { key: 'role', header: 'Role', width: proportional(1, { minWidth: 190 }), renderCell: (b) => <Badge variant="blue" label={b.role} /> },
  { key: 'scope', header: 'Scope', width: proportional(1, { minWidth: 120 }), renderCell: (b) => <Text size="sm">{scopeText(b.scope)}</Text> },
  {
    key: 'subjects',
    header: 'Subjects',
    width: proportional(2, { minWidth: 160 }),
    renderCell: (b) => <Text size="sm">{(b.subjects ?? []).map((s) => `${s.kind} ${s.namespace ? `${s.namespace}/` : ''}${s.name}`).join(', ') || '–'}</Text>,
  },
  {
    key: 'binding',
    header: 'Binding',
    width: proportional(2, { minWidth: 160 }),
    renderCell: (b) => (
      <Text size="sm">
        <Text type="code" size="sm">
          {bindingName(b)}
        </Text>{' '}
        <Text color="secondary">({b.managedBy ? `managed by ${b.managedBy}` : b.source === 'config' ? 'kudurru.yaml' : 'object'})</Text>
      </Text>
    ),
  },
];

const providerColumns: TableColumn<ProviderRow>[] = [
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
];

function Iam() {
  const api = useApi();
  const can = useCan();
  const users = useQuery({ queryKey: ['iam', 'users'], queryFn: () => api.iam.users(), enabled: can('users', 'read') });
  const sas = useQuery({ queryKey: ['iam', 'sa'], queryFn: () => api.iam.serviceAccounts(), enabled: can('serviceaccounts', 'read') });
  const rbs = useQuery({ queryKey: ['iam', 'rb'], queryFn: () => api.iam.roleBindings(), enabled: can('rolebindings', 'read') });
  const idps = useQuery({ queryKey: ['iam', 'idp'], queryFn: () => api.iam.identityProviders(), enabled: can('identityproviders', 'read') });
  const rbac = useFeature('rbac');
  const oidc = useFeature('oidc');
  const empty = (what: string, loading: boolean) => (
    <Text type="supporting" as="p">
      {loading ? 'Loading…' : `No ${what}.`}
    </Text>
  );
  return (
    <VStack gap={4}>
      <Banner
        status="info"
        title="Identity management"
        description="Users are managed with `kudurru admin` on the host. Administrators with cluster users.write can recover a selected user’s authenticator here. Service accounts, role bindings and identity providers are declared in kudurru.yaml or as objects (objects.d or Kubernetes)."
      />
      <Panel title="Local users" headingId="iam-users">
        {users.error ? <ErrorBanner error={users.error} /> : null}
        <Diagnoses items={users.data?.diagnoses} />
        {users.data?.items.length ? (
          <Table<UserRow> aria-label="Local users" density="compact" idKey="name" data={users.data.items} columns={[...userColumns, { key: 'actions', header: 'Actions', width: proportional(1, { minWidth: 190 }), renderCell: (u) => <RecoverAuthenticatorButton user={u} /> }]} />
        ) : (
          empty('local users', users.isLoading)
        )}
      </Panel>
      <Panel title="Service accounts" headingId="iam-sa">
        {sas.error ? <ErrorBanner error={sas.error} /> : null}
        {sas.data?.items.length ? (
          <Table<ServiceAccountRow>
            aria-label="Service accounts"
            density="compact"
            idKey={(s) => `${s.namespace}/${s.name}`}
            data={sas.data.items as ServiceAccountRow[]}
            columns={serviceAccountColumns}
          />
        ) : (
          empty('service accounts', sas.isLoading)
        )}
      </Panel>
      <Panel title="Role bindings" headingId="iam-rb">
        {rbs.error ? <ErrorBanner error={rbs.error} /> : null}
        {!rbac.allowed && rbs.data?.items.length ? (
          <Banner
            data-testid="rbac-locked"
            status="warning"
            title="Roles need a license key"
            description="Without one the built-in administrator is the only role: these bindings are configured but grant nothing."
            endContent={<Link href="/license">View license</Link>}
          />
        ) : null}
        <Diagnoses items={rbs.data?.diagnoses} />
        {rbs.data?.items.length ? (
          <Table<BindingRow>
            aria-label="Role bindings"
            density="compact"
            idKey={(b) => `${b.source}/${bindingName(b)}`}
            data={rbs.data.items as BindingRow[]}
            columns={bindingColumns}
          />
        ) : (
          empty('role bindings', rbs.isLoading)
        )}
      </Panel>
      <Panel title="Identity providers" headingId="iam-idp">
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
        <Diagnoses items={idps.data?.diagnoses} />
        {idps.data?.items.length ? (
          <Table<ProviderRow> aria-label="Identity providers" density="compact" idKey="id" data={idps.data.items as ProviderRow[]} columns={providerColumns} />
        ) : (
          empty('identity providers', idps.isLoading)
        )}
      </Panel>
    </VStack>
  );
}

export default function AccessPage() {
  const [sp, setParams] = useSearchParams();
  const tab = sp.get('tab') ?? 'me';
  const navigate = useNavigate();
  useEffect(() => {
    if (tab === 'audit') navigate('/audit', { replace: true });
  }, [tab, navigate]);
  return (
    <Page>
      <PageHeader title="Access" />
      <TabList value={tab} onChange={(v) => setParams({ tab: v === 'me' ? null : v })} aria-label="Access sections">
        <Tab value="me" label="You & tokens" />
        <Tab value="iam" label="Identity" />
      </TabList>
      {tab === 'me' ? (
        <>
          <Me />
          <AuthenticatorPanel />
          <SessionsPanel />
          <Tokens />
        </>
      ) : null}
      {tab === 'iam' ? <Iam /> : null}
    </Page>
  );
}
