import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { useEffect } from 'react';
import { describe, expect, it, vi } from 'vitest';
import { ApiError } from '../../api/client';
import { qk, useApiContext } from '../../api/context';
import type { Me, SessionRecordInfo } from '../../api/types';
import { resetDB, signIn } from '../../mocks/handlers';
import AccessPage from '../../pages/AccessPage';
import { server } from '../../test/setup';
import { API, renderWithApp } from '../../test/utils';
import { SessionsPanel } from './Sessions';

const rows: SessionRecordInfo[] = [
  { id: 'newer/other', issuedAt: '2026-10-15T20:00:00Z', expiresAt: '2026-10-16T08:00:00Z', idleExpiresAt: '2026-10-15T20:30:00Z', current: false, revoked: false },
  { id: 'older-current', issuedAt: '2026-10-15T19:00:00Z', expiresAt: '2026-10-16T07:00:00Z', idleExpiresAt: '2026-10-15T19:30:00Z', current: true, revoked: false },
  { id: 'revoked', issuedAt: '2026-10-15T18:00:00Z', expiresAt: '2026-10-16T06:00:00Z', idleExpiresAt: '2026-10-15T18:30:00Z', current: false, revoked: true },
];
type Context = ReturnType<typeof useApiContext>;
function Probe({ capture }: { capture: (ctx: Context) => void }) {
  const ctx = useApiContext();
  useEffect(() => { capture(ctx); }, [capture, ctx]);
  return <button onClick={() => { void ctx.client.request('POST', '/probe-csrf'); }}>Probe CSRF</button>;
}
function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
function fail(status: number, key = 'test') {
  return HttpResponse.json({ error: { key, message: 'Untrusted server details' } }, { status, headers: { 'Retry-After': '5' } });
}
function click(name: string) { fireEvent.click(screen.getByRole('button', { name })); }
function confirm() { fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: /^(Revoke selected session|Revoke this session)$/ })); }
async function open(data = rows) {
  let ctx!: Context;
  server.use(http.get(`${API}/auth/sessions`, () => HttpResponse.json({ items: data })));
  const rendered = renderWithApp(<><SessionsPanel /><Probe capture={(v) => { ctx = v; }} /></>);
  await screen.findByRole('table', { name: 'Your browser sessions' });
  return { ...rendered, ctx };
}
async function probeCSRF(ctx: Context, expected: string | undefined) {
  let matches: boolean | undefined;
  server.use(http.post(`${API}/probe-csrf`, ({ request }) => { matches = request.headers.get('X-Kudurru-CSRF') === (expected ?? null); return new HttpResponse(null, { status: 204 }); }));
  await ctx.client.request('POST', '/probe-csrf');
  expect(matches).toBe(true);
}
async function update(fn: () => void) { await act(async () => { fn(); await Promise.resolve(); }); }
async function drain() { await act(async () => { await new Promise((r) => setTimeout(r, 15)); }); }

