import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http } from 'msw';
import { describe, expect, it, vi } from 'vitest';
import type { DHCPSetup, DHCPSetupRequest } from '../api/types';
import { DHCPAdoptionWizard } from '../components/DHCPAdoptionWizard';
import { useDHCPSetup } from '../components/DHCPSetup';
import { WizardDialog } from '../components/Wizard';
import { mockDHCP, mockDHCPSetup } from '../mocks/dhcp';
import { server } from '../test/setup';
import { renderWithApp } from '../test/utils';

function AdoptionDialog({ close }: { close: () => void }) {
  const setup = useDHCPSetup();
  return setup.data ? <WizardDialog isOpen onClose={close} title="Complete and adopt setup" what="DHCP setup">
    <DHCPAdoptionWizard setup={setup.data} close={close} />
  </WizardDialog> : null;
}

function fileSetup(): DHCPSetup {
  mockDHCP().enabled = false;
  mockDHCP().config.management = 'file';
  const setup = { ...mockDHCPSetup(), enabled: false, managedBy: 'file' as const };
  mockDHCP().setup = setup;
  return setup;
}

const change = (name: string, value: string) => fireEvent.change(screen.getByRole('textbox', { name }), { target: { value } });
async function next(heading: string) {
  fireEvent.click(screen.getByRole('button', { name: 'Next' }));
  await screen.findByRole('heading', { name: heading });
}

function recordSaves() {
  const requests: { etag: string | null; body: DHCPSetupRequest }[] = [];
  // Returning no response lets the existing permission/revision mock handle it.
  server.use(http.put('*/api/v1/node/dhcp/v4/setup', async ({ request }) => {
    requests.push({ etag: request.headers.get('If-Match'), body: await request.clone().json() as DHCPSetupRequest });
  }));
  return requests;
}

async function completeScope(prefix: string, domain: string) {
  await next('Subnet');
  expect(screen.getByRole('textbox', { name: 'Scope name' })).toBeDisabled();
  change('Server address', `${prefix}.2`);
  change('Client subnet', `${prefix}.0/24`);
  await next('Addresses');
  change('First address', `${prefix}.100`);
  change('Last address', `${prefix}.199`);
  change('Router', `${prefix}.1`);
  fireEvent.click(screen.getByRole('button', { name: 'Add DNS server' }));
  change('DNS servers 1', `${prefix}.2`);
  await next('DNS and reservations');
  change('Domain name', domain);
}

