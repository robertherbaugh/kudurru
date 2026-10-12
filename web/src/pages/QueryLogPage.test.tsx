import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { collectEntries } from '../mocks/domains';
import { mockDB } from '../mocks/handlers';
import { getSelector, valueOf } from '../test/controls';
import { server } from '../test/setup';
import { API, renderWithApp, uiEntryValues } from '../test/utils';
import QueryLogPage, { filtersFrom, fromLocalInput, requestsByDomain, toLocalInput, unblockLink } from './QueryLogPage';

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
  it('retains same-ID queries from different nodes and expands them independently', async () => {
    const original = mockDB().queries[0]!;
    const queries = [{ ...original, node: 'node-a', domain: 'a-cluster.test' }, { ...original, node: 'node-b', domain: 'b-cluster.test' }];
    server.use(http.get(`${API}/queries`, () => HttpResponse.json({ scope: 'cluster', nodes: [{ name: 'node-a', ok: true }, { name: 'node-b', ok: true }], partial: false, queries, records_total: 2, records_filtered: 2 })));
    renderWithApp(<QueryLogPage />, { route: '/queries' });
    expect(await screen.findByText('Showing 2 queries')).toBeInTheDocument();
    const a = await logRow('a-cluster.test');
    const b = await logRow('b-cluster.test');
    fireEvent.keyDown(a, { key: 'Enter' });
    expect(a).toHaveAttribute('aria-expanded', 'true');
    expect(b).toHaveAttribute('aria-expanded', 'false');
    fireEvent.keyDown(b, { key: 'Enter' });
    expect(a.getAttribute('aria-controls')).not.toBe(b.getAttribute('aria-controls'));
  });

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
    expect(valueOf(getSelector('Status'))).toBe('blocked');
  });

  it('expands the "why" row from the keyboard', async () => {
    renderWithApp(<QueryLogPage />, { route: '/queries?status_class=blocked' });
    await screen.findByTestId('log-totals');
    const rows = await screen.findAllByRole('row', { expanded: false });
    const first = rows[0]!;
    fireEvent.keyDown(first, { key: 'Enter' });
    expect(first).toHaveAttribute('aria-expanded', 'true');
    const why = screen.getByLabelText(/^Why .* got this answer$/);
    expect(first).toHaveAttribute('aria-controls', why.closest('tr')?.id);
    expect(within(why).getByText('Matched list')).toBeInTheDocument();
    expect(within(why).getByText('Node')).toBeInTheDocument();
    expect(within(why).getByText('Policy')).toBeInTheDocument();
    fireEvent.keyDown(first, { key: ' ' });
    expect(first).toHaveAttribute('aria-expanded', 'false');
  });

  it('allows a blocked domain from its row and confirms with a toast', async () => {
    renderWithApp(<QueryLogPage />, { route: '/queries?status_class=blocked' });
    await screen.findByTestId('log-totals');
    const [allow] = await screen.findAllByRole('button', { name: 'Allow' });
    // Enabled once GET /auth/me has loaded the permissions.
    await waitFor(() => expect(allow).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(allow!);
    expect(await screen.findByText(/^Allowed .* \(list ui-allow\)$/)).toBeInTheDocument();
    // An exact entry for everyone, from the query log (POST /allowlist).
    const added = collectEntries(mockDB(), 'allow').find((e) => e.origin === 'querylog' && e.value !== 'clickserve.dartsearch.net');
    expect(added).toMatchObject({ type: 'exact', everyone: true, groups: [] });
    expect(uiEntryValues('ui-allow')).toHaveLength(6);
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

/** The log row (not the "why" row) of a name. */
async function logRow(domain: string): Promise<HTMLElement> {
  let found: HTMLElement | undefined;
  await waitFor(() => {
    found = screen.getAllByRole('row').find((r) => r.hasAttribute('aria-expanded') && within(r).queryByText(domain, { exact: true }));
    expect(found).toBeDefined();
  });
  return found!;
}

describe('QueryLogPage policy filter', () => {
  it('suggests policies (field=policy) and filters by the picked one', async () => {
    const seen = spyQueries();
    const asked: URLSearchParams[] = [];
    server.events.on('request:start', ({ request }) => {
      const u = new URL(request.url);
      if (u.pathname === '/api/v1/queries/suggestions') asked.push(u.searchParams);
    });
    renderWithApp(<QueryLogPage />, { route: '/queries' });
    await screen.findByTestId('log-totals');
    const box = screen.getByRole('combobox', { name: /^Policy/ });
    fireEvent.focus(box);
    fireEvent.change(box, { target: { value: 'kids' } });
    fireEvent.click(await screen.findByRole('option', { name: 'default/kids-bedtime' }));
    await waitFor(() => expect(new URLSearchParams(window.location.search).get('policy')).toBe('default/kids-bedtime'));
    expect(asked.some((p) => p.get('field') === 'policy' && p.get('prefix') === 'kids')).toBe(true);
    await waitFor(() => expect(seen.at(-1)?.get('policy')).toBe('default/kids-bedtime'));
    // Every row the mock answers was decided by that policy.
    await waitFor(() => expect(screen.getByTestId('log-totals')).toHaveTextContent(/^\d+ of 500 queries match/));
    expect(mockDB().queries.filter((q) => q.policy === 'default/kids-bedtime').length).toBeGreaterThan(0);

    fireEvent.click(screen.getByRole('button', { name: 'Clear filters' }));
    await waitFor(() => expect(window.location.search).toBe(''));
  });

  it('reads the policy filter from the URL into the box', async () => {
    const seen = spyQueries();
    renderWithApp(<QueryLogPage />, { route: '/queries?policy=default%2Fiot-strict' });
    await screen.findByTestId('log-totals');
    expect(seen[0]?.get('policy')).toBe('default/iot-strict');
    expect(screen.getByText('default/iot-strict')).toBeInTheDocument();
  });
});

describe('QueryLogPage unblock link', () => {
  it('links a blocked name with requests to them on /requests', async () => {
    renderWithApp(<QueryLogPage />, { route: '/queries?status_class=blocked&domain=tiktokv.com' });
    const row = await logRow('tiktokv.com');
    fireEvent.keyDown(row, { key: 'Enter' });
    const why = screen.getByLabelText('Why tiktokv.com got this answer');
    const link = await within(why).findByRole('link', { name: 'Requests' });
    expect(link).toHaveAttribute('href', '/requests?domain=tiktokv.com&state=all');
    expect(within(why).getByText('1 unblock request for this name, 1 pending')).toBeInTheDocument();
  });

  it('links a blocked name without requests to the Allowlist add dialog', async () => {
    renderWithApp(<QueryLogPage />, { route: '/queries?status_class=blocked&domain=app-measurement.com' });
    const row = await logRow('app-measurement.com');
    fireEvent.keyDown(row, { key: 'Enter' });
    const why = screen.getByLabelText('Why app-measurement.com got this answer');
    const link = await within(why).findByRole('link', { name: 'Allow…' });
    expect(link).toHaveAttribute('href', '/allowlist?add=app-measurement.com');
  });

  it('shows no unblock link on a permitted row', async () => {
    renderWithApp(<QueryLogPage />, { route: '/queries?status_class=cached' });
    await screen.findByTestId('log-totals');
    const [first] = await screen.findAllByRole('row', { expanded: false });
    fireEvent.keyDown(first!, { key: 'Enter' });
    const why = screen.getByLabelText(/^Why .* got this answer$/);
    expect(within(why).queryByText('Unblock')).toBeNull();
  });
});

describe('unblockLink', () => {
  const reqs = requestsByDomain([
    { domain: 'a.example', state: 'Denied' },
    { domain: 'A.example', state: 'Pending' },
    { domain: 'b.example', state: 'Approved' },
  ]);

  it('counts the requests per name, case-insensitively', () => {
    expect(reqs.get('a.example')).toEqual({ total: 2, pending: 1 });
    expect(reqs.get('b.example')).toEqual({ total: 1, pending: 0 });
  });

  it('links to the requests of a name (any state), trailing dot and case ignored', () => {
    expect(unblockLink('A.Example.', reqs, false)).toEqual({ href: '/requests?domain=a.example&state=all', label: 'Requests', note: '2 unblock requests for this name, 1 pending' });
    expect(unblockLink('b.example', reqs, true)?.note).toBe('1 unblock request for this name');
  });

  it('offers the Allowlist add dialog otherwise, only to roles that can write lists', () => {
    expect(unblockLink('c.example.', reqs, true)?.href).toBe('/allowlist?add=c.example');
    expect(unblockLink('c.example', reqs, false)).toBeNull();
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
