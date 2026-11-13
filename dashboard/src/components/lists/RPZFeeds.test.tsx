// RPZ feeds in the dashboard (D391): the list page shows each feed's state on
// the answering node from GET /blocklists/{ns}/{name}/rpz, says when clients
// get SERVFAIL, and renders only the fields the node sends; the Lists page
// shows the worst feed state; the source dialog and the wizard create RPZ
// sources; static mode and an enrolled follower keep the gating every other
// list action has.
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import type { BlockList, ClusterInfo, RPZFeed } from '../../api/types';
import { mockDB } from '../../mocks/handlers';
import { getSelector, pick } from '../../test/controls';
import { server } from '../../test/setup';
import { API, renderWithApp } from '../../test/utils';
import { feedStateView, rpzDraftError, worstFeed } from '../../lib/rpz';
import { ApiError } from '../../api/client';
import { RPZ_POLL_MS, RPZ_SETTLING_POLL_MS, rpzSettling } from './RPZFeeds';
import ListDetailPage from '../../pages/ListDetailPage';
import ListsPage from '../../pages/ListsPage';
import { ListWizardDialog } from '../wizards/ListWizard';

const ISO = (msFromNow: number) => new Date(Date.now() + msFromNow).toISOString();

function threats(sources: BlockList['spec']['sources'] = [{ name: 'vendor', format: 'rpz', rpz: { zoneName: 'rpz.vendor.example.', primaries: ['192.0.2.53', '[2001:db8::53]:5353'], tsigKeyRef: 'vendor-xfr.' } }]): BlockList {
  return {
    apiVersion: 'kudurru.io/v1alpha1',
    kind: 'BlockList',
    metadata: { name: 'threats', namespace: 'default', resourceVersion: '1' },
    spec: { mode: 'block', sources },
    status: { sources: [{ name: 'vendor' }] },
  };
}

/** A serving feed with every field the API defines. */
function serving(over: Partial<RPZFeed> = {}): RPZFeed {
  return {
    node: 'kudurru-0', namespace: 'default', list: 'threats', source: 'vendor', origin: 'rpz.vendor.example.', state: 'serving', available: true,
    serial: 2026101107, primary: '192.0.2.53:53', lastConfirmedAt: ISO(-60_000), expiresAt: ISO(110 * 60_000), nextAttemptAt: ISO(5 * 60_000),
    counts: { records: 1210, rules: 1197, invalid: 1, unsupported: 3, excluded: 2, ignoredRecords: 7, staleExcludes: 0 },
    rules: { qname: 1100, clientIP: 7, responseIP: 90, nxdomain: 1000, nodata: 50, passthru: 20, drop: 5, tcpOnly: 2, localData: 120 },
    ignored: [
      { reason: 'resolver-ns-provenance', unsupported: true, count: 3, examples: ['ns.bad.rpz-nsdname.rpz.vendor.example.', '32.1.2.0.192.rpz-nsip.rpz.vendor.example.'] },
      { reason: 'invalid-address-trigger', unsupported: false, count: 1, examples: ['999.rpz-ip.rpz.vendor.example.'] },
    ],
    ...over,
  };
}

function seed(feeds: RPZFeed[] | false, list: BlockList = threats()) {
  const db = mockDB();
  db.lists.push(list);
  db.rpzFeeds = feeds;
}

function renderThreats() {
  return renderWithApp(<ListDetailPage params={{ ns: 'default', name: 'threats' }} />, { route: '/lists/default/threats' });
}

async function feedSection(source = 'vendor'): Promise<HTMLElement> {
  return screen.findByTestId(`rpz-feed-${source}`);
}

