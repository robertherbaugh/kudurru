import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { mockDB } from '../mocks/handlers';
import { server } from '../test/setup';
import { API, renderWithApp } from '../test/utils';
import ClientsPage, { matcherFor } from './ClientsPage';

describe('ClientsPage', () => {
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
    fireEvent.change(within(dialog).getByRole('combobox', { name: 'Group' }), { target: { value: 'kids' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Add' }));
    await waitFor(() => expect(mockDB().groups.find((g) => g.metadata.name === 'kids')?.spec.clients?.some((c) => c.ip === '192.168.1.40' && c.name === 'nas')).toBe(true));
  });

  it('matches by client ID when the key is not an address', () => {
    expect(matcherFor({ key: 'phone.dot', count: 1, blocked: 0 }, '')).toEqual({ clientID: 'phone.dot' });
    expect(matcherFor({ key: '2001:db8::1', name: 'v6', count: 1, blocked: 0 }, 'c')).toEqual({ ip: '2001:db8::1', name: 'v6', comment: 'c' });
  });
});
