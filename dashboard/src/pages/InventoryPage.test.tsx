import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { ApiError } from '../api/client';
import type { IntegrationSummary } from '../api/types';
import { NO_ROLE } from '../lib/follower';
import { ADDRESS_STATE_TEXT, integrationsBySource, inventoryUnavailable, sourceRef, sqliteClusterMember } from '../lib/inventory';
import { mockInventory, OFFICE_SOURCE } from '../mocks/inventory';
import { getSelector, pick } from '../test/controls';
import { server } from '../test/setup';
import { API, renderWithApp } from '../test/utils';
import ClientsPage from './ClientsPage';
import InventoryPage from './InventoryPage';

const rowOf = async (name: string) => (await screen.findByText(name, { selector: 'a, span, p' })).closest('tr')!;

describe('InventoryPage', () => {
  it('lists every inventory device and lease with MACs, address states in words, and links to the client page and the reporting Integration', async () => {
    renderWithApp(<InventoryPage />, { route: '/inventory' });
    const table = await screen.findByRole('table', { name: 'Inventory devices' });
    // A device that never queried is listed (thermostat is not in GET /clients).
    const thermostat = await rowOf('thermostat');
    expect(within(thermostat).getByText('aa:bb:cc:dd:ee:60')).toBeInTheDocument();
    expect(within(thermostat).getByText('Ambiguous')).toBeInTheDocument();
    expect(within(thermostat).queryByRole('link', { name: '192.168.1.60' })).not.toBeInTheDocument();
    // The UniFi device reported by default/office links to that Integration.
    expect(within(thermostat).getByRole('link', { name: 'Integration office' })).toHaveAttribute('href', '/integrations/default/office');
    // kids-tablet queried (GET /clients knows 192.168.1.30): its name and address open the client page.
    const tablet = within(table).getByText('tags kids').closest('tr')!;
    expect(within(tablet).getByRole('link', { name: 'kids-tablet' })).toHaveAttribute('href', '/clients/192.168.1.30');
    expect(within(tablet).getByRole('link', { name: '192.168.1.30' })).toHaveAttribute('href', '/clients/192.168.1.30');
    expect(within(tablet).getByText('Assigned')).toBeInTheDocument();
    // The lease of the same address is superseded by the UniFi MAC and names its owner.
    const lease = within(table).getByText(/^Superseded by/).closest('tr')!;
    expect(within(lease).getByText(`Superseded by default/aa:bb:cc:dd:ee:30 of ${OFFICE_SOURCE}`)).toBeInTheDocument();
    expect(within(lease).getByRole('link', { name: 'DHCP scope lan' })).toHaveAttribute('href', '/dhcp/lan');
    expect(within(lease).getByRole('link', { name: 'kids-tablet' })).toHaveAttribute('href', '/clients/192.168.1.30');
    // A stale device is the only badge; others read Live.
    const old = await rowOf('old-laptop');
    expect(within(old).getByText('Stale')).toBeInTheDocument();
    expect(within(old).getByText('Expired')).toBeInTheDocument();
    expect(within(thermostat).getByText('Live')).toBeInTheDocument();
    expect(screen.getByTestId('inventory-meta')).toHaveTextContent('Answered by node kudurru-0');
    expect(table.textContent).not.toMatch(/undefined|NaN/);
  });

  it('sends every filter to the server and keeps it in the URL', async () => {
    renderWithApp(<InventoryPage />, { route: '/inventory' });
    await screen.findByRole('table', { name: 'Inventory devices' });
    pick(getSelector('Kind'), 'Integration devices');
    await waitFor(() => expect(mockInventory().requests.at(-1)?.get('kind')).toBe('inventory'));
    expect(window.location.search).toContain('kind=inventory');
    expect(await screen.findByText('thermostat')).toBeInTheDocument();
    expect(screen.queryByText('printer')).not.toBeInTheDocument();
    pick(getSelector('Staleness'), 'Stale only');
    await waitFor(() => expect(mockInventory().requests.at(-1)?.get('stale')).toBe('true'));
    await waitFor(() => expect(screen.queryByText('thermostat')).not.toBeInTheDocument());
    expect(screen.getByText('old-laptop')).toBeInTheDocument();
    pick(getSelector('Staleness'), 'Live and stale');
    pick(getSelector('Source'), 'Integration office');
    await waitFor(() => expect(mockInventory().requests.at(-1)?.get('source')).toBe(OFFICE_SOURCE));
    fireEvent.change(screen.getByRole('textbox', { name: /Find a device/ }), { target: { value: 'ee:61' } });
    await waitFor(() => expect(mockInventory().requests.at(-1)?.get('q')).toBe('ee:61'));
    await waitFor(() => expect(screen.queryByText('thermostat')).not.toBeInTheDocument());
    expect(screen.getByText('doorbell')).toBeInTheDocument();
    fireEvent.change(screen.getByRole('textbox', { name: /Find a device/ }), { target: { value: 'nothing-here' } });
    expect(await screen.findByText('No device matches these filters.')).toBeInTheDocument();
  });

  it('reads the next page with the continue token', async () => {
    const devices = mockInventory().devices;
    for (let i = 0; i < 205; i++) {
      devices.push({ source: 'dhcp/guest', kind: 'dhcp', id: `10.9.${Math.floor(i / 250)}.${i}`, macs: [`02:00:00:00:00:${(i % 256).toString(16).padStart(2, '0')}`], tags: [], addresses: [{ ip: `10.9.0.${i}`, state: 'assigned' }], expiresAt: new Date(Date.now() + 3_600_000).toISOString(), stale: false });
    }
    renderWithApp(<InventoryPage />, { route: '/inventory' });
    const more = await screen.findByRole('button', { name: 'Load more devices' });
    expect(mockInventory().requests.at(-1)?.get('limit')).toBe('200');
    fireEvent.click(more);
    await waitFor(() => expect(mockInventory().requests.at(-1)?.get('continue')).toBeTruthy());
    expect(await screen.findByText('old-laptop')).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Load more devices' })).not.toBeInTheDocument());
  });

  it('asks nothing without inventory.read and says what is needed (viewer)', async () => {
    renderWithApp(<InventoryPage />, { route: '/inventory', user: 'viewer' });
    expect(await screen.findByText('The inventory is restricted')).toBeInTheDocument();
    expect(mockInventory().requests).toHaveLength(0);
  });

  it('shows the source ID when the session cannot read integrations (auditor)', async () => {
    renderWithApp(<InventoryPage />, { route: '/inventory', user: 'auditor' });
    const thermostat = await rowOf('thermostat');
    expect(within(thermostat).queryByRole('link', { name: /Integration/ })).not.toBeInTheDocument();
    expect(within(thermostat).getByText(OFFICE_SOURCE)).toBeInTheDocument();
  });

  it('shows the two 503 answers as what they are', async () => {
    mockInventory().unavailable = true;
    const { unmount } = renderWithApp(<InventoryPage />, { route: '/inventory' });
    expect(await screen.findByText('The serving inventory is unavailable')).toBeInTheDocument();
    expect(screen.queryByRole('table', { name: 'Inventory devices' })).not.toBeInTheDocument();
    unmount();
    mockInventory().unavailable = false;
    mockInventory().unmounted = true;
    renderWithApp(<InventoryPage />, { route: '/inventory' });
    expect(await screen.findByText('This server has no client inventory')).toBeInTheDocument();
  });

  it('says on a SQLite cluster member that leases and lease MACs are this node’s own', async () => {
    renderWithApp(<InventoryPage />, { route: '/inventory' });
    expect(await screen.findByText('DHCP leases are this node’s own')).toBeInTheDocument();
    expect(await screen.findByText(/come from node kudurru-0 only/)).toBeInTheDocument();
  });

  it('shows the caveat to an auditor on a cluster primary, who cannot read GET /cluster (as an operator)', async () => {
    // permissions.yaml gives cluster.read to admin only (the mock viewer's
    // matrix has it), so GET /cluster answers 403 here as on the real server.
    server.use(http.get(`${API}/cluster`, () => HttpResponse.json({ error: { key: 'forbidden', message: 'You may not read cluster' }, took_ms: 1 }, { status: 403 })));
    renderWithApp(<InventoryPage />, { route: '/inventory', user: 'auditor' });
    expect(await screen.findByText('DHCP leases are this node’s own')).toBeInTheDocument();
    expect(screen.queryByText(/DHCP leases are those of node/)).not.toBeInTheDocument();
  });

  it('shows no cluster caveat on a single node, only whose leases they are', async () => {
    mockInventory().nodeLocalLeases = false;
    server.use(http.get(`${API}/cluster`, () => HttpResponse.json({ driver: 'kube', epoch: 1, nodes: 1, node: 'kudurru-0', readOnly: false, primaryRequired: false, took_ms: 1 })));
    renderWithApp(<InventoryPage />, { route: '/inventory' });
    expect(await screen.findByText('DHCP leases are those of node kudurru-0.')).toBeInTheDocument();
    expect(screen.queryByText('DHCP leases are this node’s own')).not.toBeInTheDocument();
  });

  it('lists the inventory on an enrolled follower and on a static host (a read, served by the node itself)', async () => {
    server.use(http.get(`${API}/cluster`, () => HttpResponse.json({ driver: 'sqlite', epoch: 7, nodes: 2, node: 'kudurru-1', role: 'follower', primary: 'kudurru-0', primaryRequired: true, forwardedOperations: [], localOperations: [], readOnly: false, took_ms: 1 })));
    const { unmount } = renderWithApp(<InventoryPage />, { route: '/inventory' });
    expect(await screen.findByText('thermostat')).toBeInTheDocument();
    expect(screen.getByText('DHCP leases are this node’s own')).toBeInTheDocument();
    unmount();
    server.use(http.get(`${API}/cluster`, () => HttpResponse.json({ driver: 'sqlite', epoch: 1, nodes: 1, node: 'kudurru-0', readOnly: true, primaryRequired: false, took_ms: 1 })));
    renderWithApp(<InventoryPage />, { route: '/inventory' });
    expect(await screen.findByText('thermostat')).toBeInTheDocument();
  });

  it('is linked from Clients for sessions that may read it', async () => {
    const { unmount } = renderWithApp(<ClientsPage />, { route: '/clients' });
    expect(await screen.findByRole('link', { name: 'Inventory' })).toHaveAttribute('href', '/inventory');
    unmount();
    renderWithApp(<ClientsPage />, { route: '/clients', user: 'viewer' });
    await screen.findByRole('table', { name: 'Clients' });
    expect(screen.queryByRole('link', { name: 'Inventory' })).not.toBeInTheDocument();
  });
});

