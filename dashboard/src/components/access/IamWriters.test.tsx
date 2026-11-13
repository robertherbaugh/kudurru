// The IAM write surfaces of the Identity tab (D294, D396) against the MSW
// mock of the reviewed writers (mocks/iam.ts): local accounts, service
// accounts with their tokens, identity providers and role bindings. Every
// command names the reviewed UID and resourceVersion and is sent once; a
// refusal and an unknown outcome are shown, and nothing is sent again; the
// controls are disabled with the reason on a follower, in static mode,
// without the permission and where the driver (Kubernetes) offers no
// writer; a service account token is shown once and kept nowhere else.
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';
import { ApiError } from '../../api/client';
import type { ClusterInfo, Me } from '../../api/types';
import { iamOutcomeUnknown } from '../../lib/iam';
import { providerErrors as providerErrorsFor } from './IdentityProviders';
import { mockDB } from '../../mocks/handlers';
import { mockIAM } from '../../mocks/iam';
import AccessPage from '../../pages/AccessPage';
import { getSelector, pick } from '../../test/controls';
import { server } from '../../test/setup';
import { API, renderWithApp } from '../../test/utils';

function off(el: HTMLElement): boolean {
  return el.hasAttribute('disabled') || el.getAttribute('aria-disabled') === 'true';
}

const cleanups: (() => void)[] = [];
afterEach(() => {
  for (const f of cleanups.splice(0)) f();
});

/** Counts requests of one method and path (the mock also logs bodies in mockIAM().calls). */
function count(method: string, path: string | RegExp): () => number {
  let n = 0;
  const listener = ({ request }: { request: Request }) => {
    const p = new URL(request.url).pathname;
    if (request.method === method && (typeof path === 'string' ? p === `/api/v1${path}` : path.test(p))) n++;
  };
  server.events.on('request:start', listener);
  cleanups.push(() => server.events.removeListener('request:start', listener));
  return () => n;
}

function calls(method: string, path: string) {
  return mockIAM().calls.filter((c) => c.method === method && c.path === `/api/v1${path}`);
}

function asFollower() {
  const info: ClusterInfo = {
    clusterID: 'c', driver: 'sqlite', epoch: 7, leader: 'kudurru-0', nodes: 2, readOnly: false, node: 'kudurru-1',
    role: 'follower', primary: 'kudurru-0', primaryRequired: true, forwardedOperations: ['putObject'], localOperations: [], took_ms: 1,
  };
  server.use(http.get(`${API}/cluster`, () => HttpResponse.json(info)));
}

function meAs(permissions: Record<string, string[]>) {
  server.use(
    http.get(`${API}/auth/me`, () =>
      HttpResponse.json({ principal: { kind: 'User', id: 'local:ops', name: 'ops', auth: 'cookie', provider: 'local', groups: [] }, roles: ['custom'], csrf: 'csrf-mock-token', permissions } satisfies Me),
    ),
  );
}

function identity() {
  return renderWithApp(<AccessPage />, { route: '/access?tab=iam' });
}

/** Waits for a control to be enabled and clicks it. */
async function press(name: string | RegExp, root: HTMLElement | Document = document) {
  const button = await within(root as HTMLElement).findByRole('button', { name });
  await waitFor(() => expect(off(button)).toBe(false));
  fireEvent.click(button);
}

async function dialog(): Promise<HTMLElement> {
  return screen.findByRole('dialog');
}

/** The dialog once its reviewed read has answered (the loading frame is replaced by the form's). */
async function reviewed(): Promise<HTMLElement> {
  await waitFor(() => expect(screen.getByRole('dialog').textContent).not.toMatch(/Reading the /));
  return screen.getByRole('dialog');
}

const closed = () => waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());

