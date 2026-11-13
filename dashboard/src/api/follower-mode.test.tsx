// An enrolled SQLite follower (D368): the role banner, gated primary-only
// controls with the reason, refusal texts, the Nodes page, the login page and
// sign-out, and the delayed refetch after a forwarded change.
import { act, fireEvent, renderHook, screen, waitFor, within } from '@testing-library/react';
import { QueryClient, QueryClientProvider, useQuery as useQueryHook } from '@tanstack/react-query';
import { http, HttpResponse } from 'msw';
import fs from 'node:fs';
import path from 'node:path';
import type { ReactNode } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from './client';
import { ApiProvider, FOLLOWER_SYNC_MS, qk, ROLE_POLL_MS, useApiContext, useCan, useCannotReason, useClusterRole, usePrimaryOnly, useSession, useSignOut } from './context';
import type { ClusterInfo } from './types';
import { ErrorBanner, errorText } from '../components/Feedback';
import { FollowerBanner } from '../components/FollowerBanner';
import { AuthenticatorPanel, RecoverAuthenticatorButton } from '../components/access/Authenticators';
import { ExternalUsersPanel } from '../components/access/ExternalUsers';
import { MigrationHistory } from '../components/wizards/MigrationHistory';
import { MigrationImport } from '../components/wizards/MigrationWizard';
import { PolicyWizardDialog } from '../components/wizards/PolicyWizard';
import { mockDB } from '../mocks/handlers';
import { getSelector, pick } from '../test/controls';
import { server } from '../test/setup';
import { API, renderWithApp } from '../test/utils';
import AllowlistPage from '../pages/AllowlistPage';
import ClientDetailPage from '../pages/ClientDetailPage';
import MigrationRunPage from '../pages/MigrationRunPage';
import QueryLogPage from '../pages/QueryLogPage';
import ListDetailPage from '../pages/ListDetailPage';
import ListsPage from '../pages/ListsPage';
import { LoginPage } from '../pages/LoginPage';
import NodesPage from '../pages/NodesPage';
import PoliciesPage from '../pages/PoliciesPage';
import RequestDetailPage from '../pages/RequestDetailPage';
import RequestsPage from '../pages/RequestsPage';
import SettingsPage from '../pages/SettingsPage';
import ZoneDetailPage from '../pages/ZoneDetailPage';

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

/** The built-in operator role: blocklists.* and unblockrequests.*, but no
 * cluster.read, so GET /cluster answers 403 (internal/rbac/permissions.yaml). */
const OPERATOR_PERMISSIONS = {
  blocklists: ['read', 'write', 'delete', 'refresh'], unblockrequests: ['read', 'decide', 'delete'], edgeblocks: ['read', 'write'],
  zones: ['read'], records: ['read'], policies: ['read'], clientgroups: ['read'], querylog: ['read'], stats: ['read'], settings: ['read'],
};

/** An operator session on a follower: /cluster is forbidden, the public
 * /auth/status names the role and the primary. Returns the /cluster reads. */
function asOperatorOnFollower(): { clusterReads: () => number; statusReads: () => number } {
  let cluster = 0;
  let status = 0;
  server.use(
    http.get(`${API}/auth/me`, () => HttpResponse.json({ principal: { kind: 'User', id: 'local:ops', name: 'ops', auth: 'cookie', provider: 'local' }, roles: ['operator'], csrf: 'csrf-mock-token', permissions: OPERATOR_PERMISSIONS })),
    http.get(`${API}/cluster`, () => {
      cluster++;
      return HttpResponse.json({ error: { key: 'forbidden', message: 'You may not read cluster', hint: { missing: { resource: 'cluster', verb: 'read', scope: 'cluster' } } } }, { status: 403 });
    }),
    http.get(`${API}/auth/status`, () => {
      status++;
      return HttpResponse.json({ localLogin: true, localLoginMode: 'enabled', providers: [], hasDefaultCredentials: false, role: 'follower', primary: 'kudurru-0', capabilities: { factorManagement: true, localUserCommands: true } });
    }),
  );
  return { clusterReads: () => cluster, statusReads: () => status };
}

