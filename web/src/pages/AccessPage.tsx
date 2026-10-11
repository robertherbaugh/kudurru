// Access & audit: the signed-in principal, its roles and permissions, its
// own API tokens (the kdr_ secret is shown once), the read-only IAM views
// (the write API is v0.2; IAM is declared in YAML today) and the audit log,
// newest first with keyset paging.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { HStack } from '@astryxdesign/core/HStack';
import { Tab, TabList } from '@astryxdesign/core/TabList';
import { VStack } from '@astryxdesign/core/VStack';
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { qk, useApi, useCan, useSession } from '../api/context';
import type { AuditEvent, CreatedToken, IamRoleBinding, RbacScope } from '../api/types';
import { ChangePasswordButton } from '../components/AccountMenu';
import { ErrorBanner, Loading, useNotify } from '../components/Feedback';
import { DurationField, durationPresets } from '../components/fields/DurationField';
import { EnumSelect, type EnumOption } from '../components/fields/EnumSelect';
import { ObjectRefPicker } from '../components/fields/ObjectRefPicker';
import { TextField } from '../components/fields/TextField';
import { formatDateTime, formatRelative } from '../lib/format';
import { useSearchParams } from '../lib/router';

function Me() {
  const { data } = useSession();
  if (!data) return null;
  return (
    <section className="k-panel" aria-labelledby="me-title">
      <div className="k-panel-title">
        <h2 id="me-title">Signed in as {data.principal.name}</h2>
        <ChangePasswordButton />
      </div>
      <p className="k-small">
        {data.principal.kind} <code>{data.principal.id}</code>
        {data.principal.groups?.length ? `, groups ${data.principal.groups.join(', ')}` : ''}
      </p>
      <p className="k-row" aria-label="Roles">
        {data.roles.length ? data.roles.map((r) => <Badge key={r} variant="blue" label={r} />) : <span className="k-muted k-small">No roles reported for this session.</span>}
      </p>
      <details>
        <summary>Permissions</summary>
        <table className="k-table">
          <tbody>
            {Object.entries(data.permissions).map(([res, verbs]) => (
              <tr key={res}>
                <th scope="row">{res}</th>
                <td>{(verbs ?? []).join(', ')}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </details>
    </section>
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

function Tokens() {
  const api = useApi();
  const qc = useQueryClient();
  const notify = useNotify();
  const can = useCan();
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
  const allowed = can('tokens', 'own') || can('tokens', 'write');
  return (
    <section className="k-panel" aria-labelledby="tok-title">
      <h2 id="tok-title" className="k-h3">
        API tokens
      </h2>
      <p className="k-muted k-small">
        Tokens act as you with your roles, or with the narrower scopes you give them. Use them as <code>Authorization: Bearer kdr_…</code> or with{' '}
        <code>kudurru login --token</code>.
      </p>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (description.trim()) create.mutate();
        }}
      >
        <VStack gap={2}>
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
      </form>
      {created ? (
        <Banner
          status="success"
          title={`Token ${created.description || created.id} created`}
          description="Copy it now; it is not shown again."
          isDismissable
          onDismiss={() => setCreated(null)}
          collapsible={false}
        >
          <pre className="k-pre" data-testid="new-token">
            {created.token}
          </pre>
        </Banner>
      ) : null}
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.data ? (
        <table className="k-table" aria-label="Your API tokens">
          <thead>
            <tr>
              <th scope="col">Description</th>
              <th scope="col">Scopes</th>
              <th scope="col">Created</th>
              <th scope="col">Expires</th>
              <th scope="col">Last used</th>
              <th scope="col">
                <span className="k-visually-hidden">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {q.data.items.length === 0 ? (
              <tr>
                <td colSpan={6} className="k-muted">
                  No tokens.
                </td>
              </tr>
            ) : null}
            {q.data.items.map((t) => (
              <tr key={t.id}>
                <td>
                  {t.description || <span className="k-muted">(no description)</span>} <span className="k-mono k-muted k-small">kdr_{t.id}_…</span>
                </td>
                <td>{t.scopes?.length ? <span className="k-row">{t.scopes.map((s) => <Badge key={s} variant="neutral" label={s} />)}</span> : <span className="k-muted k-small">all your roles</span>}</td>
                <td>{formatDateTime(t.createdAt)}</td>
                <td>{t.expiresAt ? <span title={t.expiresAt}>{formatRelative(t.expiresAt)}</span> : 'never'}</td>
                <td>
                  {t.lastUsedAt ? formatRelative(t.lastUsedAt) : 'never'}
                  {t.lastUsedIP ? <span className="k-muted"> from {t.lastUsedIP}</span> : null}
                </td>
                <td>
                  <Button label="Revoke" size="sm" variant="ghost" onClick={() => del.mutate(t.id)} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}
    </section>
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

function Iam() {
  const api = useApi();
  const can = useCan();
  const users = useQuery({ queryKey: ['iam', 'users'], queryFn: () => api.iam.users(), enabled: can('users', 'read') });
  const sas = useQuery({ queryKey: ['iam', 'sa'], queryFn: () => api.iam.serviceAccounts(), enabled: can('serviceaccounts', 'read') });
  const rbs = useQuery({ queryKey: ['iam', 'rb'], queryFn: () => api.iam.roleBindings(), enabled: can('rolebindings', 'read') });
  const idps = useQuery({ queryKey: ['iam', 'idp'], queryFn: () => api.iam.identityProviders(), enabled: can('identityproviders', 'read') });
  const empty = (what: string, loading: boolean) => <p className="k-muted k-small">{loading ? 'Loading…' : `No ${what}.`}</p>;
  return (
    <div className="k-stack-lg">
      <Banner
        status="info"
        title="Read-only in this release"
        description="Users are managed with `kudurru admin` on the host; service accounts, role bindings and identity providers are declared in kudurru.yaml or as objects (objects.d or Kubernetes). The write API arrives in v0.2."
      />
      <section className="k-panel" aria-labelledby="iam-users">
        <h2 id="iam-users" className="k-h3">
          Local users
        </h2>
        {users.error ? <ErrorBanner error={users.error} /> : null}
        <Diagnoses items={users.data?.diagnoses} />
        {users.data?.items.length ? (
          <table className="k-table" aria-label="Local users">
            <thead>
              <tr>
                <th scope="col">User</th>
                <th scope="col">Groups</th>
                <th scope="col">Last sign-in</th>
                <th scope="col">Password changed</th>
              </tr>
            </thead>
            <tbody>
              {users.data.items.map((u) => (
                <tr key={u.name}>
                  <td>
                    <strong>{u.displayName || u.name}</strong>
                    {u.displayName ? <span className="k-muted k-small k-mono"> {u.name}</span> : null}{' '}
                    {u.builtinAdmin ? <Badge variant="blue" label="admin" /> : null} {u.disabled ? <Badge variant="warning" label="disabled" /> : null}{' '}
                    {u.mustChangePassword ? <Badge variant="orange" label="must change password" /> : null}
                  </td>
                  <td className="k-small">{u.groups?.length ? u.groups.join(', ') : '–'}</td>
                  <td className="k-small">{u.lastLoginAt ? formatRelative(u.lastLoginAt) : 'never'}</td>
                  <td className="k-small">{u.passwordChangedAt ? formatRelative(u.passwordChangedAt) : '–'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : (
          empty('local users', users.isLoading)
        )}
      </section>
      <section className="k-panel" aria-labelledby="iam-sa">
        <h2 id="iam-sa" className="k-h3">
          Service accounts
        </h2>
        {sas.error ? <ErrorBanner error={sas.error} /> : null}
        {sas.data?.items.length ? (
          <table className="k-table" aria-label="Service accounts">
            <thead>
              <tr>
                <th scope="col">Service account</th>
                <th scope="col">Description</th>
                <th scope="col" className="num">
                  Tokens
                </th>
              </tr>
            </thead>
            <tbody>
              {sas.data.items.map((s) => (
                <tr key={`${s.namespace}/${s.name}`}>
                  <td>
                    <span className="k-mono">
                      {s.namespace}/{s.name}
                    </span>{' '}
                    {s.builtinAdmin ? <Badge variant="blue" label="admin" /> : null} {s.disabled ? <Badge variant="warning" label="disabled" /> : null}
                  </td>
                  <td className="k-small">{s.description || '–'}</td>
                  <td className="num">{s.tokens}</td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : (
          empty('service accounts', sas.isLoading)
        )}
      </section>
      <section className="k-panel" aria-labelledby="iam-rb">
        <h2 id="iam-rb" className="k-h3">
          Role bindings
        </h2>
        {rbs.error ? <ErrorBanner error={rbs.error} /> : null}
        <Diagnoses items={rbs.data?.diagnoses} />
        {rbs.data?.items.length ? (
          <table className="k-table" aria-label="Role bindings">
            <thead>
              <tr>
                <th scope="col">Role</th>
                <th scope="col">Scope</th>
                <th scope="col">Subjects</th>
                <th scope="col">Binding</th>
              </tr>
            </thead>
            <tbody>
              {rbs.data.items.map((b) => (
                <tr key={`${b.source}/${bindingName(b)}`}>
                  <td>
                    <Badge variant="blue" label={b.role} />
                  </td>
                  <td className="k-small">{scopeText(b.scope)}</td>
                  <td className="k-small">{(b.subjects ?? []).map((s) => `${s.kind} ${s.namespace ? `${s.namespace}/` : ''}${s.name}`).join(', ') || '–'}</td>
                  <td className="k-small">
                    <span className="k-mono">{bindingName(b)}</span> <span className="k-muted">({b.managedBy ? `managed by ${b.managedBy}` : b.source === 'config' ? 'kudurru.yaml' : 'object'})</span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : (
          empty('role bindings', rbs.isLoading)
        )}
      </section>
      <section className="k-panel" aria-labelledby="iam-idp">
        <h2 id="iam-idp" className="k-h3">
          Identity providers
        </h2>
        {idps.error ? <ErrorBanner error={idps.error} /> : null}
        <Diagnoses items={idps.data?.diagnoses} />
        {idps.data?.items.length ? (
          <table className="k-table" aria-label="Identity providers">
            <thead>
              <tr>
                <th scope="col">Provider</th>
                <th scope="col">Issuer</th>
                <th scope="col">Client ID</th>
                <th scope="col">Source</th>
              </tr>
            </thead>
            <tbody>
              {idps.data.items.map((p) => (
                <tr key={p.id}>
                  <td>
                    <strong>{p.displayName || p.id}</strong> <span className="k-muted k-small k-mono">{p.id}</span>
                  </td>
                  <td className="k-mono k-small k-break">{p.issuer}</td>
                  <td className="k-mono k-small">{p.clientID}</td>
                  <td className="k-small">{p.source === 'config' ? 'kudurru.yaml' : (p.source ?? '–')}</td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : (
          empty('identity providers', idps.isLoading)
        )}
      </section>
    </div>
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
  const events = useMemo(() => q.data?.pages.flatMap((p) => p.items) ?? [], [q.data]);
  return (
    <section className="k-panel" aria-labelledby="audit-title">
      <div className="k-panel-title">
        <h2 id="audit-title">Audit log</h2>
        <span className="k-muted k-small">Newest first</span>
        <a href={api.audit.exportURL()} download="audit.jsonl">
          Export
        </a>
      </div>
      <HStack gap={2} wrap="wrap" align="start">
        <TextField label="Actor" startIcon="search" value={actor} onChange={setActor} hasClear />
        <EnumSelect label="Kind" value={kind} options={AUDIT_KINDS} onChange={setKind} emptyLabel="Any kind" />
      </HStack>
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isLoading ? <Loading /> : null}
      {q.data ? (
        <div className="k-table-wrap">
          <table className="k-table" aria-label="Audit entries">
            <thead>
              <tr>
                <th scope="col">Time</th>
                <th scope="col">Actor</th>
                <th scope="col">Action</th>
                <th scope="col">Object</th>
                <th scope="col">Result</th>
                <th scope="col">From</th>
              </tr>
            </thead>
            <tbody>
              {events.length === 0 ? (
                <tr>
                  <td colSpan={6} className="k-muted">
                    No audit events{actor || kind ? ' match' : ''}.
                  </td>
                </tr>
              ) : null}
              {events.map((a, i) => (
                <tr key={a.id ?? `${a.ts}-${i}`}>
                  <td className="k-nowrap">{formatDateTime(a.ts)}</td>
                  <td>{actorText(a.actor)}</td>
                  <td className="k-mono k-small">{a.action}</td>
                  <td className="k-mono k-small">{resourceText(a.resource)}</td>
                  <td>
                    <Badge variant={a.result === 'success' ? 'success' : a.result === 'denied' ? 'warning' : 'error'} label={a.result} />
                    {a.reason ? <div className="k-muted k-small">{a.reason}</div> : null}
                  </td>
                  <td className="k-small">
                    {a.request?.ip ?? '–'}
                    {a.authz ? <div className="k-muted">via {a.authz}</div> : null}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {q.hasNextPage ? (
            <div className="k-row">
              <Button label="Load older events" variant="secondary" size="sm" isLoading={q.isFetchingNextPage} onClick={() => void q.fetchNextPage()} />
            </div>
          ) : null}
        </div>
      ) : null}
    </section>
  );
}

export default function AccessPage() {
  const [sp, setParams] = useSearchParams();
  const tab = sp.get('tab') ?? 'me';
  const can = useCan();
  return (
    <div className="k-page">
      <div className="k-page-header">
        <h1>Access &amp; audit</h1>
      </div>
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
    </div>
  );
}
