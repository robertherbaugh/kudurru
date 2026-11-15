import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { qk } from '../../api/context';
import type { ExternalUser, Me } from '../../api/types';
import { server } from '../../test/setup';
import { API, renderWithApp } from '../../test/utils';
import { ExternalUsersPanel } from './ExternalUsers';

const row: ExternalUser = { name: 'a'.repeat(64), uid: 'b'.repeat(32), revision: 'c'.repeat(64), provider: 'corp', providerUID: 'provider-one', origin: 'config', issuer: 'https://identity.example', subject: 'independent-subject', username: 'corp:alice', displayName: 'Alice', email: 'alice@example.test', groups: ['corp:dns'], disabled: false, createdAt: '2026-10-01T00:00:00Z' };
function click(label: string) { fireEvent.click(screen.getByRole('button', { name: label })); }
function confirm(label: string) { fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: label })); }
async function open() {
  server.use(http.get(`${API}/iam/external-users`, () => HttpResponse.json({ items: [row], took_ms: 1 })));
  const rendered = renderWithApp(<ExternalUsersPanel />);
  await screen.findByRole('table', { name: 'External accounts' });
  return rendered;
}
describe('external account administration', () => {
  it('shows provider, exact UID and subject and confirms exact revision once', async () => {
    const f = await open();
    let calls = 0;
    let body: unknown;
    let csrf = '';
    server.use(http.post(`${API}/iam/external-users/:name/disable`, async ({ request, params }) => {
      calls++; expect(params.name).toBe(row.name); body = await request.json(); csrf = request.headers.get('X-Kudurru-CSRF') ?? '';
      return new HttpResponse(null, { status: 204 });
    }));
    click('Disable account');
    expect(screen.getByRole('dialog')).toHaveTextContent(row.uid);
    expect(screen.getByRole('dialog')).toHaveTextContent(row.subject);
    confirm('Disable account'); confirm('Disable account');
    await screen.findByText(/snapshot still shows the earlier revision/);
    expect(calls).toBe(1); expect(body).toEqual({ uid: row.uid, revision: row.revision });
    expect(csrf).toBe(f.client.getQueryData<Me>(qk.me)!.csrf);
    expect(screen.getByRole('button', { name: 'Disable account' })).toBeDisabled();
    server.use(http.get(`${API}/iam/external-users`, () => HttpResponse.json({ items: [{ ...row, disabled: true, revision: 'd'.repeat(64) }] })));
    click('Refresh accounts');
    await screen.findByRole('button', { name: 'Enable account' });
    expect(screen.getByRole('button', { name: 'Enable account' })).toBeEnabled();
    expect(calls).toBe(1);
  });
  it.each([409, 503, 0])('never retries an uncertain or stale command (%s)', async (status) => {
    await open(); let calls = 0;
    server.use(http.post(`${API}/iam/external-users/:name/logout`, () => { calls++; return status ? HttpResponse.json({ error: { key: 'conflict', message: 'Untrusted response detail' } }, { status }) : HttpResponse.error(); }));
    click('Log out everywhere'); confirm('Log out everywhere');
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(screen.getByRole('button', { name: 'Log out everywhere' })).toBeDisabled();
    expect(screen.queryByText('Untrusted response detail')).not.toBeInTheDocument();
    click('Refresh accounts');
    await waitFor(() => expect(screen.getByRole('button', { name: 'Log out everywhere' })).toBeEnabled());
    expect(calls).toBe(1);
  });
  it('does not query without users.read and never exposes local password actions', async () => {
    let reads = 0;
    server.use(http.get(`${API}/auth/me`, () => HttpResponse.json({ principal: { id: 'viewer', name: 'Viewer', kind: 'User', groups: [], auth: 'cookie' }, csrf: 'fixture', roles: [], permissions: {} })), http.get(`${API}/iam/external-users`, () => { reads++; return HttpResponse.json({ items: [row] }); }));
    renderWithApp(<ExternalUsersPanel />);
    await screen.findByText('Reading external accounts requires users.read permission.');
    expect(reads).toBe(0); expect(screen.queryByRole('button', { name: /password|authenticator/i })).not.toBeInTheDocument();
  });
  it('refuses writes for a read-only caller', async () => {
    server.use(http.get(`${API}/auth/me`, () => HttpResponse.json({ principal: { id: 'reader', name: 'Reader', kind: 'User', groups: [], auth: 'cookie' }, csrf: 'fixture', roles: [], permissions: { users: ['read'] } })));
    await open();
    expect(screen.getByRole('button', { name: 'Disable account' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Log out everywhere' })).toBeDisabled();
  });
  it('shows explicit unsupported runtime without presenting an empty inventory', async () => {
    server.use(http.get(`${API}/iam/external-users`, () => HttpResponse.json({ error: { key: 'external_users_unavailable', message: 'private diagnostic' } }, { status: 503 })));
    renderWithApp(<ExternalUsersPanel />);
    await screen.findByText('External account administration is unavailable on this server.');
    expect(screen.queryByText('No persisted external accounts.')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Disable account' })).not.toBeInTheDocument();
  });
  it('retires a selected account when the caller changes', async () => {
    const f = await open();
    click('Disable account');
    const previous = f.client.getQueryData<Me>(qk.me)!;
    await act(async () => { f.client.setQueryData(qk.me, { ...previous, csrf: 'successor-csrf' }); await Promise.resolve(); });
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(JSON.stringify(f.client.getQueryCache().getAll().map((q) => q.queryKey))).not.toContain('successor-csrf');
  });
  it('recovers an invalidated later-page cursor without replaying its mutation', async () => {
    let changed = false;
    let calls = 0;
    const second = { ...row, name: 'd'.repeat(64), uid: 'e'.repeat(32) };
    server.use(http.get(`${API}/iam/external-users`, ({ request }) => {
      const cursor = new URL(request.url).searchParams.get('continue');
      if (cursor && changed) return HttpResponse.json({ error: { key: 'conflict', message: 'changed page' } }, { status: 409 });
      return HttpResponse.json(cursor ? { items: [second] } : { items: [row], continue: changed ? undefined : '1:cursor' });
    }), http.post(`${API}/iam/external-users/:name/logout`, () => { calls++; changed = true; return new HttpResponse(null, { status: 204 }); }));
    renderWithApp(<ExternalUsersPanel />);
    await screen.findByRole('table', { name: 'External accounts' });
    click('Next account page');
    await screen.findByText(second.uid);
    click('Log out everywhere'); confirm('Log out everywhere');
    await screen.findByText('Accounts could not be refreshed. No action was retried.');
    expect(screen.getByRole('button', { name: 'First account page' })).toBeEnabled();
    click('First account page');
    await screen.findByText(row.uid);
    click('Refresh accounts');
    await waitFor(() => expect(screen.getByRole('button', { name: 'Log out everywhere' })).toBeEnabled());
    expect(calls).toBe(1);
  });

});