/** Counts GETs of an API path while the test runs. */
function countReads(pathname: string): () => number {
  let n = 0;
  const listener = ({ request }: { request: Request }) => {
    if (request.method === 'GET' && new URL(request.url).pathname === `/api/v1${pathname}`) n++;
  };
  server.events.on('request:start', listener);
  afterEachCleanups.push(() => server.events.removeListener('request:start', listener));
  return () => n;
}

const afterEachCleanups: (() => void)[] = [];

function wrapper(queryClient: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={queryClient}><ApiProvider baseUrl={API}>{children}</ApiProvider></QueryClientProvider>;
  };
}

afterEach(() => {
  vi.useRealTimers();
  for (const f of afterEachCleanups.splice(0)) f();
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

  it('keeps the session when the sign-out never reached the server, and rejects', async () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const me = { principal: { name: 'original' } };
    queryClient.setQueryData(qk.me, me);
    server.use(http.post(`${API}/auth/logout/browser`, () => HttpResponse.error()));
    const hook = renderHook(() => useSignOut(), { wrapper: wrapper(queryClient) });
    let rejected: unknown;
    await act(async () => { await hook.result.current().catch((err: unknown) => { rejected = err; }); });
    expect(rejected).toBeInstanceOf(ApiError);
    expect((rejected as ApiError).status).toBe(0);
    // Nothing cleared the cookie, so the dashboard does not pretend it signed out.
    expect(queryClient.getQueryData(qk.me)).toBe(me);
    expect(queryClient.getQueryData(qk.signOutNotice)).toBeUndefined();
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
  /** A hook harness: the session, an active query and the API client. */
  function harness(queryClient: QueryClient, reads: () => Promise<string>) {
    queryClient.setQueryData(qk.me, { principal: { name: 'admin' }, permissions: { '*': ['*'] } });
    return renderHook(() => ({ ctx: useApiContext(), session: useSession(), role: useClusterRole(), objects: useQueryObserver(reads) }), { wrapper: wrapper(queryClient) });
  }

  it('refetches once more after the follower’s next pull for a forwarded change the client sends, and never on a primary', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    server.use(http.put(`${API}/objects/policies/default/probe`, () => new HttpResponse(null, { status: 204 })));
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
    const reads = vi.fn(() => Promise.resolve('value'));
    // The first answer is a primary: a write schedules nothing.
    server.use(http.get(`${API}/cluster`, () => HttpResponse.json({ ...followerInfo(), role: 'primary', primaryRequired: false, forwardedOperations: undefined, localOperations: undefined })));
    const hook = harness(queryClient, reads);
    await vi.waitFor(() => expect(hook.result.current.role.role).toBe('primary'));
    await vi.waitFor(() => expect(reads).toHaveBeenCalledTimes(1));
    // A plain await of the client, the way the wizards write (not useMutation).
    await act(async () => { await hook.result.current.ctx.client.request('PUT', '/objects/policies/default/probe', { body: {} }); });
    await act(async () => { await vi.advanceTimersByTimeAsync(FOLLOWER_SYNC_MS + 100); });
    expect(reads).toHaveBeenCalledTimes(1);

    queryClient.setQueryData(qk.cluster, followerInfo());
    await act(async () => { await hook.result.current.ctx.client.request('PUT', '/objects/policies/default/probe', { body: {} }); });
    await act(async () => { await vi.advanceTimersByTimeAsync(FOLLOWER_SYNC_MS - 100); });
    expect(reads).toHaveBeenCalledTimes(1);
    await act(async () => { await vi.advanceTimersByTimeAsync(200); });
    await vi.waitFor(() => expect(reads).toHaveBeenCalledTimes(2));
    hook.unmount();
    queryClient.clear();
  });

  it('schedules no read-back on a follower for a sign-in, a DNS query or a change the node runs itself', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    server.use(
      http.post(`${API}/auth/login`, () => HttpResponse.json({ principal: { kind: 'User', name: 'admin', id: 'local:admin' }, roles: ['admin'], permissions: { '*': ['*'] }, csrf: 'c' })),
      http.post(`${API}/dns/query`, () => HttpResponse.json({ answers: [] })),
      http.post(`${API}/cache/flush`, () => new HttpResponse(null, { status: 204 })),
    );
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
    const reads = vi.fn(() => Promise.resolve('value'));
    asFollower();
    const hook = harness(queryClient, reads);
    await vi.waitFor(() => expect(hook.result.current.role.follower).toBe(true));
    await vi.waitFor(() => expect(reads).toHaveBeenCalledTimes(1));
    for (const path of ['/auth/login', '/dns/query', '/cache/flush']) {
      await act(async () => { await hook.result.current.ctx.client.request('POST', path, { body: {} }); });
    }
    await act(async () => { await vi.advanceTimersByTimeAsync(FOLLOWER_SYNC_MS * 2); });
    expect(reads).toHaveBeenCalledTimes(1);
    hook.unmount();
    queryClient.clear();
  });

  it('reads the policies again after the wizard created one on a follower', async () => {
    asFollower();
    const policyReads = countReads('/objects/policies');
    renderWithApp(<><PoliciesPage /><PolicyWizardDialog isOpen onClose={() => {}} /></>, { route: '/policies' });
    await screen.findByRole('checkbox', { name: 'kids' });
    fireEvent.change(screen.getByRole('textbox', { name: /^Name/ }), { target: { value: 'follower-made' } });
    fireEvent.click(screen.getByRole('checkbox', { name: 'kids' }));
    for (const step of ['What', 'Safe search', 'How', 'When', 'Review']) {
      fireEvent.click(screen.getByRole('button', { name: 'Next' }));
      await screen.findByRole('heading', { level: 3, name: step });
    }
    fireEvent.click(screen.getByRole('button', { name: 'Create policy' }));
    await waitFor(() => expect(mockDB().policies.some((p) => p.metadata.name === 'follower-made')).toBe(true));
    // The wizard's own invalidation reads at once; the follower has not pulled yet.
    await waitFor(() => expect(policyReads()).toBeGreaterThanOrEqual(1));
    const afterWrite = policyReads();
    await new Promise((resolve) => setTimeout(resolve, FOLLOWER_SYNC_MS / 2));
    expect(policyReads()).toBe(afterWrite);
    await waitFor(() => expect(policyReads()).toBe(afterWrite + 1), { timeout: FOLLOWER_SYNC_MS + 4000 });
  }, 40_000);
});

