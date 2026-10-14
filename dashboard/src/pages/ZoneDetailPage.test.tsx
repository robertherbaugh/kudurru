import { screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it, vi } from 'vitest';
import type { SecondaryRuntime } from '../api/types';
import { formatDateTime } from '../lib/format';
import { mockDB } from '../mocks/handlers';
import { server } from '../test/setup';
import { API, renderWithApp } from '../test/utils';
import ZoneDetailPage from './ZoneDetailPage';
import ZonesPage from './ZonesPage';

const ZONE = 'secondary.example';
const SERVING: SecondaryRuntime = {
  node: 'kudurru-1', state: 'serving', serial: 0, recordCount: 25, primary: 'ns1.example.com:5353',
  lastConfirmedAt: '2026-10-14T12:00:00Z', expiresAt: '2026-10-15T12:00:00Z', nextAttemptAt: '2026-10-14T13:00:00Z',
};

function seed(runtime?: SecondaryRuntime) {
  mockDB().zones.push({
    apiVersion: 'kudurru.io/v1alpha1', kind: 'DNSZone', metadata: { name: ZONE },
    spec: { zoneName: `${ZONE}.`, type: 'secondary', secondary: { primaries: ['ns1.example.com:5353'] } },
    status: { serial: 999999, recordCount: 777777, conditions: [{ type: 'Ready', status: 'True', reason: 'Ready', message: 'Shared configuration is ready' }] },
  });
  if (runtime) mockDB().secondaryRuntime[ZONE] = runtime;
}

describe('secondary runtime', () => {
  it('shows the local serial including zero, confirmation and expiry instead of shared status', async () => {
    seed(SERVING);
    renderWithApp(<ZoneDetailPage params={{ zone: ZONE }} />, { route: `/zones/${ZONE}` });
    const runtime = await screen.findByRole('region', { name: 'Secondary on this node' });
    expect(runtime).toHaveTextContent('Serving');
    expect(runtime).toHaveTextContent('on node kudurru-1');
    expect(runtime).toHaveTextContent('Served serial0');
    expect(runtime).toHaveTextContent('Served records25');
    expect(runtime).toHaveTextContent(formatDateTime(SERVING.lastConfirmedAt));
    expect(runtime).toHaveTextContent(formatDateTime(SERVING.expiresAt));
    expect(runtime).toHaveTextContent(formatDateTime(SERVING.nextAttemptAt));
    expect(screen.queryByText(/999999|777777|Shared configuration is ready/)).not.toBeInTheDocument();
    expect(screen.queryByText('Ready', { exact: true })).not.toBeInTheDocument();
  });

  it.each([
    ['pending', 'Pending transfer'], ['expired', 'Expired'], ['unavailable', 'Unavailable'],
  ] as const)('shows %s locally without claiming the retained snapshot is served', async (state, label) => {
    seed({ ...SERVING, state, serial: 42, reason: 'TransferFailed', message: 'Primary did not answer' });
    renderWithApp(<ZoneDetailPage params={{ zone: ZONE }} />, { route: `/zones/${ZONE}` });
    const runtime = await screen.findByRole('region', { name: 'Secondary on this node' });
    expect(runtime).toHaveTextContent(label);
    expect(runtime).toHaveTextContent('Last snapshot serial42');
    expect(runtime).toHaveTextContent('Primary did not answer');
    expect(within(runtime).queryByText('Served serial')).not.toBeInTheDocument();
    expect(within(runtime).queryByText('Serving', { exact: true })).not.toBeInTheDocument();
  });

  it('keeps last-good data serving while reporting a failed refresh', async () => {
    seed({ ...SERVING, reason: 'TransferFailed', message: 'Retrying the primary; last-good data is unexpired' });
    renderWithApp(<ZoneDetailPage params={{ zone: ZONE }} />, { route: `/zones/${ZONE}` });
    const runtime = await screen.findByRole('region', { name: 'Secondary on this node' });
    expect(runtime).toHaveTextContent('Serving');
    expect(runtime).toHaveTextContent('Retrying the primary; last-good data is unexpired');
    expect(runtime).toHaveTextContent('Served serial0');
  });

  it('reports unknown when runtime is absent and hides object record/actions without fetching them', async () => {
    seed();
    const recordsRequest = vi.fn();
    server.use(http.get(`${API}/zones/${ZONE}/records`, () => {
      recordsRequest();
      return HttpResponse.json({ items: [] });
    }));
    const { client } = renderWithApp(<ZoneDetailPage params={{ zone: ZONE }} />, { route: `/zones/${ZONE}` });
    const runtime = await screen.findByRole('region', { name: 'Secondary on this node' });
    expect(runtime).toHaveTextContent('Unknown');
    expect(runtime).toHaveTextContent('served serial and expiry are unknown');
    expect(screen.queryByText(/999999|777777/)).not.toBeInTheDocument();
    expect(screen.queryByRole('table', { name: `Records of ${ZONE}.` })).not.toBeInTheDocument();
    for (const name of ['Add record', 'Import BIND', 'Export', 'Send NOTIFY']) expect(screen.queryByRole('button', { name })).not.toBeInTheDocument();
    expect(screen.queryByText('No records yet.')).not.toBeInTheDocument();
    await waitFor(() => expect(client.isFetching()).toBe(0));
    expect(recordsRequest).not.toHaveBeenCalled();
  });

  it.each([undefined, SERVING, { ...SERVING, state: 'expired' as const, serial: 42 }])('uses only local state in the list when runtime is %j', async (runtime) => {
    seed(runtime);
    renderWithApp(<ZonesPage />, { route: '/zones' });
    const link = await screen.findByRole('link', { name: `${ZONE}.` });
    const row = link.closest('tr');
    expect(row).not.toBeNull();
    const item = within(row!);
    expect(row).not.toHaveTextContent(/999999|777777/);
    expect(item.queryByText('Ready', { exact: true })).not.toBeInTheDocument();
    if (!runtime) expect(item.getByText('Unknown')).toBeInTheDocument();
    else {
      expect(item.getByText(`on node ${runtime.node}`)).toBeInTheDocument();
      expect(item.getByText(String(runtime.serial), { exact: true })).toBeInTheDocument();
      expect(item.getByText(runtime.state === 'serving' ? 'Serving' : 'Expired')).toBeInTheDocument();
      if (runtime.state === 'expired') expect(item.getByText('last snapshot')).toBeInTheDocument();
    }
  });
});
