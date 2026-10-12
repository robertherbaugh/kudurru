// The Allowlist, Denylist and combined Domains pages (one shared view):
// rows, the optimistic switch with rollback, per-type validation of the add
// dialog, group scope, filters, bulk actions, inline comments, edit and
// delete, read-only rows and the unattached-policy warning.
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { collectEntries } from '../mocks/domains';
import { mockDB } from '../mocks/handlers';
import { findSelector, getSelector, pick } from '../test/controls';
import { server } from '../test/setup';
import { API, renderWithApp, uiEntryValues } from '../test/utils';
import AllowlistPage from './AllowlistPage';
import DenylistPage from './DenylistPage';
import DomainsPage from './DomainsPage';

const entry = (action: 'allow' | 'deny', value: string) => collectEntries(mockDB(), action).find((e) => e.value === value);

async function row(value: string): Promise<HTMLElement> {
  return screen.findByTestId(`entry-${value}`);
}

/** A row's switch once the session has loaded and enabled it. */
async function switchOf(value: string): Promise<HTMLElement> {
  const sw = within(await row(value)).getByRole('switch', { name: `${value} enabled` });
  await waitFor(() => expect(sw).not.toBeDisabled());
  return sw;
}

async function openAdd(name: RegExp | string): Promise<HTMLElement> {
  const btn = await screen.findByRole('button', { name });
  await waitFor(() => expect(btn).not.toHaveAttribute('aria-disabled', 'true'));
  fireEvent.click(btn);
  return screen.findByRole('dialog');
}

