import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { UnblockApproval } from '../api/types';
import { UnblockBadge, UnblockNotice } from '../components/UnblockBadge';
import { approvalMessage } from '../components/unblock/UnblockDialogs';
import { HOSTILE } from '../mocks/fixtures';
import { mockDB } from '../mocks/handlers';
import { getSelector, optionLabels, optionsOf, pick, valueOf } from '../test/controls';
import { server } from '../test/setup';
import { API, renderWithApp } from '../test/utils';
import { approvalBody, approvalDurationError, blockSource, scopeOptions } from '../lib/unblock';
import RequestDetailPage, { recentFilter } from './RequestDetailPage';
import RequestsPage from './RequestsPage';

const req = (domain: string) => mockDB().unblock.find((r) => r.domain === domain)!;

/** Records the JSON bodies of POSTs to paths ending in `suffix` (the mock still answers). */
function spyBodies(suffix: string): unknown[] {
  const seen: unknown[] = [];
  server.events.on('request:start', ({ request }) => {
    if (request.method === 'POST' && new URL(request.url).pathname.endsWith(suffix)) {
      void request
        .clone()
        .json()
        .then((b) => seen.push(b));
    }
  });
  return seen;
}

async function row(domain: string) {
  return screen.findByTestId(`request-${domain}`);
}

