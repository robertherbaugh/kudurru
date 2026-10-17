import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it, vi } from 'vitest';
import { qk, useApiContext } from '../../api/context';
import type { Me } from '../../api/types';
import { mockDB } from '../../mocks/handlers';
import AccessPage from '../../pages/AccessPage';
import { server } from '../../test/setup';
import { API, renderWithApp } from '../../test/utils';
import { AuthenticatorPanel, RecoverAuthenticatorButton } from './Authenticators';

const seed = 'UI-TEST-PRIVATE-KEY';
const challenge = 'ui-private-challenge';
const password = 'admin';
function setupResponse(ttl = 60_000) {
  return HttpResponse.json({ secret: seed, challenge, expiresAt: new Date(Date.now() + ttl).toISOString(), otpauthURI: 'otpauth://must-not-be-rendered' });
}
function fail(status: number, retryAfter?: string) {
  return HttpResponse.json({ error: { key: 'test', message: 'server details must not be displayed' } }, { status, headers: retryAfter ? { 'Retry-After': retryAfter } : {} });
}
function input(label: string, value: string) { fireEvent.change(screen.getByLabelText(label), { target: { value } }); }
function click(name: string) { fireEvent.click(screen.getByRole('button', { name })); }
async function openEnrollment(replacing = false) {
  click(replacing ? 'Replace authenticator' : 'Set up authenticator');
  input('Current password', password);
  if (replacing) input('Current authenticator code (old authenticator)', '001234');
  click('Next');
  await screen.findByLabelText('Authenticator key');
}
async function openPanel(enabled = false) {
  server.use(http.get(`${API}/auth/totp`, () => HttpResponse.json({ enabled })));
  const rendered = renderWithApp(<AuthenticatorPanel />);
  await screen.findByRole('button', { name: enabled ? 'Replace authenticator' : 'Set up authenticator' });
  return rendered;
}

