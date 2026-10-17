import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { canAnyScope } from '../api/context';
import type { Me } from '../api/types';
import { mockDB } from '../mocks/handlers';
import { findSelector, getSelector, optionsOf, pick, valueOf } from '../test/controls';
import { server } from '../test/setup';
import { renderWithApp } from '../test/utils';
import AccessPage, { scopeString, scopeText } from './AccessPage';
import AuditPage, { actorText, resourceText } from './AuditPage';

/** Records the query strings of GET /audit while passing through to the mock. */
function spyAudit(): URLSearchParams[] {
  const seen: URLSearchParams[] = [];
  server.events.on('request:start', ({ request }) => {
    const u = new URL(request.url);
    if (u.pathname === '/api/v1/audit') seen.push(u.searchParams);
  });
  return seen;
}

describe('AccessPage', () => {
  it('lists tokens by description and creates one with the real body', async () => {
    renderWithApp(<AccessPage />, { route: '/access' });
    const table = await screen.findByRole('table', { name: 'Your API tokens' });
    expect(within(table).getByText('grafana')).toBeInTheDocument();
    expect(within(table).getByText('never')).toBeInTheDocument();
    fireEvent.change(screen.getByRole('textbox', { name: /^Description/ }), { target: { value: 'ci-deploy' } });
    pick(getSelector('Limit to role'), 'Viewer');
    expect(screen.getByText('Read zones, records, lists, policies, stats and settings.')).toBeInTheDocument();
    expect(valueOf(getSelector('Expires in'))).toBe('720h');
    const create = screen.getByRole('button', { name: 'Create token' });
    await waitFor(() => expect(create).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(create);
    expect(await screen.findByText('Token ci-deploy created')).toBeInTheDocument();
    expect(screen.getByTestId('new-token').textContent).toMatch(/^kdr_/);
    const t = mockDB().tokens.find((x) => x.description === 'ci-deploy');
    expect(t?.scopes).toEqual(['viewer']);
  });

  it('scopes a token to one zone', async () => {
    renderWithApp(<AccessPage />, { route: '/access' });
    fireEvent.change(await screen.findByRole('textbox', { name: /^Description/ }), { target: { value: 'dns-edit' } });
    pick(getSelector('Limit to role'), 'Zone admin');
    pick(getSelector('For'), 'One zone');
    const zone = await findSelector('Zone');
    await waitFor(() => expect(optionsOf(zone).length).toBeGreaterThan(1));
    pick(zone, 'example-internal');
    pick(getSelector('Expires in'), 'Server maximum');
    const create = screen.getByRole('button', { name: 'Create token' });
    await waitFor(() => expect(create).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(create);
    expect(await screen.findByText('Token dns-edit created')).toBeInTheDocument();
    expect(scopeString('zone-admin', 'zone', 'example-internal')).toBe('zone-admin@zone:example-internal');
    expect(mockDB().tokens.find((x) => x.description === 'dns-edit')?.scopes).toEqual(['zone-admin@zone:example-internal']);
  });

  it('lists the audit log newest first and pages with the cursor', async () => {
    for (let i = 0; i < 150; i++) mockDB().audit.unshift({ ...mockDB().audit[0]!, id: 2000 + i, ts: new Date(Date.UTC(2026, 9, 11, 0, i)).toISOString(), action: `zone.update-${i}` });
    const seen = spyAudit();
    renderWithApp(<AuditPage />, { route: '/audit' });
    const table = await screen.findByRole('table', { name: 'Audit entries' });
    await within(table).findByText('zone.update-149');
    expect(seen[0]?.get('order')).toBe('desc');
    const rows = within(table).getAllByRole('row');
    // Header + the first page of 100, newest first.
    expect(rows.length).toBe(101);
    expect(within(rows[1]!).getByText('zone.update-149')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Load older events' }));
    expect(await within(table).findByText('blocklist.create')).toBeInTheDocument();
    expect(seen[1]?.get('cursor')).toBe('2050');
    expect(table.textContent).not.toMatch(/undefined|NaN|\[object Object\]/);
  });

  it('renders the IAM collections with their real shapes', async () => {
    renderWithApp(<AccessPage />, { route: '/access?tab=iam' });
    const rb = await screen.findByRole('table', { name: 'Role bindings' });
    const zoneRow = within(rb).getByText('zone-admin').closest('tr')!;
    expect(within(zoneRow).getByText('zone home-arpa')).toBeInTheDocument();
    expect(within(zoneRow).getByText('User robert@example.com')).toBeInTheDocument();
    const idp = await screen.findByRole('table', { name: 'Identity providers' });
    expect(within(idp).getByText('Dex (corporate SSO)')).toBeInTheDocument();
    const sa = await screen.findByRole('table', { name: 'Service accounts' });
    expect(within(sa).getByText('kudurru/external-dns')).toBeInTheDocument();
    const users = await screen.findByRole('table', { name: 'Local users' });
    expect(within(users).getByText('viewer')).toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/undefined|NaN/);
  });

  it('formats actors, resources and scopes', () => {
    expect(actorText({ name: 'bootstrap' })).toBe('bootstrap');
    expect(actorText({ kind: 'ServiceAccount', name: 'grafana' })).toBe('grafana (ServiceAccount)');
    expect(resourceText({ kind: 'blocking' })).toBe('blocking');
    expect(resourceText({ kind: 'BlockList', namespace: 'default', name: 'starter' })).toBe('BlockList default/starter');
    expect(scopeText({ kind: 'cluster' })).toBe('cluster');
    expect(scopeText({ kind: 'zone', name: 'home-arpa' })).toBe('zone home-arpa');
  });

  it('lets a zone-scoped principal manage its own tokens', () => {
    const me: Me = { principal: { kind: 'User', id: 'local:zed', name: 'zed', groups: [] }, roles: ['zone-admin@zone:home'], permissions: {} };
    expect(canAnyScope({ ...me }, 'tokens', 'own')).toBe(false);
    expect(canAnyScope({ ...me, scopedPermissions: { 'zone:home': { tokens: ['own'] } } }, 'tokens', 'own')).toBe(true);
    expect(canAnyScope({ ...me, scopedPermissions: { 'zone:home': { tokens: ['own'] } } }, 'tokens', 'read')).toBe(false);
    expect(canAnyScope({ ...me, permissions: { tokens: ['own'] } }, 'tokens', 'own')).toBe(true);
  });
});
