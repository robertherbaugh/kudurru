import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { describe, expect, it } from 'vitest';
import { mockDB } from '../mocks/handlers';
import { getSelector, pick } from '../test/controls';
import { server } from '../test/setup';
import { renderWithApp } from '../test/utils';
import AuditPage, { CHEVRON_COLUMN_WIDTH } from './AuditPage';

describe('AuditPage', () => {
  it('sorts oldest first on the server and pages toward newer events', async () => {
    const base = mockDB().audit[0]!;
    const oldest = { ...base, id: 1, action: 'blocklist.create' };
    const middle = { ...base, id: 2, action: 'zone.update-1' };
    const newest = { ...base, id: 3, action: 'zone.update-2' };
    const seen: URLSearchParams[] = [];
    // Small pages exercise the cursor contract without using hundreds of
    // JSDOM rows as a rendering benchmark. AccessPage.test covers the 100-row page.
    server.use(
      http.get('*/api/v1/audit', ({ request }) => {
        const params = new URL(request.url).searchParams;
        seen.push(params);
        if (params.get('order') === 'desc') return HttpResponse.json({ items: [newest] });
        if (params.get('cursor')) return HttpResponse.json({ items: [newest] });
        return HttpResponse.json({ items: [oldest, middle], continue: 'older-page-cursor' });
      }),
    );
    renderWithApp(<AuditPage />, { route: '/audit' });
    await screen.findByRole('table', { name: 'Audit entries' });
    pick(getSelector('Time order'), 'Oldest first');
    const table = await screen.findByRole('table', { name: 'Audit entries' });
    await within(table).findByText('blocklist.create');
    expect(seen.at(-1)?.get('order')).toBe('asc');
    expect(seen.at(-1)?.get('limit')).toBe('100');
    expect(seen.at(-1)?.has('cursor')).toBe(false);
    fireEvent.click(screen.getByRole('button', { name: 'Load newer events' }));
    await within(table).findByText('zone.update-2');
    expect(seen.at(-1)?.get('cursor')).toBe('older-page-cursor');
    expect(seen.at(-1)?.get('order')).toBe('asc');
    expect(await screen.findByText(/End of retained events/)).toBeInTheDocument();
  });

  it('expands token details inline and keeps credentials hidden', async () => {
    mockDB().audit.unshift({
      ...mockDB().audit[0]!,
      id: 9999,
      action: 'apitoken.delete',
      resource: { kind: 'APIToken', name: 'abcdefgh2345' },
      before: { spec: { description: 'deployment check', owner: { kind: 'User', name: 'admin' }, expiresAt: null, scopes: ['viewer'], secretHash: 'credential-must-stay-hidden' } },
      after: undefined,
      hash: 'test-chain-hash',
      prev_hash: 'test-previous-hash',
      request: { ip: '127.0.0.1', method: 'DELETE', path: '/api/v1/auth/tokens/abcdefgh2345', ua: 'Python-urllib/3.9' },
    });
    renderWithApp(<AuditPage />, { route: '/audit' });
    expect(await screen.findByText('Label: deployment check')).toBeInTheDocument();
    expect(screen.getByText('Owner: User admin')).toBeInTheDocument();
    expect(screen.getByText('Scopes: viewer')).toBeInTheDocument();
    const row = screen.getByText('apitoken.delete').closest('tr')!;
    expect(row).toHaveAttribute('aria-expanded', 'false');
    fireEvent.click(row);
    const details = await screen.findByRole('group', { name: 'Audit event details for apitoken.delete abcdefgh2345' });
    expect(row).toHaveAttribute('aria-expanded', 'true');
    expect(row.nextElementSibling).toContainElement(details);
    expect(row.getAttribute('aria-controls')).toBe(details.id);
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(details).toHaveTextContent('test-chain-hash');
    expect(details).toHaveTextContent('Python-urllib/3.9');
    expect(details).not.toHaveTextContent('credential-must-stay-hidden');
    fireEvent.click(within(row).getByRole('button', { name: 'Collapse row' }));
    expect(screen.queryByRole('group', { name: 'Audit event details for apitoken.delete abcdefgh2345' })).toBeNull();
    pick(getSelector('Kind'), 'APIToken');
    await waitFor(() => expect(within(screen.getByRole('table', { name: 'Audit entries' })).getAllByRole('row')).toHaveLength(2));
  });

  it('widens the row expansion chevron column so roomier themes do not cut it off', async () => {
    renderWithApp(<AuditPage />, { route: '/audit' });
    const table = await screen.findByRole('table', { name: 'Audit entries' });
    await within(table).findAllByRole('button', { name: /expand row/i });
    // The chevron column comes first; its width is set on the header cell.
    const first = within(table).getAllByRole('columnheader')[0]!;
    expect(first.style.width).toBe(`${CHEVRON_COLUMN_WIDTH}px`);
  });

  it('toggles details and navigates rows with the query-table keyboard controls', async () => {
    renderWithApp(<AuditPage />, { route: '/audit' });
    const table = await screen.findByRole('table', { name: 'Audit entries' });
    const rows = within(table).getAllByRole('row').slice(1);
    const first = rows[0]!;
    first.focus();
    fireEvent.keyDown(first, { key: 'Enter' });
    expect(first).toHaveAttribute('aria-expanded', 'true');
    fireEvent.keyDown(first, { key: ' ' });
    expect(first).toHaveAttribute('aria-expanded', 'false');
    fireEvent.keyDown(first, { key: 'ArrowDown' });
    expect(rows[1]).toHaveFocus();
    fireEvent.keyDown(rows[1]!, { key: 'ArrowUp' });
    expect(first).toHaveFocus();
    fireEvent.keyDown(first, { key: 'ArrowUp' });
    expect(first).toHaveFocus();
    fireEvent.click(within(first).getByRole('button', { name: 'Expand row' }));
    expect(first).toHaveAttribute('aria-expanded', 'true');
  });

  it('reports integrity verification failures without claiming success', async () => {
    server.use(http.get('*/api/v1/audit/verify', () => HttpResponse.json({ verified: false, checkedAt: new Date().toISOString(), reason: 'event 4 was changed' })));
    renderWithApp(<AuditPage />, { route: '/audit' });
    fireEvent.click(await screen.findByRole('button', { name: 'Verify integrity' }));
    expect(await screen.findByText('Audit verification failed')).toBeInTheDocument();
    expect(screen.getByText('event 4 was changed')).toBeInTheDocument();
    expect(screen.queryByText('Audit integrity verified')).toBeNull();
  });

  it('verifies the complete trail with no list filters', async () => {
    let query = '';
    server.events.on('request:start', ({ request }) => {
      const url = new URL(request.url);
      if (url.pathname === '/api/v1/audit/verify') query = url.search;
    });
    renderWithApp(<AuditPage />, { route: '/audit' });
    fireEvent.click(await screen.findByRole('button', { name: 'Verify integrity' }));
    expect(await screen.findByText('Audit integrity verified')).toBeInTheDocument();
    expect(query).toBe('');
  });

  it('shows storage failures as unavailable instead of an integrity failure', async () => {
    server.use(
      http.get('*/api/v1/audit/verify', () => HttpResponse.json({ error: { key: 'unavailable', message: 'audit verification is temporarily unavailable' } }, { status: 503 })),
    );
    renderWithApp(<AuditPage />, { route: '/audit' });
    fireEvent.click(await screen.findByRole('button', { name: 'Verify integrity' }));
    expect(await screen.findByText('Verification unavailable')).toBeInTheDocument();
    expect(screen.getByText('audit verification is temporarily unavailable')).toBeInTheDocument();
    expect(screen.queryByText('Audit verification failed')).toBeNull();
    expect(screen.queryByText('Audit integrity verified')).toBeNull();
  });
});
