import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import type { DHCPFailover } from '../api/types';
import { mockDHCP, mockDHCPSetup } from '../mocks/dhcp';
import { renderWithApp } from '../test/utils';
import DHCPPage from './DHCPPage';
import DHCPScopePage from './DHCPScopePage';

const change = (label: string, value: string) => fireEvent.change(screen.getByRole('textbox', { name: label }), { target: { value } });
const failover = (): DHCPFailover => ({ enabled: true, clusterID: 'home-dhcp', nodeID: 'node1', listenAddress: '0.0.0.0:7447',
  peers: [{ id: 'node1', address: '192.0.2.2:7447' }, { id: 'node2', address: '192.0.2.3:7447' }, { id: 'node3', address: '192.0.2.4:7447' }],
  bootstrap: false, caFile: '', certFile: '', keyFile: '' });
function configure(enabled = true) {
  const setup = mockDHCPSetup();
  mockDHCP().setup = { ...setup, failover: { ...failover(), enabled }, scopes: setup.scopes.map((s) => ({ ...s, failoverServers: [
    { nodeID: 'node1', address: '192.0.2.2' }, { nodeID: 'node2', address: '192.0.2.3' }, { nodeID: 'node3', address: '192.0.2.4' },
  ] })) };
}
const openEditor = async () => fireEvent.click(await screen.findByRole('button', { name: 'Configure failover' }));

