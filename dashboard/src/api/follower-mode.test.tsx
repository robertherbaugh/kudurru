// An enrolled SQLite follower (D368): the role banner, gated primary-only
// controls with the reason, refusal texts, the Nodes page, the login page and
// sign-out, and the delayed refetch after a forwarded change.
import { act, fireEvent, renderHook, screen, waitFor, within } from '@testing-library/react';
import { QueryClient, QueryClientProvider, QueryObserver } from '@tanstack/react-query';
import { http, HttpResponse } from 'msw';
import fs from 'node:fs';
import path from 'node:path';
import type { ReactNode } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { makeQueryClient } from '../App';
import { ApiError } from './client';
import { ApiProvider, FOLLOWER_SYNC_MS, qk, scheduleFollowerRefetch, useCan, useCannotReason, useClusterRole, usePrimaryOnly, useSignOut } from './context';
import type { ClusterInfo } from './types';
import { ErrorBanner, errorText } from '../components/Feedback';
import { FollowerBanner } from '../components/FollowerBanner';
import { AuthenticatorPanel } from '../components/access/Authenticators';
import { server } from '../test/setup';
import { API, renderWithApp } from '../test/utils';
import AllowlistPage from '../pages/AllowlistPage';
import ListsPage from '../pages/ListsPage';
import { LoginPage } from '../pages/LoginPage';
import NodesPage from '../pages/NodesPage';

/** The follower lists exactly as the OpenAPI document classifies them. */
function documentLists(): { forwarded: string[]; local: string[] } {
  const doc = JSON.parse(fs.readFileSync(path.resolve(process.cwd(), '../api/openapi.json'), 'utf8')) as { paths: Record<string, Record<string, { operationId?: string; 'x-kudurru-follower'?: string }>> };
  const forwarded: string[] = [];
  const local: string[] = [];
  for (const ops of Object.values(doc.paths)) {
    for (const op of Object.values(ops)) {
      if (op.operationId && op['x-kudurru-follower'] === 'forwarded') forwarded.push(op.operationId);
      if (op.operationId && op['x-kudurru-follower'] === 'local') local.push(op.operationId);
    }
  }
  return { forwarded, local };
}

function followerInfo(): ClusterInfo {
  const { forwarded, local } = documentLists();
  return {
    clusterID: 'dc493171-2dd3-4282-b0ab-c1a0a31ea26f', driver: 'sqlite', epoch: 7, leader: 'kudurru-0', nodes: 2, readOnly: false, node: 'kudurru-1',
    role: 'follower', primary: 'kudurru-0', primaryRequired: true, forwardedOperations: forwarded, localOperations: local, took_ms: 1,
  };
}

const PRIMARY_REQUIRED = {
  error: {
    key: 'primary_required',
    message: 'this request runs only on the primary; this node is an enrolled follower of kudurru-0 and does not forward it',
    hint: 'send it to the API of the primary kudurru-0; GET /api/v1/cluster on this node names the operations a follower forwards or runs itself',
  },
};

function asFollower() {
  server.use(http.get(`${API}/cluster`, () => HttpResponse.json(followerInfo())));
}

function wrapper(queryClient: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={queryClient}><ApiProvider baseUrl={API}>{children}</ApiProvider></QueryClientProvider>;
  };
}

afterEach(() => {
  vi.useRealTimers();
});

