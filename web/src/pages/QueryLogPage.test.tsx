import { fireEvent, screen, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { mockDB } from '../mocks/handlers';
import { server } from '../test/setup';
import { API, renderWithApp } from '../test/utils';
import QueryLogPage, { filtersFrom, fromLocalInput, toLocalInput } from './QueryLogPage';

/** Records the query strings of GET /queries while passing through to the mock. */
function spyQueries(): URLSearchParams[] {
  const seen: URLSearchParams[] = [];
  server.events.on('request:start', ({ request }) => {
    const u = new URL(request.url);
    if (u.pathname === '/api/v1/queries') seen.push(u.searchParams);
  });
  return seen;
}

describe('QueryLogPage', () => {
  it('pages through the log with the keyset cursor', async () => {
    const seen = spyQueries();
    renderWithApp(<QueryLogPage />, { route: '/queries' });
    expect(await screen.findByText('Showing 100 queries; more available')).toBeInTheDocument();
    expect(screen.getByTestId('log-totals')).toHaveTextContent('500 of 500 queries match');
    expect(seen[0]?.get('cursor')).toBeNull();
    expect(seen[0]?.get('limit')).toBe('100');

    fireEvent.click(screen.getByRole('button', { name: 'Load more' }));
    expect(await screen.findByText('Showing 200 queries; more available')).toBeInTheDocument();
    const lastID = mockDB().queries[99]?.id;
    expect(seen[1]?.get('cursor')).toBe(String(lastID));

    for (let n = 300; n <= 500; n += 100) {
      fireEvent.click(screen.getByRole('button', { name: 'Load more' }));
      await screen.findByText(n === 500 ? 'Showing 500 queries' : `Showing ${n} queries; more available`);
    }
    expect(screen.queryByRole('button', { name: 'Load more' })).toBeNull();
  });

  it('applies URL filters and sends the node scope', async () => {
    const seen = spyQueries();
    window.localStorage.setItem('kudurru.scope', JSON.stringify({ scope: 'node', node: 'kudurru-1' }));
    renderWithApp(<QueryLogPage />, { route: '/queries?status_class=blocked&domain=*.doubleclick.net' });
    await screen.findByTestId('log-totals');
    expect(seen[0]?.get('status_class')).toBe('blocked');
    expect(seen[0]?.get('domain')).toBe('*.doubleclick.net');
    expect(seen[0]?.get('scope')).toBe('node');
    expect(seen[0]?.get('node')).toBe('kudurru-1');
    expect(screen.getByRole('combobox', { name: 'Status' })).toHaveValue('blocked');
  });

  it('expands the "why" row from the keyboard', async () => {
    renderWithApp(<QueryLogPage />, { route: '/queries?status_class=blocked' });
    await screen.findByTestId('log-totals');
    const rows = await screen.findAllByRole('row', { expanded: false });
    const first = rows[0]!;
    fireEvent.keyDown(first, { key: 'Enter' });
    expect(first).toHaveAttribute('aria-expanded', 'true');
    const why = document.querySelector<HTMLElement>('dl.k-why-grid');
    expect(why).not.toBeNull();
    expect(why).toHaveAttribute('aria-label', expect.stringMatching(/^Why .* got this answer$/) as string);
    expect(within(why!).getByText('Matched list')).toBeInTheDocument();
    expect(within(why!).getByText('Node')).toBeInTheDocument();
    fireEvent.keyDown(first, { key: ' ' });
    expect(first).toHaveAttribute('aria-expanded', 'false');
  });

  it('allows a blocked domain from its row and confirms with a toast', async () => {
    renderWithApp(<QueryLogPage />, { route: '/queries?status_class=blocked' });
    await screen.findByTestId('log-totals');
    const [allow] = await screen.findAllByRole('button', { name: 'Allow' });
    fireEvent.click(allow!);
    expect(await screen.findByText(/^Allowed .* \(list ui-allow\)$/)).toBeInTheDocument();
    const list = mockDB().lists.find((l) => l.metadata.name === 'ui-allow');
    expect(list?.spec.sources[0]?.inline?.length).toBe(3);
  });

  it('shows the API error when the log cannot be read', async () => {
    server.use(http.get(`${API}/queries`, () => HttpResponse.json({ error: { key: 'unavailable', message: 'querylog.db is locked' } }, { status: 503 })));
    renderWithApp(<QueryLogPage />, { route: '/queries' });
    expect(await screen.findByText('querylog.db is locked')).toBeInTheDocument();
  });

  it('disables row actions for a viewer', async () => {
    renderWithApp(<QueryLogPage />, { route: '/queries?status_class=blocked', user: 'viewer' });
    await screen.findByTestId('log-totals');
    const [allow] = await screen.findAllByRole('button', { name: 'Allow' });
    expect(allow).toHaveAttribute('aria-disabled', 'true');
  });
});

describe('filter helpers', () => {
  it('reads only known filter keys', () => {
    expect(filtersFrom(new URLSearchParams('domain=a.com&x=1&status_class=blocked&cursor=9'))).toEqual({ domain: 'a.com', status_class: 'blocked' });
  });

  it('round-trips datetime-local values', () => {
    const iso = '2026-10-10T17:55:00.000Z';
    expect(fromLocalInput(toLocalInput(iso))).toBe(iso);
    expect(toLocalInput(undefined)).toBe('');
    expect(fromLocalInput('')).toBe('');
  });
});
