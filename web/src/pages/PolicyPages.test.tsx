import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { mockDB } from '../mocks/handlers';
import { server } from '../test/setup';
import { findSelector, getSelector, pick } from '../test/controls';
import { API, renderWithApp } from '../test/utils';
import PoliciesPage from './PoliciesPage';
import PolicyDetailPage from './PolicyDetailPage';
import { PolicyWizardDialog } from '../components/wizards/PolicyWizard';

/** Clicks a button once the session has loaded and enabled it. */
async function clickWhenEnabled(name: string | RegExp, root: HTMLElement = document.body): Promise<void> {
  const btn = await within(root).findByRole('button', { name });
  await waitFor(() => expect(btn).not.toHaveAttribute('aria-disabled', 'true'));
  fireEvent.click(btn);
}

/** Next, then waits for the step's heading. */
async function goTo(step: string): Promise<void> {
  fireEvent.click(screen.getByRole('button', { name: 'Next' }));
  await screen.findByRole('heading', { level: 3, name: step });
}

const policy = (name: string) => mockDB().policies.find((p) => p.metadata.name === name);

describe('PoliciesPage', () => {
  const stateOf = (name: string) => screen.getByTestId(`row-${name}`).querySelector('[data-state]')?.getAttribute('data-state');

  it('lists policies by priority with state badges, links, lists and service logos', async () => {
    renderWithApp(<PoliciesPage />, { route: '/policies' });
    const table = await screen.findByRole('table', { name: 'Policies' });
    const names = within(table).getAllByRole('row').slice(1).map((r) => r.getAttribute('data-testid'));
    expect(names).toEqual(['row-streaming-off', 'row-kids-bedtime', 'row-iot-strict', 'row-social-media', 'row-default']);
    await waitFor(() => expect(stateOf('streaming-off')).toBe('disabled'));
    expect(within(screen.getByTestId('row-streaming-off')).getByText('Disabled')).toBeInTheDocument();
    expect(stateOf('default')).toBe('shadowed');
    expect(stateOf('social-media')).toBe('active');
    const social = screen.getByTestId('row-social-media');
    expect(within(social).getByRole('link', { name: 'social-media' })).toHaveAttribute('href', '/policies/default/social-media');
    expect(within(social).getByRole('link', { name: 'default' })).toHaveAttribute('href', '/groups/default/default');
    expect(await within(social).findByTestId('icon-tiktok')).toBeInTheDocument();
    expect(within(social).getByTestId('monogram-9gag')).toHaveTextContent('9G');
    expect(within(screen.getByTestId('row-streaming-off')).getByText('+1')).toBeInTheDocument();
    expect(within(screen.getByTestId('row-iot-strict')).getByLabelText('Managed by file:objects.d/policies.yaml')).toBeInTheDocument();
    expect(within(screen.getByTestId('row-kids-bedtime')).getByRole('link', { name: 'starter' })).toHaveAttribute('href', '/lists/default/starter');
    expect(table.textContent).not.toMatch(/undefined|NaN/);
  });

  it('searches and opens the wizard from "New policy"', async () => {
    renderWithApp(<PoliciesPage />, { route: '/policies?q=kids' });
    await screen.findByTestId('row-kids-bedtime');
    expect(screen.queryByTestId('row-default')).toBeNull();
    await clickWhenEnabled('New policy');
    expect(window.location.search).toBe('?q=kids&new=1');
    const dialog = await screen.findByRole('dialog', { name: 'New policy' });
    expect(await within(dialog).findByRole('heading', { level: 3, name: 'Who' })).toBeInTheDocument();
  });
});