describe('RPZ feeds on the list page', () => {
  it('shows a serving feed with serial, copy, rules by trigger and action and every ignored reason', async () => {
    seed([serving()]);
    renderThreats();
    const feed = await feedSection();
    expect(feed).toHaveAttribute('data-state', 'serving');
    expect(within(feed).getByText('serving')).toBeInTheDocument();
    expect(within(feed).getByText('rpz.vendor.example.')).toBeInTheDocument();
    expect(within(feed).getByText('2026101107')).toBeInTheDocument();
    expect(within(feed).getByText('192.0.2.53:53')).toBeInTheDocument();
    expect(within(feed).getByText('1,197 of 1,210 records')).toBeInTheDocument();
    expect(within(feed).getByText('3 unsupported, 1 invalid')).toBeInTheDocument();
    expect(within(feed).getByText('2 switched off')).toBeInTheDocument();
    const rules = within(feed).getByTestId('rpz-rules');
    for (const [label, n] of [['QNAME', '1,100'], ['CLIENT-IP', '7'], ['RESPONSE-IP', '90'], ['NXDOMAIN', '1,000'], ['NODATA', '50'], ['PASSTHRU', '20'], ['DROP', '5'], ['TCP-only', '2'], ['Local data', '120']]) {
      expect(within(rules).getByText(label as string).closest('[class]')?.parentElement).toHaveTextContent(n as string);
    }
    const ignored = within(feed).getByRole('table', { name: 'Ignored owners of vendor' });
    expect(within(ignored).getByText('resolver-ns-provenance')).toBeInTheDocument();
    expect(within(ignored).getByText(/NSDNAME or NSIP trigger/)).toBeInTheDocument();
    expect(within(ignored).getByText('unsupported')).toBeInTheDocument();
    expect(within(ignored).getByText('malformed')).toBeInTheDocument();
    expect(within(ignored).getByText('ns.bad.rpz-nsdname.rpz.vendor.example.')).toBeInTheDocument();
    expect(within(ignored).getByText('999.rpz-ip.rpz.vendor.example.')).toBeInTheDocument();
    // Serving: no SERVFAIL warning, and the node is named.
    expect(within(feed).queryByText(/SERVFAIL/)).not.toBeInTheDocument();
    expect(screen.getByRole('region', { name: 'RPZ feeds on this node' })).toHaveTextContent('node kudurru-0');
    // The sources table names the feed and offers no entries browser for it.
    const row = screen.getByTestId('source-vendor');
    expect(within(row).getByText('rpz.vendor.example. from 192.0.2.53, [2001:db8::53]:5353 (TSIG vendor-xfr.)')).toBeInTheDocument();
    expect(within(row).getByText('RPZ')).toBeInTheDocument();
    expect(within(row).queryByRole('button', { name: 'Entries' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Refresh now' })).not.toBeInTheDocument();
  });

  it('says plainly that a pending feed answers SERVFAIL and names the next attempt', async () => {
    seed([{ node: 'kudurru-0', namespace: 'default', list: 'threats', source: 'vendor', origin: 'rpz.vendor.example.', state: 'pending', available: false, reason: 'TransferPending',
      message: "Waiting for this node's first complete transfer; until then queries whose policy selects this list answer SERVFAIL on this node.", nextAttemptAt: ISO(30_000), ignored: [] }]);
    renderThreats();
    const feed = await feedSection();
    expect(feed).toHaveAttribute('data-state', 'pending');
    const banner = within(feed).getByRole('alert');
    expect(banner).toHaveTextContent('Pending: clients get SERVFAIL');
    expect(banner).toHaveTextContent('Until its first complete transfer, node kudurru-0 answers SERVFAIL to every client whose policy uses this list');
    expect(banner).toHaveTextContent("TransferPending: Waiting for this node's first complete transfer");
    expect(banner).toHaveTextContent(/Next transfer attempt in/);
    expect(within(feed).getAllByText('no copy')).toHaveLength(2);
    expect(within(feed).queryByTestId('rpz-rules')).not.toBeInTheDocument();
  });

  it('keeps the last serial and counts of an expired copy with its last error', async () => {
    seed([serving({ state: 'expired', available: false, reason: 'RPZExpired', expiresAt: ISO(-60_000), lastError: 'dial tcp 192.0.2.53:53: connection refused',
      message: 'The last copy has expired; queries whose policy selects this list answer SERVFAIL on this node. Last refresh error: dial tcp 192.0.2.53:53: connection refused' })]);
    renderThreats();
    const feed = await feedSection();
    expect(within(feed).getByRole('alert')).toHaveTextContent('Expired: clients get SERVFAIL');
    expect(within(feed).getByText('Last serial')).toBeInTheDocument();
    expect(within(feed).getByText('2026101107')).toBeInTheDocument();
    expect(within(feed).getByText('Expired')).toBeInTheDocument();
    expect(within(feed).getByTestId('rpz-last-error')).toHaveTextContent('connection refused');
    expect(within(feed).getByTestId('rpz-rules')).toBeInTheDocument();
  });

  it('renders a discarded copy from what the node sends, without inventing copy fields', async () => {
    // RPZ-OPEN: a copy the manager discarded carries only identity, state and the message.
    seed([{ state: 'unavailable', source: 'vendor', list: 'threats', namespace: 'default', reason: 'RPZTransfer',
      message: 'This node holds no usable copy until the next complete transfer; queries whose policy selects this list answer SERVFAIL on this node.' }]);
    renderThreats();
    const feed = await feedSection();
    expect(within(feed).getByRole('alert')).toHaveTextContent('Unavailable: clients get SERVFAIL');
    expect(within(feed).getByRole('alert')).toHaveTextContent('This node answers SERVFAIL');
    expect(within(feed).getByRole('alert')).toHaveTextContent('RPZTransfer: This node holds no usable copy');
    expect(within(feed).getAllByText('no copy')).toHaveLength(2);
    expect(within(feed).queryByRole('table')).not.toBeInTheDocument();
  });

  it('shows a configuration failure and a serving copy whose refresh failed', async () => {
    seed([
      { node: 'kudurru-0', namespace: 'default', list: 'threats', source: 'vendor', origin: 'rpz.vendor.example.', state: 'unavailable', available: false, reason: 'ConfigurationUnavailable',
        message: 'TSIG key vendor-xfr. is not configured', configurationError: 'TSIG key vendor-xfr. is not configured', ignored: [] },
      serving({ source: 'backup', origin: 'rpz.backup.example.', lastError: 'SOA query timed out', reason: 'RPZTransfer', message: 'The last refresh failed; this node keeps applying the copy until it expires: SOA query timed out', ignored: [] }),
    ], threats([
      { name: 'vendor', format: 'rpz', rpz: { zoneName: 'rpz.vendor.example.', primaries: ['192.0.2.53'], tsigKeyRef: 'vendor-xfr.' } },
      { name: 'backup', format: 'rpz', rpz: { zoneName: 'rpz.backup.example.', primaries: ['192.0.2.54'] } },
    ]));
    renderThreats();
    const vendor = await feedSection('vendor');
    expect(within(vendor).getByRole('alert')).toHaveTextContent('ConfigurationUnavailable: TSIG key vendor-xfr. is not configured');
    const backup = await feedSection('backup');
    expect(within(backup).getByText('serving, refresh failed')).toBeInTheDocument();
    expect(within(backup).getByText('The last refresh failed')).toBeInTheDocument();
    expect(within(backup).getByTestId('rpz-message')).toHaveTextContent('keeps applying the copy until it expires: SOA query timed out');
    expect(within(backup).queryByText(/clients get SERVFAIL/)).not.toBeInTheDocument();
  });

  it('explains a node without a feed manager and a list its configuration lacks', async () => {
    seed(false);
    const first = renderThreats();
    expect(await screen.findByText('This node reports no RPZ feed state')).toBeInTheDocument();
    first.unmount();
    seed([]);
    server.use(http.get(`${API}/blocklists/default/threats/rpz`, () => HttpResponse.json({ error: { key: 'not_found', message: 'blocklist not found' } }, { status: 404 })));
    renderThreats();
    expect(await screen.findByText("This node's configuration does not include the list yet")).toBeInTheDocument();
  });

  it('explains a node that has no configuration yet (503)', async () => {
    seed([]);
    server.use(http.get(`${API}/blocklists/default/threats/rpz`, () => HttpResponse.json({ error: { key: 'unavailable', message: 'no configuration snapshot yet' } }, { status: 503 })));
    renderThreats();
    const region = await screen.findByRole('region', { name: 'RPZ feeds on this node' });
    expect(await within(region).findByText('This node has no configuration yet')).toBeInTheDocument();
    expect(within(region).getByText(/has not loaded its first configuration snapshot/)).toBeInTheDocument();
  });

  it('reads again within seconds while the node lacks the list, and shows the feed once it has it', async () => {
    seed([serving()]);
    let reads = 0;
    server.use(
      http.get(`${API}/blocklists/default/threats/rpz`, () => {
        reads++;
        if (reads === 1) return HttpResponse.json({ error: { key: 'not_found', message: 'blocklist not found' } }, { status: 404 });
        return HttpResponse.json({ items: [serving()] });
      }),
    );
    renderThreats();
    expect(await screen.findByText("This node's configuration does not include the list yet")).toBeInTheDocument();
    // No reload: the settling poll (not the 30 s one) brings the feed in.
    const feed = await screen.findByTestId('rpz-feed-vendor', {}, { timeout: RPZ_SETTLING_POLL_MS + 3_000 });
    expect(feed).toHaveAttribute('data-state', 'serving');
    expect(screen.queryByText("This node's configuration does not include the list yet")).not.toBeInTheDocument();
  });

  it('reads the feed state again after a source change', async () => {
    seed([serving()]);
    let reads = 0;
    server.use(
      http.get(`${API}/blocklists/default/threats/rpz`, () => {
        reads++;
        return HttpResponse.json({ items: [serving()] });
      }),
    );
    renderThreats();
    await feedSection();
    expect(reads).toBe(1);
    fireEvent.click(screen.getByRole('switch', { name: 'Source vendor enabled' }));
    // Serving polls every 30 s, so only the invalidation explains a second read now.
    await waitFor(() => expect(reads).toBeGreaterThan(1), { timeout: 3_000 });
  });

  it('flags a SERVFAIL feed next to the shared health and words fetch times per node', async () => {
    const list = threats();
    list.status = { ...list.status, conditions: [{ type: 'Ready', status: 'True', reason: 'Serving', message: '1 sources' }] };
    seed([serving({ state: 'expired', available: false, reason: 'RPZExpired', expiresAt: ISO(-60_000) })], list);
    renderThreats();
    await feedSection();
    expect(await screen.findByTestId('rpz-header-state')).toHaveTextContent('RPZ expired on this node');
    expect(screen.getByTestId('rpz-status')).toHaveTextContent('RPZ feed vendor is expired on node kudurru-0: clients whose policy uses this list get SERVFAIL from it.');
    expect(screen.getByText('per node (see RPZ feeds)')).toBeInTheDocument();
    expect(screen.queryByText('not yet')).not.toBeInTheDocument();
    expect(screen.getByText("not used: RPZ feeds follow their zone's SOA timers")).toBeInTheDocument();
    expect(screen.queryByText('24h (default)')).not.toBeInTheDocument();
  });

  it('adds no feed flag while every feed serves', async () => {
    seed([serving()]);
    renderThreats();
    await feedSection();
    expect(screen.queryByTestId('rpz-header-state')).not.toBeInTheDocument();
    expect(screen.queryByTestId('rpz-status')).not.toBeInTheDocument();
  });

  it('shows an RPZ source opened in the entries browser as a feed without entries', async () => {
    seed([serving()]);
    renderWithApp(<ListDetailPage params={{ ns: 'default', name: 'threats' }} />, { route: '/lists/default/threats?source=vendor' });
    expect(await screen.findByText('vendor is an RPZ feed')).toBeInTheDocument();
  });

  it('adds an RPZ feed from the source dialog of a block list only', async () => {
    renderWithApp(<ListDetailPage params={{ ns: 'default', name: 'family' }} />, { route: '/lists/default/family' });
    fireEvent.click(await screen.findByRole('button', { name: 'Add source' }));
    fireEvent.click(await screen.findByRole('radio', { name: 'RPZ feed' }));
    const dialog = screen.getByRole('dialog');
    expect(within(dialog).queryByRole('switch', { name: /Include subdomains/ })).not.toBeInTheDocument();
    fireEvent.change(within(dialog).getByRole('textbox', { name: /^Name/ }), { target: { value: 'vendor' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Add source' }));
    expect(await within(dialog).findByText(/Required: the policy zone/)).toBeInTheDocument();
    expect(within(dialog).getByText('At least one primary server is required')).toBeInTheDocument();
    fireEvent.change(within(dialog).getByRole('textbox', { name: /^Policy zone/ }), { target: { value: 'rpz.vendor.example.' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Add primary server' }));
    fireEvent.change(within(dialog).getByRole('textbox', { name: 'Primary servers 1' }), { target: { value: '192.0.2.53' } });
    pick(getSelector(/^Transfer transport/, dialog), 'TLS');
    fireEvent.change(within(dialog).getByRole('textbox', { name: /^TSIG key name/ }), { target: { value: 'vendor-xfr.' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Add source' }));
    const family = () => mockDB().lists.find((l) => l.metadata.name === 'family');
    await waitFor(() => expect(family()?.spec.sources).toHaveLength(5));
    expect(family()?.spec.sources[4]).toEqual({ name: 'vendor', format: 'rpz', rpz: { zoneName: 'rpz.vendor.example.', primaries: ['192.0.2.53'], transferProtocol: 'tls', tsigKeyRef: 'vendor-xfr.' } });
  });

  it('offers no RPZ feed for an allow list', async () => {
    const allow = threats([{ name: 'mine', inline: ['ok.example'] }]);
    allow.metadata = { ...allow.metadata, name: 'trusted' };
    allow.spec = { ...allow.spec, mode: 'allow' };
    mockDB().lists.push(allow);
    renderWithApp(<ListDetailPage params={{ ns: 'default', name: 'trusted' }} />, { route: '/lists/default/trusted' });
    fireEvent.click(await screen.findByRole('button', { name: 'Add source' }));
    expect(await screen.findByRole('radio', { name: 'URL' })).toBeInTheDocument();
    expect(screen.queryByRole('radio', { name: 'RPZ feed' })).not.toBeInTheDocument();
  });

  it('keeps showing the feed state in static mode while every change is refused', async () => {
    seed([serving()]);
    server.use(http.get(`${API}/cluster`, () => HttpResponse.json({ readOnly: true })));
    renderThreats();
    await feedSection();
    await waitFor(() => expect(screen.getByRole('switch', { name: 'Source vendor enabled' })).toBeDisabled());
    expect(screen.queryByRole('button', { name: 'Add source' })).not.toBeInTheDocument();
    expect(within(screen.getByTestId('source-vendor')).queryByRole('button', { name: 'Edit vendor' })).not.toBeInTheDocument();
  });

  it("shows a follower's own feed state and keeps forwarded list edits", async () => {
    seed([serving({ node: 'kudurru-1', serial: 2026101106 })]);
    const info: ClusterInfo = { clusterID: 'c', driver: 'sqlite', epoch: 7, leader: 'kudurru-0', nodes: 2, readOnly: false, node: 'kudurru-1', role: 'follower', primary: 'kudurru-0', primaryRequired: true,
      forwardedOperations: ['putBlocklists', 'putObject'], localOperations: [], took_ms: 1 };
    server.use(http.get(`${API}/cluster`, () => HttpResponse.json(info)));
    renderThreats();
    const feed = await feedSection();
    expect(within(feed).getByText('2026101106')).toBeInTheDocument();
    expect(screen.getByRole('region', { name: 'RPZ feeds on this node' })).toHaveTextContent('node kudurru-1');
    // Source edits are forwarded to the primary, so they stay offered.
    expect(await screen.findByRole('button', { name: 'Add source' })).toBeInTheDocument();
  });
});

describe('RPZ feeds on the Lists page', () => {
  it('shows the worst feed state under the health, as text while serving', async () => {
    seed([serving(), { node: 'kudurru-0', namespace: 'default', list: 'other', source: 'feed', origin: 'rpz.other.example.', state: 'pending', available: false,
      message: "Waiting for this node's first complete transfer; until then queries whose policy selects this list answer SERVFAIL on this node." }]);
    const other = threats([{ name: 'feed', format: 'rpz', rpz: { zoneName: 'rpz.other.example.', primaries: ['192.0.2.9'] } }]);
    other.metadata = { ...other.metadata, name: 'other' };
    mockDB().lists.push(other);
    renderWithApp(<ListsPage />, { route: '/lists' });
    const states = await screen.findAllByTestId('rpz-state');
    expect(states.map((s) => s.textContent).sort()).toEqual(['RPZ pending', 'RPZ serving']);
  });

  it('words the update time of an RPZ-only list per node', async () => {
    seed([serving()]);
    renderWithApp(<ListsPage />, { route: '/lists' });
    const row = (await screen.findByRole('link', { name: 'threats' })).closest('tr') as HTMLElement;
    expect(within(row).getByText('per node')).toBeInTheDocument();
  });

  it('says the feed state is unknown on a node without a feed manager', async () => {
    seed(false);
    renderWithApp(<ListsPage />, { route: '/lists' });
    expect(await screen.findByText('RPZ unknown')).toBeInTheDocument();
  });

  it('does not read feed state without a list that has a feed', async () => {
    let reads = 0;
    server.use(http.get(`${API}/blocklists/_rpz`, () => {
      reads++;
      return HttpResponse.json({ items: [] });
    }));
    renderWithApp(<ListsPage />, { route: '/lists' });
    await screen.findByRole('link', { name: 'family' });
    expect(reads).toBe(0);
  });
});

describe('RPZ feeds in the new-list wizard', () => {
  it('creates a block list with an RPZ feed source', async () => {
    renderWithApp(<ListWizardDialog isOpen onClose={() => {}} />, { route: '/lists?new=1' });
    fireEvent.click(await screen.findByRole('button', { name: 'Next' }));
    fireEvent.click(await screen.findByRole('switch', { name: /Add an RPZ feed/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(await screen.findByText(/RPZ feed: Required: the policy zone/)).toBeInTheDocument();
    fireEvent.change(screen.getByRole('textbox', { name: /^Policy zone/ }), { target: { value: 'rpz.vendor.example.' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add primary server' }));
    fireEvent.change(screen.getByRole('textbox', { name: 'Primary servers 1' }), { target: { value: 'ns1.vendor.example:5353' } });
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(await screen.findByRole('textbox', { name: /^Name/ })).toHaveValue('rpz-vendor-example');
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(await screen.findByText('rpz.vendor.example. from ns1.vendor.example:5353')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Create list' }));
    await waitFor(() => expect(mockDB().lists.find((l) => l.metadata.name === 'rpz-vendor-example')).toBeDefined());
    expect(mockDB().lists.find((l) => l.metadata.name === 'rpz-vendor-example')?.spec.sources).toEqual([
      { name: 'rpz-vendor-example', format: 'rpz', rpz: { zoneName: 'rpz.vendor.example.', primaries: ['ns1.vendor.example:5353'] } },
    ]);
  });
});

describe('RPZ helpers', () => {
  it('words each state and treats an unknown one as not applying', () => {
    expect(feedStateView({ state: 'serving' })).toEqual({ label: 'serving', tone: 'success', servfail: false });
    expect(feedStateView({ state: 'serving', lastError: 'x' }).tone).toBe('warning');
    for (const state of ['pending', 'expired', 'unavailable']) expect(feedStateView({ state }).servfail).toBe(true);
    expect(feedStateView({ state: 'resyncing' })).toEqual({ label: 'resyncing', tone: 'neutral', servfail: true });
    expect(feedStateView({ state: 'resyncing', available: true }).servfail).toBe(false);
    expect(feedStateView({})).toMatchObject({ label: 'unknown', servfail: true });
  });

  it('polls fast only while the read settles', () => {
    expect(rpzSettling(undefined, new ApiError(404, 'not_found', 'x'))).toBe(true);
    expect(rpzSettling(undefined, new ApiError(503, 'unavailable', 'x'))).toBe(true);
    expect(rpzSettling(undefined, new ApiError(501, 'not_implemented', 'x'))).toBe(false);
    expect(rpzSettling({ items: [serving()] }, null)).toBe(false);
    expect(rpzSettling({ items: [serving(), { state: 'pending' }] }, null)).toBe(true);
    expect(RPZ_SETTLING_POLL_MS).toBeLessThan(RPZ_POLL_MS);
  });

  it('picks the feed that most needs attention', () => {
    const s = serving();
    const failing = serving({ source: 'b', lastError: 'x' });
    const pending: RPZFeed = { source: 'c', state: 'pending' };
    const expired: RPZFeed = { source: 'd', state: 'expired' };
    expect(worstFeed([s, failing])?.source).toBe('b');
    expect(worstFeed([s, failing, pending])?.source).toBe('c');
    expect(worstFeed([pending, expired, s])?.source).toBe('d');
    expect(worstFeed([])).toBeUndefined();
  });

  it('checks the transfer settings like the CRD', () => {
    const ok = { zoneName: 'rpz.vendor.example.', primaries: ['192.0.2.53'], transferProtocol: 'tcp' as const, tsigKeyRef: '' };
    expect(rpzDraftError(ok)).toBeNull();
    expect(rpzDraftError({ ...ok, zoneName: '.' })).toMatch(/root/);
    expect(rpzDraftError({ ...ok, zoneName: 'rpz_x.example.' })).toMatch(/Letters/);
    expect(rpzDraftError({ ...ok, primaries: [' '] })).toBe('At least one primary server is required');
    expect(rpzDraftError({ ...ok, primaries: Array.from({ length: 33 }, (_x, i) => `192.0.2.${i + 1}`) })).toBe('At most 32 primary servers');
    expect(rpzDraftError({ ...ok, primaries: ['2001:db8::1:5353x'] })).toMatch(/^Primary 1/);
    expect(rpzDraftError({ ...ok, tsigKeyRef: 'bad key' })).not.toBeNull();
  });
});
