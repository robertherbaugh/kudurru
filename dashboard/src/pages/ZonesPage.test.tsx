import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { importSummary, originOf } from '../components/ZoneImportDialog';
import { mockDB } from '../mocks/handlers';
import { findSelector, getSelector, optionLabels, pick } from '../test/controls';
import { renderWithApp } from '../test/utils';
import ZoneDetailPage from './ZoneDetailPage';
import ZonesPage from './ZonesPage';
import { ZoneWizardDialog } from '../components/wizards/ZoneWizard';

const FILE = '$ORIGIN lab.example.\n@ 3600 IN SOA ns1 hostmaster 42 7200 3600 1209600 300\nwww 300 IN A 192.0.2.10\nmail 300 IN A 192.0.2.25\n';

describe('zone import', () => {
  it('summarises the real import result', () => {
    expect(importSummary({ zone: 'z', zoneName: 'z.', created: false, recordSets: ['default/import-z-1'], endpoints: 3, serial: 1, warnings: [], took_ms: 1 })).toBe(
      '3 records imported into default/import-z-1',
    );
    expect(importSummary({ zone: 'z', zoneName: 'z.', created: true, recordSets: null, endpoints: 1, serial: 1, warnings: [], took_ms: 1 })).toBe('Created zone z.: 1 record imported');
    expect(originOf(FILE)).toBe('lab.example.');
  });

  it('shows the record count and the warnings of an import into an existing zone', async () => {
    renderWithApp(<ZoneDetailPage params={{ zone: 'example-internal' }} />, { route: '/zones/example-internal' });
    const btn = await screen.findByRole('button', { name: 'Import BIND' });
    await waitFor(() => expect(btn.getAttribute('aria-disabled') !== 'true' && !btn.hasAttribute('disabled')).toBe(true));
    fireEvent.click(btn);
    const dialog = await screen.findByRole('dialog');
    fireEvent.change(within(dialog).getByRole('textbox', { name: /paste the zone text/ }), { target: { value: 'nas2 300 IN A 192.168.1.60\n' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Import' }));
    expect(await within(dialog).findByText('1 record imported into default/import-example-internal-1')).toBeInTheDocument();
    expect(within(dialog).getByText('no SOA record; defaults apply')).toBeInTheDocument();
    expect(dialog.textContent).not.toMatch(/undefined/);
  });

  it('creates a zone from a file with ?origin=', async () => {
    renderWithApp(<ZonesPage />, { route: '/zones' });
    const btn = await screen.findByRole('button', { name: 'Import zone file' });
    await waitFor(() => expect(btn.getAttribute('aria-disabled') !== 'true' && !btn.hasAttribute('disabled')).toBe(true));
    fireEvent.click(btn);
    const dialog = await screen.findByRole('dialog');
    fireEvent.change(within(dialog).getByRole('textbox', { name: /^Zone apex/ }), { target: { value: 'lab.example' } });
    fireEvent.change(within(dialog).getByRole('textbox', { name: /paste the zone text/ }), { target: { value: FILE } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Import' }));
    expect(await within(dialog).findByText('Created zone lab.example.: 2 records imported into default/import-lab.example-1')).toBeInTheDocument();
    expect(mockDB().zones.find((z) => z.metadata.name === 'lab.example')?.spec.zoneName).toBe('lab.example.');
  });

  it('reads a chosen zone file and takes the apex from its $ORIGIN', async () => {
    renderWithApp(<ZonesPage />, { route: '/zones' });
    const btn = await screen.findByRole('button', { name: 'Import zone file' });
    await waitFor(() => expect(btn.getAttribute('aria-disabled') !== 'true' && !btn.hasAttribute('disabled')).toBe(true));
    fireEvent.click(btn);
    const dialog = await screen.findByRole('dialog');
    const input = dialog.querySelector<HTMLInputElement>('input[type="file"]');
    expect(input).not.toBeNull();
    fireEvent.change(input!, { target: { files: [new File([FILE], 'lab.zone', { type: 'text/plain' })] } });
    await waitFor(() => expect(within(dialog).getByRole('textbox', { name: /paste the zone text/ })).toHaveValue(FILE));
    expect(within(dialog).getByRole('textbox', { name: /^Zone apex/ })).toHaveValue('lab.example.');
  });

  it('opens the new-zone wizard from the list', async () => {
    renderWithApp(<ZonesPage />, { route: '/zones' });
    const btn = await screen.findByRole('button', { name: 'New zone' });
    await waitFor(() => expect(btn.getAttribute('aria-disabled') !== 'true' && !btn.hasAttribute('disabled')).toBe(true));
    fireEvent.click(btn);
    expect(await screen.findByRole('dialog', { name: 'New zone' })).toBeInTheDocument();
    expect(window.location.pathname + window.location.search).toBe('/zones?new=1');
  });
});

describe('ZoneWizardDialog', () => {
  it('creates a forward zone with an upstream group picked from the configured ones', async () => {
    renderWithApp(<ZoneWizardDialog isOpen onClose={() => {}} />, { route: '/zones?new=1' });
    fireEvent.click(await screen.findByRole('radio', { name: /^Forward/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    const apex = await screen.findByRole('textbox', { name: /^Zone name/ });
    fireEvent.change(apex, { target: { value: 'bad..corp' } });
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect((await screen.findAllByText('Empty label (two dots in a row)')).length).toBeGreaterThan(0);
    fireEvent.change(apex, { target: { value: 'Corp.Example.' } });
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    const sel = await findSelector(/Upstream group/);
    await waitFor(() => expect(optionLabels(sel)).toEqual(['Not set (default)', 'default', 'filtered', 'resolver']));
    pick(sel, /^filtered/);
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(await screen.findByTestId('zone-review')).toHaveTextContent('corp.example.');
    fireEvent.click(screen.getByRole('button', { name: 'Create zone' }));
    await waitFor(() => expect(mockDB().zones.find((z) => z.metadata.name === 'corp.example')?.spec).toEqual({ zoneName: 'corp.example.', type: 'forward', forward: { upstreamGroup: 'filtered' } }));
    await waitFor(() => expect(window.location.pathname).toBe('/zones/corp.example'));
  });

  it('creates a primary zone with typed SOA timers and a query ACL', async () => {
    renderWithApp(<ZoneWizardDialog isOpen onClose={() => {}} />, { route: '/zones?new=1' });
    fireEvent.click(await screen.findByRole('button', { name: 'Next' }));
    const apex = await screen.findByRole('textbox', { name: /^Zone name/ });
    fireEvent.change(apex, { target: { value: 'example.internal' } });
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(await screen.findByText('A zone example.internal exists')).toBeInTheDocument();
    fireEvent.change(apex, { target: { value: 'lab.example' } });
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    pick(await findSelector('Refresh'), /^2 hours/);
    pick(getSelector('Who may query'), /^Clients in the ACL/);
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(await screen.findByText('The ACL needs at least one entry for this access mode')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Add entry' }));
    fireEvent.change(screen.getByRole('textbox', { name: 'Query ACL 1' }), { target: { value: '!192.168.1.66' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add entry' }));
    fireEvent.change(screen.getByRole('textbox', { name: 'Query ACL 2' }), { target: { value: '192.168.1.0/24' } });
    fireEvent.change(screen.getByRole('textbox', { name: /^Responsible mailbox/ }), { target: { value: 'hostmaster@lab.example' } });
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    await screen.findByTestId('zone-review');
    fireEvent.click(screen.getByRole('button', { name: 'Create zone' }));
    await waitFor(() =>
      expect(mockDB().zones.find((z) => z.metadata.name === 'lab.example')?.spec).toEqual({
        zoneName: 'lab.example.',
        type: 'primary',
        soa: { rname: 'hostmaster.lab.example.', refresh: 7200 },
        policy: { queryAccess: 'useNetworkACL', queryACL: ['!192.168.1.66', '192.168.1.0/24'] },
      }),
    );
  });
});