describe('DHCP automatic failover', () => {
  it('requires explicit bootstrap and saves all three voters and scope addresses together', async () => {
    renderWithApp(<DHCPPage />);await openEditor();
    fireEvent.click(screen.getByRole('switch', { name: 'Automatic failover' }));
    change('Failover cluster ID', 'home-dhcp');change('This node ID', 'node2');change('Peer listen address', '0.0.0.0:7447');
    for (let i = 1; i <= 3; i++) { change(`Voter ${i} ID`, `node${i}`);change(`Voter ${i} peer address`, `192.0.2.${i + 1}:7447`); }
    expect(screen.getByRole('checkbox', { name: 'Bootstrap this new cluster' })).toBeDisabled();
    change('This node ID', 'node1');
    const bootstrap = screen.getByRole('checkbox', { name: 'Bootstrap this new cluster' });
    expect(bootstrap).not.toBeChecked();expect(bootstrap).not.toBeDisabled();
    for (let i = 1; i <= 3; i++) change(`default: node${i} server address`, `192.0.2.${i + 1}`);
    fireEvent.click(bootstrap);
    fireEvent.click(screen.getByRole('button', { name: 'Save failover for restart' }));
    await waitFor(() => expect(mockDHCP().setup?.failover.enabled).toBe(true));
    expect(mockDHCP().setup?.failover.bootstrap).toBe(true);
    expect(mockDHCP().setup?.failover.peers).toHaveLength(3);
    expect(mockDHCP().setup?.scopes[0]?.failoverServers).toHaveLength(3);
    expect(mockDHCP().setup?.restartRequired).toBe(true);
  });

  it('preserves failover when stopping an established voter and blocks topology editing', async () => {
    configure();mockDHCP().setup!.failoverInitialized = true;
    const expected = structuredClone(mockDHCP().setup!.failover);
    renderWithApp(<DHCPPage />);await openEditor();
    expect(screen.getByRole('switch', { name: 'Automatic failover' })).toBeDisabled();
    expect(screen.getByRole('textbox', { name: 'Voter 1 peer address' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'New scope' })).toHaveAttribute('aria-disabled', 'true');
    fireEvent.click(screen.getByRole('switch', { name: 'Enable DHCP after restart' }));
    await waitFor(() => expect(mockDHCP().setup?.enabled).toBe(false));
    expect(mockDHCP().setup?.failover).toEqual(expected);
    expect(mockDHCP().enabled).toBe(true);
  });

  it('preserves saved failover during file adoption', async () => {
    mockDHCP().config.management = 'file';configure(false);
    const expected = structuredClone(mockDHCP().setup!.failover);
    renderWithApp(<DHCPPage />);
    fireEvent.click(await screen.findByRole('button', { name: 'Manage in dashboard' }));
    fireEvent.click(screen.getByRole('button', { name: 'Adopt setup' }));
    await waitFor(() => expect(mockDHCP().setup?.managedBy).toBe('api'));
    expect(mockDHCP().setup?.failover).toEqual(expected);
  });

  it('retains a stale failover draft and reloads the new revision before saving', async () => {
    configure();renderWithApp(<DHCPPage />);await openEditor();
    change('Failover cluster ID', 'draft-cluster');
    mockDHCP().setup = { ...mockDHCP().setup!, etag: '"newer-setup"', revision: '2' };
    fireEvent.click(screen.getByRole('button', { name: 'Save failover for restart' }));
    const reload = await screen.findByRole('button', { name: 'Reload failover settings' });
    expect(screen.getByRole('textbox', { name: 'Failover cluster ID' })).toHaveValue('draft-cluster');
    expect(mockDHCP().setup?.failover.clusterID).toBe('home-dhcp');
    expect(screen.getByRole('button', { name: 'Save failover for restart' })).toBeDisabled();
    await waitFor(() => expect(reload).not.toBeDisabled());
    fireEvent.click(reload);
    expect(screen.getByRole('textbox', { name: 'Failover cluster ID' })).toHaveValue('home-dhcp');
    change('Peer listen address', '127.0.0.1:7447');
    fireEvent.click(screen.getByRole('button', { name: 'Save failover for restart' }));
    await waitFor(() => expect(mockDHCP().setup?.failover.listenAddress).toBe('127.0.0.1:7447'));
  });

  it('validates duplicate voters before changing saved setup', async () => {
    configure();renderWithApp(<DHCPPage />);await openEditor();
    change('Voter 2 ID', 'node1');
    fireEvent.click(screen.getByRole('button', { name: 'Save failover for restart' }));
    expect(await screen.findByText('Voter IDs and peer addresses must be unique.')).toBeInTheDocument();
    expect(mockDHCP().setup?.failover.peers?.[1]?.id).toBe('node2');
  });

  it('requires all certificate paths when file credentials are selected', async () => {
    configure();renderWithApp(<DHCPPage />);await openEditor();
    fireEvent.click(screen.getByRole('combobox', { name: 'Peer credentials' }));
    fireEvent.click(screen.getByRole('option', { name: 'Certificate files on this node' }));
    fireEvent.click(screen.getByRole('button', { name: 'Save failover for restart' }));
    expect(await screen.findByText('Provide the CA, certificate and private key file paths for this node.')).toBeInTheDocument();
    expect(mockDHCP().setup?.revision).toBe('1');
    change('Peer CA file', '/etc/kudurru/ca.pem');change('Peer certificate file', '/etc/kudurru/node.pem');change('Peer private key file', '/etc/kudurru/node.key');
    fireEvent.click(screen.getByRole('button', { name: 'Save failover for restart' }));
    await waitFor(() => expect(mockDHCP().setup?.failover.keyFile).toBe('/etc/kudurru/node.key'));
    expect(mockDHCP().setup?.failover).toMatchObject({ caFile: '/etc/kudurru/ca.pem', certFile: '/etc/kudurru/node.pem' });
  });

  it('rejects credential-bearing dashboard URLs without changing the saved voters', async () => {
    configure();renderWithApp(<DHCPPage />);await openEditor();
    change('Voter 1 dashboard URL', 'https://operator:secret@example.invalid');
    fireEvent.click(screen.getByRole('button', { name: 'Save failover for restart' }));
    expect(await screen.findByText('Dashboard addresses must be HTTP or HTTPS URLs without credentials, query parameters or fragments.')).toBeInTheDocument();
    expect(mockDHCP().setup?.failover.peers?.[0]?.apiURL).toBeUndefined();
  });

  it('keeps credential fields read-only for environment locks and viewer permission', async () => {
    configure();mockDHCP().setup!.environmentLocked = true;
    const view = renderWithApp(<DHCPPage />);await openEditor();
    expect(screen.getByRole('textbox', { name: 'Peer listen address' })).toBeDisabled();
    view.unmount();mockDHCP().setup!.environmentLocked = false;
    renderWithApp(<DHCPPage />, { user: 'viewer' });await openEditor();
    expect(screen.getByRole('textbox', { name: 'Peer listen address' })).toBeDisabled();
    expect(screen.getByText('Changing failover requires dhcp.write permission.')).toBeInTheDocument();
  });

  it('explains quorum loss separately from DNS service health', async () => {
    mockDHCP().failoverStatus = { ...failover(), role: 'Leader', leaderID: 'node1', leaderAddress: '192.0.2.2:7447', ready: false, initialized: true, appliedIndex: 12, commitIndex: 12, lastContact: 0, error: '' };
    renderWithApp(<DHCPPage />);
    expect(await screen.findByText('Quorum is unavailable')).toBeInTheDocument();
    expect(screen.getByText('DHCP requests are dropped until a majority is available. DNS continues serving.')).toBeInTheDocument();
  });

  it('saves extended live client options with typed routes and architecture profiles', async () => {
    renderWithApp(<DHCPScopePage params={{ scope: 'default' }} />);
    await screen.findByRole('button', { name: 'Save network options' });
    fireEvent.click(within(screen.getByRole('group', { name: 'NTP servers' })).getByRole('button', { name: 'Add' }));
    change('NTP servers 1', '192.0.2.1');
    fireEvent.click(within(screen.getByRole('group', { name: 'Search domains' })).getByRole('button', { name: 'Add' }));
    change('Search domains 1', 'lab.home.arpa');
    fireEvent.click(within(screen.getByRole('group', { name: 'Classless routes' })).getByRole('button', { name: 'Add' }));
    change('Destination prefix', '198.51.100.0/24');change('Next-hop router', '192.0.2.1');
    change('Boot server address', '192.0.2.5');change('Boot file', 'ipxe.efi');
    fireEvent.click(within(screen.getByRole('group', { name: 'Boot architecture profiles' })).getByRole('button', { name: 'Add' }));
    fireEvent.input(screen.getByRole('spinbutton', { name: 'Architecture' }), { target: { value: '7' } });
    change('Profile boot file', 'uefi.efi');
    fireEvent.click(screen.getByRole('button', { name: 'Save network options' }));
    await waitFor(() => expect(mockDHCP().config.options.bootFile).toBe('ipxe.efi'));
    expect(mockDHCP().config.options.ntpServers).toEqual(['192.0.2.1']);
    expect(mockDHCP().config.options.domainSearch).toEqual(['lab.home.arpa']);
    expect(mockDHCP().config.options.classlessRoutes).toEqual([{ destination: '198.51.100.0/24', router: '192.0.2.1' }]);
    expect(mockDHCP().config.options.bootProfiles?.[0]).toMatchObject({ architecture: 7, bootFile: 'uefi.efi' });
  });

  it('saves probe and trusted relay extensions without losing failover configuration', async () => {
    mockDHCP().enabled = false;configure(false);
    renderWithApp(<DHCPScopePage params={{ scope: 'default' }} />);
    await screen.findByRole('button', { name: 'Save setup for restart' });
    fireEvent.click(within(screen.getByRole('group', { name: 'Trusted relays' })).getByRole('button', { name: 'Add' }));
    change('Relay source address', '192.0.2.1');change('Relay gateway address', '192.0.2.1');
    fireEvent.input(screen.getByRole('spinbutton', { name: 'Relay source port' }), { target: { value: '1067' } });
    change('Relay link selection', '192.0.2.1');change('Relay server identifier', '192.0.2.1');
    fireEvent.click(screen.getByRole('combobox', { name: 'Address conflict detection' }));
    fireEvent.click(screen.getByRole('option', { name: 'ICMP echo' }));
    fireEvent.click(screen.getByRole('combobox', { name: 'Conflict probe timeout' }));
    fireEvent.click(screen.getByRole('option', { name: '100 milliseconds' }));
    fireEvent.click(screen.getByRole('button', { name: 'Save setup for restart' }));
    await waitFor(() => expect(mockDHCP().setup?.scopes[0]?.conflictDetection).toBe('icmp'));
    expect(mockDHCP().setup?.scopes[0]?.probeTimeout).toBe('100ms');
    expect(mockDHCP().setup?.scopes[0]?.trustedRelays[0]).toMatchObject({ sourcePort: 1067, linkSelection: '192.0.2.1', serverIdentifier: '192.0.2.1' });
    expect(mockDHCP().setup?.failover).toEqual({ ...failover(), enabled: false });
  });
});
