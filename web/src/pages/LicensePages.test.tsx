// The license page, the banner above every page, and the pages that ask for
// a key instead of their content. docs/architecture/licensing.md.
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { LicenseBanner } from '../components/LicenseBanner';
import { communityLicense } from '../mocks/fixtures';
import { mockDB } from '../mocks/handlers';
import { server } from '../test/setup';
import { API, renderWithApp } from '../test/utils';
import AccessPage from './AccessPage';
import LicensePage from './LicensePage';
import LongTermPage from './LongTermPage';
import PoliciesPage from './PoliciesPage';
import RequestsPage from './RequestsPage';

/** Counts the requests the server sees for a path while a test runs. */
function watch(path: string): { count: () => number } {
  let n = 0;
  server.events.on('request:start', ({ request }) => {
    if (new URL(request.url).pathname.endsWith(path)) n++;
  });
  return { count: () => n };
}

describe('LicensePage', () => {
  it('shows the key, the limits and every feature as included', async () => {
    renderWithApp(<LicensePage />, { route: '/license' });
    expect(await screen.findByRole('heading', { level: 1, name: 'License' })).toBeInTheDocument();
    expect(await screen.findByText('Personal (free, non-commercial)')).toBeInTheDocument();
    expect(screen.getByText('Homelab Hank')).toBeInTheDocument();
    expect(screen.getByText('lic_mock0000001')).toBeInTheDocument();
    expect(screen.getByTestId('license-nodes')).toHaveTextContent('2 of 3');
    expect(screen.getByText('no limit')).toBeInTheDocument();
    for (const f of ['rbac', 'oidc', 'audit', 'longterm', 'requests', 'edge', 'integrations']) {
      expect(screen.getByTestId(`feature-${f}`)).toHaveTextContent('Included');
    }
    expect(screen.getByText('hello@kudurru.io').closest('a')).toHaveAttribute('href', 'mailto:hello@kudurru.io');
    expect(document.body.textContent).not.toMatch(/undefined|NaN|\[object Object\]/);
  });

  it('shows a node without a key as locked, with the free limits', async () => {
    mockDB().license = communityLicense();
    renderWithApp(<LicensePage />, { route: '/license' });
    expect(await screen.findByText('No key', { selector: 'span' })).toBeInTheDocument();
    expect(screen.getByTestId('license-nodes')).toHaveTextContent('2 of 1');
    expect(screen.getByTestId('license-nodes')).toHaveTextContent('over the limit');
    expect(screen.getByText('up to 2')).toBeInTheDocument();
    for (const f of ['rbac', 'oidc', 'audit', 'longterm', 'requests', 'edge', 'integrations']) {
      expect(screen.getByTestId(`feature-${f}`)).toHaveTextContent('Needs a key');
    }
  });

  it('explains a failed lookup instead of crashing', async () => {
    server.use(http.get(`${API}/license`, () => HttpResponse.json({ error: { key: 'forbidden', message: 'forbidden' } }, { status: 403 })));
    renderWithApp(<LicensePage />, { route: '/license' });
    expect(await screen.findByText('Not permitted')).toBeInTheDocument();
  });
});