describe('incomplete file DHCP adoption', () => {
  it('completes two scope drafts and adopts the entire registry only on explicit finish', async () => {
    const setup = fileSetup();
    const original = setup.scopes[0]!;
    setup.scopes = ['lan', 'guest'].map((name, index) => ({ ...original, name, interface: `eth${index}`, serverIP: '', subnet: '',
      rangeStart: '', rangeEnd: '', router: '', dnsServers: [], domainName: '', registerDNS: false, reservations: [] }));
    setup.scopeRevisions = { lan: '0', guest: '0', retained: '7' };
    const requests = recordSaves();
    const close = vi.fn();
    renderWithApp(<AdoptionDialog close={close} />);
    await screen.findByRole('heading', { name: 'Choose scope' });
    await completeScope('192.0.2', 'lan.home.arpa');
    fireEvent.click(within(screen.getByRole('group', { name: 'Reservations' })).getByRole('button', { name: 'Add' }));
    change('MAC address', '02:00:00:00:00:10');
    change('Reserved address', '192.0.2.10');
    change('Hostname', 'printer');
    await next('Review all scopes');
    fireEvent.click(screen.getByRole('button', { name: 'Adopt setup' }));
    expect(await screen.findByText('Complete every scope before adoption: guest.')).toBeInTheDocument();
    expect(requests).toHaveLength(0);
    expect(setup.scopes[0]?.serverIP).toBe('');
    fireEvent.click(screen.getByRole('button', { name: 'Complete guest' }));
    await screen.findByRole('heading', { name: 'Choose scope' });
    expect(screen.getByRole('combobox', { name: 'Scope to complete' })).toHaveAttribute('data-value', 'guest');
    await completeScope('198.51.100', 'guest.home.arpa');
    await next('Review all scopes');
    expect(screen.queryByText('Some scopes need completion')).not.toBeInTheDocument();
    expect(requests).toHaveLength(0);
    fireEvent.click(screen.getByRole('button', { name: 'Adopt setup' }));
    await waitFor(() => expect(close).toHaveBeenCalledOnce());
    expect(requests).toHaveLength(1);
    expect(requests[0]).toMatchObject({ etag: setup.etag, body: { adopt: true, enabled: false, scopeRevisions: { lan: '0', guest: '0', retained: '7' },
      scopes: [
        { name: 'lan', serverIP: '192.0.2.2', domainName: 'lan.home.arpa', reservations: [{ mac: '02:00:00:00:00:10', ip: '192.0.2.10', hostname: 'printer' }] },
        { name: 'guest', serverIP: '198.51.100.2', domainName: 'guest.home.arpa' },
      ] } });
    expect(mockDHCP().setup).toMatchObject({ managedBy: 'api', enabled: false, restartRequired: true });
    expect(mockDHCP().enabled).toBe(false);
    expect(mockDHCP().config.management).toBe('file');
  });

  it('retains every draft on 412 until an explicit reload, then uses the fresh revision map', async () => {
    const setup = fileSetup();
    const requests = recordSaves();
    const close = vi.fn();
    renderWithApp(<AdoptionDialog close={close} />);
    await screen.findByRole('heading', { name: 'Choose scope' });
    await next('Subnet');
    await next('Addresses');
    await next('DNS and reservations');
    change('Domain name', 'draft.home.arpa');
    await next('Review all scopes');
    // The setup ETag is unchanged: an external live edit changes only its DB revision.
    mockDHCP().setup = { ...setup, scopes: setup.scopes.map((s) => ({ ...s, domainName: 'latest.home.arpa' })), scopeRevisions: { default: '2' } };
    fireEvent.click(screen.getByRole('button', { name: 'Adopt setup' }));
    expect(await screen.findByText('The saved setup changed')).toBeInTheDocument();
    expect(screen.getByText(/domain: draft\.home\.arpa/)).toBeInTheDocument();
    expect(close).not.toHaveBeenCalled();
    expect(mockDHCP().setup?.managedBy).toBe('file');
    fireEvent.click(screen.getByRole('button', { name: 'Adopt setup' }));
    await screen.findByText('Reload saved setup and review every scope before adopting.');
    expect(requests).toHaveLength(1);
    const reload = screen.getByRole('button', { name: 'Reload saved setup' });
    await waitFor(() => expect(reload).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(reload);
    expect(screen.getByText(/domain: latest\.home\.arpa/)).toBeInTheDocument();
    expect(screen.queryByText(/domain: draft\.home\.arpa/)).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Adopt setup' }));
    await waitFor(() => expect(close).toHaveBeenCalledOnce());
    expect(requests).toHaveLength(2);
    expect(requests[1]).toMatchObject({ etag: setup.etag, body: { adopt: true, enabled: false, scopeRevisions: { default: '2' }, scopes: [{ domainName: 'latest.home.arpa' }] } });
  });

  it.each([
    { name: 'environment ownership', user: 'admin', locked: true, available: true },
    { name: 'missing write permission', user: 'viewer', locked: false, available: true },
    { name: 'unavailable setup manager', user: 'admin', locked: false, available: false },
  ])('prevents adoption for $name', async ({ user, locked, available }) => {
    const setup = fileSetup();
    setup.environmentLocked = locked;
    setup.available = available;
    const requests = recordSaves();
    const close = vi.fn();
    renderWithApp(<AdoptionDialog close={close} />, { user });
    await screen.findByText('Setup cannot be adopted');
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(screen.getByRole('heading', { name: 'Choose scope' })).toBeInTheDocument();
    expect(screen.queryByRole('textbox', { name: 'Server address' })).not.toBeInTheDocument();
    expect(requests).toHaveLength(0);
    expect(close).not.toHaveBeenCalled();
    expect(mockDHCP().setup?.managedBy).toBe('file');
  });
});