describe('enrolled follower capabilities', () => {
  it('refuses primary-only permissions and operations with the primary named, and keeps forwarded and local ones', async () => {
    asFollower();
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    queryClient.setQueryData(qk.me, { principal: { name: 'admin' }, permissions: { '*': ['*'] } });
    const hook = renderHook(() => ({ role: useClusterRole(), can: useCan(), cannot: useCannotReason(), primaryOnly: usePrimaryOnly() }), { wrapper: wrapper(queryClient) });
    await waitFor(() => expect(hook.result.current.role.follower).toBe(true));
    const { role, can, cannot, primaryOnly } = hook.result.current;
    expect(role).toMatchObject({ role: 'follower', primary: 'kudurru-0' });
    for (const [resource, verb] of [['users', 'write'], ['unblockrequests', 'decide'], ['unblockrequests', 'delete'], ['blocklists', 'refresh'], ['zones', 'transfer'], ['node', 'config']] as const) {
      expect(can(resource, verb), `${resource}.${verb}`).toBe(false);
      expect(cannot(resource, verb)).toBe('Runs only on the primary, kudurru-0: this node is a follower and does not forward it');
    }
    for (const [resource, verb] of [['zones', 'write'], ['records', 'write'], ['blocklists', 'write'], ['settings', 'write'], ['blocking', 'toggle'], ['cache', 'flush'], ['dhcp', 'write'], ['zones', 'read']] as const) {
      expect(can(resource, verb), `${resource}.${verb}`).toBe(true);
      expect(cannot(resource, verb)).toBeUndefined();
    }
    expect(primaryOnly('createAllowlistEntries')).toContain('kudurru-0');
    expect(primaryOnly('patchSourceEntries')).toContain('kudurru-0');
    expect(primaryOnly('previewMigration')).toContain('kudurru-0');
    expect(primaryOnly('createBlocklists')).toBeUndefined();
    expect(primaryOnly('patchSettings')).toBeUndefined();
    expect(primaryOnly('flushCache')).toBeUndefined();
    hook.unmount();
    queryClient.clear();
  });

  it('gates nothing on a primary', async () => {
    server.use(http.get(`${API}/cluster`, () => HttpResponse.json({ ...followerInfo(), node: 'kudurru-0', role: 'primary', primaryRequired: false, forwardedOperations: undefined, localOperations: undefined })));
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    queryClient.setQueryData(qk.me, { principal: { name: 'admin' }, permissions: { '*': ['*'] } });
    const hook = renderHook(() => ({ role: useClusterRole(), can: useCan(), primaryOnly: usePrimaryOnly() }), { wrapper: wrapper(queryClient) });
    await waitFor(() => expect(hook.result.current.role.role).toBe('primary'));
    expect(hook.result.current.role.follower).toBe(false);
    expect(hook.result.current.can('users', 'write')).toBe(true);
    expect(hook.result.current.primaryOnly('createAllowlistEntries')).toBeUndefined();
    hook.unmount();
    queryClient.clear();
  });

  it('shows the follower banner naming the primary, and none on a standalone node', async () => {
    asFollower();
    const first = renderWithApp(<FollowerBanner />);
    const banner = await screen.findByTestId('follower-banner');
    expect(within(banner).getByText('This node is a follower of kudurru-0')).toBeInTheDocument();
    expect(within(banner).getByText(/are sent to kudurru-0 and show here after the next sync/)).toBeInTheDocument();
    expect(within(banner).getByRole('link', { name: 'Cluster details' })).toHaveAttribute('href', '/nodes');
    first.unmount();
    server.resetHandlers();
    const second = renderWithApp(<FollowerBanner />);
    await waitFor(() => expect(second.client.getQueryData(qk.cluster)).toBeDefined());
    expect(screen.queryByTestId('follower-banner')).toBeNull();
  });

  it('disables list refresh and allow/deny entries with the reason, and keeps list writes', async () => {
    asFollower();
    const lists = renderWithApp(<ListsPage />, { route: '/lists' });
    const row = await screen.findByTestId('row-oisd-small');
    const refresh = within(row).getByRole('button', { name: 'Refresh' });
    await waitFor(() => expect(refresh).toHaveAttribute('aria-disabled', 'true'));
    expect(refresh).toHaveAccessibleDescription(/Runs only on the primary, kudurru-0/);
    const add = screen.getByRole('button', { name: 'Add domains' });
    expect(add).toHaveAttribute('aria-disabled', 'true');
    expect(add).toHaveAccessibleDescription(/Runs only on the primary, kudurru-0/);
    // Creating a list is forwarded, so it stays available.
    expect(screen.getByRole('button', { name: /New list/ })).not.toHaveAttribute('aria-disabled', 'true');
    lists.unmount();

    renderWithApp(<AllowlistPage />, { route: '/allowlist' });
    const sw = within(await screen.findByTestId('entry-s.youtube.com')).getByRole('switch');
    await waitFor(() => expect(sw).toHaveAttribute('aria-disabled', 'true'));
    expect(sw).toHaveAccessibleDescription(/Runs only on the primary, kudurru-0/);
    expect(screen.getByRole('button', { name: 'Add to the Allowlist' })).toHaveAttribute('aria-disabled', 'true');
  });

  it('explains that authenticator changes need the primary', async () => {
    asFollower();
    renderWithApp(<AuthenticatorPanel />);
    expect(await screen.findByText('Authenticator changes run only on the primary')).toBeInTheDocument();
    expect(screen.getByText(/Sign in on kudurru-0 to set up or remove your authenticator/)).toBeInTheDocument();
  });

  it('shows the role, the primary and what the follower forwards on the Nodes page', async () => {
    asFollower();
    renderWithApp(<NodesPage />, { route: '/nodes' });
    const panel = (await screen.findByRole('heading', { name: 'Cluster' })).closest('section') ?? document.body;
    await within(panel).findByText('This node');
    expect(within(panel).getAllByText('follower').length).toBeGreaterThan(0);
    expect(within(panel).getByText('forwarded to kudurru-0')).toBeInTheDocument();
    expect(within(panel).getByText(/kinds of change are forwarded to kudurru-0 and \d+ run on this node; every other change answers primary_required/)).toBeInTheDocument();
    expect(screen.getByText('primary kudurru-0')).toBeInTheDocument();
    expect(screen.getByText('this node: follower')).toBeInTheDocument();
  });
});