describe('inventory helpers', () => {
  it('names every address state the API defines', () => {
    expect(Object.keys(ADDRESS_STATE_TEXT).sort()).toEqual(['ambiguous', 'assigned', 'expired', 'superseded', 'withdrawn']);
  });

  it('maps sources to Integrations and DHCP scopes', () => {
    const s = { namespace: 'apps', name: 'lab', inventorySource: 'integration-x' } as IntegrationSummary;
    const by = integrationsBySource([s, { namespace: 'default', name: 'cf' } as IntegrationSummary]);
    expect(by.size).toBe(1);
    const href = (ns: string, n: string) => `/integrations/${ns}/${n}`;
    expect(sourceRef('integration-x', by, href)).toEqual({ label: 'Integration apps/lab', href: '/integrations/apps/lab' });
    expect(sourceRef('dhcp/guest wifi', by, href)).toEqual({ label: 'DHCP scope guest wifi', href: '/dhcp/guest%20wifi' });
    expect(sourceRef('integration-gone', by, href)).toEqual({ label: 'integration-gone' });
  });

  it('decides the node-local caveat from GET /inventory, GET /cluster or the follower status', () => {
    const base = { epoch: 1, node: 'a', readOnly: false, primaryRequired: false, took_ms: 0 };
    expect(sqliteClusterMember({ ...base, driver: 'sqlite', nodes: 2 }, NO_ROLE)).toBe(true);
    expect(sqliteClusterMember({ ...base, driver: 'sqlite', nodes: 1, role: 'primary' }, NO_ROLE)).toBe(true);
    expect(sqliteClusterMember({ ...base, driver: 'sqlite', nodes: 1, role: 'standalone' }, NO_ROLE)).toBe(false);
    expect(sqliteClusterMember({ ...base, driver: 'kube', nodes: 3 }, NO_ROLE)).toBe(false);
    expect(sqliteClusterMember(undefined, { ...NO_ROLE, follower: true })).toBe(true);
    expect(sqliteClusterMember(undefined, NO_ROLE)).toBe(false);
    // GET /inventory's own nodeLocalLeases needs no cluster.read.
    expect(sqliteClusterMember(undefined, NO_ROLE, true)).toBe(true);
    expect(sqliteClusterMember({ ...base, driver: 'kube', nodes: 1 }, NO_ROLE, false)).toBe(false);
  });

  it('tells the two 503 answers apart and ignores every other error', () => {
    expect(inventoryUnavailable(new ApiError(503, 'unavailable', 'the client inventory is not mounted on this server'))).toBe('unmounted');
    expect(inventoryUnavailable(new ApiError(503, 'unavailable', 'the serving inventory is unavailable'))).toBe('unavailable');
    expect(inventoryUnavailable(new ApiError(503, 'cluster_fenced', 'fenced'))).toBeUndefined();
    expect(inventoryUnavailable(new ApiError(403, 'forbidden', 'no'))).toBeUndefined();
  });
});
