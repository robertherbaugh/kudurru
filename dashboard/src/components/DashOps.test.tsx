// The dashboard surfaces of the v0.2.0 operations pass: operator edge blocks
// on the Clients pages (D388), the query-log sink status (D371, D372), the
// zone-group scope in role bindings and scoped permissions on zone pages
// (D385), and the account-command capabilities of /auth/status (D387).
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';
import { can } from '../api/context';
import type { ClusterInfo, Me } from '../api/types';
import { mockDB } from '../mocks/handlers';
import AccessPage from '../pages/AccessPage';
import ClientDetailPage from '../pages/ClientDetailPage';
import ClientsPage from '../pages/ClientsPage';
import { LoginPage } from '../pages/LoginPage';
import NodesPage from '../pages/NodesPage';
import ZoneDetailPage from '../pages/ZoneDetailPage';
import ZonesPage from '../pages/ZonesPage';
import { getSelector, pick } from '../test/controls';
import { server } from '../test/setup';
import { API, renderWithApp } from '../test/utils';
import { outcomeUnknown, roleBindingOutcomeUnknown } from '../lib/cluster';
import { ApiError } from '../api/client';
import { roleBindingManifest } from './access/RoleBindingForm';
import { AuthenticatorPanel, RecoverAuthenticatorButton } from './access/Authenticators';
import { edgeDurationError, edgePrefixError } from './EdgeBlocks';

/** Records the requests of one method and path while the test runs. */
function record(method: string, path: string): { bodies: unknown[]; count: () => number } {
  const bodies: unknown[] = [];
  let n = 0;
  const listener = ({ request }: { request: Request }) => {
    if (request.method === method && new URL(request.url).pathname === `/api/v1${path}`) {
      n++;
      void request.clone().text().then((t) => bodies.push(t ? JSON.parse(t) : undefined));
    }
  };
  server.events.on('request:start', listener);
  cleanups.push(() => server.events.removeListener('request:start', listener));
  return { bodies, count: () => n };
}

/** Whether an Astryx button is disabled: `disabled`, or aria-disabled when it carries a tooltip. */
function off(el: HTMLElement): boolean {
  return el.hasAttribute('disabled') || el.getAttribute('aria-disabled') === 'true';
}

const cleanups: (() => void)[] = [];
afterEach(() => {
  for (const f of cleanups.splice(0)) f();
});

function meAs(permissions: Record<string, string[]>, scopedPermissions?: Record<string, Record<string, string[]>>, roles: string[] = ['custom']) {
  server.use(
    http.get(`${API}/auth/me`, () =>
      HttpResponse.json({ principal: { kind: 'User', id: 'local:ops', name: 'ops', auth: 'cookie', provider: 'local', groups: [] }, roles, csrf: 'csrf-mock-token', permissions, ...(scopedPermissions ? { scopedPermissions } : {}) } satisfies Me),
    ),
  );
}

function asFollower() {
  const info: ClusterInfo = {
    clusterID: 'c', driver: 'sqlite', epoch: 7, leader: 'kudurru-0', nodes: 2, readOnly: false, node: 'kudurru-1',
    role: 'follower', primary: 'kudurru-0', primaryRequired: true, forwardedOperations: ['putObject'], localOperations: [], took_ms: 1,
  };
  server.use(http.get(`${API}/cluster`, () => HttpResponse.json(info)));
}