describe('RequestsPage', () => {
  it('lists the pending requests newest first with requester, reason and why blocked', async () => {
    renderWithApp(<RequestsPage />, { route: '/requests' });
    const tiktok = await row('tiktokv.com');
    const rows = screen.getAllByTestId(/^request-/).filter((e) => e.tagName === 'TR');
    expect(rows.map((r) => r.dataset.testid)).toEqual(['request-tiktokv.com', 'request-graph.facebook.com', 'request-www.roblox.com']);
    // Full client: linked to its page; groups and the deciding list linked too.
    expect(within(tiktok).getByRole('link', { name: 'tiktokv.com' })).toHaveAttribute('href', '/requests/default/tiktokv-com-c0a8011e');
    expect(within(tiktok).getByRole('link', { name: '192.168.1.30' })).toHaveAttribute('href', '/clients/192.168.1.30');
    expect(within(tiktok).getByRole('link', { name: 'kids' })).toHaveAttribute('href', '/groups/default/kids');
    expect(within(tiktok).getByRole('link', { name: 'starter' })).toHaveAttribute('href', '/lists/default/starter');
    expect(within(tiktok).getByRole('link', { name: 'kids-bedtime' })).toHaveAttribute('href', '/policies/default/kids-bedtime');
    expect(within(tiktok).getByText('asked 2 times')).toBeInTheDocument();
    expect(within(tiktok).getByText('Pending')).toBeInTheDocument();
    // Prefix: the network address with the privacy note, not a link.
    const fb = screen.getByTestId('request-graph.facebook.com');
    expect(within(fb).getByText('192.168.1.0')).toBeInTheDocument();
    expect(within(fb).queryByRole('link', { name: '192.168.1.0' })).toBeNull();
    expect(within(fb).getByText(/anonymised by query-log privacy/)).toBeInTheDocument();
    // Hidden: said so; a blocked service is text.
    const roblox = screen.getByTestId('request-www.roblox.com');
    expect(within(roblox).getByText('hidden by query-log privacy')).toBeInTheDocument();
    expect(within(roblox).getByText(/service roblox/)).toBeInTheDocument();
    // The state filter counts every state.
    const state = getSelector(/^State/);
    expect(optionLabels(state)).toContain('Pending (3)');
    expect(optionLabels(state)).toContain('All (6)');
  });

  it('renders what the requester typed as text, never as HTML', async () => {
    renderWithApp(<RequestsPage />, { route: '/requests' });
    const fb = await row('graph.facebook.com');
    expect(within(fb).getByText(`${HOSTILE} (guest@example.com)`)).toBeInTheDocument();
    expect(within(fb).getByText(`${HOSTILE} please`)).toBeInTheDocument();
    expect(document.querySelector('img[src="x"]')).toBeNull();
    expect(document.querySelector('[onerror]')).toBeNull();
  });

  it('filters by state through the URL', async () => {
    renderWithApp(<RequestsPage />, { route: '/requests' });
    await row('tiktokv.com');
    pick(getSelector(/^State/), /^Approved/);
    const dc = await row('doubleclick.net');
    expect(window.location.search).toBe('?state=Approved');
    expect(screen.queryByTestId('request-tiktokv.com')).toBeNull();
    // An approval in force can be revoked, not approved again.
    expect(within(dc).queryByRole('button', { name: 'Approve' })).toBeNull();
    expect(within(dc).getByRole('button', { name: 'Deny' })).toBeInTheDocument();
    pick(getSelector(/^State/), /^All/);
    await row('cdn.tracker-cname.example');
    expect(screen.getAllByTestId(/^request-/).filter((e) => e.tagName === 'TR')).toHaveLength(6);
  });

  it('pages with the continue token', async () => {
    const base = mockDB().unblock[0]!;
    for (let i = 0; i < 60; i++) {
      mockDB().unblock.push({ ...base, name: `extra-${i}`, domain: `extra${i}.example`, requestedAt: new Date(Date.UTC(2026, 0, 1, 0, i)).toISOString() });
    }
    renderWithApp(<RequestsPage />, { route: '/requests' });
    await row('tiktokv.com');
    expect(screen.getAllByTestId(/^request-/).filter((e) => e.tagName === 'TR')).toHaveLength(50);
    expect(screen.getByText('50 of 63')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Load more' }));
    await waitFor(() => expect(screen.getAllByTestId(/^request-/).filter((e) => e.tagName === 'TR')).toHaveLength(63));
    expect(screen.queryByRole('button', { name: 'Load more' })).toBeNull();
  });

  it('approves for the group, for 8 hours, with a note', async () => {
    const bodies = spyBodies('/approve');
    renderWithApp(<RequestsPage />, { route: '/requests' });
    fireEvent.click(await within(await row('tiktokv.com')).findByRole('button', { name: 'Approve' }));
    const dialog = await screen.findByRole('dialog');
    const scope = getSelector('Scope', dialog);
    expect(within(dialog).getByText(/attached to the default policy and the requester's policies/)).toBeInTheDocument();
    pick(scope, /^The requester's group/);
    expect(within(dialog).getByText('Adds it to ui-allow-kids, attached to the policies of client group kids only.')).toBeInTheDocument();
    pick(getSelector('Match', dialog), 'This name and its subdomains');
    const duration = getSelector('Duration', dialog);
    expect(optionLabels(duration)).toEqual(['Permanently', '1 hour', '8 hours', '1 day', '7 days', 'Custom…']);
    pick(duration, '8 hours');
    fireEvent.change(within(dialog).getByRole('textbox', { name: /^Note/ }), { target: { value: 'Until the project is done' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Approve' }));
    await waitFor(() => expect(req('tiktokv.com').state).toBe('Approved'));
    expect(bodies).toEqual([{ scope: 'group', match: 'subdomains', group: 'default/kids', duration: '8h', note: 'Until the project is done' }]);
    const r = req('tiktokv.com');
    expect(r.allow).toMatchObject({ scope: 'group', group: 'default/kids', list: 'default/ui-allow-kids', entry: '||tiktokv.com^' });
    expect(r.allow?.expiresAt).toBeDefined();
    expect(r.note).toBe('Until the project is done');
    expect(await screen.findByText(/^Allowed tiktokv\.com for kids until .* \(list ui-allow-kids\)\. ui-allow-kids attached to kids-bedtime, streaming-off\.$/)).toBeInTheDocument();
    // Gone from the pending list.
    await waitFor(() => expect(screen.queryByTestId('request-tiktokv.com')).toBeNull());
  });

  it('approves permanently for everyone by default (no duration sent)', async () => {
    const bodies = spyBodies('/approve');
    renderWithApp(<RequestsPage />, { route: '/requests' });
    fireEvent.click(await within(await row('graph.facebook.com')).findByRole('button', { name: 'Approve' }));
    const dialog = await screen.findByRole('dialog');
    expect(valueOf(getSelector('Duration', dialog))).toBe('');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Approve' }));
    await waitFor(() => expect(req('graph.facebook.com').state).toBe('Approved'));
    expect(bodies).toEqual([{ scope: 'everyone', match: 'exact' }]);
    expect(req('graph.facebook.com').allow).toMatchObject({ scope: 'everyone', list: 'default/ui-allow', entry: 'graph.facebook.com' });
    expect(req('graph.facebook.com').allow?.expiresAt).toBeUndefined();
    expect(mockDB().lists.find((l) => l.metadata.name === 'ui-allow')?.spec.sources[0]?.inline).toContain('graph.facebook.com');
  });

  it('offers the group scope only when the request names a group', async () => {
    renderWithApp(<RequestsPage />, { route: '/requests' });
    fireEvent.click(await within(await row('www.roblox.com')).findByRole('button', { name: 'Approve' }));
    const dialog = await screen.findByRole('dialog');
    const none = optionsOf(getSelector('Scope', dialog)).find((o) => o.textContent?.includes("The requester's group (the request names none)"));
    expect(none).toHaveAttribute('aria-disabled', 'true');
  });

  it('reports policies managed elsewhere after an approval', async () => {
    renderWithApp(<RequestsPage />, { route: '/requests?state=Denied' });
    fireEvent.click(await within(await row('metrics.roku.com')).findByRole('button', { name: 'Approve' }));
    const dialog = await screen.findByRole('dialog');
    pick(getSelector('Scope', dialog), /^The requester's group/);
    expect(valueOf(getSelector('Scope', dialog))).toBe('group');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Approve' }));
    expect(await screen.findByText(/not attached to iot-strict: managed elsewhere, add the list there\.$/)).toBeInTheDocument();
  });

  it('denies with a note', async () => {
    const bodies = spyBodies('/deny');
    renderWithApp(<RequestsPage />, { route: '/requests' });
    fireEvent.click(await within(await row('www.roblox.com')).findByRole('button', { name: 'Deny' }));
    const dialog = await screen.findByRole('dialog');
    fireEvent.change(within(dialog).getByRole('textbox', { name: /^Note/ }), { target: { value: 'Not on school nights' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Deny' }));
    await waitFor(() => expect(req('www.roblox.com').state).toBe('Denied'));
    expect(bodies).toEqual([{ note: 'Not on school nights' }]);
    expect(await screen.findByText('Denied www.roblox.com')).toBeInTheDocument();
  });

  it('shows no decisions to a role without unblockrequests.decide', async () => {
    renderWithApp(<RequestsPage />, { route: '/requests', user: 'auditor' });
    const tiktok = await row('tiktokv.com');
    expect(within(tiktok).queryByRole('button', { name: 'Approve' })).toBeNull();
    expect(within(tiktok).queryByRole('button', { name: 'Deny' })).toBeNull();
  });
});

describe('RequestDetailPage', () => {
  it('shows the request, its recent queries and the decision history', async () => {
    renderWithApp(<RequestDetailPage params={{ ns: 'default', name: 'metrics-roku-com-c0a80114' }} />, { route: '/requests/default/metrics-roku-com-c0a80114' });
    expect(await screen.findByRole('heading', { level: 1, name: 'metrics.roku.com' })).toBeInTheDocument();
    const decision = screen.getByTestId('request-decision');
    expect(within(decision).getByText('Telemetry stays blocked')).toBeInTheDocument();
    const details = screen.getByTestId('request-details');
    expect(within(details).getByRole('link', { name: 'iot' })).toHaveAttribute('href', '/groups/default/iot');
    // Its policies, and the policy that blocked the name.
    expect(within(details).getAllByRole('link', { name: 'iot-strict' }).map((a) => a.getAttribute('href'))).toEqual(['/policies/default/iot-strict', '/policies/default/iot-strict']);
    expect(within(details).getByText('/^metrics\\./')).toBeInTheDocument();
    expect(screen.getByText(/from the query log of kudurru-0, the node that took the request/)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Open in the query log' })).toHaveAttribute('href', '/queries?domain=metrics.roku.com&client=192.168.1.20');
    const history = screen.getByRole('table', { name: 'Decision history' });
    const rows = within(history).getAllByRole('row').slice(1);
    expect(rows).toHaveLength(2);
    expect(within(rows[0]!).getByText('current')).toBeInTheDocument();
    expect(within(rows[0]!).getByText('Denied')).toBeInTheDocument();
    expect(within(rows[1]!).getByText('Approved')).toBeInTheDocument();
    expect(within(rows[1]!).getByRole('link', { name: 'ui-allow-iot' })).toHaveAttribute('href', '/lists/default/ui-allow-iot');
  });

  it('approves from the header', async () => {
    renderWithApp(<RequestDetailPage params={{ ns: 'default', name: 'tiktokv-com-c0a8011e' }} />, { route: '/requests/default/tiktokv-com-c0a8011e' });
    const approve = await screen.findByRole('button', { name: 'Approve' });
    await waitFor(() => expect(approve).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(approve);
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Approve' }));
    const decision = await screen.findByTestId('request-decision');
    await waitFor(() => expect(within(decision).getByText('permanent')).toBeInTheDocument());
    expect(within(decision).getByRole('link', { name: 'ui-allow' })).toHaveAttribute('href', '/lists/default/ui-allow');
    expect(within(screen.getByTestId('request-decision')).getByText('Approved')).toBeInTheDocument();
  });

  it('deletes after confirmation', async () => {
    renderWithApp(<RequestDetailPage params={{ ns: 'default', name: 'www-roblox-com-7d1e9a40' }} />, { route: '/requests/default/www-roblox-com-7d1e9a40' });
    const del = await screen.findByRole('button', { name: 'Delete' });
    await waitFor(() => expect(del).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(del);
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete request' }));
    await waitFor(() => expect(window.location.pathname).toBe('/requests'));
    expect(mockDB().unblock.some((r) => r.domain === 'www.roblox.com')).toBe(false);
  });

  it('explains a refused delete of a time-limited approval', async () => {
    req('doubleclick.net').allow!.expiresAt = '2099-01-01T00:00:00Z';
    renderWithApp(<RequestDetailPage params={{ ns: 'default', name: 'doubleclick-net-c0a8010a' }} />, { route: '/requests/default/doubleclick-net-c0a8010a' });
    const del = await screen.findByRole('button', { name: 'Delete' });
    await waitFor(() => expect(del).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(del);
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText(/deny it first/)).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole('button', { name: 'Delete request' }));
    expect(await screen.findByText(/^Could not delete the request: .*deny it first/)).toBeInTheDocument();
    expect(mockDB().unblock.some((r) => r.domain === 'doubleclick.net')).toBe(true);
  });
});

describe('pending badge and dashboard notice', () => {
  it('show the pending count', async () => {
    renderWithApp(
      <>
        <UnblockBadge />
        <UnblockNotice />
      </>,
    );
    expect(await screen.findByLabelText('3 unblock requests waiting')).toHaveTextContent('3');
    const notice = screen.getByTestId('requests-notice');
    expect(within(notice).getByText('3 unblock requests waiting')).toBeInTheDocument();
    expect(within(notice).getByRole('link', { name: 'Review requests' })).toHaveAttribute('href', '/requests');
  });

  it('hide on a 403', async () => {
    let asked = 0;
    server.use(
      http.get(`${API}/unblockrequests`, () => {
        asked++;
        return HttpResponse.json({ error: { key: 'forbidden', message: 'You may not read unblockrequests' } }, { status: 403 });
      }),
    );
    renderWithApp(
      <>
        <UnblockBadge />
        <UnblockNotice />
        <span>rendered</span>
      </>,
    );
    await screen.findByText('rendered');
    await waitFor(() => expect(asked).toBeGreaterThan(0));
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByTestId('requests-badge')).toBeNull();
    expect(screen.queryByTestId('requests-notice')).toBeNull();
  });

  it('hide without the read permission', async () => {
    renderWithApp(<UnblockNotice />, { user: 'viewer' });
    await new Promise((r) => setTimeout(r, 100));
    expect(screen.queryByTestId('requests-notice')).toBeNull();
  });

  it('hide when nothing waits', async () => {
    for (const r of mockDB().unblock) if (r.state === 'Pending') r.state = 'Denied';
    let asked = 0;
    server.events.on('request:end', ({ request }) => {
      if (new URL(request.url).pathname === '/api/v1/unblockrequests') asked++;
    });
    renderWithApp(<UnblockBadge />);
    await waitFor(() => expect(asked).toBeGreaterThan(0));
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByTestId('requests-badge')).toBeNull();
  });
});

describe('unblock helpers', () => {
  it('builds the approve body: permanent sends no duration, group only for a group approval', () => {
    const d = { scope: 'everyone' as const, match: 'exact' as const, group: 'default/kids', duration: '', note: '  ' };
    expect(approvalBody(d)).toEqual<UnblockApproval>({ scope: 'everyone', match: 'exact' });
    expect(approvalBody({ ...d, scope: 'group', duration: '24h', note: 'ok' })).toEqual({ scope: 'group', match: 'exact', group: 'default/kids', duration: '24h', note: 'ok' });
    expect(approvalDurationError('')).toBeNull();
    expect(approvalDurationError('36h')).toBeNull();
    expect(approvalDurationError('soon')).toMatch(/duration/);
    expect(approvalDurationError('9000h')).toMatch(/366 days/);
    expect(scopeOptions({ groups: [] })[1]?.disabled).toBe(true);
    expect(blockSource('service:roblox')).toEqual({ kind: 'service', id: 'roblox' });
    expect(blockSource('safesearch:google')).toEqual({ kind: 'safesearch', id: 'google' });
    expect(blockSource('default/ads')).toEqual({ kind: 'list', ref: 'default/ads' });
  });

  it('filters recent queries by the client only when it was recorded', () => {
    expect(recentFilter({ domain: 'a.example', client: '192.0.2.1', clientPrivacy: 'full' })).toMatchObject({ domain: 'a.example', client: '192.0.2.1' });
    expect(recentFilter({ domain: 'a.example', client: '192.0.2.0', clientPrivacy: 'prefix' })).toMatchObject({ client: '192.0.2.0' });
    expect(recentFilter({ domain: 'a.example', clientPrivacy: 'hidden' }).client).toBeUndefined();
  });

  it('words the approval toast with attached and skipped policies', () => {
    const r = { ...mockDB().unblock[3]! };
    expect(approvalMessage({ request: r, attached: ['default/default'], skipped: ['default/iot-strict'], flushed: 1, took_ms: 1 })).toMatch(
      /^Allowed doubleclick\.net for everyone until .* \(list ui-allow\)\. ui-allow attached to default\. not attached to iot-strict: managed elsewhere, add the list there\.$/,
    );
  });
});
