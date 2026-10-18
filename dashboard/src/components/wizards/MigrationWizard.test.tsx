// @vitest-environment-options {"url":"https://dns.kudurru.test:8443/kudurru/"}
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { server } from '../../test/setup';
import { getSelector, pick } from '../../test/controls';
import { renderWithApp } from '../../test/utils';
import MigrationRunPage from '../../pages/MigrationRunPage';
import { useLocation } from '../../lib/router';
import { MigrationImport } from './MigrationWizard';

function TestFlow() {
  const location = useLocation();
  return location.path.startsWith('/tools/migrations/') ? <MigrationRunPage params={{ id: 'migration-test' }} /> : <MigrationImport />;
}
const startedRun = { id: 'migration-test', revision: '1', data: { source: 'technitium', actor: 'admin', startedAt: '2026-10-18T00:00:00Z', status: 'running', items: [{ kind: 'Policy', namespace: 'default', name: 'default', result: 'pending' }], zones: [], warnings: [], cutovers: [] } };

describe('API migration wizard' , () => {
  it('imports generated authority using discovered VIPs after review', async () => {
    const previews: unknown[] = [];
    server.use(
      http.get('/api/v1/edge', () => HttpResponse.json({ vips: [{ address: '192.0.2.53/24' }] })),
      http.post('/api/v1/imports/preview', async ({ request }) => {
        previews.push(await request.json());
        return HttpResponse.json({ objects: [
          { apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSZone', metadata: { name: 'home' }, spec: { zoneName: 'home.test.', nameservers: ['ns-c0000235.home.test.'], soa: { mname: 'ns-c0000235.home.test.' } } },
          { apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSRecordSet', metadata: { name: 'home', namespace: 'default' }, spec: { zoneRef: { name: 'home' }, endpoints: [{ dnsName: 'ns-c0000235.home.test.', recordType: 'A', targets: ['192.0.2.53'] }] } },
        ], settings: {}, warnings: [] });
      }),
    );
    renderWithApp(<TestFlow />);
    const launch = await screen.findByRole('button', { name: 'Import from DNS server' });
    await waitFor(() => { expect(launch).toBeEnabled(); expect(launch).not.toHaveAttribute('aria-disabled', 'true'); });
    fireEvent.click(launch);
    const dialog = await screen.findByRole('dialog', { name: 'Import from DNS server' });
    fireEvent.change(within(dialog).getByLabelText('Server URL'), { target: { value: 'http://source.test:5380' } });
    fireEvent.change(within(dialog).getByLabelText('API token'), { target: { value: 'source-secret' } });
    fireEvent.click(within(dialog).getByRole('checkbox', { name: 'Update nameservers during import' }));
    pick(getSelector('Advertise kudurru using', dialog), 'Serving IPs or VIPs');
    fireEvent.click(await within(dialog).findByRole('button', { name: 'Use detected VIPs' }));
    fireEvent.click(within(dialog).getByRole('button', { name: 'Next' }));
    const changes = await within(dialog).findByRole('table', { name: 'Nameserver and address changes' });
    expect(changes).toHaveTextContent('ns-c0000235.home.test. A 192.0.2.53');
    expect(previews).toEqual([expect.objectContaining({ nameservers: [], nameserverAddresses: ['192.0.2.53'] })]);
    expect(within(dialog).getByRole('checkbox', { name: 'I reviewed the configuration and migration warnings' })).not.toBeChecked();
  });

  it('previews before import, requires review, and keeps credentials out of the apply request', async () => {
    const previews: unknown[] = [];
    const applied: unknown[] = [];
    server.use(
      http.post('/api/v1/imports/preview', async ({ request }) => {
        previews.push(await request.json());
        return HttpResponse.json({ objects: [{ apiVersion: 'kudurru.io/v1alpha1', kind: 'Policy', metadata: { name: 'default', namespace: 'default' }, spec: {} }], settings: {}, warnings: ['DHCP is not migrated.'] });
      }),
      http.post('/api/v1/imports/start', async ({ request }) => {
        applied.push(await request.json());
        return HttpResponse.json(startedRun, { status: 202 });
      }),
    );
    renderWithApp(<TestFlow />);
    const launch = await screen.findByRole('button', { name: 'Import from DNS server' });
    await waitFor(() => { expect(launch).toBeEnabled(); expect(launch).not.toHaveAttribute('aria-disabled', 'true'); });
    fireEvent.click(launch);
    const dialog = await screen.findByRole('dialog', { name: 'Import from DNS server' });
    fireEvent.change(within(dialog).getByLabelText('Server URL'), { target: { value: 'http://dns.local:5380' } });
    fireEvent.change(within(dialog).getByLabelText('API token'), { target: { value: 'source-secret' } });
    fireEvent.click(within(dialog).getByRole('checkbox', { name: 'Update nameservers during import' }));
    expect(within(dialog).getByPlaceholderText('dns1.example.com')).toHaveValue('dns.kudurru.test');
    fireEvent.change(within(dialog).getByPlaceholderText('dns1.example.com'), { target: { value: 'dns.kudurru.test' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Next' }));
    await within(dialog).findByRole('table', { name: 'Migration objects' });
    expect(previews).toEqual([expect.objectContaining({ source: 'technitium', credential: 'source-secret', nameservers: ['dns.kudurru.test'] })]);
    expect(applied).toHaveLength(0);
    fireEvent.click(within(dialog).getByRole('button', { name: 'Import configuration' }));
    await waitFor(() => expect(within(dialog).getAllByText('Review the configuration and acknowledge the migration warnings.').length).toBeGreaterThan(0));
    expect(applied).toHaveLength(0);
    fireEvent.click(within(dialog).getByRole('checkbox', { name: 'I reviewed the configuration and migration warnings' }));
    fireEvent.click(within(dialog).getByRole('button', { name: 'Import configuration' }));
    await screen.findByText('Import in progress');
    expect(window.location.pathname).toBe('/tools/migrations/migration-test');
    expect(applied[0]).toEqual(expect.objectContaining({ selection: { objects: ['Policy/default/default'], settings: [] } }));
    expect(applied).toHaveLength(1);
    expect(JSON.stringify(applied)).not.toContain('source-secret');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('shows a failed connection without advancing to import', async () => {
    server.use(http.post('/api/v1/imports/preview', () => HttpResponse.json({ error: { key: 'invalid', message: 'Source authentication failed' } }, { status: 400 })));
    renderWithApp(<TestFlow />);
    const launch = await screen.findByRole('button', { name: 'Import from DNS server' });
    await waitFor(() => { expect(launch).toBeEnabled(); expect(launch).not.toHaveAttribute('aria-disabled', 'true'); });
    fireEvent.click(launch);
    const dialog = await screen.findByRole('dialog', { name: 'Import from DNS server' });
    fireEvent.change(within(dialog).getByLabelText('Server URL'), { target: { value: 'http://dns.local:5380' } });
    fireEvent.change(within(dialog).getByLabelText('API token'), { target: { value: 'incorrect' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Next' }));
    await waitFor(() => expect(within(dialog).getAllByText(/Source authentication failed/).length).toBeGreaterThan(0));
    expect(within(dialog).queryByRole('button', { name: 'Import configuration' })).not.toBeInTheDocument();
  });

  it('selects individual objects and settings and requires renewed acknowledgment', async () => {
    const started: unknown[] = [];
    server.use(
      http.post('/api/v1/imports/preview', () => HttpResponse.json({ objects: [
        { kind: 'Policy', metadata: { name: 'default', namespace: 'default' }, spec: {} },
        { kind: 'DNSZone', metadata: { name: 'home' }, spec: { zoneName: 'home.test.' } },
      ], settings: { blocking: { enabled: true }, upstreams: {} }, warnings: [] })),
      http.post('/api/v1/imports/start', async ({ request }) => { started.push(await request.json()); return HttpResponse.json(startedRun, { status: 202 }); }),
    );
    renderWithApp(<TestFlow />);
    const launch = await screen.findByRole('button', { name: 'Import from DNS server' });
    await waitFor(() => expect(launch).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(launch);
    const dialog = await screen.findByRole('dialog', { name: 'Import from DNS server' });
    fireEvent.change(within(dialog).getByLabelText('Server URL'), { target: { value: 'http://source.test' } });
    fireEvent.change(within(dialog).getByLabelText('API token'), { target: { value: 'source-secret' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Next' }));
    await within(dialog).findByRole('table', { name: 'Migration objects' });
    expect(within(dialog).getByRole('checkbox', { name: 'Import DNSZone/home' })).toBeChecked();
    expect(within(dialog).getByRole('checkbox', { name: 'Import settings: blocking' })).toBeChecked();
    const acknowledgment = within(dialog).getByRole('checkbox', { name: 'I reviewed the configuration and migration warnings' });
    fireEvent.click(acknowledgment);
    fireEvent.click(within(dialog).getByRole('checkbox', { name: 'Import DNSZone/home' }));
    expect(acknowledgment).not.toBeChecked();
    fireEvent.click(within(dialog).getByRole('button', { name: 'Clear selection' }));
    fireEvent.click(within(dialog).getByRole('button', { name: 'Import configuration' }));
    await waitFor(() => expect(within(dialog).getAllByText('Select at least one object or settings section.').length).toBeGreaterThan(0));
    expect(started).toHaveLength(0);
    fireEvent.click(within(dialog).getByRole('checkbox', { name: 'Import Policy/default/default' }));
    fireEvent.click(within(dialog).getByRole('checkbox', { name: 'Import settings: blocking' }));
    fireEvent.click(acknowledgment);
    fireEvent.click(within(dialog).getByRole('button', { name: 'Import configuration' }));
    await screen.findByText('Import in progress');
    expect(started).toEqual([expect.objectContaining({ selection: { objects: ['Policy/default/default'], settings: ['blocking'] } })]);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });
});