describe('operator edge blocks', () => {
  it('blocks a client from its row with prefix, duration and reason, then lists and revokes it', async () => {
    const posts = record('POST', '/edge/blocks');
    renderWithApp(<ClientsPage />, { route: '/clients' });
    const block = await screen.findByRole('button', { name: 'Block nas at the edge' });
    await waitFor(() => expect(off(block)).toBe(false));
    fireEvent.click(block);
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByLabelText(/Address or prefix/)).toHaveValue('192.168.1.40');
    fireEvent.change(within(dialog).getByLabelText(/Reason/), { target: { value: 'port scan' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Block' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(posts.count()).toBe(1);
    await waitFor(() => expect(posts.bodies[0]).toEqual({ prefix: '192.168.1.40', seconds: 3600, reason: 'port scan' }));
    const table = await screen.findByRole('table', { name: 'Active edge blocks' });
    await within(table).findByText('192.168.1.40/32');
    expect(within(table).getByText('port scan')).toBeInTheDocument();
    expect(within(table).getByText('203.0.113.0/24')).toBeInTheDocument();

    fireEvent.click(within(table).getByRole('button', { name: 'Revoke the edge block of 203.0.113.0/24' }));
    const confirm = await screen.findByRole('dialog');
    fireEvent.click(within(confirm).getByRole('button', { name: 'Revoke' }));
    await waitFor(() => expect(mockDB().edgeBlocks.map((b) => b.prefix)).toEqual(['192.168.1.40/32']));
    await waitFor(() => expect(within(table).queryByText('203.0.113.0/24')).toBeNull());
  });

  it('refuses a prefix wider than the bounds before any request', async () => {
    const posts = record('POST', '/edge/blocks');
    renderWithApp(<ClientDetailPage params={{ key: '192.168.1.40' }} />, { route: '/clients/192.168.1.40' });
    const block = await screen.findByRole('button', { name: 'Block nas at the edge' });
    await waitFor(() => expect(off(block)).toBe(false));
    fireEvent.click(block);
    const dialog = await screen.findByRole('dialog');
    fireEvent.change(within(dialog).getByLabelText(/Address or prefix/), { target: { value: '192.168.0.0/8' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Block' }));
    expect((await within(dialog).findAllByText('IPv4 prefixes must be /16 or longer')).length).toBeGreaterThan(0);
    expect(posts.count()).toBe(0);
    expect(edgePrefixError('2001:db8::/24')).toBe('IPv6 prefixes must be /32 or longer');
    expect(edgePrefixError('2001:db8::/48')).toBeNull();
    expect(edgePrefixError('phone.dot')).toBe('Not an IP address or CIDR prefix');
    expect(edgeDurationError('30s')).toBe('Between 1 minute and 7 days');
    expect(edgeDurationError('168h')).toBeNull();
  });

  it('sends an uncertain block once and then stays locked', async () => {
    let n = 0;
    server.use(http.post(`${API}/edge/blocks`, () => { n++; return HttpResponse.error(); }));
    renderWithApp(<ClientsPage />, { route: '/clients' });
    const block = await screen.findByRole('button', { name: 'Block nas at the edge' });
    await waitFor(() => expect(off(block)).toBe(false));
    fireEvent.click(block);
    const dialog = await screen.findByRole('dialog');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Block' }));
    expect(await within(dialog).findByText('Outcome unknown')).toBeInTheDocument();
    expect(off(within(dialog).getByRole('button', { name: 'Block' }))).toBe(true);
    fireEvent.submit(within(dialog).getByRole('button', { name: 'Block' }).closest('form')!);
    await new Promise((r) => setTimeout(r, 50));
    expect(n).toBe(1);
    // The Kubernetes driver reports a dispatched write as 503 unavailable with an outcome hint.
    expect(outcomeUnknown(new ApiError(503, 'unavailable', 'cluster edge block state unavailable', { dispatched: true, outcome: 'unknown' }))).toBe(true);
    expect(outcomeUnknown(new ApiError(503, 'primary_required', 'follower'))).toBe(false);
    expect(outcomeUnknown(new ApiError(409, 'conflict', 'exists'))).toBe(false);
    expect(outcomeUnknown(new ApiError(0, 'network', 'unreachable'))).toBe(true);
  });

  it('locks the block dialog after a dispatched write whose outcome the Kubernetes driver cannot tell', async () => {
    let n = 0;
    server.use(http.post(`${API}/edge/blocks`, () => {
      n++;
      return HttpResponse.json({ error: { key: 'unavailable', message: 'cluster edge block state unavailable', hint: { dispatched: true, outcome: 'unknown' } } }, { status: 503 });
    }));
    renderWithApp(<ClientsPage />, { route: '/clients' });
    const block = await screen.findByRole('button', { name: 'Block nas at the edge' });
    await waitFor(() => expect(off(block)).toBe(false));
    fireEvent.click(block);
    const dialog = await screen.findByRole('dialog');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Block' }));
    expect(await within(dialog).findByText('Outcome unknown')).toBeInTheDocument();
    expect(off(within(dialog).getByRole('button', { name: 'Block' }))).toBe(true);
    fireEvent.submit(within(dialog).getByRole('button', { name: 'Block' }).closest('form')!);
    await new Promise((r) => setTimeout(r, 50));
    expect(n).toBe(1);
  });

  it('sends a revoke that fails with a 500 once and then stays locked', async () => {
    let n = 0;
    server.use(http.delete(`${API}/edge/blocks/:id`, () => {
      n++;
      return HttpResponse.json({ error: { key: 'internal', message: 'internal error' } }, { status: 500 });
    }));
    renderWithApp(<ClientsPage />, { route: '/clients' });
    const table = await screen.findByRole('table', { name: 'Active edge blocks' });
    const revoke = await within(table).findByRole('button', { name: 'Revoke the edge block of 203.0.113.0/24' });
    await waitFor(() => expect(off(revoke)).toBe(false));
    fireEvent.click(revoke);
    const confirm = await screen.findByRole('dialog');
    fireEvent.click(within(confirm).getByRole('button', { name: 'Revoke' }));
    expect(await within(confirm).findByText('Outcome unknown')).toBeInTheDocument();
    expect(off(within(confirm).getByRole('button', { name: 'Revoke' }))).toBe(true);
    fireEvent.submit(within(confirm).getByRole('button', { name: 'Revoke' }).closest('form')!);
    await new Promise((r) => setTimeout(r, 50));
    expect(n).toBe(1);
  });

  it('disables the actions without edgeblocks.write and hides the list without edgeblocks.read', async () => {
    const reads = record('GET', '/edge/blocks');
    meAs({ clients: ['read'], clientgroups: ['read'], querylog: ['read'], stats: ['read'] });
    renderWithApp(<ClientsPage />, { route: '/clients' });
    const block = await screen.findByRole('button', { name: 'Block nas at the edge' });
    expect(off(block)).toBe(true);
    expect(screen.queryByRole('table', { name: 'Active edge blocks' })).toBeNull();
    expect(reads.count()).toBe(0);
  });

  it('runs only on the primary: a follower disables block and revoke and names the primary', async () => {
    asFollower();
    renderWithApp(<ClientsPage />, { route: '/clients' });
    const table = await screen.findByRole('table', { name: 'Active edge blocks' });
    const revoke = await within(table).findByRole('button', { name: 'Revoke the edge block of 203.0.113.0/24' });
    await waitFor(() => expect(off(revoke)).toBe(true));
    expect(off(screen.getByRole('button', { name: 'Block nas at the edge' }))).toBe(true);
  });

  it('offers no edge block for a client ID', async () => {
    server.use(http.get(`${API}/clients`, () => HttpResponse.json({ items: [{ key: 'phone.dot', count: 3, blocked: 0 }, { key: '192.0.2.7', count: 1, blocked: 0 }], took_ms: 1 })));
    renderWithApp(<ClientsPage />, { route: '/clients' });
    await screen.findByRole('button', { name: 'Block 192.0.2.7 at the edge' });
    expect(screen.queryByRole('button', { name: 'Block phone.dot at the edge' })).toBeNull();
  });
});

describe('query-log sink status', () => {
  it('shows each sink with queue, drops, failures and last success, and never a credential or URL', async () => {
    server.use(
      http.get(`${API}/querylog/sinks`, () =>
        HttpResponse.json({
          node: 'kudurru-0',
          sinks: [
            { name: 'loki', type: 'loki', raw: false, queued: 10_000, queue_size: 10_000, sent: 3_200, dropped: 1_877, errors: 41, last_success: new Date(Date.now() - 60_000).toISOString(), url: 'https://writer:s3cr3t@loki.example:3100', passwordRef: 'env:LOKI_PASSWORD', lastError: 'HTTP 401 for https://writer:s3cr3t@loki.example' },
            { name: 'collector', type: 'dnstap', raw: true, queued: 0, queue_size: 500, sent: 0, dropped: 0, errors: 3 },
          ],
        }),
      ),
    );
    renderWithApp(<NodesPage />, { route: '/nodes' });
    const table = await screen.findByRole('table', { name: 'Query-log sinks' });
    const loki = within(table).getByText('loki').closest('tr')!;
    expect(loki).toHaveTextContent('10,000 / 10,000');
    expect(loki).toHaveTextContent('1,877');
    expect(loki).toHaveTextContent('41');
    const collector = within(table).getByText('collector').closest('tr')!;
    expect(within(collector).getByText('raw')).toBeInTheDocument();
    expect(collector).toHaveTextContent('never');
    expect(screen.getByRole('region', { name: 'Query-log sinks' })).toHaveTextContent('kudurru-0');
    expect(document.body.textContent).not.toMatch(/s3cr3t|writer@|loki\.example|LOKI_PASSWORD|HTTP 401/);
  });

  it('is hidden and never asked without querylog.read', async () => {
    const reads = record('GET', '/querylog/sinks');
    meAs({ stats: ['read'], settings: ['read'] });
    renderWithApp(<NodesPage />, { route: '/nodes' });
    await screen.findByRole('heading', { name: 'Nodes' });
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByRole('region', { name: 'Query-log sinks' })).toBeNull();
    expect(reads.count()).toBe(0);
  });
});

/** Leaves example-internal with only API-managed records so its deletion turns on permissions alone. */
function deletableExampleInternal() {
  const db = mockDB();
  db.recordSets = db.recordSets.filter((rs) => rs.metadata.name !== 'external-dns-ingress');
}

describe('zone-group scope', () => {
  it('counts zone and zone-group grants on that zone only', () => {
    const me: Me = { principal: { kind: 'User', id: 'u', name: 'u', groups: [] }, roles: ['zone-admin@zonegroup:prod'], permissions: { zones: ['read'] }, scopedPermissions: { 'zonegroup:prod': { records: ['read', 'write'] }, 'zone:lab': { zones: ['*'] } } };
    expect(can(me, 'records', 'write')).toBe(false);
    expect(can(me, 'records', 'write', { zone: 'a', zoneGroup: 'prod' })).toBe(true);
    expect(can(me, 'records', 'write', { zone: 'a', zoneGroup: 'dev' })).toBe(false);
    expect(can(me, 'records', 'write', { zone: 'a' })).toBe(false);
    expect(can(me, 'zones', 'delete', { zone: 'lab' })).toBe(true);
    expect(can(me, 'zones', 'delete', { zone: 'lab2' })).toBe(false);
  });

  it('enables record actions for a member zone of the signed-in user’s group and not for others', async () => {
    const zone = mockDB().zones.find((z) => z.metadata.name === 'example-internal')!;
    zone.metadata.labels = { 'kudurru.io/zonegroup': 'prod' };
    meAs({ zones: ['read'], records: ['read'] }, { 'zonegroup:prod': { zones: ['read', 'write', 'transfer'], records: ['read', 'write', 'delete'] } }, ['zone-admin@zonegroup:prod']);
    const view = renderWithApp(<ZoneDetailPage params={{ zone: 'example-internal' }} />, { route: '/zones/example-internal' });
    expect(await screen.findByText('zone group prod')).toBeInTheDocument();
    await waitFor(() => expect(off(screen.getByRole('button', { name: 'Add record' }))).toBe(false));
    expect(off(screen.getByRole('button', { name: 'Import BIND' }))).toBe(false);
    expect(off(screen.getByRole('button', { name: 'Send NOTIFY' }))).toBe(false);
    // The group grants no zones.delete: the zone page keeps Delete zone off
    // (example-internal also carries external-dns records here; the next test
    // proves the grant alone).
    expect(off(screen.getByRole('button', { name: 'Delete zone' }))).toBe(true);
    const settings = await screen.findByTestId('section-zone-settings');
    await waitFor(() => expect(off(within(settings).getByRole('button', { name: 'Edit Zone settings' }))).toBe(false));
    view.unmount();
    zone.metadata.labels = {};
    renderWithApp(<ZoneDetailPage params={{ zone: 'example-internal' }} />, { route: '/zones/example-internal' });
    const add = await screen.findByRole('button', { name: 'Add record' });
    await waitFor(() => expect(screen.getByText('example.internal.', { exact: false })).toBeInTheDocument());
    expect(off(add)).toBe(true);
    expect(off(screen.getByRole('button', { name: 'Import BIND' }))).toBe(true);
    expect(off(screen.getByRole('button', { name: 'Send NOTIFY' }))).toBe(true);
    const ro = await screen.findByTestId('section-zone-settings');
    expect(off(within(ro).getByRole('button', { name: 'Edit Zone settings' }))).toBe(true);
  });

  it('enables Delete zone where a zone-group grant holds zones.delete', async () => {
    deletableExampleInternal();
    const zone = mockDB().zones.find((z) => z.metadata.name === 'example-internal')!;
    zone.metadata.labels = { 'kudurru.io/zonegroup': 'prod' };
    meAs({ zones: ['read'], records: ['read'] }, { 'zonegroup:prod': { zones: ['read', 'delete'] } }, ['zone-admin@zonegroup:prod']);
    renderWithApp(<ZoneDetailPage params={{ zone: 'example-internal' }} />, { route: '/zones/example-internal' });
    expect(await screen.findByText('zone group prod')).toBeInTheDocument();
    await waitFor(() => expect(off(screen.getByRole('button', { name: 'Delete zone' }))).toBe(false));
    expect(off(screen.getByRole('button', { name: 'Add record' }))).toBe(true);
  });

  it('shows each zone’s group on the Zones page and enables Delete only for member zones', async () => {
    deletableExampleInternal();
    const zone = mockDB().zones.find((z) => z.metadata.name === 'example-internal')!;
    zone.metadata.labels = { 'kudurru.io/zonegroup': 'prod' };
    meAs({ zones: ['read'] }, { 'zonegroup:prod': { zones: ['read', 'delete'] } }, ['zone-admin@zonegroup:prod']);
    const view = renderWithApp(<ZonesPage />, { route: '/zones' });
    expect(await screen.findByText('Zone group prod')).toBeInTheDocument();
    await waitFor(() => expect(off(screen.getByRole('button', { name: 'Delete zone example.internal.' }))).toBe(false));
    view.unmount();
    // The same zone in another group: the grant no longer reaches it.
    zone.metadata.labels = { 'kudurru.io/zonegroup': 'dev' };
    renderWithApp(<ZonesPage />, { route: '/zones' });
    expect(await screen.findByText('Zone group dev')).toBeInTheDocument();
    expect(off(screen.getByRole('button', { name: 'Delete zone example.internal.' }))).toBe(true);
  });

  it('creates a role binding scoped to a zone group', async () => {
    const posts = record('POST', '/iam/rolebindings');
    renderWithApp(<AccessPage />, { route: '/access?tab=iam' });
    const open = await screen.findByRole('button', { name: 'New role binding' });
    await waitFor(() => expect(off(open)).toBe(false));
    fireEvent.click(open);
    const dialog = await screen.findByRole('dialog');
    expect(getSelector('Scope', dialog)).toBeInTheDocument();
    fireEvent.change(within(dialog).getByLabelText(/^Name(?!space)/), { target: { value: 'prod-admins' } });
    fireEvent.change(within(dialog).getByLabelText(/^Zone group/), { target: { value: 'bad group!' } });
    fireEvent.change(within(dialog).getByLabelText(/^User name/), { target: { value: 'alice' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Create' }));
    expect((await within(dialog).findAllByText(/Letters, digits/)).length).toBeGreaterThan(0);
    expect(posts.count()).toBe(0);
    fireEvent.change(within(dialog).getByLabelText(/^Zone group/), { target: { value: 'prod' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Create' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(posts.count()).toBe(1);
    expect(mockDB().createdRoleBindings[0]).toEqual({
      apiVersion: 'kudurru.io/v1alpha1', kind: 'RoleBinding', metadata: { name: 'prod-admins', namespace: 'default' },
      spec: { role: 'zone-admin', scope: { kind: 'zonegroup', name: 'prod' }, subjects: [{ kind: 'User', name: 'alice' }] },
    });
    const rb = await screen.findByRole('table', { name: 'Role bindings' });
    expect(await within(rb).findByText('zone group prod')).toBeInTheDocument();
  });

  it('offers cluster, zone and zone group, with a zone picker for one zone', async () => {
    renderWithApp(<AccessPage />, { route: '/access?tab=iam' });
    const open = await screen.findByRole('button', { name: 'New role binding' });
    await waitFor(() => expect(off(open)).toBe(false));
    fireEvent.click(open);
    const dialog = await screen.findByRole('dialog');
    const scope = getSelector('Scope', dialog);
    pick(scope, 'One zone');
    expect(await within(dialog).findByText('Zone', { selector: 'label, label *' })).toBeInTheDocument();
    pick(getSelector('Scope', dialog), 'Everything');
    expect(within(dialog).queryByLabelText(/^Zone group/)).toBeNull();
    expect(roleBindingManifest({ name: 'all', namespace: 'default', role: 'viewer', scopeKind: 'cluster', scopeName: '', subjectKind: 'ServiceAccount', subjectName: 'grafana', subjectNamespace: 'mon' }).spec).toEqual({
      role: 'viewer', scope: { kind: 'cluster' }, subjects: [{ kind: 'ServiceAccount', name: 'grafana', namespace: 'mon' }],
    });
  });

  /** Opens the dialog, fills a valid zone-group binding and presses Create once. */
  async function createProdAdmins(): Promise<HTMLElement> {
    renderWithApp(<AccessPage />, { route: '/access?tab=iam' });
    const open = await screen.findByRole('button', { name: 'New role binding' });
    await waitFor(() => expect(off(open)).toBe(false));
    fireEvent.click(open);
    const dialog = await screen.findByRole('dialog');
    fireEvent.change(within(dialog).getByLabelText(/^Name(?!space)/), { target: { value: 'prod-admins' } });
    fireEvent.change(within(dialog).getByLabelText(/^Zone group/), { target: { value: 'prod' } });
    fireEvent.change(within(dialog).getByLabelText(/^User name/), { target: { value: 'alice' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Create' }));
    return dialog;
  }

  it('sends a role binding once and locks when the SQLite server cannot tell whether it committed', async () => {
    let n = 0;
    // servingHTTPAuthError's catch-all: any unclassified error, including a
    // commit that was attempted and whose outcome is unknown.
    server.use(http.post(`${API}/iam/rolebindings`, () => {
      n++;
      return HttpResponse.json({ error: { key: 'unavailable', message: 'authentication operation unavailable' } }, { status: 503 });
    }));
    const dialog = await createProdAdmins();
    expect(await within(dialog).findByText('Outcome unknown')).toBeInTheDocument();
    expect(off(within(dialog).getByRole('button', { name: 'Create' }))).toBe(true);
    fireEvent.submit(within(dialog).getByRole('button', { name: 'Create' }).closest('form')!);
    await new Promise((r) => setTimeout(r, 50));
    expect(n).toBe(1);
  });

  it('treats a role binding the runtime refuses before any effect as an ordinary error', async () => {
    let n = 0;
    server.use(http.post(`${API}/iam/rolebindings`, () => {
      n++;
      return HttpResponse.json({ error: { key: 'unavailable', message: 'public RoleBinding command unavailable', hint: 'requires the complete SQLite runtime' } }, { status: 503 });
    }));
    const dialog = await createProdAdmins();
    expect(await within(dialog).findByText(/public RoleBinding command unavailable/)).toBeInTheDocument();
    expect(within(dialog).queryByText('Outcome unknown')).toBeNull();
    expect(off(within(dialog).getByRole('button', { name: 'Create' }))).toBe(false);
    expect(n).toBe(1);
    expect(roleBindingOutcomeUnknown(new ApiError(503, 'primary_required', 'follower'))).toBe(false);
    expect(roleBindingOutcomeUnknown(new ApiError(409, 'conflict', 'exists'))).toBe(false);
    expect(roleBindingOutcomeUnknown(new ApiError(503, 'unavailable', 'current writable serving generation unavailable'))).toBe(true);
    expect(roleBindingOutcomeUnknown(new ApiError(500, 'internal', 'internal error'))).toBe(true);
  });

  it('runs role binding writes only on the primary', async () => {
    asFollower();
    renderWithApp(<AccessPage />, { route: '/access?tab=iam' });
    const open = await screen.findByRole('button', { name: 'New role binding' });
    await waitFor(() => expect(off(open)).toBe(true));
  });
});

describe('account-command capabilities (D387)', () => {
  it('hides authenticator controls and the login code where the driver offers none', async () => {
    mockDB().authCapabilities = { factorManagement: false, localUserCommands: false };
    const totp = record('GET', '/auth/totp');
    const view = renderWithApp(<><AuthenticatorPanel /><RecoverAuthenticatorButton user={{ name: 'viewer', uid: 'u1', builtinAdmin: false, disabled: false, mustChangePassword: false }} /></>);
    expect(await screen.findByText('Authenticators are not offered on this server')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Recover authenticator/ })).toBeNull();
    expect(screen.queryByRole('button', { name: /Set up authenticator/ })).toBeNull();
    expect(totp.count()).toBe(0);
    view.unmount();
    renderWithApp(<LoginPage />, { route: '/login', user: null });
    await screen.findByLabelText(/Password/);
    await waitFor(() => expect(screen.queryByLabelText(/Authenticator code/)).toBeNull());
  });

  it('keeps them where the driver offers them', async () => {
    renderWithApp(<LoginPage />, { route: '/login', user: null });
    expect(await screen.findByLabelText(/Authenticator code/)).toBeInTheDocument();
  });
});