describe('a session without cluster.read on a follower', () => {
  it('shows the banner and gates primary-only actions from /auth/status', async () => {
    const { statusReads } = asOperatorOnFollower();
    renderWithApp(<><FollowerBanner /><RequestsPage /></>, { route: '/requests' });
    const banner = await screen.findByTestId('follower-banner');
    expect(within(banner).getByText('This node is a follower of kudurru-0')).toBeInTheDocument();
    expect(statusReads()).toBeGreaterThan(0);
    // Unblock decisions need the primary: shown disabled with the reason, not hidden.
    const approve = await within(await screen.findByTestId('request-tiktokv.com')).findByRole('button', { name: 'Approve' });
    await waitFor(() => expect(approve).toHaveAttribute('aria-disabled', 'true'));
    expect(approve).toHaveAccessibleDescription(/Runs only on the primary, kudurru-0/);
  });

  it('gates the same permissions as GET /cluster would', async () => {
    asOperatorOnFollower();
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    queryClient.setQueryData(qk.me, { principal: { name: 'ops' }, permissions: OPERATOR_PERMISSIONS });
    const hook = renderHook(() => ({ role: useClusterRole(), can: useCan(), primaryOnly: usePrimaryOnly() }), { wrapper: wrapper(queryClient) });
    await waitFor(() => expect(hook.result.current.role.follower).toBe(true));
    expect(hook.result.current.role.primary).toBe('kudurru-0');
    expect(hook.result.current.can('unblockrequests', 'decide')).toBe(false);
    expect(hook.result.current.can('blocklists', 'refresh')).toBe(false);
    expect(hook.result.current.can('blocklists', 'write')).toBe(true);
    expect(hook.result.current.primaryOnly('createAllowlistEntries')).toContain('kudurru-0');
    expect(hook.result.current.primaryOnly('createBlocklists')).toBeUndefined();
    hook.unmount();
    queryClient.clear();
  });

  it('never asks GET /cluster, which would be audited as a denial, and follows the role through /auth/status', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval'] });
    const { clusterReads, statusReads } = asOperatorOnFollower();
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    queryClient.setQueryData(qk.me, { principal: { name: 'ops' }, permissions: OPERATOR_PERMISSIONS });
    const hook = renderHook(() => ({ role: useClusterRole(), can: useCan() }), { wrapper: wrapper(queryClient) });
    await vi.waitFor(() => expect(hook.result.current.role.follower).toBe(true));
    const first = statusReads();
    for (let i = 0; i < 4; i++) await act(async () => { await vi.advanceTimersByTimeAsync(ROLE_POLL_MS + 100); });
    expect(clusterReads()).toBe(0);
    // The public route carries the poll instead.
    expect(statusReads()).toBeGreaterThanOrEqual(first + 4);
    hook.unmount();
    queryClient.clear();
  });

  it('stops polling GET /cluster after a 403', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval'] });
    const { clusterReads } = asOperatorOnFollower();
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    // The session claims cluster.read (a grant withdrawn since /auth/me was read).
    queryClient.setQueryData(qk.me, { principal: { name: 'ops' }, permissions: { ...OPERATOR_PERMISSIONS, cluster: ['read'] } });
    const hook = renderHook(() => ({ role: useClusterRole() }), { wrapper: wrapper(queryClient) });
    await vi.waitFor(() => expect(hook.result.current.role.follower).toBe(true));
    expect(clusterReads()).toBe(1);
    for (let i = 0; i < 4; i++) await act(async () => { await vi.advanceTimersByTimeAsync(ROLE_POLL_MS + 100); });
    expect(clusterReads()).toBe(1);
    hook.unmount();
    queryClient.clear();
  });

  it('shows no banner when /auth/status names no role', async () => {
    asOperatorOnFollower();
    server.use(http.get(`${API}/auth/status`, () => HttpResponse.json({ localLogin: true, localLoginMode: 'enabled', providers: [], hasDefaultCredentials: false })));
    const { client } = renderWithApp(<FollowerBanner />);
    await waitFor(() => expect(client.getQueryData(qk.authStatus)).toBeDefined());
    expect(screen.queryByTestId('follower-banner')).toBeNull();
  });
});