describe('Access own browser sessions', () => {
  it('renders exact returned times and current/revoked flags without guessing activity or UID', async () => {
    const { client } = await open();
    const table = screen.getByRole('table', { name: 'Your browser sessions' });
    const displayed = within(table).getAllByRole('row');
    expect(displayed[1]).toHaveTextContent(rows[0]!.id);
    expect(displayed[1]).not.toHaveTextContent('Current session');
    expect(displayed[2]).toHaveTextContent('Current session');
    for (const s of rows) {
      const row = within(table).getByText(s.id).closest('tr')!;
      for (const at of [s.issuedAt, s.expiresAt, s.idleExpiresAt]) expect(within(row).getByText(at, { exact: true })).toBeVisible();
    }
    expect(within(table).getByRole('button', { name: 'Revoke session revoked' })).toBeDisabled();
    expect(table).not.toHaveTextContent(/last (seen|activity)|device|ip address|active|account.?uid/i);
    expect(JSON.stringify(client.getQueryCache().getAll().map((q) => q.queryKey))).not.toContain(client.getQueryData<Me>(qk.me)!.csrf);
    expect(client.getMutationCache().getAll()).toHaveLength(0);
  });

  it.each(['local', 'external'])('admits a %s cookie owner without management grants', async (provider) => {
    const me: Me = { principal: { id: `${provider}:owner`, kind: 'User', name: 'owner', provider, authMethod: 'cookie', groups: [] }, csrf: 'test-cookie-csrf', roles: [], permissions: {} };
    server.use(http.get(`${API}/auth/me`, () => HttpResponse.json(me)));
    await open();
    expect(screen.getByRole('button', { name: 'Revoke this session' })).toBeEnabled();
  });

  it.each(['token', 'service-account', 'password-change', 'missing-csrf'])('does not query sessions for %s', async (restriction) => {
    let reads = 0;
    const me: Me = { principal: { id: 'owner', name: 'owner', kind: restriction === 'service-account' ? 'ServiceAccount' : 'User', groups: [], authMethod: restriction === 'token' ? 'token' : 'cookie' }, roles: [], permissions: {}, csrf: restriction === 'missing-csrf' ? undefined : 'csrf-fixture', mustChangePassword: restriction === 'password-change' };
    server.use(http.get(`${API}/auth/me`, () => HttpResponse.json(me)), http.get(`${API}/auth/sessions`, () => { reads++; return HttpResponse.json({ items: [] }); }));
    renderWithApp(<SessionsPanel />);
    await screen.findByText('Session management requires a cookie session with a completed password change.');
    expect(reads).toBe(0);
  });

  it('uses encoded exact ID and CSRF once, retains the caller, and observes committed revocation', async () => {
    let calls = 0;
    let sentID = '';
    let csrf = false;
    let body = 'unread';
    let state = structuredClone(rows);
    const f = await open();
    const original = f.client.getQueryData<Me>(qk.me)!;
    server.use(
      http.get(`${API}/auth/sessions`, () => HttpResponse.json({ items: state })),
      http.delete(`${API}/auth/sessions/:id`, async ({ request, params }) => { calls++; sentID = String(params.id); csrf = request.headers.get('X-Kudurru-CSRF') === original.csrf; body = await request.text(); state = state.map((s) => s.id === sentID ? { ...s, revoked: true } : s); return new HttpResponse(null, { status: 204 }); }),
    );
    click(`Revoke session ${rows[0]!.id}`);
    expect(screen.getByRole('dialog')).toHaveTextContent(rows[0]!.issuedAt);
    confirm(); confirm();
    await screen.findByText('Session revocation is reflected in this snapshot.');
    expect({ calls, sentID, csrf, body }).toEqual({ calls: 1, sentID: rows[0]!.id, csrf: true, body: '' });
    expect(f.client.getQueryData(qk.me)).toEqual(original);
    await probeCSRF(f.ctx, original.csrf);
    expect(f.client.getMutationCache().getAll()).toHaveLength(0);
  });

  it('blocks another command while an old cut has not observed the committed revoke', async () => {
    const f = await open();
    const revoke = vi.spyOn(f.ctx.api.auth, 'revokeSession').mockResolvedValue();
    click(`Revoke session ${rows[0]!.id}`); confirm();
    await screen.findByText(/This snapshot has not shown the change yet/);
    expect(screen.getByRole('button', { name: 'Revoke this session' })).toBeDisabled();
    expect(revoke).toHaveBeenCalledTimes(1);
    server.use(http.get(`${API}/auth/sessions`, () => HttpResponse.json({ items: rows.map((r, i) => i ? r : { ...r, revoked: true }) })));
    click('Refresh sessions');
    await screen.findByText('Session revocation is reflected in this snapshot.');
    expect(screen.getByRole('button', { name: 'Revoke this session' })).toBeEnabled();
    expect(revoke).toHaveBeenCalledTimes(1);
  });

  it('clears current auth after 204 without logout and retires a delayed me CSRF side effect', async () => {
    const f = await open();
    const old = f.client.getQueryData<Me>(qk.me)!;
    const response = deferred<void>();
    let started = false;
    let ended = false;
    let logout = 0;
    server.use(
      http.get(`${API}/auth/me`, async () => { started = true; await response.promise; ended = true; return HttpResponse.json(old); }),
      http.delete(`${API}/auth/sessions/:id`, () => new HttpResponse(null, { status: 204 })),
      http.post(`${API}/auth/logout`, () => { logout++; return new HttpResponse(null, { status: 204 }); }),
    );
    void f.client.refetchQueries({ queryKey: qk.me });
    await waitFor(() => expect(started).toBe(true));
    click('Revoke this session');
    expect(screen.getByRole('dialog')).toHaveTextContent('returns you to sign in');
    confirm();
    await waitFor(() => expect(f.client.getQueryData(qk.me)).toBeNull());
    response.resolve(); await waitFor(() => expect(ended).toBe(true)); await drain();
    expect(f.client.getQueryData(qk.me)).toBeNull();
    await probeCSRF(f.ctx, undefined);
    expect(logout).toBe(0);
    expect(f.client.getQueryCache().getAll().map((q) => q.queryKey)).toEqual([qk.me]);
  });

  it.each(['204', '401'])('ignores old %s after same-name successor login even if transport ignores abort', async (outcome) => {
    const f = await open();
    const late = deferred<void>();
    const revoke = vi.spyOn(f.ctx.api.auth, 'revokeSession').mockImplementation(() => late.promise);
    click('Revoke this session'); confirm();
    const successor = { ...f.client.getQueryData<Me>(qk.me)!, csrf: 'successor-csrf', expiresAt: '2030-01-01T00:00:00Z' };
    await update(() => { f.ctx.setCsrf(successor.csrf); f.client.setQueryData(qk.me, successor); });
    await screen.findByRole('table');
    await update(() => { if (outcome === '204') late.resolve(); else late.reject(new ApiError(401, 'unauthenticated', 'Old proof')); });
    await drain();
    expect(revoke.mock.calls[0]![1]!.aborted).toBe(true);
    expect(f.client.getQueryData(qk.me)).toEqual(successor);
    await probeCSRF(f.ctx, successor.csrf);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('checks the original lifetime again AFTER cancelQueries yields', async () => {
    const f = await open();
    vi.spyOn(f.ctx.api.auth, 'revokeSession').mockResolvedValue();
    const entered = deferred<void>();
    const release = deferred<void>();
    const cancel = f.client.cancelQueries.bind(f.client);
    vi.spyOn(f.client, 'cancelQueries').mockImplementation(async () => { await cancel(); entered.resolve(); await release.promise; });
    click('Revoke this session'); confirm();
    await entered.promise;
    const successor = { ...f.client.getQueryData<Me>(qk.me)!, csrf: 'later-successor' };
    await update(() => { f.ctx.setCsrf(successor.csrf); f.client.setQueryData(qk.me, successor); });
    await update(() => { release.resolve(); });
    await drain();
    expect(f.client.getQueryData(qk.me)).toEqual(successor);
    await probeCSRF(f.ctx, successor.csrf);
  });

  it.each(['204', '401'])('ignores old %s after unmount', async (outcome) => {
    const f = await open();
    const late = deferred<void>();
    vi.spyOn(f.ctx.api.auth, 'revokeSession').mockImplementation(() => late.promise);
    const original = f.client.getQueryData<Me>(qk.me)!;
    click('Revoke this session'); confirm(); f.unmount();
    await update(() => { if (outcome === '204') late.resolve(); else late.reject(new ApiError(401, 'unauthenticated', 'Old proof')); });
    expect(f.client.getQueryData(qk.me)).toEqual(original);
    await probeCSRF(f.ctx, original.csrf);
  });

  it('discards old deferred list data when the same named session is replaced', async () => {
    let ctx!: Context;
    const f = renderWithApp(<><SessionsPanel /><Probe capture={(v) => { ctx = v; }} /></>);
    await screen.findByRole('table');
    const late = deferred<{ items: SessionRecordInfo[] }>();
    const list = vi.spyOn(ctx.api.auth, 'sessions').mockImplementationOnce(() => late.promise).mockResolvedValue({ items: rows });
    click('Refresh sessions');
    await waitFor(() => expect(list).toHaveBeenCalledTimes(1));
    const successor = { ...f.client.getQueryData<Me>(qk.me)!, csrf: 'new-csrf' };
    await update(() => { ctx.setCsrf(successor.csrf); f.client.setQueryData(qk.me, successor); });
    await screen.findByText(rows[0]!.id);
    await update(() => { late.resolve({ items: [{ ...rows[0]!, id: 'retired-private-row' }] }); });
    await drain();
    expect(screen.queryByText('retired-private-row')).not.toBeInTheDocument();
    expect(list.mock.calls[0]![0]!.aborted).toBe(true);
  });

  it.each([403, 404, 409, 429, 503, 0])('does not retry a failed revoke (%s) or optimistically change rows', async (status) => {
    const f = await open();
    const revoke = vi.spyOn(f.ctx.api.auth, 'revokeSession').mockRejectedValue(new ApiError(status, 'test', 'Untrusted details', undefined, status === 429 ? 5 : undefined));
    click(`Revoke session ${rows[0]!.id}`); confirm();
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(screen.getByRole('button', { name: 'Revoke this session' })).toBeDisabled();
    expect(screen.queryByText('Untrusted details')).not.toBeInTheDocument();
    if (status === 429) expect(screen.getByText(/Retry after 5 seconds/)).toBeVisible();
    if (status === 0 || status === 503) expect(screen.getByText(/revocation outcome is unknown/)).toBeVisible();
    expect(revoke).toHaveBeenCalledTimes(1);
    click('Refresh sessions');
    await screen.findByText(/Session inventory refreshed/);
    expect(revoke).toHaveBeenCalledTimes(1);
    expect(screen.getByRole('button', { name: 'Revoke this session' })).toBeEnabled();
  });

  it('clears the still-original session on revoke401', async () => {
    const f = await open();
    vi.spyOn(f.ctx.api.auth, 'revokeSession').mockRejectedValue(new ApiError(401, 'unauthenticated', 'Denied'));
    click('Revoke this session'); confirm();
    await waitFor(() => expect(f.client.getQueryData(qk.me)).toBeNull());
    await probeCSRF(f.ctx, undefined);
  });

  it('cancels a pending selection without replay or authority claims', async () => {
    const f = await open();
    const late = deferred<void>();
    const revoke = vi.spyOn(f.ctx.api.auth, 'revokeSession').mockImplementation(() => late.promise);
    click(`Revoke session ${rows[0]!.id}`); confirm(); click('Cancel');
    expect(revoke.mock.calls[0]![1]!.aborted).toBe(true);
    expect(screen.getByText(/revocation outcome is unknown/)).toBeVisible();
    await update(() => { late.resolve(); });
    expect(revoke).toHaveBeenCalledTimes(1);
    expect(f.client.getQueryData(qk.me)).toBeTruthy();
  });

  it.each([403, 503])('keeps unavailable read%s separate from empty and allows a safe refresh', async (status) => {
    server.use(http.get(`${API}/auth/sessions`, () => fail(status, status === 503 ? 'sessions_unavailable' : 'forbidden')));
    renderWithApp(<SessionsPanel />);
    await screen.findByText(status === 503 ? 'Session inventory is unavailable on this server.' : 'Session inventory was refused. Your current session may no longer have access.');
    expect(screen.queryByText('No retained sessions in this snapshot.')).not.toBeInTheDocument();
    server.use(http.get(`${API}/auth/sessions`, () => HttpResponse.json({ items: [] })));
    click('Refresh sessions');
    await screen.findByText('No retained sessions in this snapshot.');
  });

  it('clears the original session on inventory401', async () => {
    server.use(http.get(`${API}/auth/sessions`, () => fail(401)));
    const f = renderWithApp(<SessionsPanel />);
    await waitFor(() => expect(f.client.getQueryData(qk.me)).toBeNull());
  });

  it('never invents a current row for a snapshot with none', async () => {
    await open(rows.map((s) => ({ ...s, current: false })));
    expect(screen.getByText(/snapshot has no current-session row/)).toBeVisible();
    expect(screen.queryByRole('button', { name: 'Revoke this session' })).not.toBeInTheDocument();
  });

  it('integrates into Access and the safe mock inventory resets after individual revoke', async () => {
    const f = renderWithApp(<AccessPage />, { route: '/access' });
    await screen.findByRole('table', { name: 'Your browser sessions' });
    click('Revoke session mock-session-admin-other'); confirm();
    await screen.findByText('Session revocation is reflected in this snapshot.');
    f.unmount(); resetDB(); signIn();
    renderWithApp(<SessionsPanel />);
    expect(await screen.findByRole('button', { name: 'Revoke session mock-session-admin-other' })).toBeEnabled();
  });
});
