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
    expect(within(fields).queryByRole('group', { name: /Push/ })).toBeNull();
    expect(within(fields).getByTestId('refused-fields')).toHaveTextContent('spec.unifi.inventory');
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
