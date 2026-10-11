// Access & audit: the signed-in principal and its permissions, its own API
// tokens (the kdr_ secret is shown once), the read-only IAM views (the
// write API is v0.2; IAM is declared in YAML today) and the audit log.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Tab, TabList } from '@astryxdesign/core/TabList';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { qk, useApi, useCan, useSession } from '../api/context';
import type { CreatedToken } from '../api/types';
import { ErrorBanner, Loading, useNotify } from '../components/Feedback';
import { formatDateTime, formatRelative } from '../lib/format';
import { useSearchParams } from '../lib/router';

function Me() {
  const { data } = useSession();
  if (!data) return null;
  return (
    <section className="k-panel" aria-labelledby="me-title">
      <h2 id="me-title" className="k-h3">
        Signed in as {data.principal.name}
      </h2>
      <p className="k-small">
        {data.principal.kind} <code>{data.principal.id}</code>
        {data.principal.groups?.length ? `, groups ${data.principal.groups.join(', ')}` : ''}
      </p>
      <p className="k-row">
        {data.roles.map((r) => (
          <Badge key={r} variant="blue" label={r} />
        ))}
      </p>
      <details>
        <summary>Permissions</summary>
        <table className="k-table">
          <tbody>
            {Object.entries(data.permissions).map(([res, verbs]) => (
              <tr key={res}>
                <th scope="row">{res}</th>
                <td>{verbs.join(', ')}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </details>
    </section>
  );
}

function Tokens() {
  const api = useApi();
  const qc = useQueryClient();
  const notify = useNotify();
  const can = useCan();
  const [name, setName] = useState('');
  const [expires, setExpires] = useState('720h');
  const [created, setCreated] = useState<CreatedToken | null>(null);
  const q = useQuery({ queryKey: qk.tokens, queryFn: () => api.auth.tokens() });
  const create = useMutation({
    mutationFn: () => api.auth.createToken(name.trim(), expires.trim() || undefined),
    onSuccess: (t) => {
      setCreated(t);
      setName('');
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
        Tokens act as you with your roles. Use them as <code>Authorization: Bearer kdr_…</code> or with <code>kudurru login --token</code>.
      </p>
      <form
        className="k-row-end"
        onSubmit={(e) => {
          e.preventDefault();
          if (name.trim()) create.mutate();
        }}
      >
        <TextInput label="Token name" value={name} onChange={setName} placeholder="ci-deploy" />
        <TextInput label="Expires in" value={expires} onChange={setExpires} placeholder="720h" description="Go duration; empty for no expiry" />
        <Button type="submit" label="Create token" variant="primary" isDisabled={!allowed || !name.trim()} isLoading={create.isPending} />
      </form>
      {created ? (
        <Banner status="success" title={`Token ${created.name} created`} description="Copy it now; it is not shown again." isDismissable onDismiss={() => setCreated(null)}>
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
              <th scope="col">Name</th>
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
                <td colSpan={5} className="k-muted">
                  No tokens.
                </td>
              </tr>
            ) : null}
            {q.data.items.map((t) => (
              <tr key={t.id}>
                <td>
                  {t.name} <span className="k-mono k-muted k-small">kdr_{t.id}_…</span>
                </td>
                <td>{formatDateTime(t.createdAt)}</td>
                <td>{t.expiresAt ? formatRelative(t.expiresAt) : 'never'}</td>
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

function Iam() {
  const api = useApi();
  const users = useQuery({ queryKey: ['iam', 'users'], queryFn: () => api.iam.users() });
  const sas = useQuery({ queryKey: ['iam', 'sa'], queryFn: () => api.iam.serviceAccounts() });
  const rbs = useQuery({ queryKey: ['iam', 'rb'], queryFn: () => api.iam.roleBindings() });
  const idps = useQuery({ queryKey: ['iam', 'idp'], queryFn: () => api.iam.identityProviders() });
  return (
    <div className="k-stack-lg">
      <Banner status="info" title="Read-only in this release" description="Users, service accounts, role bindings and identity providers are declared in YAML (objects.d or Kubernetes); the write API arrives in v0.2." />
      <div className="k-grid k-grid-2">
        <section className="k-panel" aria-labelledby="iam-users">
          <h2 id="iam-users" className="k-h3">
            Users
          </h2>
          {users.error ? <ErrorBanner error={users.error} /> : null}
          <ul>
            {(users.data?.items ?? []).map((u) => (
              <li key={u.name}>
                <strong>{u.displayName ?? u.name}</strong> {u.local ? <Badge variant="neutral" label="local" /> : null} {u.disabled ? <Badge variant="warning" label="disabled" /> : null}
                {u.roles?.length ? <span className="k-muted k-small"> {u.roles.join(', ')}</span> : null}
              </li>
            ))}
          </ul>
        </section>
        <section className="k-panel" aria-labelledby="iam-sa">
          <h2 id="iam-sa" className="k-h3">
            Service accounts
          </h2>
          {sas.error ? <ErrorBanner error={sas.error} /> : null}
          <ul>
            {(sas.data?.items ?? []).map((s) => (
              <li key={`${s.namespace ?? ''}/${s.name}`}>
                {s.namespace ? `${s.namespace}/` : ''}
                {s.name}
                {s.tokens !== undefined ? <span className="k-muted k-small"> {s.tokens} tokens</span> : null}
              </li>
            ))}
          </ul>
        </section>
        <section className="k-panel" aria-labelledby="iam-rb">
          <h2 id="iam-rb" className="k-h3">
            Role bindings
          </h2>
          {rbs.error ? <ErrorBanner error={rbs.error} /> : null}
          <ul>
            {(rbs.data?.items ?? []).map((b) => (
              <li key={b.name}>
                <Badge variant="blue" label={b.role} /> {b.subjects.map((s) => `${s.kind} ${s.name}`).join(', ')}
              </li>
            ))}
          </ul>
        </section>
        <section className="k-panel" aria-labelledby="iam-idp">
          <h2 id="iam-idp" className="k-h3">
            Identity providers
          </h2>
          {idps.error ? <ErrorBanner error={idps.error} /> : null}
          <ul>
            {(idps.data?.items ?? []).map((p) => (
              <li key={p.name}>
                <strong>{p.name}</strong> <span className="k-mono k-small">{p.issuer}</span>
              </li>
            ))}
          </ul>
        </section>
      </div>
    </div>
  );
}

function Audit() {
  const api = useApi();
  const [actor, setActor] = useState('');
  const [kind, setKind] = useState('');
  const q = useQuery({ queryKey: qk.audit(actor, kind), queryFn: () => api.audit.list({ actor: actor || undefined, kind: kind || undefined, limit: 200 }) });
  return (
    <section className="k-panel" aria-labelledby="audit-title">
      <div className="k-panel-title">
        <h2 id="audit-title">Audit log</h2>
        <a href={api.audit.exportURL()} download="audit.jsonl">
          Export
        </a>
      </div>
      <div className="k-row-end">
        <TextInput label="Actor" value={actor} onChange={setActor} hasClear />
        <TextInput label="Kind" value={kind} onChange={setKind} placeholder="BlockList" hasClear />
      </div>
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
                <th scope="col">Outcome</th>
              </tr>
            </thead>
            <tbody>
              {q.data.items.map((a) => (
                <tr key={a.id}>
                  <td className="k-nowrap">{formatDateTime(a.ts)}</td>
                  <td>{a.actor}</td>
                  <td>{a.action}</td>
                  <td className="k-mono k-small">
                    {a.kind} {a.namespace ? `${a.namespace}/` : ''}
                    {a.name}
                  </td>
                  <td>
                    <Badge variant={a.outcome === 'success' ? 'success' : a.outcome === 'denied' ? 'warning' : 'error'} label={a.outcome} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
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
