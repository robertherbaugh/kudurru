import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { MigrationRun } from '../api/types';
import { getSelector, pick } from '../test/controls';
import { server } from '../test/setup';
import { renderWithApp } from '../test/utils';
import MigrationRunPage from './MigrationRunPage';

const run: MigrationRun = { id: 'job', revision: '1', data: { source: 'technitium', actor: 'admin', startedAt: '2026-10-18T00:00:00Z', status: 'running', items: [
  { kind: 'DNSZone', name: 'home', result: 'created', resourceVersion: '42' },
  { kind: 'BlockList', namespace: 'default', name: 'technitium-block', result: 'running' },
  { kind: 'Policy', namespace: 'default', name: 'default', result: 'skipped' },
  { kind: 'Settings', name: 'blocking', result: 'pending' },
], zones: [], warnings: ['Review DNS before switching clients.'], cutovers: [] } };

describe('Migration run report', () => {
  it('polls progress and keeps success and error details visible after partial completion', async () => {
    let finished = false;
    let reads = 0;
    server.use(http.get('/api/v1/imports/runs/job', () => {
      reads++;
      return HttpResponse.json(!finished ? run : { ...run, revision: '2', data: { ...run.data, status: 'partial', finishedAt: '2026-10-18T00:01:00Z', items: [
        run.data.items[0], { ...run.data.items[1], result: 'error', error: 'Managed by kubectl-patch', errorKey: 'managed_by', hint: 'Change it with its owner.' }, run.data.items[2], { ...run.data.items[3], result: 'not_applied', hint: 'An object failed; settings were not changed.' },
      ] } });
    }));
    renderWithApp(<MigrationRunPage params={{ id: 'job' }} />);
    await screen.findByText('Import in progress');
    expect(screen.getByText(/You can leave this page or close the browser/)).toBeVisible();
    expect(screen.getByRole('progressbar', { name: 'Selected items processed' })).toHaveAttribute('aria-valuenow', '1');
    finished = true;
    await screen.findByText('Import completed with errors', {}, { timeout: 4000 });
    expect(reads).toBeGreaterThan(1);
    expect(screen.getByText(/1 succeeded, 1 failed, 0 unconfirmed, 1 not applied, 1 not selected/)).toBeVisible();
    const table = screen.getByRole('table', { name: 'Import results' });
    expect(table).toHaveTextContent('Created');
    expect(table).toHaveTextContent('Failed');
    expect(table).toHaveTextContent('Managed by kubectl-patch');
    expect(table).toHaveTextContent('Change it with its owner.');
    expect(table).toHaveTextContent('Saved version: 42');
    pick(getSelector('Show results'), 'Errors');
    expect(within(table).getAllByRole('row')).toHaveLength(2);
    expect(table).not.toHaveTextContent('home');
    fireEvent.change(screen.getByLabelText('Search import results'), { target: { value: 'nonexistent' } });
    expect(table).not.toHaveTextContent('technitium-block');
  });

  it('shows interruptions and unconfirmed outcomes after reopening a run', async () => {
    server.use(http.get('/api/v1/imports/runs/job', () => HttpResponse.json({ ...run, data: { ...run.data, status: 'interrupted', failure: 'The worker stopped reporting; inspect the destination.', items: [{ kind: 'DNSZone', name: 'home', result: 'unconfirmed', hint: 'Check the destination before retrying.' }] } })));
    renderWithApp(<MigrationRunPage params={{ id: 'job' }} />);
    await screen.findByText('Import interrupted');
    expect(screen.getByText('Unconfirmed')).toBeVisible();
    expect(screen.getByText('Check the destination before retrying.')).toBeVisible();
    expect(screen.getByText('The worker stopped reporting; inspect the destination.')).toBeVisible();
  });

  it('does not request history without settings.write', async () => {
    let reads = 0;
    server.use(http.get('/api/v1/imports/runs/job', () => { reads++; return HttpResponse.json(run); }));
    renderWithApp(<MigrationRunPage params={{ id: 'job' }} />, { user: 'viewer' });
    await screen.findByText('Migration history requires settings.write');
    await waitFor(() => expect(reads).toBe(0));
  });
});
