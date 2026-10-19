import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { mockDHCP, mockDHCPSetup, mockKubeDHCP } from '../mocks/dhcp';
import { server } from '../test/setup';
import { renderWithApp } from '../test/utils';
import DHCPLeasesPage from './DHCPLeasesPage';
import DHCPPage from './DHCPPage';
import DHCPScopePage from './DHCPScopePage';

const scopePage = () => <DHCPScopePage params={{ scope: 'default' }} />;

describe('DHCP dashboard', () => {
  it('lists the connected node scopes and explains disabled setup', async () => {
    const view = renderWithApp(<DHCPPage />, { route: '/dhcp' });
    expect(await screen.findByText('Connected node: stone-1')).toBeInTheDocument();
    expect(await screen.findByRole('link', { name: 'default' })).toHaveAttribute('href', '/dhcp/default');
    view.unmount();
    mockDHCP().enabled = false;
    renderWithApp(<DHCPPage />, { route: '/dhcp' });
    expect(await screen.findByText('DHCP is disabled on this node')).toBeInTheDocument();
  });

  it('saves network options with the current ETag and retains them on reload', async () => {
    const view = renderWithApp(scopePage());
    fireEvent.change(await screen.findByRole('textbox', { name: 'Router' }), { target: { value: '192.0.2.254' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save network options' }));
    await waitFor(() => expect(mockDHCP().config.options.router).toBe('192.0.2.254'));
    expect(mockDHCP().config.revision).toBe('2');
    view.unmount();
    renderWithApp(scopePage());
    expect(await screen.findByRole('textbox', { name: 'Router' })).toHaveValue('192.0.2.254');
  });

  it('rejects a stale save without losing the draft or overwriting saved state', async () => {
    renderWithApp(scopePage());
    fireEvent.change(await screen.findByRole('textbox', { name: 'Router' }), { target: { value: '192.0.2.254' } });
    mockDHCP().config.etag = '"newer-revision"';
    fireEvent.click(screen.getByRole('button', { name: 'Save network options' }));
    expect(await screen.findByText(/The DHCP scope changed/)).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'Router' })).toHaveValue('192.0.2.254');
    expect(mockDHCP().config.options.router).toBe('192.0.2.1');
    fireEvent.click(await screen.findByRole('button', { name: 'Reload saved values' }));
    expect(screen.getByRole('textbox', { name: 'Router' })).toHaveValue('192.0.2.1');
  });

  it('adds a reservation and refuses to take another client’s occupied address', async () => {
    renderWithApp(scopePage());
    fireEvent.click(await screen.findByRole('button', { name: 'Add reservation' }));
    const dialog = within(screen.getByRole('dialog', { name: 'Add reservation' }));
    fireEvent.change(dialog.getByRole('textbox', { name: 'MAC address' }), { target: { value: '02:00:00:00:00:30' } });
    fireEvent.change(dialog.getByRole('textbox', { name: 'Reserved address' }), { target: { value: '192.0.2.100' } });
    fireEvent.change(dialog.getByRole('textbox', { name: 'Hostname' }), { target: { value: 'camera' } });
    fireEvent.click(dialog.getByRole('button', { name: 'Save reservation' }));
    expect(await dialog.findByText('The address is held by another client')).toBeInTheDocument();
    fireEvent.change(dialog.getByRole('textbox', { name: 'Reserved address' }), { target: { value: '192.0.2.30' } });
    fireEvent.click(dialog.getByRole('button', { name: 'Save reservation' }));
    expect(await within(screen.getByRole('table', { name: 'DHCP reservations' })).findByText('camera')).toBeInTheDocument();
    expect(mockDHCP().leases).toHaveLength(2);
  });

  it('confirms reservation removal and keeps its active lease', async () => {
    renderWithApp(scopePage());
    fireEvent.click(await screen.findByRole('button', { name: 'Remove 02:00:00:00:00:10' }));
    const dialog = within(screen.getByRole('dialog', { name: 'Remove reservation' }));
    expect(dialog.getByText(/active lease will remain/)).toBeInTheDocument();
    fireEvent.click(dialog.getByRole('button', { name: 'Remove reservation' }));
    await waitFor(() => expect(mockDHCP().config.reservations).toHaveLength(0));
    expect(mockDHCP().leases.find((l) => l.address === '192.0.2.10')).toBeDefined();
  });

  it('links a running scope to its leases', async () => {
    renderWithApp(scopePage());
    expect(await screen.findByRole('link', { name: 'View leases' })).toHaveAttribute('href', '/leases?scope=default');
    expect(screen.queryByRole('table', { name: 'DHCP leases' })).not.toBeInTheDocument();
  });

  it('filters lease inventory and reports import results', async () => {
    renderWithApp(<DHCPLeasesPage />, { route: '/leases?scope=default' });
    const table = await screen.findByRole('table', { name: 'DHCP leases' });
    expect(within(table).getByText('printer')).toBeInTheDocument();
    fireEvent.change(screen.getByRole('textbox', { name: 'Find a lease' }), { target: { value: 'laptop' } });
    await waitFor(() => expect(within(table).queryByText('printer')).not.toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: 'Import leases' }));
    const dialog = within(screen.getByRole('dialog', { name: 'Import leases' }));
    fireEvent.change(dialog.getByRole('textbox', { name: 'Lease data' }), { target: { value: '{"version":1,"leases":[]}' } });
    fireEvent.click(dialog.getByRole('button', { name: 'Import leases' }));
    expect(await dialog.findByText('Lease import complete')).toBeInTheDocument();
    expect(dialog.getByText(/1 imported, 0 unchanged/)).toBeInTheDocument();
  });

  it('reserves a lease’s client, at its address or another one', async () => {
    renderWithApp(<DHCPLeasesPage />, { route: '/leases?scope=default' });
    const table = within(await screen.findByRole('table', { name: 'DHCP leases' }));
    // The printer is already reserved at the address it holds.
    expect(await table.findByRole('button', { name: 'Edit 02:00:00:00:00:10' })).toHaveAttribute('aria-disabled', 'true');
    fireEvent.click(table.getByRole('button', { name: 'Reserve 192.0.2.100' }));
    const dialog = within(screen.getByRole('dialog', { name: 'Add reservation' }));
    expect(dialog.getByRole('textbox', { name: 'MAC address' })).toHaveValue('02:00:00:00:00:20');
    expect(dialog.getByRole('textbox', { name: 'MAC address' })).toBeDisabled();
    expect(dialog.getByRole('textbox', { name: 'Hostname' })).toHaveValue('laptop');
    fireEvent.change(dialog.getByRole('textbox', { name: 'Reserved address' }), { target: { value: '192.0.2.40' } });
    fireEvent.click(dialog.getByRole('button', { name: 'Save reservation' }));
    await waitFor(() => expect(mockDHCP().config.reservations).toContainEqual({ mac: '02:00:00:00:00:20', ip: '192.0.2.40', hostname: 'laptop' }));
    expect(await table.findByRole('button', { name: 'Edit 02:00:00:00:00:20' })).not.toHaveAttribute('aria-disabled', 'true');
  });

  it('keeps file-owned and environment-owned options read-only', async () => {
    mockDHCP().config.management = 'file';
    mockDHCP().config.envLocked = ['dnsServers'];
    renderWithApp(scopePage());
    expect((await screen.findAllByText('Managed by the configuration file')).length).toBeGreaterThan(0);
    expect(screen.getByRole('textbox', { name: 'Router' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Add reservation' })).toHaveAttribute('aria-disabled', 'true');
  });

  it('keeps lease import available for a file-owned scope', async () => {
    mockDHCP().config.management = 'file';
    renderWithApp(<DHCPLeasesPage />, { route: '/leases' });
    await screen.findByRole('table', { name: 'DHCP leases' });
    expect(screen.getByRole('button', { name: 'Import leases' })).not.toBeDisabled();
  });

  it('explains a requested scope that is not running', async () => {
    renderWithApp(<DHCPLeasesPage />, { route: '/leases?scope=guest' });
    expect(await screen.findByText('Scope guest is not running')).toBeInTheDocument();
    expect(await screen.findByRole('table', { name: 'DHCP leases' })).toBeInTheDocument();
  });

  it('says when no scope is running', async () => {
    mockDHCP().enabled = false;
    renderWithApp(<DHCPLeasesPage />, { route: '/leases' });
    expect(await screen.findByText('No DHCP scope is running')).toBeInTheDocument();
  });

  it('does not request protected lease data for a viewer', async () => {
    let leaseRequests = 0;
    server.use(http.get('*/api/v1/node/dhcp/v4/scopes/default/leases', () => { leaseRequests++; return HttpResponse.json({ items: [] }); }));
    const view = renderWithApp(scopePage(), { user: 'viewer' });
    expect(await screen.findByRole('button', { name: 'Save network options' })).toHaveAttribute('aria-disabled', 'true');
    view.unmount();
    renderWithApp(<DHCPLeasesPage />, { route: '/leases', user: 'viewer' });
    expect(await screen.findByText('Viewing lease inventory requires dhcpleases.read permission.')).toBeInTheDocument();
    expect(leaseRequests).toBe(0);
  });
});

describe('DHCP dashboard in Kubernetes mode', () => {
  it('shows the serving replica, relay address and runtime problems instead of node texts', async () => {
    mockKubeDHCP({ runtimeError: 'listen eth0: address already in use', pendingScopes: ['guest', 'iot'] });
    const [lan] = mockDHCPSetup().scopes;
    if (!lan) throw new Error('fixture scope missing');
    mockDHCPSetup().scopes.push({ ...lan, name: 'guest', subnet: '198.51.100.0/24' }, { ...lan, name: 'iot', subnet: '203.0.113.0/24' });
    mockDHCP().status = { ...mockDHCP().status, invalidScopes: { iot: 'subnet overlaps scope lan' } };
    renderWithApp(<DHCPPage />, { route: '/dhcp' });
    expect(await screen.findByText('Serving replica: kudurru-1 · Relays send to 10.200.200.67')).toBeInTheDocument();
    expect(screen.queryByText(/Connected node/)).not.toBeInTheDocument();
    expect(await screen.findByText('DHCP reports an error')).toBeInTheDocument();
    expect(screen.getByText('listen eth0: address already in use')).toBeInTheDocument();
    expect(screen.getByText('Some scopes are not served')).toBeInTheDocument();
    expect(screen.getByText('iot: subnet overlaps scope lan')).toBeInTheDocument();
    const table = within(screen.getByRole('table', { name: 'DHCP scopes' }));
    expect(table.getByRole('row', { name: /^default/ })).toHaveTextContent('Running');
    expect(table.getByRole('row', { name: /^guest/ })).toHaveTextContent('Applying');
    expect(table.getByRole('row', { name: /^iot/ })).toHaveTextContent('Not served');
    for (const text of ['Restart required', 'DHCP is disabled on this node', 'DHCP could not start', 'Automatic failover', 'Manage in dashboard', 'DHCP is controlled by environment variables'])
      expect(screen.queryByText(text)).not.toBeInTheDocument();
  });

  it('says when no replica is serving', async () => {
    mockKubeDHCP();
    mockDHCP().status = { ...mockDHCP().status, servingNode: '' };
    renderWithApp(<DHCPPage />, { route: '/dhcp' });
    expect(await screen.findByText('No replica is serving · Relays send to 10.200.200.67')).toBeInTheDocument();
  });
});