describe('follower refusals', () => {
  it('says where a primary_required change is made, and keeps a specific hint', () => {
    const err = new ApiError(503, 'primary_required', PRIMARY_REQUIRED.error.message, PRIMARY_REQUIRED.error.hint);
    expect(errorText(err)).toBe('Only the primary, kudurru-0, can make this change: this node is a follower and does not forward it. Open the dashboard on kudurru-0 to make it there.');
    const node = new ApiError(503, 'primary_required', PRIMARY_REQUIRED.error.message, 'a follower cannot store its own log settings, and the primary changes only its own; set log.level and log.format in this node\'s configuration file or environment');
    expect(errorText(node)).toBe('Only the primary, kudurru-0, can make this change: this node is a follower and does not forward it. A follower cannot store its own log settings, and the primary changes only its own; set log.level and log.format in this node\'s configuration file or environment.');
    renderWithApp(<ErrorBanner error={err} />, { user: null });
    expect(screen.getByText('Needs the primary')).toBeInTheDocument();
  });

  it('tells the user to check before repeating a change whose outcome is unknown', () => {
    const err = new ApiError(503, 'completion_unknown', "the forwarded write's outcome is unknown; inspect the object before a new decision; do not replay this request");
    expect(errorText(err)).toBe("The forwarded write's outcome is unknown; inspect the object before a new decision; do not replay this request. Reload to check what is in effect before you try again.");
    renderWithApp(<ErrorBanner error={err} />, { user: null });
    expect(screen.getByText('Outcome unknown')).toBeInTheDocument();
  });

  it('keeps an unconfirmed sign-out on the login page and clears it after the next sign-in', async () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    queryClient.setQueryData(qk.me, { principal: { name: 'original' } });
    server.use(http.post(`${API}/auth/logout/browser`, () => HttpResponse.json({ error: {
      key: 'completion_unknown',
      message: "the sign-out's outcome is unknown; the session may stay valid until it expires; this browser's session cookie was cleared; do not replay this request",
      hint: 'sign in and review the session list; revoke the session there if it is still listed',
    } }, { status: 503 })));
    const hook = renderHook(() => useSignOut(), { wrapper: wrapper(queryClient) });
    await act(async () => { await hook.result.current(); });
    expect(queryClient.getQueryData(qk.me)).toBeNull();
    const notice = queryClient.getQueryData<string>(qk.signOutNotice);
    expect(notice).toMatch(/^The sign-out's outcome is unknown; the session may stay valid until it expires/);
    expect(notice).toContain('revoke the session there if it is still listed');
    hook.unmount();

    renderWithApp(<LoginPage />, { user: null, client: queryClient });
    expect(await screen.findByText('Sign-out not confirmed')).toBeInTheDocument();
    server.use(http.post(`${API}/auth/login`, () => HttpResponse.json({ principal: { kind: 'User', name: 'admin', id: 'local:admin' }, roles: ['admin'], permissions: { '*': ['*'] }, csrf: 'c' })));
    fireEvent.change(screen.getByLabelText(/^Username/), { target: { value: 'admin' } });
    fireEvent.change(screen.getByLabelText(/^Password/), { target: { value: 'test-password' } });
    fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));
    await waitFor(() => expect(queryClient.getQueryData(qk.signOutNotice)).toBeNull());
  });

  it('adds the session warning to a refusal that does not mention it', async () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    queryClient.setQueryData(qk.me, { principal: { name: 'original' } });
    server.use(http.post(`${API}/auth/logout/browser`, () => HttpResponse.json({ error: { key: 'unavailable', message: 'Unavailable' } }, { status: 503 })));
    const hook = renderHook(() => useSignOut(), { wrapper: wrapper(queryClient) });
    await act(async () => { await hook.result.current(); });
    expect(queryClient.getQueryData(qk.signOutNotice)).toBe('Unavailable. The dashboard signed out, but the server did not confirm that the session ended; it may stay valid until it expires.');
    hook.unmount();
  });

  it('explains on the login page of a follower why single sign-on is absent', async () => {
    server.use(http.get(`${API}/auth/status`, () => HttpResponse.json({ localLogin: true, localLoginMode: 'enabled', providers: [], hasDefaultCredentials: false, role: 'follower', primary: 'kudurru-0' })));
    renderWithApp(<LoginPage />, { user: null });
    expect(await screen.findByText('Single sign-on runs on the primary')).toBeInTheDocument();
    expect(screen.getByText(/open the dashboard on kudurru-0/)).toBeInTheDocument();
  });
});

describe('read after a forwarded change', () => {
  it('refetches once more after the follower’s next pull, and never on a primary', async () => {
    vi.useFakeTimers();
    const client = makeQueryClient();
    const reads = vi.fn(() => Promise.resolve('value'));
    // An active query (one observer) is refetched by an invalidation.
    const watch = new QueryObserver(client, { queryKey: ['objects', 'x'], queryFn: reads });
    const stop = watch.subscribe(() => undefined);
    await vi.waitFor(() => expect(reads).toHaveBeenCalledTimes(1));

    client.setQueryData(qk.cluster, { ...followerInfo(), primaryRequired: false });
    scheduleFollowerRefetch(client);
    await vi.advanceTimersByTimeAsync(FOLLOWER_SYNC_MS + 100);
    expect(reads).toHaveBeenCalledTimes(1);

    client.setQueryData(qk.cluster, followerInfo());
    await client.getMutationCache().build(client, { mutationFn: () => Promise.resolve('ok') }).execute(undefined);
    await vi.advanceTimersByTimeAsync(FOLLOWER_SYNC_MS - 100);
    expect(reads).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(200);
    expect(reads).toHaveBeenCalledTimes(2);
    stop();
    client.clear();
  });
});
