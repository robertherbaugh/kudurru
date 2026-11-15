// Access: the signed-in principal, its roles and permissions, its own API
// tokens (the kdr_ secret is shown once), and on the Identity tab the IAM
// inventory with the reviewed writers of the SQLite driver (D294, D396).
import { ExternalUsersPanel } from '../components/access/ExternalUsers';
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
import { canAnyScope, qk, useApi, useAuthCapabilities, useCan, useSession, useStaticMode } from '../api/context';
import { useFeature } from '../api/hooks';
import type { ApiToken, CreatedToken, IamRoleBinding, RbacScope } from '../api/types';
import { AuthenticatorPanel } from '../components/access/Authenticators';
import { IdentityProvidersPanel } from '../components/access/IdentityProviders';
import { LocalUsersPanel } from '../components/access/LocalUsers';
import { NewRoleBindingButton, RoleBindingActions } from '../components/access/RoleBindingForm';
import { ServiceAccountsPanel } from '../components/access/ServiceAccounts';
import { SessionsPanel } from '../components/access/Sessions';
import { ChangePasswordButton, principalName } from '../components/AccountMenu';
import { ErrorBanner, useNotify } from '../components/Feedback';
import { DurationField, durationPresets } from '../components/fields/DurationField';
import { EnumSelect } from '../components/fields/EnumSelect';
import { ObjectRefPicker } from '../components/fields/ObjectRefPicker';
import { TextField } from '../components/fields/TextField';
import { Page, PageHeader, Panel } from '../components/layout/Page';
import { formatDateTime, formatRelative } from '../lib/format';
import { ROLES, SCOPE_KINDS, scopeString } from '../lib/rbac';
import { useNavigate, useSearchParams } from '../lib/router';
import { zoneGroupError } from '../lib/zones';

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

export { ROLES, scopeString };

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
  const readOnly = useStaticMode();
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
  const allowed = !readOnly && ((!!session && canAnyScope(session, 'tokens', 'own')) || can('tokens', 'write'));
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
      renderCell: (t) => <Button label="Revoke" size="sm" variant="ghost" isDisabled={readOnly} tooltip={readOnly ? 'Tokens are read-only in static mode' : undefined} onClick={() => del.mutate(t.id)} />,
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
          {role ? <EnumSelect label="For" value={scopeKind} options={SCOPE_KINDS} onChange={(k) => { setScopeKind(k); setScopeZone(''); }} /> : null}
          {role && scopeKind === 'zone' ? (
            <ObjectRefPicker kind="dnszones" label="Zone" value={scopeZone ? [scopeZone] : []} onChange={(v) => setScopeZone(v[0] ?? '')} />
          ) : null}
          {role && scopeKind === 'zonegroup' ? (
            <TextField label="Zone group" value={scopeZone} onChange={setScopeZone} mono placeholder="prod" error={scopeZone ? (zoneGroupError(scopeZone) ?? undefined) : undefined} />
          ) : null}
        </HStack>
        <HStack gap={2}>
          <Button type="submit" label="Create token" variant="primary" isDisabled={!allowed || !description.trim() || (!!role && scopeKind === 'zonegroup' && !!zoneGroupError(scopeZone))} isLoading={create.isPending} />
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

/** "cluster", "zone home-arpa" or "zone group prod". */
export function scopeText(s: RbacScope | undefined): string {
  if (!s || !s.kind || s.kind === 'cluster') return 'cluster';
  const kind = s.kind === 'zonegroup' ? 'zone group' : s.kind;
  return s.name ? `${kind} ${s.name}` : kind;
}

function bindingName(b: IamRoleBinding): string {
  return b.namespace ? `${b.namespace}/${b.name}` : b.name;
}

function Diagnoses({ items }: { items: string[] | undefined }) {
  if (!items?.length) return null;
  return <Banner status="warning" title="Some entries were ignored" description={items.join('; ')} />;
}

type BindingRow = IamRoleBinding & Record<string, unknown>;

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

function Iam() {
  const api = useApi();
  const can = useCan();
  const rbs = useQuery({ queryKey: ['iam', 'rb'], queryFn: () => api.iam.roleBindings(), enabled: can('rolebindings', 'read') });
  const rbac = useFeature('rbac');
  const { localUserCommands, iamObjectCommands } = useAuthCapabilities();
  const readOnly = useStaticMode();
  return (
    <VStack gap={4}>
      <Banner
        status="info"
        title="Identity management"
        description={
          readOnly
            ? 'This node runs in static mode: identity is read-only here. Change accounts with kudurru admin on each host and objects in the distributed files.'
            : !localUserCommands || !iamObjectCommands
              ? 'This server (the Kubernetes driver) does not change identity through the API: manage people in your identity provider, service accounts, identity providers and role bindings with kubectl or your GitOps tool, and the built-in admin with kudurru admin. Administrators with users.write can still disable or sign out external accounts.'
              : 'Local users, service accounts with their tokens, identity providers and role bindings are created and changed here on the SQLite driver; on a cluster, use the primary. Objects of kudurru.yaml or objects.d are changed where they are declared, and the built-in admin with kudurru admin on the host. Every change names the exact version you reviewed and is sent once.'
        }
      />
      <LocalUsersPanel />
      <ExternalUsersPanel />
      <ServiceAccountsPanel />
      <Panel title="Role bindings" headingId="iam-rb" actions={<NewRoleBindingButton />}>
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
            columns={[...bindingColumns, { key: 'actions', header: 'Actions', width: proportional(1.5, { minWidth: 180 }), renderCell: (b) => <RoleBindingActions binding={b} /> }]}
          />
        ) : (
          <Text type="supporting" as="p">
            {rbs.isLoading ? 'Loading…' : 'No role bindings.'}
          </Text>
        )}
      </Panel>
      <IdentityProvidersPanel />
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