describe('AllowlistPage', () => {
  it('shows one row per allow entry with type, scope, comment, origin, list and overrides', async () => {
    renderWithApp(<AllowlistPage />, { route: '/allowlist' });
    expect(await screen.findByRole('heading', { level: 1, name: 'Allowlist' })).toBeInTheDocument();
    const yt = await row('s.youtube.com');
    expect(within(yt).getByText('exact')).toBeInTheDocument();
    expect(within(yt).getByText('Everyone')).toBeInTheDocument();
    expect(within(yt).getByText('YouTube search suggestions')).toBeInTheDocument();
    expect(within(yt).getByText(/by admin/)).toBeInTheDocument();
    expect(within(yt).getByText('by admin · from manual')).toBeInTheDocument();
    expect(within(yt).getByRole('link', { name: 'ui-allow' })).toHaveAttribute('href', '/lists/default/ui-allow');

    // Approved from the block page: links the request; the starter list blocks it.
    const dc = await row('doubleclick.net');
    expect(within(dc).getByRole('link', { name: 'unblock request' })).toHaveAttribute('href', '/requests/default/doubleclick-net-c0a8010a');
    expect(within(dc).getByText(/Overrides block list/)).toBeInTheDocument();
    expect(within(dc).getByRole('link', { name: 'starter' })).toHaveAttribute('href', '/lists/default/starter');

    expect(within(await row('*.cdn.example-video.com')).getByText('wildcard')).toBeInTheDocument();
    expect(within(await row('clickserve.dartsearch.net')).getByText('by admin · from query log')).toBeInTheDocument();
    // Group-scoped: a chip linking to the group.
    expect(within(await row('scratch.mit.edu')).getByRole('link', { name: 'kids' })).toHaveAttribute('href', '/groups/default/kids');
    expect(within(await row('ads.partner.example')).getByRole('switch')).not.toBeChecked();
    // Only allow entries; the lists behind them are named.
    expect(screen.queryByTestId('entry-tracking.example-shop.com')).toBeNull();
    expect(screen.getByTestId('domain-lists')).toHaveTextContent('ui-allow-kids (1 entry, group kids');
  });

  it('moves the switch at once and back when the write fails', async () => {
    let release = () => {};
    server.use(
      http.patch(`${API}/allowlist/:id`, async () => {
        await new Promise<void>((r) => (release = r));
        return HttpResponse.json({ error: { key: 'unavailable', message: 'the store is read-only' } }, { status: 503 });
      }),
    );
    renderWithApp(<AllowlistPage />, { route: '/allowlist' });
    const sw = await switchOf('s.youtube.com');
    expect(sw).toBeChecked();
    fireEvent.click(sw);
    await waitFor(() => expect(within(screen.getByTestId('entry-s.youtube.com')).getByRole('switch')).not.toBeChecked());
    release();
    expect(await screen.findByText('Could not disable s.youtube.com: the store is read-only')).toBeInTheDocument();
    await waitFor(() => expect(within(screen.getByTestId('entry-s.youtube.com')).getByRole('switch')).toBeChecked());
    expect(entry('allow', 's.youtube.com')?.enabled).toBe(true);
  });

  it('switches an entry on and saves it', async () => {
    renderWithApp(<AllowlistPage />, { route: '/allowlist' });
    fireEvent.click(await switchOf('ads.partner.example'));
    await waitFor(() => expect(entry('allow', 'ads.partner.example')?.enabled).toBe(true));
    expect(await screen.findByText('ads.partner.example enabled.')).toBeInTheDocument();
  });

  it('validates the values per type and adds a wildcard', async () => {
    renderWithApp(<AllowlistPage />, { route: '/allowlist' });
    const dialog = await openAdd('Add to the Allowlist');
    const box = within(dialog).getByRole('textbox', { name: /^Domains/ });
    fireEvent.change(box, { target: { value: '*.videos.example' } });
    expect(await within(dialog).findByText(/matches subdomains: use the type Wildcard/)).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: 'Add' })).toBeDisabled();
    pick(getSelector(/^Type/, dialog), 'Wildcard');
    expect(await within(dialog).findByText('Saved as *.videos.example.')).toBeInTheDocument();
    fireEvent.change(within(dialog).getByRole('textbox', { name: /^Comment/ }), { target: { value: 'the video CDN' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Add' }));
    await waitFor(() => expect(entry('allow', '*.videos.example')).toMatchObject({ type: 'wildcard', everyone: true, comment: 'the video CDN', origin: 'manual' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(await row('*.videos.example')).toBeInTheDocument();
  });

  it('refuses RE2-incompatible expressions and accepts several values, one per line', async () => {
    renderWithApp(<AllowlistPage />, { route: '/allowlist' });
    const dialog = await openAdd('Add to the Allowlist');
    pick(getSelector(/^Type/, dialog), 'Regular expression');
    const box = within(dialog).getByRole('textbox', { name: /^Expressions/ });
    fireEvent.change(box, { target: { value: '^(?!ads)\\.example$' } });
    expect(await within(dialog).findByText(/RE2 has no lookahead/)).toBeInTheDocument();
    fireEvent.change(box, { target: { value: '^cdn[0-9]+\\.example$\n/^img\\./' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Add 2' }));
    await waitFor(() => expect(uiEntryValues('ui-allow')).toEqual(expect.arrayContaining(['^cdn[0-9]+\\.example$', '^img\\.'])));
  });

  it('adds an entry for some client groups only', async () => {
    renderWithApp(<AllowlistPage />, { route: '/allowlist' });
    const dialog = await openAdd('Add to the Allowlist');
    fireEvent.change(within(dialog).getByRole('textbox', { name: /^Domains/ }), { target: { value: 'homework.example' } });
    pick(getSelector(/^Applies to/, dialog), 'Some client groups');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Add' }));
    expect(await within(dialog).findByText('Choose at least one group')).toBeInTheDocument();
    fireEvent.click(await within(dialog).findByRole('checkbox', { name: /^default\/kids/ }));
    fireEvent.click(within(dialog).getByRole('button', { name: 'Add' }));
    await waitFor(() => expect(entry('allow', 'homework.example')).toMatchObject({ everyone: false, groups: ['default/kids'] }));
    expect(uiEntryValues('ui-allow-kids')).toContain('homework.example');
    expect(uiEntryValues('ui-allow')).not.toContain('homework.example');
  });

  it('enables, disables and deletes the selected entries in one batch', async () => {
    renderWithApp(<AllowlistPage />, { route: '/allowlist' });
    await switchOf('s.youtube.com');
    fireEvent.click(within(await row('s.youtube.com')).getByRole('checkbox', { name: 'Select s.youtube.com' }));
    fireEvent.click(within(await row('clickserve.dartsearch.net')).getByRole('checkbox', { name: 'Select clickserve.dartsearch.net' }));
    const bar = screen.getByRole('toolbar', { name: 'Bulk actions' });
    expect(within(bar).getByText('2 selected')).toBeInTheDocument();
    fireEvent.click(within(bar).getByRole('button', { name: 'Disable' }));
    await waitFor(() => expect(entry('allow', 's.youtube.com')?.enabled).toBe(false));
    expect(entry('allow', 'clickserve.dartsearch.net')?.enabled).toBe(false);
    expect(await screen.findByText('Disabled 2 entries.')).toBeInTheDocument();

    fireEvent.click(within(await row('s.youtube.com')).getByRole('checkbox', { name: 'Select s.youtube.com' }));
    fireEvent.click(within(await row('clickserve.dartsearch.net')).getByRole('checkbox', { name: 'Select clickserve.dartsearch.net' }));
    fireEvent.click(within(screen.getByRole('toolbar', { name: 'Bulk actions' })).getByRole('button', { name: 'Delete' }));
    const confirm = await screen.findByRole('dialog', { name: 'Delete 2 entries?' });
    fireEvent.click(within(confirm).getByRole('button', { name: 'Delete' }));
    await waitFor(() => expect(uiEntryValues('ui-allow')).not.toContain('s.youtube.com'));
    expect(uiEntryValues('ui-allow')).not.toContain('clickserve.dartsearch.net');
    await waitFor(() => expect(screen.queryByTestId('entry-s.youtube.com')).toBeNull());
  });

  it('edits the comment in place', async () => {
    renderWithApp(<AllowlistPage />, { route: '/allowlist' });
    await switchOf('clickserve.dartsearch.net');
    fireEvent.click(within(await row('clickserve.dartsearch.net')).getByRole('button', { name: 'Add comment' }));
    const input = within(screen.getByTestId('entry-clickserve.dartsearch.net')).getByRole('textbox', { name: 'Comment for clickserve.dartsearch.net' });
    fireEvent.change(input, { target: { value: 'search ads click-through' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    await waitFor(() => expect(entry('allow', 'clickserve.dartsearch.net')?.comment).toBe('search ads click-through'));
    expect(await within(screen.getByTestId('entry-clickserve.dartsearch.net')).findByText('search ads click-through')).toBeInTheDocument();
  });

  it('edits value, scope and state in a dialog and deletes one entry', async () => {
    renderWithApp(<AllowlistPage />, { route: '/allowlist' });
    await switchOf('s.youtube.com');
    fireEvent.click(within(await row('s.youtube.com')).getByRole('button', { name: 'Edit' }));
    const dialog = await screen.findByRole('dialog', { name: /Edit s\.youtube\.com/ });
    expect(within(dialog).getByRole('button', { name: 'Save' })).toBeDisabled();
    pick(getSelector(/^Type/, dialog), 'Wildcard');
    fireEvent.change(within(dialog).getByRole('textbox', { name: /^Domain/ }), { target: { value: 'youtube.com' } });
    pick(getSelector(/^Applies to/, dialog), 'Some client groups');
    fireEvent.click(await within(dialog).findByRole('checkbox', { name: /^default\/kids/ }));
    fireEvent.click(within(dialog).getByRole('switch', { name: /^Enabled/ }));
    fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(entry('allow', '*.youtube.com')).toMatchObject({ type: 'wildcard', everyone: false, groups: ['default/kids'], enabled: false, comment: 'YouTube search suggestions' }));
    expect(entry('allow', 's.youtube.com')).toBeUndefined();

    fireEvent.click(within(await row('doubleclick.net')).getByRole('button', { name: 'Delete' }));
    fireEvent.click(within(await screen.findByRole('dialog', { name: 'Delete doubleclick.net?' })).getByRole('button', { name: 'Delete' }));
    await waitFor(() => expect(entry('allow', 'doubleclick.net')).toBeUndefined());
  });

  it('keeps every control disabled for a viewer, naming the permission', async () => {
    renderWithApp(<AllowlistPage />, { route: '/allowlist', user: 'viewer' });
    // Disabled with the reason as a tooltip (aria-disabled keeps it focusable).
    const sw = within(await row('s.youtube.com')).getByRole('switch');
    expect(sw).toHaveAttribute('aria-disabled', 'true');
    expect(sw).toHaveAccessibleDescription(/Needs blocklists\.write/);
    expect(screen.getByRole('button', { name: 'Add to the Allowlist' })).toHaveAttribute('aria-disabled', 'true');
    expect(within(await row('s.youtube.com')).getByRole('button', { name: 'Edit' })).toHaveAttribute('aria-disabled', 'true');
    expect(within(await row('s.youtube.com')).queryByRole('button', { name: 'Add comment' })).toBeNull();
  });

  it('shows read-only rows with the reason', async () => {
    const l = mockDB().lists.find((x) => x.metadata.name === 'ui-allow-kids');
    if (l?.metadata.annotations) l.metadata.annotations['kudurru.io/managed-by'] = 'file:objects.d/allow.yaml';
    renderWithApp(<AllowlistPage />, { route: '/allowlist' });
    const r = await row('scratch.mit.edu');
    expect(within(r).getByText(/Read-only: managed by objects\.d\/allow\.yaml/)).toBeInTheDocument();
    expect(within(r).getByRole('switch')).toHaveAttribute('aria-disabled', 'true');
    expect(within(r).getByRole('button', { name: 'Delete' })).toHaveAttribute('aria-disabled', 'true');
  });

  it('checks whether a name is allowed for a client', async () => {
    renderWithApp(<AllowlistPage />, { route: '/allowlist' });
    const panel = await screen.findByRole('region', { name: 'Is it allowed?' });
    fireEvent.change(within(panel).getByRole('textbox', { name: /^Name/ }), { target: { value: 'ads.google.com' } });
    fireEvent.change(within(panel).getByRole('textbox', { name: /^For client/ }), { target: { value: 'not-an-ip' } });
    fireEvent.click(within(panel).getByRole('button', { name: 'Check' }));
    expect(await within(panel).findByText(/IP address/)).toBeInTheDocument();
    fireEvent.change(within(panel).getByRole('textbox', { name: /^For client/ }), { target: { value: '192.168.1.30' } });
    fireEvent.click(within(panel).getByRole('button', { name: 'Check' }));
    const result = await within(panel).findByTestId('domain-check-result');
    expect(result).toHaveTextContent('ads.google.com');
    expect(result).toHaveTextContent(/blocked|allowed/);
  });
});

describe('DenylistPage', () => {
  it('lists deny entries and warns about a policy managed elsewhere', async () => {
    renderWithApp(<DenylistPage />, { route: '/denylist' });
    expect(await row('tracking.example-shop.com')).toBeInTheDocument();
    expect(within(await row('^ads?[0-9]*\\.')).getByText('regex')).toBeInTheDocument();
    expect(within(await row('telemetry.tv.example')).getByText('by system · from migrated')).toBeInTheDocument();
    expect(screen.queryByTestId('entry-s.youtube.com')).toBeNull();
    expect(
      await screen.findByText('Policy iot-strict is managed by file:objects.d/policies.yaml: add ui-deny to its blockListRefs so these entries apply to its clients.'),
    ).toBeInTheDocument();
  });

  it('adds through POST /denylist', async () => {
    renderWithApp(<DenylistPage />, { route: '/denylist' });
    const dialog = await openAdd('Add to the Denylist');
    fireEvent.change(within(dialog).getByRole('textbox', { name: /^Domains/ }), { target: { value: 'games.example.com, chat.example.com' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Add 2' }));
    await waitFor(() => expect(uiEntryValues('ui-deny')).toEqual(expect.arrayContaining(['games.example.com', 'chat.example.com'])));
    // iot-strict is file-managed: the write says the list could not be attached there.
    expect(await screen.findByText(/^Added 2 to the Denylist\. Not applied for iot-strict: managed elsewhere, add the list to its blockListRefs there\.$/)).toBeInTheDocument();
  });
});

describe('DomainsPage', () => {
  it('shows allow and deny entries together and links the focused pages', async () => {
    renderWithApp(<DomainsPage />, { route: '/domains' });
    const allow = await row('s.youtube.com');
    expect(within(allow).getByText('allow')).toBeInTheDocument();
    expect(within(await row('tracking.example-shop.com')).getByText('deny')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Allowlist only' })).toHaveAttribute('href', '/allowlist');
    expect(screen.getByRole('link', { name: 'Denylist only' })).toHaveAttribute('href', '/denylist');
    // Only the dashboard's entries, not the big lists.
    expect(screen.queryByTestId('entry-tiktokv.com')).toBeNull();
  });

  it('filters by action, type, state, scope and text through the URL', async () => {
    renderWithApp(<DomainsPage />, { route: '/domains' });
    await row('s.youtube.com');
    pick(await findSelector(/^Type/), 'Regular expression');
    await waitFor(() => expect(screen.queryByTestId('entry-s.youtube.com')).toBeNull());
    expect(await row('^ads?[0-9]*\\.')).toBeInTheDocument();
    expect(window.location.search).toBe('?type=regex');

    fireEvent.click(screen.getByRole('button', { name: 'Clear filters' }));
    pick(getSelector(/^Enabled/), 'Disabled only');
    expect(await row('ads.partner.example')).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByTestId('entry-s.youtube.com')).toBeNull());

    fireEvent.click(screen.getByRole('button', { name: 'Clear filters' }));
    pick(await findSelector(/^Applies to/), 'kids');
    expect(await row('scratch.mit.edu')).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByTestId('entry-s.youtube.com')).toBeNull());
    expect(window.location.search).toBe('?scope=default%2Fkids');

    fireEvent.click(screen.getByRole('button', { name: 'Clear filters' }));
    pick(getSelector(/^Action/), 'Deny');
    await waitFor(() => expect(screen.queryByTestId('entry-s.youtube.com')).toBeNull());
    fireEvent.change(screen.getByRole('textbox', { name: 'Search' }), { target: { value: 'example-shop' } });
    await waitFor(() => expect(window.location.search).toBe('?action=deny&q=example-shop'));
    await waitFor(() => expect(screen.queryByTestId('entry-telemetry.tv.example')).toBeNull());
    expect(screen.getByTestId('entry-tracking.example-shop.com')).toBeInTheDocument();
  });

  it('pages with continue tokens', async () => {
    const values = Array.from({ length: 55 }, (_, i) => `bulk${String(i).padStart(2, '0')}.example`);
    const l = mockDB().lists.find((x) => x.metadata.name === 'ui-deny');
    l?.spec.sources[0]?.entries?.push(...values.map((value) => ({ value, type: 'exact' as const, created: '2026-01-01T00:00:00Z' })));
    renderWithApp(<DomainsPage />, { route: '/domains?action=deny' });
    expect(await screen.findByText('Showing 1–50 of 58 matching (58 in total)')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Next page' }));
    expect(await screen.findByText('Showing 51–58 of 58 matching (58 in total)')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Previous page' }));
    expect(await screen.findByText('Showing 1–50 of 58 matching (58 in total)')).toBeInTheDocument();
  });

  it('batches a mixed selection per action', async () => {
    renderWithApp(<DomainsPage />, { route: '/domains' });
    await switchOf('s.youtube.com');
    fireEvent.click(within(await row('s.youtube.com')).getByRole('checkbox', { name: 'Select s.youtube.com' }));
    fireEvent.click(within(await row('tracking.example-shop.com')).getByRole('checkbox', { name: 'Select tracking.example-shop.com' }));
    fireEvent.click(within(screen.getByRole('toolbar', { name: 'Bulk actions' })).getByRole('button', { name: 'Disable' }));
    await waitFor(() => expect(entry('deny', 'tracking.example-shop.com')?.enabled).toBe(false));
    expect(entry('allow', 's.youtube.com')?.enabled).toBe(false);
  });
});