describe('the role after a failover', () => {
  it('rereads the role on a timer and at once when a write answers primary_required', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval'] });
    let primary = true;
    server.use(
      http.get(`${API}/cluster`, () => HttpResponse.json(primary ? { ...followerInfo(), node: 'kudurru-1', role: 'primary', primaryRequired: false, forwardedOperations: undefined, localOperations: undefined } : followerInfo())),
      http.post(`${API}/probe-write`, () => HttpResponse.json(PRIMARY_REQUIRED, { status: 503 })),
    );
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    queryClient.setQueryData(qk.me, { principal: { name: 'admin' }, permissions: { '*': ['*'] } });
    const hook = renderHook(() => ({ ctx: useApiContext(), role: useClusterRole() }), { wrapper: wrapper(queryClient) });
    await vi.waitFor(() => expect(hook.result.current.role.role).toBe('primary'));
    // Demoted: a refused write rereads the role without waiting for the poll.
    primary = false;
    await act(async () => { await hook.result.current.ctx.client.request('POST', '/probe-write').catch(() => undefined); });
    await vi.waitFor(() => expect(hook.result.current.role.follower).toBe(true));
    // Promoted again: the poll picks it up.
    primary = true;
    await act(async () => { await vi.advanceTimersByTimeAsync(ROLE_POLL_MS + 100); });
    await vi.waitFor(() => expect(hook.result.current.role.follower).toBe(false));
    hook.unmount();
    queryClient.clear();
  });
});