describe('PolicyDetailPage', () => {
  it('shows the current state and the last 24 hours from GET …/state', async () => {
    renderWithApp(<PolicyDetailPage params={{ ns: 'default', name: 'default' }} />, { route: '/policies/default/default' });
    const state = await screen.findByTestId('policy-state');
    expect(await within(state).findByText('Shadowed')).toBeInTheDocument();
    expect(within(state).getByRole('link', { name: 'social-media' })).toHaveAttribute('href', '/policies/default/social-media');
    expect(within(state).getByText('Ready')).toBeInTheDocument();
    const activity = screen.getByTestId('section-activity');
    expect(within(activity).getByRole('link', { name: 'Open in the query log' })).toHaveAttribute('href', '/queries?policy=default%2Fdefault');
    const rows = mockDB().queries.filter((q) => q.policy === 'default/default');
    expect(within(activity).getByText((rows.length * 20).toLocaleString())).toBeInTheDocument();
    const lists = within(screen.getByTestId('section-what')).getByRole('table', { name: 'Lists of the policy' });
    expect(within(lists).getByRole('link', { name: 'ui-allow' })).toHaveAttribute('href', '/lists/default/ui-allow');
    expect(document.body.textContent).not.toMatch(/undefined|NaN|\[object Object\]/);
  });

  it('edits a section with its own Save and keeps the rest of the spec', async () => {
    renderWithApp(<PolicyDetailPage params={{ ns: 'default', name: 'social-media' }} />, { route: '/policies/default/social-media' });
    await clickWhenEnabled('Edit Who');
    const who = screen.getByTestId('section-who');
    fireEvent.click(await within(who).findByRole('checkbox', { name: /^kids·/ }));
    fireEvent.change(within(who).getByRole('spinbutton', { name: /Priority/ }), { target: { value: '7' } });
    fireEvent.click(within(who).getByRole('button', { name: 'Save Who' }));
    await waitFor(() => expect(policy('social-media')?.spec.clientGroupRefs).toEqual(['default', 'kids']));
    expect(policy('social-media')?.spec).toMatchObject({ priority: 7, blockedServices: ['tiktok', 'facebook', 'instagram', '9gag'], safeSearch: true });

    await clickWhenEnabled('Edit How');
    const how = screen.getByTestId('section-how');
    fireEvent.change(within(how).getByRole('combobox', { name: 'Block response' }), { target: { value: 'blockPage' } });
    expect(within(how).getByText('Answer with the block page address (Settings → Block page)')).toBeInTheDocument();
    fireEvent.change(within(how).getByRole('combobox', { name: 'Block TTL' }), { target: { value: '5m' } });
    fireEvent.click(within(how).getByRole('button', { name: 'Save How' }));
    await waitFor(() => expect(policy('social-media')?.spec).toMatchObject({ blockMode: 'blockPage', blockTTL: '5m' }));
  });

  it('blocks a service with a logo toggle and Save', async () => {
    renderWithApp(<PolicyDetailPage params={{ ns: 'default', name: 'social-media' }} />, { route: '/policies/default/social-media' });
    const sec = await screen.findByTestId('section-services');
    const netflix = await within(sec).findByRole('button', { name: /Netflix/ });
    await waitFor(() => expect(netflix).not.toBeDisabled());
    fireEvent.click(netflix);
    fireEvent.click(within(sec).getByRole('button', { name: /TikTok/ }));
    expect(within(sec).getByText('4 selected (was 4)')).toBeInTheDocument();
    fireEvent.click(within(sec).getByRole('button', { name: 'Save services' }));
    await waitFor(() => expect(policy('social-media')?.spec.blockedServices).toEqual(['facebook', 'instagram', '9gag', 'netflix']));
  });

  it('disables, shows YAML and deletes after confirmation', async () => {
    renderWithApp(<PolicyDetailPage params={{ ns: 'default', name: 'social-media' }} />, { route: '/policies/default/social-media' });
    await clickWhenEnabled('Disable');
    await waitFor(() => expect(policy('social-media')?.metadata.annotations?.['kudurru.io/enabled']).toBe('false'));
    expect(await screen.findByRole('button', { name: 'Enable' })).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'View as YAML' }));
    const yaml = await screen.findByLabelText('YAML');
    expect(yaml.textContent).toContain('kind: Policy');
    expect(yaml.textContent).toContain('- tiktok');
    expect(yaml.textContent).not.toContain('resourceVersion');
    fireEvent.click(screen.getAllByRole('button', { name: 'Close' }).at(-1)!);

    await clickWhenEnabled('Delete');
    fireEvent.click(await screen.findByRole('button', { name: 'Delete policy' }));
    await waitFor(() => expect(policy('social-media')).toBeUndefined());
  });

  it('opens a file-managed policy read-only with the reason', async () => {
    renderWithApp(<PolicyDetailPage params={{ ns: 'default', name: 'iot-strict' }} />, { route: '/policies/default/iot-strict' });
    expect(await screen.findByText('Read-only: managed by file:objects.d/policies.yaml')).toBeInTheDocument();
    const edit = screen.getByRole('button', { name: 'Edit Who' });
    expect(edit).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByRole('button', { name: 'Delete' })).toHaveAttribute('aria-disabled', 'true');
    const sec = screen.getByTestId('section-services');
    expect(await within(sec).findByRole('button', { name: /YouTube/ })).toBeDisabled();
  });

  it('derives the state from the objects when the server has no state route', async () => {
    server.use(http.get(`${API}/policies/:ns/:name/state`, () => HttpResponse.json({ error: { key: 'not_found', message: 'no route' } }, { status: 404 })));
    renderWithApp(<PolicyDetailPage params={{ ns: 'default', name: 'default' }} />, { route: '/policies/default/default' });
    expect(await screen.findByText(/derived from the objects/)).toBeInTheDocument();
    expect(within(screen.getByTestId('policy-state')).getByText('Shadowed')).toBeInTheDocument();
    expect(screen.getByText(/does not attribute queries to policies yet/)).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'Open in the query log' })).toBeNull();
  });

  it('says so when the server has no query log (no stats24h)', async () => {
    server.use(
      http.get(`${API}/policies/:ns/:name/state`, () =>
        HttpResponse.json({
          policy: 'default/default', enabled: true, active: true, priority: 0, groups: [{ ref: 'default', exists: true, enabled: true, clients: 2 }],
          lists: [{ ref: 'ui-deny', role: 'block', mode: 'block', exists: true, enabled: false, ready: false, entries: 1, sourcesEnabled: 1, sourcesTotal: 1 }],
          services: [], safeSearch: { enabled: false, mode: 'strict', engines: [] }, blockMode: 'zeroIP', conditions: [], took_ms: 1,
        }),
      ),
    );
    renderWithApp(<PolicyDetailPage params={{ ns: 'default', name: 'default' }} />, { route: '/policies/default/default' });
    expect(await screen.findByText(/No query log on this server/)).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'Open in the query log' })).toBeNull();
    const lists = await within(screen.getByTestId('section-what')).findByRole('table', { name: 'Lists of the policy' });
    expect(within(lists).getByText('disabled')).toBeInTheDocument();
    expect(within(lists).getByRole('link', { name: 'ui-deny' })).toHaveAttribute('href', '/lists/default/ui-deny');
    expect(within(screen.getByTestId('section-how')).getByText('0.0.0.0 / ::')).toBeInTheDocument();
  });

  it('names a missing policy', async () => {
    renderWithApp(<PolicyDetailPage params={{ ns: 'default', name: 'nope' }} />, { route: '/policies/default/nope' });
    expect(await screen.findByText(/Policy nope not found/)).toBeInTheDocument();
  });
});