describe('LicenseBanner', () => {
  it('is silent for a healthy key and for a node without one', async () => {
    const seen = watch('/license');
    const { container } = renderWithApp(<LicenseBanner />);
    await waitFor(() => expect(seen.count()).toBeGreaterThan(0));
    expect(screen.queryByTestId('license-banner')).toBeNull();
    expect(container.textContent).toBe('');
  });

  it('warns about a cluster over its node limit and links to the license page', async () => {
    mockDB().license = { ...mockDB().license, nodes: 1 };
    renderWithApp(<LicenseBanner />);
    const b = await screen.findByTestId('license-banner');
    expect(b).toHaveTextContent('More nodes than the license allows');
    expect(within(b).getByRole('link', { name: 'View license' })).toHaveAttribute('href', '/license');
  });

  it('reports an invalid key with the reason', async () => {
    mockDB().license = { ...communityLicense(), state: 'invalid', error: 'license key is signed with a key this build does not trust' };
    mockDB().nodes = mockDB().nodes.slice(0, 1);
    renderWithApp(<LicenseBanner />);
    const b = await screen.findByTestId('license-banner');
    expect(b).toHaveTextContent('The license key is invalid');
    expect(b).toHaveTextContent('does not trust');
  });

  it('lets the expiry reminder be closed for the session, but not an error', async () => {
    mockDB().license = { ...mockDB().license, expiresAt: new Date(Date.now() + 9 * 86_400_000).toISOString() };
    renderWithApp(<LicenseBanner />);
    const b = await screen.findByTestId('license-banner');
    expect(b).toHaveTextContent('expires soon');
    fireEvent.click(within(b).getByRole('button', { name: /dismiss|close/i }));
    await waitFor(() => expect(screen.queryByTestId('license-banner')).toBeNull());
  });
});

describe('pages behind a key', () => {
  it('Long-term asks for a key and requests nothing', async () => {
    mockDB().license = communityLicense();
    const seen = watch('/stats/timeseries');
    renderWithApp(<LongTermPage />, { route: '/history' });
    expect(await screen.findByTestId('locked-longterm')).toHaveTextContent('Long-term statistics needs a license key');
    expect(screen.getByRole('heading', { level: 1, name: 'Long-term' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'View license' })).toHaveAttribute('href', '/license');
    expect(seen.count()).toBe(0);
  });

  it('Requests asks for a key and requests nothing', async () => {
    mockDB().license = communityLicense();
    const seen = watch('/unblockrequests');
    renderWithApp(<RequestsPage />, { route: '/requests' });
    expect(await screen.findByTestId('locked-requests')).toHaveTextContent('Unblock requests needs a license key');
    expect(seen.count()).toBe(0);
  });

  it('the Audit tab asks for a key and requests nothing', async () => {
    mockDB().license = communityLicense();
    const seen = watch('/audit');
    renderWithApp(<AccessPage />, { route: '/access?tab=audit' });
    expect(await screen.findByTestId('locked-audit')).toHaveTextContent('The audit trail needs a license key');
    expect(seen.count()).toBe(0);
  });

  it('shows the feature again with a key', async () => {
    renderWithApp(<RequestsPage />, { route: '/requests' });
    expect(await screen.findByRole('heading', { level: 1, name: 'Requests' })).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByTestId('locked-requests')).toBeNull());
  });

  it('the Identity tab says bindings and providers are switched off without a key', async () => {
    mockDB().license = communityLicense();
    renderWithApp(<AccessPage />, { route: '/access?tab=iam' });
    expect(await screen.findByTestId('rbac-locked')).toHaveTextContent('Roles need a license key');
    expect(await screen.findByTestId('oidc-locked')).toHaveTextContent('Single sign-on needs a license key');
  });

  it('Policies stops at the free limit: the notice, and "New policy" disabled', async () => {
    mockDB().license = communityLicense();
    expect(mockDB().policies.length).toBeGreaterThanOrEqual(2);
    renderWithApp(<PoliciesPage />, { route: '/policies' });
    expect(await screen.findByTestId('policy-limit')).toHaveTextContent('The free limit of 2 policies is reached');
    expect(screen.getByRole('button', { name: 'New policy' })).toHaveAttribute('aria-disabled', 'true');
  });

  it('Policies has no limit with a key', async () => {
    renderWithApp(<PoliciesPage />, { route: '/policies' });
    await screen.findByRole('table', { name: 'Policies' });
    expect(screen.queryByTestId('policy-limit')).toBeNull();
    expect(screen.getByRole('button', { name: 'New policy' })).not.toHaveAttribute('aria-disabled', 'true');
  });
});