describe('Access authenticators', () => {
  it('separates replacement proof from new proof, keeps secrets out of caches, and clears session without logout', async () => {
    const seen: { path: string; body: unknown; csrf: boolean }[] = [];
    let logout = 0;
    server.use(
      http.post(`${API}/auth/totp/setup`, async ({ request }) => { seen.push({ path: 'setup', body: await request.json(), csrf: !!request.headers.get('X-Kudurru-CSRF') }); return setupResponse(); }),
      http.post(`${API}/auth/totp/confirm`, async ({ request }) => { seen.push({ path: 'confirm', body: await request.json(), csrf: !!request.headers.get('X-Kudurru-CSRF') }); return new HttpResponse(null, { status: 204 }); }),
      http.post(`${API}/auth/logout`, () => { logout++; return new HttpResponse(null, { status: 204 }); }),
    );
    const { client } = await openPanel(true);
    await openEnrollment(true);
    expect(screen.getByLabelText('Authenticator key')).toHaveValue(seed);
    expect(document.body.textContent).not.toContain(challenge);
    expect(document.body.textContent).not.toContain('otpauth://');
    expect(document.querySelector('img')).toBeNull();
    expect(JSON.stringify(client.getQueryCache().getAll().map((q) => q.state.data))).not.toContain(seed);
    expect(client.getMutationCache().getAll()).toHaveLength(0);
    expect(JSON.stringify(client.getQueryCache().getAll().map((q) => q.queryKey))).not.toContain(client.getQueryData<Me>(qk.me)!.csrf);
    click('Next');
    await screen.findByLabelText('Current password again');
    input('Current password again', password);
    input('Fresh current code (old authenticator)', '002345');
    input('New authenticator code', '003456');
    click('Confirm authenticator');
    await waitFor(() => expect(client.getQueryData(qk.me)).toBeNull());
    expect(seen).toEqual([
      { path: 'setup', body: { currentPassword: password, totpCode: '001234' }, csrf: true },
      { path: 'confirm', body: { currentPassword: password, totpCode: '002345', code: '003456', challenge }, csrf: true },
    ]);
    expect(logout).toBe(0);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(client.getMutationCache().getAll()).toHaveLength(0);
  });

  it('does not generate a second setup when returning to the verified step', async () => {
    let calls = 0;
    server.use(http.post(`${API}/auth/totp/setup`, () => { calls++; return setupResponse(); }));
    await openPanel(); await openEnrollment();
    click('Back'); click('Next');
    await screen.findByLabelText('Authenticator key');
    expect(calls).toBe(1);
    click('Back'); input('Current password', 'changed-proof'); click('Next');
    await screen.findByLabelText('Authenticator key');
    expect(calls).toBe(2);
  });

  it('invalidates delayed setup after confirmed discard and never replays it', async () => {
    let release!: () => void;
    let calls = 0;
    const barrier = new Promise<void>((resolve) => { release = resolve; });
    server.use(http.post(`${API}/auth/totp/setup`, async () => { calls++; await barrier; return setupResponse(); }));
    await openPanel();
    click('Set up authenticator'); input('Current password', password);
    const form = screen.getByRole('form', { name: 'Authenticator setup' });
    fireEvent.submit(form); fireEvent.submit(form);
    await waitFor(() => expect(calls).toBe(1));
    click('Cancel'); click('Discard');
    await act(async () => { release(); await barrier; });
    expect(screen.queryByLabelText('Authenticator key')).not.toBeInTheDocument();
    await screen.findByRole('button', { name: 'Set up authenticator' });
    click('Set up authenticator');
    expect(screen.getByLabelText('Current password')).toHaveValue('');
    expect(calls).toBe(1);
  });

  it('drops the candidate on expiry and cannot confirm through a completed step', async () => {
    let now = Date.now();
    const date = vi.spyOn(Date, 'now').mockImplementation(() => now);
    server.use(http.post(`${API}/auth/totp/setup`, () => setupResponse(200)));
    try {
      await openPanel(); await openEnrollment();
      now += 1000;
      await screen.findByText('Setup expired. Return to Verify account to start a new setup explicitly.');
      expect(screen.queryByLabelText('Authenticator key')).not.toBeInTheDocument();
      click('Next');
      expect(screen.queryByLabelText('New authenticator code')).not.toBeInTheDocument();
    } finally { date.mockRestore(); }
  });

  it.each([503, 0])('discards unknown setup outcome %s with no mutation retry', async (status) => {
    let calls = 0;
    server.use(http.post(`${API}/auth/totp/setup`, () => { calls++; return status ? fail(status) : HttpResponse.error(); }));
    await openPanel(); click('Set up authenticator'); input('Current password', password); click('Next');
    await screen.findByText(/The outcome is unknown/);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(calls).toBe(1);
    expect(document.body.textContent).not.toContain('server details');
  });

  it('honors Retry-After and clears proof without automatically resubmitting', async () => {
    let calls = 0;
    server.use(http.post(`${API}/auth/totp/setup`, () => { calls++; return fail(429, '60'); }));
    await openPanel(); click('Set up authenticator'); input('Current password', password); click('Next');
    await screen.findByText(/Wait 60 seconds/);
    expect(screen.getByLabelText('Current password')).toHaveValue('');
    click('Next'); expect(calls).toBe(1);
  });

  it('keeps a valid cookie after factor 401 and refreshes session once', async () => {
    let reads = 0;
    server.events.on('request:start', ({ request }) => { if (new URL(request.url).pathname.endsWith('/auth/me')) reads++; });
    server.use(http.post(`${API}/auth/totp/setup`, () => fail(401)));
    const { client } = await openPanel(); const before = reads;
    click('Set up authenticator'); input('Current password', 'wrong'); click('Next');
    await screen.findByText(/password or authenticator code was not accepted/);
    await waitFor(() => expect(reads).toBe(before + 1));
    expect(client.getQueryData(qk.me)).not.toBeNull();
    expect(screen.getByLabelText('Current password')).toHaveValue('');
  });

  it('clears a denied cookie on the single session observation after factor 401', async () => {
    const { client } = await openPanel();
    server.use(http.post(`${API}/auth/totp/setup`, () => fail(401)), http.get(`${API}/auth/me`, () => fail(401)));
    click('Set up authenticator'); input('Current password', password); click('Next');
    await waitFor(() => expect(client.getQueryData(qk.me)).toBeNull());
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('disables with exact proof and removes session data and CSRF without logout', async () => {
    let correctBody = false;
    server.use(http.post(`${API}/auth/totp/disable`, async ({ request }) => { const body = await request.json(); correctBody = JSON.stringify(body) === JSON.stringify({ currentPassword: password, totpCode: '001234' }); return new HttpResponse(null, { status: 204 }); }));
    const { client } = await openPanel(true);
    client.setQueryData(['private', 'cached'], { sensitive: true });
    click('Disable authenticator'); input('Current password', password); input('Current authenticator code', '001234');
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Disable authenticator' }));
    await waitFor(() => expect(client.getQueryData(qk.me)).toBeNull());
    expect(correctBody).toBe(true);
    expect(client.getQueryData(['private', 'cached'])).toBeUndefined();
  });

  it('never interprets status failure as disabled', async () => {
    server.use(http.get(`${API}/auth/totp`, () => fail(503)));
    renderWithApp(<AuthenticatorPanel />);
    await screen.findByText('Authenticator status is unavailable. No change can be started.');
    expect(screen.queryByRole('button', { name: 'Set up authenticator' })).not.toBeInTheDocument();
  });

  it.each(['external', 'token', 'mustChange', 'noCsrf'])('does not query for %s principals', async (kind) => {
    let calls = 0;
    const me: Me = { principal: { kind: 'User', id: 'local:admin', name: 'admin', provider: 'local', authMethod: 'cookie', groups: [] }, roles: ['admin'], permissions: { users: ['read', 'write'] }, csrf: 'fixture-csrf' };
    if (kind === 'external') { me.principal.provider = 'dex'; me.principal.id = 'external:other'; }
    if (kind === 'token') me.principal.authMethod = 'token';
    if (kind === 'mustChange') me.mustChangePassword = true;
    if (kind === 'noCsrf') delete me.csrf;
    server.use(
      http.get(`${API}/auth/totp`, () => { calls++; return HttpResponse.json({ enabled: false }); }),
      http.get(`${API}/auth/me`, () => HttpResponse.json(me)),
    );
    const { client } = renderWithApp(<AuthenticatorPanel />);
    await waitFor(() => expect(client.getQueryData(qk.me)).toBeDefined());
    expect(screen.queryByRole('button', { name: 'Set up authenticator' })).not.toBeInTheDocument();
    expect(calls).toBe(0);
  });

  it('drops an open candidate on session replacement', async () => {
    server.use(http.post(`${API}/auth/totp/setup`, () => setupResponse()));
    const { client } = await openPanel(); await openEnrollment();
    const me = client.getQueryData<Me>(qk.me)!;
    act(() => { client.setQueryData(qk.me, { ...me, csrf: 'new-session' }); });
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(screen.queryByLabelText('Authenticator key')).not.toBeInTheDocument();
  });

  it('recovers another user only after a fresh matching UID read and preserves the caller', async () => {
    let fresh = false; let sent = false;
    server.use(
      http.get(`${API}/iam/users`, () => { fresh = true; return HttpResponse.json({ items: [{ name: 'viewer', uid: 'uid-selected' }] }); }),
      http.post(`${API}/iam/users/viewer/totp/recover`, async ({ request }) => { sent = fresh && JSON.stringify(await request.json()) === JSON.stringify({ uid: 'uid-selected' }) && !!request.headers.get('X-Kudurru-CSRF'); return new HttpResponse(null, { status: 204 }); }),
    );
    const { client } = renderWithApp(<RecoverAuthenticatorButton user={{ name: 'viewer', uid: 'uid-selected', disabled: false, builtinAdmin: false, mustChangePassword: false }} />);
    const button = await screen.findByRole('button', { name: 'Recover authenticator for viewer' });
    await waitFor(() => expect(button).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(button);
    await screen.findByText('The selected UID matches the freshly loaded inventory.'); click('Recover authenticator');
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(sent).toBe(true); expect(client.getQueryData(qk.me)).not.toBeNull();
  });

  it('does not silently adopt a recreated account UID', async () => {
    server.use(http.get(`${API}/iam/users`, () => HttpResponse.json({ items: [{ name: 'viewer', uid: 'uid-new' }] })));
    renderWithApp(<RecoverAuthenticatorButton user={{ name: 'viewer', uid: 'uid-old', disabled: false, builtinAdmin: false, mustChangePassword: false }} />);
    const button = await screen.findByRole('button', { name: 'Recover authenticator for viewer' });
    await waitFor(() => expect(button).not.toHaveAttribute('aria-disabled', 'true')); fireEvent.click(button);
    await screen.findByText(/account UID changed or is missing/);
    expect(screen.getByRole('button', { name: 'Recover authenticator' })).toBeDisabled();
    expect(document.body.textContent).not.toContain('uid-new');
  });

  it('integrates recovery into the existing Access inventory and clears self session', async () => {
    const { client } = renderWithApp(<AccessPage />, { route: '/access?tab=iam' });
    const button = await screen.findByRole('button', { name: 'Recover authenticator for admin' });
    await waitFor(() => expect(button).not.toHaveAttribute('aria-disabled', 'true')); fireEvent.click(button);
    await screen.findByText('The selected UID matches the freshly loaded inventory.'); click('Recover authenticator');
    await waitFor(() => expect(client.getQueryData(qk.me)).toBeNull());
    expect(mockDB().session).toBeNull();
  });
  it('closes stale UID recovery conflicts and requires an explicit reselection', async () => {
    let calls = 0;
    server.use(http.post(`${API}/iam/users/viewer/totp/recover`, () => { calls++; return fail(409); }));
    renderWithApp(<RecoverAuthenticatorButton user={{ name: 'viewer', uid: 'mock-user-viewer', disabled: false, builtinAdmin: false, mustChangePassword: false }} />);
    const button = await screen.findByRole('button', { name: 'Recover authenticator for viewer' });
    await waitFor(() => expect(button).not.toHaveAttribute('aria-disabled', 'true')); fireEvent.click(button);
    await screen.findByText('The selected UID matches the freshly loaded inventory.'); click('Recover authenticator');
    await screen.findByText(/account or setup changed/);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument(); expect(calls).toBe(1);
  });

  it('allows a current external cookie administrator to recover a local account', async () => {
    const { client } = renderWithApp(<RecoverAuthenticatorButton user={{ name: 'viewer', uid: 'mock-user-viewer', disabled: false, builtinAdmin: false, mustChangePassword: false }} />);
    const button = await screen.findByRole('button', { name: 'Recover authenticator for viewer' });
    await waitFor(() => expect(button).not.toHaveAttribute('aria-disabled', 'true'));
    const me = client.getQueryData<Me>(qk.me)!;
    await act(async () => { client.setQueryData(qk.me, { ...me, principal: { ...me.principal, id: 'external:admin', provider: 'dex' } }); await new Promise((resolve) => setTimeout(resolve, 0)); });
    fireEvent.click(button); await screen.findByText('The selected UID matches the freshly loaded inventory.');
    click('Recover authenticator'); await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(client.getQueryData(qk.me)).not.toBeNull();
  });

  it('clears CSRF as well as cached session after a completed disable', async () => {
    let csrfPresent = true;
    function Probe() {
      const { client } = useApiContext();
      return <button onClick={() => { void client.request('POST', '/test-csrf'); }}>Probe client</button>;
    }
    server.use(
      http.get(`${API}/auth/totp`, () => HttpResponse.json({ enabled: true })),
      http.post(`${API}/auth/totp/disable`, () => new HttpResponse(null, { status: 204 })),
      http.post(`${API}/test-csrf`, ({ request }) => { csrfPresent = request.headers.has('X-Kudurru-CSRF'); return new HttpResponse(null, { status: 204 }); }),
    );
    const { client } = renderWithApp(<><AuthenticatorPanel /><Probe /></>);
    await screen.findByRole('button', { name: 'Disable authenticator' }); click('Disable authenticator');
    input('Current password', password); input('Current authenticator code', '001234');
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Disable authenticator' }));
    await waitFor(() => expect(client.getQueryData(qk.me)).toBeNull());
    click('Probe client'); await waitFor(() => expect(csrfPresent).toBe(false));
  });

  it.each(['scoped', 'missingUID'])('refuses recovery for %s', async (restriction) => {
    const me: Me = { principal: { kind: 'User', id: 'local:admin', name: 'admin', provider: 'local', authMethod: 'cookie', groups: [] }, roles: ['admin'], permissions: restriction === 'scoped' ? {} : { users: ['read', 'write'] }, scopedPermissions: { 'zone:home': { users: ['write'] } }, csrf: 'fixture-csrf' };
    server.use(http.get(`${API}/auth/me`, () => HttpResponse.json(me)));
    const { client } = renderWithApp(<RecoverAuthenticatorButton user={{ name: 'viewer', uid: restriction === 'missingUID' ? '' : 'mock-user-viewer', disabled: false, builtinAdmin: false, mustChangePassword: false }} />);
    const button = await screen.findByRole('button', { name: 'Recover authenticator for viewer' });
    await waitFor(() => expect(client.getQueryData(qk.me)).toBeDefined());
    expect(button).toBeDisabled();
  });

  it('does not let a canceled older session read restore CSRF after disable', async () => {
    let release!: () => void;
    let readStarted = false;
    let readEnded = false;
    let csrfPresent: boolean | undefined;
    const barrier = new Promise<void>((resolve) => { release = resolve; });
    function Probe() {
      const { client } = useApiContext();
      return <button onClick={() => { void client.request('POST', '/test-csrf'); }}>Probe client</button>;
    }
    server.use(
      http.get(`${API}/auth/totp`, () => HttpResponse.json({ enabled: true })),
      http.post(`${API}/auth/totp/disable`, () => new HttpResponse(null, { status: 204 })),
      http.post(`${API}/test-csrf`, ({ request }) => { csrfPresent = request.headers.has('X-Kudurru-CSRF'); return new HttpResponse(null, { status: 204 }); }),
    );
    const { client } = renderWithApp(<><AuthenticatorPanel /><Probe /></>);
    await screen.findByRole('button', { name: 'Disable authenticator' });
    const older = client.getQueryData<Me>(qk.me)!;
    server.use(http.get(`${API}/auth/me`, async () => { readStarted = true; await barrier; readEnded = true; return HttpResponse.json(older); }));
    void client.refetchQueries({ queryKey: qk.me });
    await waitFor(() => expect(readStarted).toBe(true));
    click('Disable authenticator'); input('Current password', password); input('Current authenticator code', '001234');
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Disable authenticator' }));
    await waitFor(() => expect(client.getQueryData(qk.me)).toBeNull());
    await act(async () => { release(); await barrier; });
    await waitFor(() => expect(readEnded).toBe(true));
    // Drain response parsing before probing the client, without retaining any credentials.
    await act(async () => { await new Promise((resolve) => setTimeout(resolve, 10)); });
    click('Probe client'); await waitFor(() => expect(csrfPresent).toBeDefined());
    expect(csrfPresent).toBe(false);
    expect(client.getQueryData(qk.me)).toBeNull();
  });

});
