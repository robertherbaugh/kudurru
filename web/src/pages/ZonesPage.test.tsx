import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { importSummary, originOf } from '../components/ZoneImportDialog';
import { mockDB } from '../mocks/handlers';
import { renderWithApp } from '../test/utils';
import ZoneDetailPage from './ZoneDetailPage';
import ZonesPage from './ZonesPage';

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

  it('offers the configured upstream groups for a forward zone', async () => {
    renderWithApp(<ZonesPage />, { route: '/zones' });
    const btn = await screen.findByRole('button', { name: 'New zone' });
    await waitFor(() => expect(btn.getAttribute('aria-disabled') !== 'true' && !btn.hasAttribute('disabled')).toBe(true));
    fireEvent.click(btn);
    const dialog = await screen.findByRole('dialog');
    fireEvent.change(within(dialog).getByRole('combobox', { name: 'Type' }), { target: { value: 'forward' } });
    const sel = await within(dialog).findByRole('combobox', { name: /Upstream group/ });
    const options = within(sel).getAllByRole('option').map((o) => o.textContent);
    expect(options).toEqual(['Not set (default)', 'default', 'filtered']);
  });
});
