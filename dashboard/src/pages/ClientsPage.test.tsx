import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { mockDB } from '../mocks/handlers';
import { getSelector, optionLabels, pick } from '../test/controls';
import { server } from '../test/setup';
import { API, renderWithApp } from '../test/utils';
import { ScopeSelector } from '../components/ScopeSelector';
import ClientsPage, { matcherFor } from './ClientsPage';

describe('ClientsPage', () => {
  it('requests the selected scope and shows central and partial inventory status', async () => {
    const seen: URLSearchParams[] = [];
    server.use(http.get(`${API}/clients`, ({ request }) => {
      const p = new URL(request.url).searchParams;
      seen.push(p);
      const node = p.get('node');
      return HttpResponse.json({
        scope: node ? 'node' : 'cluster',
        nodes: node ? [{ name: node, ok: true }] : [{ name: 'kudurru-0', ok: true }, { name: 'kudurru-1', ok: false }],
        partial: !node,
        ...(!node ? { source: 'central' } : {}),
        items: [{ key: node ? '192.0.2.9' : '192.0.2.1', name: node ? 'remote-device' : 'cluster-device', count: node ? 4 : 7, blocked: 1 }],
      });
    }));
    renderWithApp(<><ScopeSelector /><ClientsPage /></>, { route: '/clients' });
    await screen.findByRole('link', { name: 'cluster-device' });
    expect(seen[0]?.get('scope')).toBe('cluster');
    expect(seen[0]?.get('window')).toBe('24h');
    expect(seen[0]?.has('node')).toBe(false);
    expect(screen.getByTestId('scope-note')).toHaveTextContent('All nodes (2) (central database) · Partial results: 1 node delayed or unavailable');
    await waitFor(() => expect(optionLabels(getSelector('Node scope'))).toContain('kudurru-1'));
    pick(getSelector('Node scope'), 'kudurru-1');
    const row = (await screen.findByRole('link', { name: 'remote-device' })).closest('tr')!;
    expect(within(row).getByRole('link', { name: '4' })).toHaveAttribute('href', '/queries?client=192.0.2.9');
    expect(seen.at(-1)?.get('scope')).toBe('node');
    expect(seen.at(-1)?.get('node')).toBe('kudurru-1');
    expect(screen.getByTestId('scope-note')).toHaveTextContent('Node kudurru-1');
  });

  it('renders the server rows: key, name, group, counts and last seen', async () => {
    renderWithApp(<ClientsPage />, { route: '/clients' });
    const table = await screen.findByRole('table', { name: 'Clients' });
    const row = within(table).getByRole('link', { name: 'kids-tablet' }).closest('tr')!;
    expect(within(row).getByText('192.168.1.30')).toBeInTheDocument();
    expect(within(row).getByText('kids')).toBeInTheDocument();
    expect(row.textContent).not.toMatch(/undefined|NaN|never/);
    expect(table.textContent).not.toMatch(/undefined|NaN/);
  });

  it('survives the bare top-clients shape (key only, no name or last_seen)', async () => {
    server.use(http.get(`${API}/clients`, () => HttpResponse.json({ items: [{ key: '127.0.0.1', group: 'default/default', count: 3, blocked: 0 }], took_ms: 1 })));
    renderWithApp(<ClientsPage />, { route: '/clients' });
    const link = await screen.findByRole('link', { name: '127.0.0.1' });
    const row = link.closest('tr')!;
    expect(within(row).getByText('default')).toBeInTheDocument();
    expect(within(row).getByText('3')).toBeInTheDocument();
    fireEvent.change(screen.getByPlaceholderText('Search by address, name or group'), { target: { value: '127.0' } });
    expect(screen.getByRole('link', { name: '127.0.0.1' })).toBeInTheDocument();
  });

  it('adds the selected clients to a group as ip matchers', async () => {
    renderWithApp(<ClientsPage />, { route: '/clients' });
    fireEvent.click(await screen.findByRole('checkbox', { name: 'Select nas' }));
    const add = screen.getByRole('button', { name: 'Add 1 to group' });
    await waitFor(() => expect(add).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(add);
    const dialog = await screen.findByRole('dialog');
    pick(getSelector('Group', dialog), 'kids');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Add' }));
    await waitFor(() => expect(mockDB().groups.find((g) => g.metadata.name === 'kids')?.spec.clients?.some((c) => c.ip === '192.168.1.40' && c.name === 'nas')).toBe(true));
  });

  it('matches by client ID when the key is not an address', () => {
    expect(matcherFor({ key: 'phone.dot', count: 1, blocked: 0 }, '')).toEqual({ clientID: 'phone.dot' });
    expect(matcherFor({ key: '2001:db8::1', name: 'v6', count: 1, blocked: 0 }, 'c')).toEqual({ ip: '2001:db8::1', name: 'v6', comment: 'c' });
  });
});
