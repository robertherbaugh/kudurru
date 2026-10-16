import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { delay, http, HttpResponse } from 'msw';
import { server } from '../test/setup';
import { mockDHCP, mockDHCPSetup } from '../mocks/dhcp';
import { renderWithApp } from '../test/utils';
import DHCPPage from './DHCPPage';
import DHCPScopePage from './DHCPScopePage';

const change = (name: string, value: string) => fireEvent.change(screen.getByRole('textbox', { name }), { target: { value } });
const next = () => fireEvent.click(screen.getByRole('button', { name: 'Next' }));

describe('DHCP setup', () => {
  it('creates a scope from an empty dashboard without activating the service', async () => {
    mockDHCP().enabled = false;
    mockDHCP().setup = { ...mockDHCPSetup(), scopes: [], scopeRevisions: {} };
    renderWithApp(<DHCPPage />, { route: '/dhcp' });
    fireEvent.click(await screen.findByRole('button', { name: 'New scope' }));
    expect(await screen.findByRole('dialog', { name: 'New DHCP scope' })).toBeInTheDocument();
    change('Scope name', 'lan');
    fireEvent.click(await screen.findByRole('combobox', { name: 'Host interface' }));
    fireEvent.click(await screen.findByRole('option', { name: /eth0/ }));
    change('Server address', '192.0.2.2');
    change('Client subnet', '192.0.2.0/24');
    next();
    await screen.findByRole('heading', { name: 'Addresses' });
    change('First address', '192.0.2.100'); change('Last address', '192.0.2.199'); change('Router', '192.0.2.1');
    fireEvent.click(screen.getByRole('button', { name: 'Add DNS server' }));
    change('DNS servers 1', '192.0.2.2');
    next();
    await screen.findByRole('heading', { name: 'DNS' });
    change('Domain name', 'lan.home.arpa');
    next();
    await screen.findByRole('heading', { name: 'Review' });
    fireEvent.click(screen.getByRole('button', { name: 'Save scope' }));
    await waitFor(() => expect(mockDHCP().setup?.scopes[0]?.name).toBe('lan'));
    expect(mockDHCP().enabled).toBe(false);
    expect(mockDHCP().setup?.enabled).toBe(false);
    expect(mockDHCP().setup?.pendingScopes).toEqual(['lan']);
    expect(window.location.pathname).toBe('/dhcp/lan');
  });

  it('waits for runtime status before choosing the active scope editor', async () => {
    server.use(http.get('*/api/v1/node/dhcp/v4/status', async () => {
      await delay(100);
      const c = mockDHCP().config;
      return HttpResponse.json({ node: c.node, enabled: true, available: true, took_ms: 1,
        scopes: [{ name: c.scope, management: c.management, revision: c.revision, ...c.topology }] });
    }));
    renderWithApp(<DHCPScopePage params={{ scope: 'default' }} />);
    expect(await screen.findByRole('button', { name: 'Save network options' })).toBeInTheDocument();
    expect(screen.queryByRole('form', { name: 'Saved scope setup' })).not.toBeInTheDocument();
    expect(screen.getAllByRole('textbox', { name: 'Router' })).toHaveLength(1);
  });

  it('discovers activation while the scope page stays open', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    mockDHCP().enabled = false;
    const view = renderWithApp(<DHCPScopePage params={{ scope: 'default' }} />);
    try {
      await screen.findByRole('button', { name: 'Save setup for restart' });
      expect(screen.queryByRole('heading', { name: 'Running scope' })).not.toBeInTheDocument();
      mockDHCP().enabled = true;
      await act(() => vi.advanceTimersByTimeAsync(15_000));
      expect(await screen.findByRole('heading', { name: 'Running scope' })).toBeInTheDocument();
      expect(await screen.findByRole('button', { name: 'Save network options' })).toBeInTheDocument();
    } finally { view.unmount(); vi.useRealTimers(); }
  });

  it('refreshes live configuration ownership after an adoption restart', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
    mockDHCP().config.management = 'file';
    const view = renderWithApp(<DHCPScopePage params={{ scope: 'default' }} />);
    try {
      expect(await screen.findByRole('textbox', { name: 'Router' })).toBeDisabled();
      mockDHCP().config.management = 'api';
      mockDHCP().config.revision = '2';
      mockDHCP().config.etag = '"adopted-2"';
      await act(() => vi.advanceTimersByTimeAsync(15_000));
      await waitFor(() => expect(screen.getByRole('textbox', { name: 'Router' })).not.toBeDisabled());
      fireEvent.click(screen.getByRole('button', { name: 'Reload saved values' }));
      expect(screen.getByRole('button', { name: 'Save network options' })).not.toHaveAttribute('aria-disabled', 'true');
    } finally { view.unmount(); vi.useRealTimers(); }
  });

  it('stages enable changes and keeps actual running status separate', async () => {
    mockDHCP().enabled = false;
    renderWithApp(<DHCPPage />);
    fireEvent.click(await screen.findByRole('switch', { name: 'Enable DHCP after restart' }));
    expect(await screen.findByText('Restart required')).toBeInTheDocument();
    expect(mockDHCP().setup?.enabled).toBe(true);
    expect(mockDHCP().enabled).toBe(false);
    expect(screen.getByText('DHCP is disabled on this node')).toBeInTheDocument();
    expect(screen.getByText('Saved, not running')).toBeInTheDocument();
    expect(screen.queryByText('Running')).not.toBeInTheDocument();
  });

  it('requires explicit adoption before changing a file-owned setup', async () => {
    mockDHCP().config.management = 'file';
    renderWithApp(<DHCPPage />);
    expect(await screen.findByRole('button', { name: 'New scope' })).toHaveAttribute('aria-disabled', 'true');
    fireEvent.click(screen.getByRole('button', { name: 'Manage in dashboard' }));
    const dialog = within(await screen.findByRole('dialog', { name: 'Manage DHCP in the dashboard' }));
    expect(mockDHCP().setup).toBeUndefined();
    fireEvent.click(dialog.getByRole('button', { name: 'Adopt setup' }));
    await waitFor(() => expect(mockDHCP().setup?.managedBy).toBe('api'));
    expect(mockDHCP().config.management).toBe('file');
    expect(mockDHCP().setup?.scopes[0]?.reservations).toEqual(mockDHCP().config.reservations);
    expect(await screen.findByText('Restart required')).toBeInTheDocument();
  });

  it('edits a disabled existing scope, including reservations, with saved revision guards', async () => {
    mockDHCP().enabled = false;
    renderWithApp(<DHCPScopePage params={{ scope: 'default' }} />);
    await screen.findByRole('textbox', { name: 'Domain name' });
    change('Domain name', 'new.home.arpa'); change('First address', '192.0.2.110');
    const reservations = screen.getByRole('group', { name: 'Reservations' });
    fireEvent.change(within(reservations).getByRole('textbox', { name: 'Hostname' }), { target: { value: 'new-printer' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save setup for restart' }));
    await waitFor(() => expect(mockDHCP().setup?.scopes[0]?.domainName).toBe('new.home.arpa'));
    expect(mockDHCP().setup?.scopes[0]?.reservations[0]?.hostname).toBe('new-printer');
    expect(mockDHCP().config.options.domainName).toBe('lan.home.arpa');
    expect(screen.queryByRole('button', { name: 'Save network options' })).not.toBeInTheDocument();
  });

  it('retains the staged draft on a stale live revision and reloads explicitly', async () => {
    mockDHCP().enabled = false;
    renderWithApp(<DHCPScopePage params={{ scope: 'default' }} />);
    await screen.findByRole('textbox', { name: 'Domain name' });
    change('Domain name', 'draft.home.arpa');
    mockDHCP().config.revision = '2';
    mockDHCP().config.options.domainName = 'elsewhere.home.arpa';
    fireEvent.click(screen.getByRole('button', { name: 'Save setup for restart' }));
    expect(await screen.findByText(/The saved DHCP setup changed;/)).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'Domain name' })).toHaveValue('draft.home.arpa');
    fireEvent.click(await screen.findByRole('button', { name: 'Reload saved setup' }));
    expect(screen.getByRole('textbox', { name: 'Domain name' })).toHaveValue('elsewhere.home.arpa');
  });

  it('keeps a failed desired setup editable and confirms scope removal', async () => {
    mockDHCP().enabled = false;
    mockDHCP().setup = { ...mockDHCPSetup(), enabled: true, runtimeError: 'listen eth0: permission denied', restartRequired: true };
    renderWithApp(<DHCPScopePage params={{ scope: 'default' }} />);
    expect(await screen.findByText('DHCP could not start')).toBeInTheDocument();
    expect(screen.getByText('listen eth0: permission denied')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Save setup for restart' })).not.toBeDisabled();
    fireEvent.click(screen.getByRole('button', { name: 'Remove scope' }));
    const dialog = within(await screen.findByRole('dialog', { name: 'Remove scope default' }));
    expect(dialog.getByText(/lease database is retained/)).toBeInTheDocument();
    fireEvent.click(dialog.getByRole('button', { name: 'Remove scope' }));
    await waitFor(() => expect(mockDHCP().setup?.scopes).toEqual([]));
    expect(mockDHCP().setup?.enabled).toBe(false);
    expect(mockDHCP().leases).toHaveLength(2);
  });

  it('disables setup writes for viewers and environment-owned nodes', async () => {
    const view = renderWithApp(<DHCPPage />, { user: 'viewer' });
    expect(await screen.findByRole('button', { name: 'New scope' })).toHaveAttribute('aria-disabled', 'true');
    view.unmount();
    mockDHCP().config.envLocked = ['dnsServers'];
    renderWithApp(<DHCPPage />);
    expect(await screen.findByText('DHCP is controlled by environment variables')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'New scope' })).toHaveAttribute('aria-disabled', 'true');
  });
});
