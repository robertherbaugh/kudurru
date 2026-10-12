// Access & audit: the signed-in principal, its roles and permissions, its
// own API tokens (the kdr_ secret is shown once), the read-only IAM views
// (the write API is v0.2; IAM is declared in YAML today) and the audit log,
// newest first with keyset paging.
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
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { canAnyScope, qk, useApi, useCan, useSession } from '../api/context';
import type { ApiToken, AuditEvent, CreatedToken, IamIdentityProvider, IamRoleBinding, IamServiceAccount, IamUser, RbacScope } from '../api/types';
import { ChangePasswordButton } from '../components/AccountMenu';
import { ErrorBanner, Loading, useNotify } from '../components/Feedback';
import { DurationField, durationPresets } from '../components/fields/DurationField';
import { EnumSelect, type EnumOption } from '../components/fields/EnumSelect';
import { ObjectRefPicker } from '../components/fields/ObjectRefPicker';
import { TextField } from '../components/fields/TextField';
import { Page, PageHeader, Panel } from '../components/layout/Page';
import { formatDateTime, formatRelative } from '../lib/format';
import { useSearchParams } from '../lib/router';

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
    <Panel title={`Signed in as ${data.principal.name}`} headingId="me-title" actions={<ChangePasswordButton />}>
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
      width: pixel(96),
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
    width: proportional(1, { minWidth: 130 }),
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
  { key: 'tokens', header: 'Tokens', align: 'end', width: pixel(84), renderCell: (s) => <Text hasTabularNumbers>{s.tokens}</Text> },
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
  const empty = (what: string, loading: boolean) => (
    <Text type="supporting" as="p">
      {loading ? 'Loading…' : `No ${what}.`}
    </Text>
  );
  return (
    <VStack gap={4}>
      <Banner
        status="info"
        title="Read-only in this release"
        description="Users are managed with `kudurru admin` on the host; service accounts, role bindings and identity providers are declared in kudurru.yaml or as objects (objects.d or Kubernetes). The write API arrives in v0.2."
      />
      <Panel title="Local users" headingId="iam-users">
        {users.error ? <ErrorBanner error={users.error} /> : null}
        <Diagnoses items={users.data?.diagnoses} />
        {users.data?.items.length ? (
          <Table<UserRow> aria-label="Local users" density="compact" idKey="name" data={users.data.items as UserRow[]} columns={userColumns} />
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

const AUDIT_PAGE = 100;

/** The actor of an audit event as one line. */
export function actorText(a: AuditEvent['actor']): string {
  const who = a.name || a.id || 'unknown';
  return a.kind && a.kind !== 'User' ? `${who} (${a.kind})` : who;
}

/** The resource of an audit event as "Kind ns/name". */
export function resourceText(r: AuditEvent['resource']): string {
  const name = r.name ? `${r.namespace ? `${r.namespace}/` : ''}${r.name}` : '';
  return name ? `${r.kind} ${name}` : r.kind;
}

/** Resource kinds the audit log records (objects and runtime actions). */
const AUDIT_KINDS: EnumOption[] = ['DNSZone', 'DNSRecordSet', 'BlockList', 'ClientGroup', 'Policy', 'settings', 'blocking', 'cache', 'querylog', 'audit'].map((k) => ({
  value: k,
  label: k,
}));

/** An audit event with the row key it is listed under (events may carry no id). */
type AuditRow = AuditEvent & { rowKey: string } & Record<string, unknown>;

const auditColumns: TableColumn<AuditRow>[] = [
  { key: 'ts', header: 'Time', width: pixel(210), renderCell: (a) => <Text textWrap="nowrap">{formatDateTime(a.ts)}</Text> },
  { key: 'actor', header: 'Actor', width: proportional(1.5, { minWidth: 120 }), renderCell: (a) => <Text>{actorText(a.actor)}</Text> },
  {
    key: 'action',
    header: 'Action',
    width: proportional(1.5, { minWidth: 120 }),
    renderCell: (a) => (
      <Text type="code" size="sm">
        {a.action}
      </Text>
    ),
  },
  {
    key: 'resource',
    header: 'Object',
    width: proportional(2, { minWidth: 140 }),
    renderCell: (a) => (
      <Text type="code" size="sm">
        {resourceText(a.resource)}
      </Text>
    ),
  },
  {
    key: 'result',
    header: 'Result',
    width: proportional(1, { minWidth: 100 }),
    renderCell: (a) => (
      <VStack gap={0.5} align="start">
        <Badge variant={a.result === 'success' ? 'success' : a.result === 'denied' ? 'warning' : 'error'} label={a.result} />
        {a.reason ? <Text type="supporting">{a.reason}</Text> : null}
      </VStack>
    ),
  },
  {
    key: 'from',
    header: 'From',
    width: proportional(1, { minWidth: 110 }),
    renderCell: (a) => (
      <VStack gap={0.5}>
        <Text size="sm">{a.request?.ip ?? '–'}</Text>
        {a.authz ? <Text type="supporting">via {a.authz}</Text> : null}
      </VStack>
    ),
  },
];

function Audit() {
  const api = useApi();
  const [actor, setActor] = useState('');
  const [kind, setKind] = useState('');
  const q = useInfiniteQuery({
    queryKey: qk.audit(actor, kind),
    queryFn: ({ pageParam }) =>
      api.audit.list({ actor: actor.trim() || undefined, kind: kind.trim() || undefined, limit: AUDIT_PAGE, order: 'desc', ...(pageParam ? { cursor: pageParam } : {}) }),
    initialPageParam: '',
    getNextPageParam: (last) => last.continue || last.cursor_next || undefined,
  });
  const events = useMemo<AuditRow[]>(() => (q.data?.pages.flatMap((p) => p.items) ?? []).map((a, i) => ({ ...a, rowKey: a.uid ?? (a.id !== undefined ? String(a.id) : `${a.ts}-${i}`) })), [q.data]);
  return (
    <Panel
      title="Audit log"
      headingId="audit-title"
      actions={
        <>
          <Text type="supporting">Newest first</Text>
          <Link href={api.audit.exportURL()} download="audit.jsonl" hasUnderline>
            Export
          </Link>
        </>
      }
    >
      <HStack gap={2} wrap="wrap" align="start">
        <TextField label="Actor" startIcon="search" value={actor} onChange={setActor} hasClear />
        <EnumSelect label="Kind" value={kind} options={AUDIT_KINDS} onChange={setKind} emptyLabel="Any kind" />
      </HStack>
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isLoading ? <Loading /> : null}
      {q.data ? (
        <VStack gap={2}>
          <Table<AuditRow>
            aria-label="Audit entries"
            density="compact"
            verticalAlign="top"
            idKey="rowKey"
            data={events}
            columns={auditColumns}
            emptyState={<Text color="secondary">No audit events{actor || kind ? ' match' : ''}.</Text>}
          />
          {q.hasNextPage ? (
            <HStack gap={2}>
              <Button label="Load older events" variant="secondary" size="sm" isLoading={q.isFetchingNextPage} onClick={() => void q.fetchNextPage()} />
            </HStack>
          ) : null}
        </VStack>
      ) : null}
    </Panel>
  );
}

export default function AccessPage() {
  const [sp, setParams] = useSearchParams();
  const tab = sp.get('tab') ?? 'me';
  const can = useCan();
  return (
    <Page>
      <PageHeader title="Access & audit" />
      <TabList value={tab} onChange={(v) => setParams({ tab: v === 'me' ? null : v })} aria-label="Access sections">
        <Tab value="me" label="You & tokens" />
        <Tab value="iam" label="Identity" />
        {can('audit', 'read') ? <Tab value="audit" label="Audit" /> : null}
      </TabList>
      {tab === 'me' ? (
        <>
          <Me />
          <Tokens />
        </>
      ) : null}
      {tab === 'iam' ? <Iam /> : null}
      {tab === 'audit' ? <Audit /> : null}
    </Page>
  );
}
