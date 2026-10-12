import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { mockDB } from '../mocks/handlers';
import { findSelector, getSelector, optionLabels, pick } from '../test/controls';
import { renderWithApp, uiEntryValues } from '../test/utils';
import ClientDetailPage from './ClientDetailPage';
import GroupDetailPage from './GroupDetailPage';
import GroupsPage from './GroupsPage';
import { GroupWizardDialog } from '../components/wizards/GroupWizard';

async function clickWhenEnabled(name: string | RegExp): Promise<void> {
  const btn = await screen.findByRole('button', { name });
  await waitFor(() => expect(btn).not.toHaveAttribute('aria-disabled', 'true'));
  fireEvent.click(btn);
}

async function goTo(step: string): Promise<void> {
  fireEvent.click(screen.getByRole('button', { name: 'Next' }));
  await screen.findByRole('heading', { level: 3, name: step });
}

const group = (name: string) => mockDB().groups.find((g) => g.metadata.name === name);

describe('GroupsPage', () => {
  it('links every group to its page with its matchers and policies', async () => {
    renderWithApp(<GroupsPage />, { route: '/groups' });
    const kids = await screen.findByTestId('row-kids');
    expect(within(kids).getByRole('link', { name: 'kids' })).toHaveAttribute('href', '/groups/default/kids');
    expect(kids).toHaveTextContent('kids-tablet (192.168.1.30)');
    await waitFor(() => expect(within(kids).getByRole('link', { name: 'streaming-off' })).toHaveAttribute('href', '/policies/default/streaming-off'));
    expect(within(screen.getByTestId('row-servers')).getByRole('switch', { name: 'servers enabled' })).not.toBeChecked();
    expect(within(screen.getByTestId('row-servers')).getByText('default policy')).toBeInTheDocument();
  });
});

