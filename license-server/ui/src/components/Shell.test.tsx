// Permission-driven navigation and the session gate: each role sees the
// sections its permissions allow, a page outside them says "Not
// permitted", a signed-out visitor is sent to the sign-in page with the
// way back (an invite link survives the round trip), and the dev banner
// appears with a test signer.
import { screen, waitFor, within } from '@testing-library/react';
import { renderApp } from '../test/render';
import { adminMe, catalog, customerMe, openRequest, superMe, systemInfo } from '../dev/fixtures';
import { DEV_BANNER } from '../lib/words';

function navLinks(): string[] {
  const nav = screen.getAllByRole('navigation').find((n) => within(n).queryByRole('link', { name: 'My licenses' }));
  if (!nav) throw new Error('no side navigation');
  return within(nav)
    .getAllByRole('link')
    .map((a) => a.textContent?.replace(/\d+$/, '').trim() ?? '');
}

test('a customer sees only their own licenses in the navigation', async () => {
  renderApp({ route: '/my/licenses', me: customerMe, handlers: { myLicenses: () => ({ items: [] }), myRequests: () => ({ items: [] }) } });
  expect(await screen.findByRole('heading', { level: 1, name: 'My licenses' })).toBeInTheDocument();
  const links = navLinks();
  expect(links).toContain('My licenses');
  for (const hidden of ['Licenses', 'Customers', 'Requests', 'Users', 'Audit', 'System']) expect(links).not.toContain(hidden);
});

test('a license admin sees Licensing and Administration, with the open request count', async () => {
  const { api } = renderApp({
    route: '/my/licenses',
    me: adminMe,
    handlers: { myLicenses: () => ({ items: [] }), myRequests: () => ({ items: [] }), requestsList: () => ({ items: [openRequest] }) },
  });
  await screen.findByRole('heading', { level: 1, name: 'My licenses' });
  await waitFor(() => expect(api.callsTo('requestsList')).toHaveLength(1));
  expect(api.callsTo('requestsList')[0]?.query.get('status')).toBe('open');
  const links = navLinks();
  for (const shown of ['My licenses', 'Licenses', 'Customers', 'Requests', 'Users', 'Audit', 'System']) expect(links).toContain(shown);
  const requests = screen.getByRole('link', { name: /^Requests/ });
  await waitFor(() => expect(requests).toHaveTextContent('1'));
});

test('a customer does not ask for the open request count', async () => {
  const { api } = renderApp({ route: '/my/licenses', me: customerMe, handlers: { myLicenses: () => ({ items: [] }), myRequests: () => ({ items: [] }) } });
  await screen.findByRole('heading', { level: 1, name: 'My licenses' });
  expect(api.callsTo('requestsList')).toHaveLength(0);
});

test('a page outside the roles says so and asks the API nothing for it', async () => {
  const { api } = renderApp({ route: '/users', me: customerMe });
  expect(await screen.findByRole('heading', { level: 1, name: 'Not permitted' })).toBeInTheDocument();
  expect(api.callsTo('usersList')).toHaveLength(0);
});

test('a superadmin reaches the System page', async () => {
  renderApp({ route: '/system', me: superMe, handlers: { system: () => systemInfo } });
  expect(await screen.findByRole('heading', { level: 1, name: 'System' })).toBeInTheDocument();
});

test('a signed-out visitor to an invite goes to the sign-in page with the way back', async () => {
  renderApp({ route: '/invites/tok_abc', me: null, handlers: { authConfig: () => ({ displayName: 'Sign in with Dex', devMode: false }) } });
  const button = await screen.findByRole('link', { name: 'Sign in with Dex' });
  expect(window.location.pathname).toBe('/login');
  expect(new URLSearchParams(window.location.search).get('next')).toBe('/invites/tok_abc');
  expect(button).toHaveAttribute('href', '/api/v1/auth/login?next=%2Finvites%2Ftok_abc');
});

test('the dev banner shows while the signer is a test key', async () => {
  renderApp({
    route: '/my/licenses',
    me: customerMe,
    catalog: catalog({ signing: { available: true, kid: 'dev-1', testKey: true } }),
    handlers: { myLicenses: () => ({ items: [] }), myRequests: () => ({ items: [] }) },
  });
  expect(await screen.findByText(DEV_BANNER)).toBeInTheDocument();
});
