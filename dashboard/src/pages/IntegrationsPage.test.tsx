// The Integrations pages against the mock contract (src/mocks/integrations.ts):
// the list, the manifest-driven wizard (references only, refused fields
// left out, unsupported types not offered), the reviewed commands (preview
// digest sent back, a stale preview and an uncertain sync shown as what
// they are), the terminating state and release, and the gates by
// permission, license, host, follower role and static mode.
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import fs from 'node:fs';
import path from 'node:path';
import { describe, expect, it } from 'vitest';
import type { ClusterInfo, Integration } from '../api/types';
import { mockIntegrations } from '../mocks/integrations';
import { getSelector, optionLabels, pick } from '../test/controls';
import { server } from '../test/setup';
import { API, renderWithApp } from '../test/utils';
import IntegrationDetailPage from './IntegrationDetailPage';
import IntegrationsPage from './IntegrationsPage';

const next = () => fireEvent.click(screen.getByRole('button', { name: 'Next' }));

/** Every request path the page sends (pathname and search). */
function requests(): string[] {
  const seen: string[] = [];
  server.events.on('request:start', ({ request }) => {
    const u = new URL(request.url);
    seen.push(`${request.method} ${u.pathname}${u.search}`);
  });
  return seen;
}

function bodies(suffix: string): unknown[] {
  const seen: unknown[] = [];
  server.events.on('request:start', ({ request }) => {
    if (request.method !== 'GET' && new URL(request.url).pathname.endsWith(suffix)) {
      void request.clone().text().then((t) => seen.push(t ? JSON.parse(t) : null));
    }
  });
  return seen;
}

function detail(name = 'public-dns') {
  return renderWithApp(<IntegrationDetailPage params={{ ns: 'default', name }} />, { route: `/integrations/default/${name}` });
}

function documentLists() {
  const doc = JSON.parse(fs.readFileSync(path.resolve(process.cwd(), '../api/openapi.json'), 'utf8')) as { paths: Record<string, Record<string, { operationId?: string; 'x-kudurru-follower'?: string }>> };
  const forwarded: string[] = [];
  const local: string[] = [];
  for (const ops of Object.values(doc.paths)) {
    for (const op of Object.values(ops)) {
      if (op.operationId && op['x-kudurru-follower'] === 'forwarded') forwarded.push(op.operationId);
      if (op.operationId && op['x-kudurru-follower'] === 'local') local.push(op.operationId);
    }
  }
  return { forwarded, local };
}

function cluster(over: Partial<ClusterInfo>) {
  server.use(http.get(`${API}/cluster`, () => HttpResponse.json({ driver: 'sqlite', epoch: 1, node: 'b', nodes: 2, primaryRequired: false, readOnly: false, took_ms: 1, ...over })));
}