describe('local users', () => {
  it('creates an account with a first password that is sent once and never kept', async () => {
    const posts = count('POST', '/iam/users');
    const view = identity();
    await press('New user');
    const d = await dialog();
    fireEvent.change(within(d).getByLabelText(/^Username/), { target: { value: 'alice' } });
    fireEvent.change(within(d).getByLabelText(/^Display name/), { target: { value: 'Alice' } });
    fireEvent.change(within(d).getByLabelText(/^First password/), { target: { value: 'correct-horse' } });
    fireEvent.change(within(d).getByLabelText(/^Confirm password/), { target: { value: 'correct-hors' } });
    fireEvent.click(within(d).getByRole('button', { name: 'Create user' }));
    expect(await within(d).findByText('Does not match the password')).toBeInTheDocument();
    expect(posts()).toBe(0);
    fireEvent.change(within(d).getByLabelText(/^Confirm password/), { target: { value: 'correct-horse' } });
    fireEvent.click(within(d).getByRole('button', { name: 'Create user' }));
    await closed();
    expect(posts()).toBe(1);
    expect(calls('POST', '/iam/users')[0]?.body).toEqual({ name: 'alice', displayName: 'Alice', password: '<13 chars>' });
    const table = await screen.findByRole('table', { name: 'Local users' });
    expect(await within(table).findByText('Alice')).toBeInTheDocument();
    expect(within(table).getAllByText('must change password').length).toBeGreaterThan(0);
    // The password is in no query or mutation cache and not in the page.
    expect(JSON.stringify(view.client.getQueryCache().getAll().map((q) => q.state.data))).not.toContain('correct-horse');
    expect(view.client.getMutationCache().getAll()).toHaveLength(0);
    expect(document.body.innerHTML).not.toContain('correct-horse');
  });

  it('changes only the groups it was given, with the reviewed UID and resourceVersion, and says it signs the account out', async () => {
    identity();
    await press('Edit viewer');
    const d = await dialog();
    const save = within(d).getByRole('button', { name: 'Save changes' });
    expect(off(save)).toBe(true);
    fireEvent.click(within(d).getByRole('button', { name: 'Add group' }));
    fireEvent.change(within(d).getByLabelText('Local groups 1'), { target: { value: 'oidc:ops' } });
    fireEvent.click(save);
    expect(await within(d).findAllByText(/no colon/)).not.toHaveLength(0);
    expect(calls('PATCH', '/iam/users/viewer')).toHaveLength(0);
    fireEvent.change(within(d).getByLabelText('Local groups 1'), { target: { value: 'operators' } });
    expect(within(d).getByText('Changing the groups signs the account out')).toBeInTheDocument();
    expect(within(d).getByText(/about 4,000 per cluster/)).toBeInTheDocument();
    fireEvent.click(save);
    await closed();
    expect(calls('PATCH', '/iam/users/viewer').map((c) => c.body)).toEqual([{ uid: 'mock-user-viewer', resourceVersion: '1', groups: ['operators'] }]);
    const table = await screen.findByRole('table', { name: 'Local users' });
    expect(await within(table).findByText('operators')).toBeInTheDocument();
  });

  it('disables and enables an account and states the permanent revocation record and its limit', async () => {
    identity();
    await press('Disable viewer');
    let d = await dialog();
    expect(within(d).getByText('Permanent revocation record')).toBeInTheDocument();
    expect(within(d).getByText(/at the limit, deletes, disables and new sign-ins answer 503 and change nothing/)).toBeInTheDocument();
    fireEvent.click(within(d).getByRole('button', { name: 'Disable account' }));
    await closed();
    expect(calls('PATCH', '/iam/users/viewer').map((c) => c.body)).toEqual([{ uid: 'mock-user-viewer', resourceVersion: '1', disabled: true }]);
    const table = await screen.findByRole('table', { name: 'Local users' });
    expect(await within(table).findByText('disabled')).toBeInTheDocument();
    await press('Enable viewer');
    d = await dialog();
    expect(within(d).getByText(/stay ended/)).toBeInTheDocument();
    fireEvent.click(within(d).getByRole('button', { name: 'Enable account' }));
    await closed();
    const enabled = calls('PATCH', '/iam/users/viewer')[1]?.body as { disabled: boolean; resourceVersion: string };
    expect(enabled.disabled).toBe(false);
    // The second command names the resourceVersion the first one produced.
    expect(enabled.resourceVersion).not.toBe('1');
  });

  it('resets a password and deletes an account by exact review', async () => {
    identity();
    await press('Reset password of auditor');
    let d = await dialog();
    fireEvent.change(within(d).getByLabelText(/^New password/), { target: { value: 'short' } });
    fireEvent.change(within(d).getByLabelText(/^Confirm password/), { target: { value: 'short' } });
    fireEvent.click(within(d).getByRole('button', { name: 'Reset password' }));
    expect(await within(d).findByText('At least 8 characters')).toBeInTheDocument();
    fireEvent.change(within(d).getByLabelText(/^New password/), { target: { value: 'another-secret' } });
    fireEvent.change(within(d).getByLabelText(/^Confirm password/), { target: { value: 'another-secret' } });
    fireEvent.click(within(d).getByRole('button', { name: 'Reset password' }));
    await closed();
    expect(calls('POST', '/iam/users/auditor/password').map((c) => c.body)).toEqual([{ uid: 'mock-user-auditor', resourceVersion: '1', newPassword: '<14 chars>' }]);
    await waitFor(async () => expect(within(await screen.findByRole('table', { name: 'Local users' })).getAllByText('must change password').length).toBeGreaterThan(0));
    await press('Delete auditor');
    d = await dialog();
    expect(within(d).getByText(/inherits nothing/)).toBeInTheDocument();
    fireEvent.click(within(d).getByRole('button', { name: 'Delete account' }));
    await closed();
    const del = calls('DELETE', '/iam/users/auditor');
    expect(del).toHaveLength(1);
    expect((del[0]?.body as { uid: string }).uid).toBe('mock-user-auditor');
    await waitFor(async () => expect(within(await screen.findByRole('table', { name: 'Local users' })).queryByText('auditor')).toBeNull());
  });

  it('shows a stale review as refused and an unknown outcome as unknown, and never sends either again', async () => {
    const patches = count('PATCH', '/iam/users/viewer');
    mockDB().users.find((u) => u.username === 'viewer')!.resourceVersion = '99';
    identity();
    // The list still shows resourceVersion 99 here; make the stored one move after the read.
    await screen.findByRole('table', { name: 'Local users' });
    mockDB().users.find((u) => u.username === 'viewer')!.resourceVersion = '100';
    await press('Disable viewer');
    let d = await dialog();
    fireEvent.click(within(d).getByRole('button', { name: 'Disable account' }));
    expect(await within(d).findByText(/changed or already exists since you reviewed it/)).toBeInTheDocument();
    const again = within(d).getByRole('button', { name: 'Disable account' });
    expect(off(again)).toBe(true);
    fireEvent.submit(again.closest('form')!);
    await new Promise((r) => setTimeout(r, 30));
    expect(patches()).toBe(1);
    fireEvent.click(within(d).getAllByRole('button', { name: 'Close' }).at(-1)!);
    await closed();
    // An unclassified 503 unavailable (or the security-record limit): unknown, locked.
    mockIAM().failNext = { status: 503, key: 'unavailable', message: 'authentication operation unavailable' };
    await press('Disable viewer');
    d = await dialog();
    fireEvent.click(within(d).getByRole('button', { name: 'Disable account' }));
    expect(await within(d).findByText('Outcome unknown')).toBeInTheDocument();
    expect(within(d).getByText(/limit of revocation records, the server answers the same way and nothing changed/)).toBeInTheDocument();
    fireEvent.submit(within(d).getByRole('button', { name: 'Disable account' }).closest('form')!);
    await new Promise((r) => setTimeout(r, 30));
    expect(patches()).toBe(2);
  });

  it('keeps the built-in admin out of the commands', async () => {
    identity();
    const table = await screen.findByRole('table', { name: 'Local users' });
    await waitFor(() => expect(off(within(table).getByRole('button', { name: 'Edit viewer' }))).toBe(false));
    for (const name of ['Edit admin', 'Disable admin', 'Reset password of admin', 'Delete admin']) {
      const b = within(table).getByRole('button', { name });
      expect(off(b)).toBe(true);
    }
    fireEvent.mouseEnter(within(table).getByRole('button', { name: 'Delete admin' }));
    expect(await screen.findAllByText('The built-in admin is changed with kudurru admin on the host')).not.toHaveLength(0);
  });
});