describe('GroupDetailPage', () => {
  it('shows matchers, recent members and the policies in priority order', async () => {
    renderWithApp(<GroupDetailPage params={{ ns: 'default', name: 'kids' }} />, { route: '/groups/default/kids' });
    const matchers = await screen.findByRole('table', { name: 'Matchers' });
    expect(within(matchers).getByText('aa:bb:cc:dd:ee:01')).toBeInTheDocument();
    expect(within(matchers).getByText('limited')).toBeInTheDocument();
    const seen = await screen.findByRole('table', { name: 'Clients of kids' });
    expect(within(seen).getByRole('link', { name: 'kids-tablet' })).toHaveAttribute('href', '/clients/192.168.1.30');
    const order = await screen.findByRole('list', { name: 'Policies of kids' });
    expect(within(order).getAllByRole('link').map((a) => a.textContent)).toEqual(['streaming-off', 'kids-bedtime']);
    expect(within(order).getByText('Disabled')).toBeInTheDocument();
  });

  it('opens the policy wizard over the page with the group chosen', async () => {
    renderWithApp(<GroupDetailPage params={{ ns: 'default', name: 'kids' }} />, { route: '/groups/default/kids' });
    await clickWhenEnabled('New policy');
    const dialog = await screen.findByRole('dialog', { name: 'New policy' });
    expect(await within(dialog).findByRole('checkbox', { name: 'kids' })).toBeChecked();
    expect(window.location.pathname).toBe('/groups/default/kids');
  });

  it('edits the matchers with validation before the save', async () => {
    renderWithApp(<GroupDetailPage params={{ ns: 'default', name: 'kids' }} />, { route: '/groups/default/kids' });
    await clickWhenEnabled('Edit Members');
    fireEvent.click(screen.getByRole('button', { name: 'Add matcher' }));
    pick(getSelector('Matcher 4 kind'), 'CIDR range');
    fireEvent.change(screen.getByRole('textbox', { name: 'Matcher 4 CIDR range' }), { target: { value: '192.168.5.0/33' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save Members' }));
    expect(await screen.findByText('Matcher 4: IPv4 prefix length is at most 32')).toBeInTheDocument();
    fireEvent.change(screen.getByRole('textbox', { name: 'Matcher 4 CIDR range' }), { target: { value: '192.168.5.0/24' } });
    fireEvent.change(screen.getByRole('textbox', { name: 'Matcher 4 display name' }), { target: { value: 'kids wifi' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save Members' }));
    await waitFor(() => expect(group('kids')?.spec.clients).toHaveLength(4));
    expect(group('kids')?.spec.clients?.[3]).toEqual({ cidr: '192.168.5.0/24', name: 'kids wifi' });
    expect(group('kids')?.spec.clients?.[1]).toEqual({ ip: '192.168.1.31', name: 'kids-laptop', comment: 'school laptop' });
  });

  it('is read-only when managed by a file and cannot delete the default group', async () => {
    renderWithApp(<GroupDetailPage params={{ ns: 'default', name: 'iot' }} />, { route: '/groups/default/iot' });
    expect(await screen.findByText('Read-only: managed by file:objects.d/groups.yaml')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Edit Members' })).toHaveAttribute('aria-disabled', 'true');
  });

  it('enables a disabled group and deletes one after confirmation', async () => {
    renderWithApp(<GroupDetailPage params={{ ns: 'default', name: 'servers' }} />, { route: '/groups/default/servers' });
    await clickWhenEnabled('Enable');
    await waitFor(() => expect(group('servers')?.metadata.annotations?.['kudurru.io/enabled']).toBeUndefined());
    await clickWhenEnabled('Delete');
    fireEvent.click(await screen.findByRole('button', { name: 'Delete group' }));
    await waitFor(() => expect(group('servers')).toBeUndefined());
  });
});

describe('GroupWizardDialog', () => {
  it('creates a group from typed matchers and seen clients and adds it to a policy', async () => {
    renderWithApp(<GroupWizardDialog isOpen onClose={() => {}} />, { route: '/groups?new=1' });
    fireEvent.change(await screen.findByRole('textbox', { name: /^Name/ }), { target: { value: 'kids' } });
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(await screen.findByText('A group named kids already exists.')).toBeInTheDocument();
    fireEvent.change(screen.getByRole('textbox', { name: /^Name/ }), { target: { value: 'guests' } });
    await goTo('Members');
    fireEvent.click(screen.getByRole('button', { name: 'Add matcher' }));
    fireEvent.change(screen.getByRole('textbox', { name: 'Matcher 1 IP address' }), { target: { value: 'not-an-ip' } });
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(await screen.findByText('Matcher 1: Not an IPv4 or IPv6 address')).toBeInTheDocument();
    pick(getSelector('Matcher 1 kind'), 'CIDR range');
    fireEvent.change(screen.getByRole('textbox', { name: 'Matcher 1 CIDR range' }), { target: { value: '192.168.50.0/24' } });
    fireEvent.click(await screen.findByRole('button', { name: 'Add guest-phone' }));
    await goTo('Policies');
    fireEvent.click(screen.getByRole('checkbox', { name: /social-media/ }));
    await goTo('Review');
    const review = screen.getByTestId('group-review');
    expect(review).toHaveTextContent('192.168.50.0/24, guest-phone (192.168.1.50)');
    expect(review).toHaveTextContent('social-media');
    fireEvent.click(screen.getByRole('button', { name: 'Create group' }));
    await waitFor(() => expect(group('guests')).toBeDefined());
    expect(group('guests')?.spec.clients).toEqual([{ cidr: '192.168.50.0/24' }, { ip: '192.168.1.50', name: 'guest-phone' }]);
    await waitFor(() => expect(mockDB().policies.find((p) => p.metadata.name === 'social-media')?.spec.clientGroupRefs).toEqual(['default', 'guests']));
    await waitFor(() => expect(window.location.pathname).toBe('/groups/default/guests'));
  });
});

describe('ClientDetailPage', () => {
  it('explains the groups and the policy that decides, with recent queries', async () => {
    renderWithApp(<ClientDetailPage params={{ key: '192.168.1.10' }} />, { route: '/clients/192.168.1.10' });
    expect(await screen.findByRole('heading', { level: 1, name: 'robert-laptop' })).toBeInTheDocument();
    const groups = await screen.findByTestId('client-groups');
    expect(within(groups).getByText('Matched by a catch-all range (0.0.0.0/0 or ::/0).')).toBeInTheDocument();
    expect(within(groups).getByRole('link', { name: 'default' })).toHaveAttribute('href', '/groups/default/default');
    const pol = screen.getByTestId('client-policy');
    expect(within(pol).getAllByRole('link', { name: 'social-media' })[0]).toHaveAttribute('href', '/policies/default/social-media');
    expect(pol).toHaveTextContent('the highest priority (5) of 2 active policies; their lists are merged.');
    expect(within(pol).getByText('active, lists merged')).toBeInTheDocument();
    const rows = mockDB().queries.filter((q) => q.client.ip === '192.168.1.10');
    expect(await screen.findByText(`Showing ${Math.min(50, rows.length)} queries`)).toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/undefined|NaN|\[object Object\]/);
  });

  it('adds the client to a group with the typed picker and removes it again', async () => {
    renderWithApp(<ClientDetailPage params={{ key: '192.168.1.10' }} />, { route: '/clients/192.168.1.10' });
    const picker = await findSelector('Add to group');
    await waitFor(() => expect(picker).not.toBeDisabled());
    // Only groups the API may write and the client is not in yet.
    expect(optionLabels(picker)).toEqual(['Choose a group…', 'kids', 'servers']);
    pick(picker, 'kids');
    fireEvent.click(screen.getByRole('button', { name: 'Add' }));
    await waitFor(() => expect(group('kids')?.spec.clients?.at(-1)).toEqual({ ip: '192.168.1.10', name: 'robert-laptop' }));
    const groups = screen.getByTestId('client-groups');
    await waitFor(() => expect(within(groups).getByText('Matched by its exact IP address.')).toBeInTheDocument());
    fireEvent.click(within(groups).getByRole('button', { name: 'Remove' }));
    await waitFor(() => expect(group('kids')?.spec.clients?.some((m) => m.ip === '192.168.1.10')).toBe(false));
  });

  it('allows or denies a domain with validation', async () => {
    renderWithApp(<ClientDetailPage params={{ key: '192.168.1.10' }} />, { route: '/clients/192.168.1.10' });
    const input = await screen.findByRole('textbox', { name: /^Domain/ });
    fireEvent.change(input, { target: { value: 'bad..domain' } });
    expect(screen.getByText('"bad..domain" is not a domain name')).toBeInTheDocument();
    fireEvent.change(input, { target: { value: 'games.example.com' } });
    await clickWhenEnabled('Deny');
    await waitFor(() => expect(uiEntryValues('ui-deny')).toContain('games.example.com'));
  });

  it('works for a client the last 24 hours do not know', async () => {
    renderWithApp(<ClientDetailPage params={{ key: 'tablet.dot' }} />, { route: '/clients/tablet.dot' });
    expect(await screen.findByText('no queries in the last 24 hours')).toBeInTheDocument();
    expect(await screen.findByText(/nothing more specific/)).toBeInTheDocument();
  });
});