describe('IntegrationsPage', () => {
  it('lists every Integration with the status of its exact revision', async () => {
    renderWithApp(<IntegrationsPage />, { route: '/integrations' });
    const row = await screen.findByTestId('integration-default/public-dns');
    expect(within(row).getByRole('link', { name: 'default/public-dns' })).toHaveAttribute('href', '/integrations/default/public-dns');
    expect(within(row).getByText('Cloudflare')).toBeInTheDocument();
    expect(await within(row).findByText('Ready')).toBeInTheDocument();
    expect(within(row).getByText('Delete')).toBeInTheDocument();
    const office = screen.getByTestId('integration-default/office');
    expect(within(office).getByText('UniFi Network')).toBeInTheDocument();
    expect(within(office).getByText('file')).toBeInTheDocument();
  });

  it('deletes the selected instances with one batch per namespace, after naming each one’s deletion policy', async () => {
    const m = mockIntegrations();
    m.objects.push(
      { apiVersion: 'kudurru.io/v1alpha1', kind: 'Integration', metadata: { name: 'peer', namespace: 'default', uid: 'uid-peer', resourceVersion: '12', annotations: { 'kudurru.io/managed-by': 'api' } }, spec: { type: 'webhook', webhook: { url: 'https://peer.example/' } } },
      { apiVersion: 'kudurru.io/v1alpha1', kind: 'Integration', metadata: { name: 'lab', namespace: 'lab', uid: 'uid-lab', resourceVersion: '13', annotations: { 'kudurru.io/managed-by': 'api' } }, spec: { type: 'webhook', webhook: { url: 'https://lab.example/' } } },
    );
    const seen = requests();
    renderWithApp(<IntegrationsPage />, { route: '/integrations' });
    const table = await screen.findByRole('table', { name: 'Integrations' });
    await screen.findByTestId('integration-lab/lab');
    // The file-managed instance cannot be selected.
    expect(within(screen.getByTestId('integration-default/office')).getByRole('checkbox')).toBeDisabled();
    for (const key of ['default/public-dns', 'default/peer', 'lab/lab']) fireEvent.click(within(screen.getByTestId(`integration-${key}`)).getByRole('checkbox'));
    fireEvent.click(screen.getByRole('button', { name: 'Delete 3 selected' }));
    const d = await screen.findByRole('dialog');
    expect(d).toHaveTextContent('Delete 3 integrations?');
    expect(within(d).getByText('default/public-dns').closest('li')).toHaveTextContent(/Delete: stays terminating/);
    expect(within(d).getByText('default/peer').closest('li')).toHaveTextContent(/Retain: removed at once/);
    fireEvent.click(within(d).getByRole('button', { name: 'Delete' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    const batches = m.calls.filter((c) => c.path === '/api/v1/integrations:batchDelete').map((c) => c.body);
    expect(batches).toEqual([{ names: ['peer', 'public-dns'], namespace: 'default' }, { names: ['lab'], namespace: 'lab' }]);
    expect(seen.filter((p) => p.startsWith('POST') && p.includes('batchDelete'))).toHaveLength(2);
    // Retain instances are gone; the Delete instance stays terminating.
    expect(m.objects.map((o) => `${o.metadata.namespace}/${o.metadata.name}`).sort()).toEqual(['default/office', 'default/public-dns']);
    expect(m.objects.find((o) => o.metadata.name === 'public-dns')?.metadata.deletionTimestamp).toBeTruthy();
    expect(table).toBeInTheDocument();
  });

  it('offers no batch delete without integrations.delete or in static mode', async () => {
    cluster({ readOnly: true });
    renderWithApp(<IntegrationsPage />, { route: '/integrations' });
    const row = await screen.findByTestId('integration-default/public-dns');
    fireEvent.click(within(row).getByRole('checkbox'));
    const del = await screen.findByRole('button', { name: 'Delete 1 selected' });
    expect(del).toHaveAttribute('aria-disabled', 'true');
  });

  it('asks for nothing without integrations.read', async () => {
    const seen = requests();
    renderWithApp(<IntegrationsPage />, { route: '/integrations', user: 'viewer' });
    expect(await screen.findByText('Viewing integrations needs integrations.read.')).toBeInTheDocument();
    await new Promise((r) => setTimeout(r, 50));
    expect(seen.filter((p) => p.includes('integrations'))).toEqual([]);
  });

  it('says so when the node runs no integration host', async () => {
    mockIntegrations().unavailable = true;
    renderWithApp(<IntegrationsPage />, { route: '/integrations' });
    expect(await screen.findByText('Integrations are not available on this node')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'New integration' })).toHaveAttribute('aria-disabled', 'true');
  });

  it('keeps existing ones editable but offers no new one without a license', async () => {
    mockIntegrations().licensed = false;
    renderWithApp(<IntegrationsPage />, { route: '/integrations' });
    expect(await screen.findByText('The license does not cover integrations')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('button', { name: 'New integration' })).toHaveAttribute('aria-disabled', 'true'));
    expect(await within(screen.getByTestId('integration-default/public-dns')).findByText('Paused')).toBeInTheDocument();
  });

  it('creates an instance from the manifest with a credential reference, never a value', async () => {
    const posted = bodies('/objects/integrations');
    renderWithApp(<IntegrationsPage />, { route: '/integrations?new=1' });
    // Provider: every type of the build.
    expect(await screen.findByRole('radio', { name: /Cloudflare/ })).toBeChecked();
    expect(screen.getByRole('radio', { name: /Webhook/ })).not.toBeDisabled();
    next();
    // Settings: from the configuration schema.
    const name = await screen.findByRole('textbox', { name: /^Name(?!space)/ });
    next();
    expect((await screen.findAllByText(/^Name: /)).length).toBeGreaterThan(0);
    fireEvent.change(name, { target: { value: 'public' } });
    next();
    expect(await screen.findByText(/Zones is required/)).toBeInTheDocument();
    fireEvent.click(within(screen.getByTestId('provider-fields')).getByRole('button', { name: 'Add' }));
    fireEvent.change(await screen.findByRole('textbox', { name: /^Zone$/ }), { target: { value: 'example.org' } });
    next();
    // Credentials: the forms the node resolves, the granted files offered.
    const cred = await screen.findByTestId('credential-apiToken');
    expect(within(cred).queryByLabelText(/value/i)).toBeNull();
    next();
    expect(await screen.findByText('API token is required')).toBeInTheDocument();
    const form = getSelector(/^API token reference/);
    expect(optionLabels(form)).toEqual(['Not set', 'File on the node', 'Environment variable']);
    pick(form, 'File on the node');
    pick(await waitFor(() => getSelector(/^API token file/)), '/run/secrets/cloudflare-token');
    next();
    // Review, then create.
    expect(await screen.findByTestId('integration-review')).toHaveTextContent('apiToken: file /run/secrets/cloudflare-token');
    fireEvent.click(screen.getByRole('button', { name: 'Create integration' }));
    await waitFor(() => expect(window.location.pathname).toBe('/integrations/default/public'));
    const created = mockIntegrations().objects.find((o) => o.metadata.name === 'public') as Integration;
    expect(created.spec).toEqual({ type: 'cloudflare', cloudflare: { zones: [{ zone: 'example.org' }] }, credentials: { apiToken: { file: '/run/secrets/cloudflare-token' } }, deletionPolicy: 'Retain' });
    expect(posted).toHaveLength(1);
  });

  it('offers on Kubernetes only what the driver runs', async () => {
    mockIntegrations().driver = 'kube';
    renderWithApp(<IntegrationsPage />, { route: '/integrations?new=1' });
    const webhook = await screen.findByRole('radio', { name: /Webhook/ });
    expect(webhook).toBeDisabled();
    expect(screen.getByText('This provider does not run on Kubernetes.')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('radio', { name: /UniFi/ }));
    next();
    expect(await screen.findByRole('textbox', { name: /^Namespace/ })).toHaveValue('kudurru');
    const fields = screen.getByTestId('provider-fields');
    expect(within(fields).queryByRole('group', { name: /Inventory/ })).toBeNull();
    // The three UniFi write switches are refused there and the settings they
    // use are inert, so no push field is offered; the manifest's reasons and
    // messages say why.
    expect(within(fields).queryByRole('group', { name: /Push/ })).toBeNull();
    expect(within(fields).queryByRole('switch', { name: /DHCP DNS servers|Client actions/ })).toBeNull();
    const refused = within(fields).getByTestId('refused-fields');
    expect(refused).toHaveTextContent(/\(refused\): spec\.unifi\.inventory\. UniFi DNS record publication \(spec\.unifi\.inventory\) does not run on the Kubernetes store yet.*\(RecordPublicationUnsupportedOnKubernetes\)/);
    expect(refused).toHaveTextContent(/\(refused\): spec\.unifi\.push\.dhcpDNSServers, spec\.unifi\.push\.mirrorRecords\.enabled, spec\.unifi\.push\.clientActions\. UniFi writes .* run only on the SQLite store.*\(UniFiPushUnsupportedOnKubernetes\)/);
    expect(refused).toHaveTextContent(/\(no effect here\): spec\.unifi\.push\.dnsServers, spec\.unifi\.push\.networks\. Used only by spec\.unifi\.push\.dhcpDNSServers, which the Kubernetes store refuses/);
    expect(refused).toHaveTextContent(/\(no effect here\): spec\.unifi\.push\.mirrorRecords\.types, spec\.unifi\.push\.mirrorRecords\.domains, spec\.unifi\.push\.mirrorRecords\.labels, spec\.unifi\.push\.mirrorRecords\.exclude\. Used only by spec\.unifi\.push\.mirrorRecords\.enabled/);
  });

  it('creates a UniFi instance with a push block from the manifest and reviews it as a publisher', async () => {
    renderWithApp(<IntegrationsPage />, { route: '/integrations?new=1' });
    fireEvent.click(await screen.findByRole('radio', { name: /UniFi/ }));
    next();
    // The first Name is the instance's (the inventory zone reference has one too).
    fireEvent.change((await screen.findAllByRole('textbox', { name: /^Name(?!space)/ }))[0]!, { target: { value: 'gateway' } });
    fireEvent.change(screen.getByRole('textbox', { name: /^Endpoint/ }), { target: { value: 'https://controller.example.com' } });
    const push = within(screen.getByTestId('provider-fields')).getByRole('group', { name: /^Push/ });
    // DHCP DNS servers: an IPv4 list checked inline, and the parameters the switch needs.
    fireEvent.click(within(push).getByRole('switch', { name: /DHCP DNS servers/ }));
    next();
    expect(await screen.findByText(/DHCP DNS servers needs at least one DNS server and one network/)).toBeInTheDocument();
    const servers = within(push).getByRole('group', { name: /^DNS servers/ });
    fireEvent.click(within(servers).getByRole('button', { name: 'Add' }));
    const server1 = within(servers).getByRole('textbox', { name: 'DNS servers 1' });
    expect(server1).toHaveAttribute('placeholder', 'e.g. 192.0.2.10');
    fireEvent.change(server1, { target: { value: '2001:db8::53' } });
    expect(await within(servers).findByText(/Not an IPv4 address/)).toBeInTheDocument();
    next();
    expect(await screen.findByText(/2001:db8::53: Not an IPv4 address/)).toBeInTheDocument();
    fireEvent.change(server1, { target: { value: '192.0.2.53' } });
    const networks = within(push).getByRole('group', { name: /^Networks/ });
    fireEvent.click(within(networks).getByRole('button', { name: 'Add' }));
    fireEvent.change(within(networks).getByRole('textbox', { name: 'Networks 1' }), { target: { value: 'LAN' } });
    // The record mirror: a nested object whose domains its switch needs.
    const mirror = within(push).getByRole('group', { name: /^Mirror records/ });
    fireEvent.click(within(mirror).getByRole('switch', { name: /Enabled/ }));
    next();
    expect(await screen.findByText(/Mirror records needs at least one domain/)).toBeInTheDocument();
    const domains = within(mirror).getByRole('group', { name: /^Domains/ });
    fireEvent.click(within(domains).getByRole('button', { name: 'Add' }));
    fireEvent.change(within(domains).getByRole('textbox', { name: 'Domains 1' }), { target: { value: 'example.com' } });
    next();
    // Credentials, then the review names it a reviewed publisher.
    pick(await waitFor(() => getSelector(/^API key reference/)), 'File on the node');
    pick(await waitFor(() => getSelector(/^API key file/)), '/run/secrets/unifi-key');
    next();
    const review = await screen.findByTestId('integration-review');
    expect(review).toHaveTextContent('Publishes records: every sync is a reviewed Preview, then Publish this plan by its digest.');
    fireEvent.click(screen.getByRole('button', { name: 'Create integration' }));
    await waitFor(() => expect(window.location.pathname).toBe('/integrations/default/gateway'));
    const created = mockIntegrations().objects.find((o) => o.metadata.name === 'gateway') as Integration;
    expect(created.spec).toEqual({
      type: 'unifi',
      unifi: { endpoint: 'https://controller.example.com', push: { dhcpDNSServers: true, dnsServers: ['192.0.2.53'], networks: ['LAN'], mirrorRecords: { enabled: true, domains: ['example.com'] } } },
      credentials: { apiKey: { file: '/run/secrets/unifi-key' } },
      deletionPolicy: 'Retain',
    });
  });
});