describe('service accounts', () => {
  it('creates an account, then issues a token whose secret is shown once and kept nowhere else', async () => {
    const view = identity();
    await press('New service account');
    let d = await dialog();
    fireEvent.change(within(d).getByLabelText(/^Name(?!space)/), { target: { value: 'backup-robot' } });
    fireEvent.change(within(d).getByLabelText(/^Namespace/), { target: { value: 'apps' } });
    fireEvent.change(within(d).getByLabelText(/^Description/), { target: { value: 'Nightly backup' } });
    fireEvent.click(within(d).getByRole('button', { name: 'Create service account' }));
    await closed();
    expect(calls('POST', '/iam/serviceaccounts').map((c) => c.body)).toEqual([{ apiVersion: 'kudurru.io/v1alpha1', kind: 'ServiceAccount', metadata: { namespace: 'apps', name: 'backup-robot' }, spec: { description: 'Nightly backup' } }]);
    const sa = await screen.findByRole('table', { name: 'Service accounts' });
    expect(await within(sa).findByText('apps/backup-robot')).toBeInTheDocument();

    const reads = count('GET', '/iam/serviceaccounts/apps/backup-robot');
    await press('New token for apps/backup-robot');
    d = await reviewed();
    fireEvent.change(within(d).getByLabelText(/^Description/), { target: { value: 'nightly' } });
    pick(getSelector('Limit to role', d), 'Viewer');
    fireEvent.click(within(d).getByRole('button', { name: 'Issue token' }));
    const secret = await within(d).findByTestId('new-sa-token');
    expect(reads()).toBe(1);
    const issued = mockIAM().issued[0]!;
    expect(secret.textContent).toContain(issued);
    expect(within(d).getByText(/it is not shown again/)).toBeInTheDocument();
    const uid = mockIAM().serviceAccounts.find((s) => s.metadata.name === 'backup-robot')!.metadata.uid;
    expect(calls('POST', '/iam/serviceaccounts/apps/backup-robot/tokens').map((c) => c.body)).toEqual([{ uid, description: 'nightly', expiresIn: '720h', scopes: ['viewer'] }]);
    // Only the dialog holds it.
    expect(JSON.stringify(view.client.getQueryCache().getAll().map((q) => q.state.data))).not.toContain(issued);
    expect(view.client.getMutationCache().getAll()).toHaveLength(0);
    fireEvent.click(within(d).getByRole('button', { name: 'Done' }));
    await closed();
    expect(document.body.innerHTML).not.toContain(issued);
    await waitFor(async () => expect(within(await screen.findByRole('table', { name: 'Service accounts' })).getByText('apps/backup-robot').closest('tr')!.textContent).toContain('1'));
    // Opening the dialog again shows the form, not the old secret.
    await press('New token for apps/backup-robot');
    d = await reviewed();
    expect(await within(d).findByRole('button', { name: 'Issue token' })).toBeInTheDocument();
    expect(within(d).queryByTestId('new-sa-token')).toBeNull();
  });

  it('disables an account with the warning that its tokens are deleted, then refuses a token for it', async () => {
    mockDB().tokens.push({ id: 'g1', owner: { kind: 'ServiceAccount', name: 'grafana', namespace: 'default' }, createdAt: '2026-11-01T00:00:00Z', expiresAt: null });
    identity();
    await press('Edit default/grafana');
    const d = await reviewed();
    fireEvent.click(within(d).getByRole('switch', { name: /Disabled/ }));
    expect(within(d).getByText('Disabling deletes every token of the account')).toBeInTheDocument();
    fireEvent.click(within(d).getByRole('button', { name: 'Save changes' }));
    await closed();
    const put = calls('PUT', '/iam/serviceaccounts/default/grafana')[0]?.body as { uid: string; resourceVersion: string; serviceAccount: { metadata: Record<string, unknown>; spec: unknown } };
    expect(put.uid).toBe('sa-default-grafana');
    expect(put.resourceVersion).toBe('11');
    // The server-owned manager annotation is never sent back.
    expect(put.serviceAccount.metadata).toEqual({ namespace: 'default', name: 'grafana' });
    expect(put.serviceAccount.spec).toEqual({ description: 'dashboards', disabled: true });
    expect(mockDB().tokens.some((t) => t.owner.name === 'grafana' && t.owner.kind === 'ServiceAccount')).toBe(false);
    const table = await screen.findByRole('table', { name: 'Service accounts' });
    await waitFor(() => expect(off(within(table).getByRole('button', { name: 'New token for default/grafana' }))).toBe(true));
  });

  it('refuses an account of another manager before sending, and deletes an API account by its reviewed read', async () => {
    const mutations = count('PUT', /\/iam\/serviceaccounts\//);
    identity();
    await press('Edit kudurru/external-dns');
    let d = await reviewed();
    expect(within(d).getByText('Managed elsewhere')).toBeInTheDocument();
    expect(within(d).getByText(/file:objects.d\/external-dns.yaml/)).toBeInTheDocument();
    fireEvent.click(within(d).getAllByRole('button', { name: 'Close' }).at(-1)!);
    await closed();
    expect(mutations()).toBe(0);
    await press('Delete default/grafana');
    d = await reviewed();
    expect(await within(d).findByText(/tokens stay refused after a restart/)).toBeInTheDocument();
    fireEvent.click(within(d).getByRole('button', { name: 'Delete service account' }));
    await closed();
    expect(calls('DELETE', '/iam/serviceaccounts/default/grafana').map((c) => c.body)).toEqual([{ uid: 'sa-default-grafana', resourceVersion: '11' }]);
  });

  it('needs tokens.write as well as serviceaccounts.write to issue a token', async () => {
    meAs({ users: ['read'], serviceaccounts: ['read', 'write', 'delete'], rolebindings: ['read'], identityproviders: ['read'] });
    identity();
    const table = await screen.findByRole('table', { name: 'Service accounts' });
    await waitFor(() => expect(off(within(table).getByRole('button', { name: 'Edit default/grafana' }))).toBe(false));
    expect(off(within(table).getByRole('button', { name: 'New token for default/grafana' }))).toBe(true);
  });
});

describe('identity providers', () => {
  it('creates a provider with a client secret reference, never a value', async () => {
    identity();
    await press('New identity provider');
    const d = await dialog();
    fireEvent.change(within(d).getByLabelText(/^Provider ID/), { target: { value: 'partner' } });
    fireEvent.change(within(d).getByLabelText(/^Display name/), { target: { value: 'Partner SSO' } });
    fireEvent.change(within(d).getByLabelText(/^Issuer/), { target: { value: 'http://partner.example' } });
    fireEvent.change(within(d).getByLabelText(/^Client ID/), { target: { value: 'kudurru' } });
    fireEvent.change(within(d).getByLabelText(/^Secret environment variable/), { target: { value: 'not a name' } });
    fireEvent.click(within(d).getByRole('button', { name: 'Create provider' }));
    expect(await within(d).findByText(/https:\/\/ issuer URL/)).toBeInTheDocument();
    expect(within(d).getByText(/An environment variable name/)).toBeInTheDocument();
    expect(calls('POST', '/iam/identityproviders')).toHaveLength(0);
    fireEvent.change(within(d).getByLabelText(/^Issuer/), { target: { value: 'https://partner.example' } });
    pick(getSelector('Client secret from', d), 'File');
    fireEvent.change(within(d).getByLabelText(/^Secret file path/), { target: { value: '/run/secrets/partner' } });
    fireEvent.click(within(d).getByRole('button', { name: 'Create provider' }));
    await closed();
    expect(calls('POST', '/iam/identityproviders').map((c) => c.body)).toEqual([
      {
        apiVersion: 'kudurru.io/v1alpha1', kind: 'IdentityProvider', metadata: { name: 'partner' },
        spec: { issuer: 'https://partner.example', clientID: 'kudurru', clientSecretRef: { file: '/run/secrets/partner' }, displayName: 'Partner SSO', scopes: ['openid', 'email', 'profile'] },
      },
    ]);
    expect(await within(await screen.findByRole('table', { name: 'Identity providers' })).findByText('Partner SSO')).toBeInTheDocument();
  });

  it('replaces a provider keeping the fields the form does not edit, and leaves kudurru.yaml providers alone', async () => {
    identity();
    const table = await screen.findByRole('table', { name: 'Identity providers' });
    await waitFor(() => expect(off(within(table).getByRole('button', { name: 'Edit github' }))).toBe(false));
    expect(off(within(table).getByRole('button', { name: 'Edit dex' }))).toBe(true);
    expect(off(within(table).getByRole('button', { name: 'Delete dex' }))).toBe(true);
    fireEvent.click(within(table).getByRole('button', { name: 'Edit github' }));
    const d = await reviewed();
    const client = within(d).getByLabelText(/^Client ID/);
    expect(within(d).getByLabelText(/^Secret environment variable/)).toHaveValue('GITHUB_CLIENT_SECRET');
    expect(within(d).getByText('Saving signs out this provider’s users')).toBeInTheDocument();
    fireEvent.change(client, { target: { value: 'kudurru-gh-2' } });
    fireEvent.click(within(d).getByRole('button', { name: 'Save changes' }));
    await closed();
    const put = calls('PUT', '/iam/identityproviders/github')[0]?.body as { uid: string; resourceVersion: string; identityProvider: { metadata: unknown; spec: Record<string, unknown> } };
    expect(put.uid).toBe('idp-github');
    expect(put.resourceVersion).toBe('21');
    expect(put.identityProvider.metadata).toEqual({ name: 'github', labels: { team: 'platform' } });
    expect(put.identityProvider.spec).toMatchObject({ clientID: 'kudurru-gh-2', clientSecretRef: { env: 'GITHUB_CLIENT_SECRET' }, claimMappings: { username: { claim: 'sub', prefix: 'gh:' } }, provisioning: { autoProvision: 'never' } });
  });

  it('deletes a provider by its reviewed read', async () => {
    identity();
    await press('Delete github');
    const d = await reviewed();
    expect(await within(d).findByText(/ends every session and token that came from it/)).toBeInTheDocument();
    fireEvent.click(within(d).getByRole('button', { name: 'Delete provider' }));
    await closed();
    expect(calls('DELETE', '/iam/identityproviders/github').map((c) => c.body)).toEqual([{ uid: 'idp-github', resourceVersion: '21' }]);
  });

  it('validates a draft field by field', () => {
    const ok = { id: 'x', displayName: '', issuer: 'https://i', clientID: 'c', cliClientID: '', redirectURL: '', scopes: [], secretSource: 'env' as const, secretName: 'X' };
    expect(providerErrorsFor(ok, true)).toEqual({});
    expect(Object.keys(providerErrorsFor({ ...ok, id: 'Bad_ID' }, true))).toEqual(['id']);
    expect(Object.keys(providerErrorsFor({ ...ok, id: 'Bad_ID' }, false))).toEqual([]);
    expect(Object.keys(providerErrorsFor({ ...ok, secretSource: 'file', secretName: 'relative' }, true))).toEqual(['secretName']);
  });
});

describe('role bindings', () => {
  it('edits and deletes an API-owned binding by its reviewed read and leaves file-managed ones alone', async () => {
    identity();
    const table = await screen.findByRole('table', { name: 'Role bindings' });
    await waitFor(() => expect(off(within(table).getByRole('button', { name: 'Edit default/home-admin' }))).toBe(false));
    expect(off(within(table).getByRole('button', { name: 'Edit default/viewers' }))).toBe(true);
    expect(within(table).queryByRole('button', { name: /rbac\.bindings/ })).toBeNull();
    fireEvent.click(within(table).getByRole('button', { name: 'Edit default/home-admin' }));
    let d = await reviewed();
    const user = within(d).getByLabelText(/^User name/);
    expect(within(d).getByLabelText(/^Name(?!space)/)).toBeDisabled();
    fireEvent.change(user, { target: { value: 'alice' } });
    fireEvent.click(within(d).getByRole('button', { name: 'Save changes' }));
    await closed();
    const put = calls('PUT', '/iam/rolebindings/default/home-admin')[0]?.body as { uid: string; resourceVersion: string; binding: { spec: unknown } };
    expect(put.uid).toBe('rb-default-home-admin');
    expect(put.resourceVersion).toBe('7');
    expect(put.binding.spec).toEqual({ role: 'zone-admin', scope: { kind: 'zone', name: 'home-arpa' }, subjects: [{ kind: 'User', name: 'alice' }] });
    await press('Delete default/home-admin');
    d = await reviewed();
    fireEvent.click(await within(d).findByRole('button', { name: 'Delete role binding' }));
    await closed();
    expect(calls('DELETE', '/iam/rolebindings/default/home-admin')).toHaveLength(1);
  });

  it('keeps the stored labels and operator annotations on an edit, never the manager', async () => {
    const home = mockIAM().roleBindings.find((b) => b.metadata.name === 'home-admin')!;
    home.metadata.labels = { team: 'dns' };
    home.metadata.annotations = { ...home.metadata.annotations, 'example.com/ticket': 'OPS-12' };
    identity();
    await press('Edit default/home-admin');
    const d = await reviewed();
    const save = within(d).getByRole('button', { name: 'Save changes' });
    expect(off(save)).toBe(true);
    fireEvent.change(within(d).getByLabelText(/^User name/), { target: { value: 'alice' } });
    fireEvent.click(save);
    await closed();
    const put = calls('PUT', '/iam/rolebindings/default/home-admin')[0]?.body as { binding: { metadata: unknown } };
    expect(put.binding.metadata).toEqual({ namespace: 'default', name: 'home-admin', labels: { team: 'dns' }, annotations: { 'example.com/ticket': 'OPS-12' } });
  });

  it('does not edit a binding whose scope kind the form does not know', async () => {
    mockIAM().roleBindings.push({
      apiVersion: 'kudurru.io/v1alpha1', kind: 'RoleBinding',
      metadata: { namespace: 'default', name: 'kids', uid: 'rb-default-kids', resourceVersion: '9', annotations: { 'kudurru.io/managed-by': 'api' } },
      spec: { role: 'admin', scope: { kind: 'clientgroup', name: 'kids' }, subjects: [{ kind: 'User', name: 'parent' }] },
    });
    identity();
    const table = await screen.findByRole('table', { name: 'Role bindings' });
    await waitFor(() => expect(off(within(table).getByRole('button', { name: 'Delete default/kids' }))).toBe(false));
    expect(off(within(table).getByRole('button', { name: 'Edit default/kids' }))).toBe(true);
    // The list said zone, the reviewed read says clientgroup: the dialog refuses.
    const home = mockIAM().roleBindings.find((b) => b.metadata.name === 'home-admin')!;
    await waitFor(() => expect(off(within(table).getByRole('button', { name: 'Edit default/home-admin' }))).toBe(false));
    home.spec.scope = { kind: 'clientgroup', name: 'kids' };
    fireEvent.click(within(table).getByRole('button', { name: 'Edit default/home-admin' }));
    const d = await reviewed();
    expect(within(d).getByText('Scope this form does not edit')).toBeInTheDocument();
    expect(within(d).queryByRole('button', { name: 'Save changes' })).toBeNull();
    expect(calls('PUT', '/iam/rolebindings/default/home-admin')).toHaveLength(0);
  });

  it('locks an edit after a refusal and after an unknown outcome, each sent once', async () => {
    const puts = count('PUT', '/iam/rolebindings/default/home-admin');
    identity();
    mockIAM().failNext = { status: 409, key: 'conflict', message: 'resourceVersion changed' };
    await press('Edit default/home-admin');
    let d = await reviewed();
    fireEvent.change(within(d).getByLabelText(/^User name/), { target: { value: 'alice' } });
    fireEvent.click(within(d).getByRole('button', { name: 'Save changes' }));
    expect(await within(d).findByText(/changed or already exists since you reviewed it/)).toBeInTheDocument();
    expect(off(within(d).getByRole('button', { name: 'Save changes' }))).toBe(true);
    fireEvent.submit(within(d).getByRole('button', { name: 'Save changes' }).closest('form')!);
    await new Promise((r) => setTimeout(r, 30));
    expect(puts()).toBe(1);
    fireEvent.click(within(d).getAllByRole('button', { name: 'Close' }).at(-1)!);
    await closed();
    mockIAM().failNext = { status: 503, key: 'unavailable', message: 'authentication operation unavailable' };
    await press('Edit default/home-admin');
    d = await reviewed();
    fireEvent.change(within(d).getByLabelText(/^User name/), { target: { value: 'alice' } });
    fireEvent.click(within(d).getByRole('button', { name: 'Save changes' }));
    expect(await within(d).findByText('Outcome unknown')).toBeInTheDocument();
    fireEvent.submit(within(d).getByRole('button', { name: 'Save changes' }).closest('form')!);
    await new Promise((r) => setTimeout(r, 30));
    expect(puts()).toBe(2);
  });
});

describe('refusals', () => {
  const writeButtons = ['New user', 'New service account', 'New role binding', 'New identity provider'];

  async function allOff(reason: RegExp) {
    identity();
    for (const name of writeButtons) {
      const b = await screen.findByRole('button', { name });
      await waitFor(() => expect(off(b)).toBe(true));
    }
    for (const name of ['Edit viewer', 'Delete viewer', 'Edit default/grafana', 'New token for default/grafana', 'Edit github', 'Edit default/home-admin']) expect(off(screen.getByRole('button', { name }))).toBe(true);
    fireEvent.mouseEnter(screen.getByRole('button', { name: 'New user' }));
    expect(await screen.findAllByText(reason)).not.toHaveLength(0);
  }

  it('disables every writer on the Kubernetes driver with the reason', async () => {
    mockDB().authCapabilities = { factorManagement: false, localUserCommands: false, iamObjectCommands: false };
    await allOff(/Kubernetes driver/);
    expect(screen.getByText(/does not change identity through the API/)).toBeInTheDocument();
  });

  it('disables every writer on a follower, naming the primary', async () => {
    asFollower();
    await allOff(/primary/i);
  });

  it('disables every writer in static mode', async () => {
    server.use(http.get(`${API}/cluster`, () => HttpResponse.json({ readOnly: true })));
    await allOff(/Read-only in static mode/);
    expect(screen.getByText(/static mode: identity is read-only here/)).toBeInTheDocument();
  });

  it('disables every writer without the permission', async () => {
    meAs({ users: ['read'], serviceaccounts: ['read'], rolebindings: ['read'], identityproviders: ['read'] });
    await allOff(/Needs users.write/);
  });

  it('shows the driver refusal of a command as a refusal, not an unknown outcome', async () => {
    mockIAM().failNext = { status: 503, key: 'iam_management_unavailable', message: 'IAM object commands are unavailable on this driver', hint: 'change ServiceAccounts with kubectl' };
    identity();
    await press('New service account');
    const d = await dialog();
    fireEvent.change(within(d).getByLabelText(/^Name(?!space)/), { target: { value: 'x' } });
    fireEvent.click(within(d).getByRole('button', { name: 'Create service account' }));
    expect(await within(d).findByText(/use kubectl or your GitOps tool/)).toBeInTheDocument();
    expect(within(d).queryByText('Outcome unknown')).toBeNull();
  });

  it('classifies answers', () => {
    expect(iamOutcomeUnknown(new ApiError(503, 'unavailable', 'authentication operation unavailable'))).toBe(true);
    expect(iamOutcomeUnknown(new ApiError(500, 'internal', 'x'))).toBe(true);
    expect(iamOutcomeUnknown(new ApiError(0, 'network', 'x'))).toBe(true);
    for (const key of ['primary_required', 'read_only', 'user_management_unavailable', 'iam_management_unavailable', 'generation_pending']) expect(iamOutcomeUnknown(new ApiError(503, key, 'x'))).toBe(false);
    expect(iamOutcomeUnknown(new ApiError(409, 'conflict', 'x'))).toBe(false);
    expect(iamOutcomeUnknown(new ApiError(403, 'forbidden', 'x'))).toBe(false);
  });
});
