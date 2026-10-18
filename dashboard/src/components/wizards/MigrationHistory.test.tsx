import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { MigrationRun } from '../../api/types';
import { server } from '../../test/setup';
import { renderWithApp } from '../../test/utils';
import { MigrationHistory } from './MigrationHistory';

const zone = { name: 'home', zoneName: 'home.test.', nameservers: ['old.test.'], primary: 'old.test.', resourceVersion: '8' };
const run: MigrationRun = { id: 'migration-test', revision: '7', data: { source: 'technitium', sourceURL: 'https://source.test', actor: 'admin', startedAt: '2026-10-18T00:00:00Z', finishedAt: '2026-10-18T00:01:00Z', status: 'completed', items: [{ kind: 'DNSZone', name: 'home', result: 'created' }], warnings: [], zones: [{ ...zone, originalNameservers: ['old.test.'], originalPrimary: 'old.test.' }], cutovers: [] } };
function mockRun() {
  server.use(
    http.get('/api/v1/imports/runs', () => HttpResponse.json({ items: [run] })),
    http.get('/api/v1/imports/runs/migration-test', () => HttpResponse.json(run)),
  );
}
async function openCutover() {
  fireEvent.click(await screen.findByRole('button', { name: 'Review migration migration-test' }));
  fireEvent.click(await screen.findByRole('button', { name: 'Decommission source nameservers' }));
  return screen.findByRole('dialog', { name: 'Decommission source nameservers' });
}

describe('Migration history', () => {
  it('shows durable run results and previews nameserver changes before applying', async () => {
    mockRun();
    const previews: unknown[] = [], applied: unknown[] = [];
    server.use(
      http.post('/api/v1/imports/runs/migration-test/decommission/preview', async ({ request }) => { previews.push(await request.json()); return HttpResponse.json({ revision: '7', nameservers: ['dns.kudurru.test.'], zones: [zone], warnings: ['Update parent delegation separately.'] }); }),
      http.post('/api/v1/imports/runs/migration-test/decommission', async ({ request }) => { applied.push(await request.json()); return HttpResponse.json({ runID: run.id, items: [{ kind: 'DNSZone', name: 'home', result: 'updated' }] }); }),
    );
    renderWithApp(<MigrationHistory />);
    const dialog = await openCutover();
    const nameserver = within(dialog).getByPlaceholderText('dns1.example.com');
    fireEvent.change(nameserver, { target: { value: '192.0.2.1' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Next' }));
    await waitFor(() => expect(within(dialog).getAllByText(/Enter a DNS hostname/).length).toBeGreaterThan(0));
    expect(previews).toHaveLength(0);
    fireEvent.change(nameserver, { target: { value: 'dns.kudurru.test' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Next' }));
    await within(dialog).findByRole('table', { name: 'Nameserver changes' });
    expect(previews).toEqual([{ nameservers: ['dns.kudurru.test'] }]);
    expect(applied).toHaveLength(0);
    fireEvent.click(within(dialog).getByRole('button', { name: 'Update nameservers' }));
    await waitFor(() => expect(within(dialog).getAllByText('Review and acknowledge the nameserver changes.').length).toBeGreaterThan(0));
    fireEvent.click(within(dialog).getByRole('checkbox', { name: 'I reviewed the nameserver changes' }));
    fireEvent.click(within(dialog).getByRole('button', { name: 'Update nameservers' }));
    await screen.findByText('Nameservers updated');
    expect(applied).toEqual([expect.objectContaining({ revision: '7', nameservers: ['dns.kudurru.test.'], zones: [zone] })]);
  });

  it('keeps partial errors visible and requires a fresh preview for a retry', async () => {
    mockRun(); let previews = 0; let writes = 0;
    server.use(
      http.post('/api/v1/imports/runs/migration-test/decommission/preview', () => { previews++; return HttpResponse.json({ revision: String(previews), nameservers: ['dns.kudurru.test.'], zones: [zone], warnings: [] }); }),
      http.post('/api/v1/imports/runs/migration-test/decommission', () => { writes++; return HttpResponse.json({ runID: run.id, items: [{ kind: 'DNSZone', name: 'home', result: 'error', error: 'Zone is managed by a file' }] }); }),
    );
    renderWithApp(<MigrationHistory />); const dialog = await openCutover();
    fireEvent.change(within(dialog).getByPlaceholderText('dns1.example.com'), { target: { value: 'dns.kudurru.test' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Next' }));
    await within(dialog).findByRole('table', { name: 'Nameserver changes' });
    fireEvent.click(within(dialog).getByRole('checkbox', { name: 'I reviewed the nameserver changes' }));
    fireEvent.click(within(dialog).getByRole('button', { name: 'Update nameservers' }));
    await within(dialog).findByText('home: Zone is managed by a file');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Update nameservers' }));
    await waitFor(() => expect(writes).toBe(1));
    fireEvent.click(within(dialog).getByRole('button', { name: 'Back' }));
    fireEvent.click(within(dialog).getByRole('button', { name: 'Next' }));
    await waitFor(() => expect(previews).toBe(2));
    expect(within(dialog).getByRole('checkbox', { name: 'I reviewed the nameserver changes' })).not.toBeChecked();
  });

  it('tracks selected previous imports without a source connection', async () => {
    const adopted: unknown[] = [];
    server.use(
      http.get('/api/v1/imports/existing', () => HttpResponse.json({ items: [zone] })),
      http.post('/api/v1/imports/adopt', async ({ request }) => { adopted.push(await request.json()); return HttpResponse.json({ runID: run.id, items: [{ kind: 'DNSZone', name: 'home', result: 'updated' }] }); }),
    );
    renderWithApp(<MigrationHistory />);
    fireEvent.click(await screen.findByRole('button', { name: 'Track previous imports' }));
    const dialog = await screen.findByRole('dialog', { name: 'Track previous imports' });
    const select = await within(dialog).findByRole('checkbox', { name: 'Track home.test.' });
    fireEvent.click(select);
    fireEvent.click(within(dialog).getByRole('button', { name: 'Track selected zones' }));
    await waitFor(() => expect(adopted).toEqual([{ zones: [zone] }]));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });
});
