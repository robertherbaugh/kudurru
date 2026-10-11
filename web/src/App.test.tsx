import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { App } from './App';
import { signIn } from './mocks/handlers';
import { ROUTES } from './routes';
import { API } from './test/utils';

function renderApp(route: string) {
  window.history.replaceState(null, '', route);
  return render(<App basePath="/" apiBase={API} />);
}

describe('App', () => {
  it('signs in locally and lands on the dashboard', async () => {
    renderApp('/');
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
    expect(await screen.findByText('Default credentials are active')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Dex (corporate SSO)' })).toHaveAttribute('href', `${API}/auth/oidc/dex/start?next=%2F`);
    fireEvent.change(screen.getByRole('textbox', { name: /Username/ }), { target: { value: 'admin' } });
    fireEvent.change(screen.getByLabelText(/Password/), { target: { value: 'wrong' } });
    fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    fireEvent.change(screen.getByLabelText(/Password/), { target: { value: 'admin' } });
    fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));
    expect(await screen.findByRole('heading', { level: 1, name: 'Dashboard' })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByTestId('tile-total')).not.toHaveTextContent('…'));
    expect(screen.getByText('The admin account still uses its default password')).toBeInTheDocument();
  });

  it('navigates client side from a tile into the pre-filtered log', async () => {
    signIn('admin');
    renderApp('/');
    const tile = await screen.findByRole('listitem', { name: /Queries blocked/ });
    fireEvent.click(tile);
    expect(await screen.findByRole('heading', { level: 1, name: 'Query log' })).toBeInTheDocument();
    expect(window.location.search).toContain('status_class=blocked');
    expect(screen.getByRole('combobox', { name: 'Status' })).toHaveValue('blocked');
  });

  it.each(ROUTES.filter((r) => !r.hidden).map((r) => [r.path, r.label] as const))('renders %s (%s) with a nav entry', async (path, label) => {
    signIn('admin');
    renderApp(path);
    const h1 = await screen.findByRole('heading', { level: 1 }, { timeout: 5000 });
    expect(h1).toBeInTheDocument();
    const nav = screen.getAllByRole('navigation').find((n) => within(n).queryAllByRole('link', { name: new RegExp(`^${label}`) }).length > 0);
    expect(nav).toBeDefined();
  });

  it.each(ROUTES.filter((r) => !r.hidden).map((r) => [r.path] as const))('shows no undefined, NaN or object text on %s', async (path) => {
    signIn('admin');
    renderApp(path);
    await screen.findByRole('heading', { level: 1 }, { timeout: 5000 });
    // Let the page's queries settle, then read every text node.
    await waitFor(() => expect(document.querySelector('[role="status"][aria-live="polite"] .astryx-spinner')).toBeNull(), { timeout: 5000 }).catch(() => undefined);
    await new Promise((r) => setTimeout(r, 300));
    expect(document.body.textContent).not.toMatch(/\bundefined\b|\bNaN\b|\[object Object\]/);
  });

  it('renders a zone with its records', async () => {
    signIn('admin');
    renderApp('/zones/home-arpa');
    expect(await screen.findByRole('heading', { level: 1, name: 'home.arpa.' })).toBeInTheDocument();
    expect(await screen.findByRole('table', { name: 'Records of home.arpa.' })).toBeInTheDocument();
  });

  it('signs out back to the login page', async () => {
    signIn('admin');
    renderApp('/');
    expect(await screen.findByRole('heading', { level: 1, name: 'Dashboard' })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Sign out' }));
    expect(await screen.findByRole('heading', { name: 'Sign in' })).toBeInTheDocument();
  });

  it('shows a not-found page for unknown routes', async () => {
    signIn('admin');
    renderApp('/nope');
    expect(await screen.findByRole('heading', { name: 'Page not found' })).toBeInTheDocument();
  });

  it('serves under a path prefix', async () => {
    signIn('admin');
    window.history.replaceState(null, '', '/kudurru/zones');
    render(<App basePath="/kudurru/" apiBase={API} />);
    expect(await screen.findByRole('heading', { level: 1, name: 'Zones' })).toBeInTheDocument();
    expect(screen.getAllByRole('link', { name: 'Lists' })[0]).toHaveAttribute('href', '/kudurru/lists');
  });
});