describe('gated surfaces on a follower', () => {
  it('disables Approve, Deny and Delete on a request with the primary named', async () => {
    asFollower();
    renderWithApp(<RequestDetailPage params={{ ns: 'default', name: 'tiktokv-com-c0a8011e' }} />, { route: '/requests/default/tiktokv-com-c0a8011e' });
    for (const name of ['Approve', 'Deny', 'Delete']) {
      const btn = await screen.findByRole('button', { name });
      await waitFor(() => expect(btn).toHaveAttribute('aria-disabled', 'true'));
      expect(btn).toHaveAccessibleDescription(/Runs only on the primary, kudurru-0/);
    }
  });

  it('disables Send NOTIFY on a zone and keeps record writes', async () => {
    asFollower();
    renderWithApp(<ZoneDetailPage params={{ zone: 'example-internal' }} />, { route: '/zones/example-internal' });
    const notify = await screen.findByRole('button', { name: 'Send NOTIFY' });
    await waitFor(() => expect(notify).toHaveAttribute('aria-disabled', 'true'));
    expect(notify).toHaveAccessibleDescription(/Runs only on the primary, kudurru-0/);
    expect(screen.getByRole('button', { name: 'Add record' })).not.toHaveAttribute('aria-disabled', 'true');
  });

  it('disables "Refresh now" on a list with the primary named', async () => {
    asFollower();
    renderWithApp(<ListDetailPage params={{ ns: 'default', name: 'family' }} />, { route: '/lists/default/family' });
    const refresh = await screen.findByRole('button', { name: 'Refresh now' });
    await waitFor(() => expect(refresh).toHaveAttribute('aria-disabled', 'true'));
    expect(refresh).toHaveAccessibleDescription(/Runs only on the primary, kudurru-0/);
  });

  it('keeps reading node values on the Settings page', async () => {
    asFollower();
    const nodeReads = countReads('/node/config');
    const { client } = renderWithApp(<SettingsPage />, { route: '/settings' });
    await waitFor(() => expect(client.getQueryData(qk.cluster)).toBeDefined());
    await waitFor(() => expect(nodeReads()).toBeGreaterThan(0));
    await waitFor(() => expect(client.getQueryData(qk.nodeConfig)).toBeDefined());
  });

  it('does not open the add dialog from ?add= on a follower', async () => {
    asFollower();
    const { client } = renderWithApp(<AllowlistPage />, { route: '/allowlist?add=cdn.example.org' });
    await screen.findByTestId('entry-s.youtube.com');
    await waitFor(() => expect(client.getQueryData(qk.cluster)).toBeDefined());
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  });

  it('does not open the add dialog from ?add= before the role of a follower is known, and drops the link', async () => {
    let answer: (() => void) | undefined;
    server.use(http.get(`${API}/cluster`, async () => {
      await new Promise<void>((r) => (answer = r));
      return HttpResponse.json(followerInfo());
    }));
    const opened = vi.fn();
    const observer = new MutationObserver(() => {
      if (screen.queryByRole('dialog')) opened();
    });
    observer.observe(document.body, { childList: true, subtree: true });
    afterEachCleanups.push(() => observer.disconnect());
    const { client } = renderWithApp(<AllowlistPage />, { route: '/allowlist?add=cdn.example.org' });
    await screen.findByTestId('entry-s.youtube.com');
    await waitFor(() => expect(answer).toBeDefined());
    // The list is on screen and the role has not answered: still no dialog.
    await new Promise((r) => setTimeout(r, 50));
    expect(opened).not.toHaveBeenCalled();
    answer!();
    await waitFor(() => expect(client.getQueryData(qk.cluster)).toBeDefined());
    await waitFor(() => expect(window.location.search).not.toContain('add='));
    expect(opened).not.toHaveBeenCalled();
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('opens the add dialog from ?add= once a primary answers', async () => {
    renderWithApp(<AllowlistPage />, { route: '/allowlist?add=cdn.example.org' });
    const d = await screen.findByRole('dialog');
    expect(within(d).getByDisplayValue('cdn.example.org')).toBeInTheDocument();
  });

  it('says on the Settings row that a forwarded save has an unknown outcome', async () => {
    asFollower();
    server.use(http.patch(`${API}/settings`, () => HttpResponse.json({ error: { key: 'completion_unknown', message: "the forwarded write's outcome is unknown; do not replay this request" } }, { status: 503 })));
    renderWithApp(<SettingsPage />, { route: '/settings' });
    const r = await waitFor(() => {
      const el = document.querySelector<HTMLElement>('[data-configkey="blocking.defaultResponse"]');
      if (!el) throw new Error('no row');
      return el;
    });
    pick(getSelector('Default block response', r), 'NXDOMAIN');
    fireEvent.click(within(r).getByRole('button', { name: 'Save' }));
    expect(await within(r).findByTestId('setting-error')).toHaveTextContent('Reload to check what is in effect before you try again.');
  });
});

// The surfaces that gate one operation (usePrimaryOnly) or one permission
// (useCan / usePrimaryOnlyPermission) themselves: each is mounted on a
// follower, shows the control disabled with the primary named, and keeps it
// enabled on a standalone node.
describe('more gated surfaces on a follower', () => {
  const PRIMARY_ONLY = /Runs only on the primary, kudurru-0/;

  /** Waits until the control is disabled for the follower and names the primary. */
  async function primaryOnly(el: HTMLElement) {
    await waitFor(() => {
      expect(el.getAttribute('aria-disabled') === 'true' || el.hasAttribute('disabled'), 'disabled').toBe(true);
      expect(el).toHaveAccessibleDescription(PRIMARY_ONLY);
    });
  }

  it('disables Allow and Deny on the query log rows', async () => {
    asFollower();
    renderWithApp(<QueryLogPage />, { route: '/queries' });
    await screen.findByTestId('log-totals');
    const [allow] = await screen.findAllByRole('button', { name: 'Allow' });
    const [deny] = await screen.findAllByRole('button', { name: 'Deny' });
    await primaryOnly(allow!);
    await primaryOnly(deny!);
  });

  it('keeps Allow on the query log rows on a standalone node', async () => {
    renderWithApp(<QueryLogPage />, { route: '/queries?status_class=blocked' });
    const [allow] = await screen.findAllByRole('button', { name: 'Allow' });
    await waitFor(() => expect(allow).not.toHaveAttribute('aria-disabled', 'true'));
    expect(allow).toHaveAccessibleDescription(/to the allow list/);
  });

  it('disables the quick and row Allow and Deny of a client', async () => {
    asFollower();
    renderWithApp(<ClientDetailPage params={{ key: '192.168.1.10' }} />, { route: '/clients/192.168.1.10' });
    const domain = await screen.findByRole('textbox', { name: /^Domain/ });
    const quick = within(domain.closest('form')!);
    fireEvent.change(domain, { target: { value: 'ads.example.com' } });
    await primaryOnly(quick.getByRole('button', { name: 'Allow' }));
    await primaryOnly(quick.getByRole('button', { name: 'Deny' }));
    const recent = within(await screen.findByRole('region', { name: /Recent queries/ }));
    const rowActions = await recent.findAllByRole('button', { name: /^(Allow|Deny)$/ });
    for (const btn of rowActions) await primaryOnly(btn);
  });

  it('keeps the source entries of a list read-only, with the reason on each switch', async () => {
    asFollower();
    renderWithApp(<ListDetailPage params={{ ns: 'default', name: 'starter' }} />, { route: '/lists/default/starter' });
    fireEvent.click(await screen.findByRole('button', { name: /Browse the 2,852 inline entries of embedded/ }));
    const browser = await screen.findByTestId('entries-browser');
    await within(browser).findByText('Showing 1–25 of 2,852');
    const [sw] = within(browser).getAllByRole('switch', { name: / enabled$/ });
    await primaryOnly(sw!);
    // Nothing to add or select: the inline additions are the same command.
    expect(within(browser).queryByRole('textbox', { name: /^Add entries/ })).toBeNull();
    expect(within(browser).queryByRole('checkbox')).toBeNull();
  });

  it('disables the migration import, tracking and decommission with the primary named', async () => {
    asFollower();
    const run = { id: 'migration-test', revision: '7', data: { source: 'technitium', sourceURL: 'https://source.test', actor: 'admin', startedAt: '2026-10-18T00:00:00Z', finishedAt: '2026-10-18T00:01:00Z', status: 'completed', items: [{ kind: 'DNSZone', name: 'home', result: 'created' }], warnings: [], zones: [{ name: 'home', zoneName: 'home.test.', nameservers: ['old.test.'], primary: 'old.test.', resourceVersion: '8', originalNameservers: ['old.test.'], originalPrimary: 'old.test.' }], cutovers: [] } };
    server.use(
      http.get(`${API}/imports/runs`, () => HttpResponse.json({ items: [run] })),
      http.get(`${API}/imports/runs/migration-test`, () => HttpResponse.json(run)),
    );
    const importer = renderWithApp(<MigrationImport />, { route: '/tools' });
    await primaryOnly(await screen.findByRole('button', { name: 'Import from DNS server' }));
    importer.unmount();
    const history = renderWithApp(<MigrationHistory />, { route: '/tools' });
    await primaryOnly(await screen.findByRole('button', { name: 'Track previous imports' }));
    history.unmount();
    renderWithApp(<MigrationRunPage params={{ id: run.id }} />, { route: `/tools/migrations/${run.id}` });
    await primaryOnly(await screen.findByRole('button', { name: 'Decommission source nameservers' }));
  });

  it('disables the external account commands and the authenticator recovery', async () => {
    asFollower();
    const row = { name: 'a'.repeat(64), uid: 'b'.repeat(32), revision: 'c'.repeat(64), provider: 'corp', providerUID: 'provider-one', origin: 'config', issuer: 'https://identity.example', subject: 'independent-subject', username: 'corp:alice', displayName: 'Alice', email: 'alice@example.test', groups: ['corp:dns'], disabled: false, createdAt: '2026-10-01T00:00:00Z' };
    server.use(http.get(`${API}/iam/external-users`, () => HttpResponse.json({ items: [row], took_ms: 1 })));
    const panel = renderWithApp(<ExternalUsersPanel />);
    await screen.findByRole('table', { name: 'External accounts' });
    await primaryOnly(screen.getByRole('button', { name: 'Disable account' }));
    await primaryOnly(screen.getByRole('button', { name: 'Log out everywhere' }));
    panel.unmount();

    renderWithApp(<RecoverAuthenticatorButton user={{ name: 'viewer', uid: 'uid-selected', disabled: false, builtinAdmin: false, mustChangePassword: false }} />);
    const recover = await screen.findByRole('button', { name: 'Recover authenticator for viewer' });
    await waitFor(() => expect(recover.getAttribute('aria-disabled') === 'true' || recover.hasAttribute('disabled')).toBe(true));
    fireEvent.focus(recover);
    fireEvent.mouseEnter(recover);
    expect(await screen.findByText(PRIMARY_ONLY)).toBeInTheDocument();
  });
});

/** An active query (one observer), so an invalidation refetches it. */
function useQueryObserver(reads: () => Promise<string>) {
  return useQueryHook({ queryKey: ['objects', 'x'], queryFn: reads });
}