describe('PolicyWizardDialog', () => {
  it('walks Who → What → Safe search → How → When → Review and creates the policy', async () => {
    renderWithApp(<PolicyWizardDialog isOpen onClose={() => {}} />, { route: '/policies?new=1' });
    const next = () => fireEvent.click(screen.getByRole('button', { name: 'Next' }));

    // Who: the name and at least one group are required.
    await screen.findByRole('checkbox', { name: 'kids' });
    next();
    expect(await screen.findByText(/^Name: Required/)).toBeInTheDocument();
    fireEvent.change(screen.getByRole('textbox', { name: /^Name/ }), { target: { value: 'default' } });
    next();
    expect(await screen.findByText('A policy named default already exists.')).toBeInTheDocument();
    fireEvent.change(screen.getByRole('textbox', { name: /^Name/ }), { target: { value: 'kids-games' } });
    next();
    expect(await screen.findByText(/Pick at least one client group/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('checkbox', { name: 'kids' }));
    expect(screen.getByTestId('priority-hint')).toHaveTextContent('streaming-off (30), kids-bedtime (20)');
    await goTo('What');

    // What: lists and services.
    fireEvent.click(await screen.findByRole('checkbox', { name: 'starter' }));
    fireEvent.click(await screen.findByRole('button', { name: /Roblox/ }));
    fireEvent.click(screen.getByRole('button', { name: /Fortnite/ }));
    await goTo('Safe search');

    // Safe search.
    fireEvent.click(await screen.findByRole('switch', { name: /Enforce safe search/ }));
    pick(getSelector('Level'), /^Moderate/);
    await goTo('How');

    // How.
    pick(await findSelector('Block response'), /^NXDOMAIN/);
    await goTo('When');

    // When: a window.
    fireEvent.click(await screen.findByRole('radio', { name: 'Only during a time window' }));
    pick(getSelector('Time zone'), 'Europe/Berlin');
    await goTo('Review');

    // Review, with a way back to a step.
    const review = await screen.findByTestId('policy-review');
    expect(review).toHaveTextContent('kids-games');
    expect(review).toHaveTextContent('Roblox');
    expect(review).toHaveTextContent('On (moderate)');
    expect(review).toHaveTextContent('NXDOMAIN');
    expect(review).toHaveTextContent('21:00–07:00 (Europe/Berlin)');
    fireEvent.click(within(review).getByRole('button', { name: 'Edit blocked services' }));
    fireEvent.click(await screen.findByRole('button', { name: /YouTube/ }));
    for (const step of ['Safe search', 'How', 'When', 'Review']) await goTo(step);

    fireEvent.click(screen.getByRole('button', { name: 'Create policy' }));
    await waitFor(() => expect(policy('kids-games')).toBeDefined());
    expect(policy('kids-games')?.spec).toEqual({
      clientGroupRefs: ['kids'],
      blockListRefs: ['starter'],
      blockedServices: ['roblox', 'fortnite', 'youtube'],
      priority: 100,
      safeSearch: true,
      safeSearchMode: 'moderate',
      blockMode: 'nxdomain',
      schedule: { weekdays: ['Sun', 'Mon', 'Tue', 'Wed', 'Thu'], start: '21:00', end: '07:00', tz: 'Europe/Berlin' },
    });
    await waitFor(() => expect(window.location.pathname).toBe('/policies/default/kids-games'));
  });

  it('shows the server error on the last step', async () => {
    server.use(http.put(`${API}/objects/policies/:ns/:name`, () => HttpResponse.json({ error: { key: 'invalid', message: 'spec.blockedServices: too many' } }, { status: 400 })));
    renderWithApp(<PolicyWizardDialog isOpen onClose={() => {}} />, { route: '/policies?new=1' });
    fireEvent.change(await screen.findByRole('textbox', { name: /^Name/ }), { target: { value: 'x' } });
    fireEvent.click(await screen.findByRole('checkbox', { name: 'kids' }));
    for (const step of ['What', 'Safe search', 'How', 'When', 'Review']) await goTo(step);
    fireEvent.click(screen.getByRole('button', { name: 'Create policy' }));
    expect(await screen.findByText('spec.blockedServices: too many')).toBeInTheDocument();
  });
});