describe('IntegrationDetailPage', () => {
  it('previews, then publishes exactly the reviewed plan by its digest', async () => {
    const synced = bodies('/sync');
    detail();
    const publish = await screen.findByRole('button', { name: 'Publish this plan' });
    await waitFor(() => expect(screen.getByRole('button', { name: 'Preview' })).not.toHaveAttribute('aria-disabled', 'true'));
    expect(publish).toHaveAttribute('aria-disabled', 'true');
    fireEvent.click(screen.getByRole('button', { name: 'Preview' }));
    const plan = await screen.findByTestId('integration-preview');
    const digest = '1'.padStart(64, 'a');
    expect(plan).toHaveTextContent(digest);
    expect(within(plan).getByRole('table', { name: 'Planned changes' })).toHaveTextContent('www.example.org A');
    expect(plan).toHaveTextContent('mail.example.org MX: owned by another writer');
    fireEvent.click(screen.getByRole('button', { name: 'Publish this plan' }));
    expect(await screen.findByText('Sync completed')).toBeInTheDocument();
    expect(synced).toEqual([{ uid: 'uid-public-dns', revision: '10', mode: 'publish', previewDigest: digest }]);
    // The plan is spent: publishing again needs a new preview.
    expect(screen.queryByTestId('integration-preview')).toBeNull();
    expect(screen.getByRole('button', { name: 'Publish this plan' })).toHaveAttribute('aria-disabled', 'true');
  });

  it('shows an expired preview as refused and publishes nothing', async () => {
    mockIntegrations().expireNextPreview = true;
    detail();
    fireEvent.click(await screen.findByRole('button', { name: 'Preview' }));
    await screen.findByTestId('integration-preview');
    fireEvent.click(screen.getByRole('button', { name: 'Publish this plan' }));
    expect(await screen.findByText('Sync refused')).toBeInTheDocument();
    expect(screen.getByText(/preview expired or no longer matches .* Nothing was published/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Publish this plan' })).toHaveAttribute('aria-disabled', 'true');
  });

  it('never sends an uncertain sync again', async () => {
    mockIntegrations().uncertainNextSync = true;
    const synced = bodies('/sync');
    detail();
    fireEvent.click(await screen.findByRole('button', { name: 'Preview' }));
    await screen.findByTestId('integration-preview');
    fireEvent.click(screen.getByRole('button', { name: 'Publish this plan' }));
    expect(await screen.findByText('Sync: outcome unknown')).toBeInTheDocument();
    expect(screen.getByText(/It was not sent again/)).toBeInTheDocument();
    const publish = screen.getByRole('button', { name: 'Publish this plan' });
    expect(publish).toHaveAttribute('aria-disabled', 'true');
    fireEvent.click(publish);
    expect(synced).toHaveLength(1);
  });

  it('keeps an uncertain sync from being sent again after the revision moves, until a Check succeeds', async () => {
    mockIntegrations().objects[1]!.metadata.annotations = { 'kudurru.io/managed-by': 'api' };
    mockIntegrations().uncertainNextSync = true;
    const synced = bodies('/sync');
    detail('office');
    const syncNow = await screen.findByRole('button', { name: 'Sync now' });
    await waitFor(() => expect(syncNow).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(syncNow);
    expect(await screen.findByText('Sync: outcome unknown')).toBeInTheDocument();
    // The revision moves (a change, or a host status write the host has not
    // captured yet, which the server still refuses, D384); a stale Check is
    // refused and rereads it. The new revision does not lift the guard.
    mockIntegrations().objects[1]!.metadata.resourceVersion = '12';
    fireEvent.click(screen.getByRole('button', { name: 'Check' }));
    expect(await screen.findByText('Check refused')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('button', { name: 'Check' })).not.toHaveAttribute('aria-disabled', 'true'));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Sync now' })).toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(screen.getByRole('button', { name: 'Sync now' }));
    expect(synced).toHaveLength(1);
    // A Check at the current revision succeeds and lifts it.
    fireEvent.click(screen.getByRole('button', { name: 'Check' }));
    expect(await screen.findByText('Reachable: yes. Credential accepted: yes.')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('button', { name: 'Sync now' })).not.toHaveAttribute('aria-disabled', 'true'));
  });

  it('keeps the uncertain-sync hold after a Check that reports the endpoint unreachable or the credential refused', async () => {
    mockIntegrations().objects[1]!.metadata.annotations = { 'kudurru.io/managed-by': 'api' };
    mockIntegrations().uncertainNextSync = true;
    const synced = bodies('/sync');
    detail('office');
    const syncNow = await screen.findByRole('button', { name: 'Sync now' });
    await waitFor(() => expect(syncNow).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(syncNow);
    expect(await screen.findByText('Sync: outcome unknown')).toBeInTheDocument();
    const held = () => expect(screen.getByRole('button', { name: 'Sync now' })).toHaveAttribute('aria-disabled', 'true');
    // 200, but not reachable: the hold stays.
    mockIntegrations().checkResult = { reachable: false, authenticated: false };
    fireEvent.click(screen.getByRole('button', { name: 'Check' }));
    expect(await screen.findByText('Reachable: no. Credential accepted: no.')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('button', { name: 'Check' })).not.toHaveAttribute('aria-disabled', 'true'));
    held();
    // 200, reachable but the credential is refused: the hold stays.
    mockIntegrations().checkResult = { reachable: true, authenticated: false };
    fireEvent.click(screen.getByRole('button', { name: 'Check' }));
    expect(await screen.findByText('Reachable: yes. Credential accepted: no.')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('button', { name: 'Check' })).not.toHaveAttribute('aria-disabled', 'true'));
    held();
    fireEvent.click(screen.getByRole('button', { name: 'Sync now' }));
    expect(synced).toHaveLength(1);
    // Both confirmed: lifted.
    mockIntegrations().checkResult = { reachable: true, authenticated: true };
    fireEvent.click(screen.getByRole('button', { name: 'Check' }));
    expect(await screen.findByText('Reachable: yes. Credential accepted: yes.')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('button', { name: 'Sync now' })).not.toHaveAttribute('aria-disabled', 'true'));
  });

  it('says so when the instance changed since the preview', async () => {
    detail();
    const previewButton = await screen.findByRole('button', { name: 'Preview' });
    await waitFor(() => expect(previewButton).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(previewButton);
    await screen.findByTestId('integration-preview');
    mockIntegrations().objects[0]!.metadata.resourceVersion = '99';
    fireEvent.click(screen.getByRole('button', { name: 'Check' }));
    const banner = await screen.findByTestId('preview-superseded');
    expect(banner).toHaveTextContent('revision 10, now 99');
    expect(screen.queryByTestId('integration-preview')).toBeNull();
    expect(screen.getByRole('button', { name: 'Publish this plan' })).toHaveAttribute('aria-disabled', 'true');
    fireEvent.click(screen.getByRole('button', { name: 'Preview' }));
    await screen.findByTestId('integration-preview');
    expect(screen.queryByTestId('preview-superseded')).toBeNull();
  });

  it('previews and publishes a UniFi record mirror by its digest, then lists its write actions', async () => {
    mockIntegrations().objects.push({
      apiVersion: 'kudurru.io/v1alpha1', kind: 'Integration',
      metadata: { name: 'gateway', namespace: 'default', uid: 'uid-gateway', resourceVersion: '31', annotations: { 'kudurru.io/managed-by': 'api' } },
      spec: { type: 'unifi', credentials: { apiKey: { file: '/run/secrets/unifi-key' } }, unifi: { endpoint: 'https://controller.example.com', legacy: true, push: { dhcpDNSServers: true, dnsServers: ['192.0.2.53'], networks: ['LAN'], clientActions: true, mirrorRecords: { enabled: true, domains: ['example.com'] } } } },
    });
    const synced = bodies('/sync');
    detail('gateway');
    // A reviewed-only publisher: no Sync now, and Publish waits for a preview.
    const publish = await screen.findByRole('button', { name: 'Publish this plan' });
    expect(screen.queryByRole('button', { name: 'Sync now' })).toBeNull();
    const previewButton = screen.getByRole('button', { name: 'Preview' });
    await waitFor(() => expect(previewButton).not.toHaveAttribute('aria-disabled', 'true'));
    expect(publish).toHaveAttribute('aria-disabled', 'true');
    fireEvent.click(previewButton);
    const plan = await screen.findByTestId('integration-preview');
    const digest = '1'.padStart(64, 'a');
    expect(plan).toHaveTextContent(digest);
    expect(within(plan).getByRole('table', { name: 'Planned changes' })).toHaveTextContent('nas.example.com A');
    expect(plan).toHaveTextContent('printer.example.com A: a DNS policy this Integration did not create');
    fireEvent.click(screen.getByRole('button', { name: 'Publish this plan' }));
    expect(await screen.findByText('Sync completed')).toBeInTheDocument();
    expect(synced).toEqual([{ uid: 'uid-gateway', revision: '31', mode: 'publish', previewDigest: digest }]);
    expect(screen.getByRole('button', { name: 'Publish this plan' })).toHaveAttribute('aria-disabled', 'true');
    // The configuration form shows the stored push block.
    fireEvent.click(screen.getByRole('button', { name: 'Edit Configuration' }));
    const push = within(screen.getByTestId('provider-fields')).getByRole('group', { name: /^Push/ });
    expect(within(push).getByRole('switch', { name: /DHCP DNS servers/ })).toBeChecked();
    expect(within(push).getByRole('textbox', { name: 'DNS servers 1' })).toHaveValue('192.0.2.53');
    // The writes the spec enables are the actions it advertises.
    const load = screen.getByRole('button', { name: 'Load actions' });
    await waitFor(() => expect(load).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(load);
    expect(await screen.findByRole('button', { name: 'Run dhcp-dns-servers' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Run block-client' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Run unblock-client' })).toBeInTheDocument();
  });

  it('saves a UniFi instance that sets no inventory zone (real schema)', async () => {
    mockIntegrations().objects[1]!.metadata.annotations = { 'kudurru.io/managed-by': 'api' };
    detail('office');
    fireEvent.click(await screen.findByRole('button', { name: 'Edit Configuration' }));
    fireEvent.click(screen.getByRole('switch', { name: /Suspended/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Save Configuration' }));
    await waitFor(() => expect(mockIntegrations().objects[1]!.spec.suspend).toBe(true));
    expect(mockIntegrations().objects[1]!.spec.unifi).toEqual({ endpoint: 'https://unifi.lan', site: 'default' });
  });

  it('checks, and syncs an observing provider without a digest', async () => {
    mockIntegrations().objects[1]!.metadata.annotations = { 'kudurru.io/managed-by': 'api' };
    const synced = bodies('/sync');
    detail('office');
    expect(screen.queryByRole('button', { name: 'Preview' })).toBeNull();
    const check = await screen.findByRole('button', { name: 'Check' });
    await waitFor(() => expect(check).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(check);
    expect(await screen.findByText('Reachable: yes. Credential accepted: yes.')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Sync now' }));
    expect(await screen.findByText(/clients 23\. Inventory committed\./)).toBeInTheDocument();
    expect(synced).toEqual([{ uid: 'uid-office', revision: '11', mode: 'observe' }]);
  });

  it('shows the written Ready condition while the host reports an older revision', async () => {
    mockIntegrations().status['default/public-dns'] = { statusKnown: false, phase: 'Unknown', reason: 'NoMatchingObservation' };
    detail();
    expect(await screen.findByText(/Ready \(from the object's status; the host has not reported this revision\)/)).toBeInTheDocument();
    // The server decides each command; nothing is held back on the client.
    await waitFor(() => expect(screen.getByRole('button', { name: 'Check' })).not.toHaveAttribute('aria-disabled', 'true'));
  });

  it('refuses a review of a changed revision', async () => {
    detail();
    const check = await screen.findByRole('button', { name: 'Check' });
    await waitFor(() => expect(check).not.toHaveAttribute('aria-disabled', 'true'));
    mockIntegrations().objects[0]!.metadata.resourceVersion = '99';
    fireEvent.click(check);
    expect(await screen.findByText('Check refused')).toBeInTheDocument();
    expect(screen.getByText(/changed since you reviewed it/)).toBeInTheDocument();
  });

  it('shows a terminating instance with its counts and releases it by its exact review', async () => {
    const released = bodies('/release');
    const seen = requests();
    detail();
    fireEvent.click(await screen.findByRole('button', { name: 'Delete' }));
    expect(await screen.findByText(/Its deletion policy is Delete/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Delete integration' }));
    const panel = await screen.findByTestId('terminating');
    expect(within(panel).getByText('Records left').parentElement).toHaveTextContent('3');
    expect(within(panel).getByText('Blocking conflicts').parentElement).toHaveTextContent('1');
    // Commands are refused while terminating, so none is offered.
    expect(screen.queryByRole('button', { name: 'Preview' })).toBeNull();
    expect(screen.getByText('Refused while the Integration is terminating.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Edit Configuration' })).toHaveAttribute('aria-disabled', 'true');
    const release = within(panel).getByRole('button', { name: 'Release without cleanup' });
    await waitFor(() => expect(release).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(release);
    fireEvent.click(within(await screen.findByRole('alertdialog')).getByRole('button', { name: 'Release without cleanup' }));
    await waitFor(() => expect(window.location.pathname).toBe('/integrations'));
    const rv = String(mockIntegrations().revision);
    expect(released).toEqual([{ uid: 'uid-public-dns', revision: rv }]);
    expect(mockIntegrations().objects.some((o) => o.metadata.name === 'public-dns')).toBe(false);
    // The page leaves without reading the removed object again (no 404).
    await new Promise((r) => setTimeout(r, 100));
    const after = seen.slice(seen.findIndex((p) => p.endsWith('/release')) + 1);
    expect(after.filter((p) => p.includes('/integrations/default/public-dns'))).toEqual([]);
  });

  it('keeps a file-managed instance read-only with the reason', async () => {
    detail('office');
    const edit = await screen.findByRole('button', { name: 'Edit Configuration' });
    expect(edit).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByRole('button', { name: 'Delete' })).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByText('file /run/secrets/unifi-key')).toBeInTheDocument();
  });

  it('saves the configuration bound to the stored resourceVersion', async () => {
    const puts = requests();
    detail();
    fireEvent.click(await screen.findByRole('button', { name: 'Edit Configuration' }));
    fireEvent.click(screen.getByRole('switch', { name: /Suspended/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Save Configuration' }));
    await waitFor(() => expect(mockIntegrations().objects[0]!.spec.suspend).toBe(true));
    expect(mockIntegrations().objects[0]!.spec.credentials).toEqual({ apiToken: { env: 'CLOUDFLARE_API_TOKEN' } });
    expect(puts).toContain('PUT /api/v1/objects/integrations/default/public-dns');
  });

  it('gates the primary-only commands on a follower and keeps the forwarded writes', async () => {
    const { forwarded, local } = documentLists();
    cluster({ role: 'follower', primary: 'kudurru-0', primaryRequired: true, forwardedOperations: forwarded, localOperations: local });
    detail();
    const check = await screen.findByRole('button', { name: 'Check' });
    await waitFor(() => expect(check).toHaveAttribute('aria-disabled', 'true'));
    for (const name of ['Check', 'Preview', 'Publish this plan']) expect(screen.getByRole('button', { name })).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByRole('button', { name: 'Edit Configuration' })).not.toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByRole('button', { name: 'Delete' })).not.toHaveAttribute('aria-disabled', 'true');
  });

  it('offers no sync, write or release in static mode', async () => {
    cluster({ readOnly: true });
    mockIntegrations().objects[0]!.metadata.deletionTimestamp = new Date().toISOString();
    detail();
    const release = await screen.findByRole('button', { name: 'Release without cleanup' });
    await waitFor(() => expect(release).toHaveAttribute('aria-disabled', 'true'));
    expect(screen.getByRole('button', { name: 'Edit Configuration' })).toHaveAttribute('aria-disabled', 'true');
  });

  it('lists and runs an advertised action with its schema parameters', async () => {
    mockIntegrations().objects.push({
      apiVersion: 'kudurru.io/v1alpha1', kind: 'Integration',
      metadata: { name: 'peer', namespace: 'default', uid: 'uid-peer', resourceVersion: '30', annotations: { 'kudurru.io/managed-by': 'api' } },
      spec: { type: 'webhook', webhook: { endpoint: 'http://127.0.0.1:9' }, credentials: { bearerToken: { file: '/run/secrets/webhook-token' } } },
    });
    const acted = bodies('/actions/refresh');
    detail('peer');
    const load = await screen.findByRole('button', { name: 'Load actions' });
    await waitFor(() => expect(load).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(load);
    fireEvent.click(await screen.findByRole('switch', { name: /Full/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Run refresh' }));
    expect(await screen.findByText('Action completed')).toBeInTheDocument();
    expect(acted).toEqual([{ uid: 'uid-peer', revision: '30', params: { full: true } }]);
  });
});
