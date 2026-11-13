import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError } from '../../api/client';
import type { ClusterEnrollmentInfo, ClusterPromotionInfo, RootAnchorStatus } from '../../api/types';
import { anchorAdvice, CLUSTER_DOCS, commandOutcome, composeCommands, composeSnippet, imageTag, joinCommand, memberAddressError, memberNameError } from '../../lib/cluster';
import NodesPage from '../../pages/NodesPage';
import { server } from '../../test/setup';
import { API, renderWithApp } from '../../test/utils';
import { ErrorBanner } from '../Feedback';

const CLUSTER_ID = '4adbbe69-f1a3-4ff7-bc83-6176229687d7';
const MEMBER_ID = '7ed8a22b-7653-497c-834c-2e347b32e977';
const SECRET = 'kdrinv1.c2VjcmV0LWludml0YXRpb24tdGhhdC1tdXN0LWJlLXNob3duLW9uY2U';

function enrollment(mode: string, dormant: boolean, n = 1): ClusterEnrollmentInfo {
  return { clusterID: CLUSTER_ID, memberID: MEMBER_ID, mode, dormant, sourceDigest: `${n}`.repeat(64).slice(0, 64), ownerDigest: 'e'.repeat(64) };
}

const promotion = (eligible: boolean, reason: string): ClusterPromotionInfo => ({
  node: 'kudurru-0',
  mode: 'follower',
  memberID: MEMBER_ID,
  eligible,
  reason,
  missedPulls: eligible ? 3 : 0,
  unreachableSeconds: eligible ? 12.5 : 0,
  // revision and the term epoch are strings on the wire ("revision,string").
  installed: { term: { clusterID: CLUSTER_ID, epoch: '4', primaryID: 'p-1', promotionID: 'pr-1', primaryName: 'node-a' }, revision: '17', digest: 'd'.repeat(64) } as unknown as ClusterPromotionInfo['installed'],
});

function anchors(over: Partial<RootAnchorStatus> = {}): RootAnchorStatus {
  return {
    node: 'kudurru-0',
    anchorMode: 'managed',
    managedTopology: 'standalone',
    authority: { mode: 'follower', memberID: MEMBER_ID, termEpoch: 4, installedTermEpoch: 4, fencedBehindTerm: false, conflict: false },
    owner: { phase: 'managed', generation: 3, pureGeneration: 3, quarantined: false, clockUncertain: false, keys: [{ keyTag: 20326, algorithm: 8, id: 'k-20326', status: 'Valid', firstSeen: '2026-11-01T00:00:00Z' }], recoveryGenerations: [], retryIntervalSeconds: 3600 },
    admissionCurrent: true,
    updater: { state: 'not_running' },
    supervisor: { state: 'running' },
    warnings: [],
    ...over,
  };
}

interface Mounted {
  role: string;
  enroll: ClusterEnrollmentInfo | (() => ClusterEnrollmentInfo);
  promotion?: ClusterPromotionInfo;
  anchors?: RootAnchorStatus;
}

/** GET /cluster with a SQLite role, the enrollment, promotion and anchor reads. */
function mount({ role, enroll, promotion: p, anchors: a }: Mounted) {
  const reads = { enrollment: 0, promotion: 0 };
  server.use(
    http.get(`${API}/cluster`, () =>
      HttpResponse.json({ clusterID: CLUSTER_ID, driver: 'sqlite', epoch: 4, leader: 'kudurru-0', nodes: 1, readOnly: false, node: 'kudurru-0', role, primaryRequired: role === 'follower', primary: role === 'follower' ? 'node-a' : 'kudurru-0', ...(role === 'follower' ? { forwardedOperations: [], localOperations: ['activateCluster', 'issueClusterJoinToken', 'promoteCluster'] } : {}), took_ms: 1 }),
    ),
    http.get(`${API}/cluster/enrollment`, () => {
      reads.enrollment++;
      return HttpResponse.json(typeof enroll === 'function' ? enroll() : enroll);
    }),
    http.get(`${API}/cluster/promotion`, () => {
      reads.promotion++;
      return HttpResponse.json(p ?? promotion(false, 'not_follower'));
    }),
    http.get(`${API}/cluster/anchors`, () => HttpResponse.json(a ?? anchors())),
  );
  return reads;
}

const dialog = (name: string) => screen.findByRole('dialog', { name });
const click = (root: HTMLElement, name: string | RegExp) => fireEvent.click(within(root).getByRole('button', { name }));
const type = (root: HTMLElement, name: RegExp, value: string) => fireEvent.change(within(root).getByRole('textbox', { name }), { target: { value } });

async function reviewAndConfirm(d: HTMLElement, confirm: RegExp) {
  await within(d).findByText(CLUSTER_ID);
  click(d, 'Next');
  expect(await within(d).findByText(/Confirm that you reviewed/)).toBeInTheDocument();
  fireEvent.click(within(d).getByRole('checkbox', { name: confirm }));
  click(d, 'Next');
  await atStep(d, 'member');
}

/** Clicks a step of the wizard's stepper (it jumps to any step already reached). */
function stepper(d: HTMLElement, label: string) {
  const list = d.querySelector('ol');
  if (!list) throw new Error('no stepper');
  fireEvent.click(within(list as HTMLElement).getByRole('button', { name: new RegExp(`^Go to step \\d+: ${label}`) }));
}

/** Waits for the wizard to show a step (Next validates asynchronously). */
const atStep = (d: HTMLElement, id: string) => waitFor(() => expect(d.querySelector(`[data-step="${id}"]`)).not.toBeNull());

afterEach(() => vi.restoreAllMocks());

describe('cluster membership on the Nodes page', () => {
  it('activates a dormant standalone node with exactly the reviewed identities, then asks for a restart', async () => {
    mount({ role: 'standalone', enroll: enrollment('standalone', true) });
    const bodies: unknown[] = [];
    server.use(http.post(`${API}/cluster/activate`, async ({ request }) => {
      bodies.push(await request.json());
      return HttpResponse.json({ committed: true, clusterID: CLUSTER_ID, memberID: MEMBER_ID, restartRequired: true });
    }));
    renderWithApp(<NodesPage />, { route: '/nodes' });
    const panel = await screen.findByRole('region', { name: 'Cluster membership' });
    expect(await within(panel).findByText(/This node is not in a cluster/)).toBeInTheDocument();
    expect(within(panel).queryByRole('button', { name: 'Invite a member' })).toBeNull();
    fireEvent.click(within(panel).getByRole('button', { name: 'Activate cluster' }));
    const d = await dialog('Activate cluster');
    await reviewAndConfirm(d, /I reviewed this node's enrollment/);
    // Member: the node's name is suggested; the address needs a port.
    expect(within(d).getByRole('textbox', { name: /^Member name/ })).toHaveValue('kudurru-0');
    type(d, /^Internal address/, '10.0.0.11');
    type(d, /^Reason/, 'start the cluster');
    click(d, 'Next');
    expect((await within(d).findAllByText(/host:port, for example/)).length).toBeGreaterThan(0);
    type(d, /^Internal address/, '10.0.0.11:8082');
    click(d, 'Next');
    expect(await within(d).findByText('One request, then a restart')).toBeInTheDocument();
    click(d, 'Activate cluster');
    expect(await within(panel).findByText('Cluster activated: restart kudurru on this node')).toBeInTheDocument();
    expect(bodies).toEqual([{ clusterID: CLUSTER_ID, memberID: MEMBER_ID, sourceDigest: '1'.repeat(64), ownerDigest: 'e'.repeat(64), name: 'kudurru-0', address: '10.0.0.11:8082', reason: 'start the cluster' }]);
    expect(screen.queryByRole('dialog', { name: 'Activate cluster' })).toBeNull();
  });

  it('refuses to activate a node that is not dormant standalone, from the review', async () => {
    mount({ role: 'standalone', enroll: enrollment('standalone', true) });
    renderWithApp(<NodesPage />, { route: '/nodes' });
    const panel = await screen.findByRole('region', { name: 'Cluster membership' });
    // Another operator activated it between the page read and the wizard's own review.
    server.use(http.get(`${API}/cluster/enrollment`, () => HttpResponse.json(enrollment('primary', false))));
    fireEvent.click(await within(panel).findByRole('button', { name: 'Activate cluster' }));
    const d = await dialog('Activate cluster');
    await within(d).findByText('primary');
    fireEvent.click(within(d).getByRole('checkbox', { name: /I reviewed/ }));
    click(d, 'Next');
    expect(await within(d).findByText(/Only a dormant standalone node can be activated; this node is primary and not dormant/)).toBeInTheDocument();
  });

  it('sends nothing more after a 409: the review changed and is not refreshed', async () => {
    mount({ role: 'standalone', enroll: enrollment('standalone', true) });
    let posts = 0;
    server.use(http.post(`${API}/cluster/activate`, () => {
      posts++;
      return HttpResponse.json({ error: { key: 'conflict', message: 'cluster source changed or is not dormant standalone; review enrollment before a new decision' } }, { status: 409 });
    }));
    renderWithApp(<NodesPage />, { route: '/nodes' });
    fireEvent.click(await screen.findByRole('button', { name: 'Activate cluster' }));
    const d = await dialog('Activate cluster');
    await reviewAndConfirm(d, /I reviewed/);
    type(d, /^Internal address/, '10.0.0.11:8082');
    type(d, /^Reason/, 'start');
    click(d, 'Next');
    await atStep(d, 'activate');
    click(d, 'Activate cluster');
    expect(await within(d).findByText('The reviewed state changed')).toBeInTheDocument();
    expect(within(d).getByRole('button', { name: 'Activate cluster' })).toBeDisabled();
    fireEvent.submit(within(d).getByRole('form', { name: 'Activate cluster' }));
    await new Promise((r) => setTimeout(r, 50));
    expect(posts).toBe(1);
  });

  it('lets a definite refusal be corrected and decided again', async () => {
    mount({ role: 'standalone', enroll: enrollment('standalone', true) });
    let posts = 0;
    server.use(http.post(`${API}/cluster/activate`, () => {
      posts++;
      return posts === 1
        ? HttpResponse.json({ error: { key: 'invalid', message: 'exact reviewed source, member name/address and audit reason required' } }, { status: 400 })
        : HttpResponse.json({ committed: true, clusterID: CLUSTER_ID, memberID: MEMBER_ID, restartRequired: true });
    }));
    renderWithApp(<NodesPage />, { route: '/nodes' });
    fireEvent.click(await screen.findByRole('button', { name: 'Activate cluster' }));
    const d = await dialog('Activate cluster');
    await reviewAndConfirm(d, /I reviewed/);
    type(d, /^Internal address/, '10.0.0.11:8082');
    type(d, /^Reason/, 'start');
    click(d, 'Next');
    await atStep(d, 'activate');
    click(d, 'Activate cluster');
    expect(await within(d).findByText(/exact reviewed source, member name\/address and audit reason required/)).toBeInTheDocument();
    expect(within(d).getByRole('button', { name: 'Activate cluster' })).toBeEnabled();
    click(d, 'Activate cluster');
    expect(await screen.findByText('Cluster activated: restart kudurru on this node')).toBeInTheDocument();
    expect(posts).toBe(2);
  });

  it('issues an invitation, shows it exactly once and keeps it out of every cache, log and URL', async () => {
    let n = 1;
    const reads = mount({ role: 'primary', enroll: () => enrollment('primary', false, n) });
    const bodies: Record<string, unknown>[] = [];
    server.use(http.post(`${API}/cluster/join-token`, async ({ request }) => {
      bodies.push((await request.json()) as Record<string, unknown>);
      n = 2; // the primary's source digest changes with the invitation
      return HttpResponse.json({ committed: true, invitation: SECRET, expiresAt: new Date(Date.now() + 300_000).toISOString() });
    }));
    const logs = [vi.spyOn(console, 'log'), vi.spyOn(console, 'info'), vi.spyOn(console, 'warn'), vi.spyOn(console, 'error'), vi.spyOn(console, 'debug')];
    const { client } = renderWithApp(<NodesPage />, { route: '/nodes' });
    const panel = await screen.findByRole('region', { name: 'Cluster membership' });
    expect(within(panel).queryByRole('button', { name: 'Activate cluster' })).toBeNull();
    fireEvent.click(await within(panel).findByRole('button', { name: 'Invite a member' }));
    const d = await dialog('Invite a member');
    await reviewAndConfirm(d, /I reviewed the primary's enrollment/);
    type(d, /^Member name/, 'Node_B');
    type(d, /^Internal address/, '10.0.0.12:8082');
    type(d, /^Reason/, 'add node-b');
    click(d, 'Next');
    expect((await within(d).findAllByText(/Lowercase letters, digits and hyphens/)).length).toBeGreaterThan(0);
    type(d, /^Member name/, 'node-b');
    click(d, 'Next');
    expect(await within(d).findByText('Shown once')).toBeInTheDocument();
    const before = Date.now();
    click(d, 'Issue invitation');
    const shown = await within(d).findByTestId('cluster-invitation');
    expect(shown).toHaveTextContent(SECRET);
    expect(within(d).getByText('Invitation for node-b issued')).toBeInTheDocument();
    // The join command and the Compose changes for exactly this member.
    expect(d.textContent).toContain('--confirm-name node-b');
    expect(d.textContent).toContain('--confirm-address 10.0.0.12:8082');
    expect(d.textContent).toContain('KUDURRU_NODE__NAME: node-b');
    // One request, with the reviewed identities and an absolute expiry within the chosen 5 minutes.
    expect(bodies).toHaveLength(1);
    const body = bodies[0]!;
    expect(body).toMatchObject({ clusterID: CLUSTER_ID, memberID: MEMBER_ID, sourceDigest: '1'.repeat(64), ownerDigest: 'e'.repeat(64), name: 'node-b', address: '10.0.0.12:8082', reason: 'add node-b' });
    const expires = Date.parse(String(body.expiresAt));
    expect(expires).toBeGreaterThanOrEqual(before + 300_000 - 1000);
    expect(expires).toBeLessThanOrEqual(Date.now() + 300_000);

    // Nowhere but on screen: no query or mutation cache, no storage, no URL, no console.
    const cached = JSON.stringify([client.getQueryCache().getAll().map((q) => q.state.data), client.getMutationCache().getAll().map((m) => m.state.data)]);
    expect(cached).not.toContain(SECRET);
    expect(JSON.stringify({ ...window.localStorage })).not.toContain(SECRET);
    expect(JSON.stringify({ ...window.sessionStorage })).not.toContain(SECRET);
    expect(window.location.href).not.toContain(SECRET);
    for (const spy of logs) expect(JSON.stringify(spy.mock.calls)).not.toContain(SECRET);

    // Closing asks first (it is not shown again), then drops it.
    fireEvent.keyDown(d, { key: 'Escape' });
    const ask = await screen.findByRole('alertdialog', { name: 'Close the invitation?' });
    fireEvent.click(within(ask).getByRole('button', { name: 'Keep it open' }));
    expect(within(d).getByTestId('cluster-invitation')).toBeInTheDocument();
    click(d, 'Done');
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Invite a member' })).toBeNull());
    expect(document.body.textContent).not.toContain(SECRET);

    // Opening again starts a new review: never the old invitation, never the old digests.
    const before2 = reads.enrollment;
    fireEvent.click(within(panel).getByRole('button', { name: 'Invite a member' }));
    const again = await dialog('Invite a member');
    await within(again).findByText('2'.repeat(64));
    expect(reads.enrollment).toBeGreaterThan(before2);
    expect(document.body.textContent).not.toContain(SECRET);
  });

  it.each([
    ['a lost response', () => HttpResponse.error()],
    ['completion_unknown', () => HttpResponse.json({ error: { key: 'completion_unknown', message: 'invitation completion is unknown; inspect enrollment before a new decision; do not replay this command' } }, { status: 503 })],
    ['committed_output_unavailable', () => HttpResponse.json({ error: { key: 'committed_output_unavailable', message: 'invitation committed but its secret response is unavailable; do not replay this command' } }, { status: 503 })],
  ])('never replays an invitation after %s', async (_name, answer) => {
    mount({ role: 'primary', enroll: enrollment('primary', false) });
    let posts = 0;
    server.use(http.post(`${API}/cluster/join-token`, () => {
      posts++;
      return answer();
    }));
    renderWithApp(<NodesPage />, { route: '/nodes' });
    fireEvent.click(await screen.findByRole('button', { name: 'Invite a member' }));
    const d = await dialog('Invite a member');
    await reviewAndConfirm(d, /I reviewed/);
    type(d, /^Member name/, 'node-b');
    type(d, /^Internal address/, '10.0.0.12:8082');
    type(d, /^Reason/, 'add node-b');
    click(d, 'Next');
    await atStep(d, 'issue');
    click(d, 'Issue invitation');
    expect(await within(d).findByText('Outcome unknown')).toBeInTheDocument();
    expect(within(d).getByText(/cannot tell whether the invitation happened\. It does not send it again/)).toBeInTheDocument();
    expect(within(d).getByRole('button', { name: 'Issue invitation' })).toBeDisabled();
    fireEvent.submit(within(d).getByRole('form', { name: 'Invite a member' }));
    await new Promise((r) => setTimeout(r, 50));
    expect(posts).toBe(1);
    expect(within(d).queryByTestId('cluster-invitation')).toBeNull();
  });

  it.each([
    ['Read again', (d: HTMLElement) => click(d, 'Read again')],
    ['unticking the confirmation', (d: HTMLElement) => fireEvent.click(within(d).getByRole('checkbox', { name: /I reviewed/ }))],
  ])('sends no invitation after %s on a revisited review step, even through the stepper', async (_name, unconfirm) => {
    let n = 1;
    mount({ role: 'primary', enroll: () => enrollment('primary', false, n) });
    let posts = 0;
    server.use(http.post(`${API}/cluster/join-token`, () => {
      posts++;
      return HttpResponse.json({ committed: true, invitation: SECRET, expiresAt: new Date(Date.now() + 300_000).toISOString() });
    }));
    renderWithApp(<NodesPage />, { route: '/nodes' });
    fireEvent.click(await screen.findByRole('button', { name: 'Invite a member' }));
    const d = await dialog('Invite a member');
    await reviewAndConfirm(d, /I reviewed/);
    type(d, /^Member name/, 'node-b');
    type(d, /^Internal address/, '10.0.0.12:8082');
    type(d, /^Reason/, 'add node-b');
    click(d, 'Next');
    await atStep(d, 'issue');
    // Back to the review through the stepper; the primary's data changed meanwhile.
    n = 2;
    stepper(d, 'Review');
    await atStep(d, 'review');
    unconfirm(d);
    await waitFor(() => expect(within(d).getByRole('checkbox', { name: /I reviewed/ })).not.toBeChecked());
    // Straight to the last step without Next, and finish there.
    stepper(d, 'Issue');
    await atStep(d, 'issue');
    click(d, 'Issue invitation');
    await atStep(d, 'review');
    expect(await within(d).findByText('Confirm that you reviewed the enrollment.')).toBeInTheDocument();
    await new Promise((r) => setTimeout(r, 50));
    expect(posts).toBe(0);
  });

  it('sends exactly the confirmed review, never a re-read one that was not confirmed again', async () => {
    let n = 1;
    mount({ role: 'standalone', enroll: () => enrollment('standalone', true, n) });
    const bodies: Record<string, unknown>[] = [];
    server.use(http.post(`${API}/cluster/activate`, async ({ request }) => {
      bodies.push((await request.json()) as Record<string, unknown>);
      return HttpResponse.json({ committed: true, clusterID: CLUSTER_ID, memberID: MEMBER_ID, restartRequired: true });
    }));
    renderWithApp(<NodesPage />, { route: '/nodes' });
    fireEvent.click(await screen.findByRole('button', { name: 'Activate cluster' }));
    const d = await dialog('Activate cluster');
    await reviewAndConfirm(d, /I reviewed/);
    type(d, /^Internal address/, '10.0.0.11:8082');
    type(d, /^Reason/, 'start');
    click(d, 'Next');
    await atStep(d, 'activate');
    n = 2;
    click(d, 'Edit review');
    await atStep(d, 'review');
    click(d, 'Read again');
    await within(d).findByText('2'.repeat(64));
    stepper(d, 'Activate');
    await atStep(d, 'activate');
    click(d, 'Activate cluster');
    expect(await within(d).findByText('Confirm that you reviewed the enrollment.')).toBeInTheDocument();
    expect(bodies).toHaveLength(0);
    // Confirming the new values sends those, and only those.
    fireEvent.click(within(d).getByRole('checkbox', { name: /I reviewed/ }));
    stepper(d, 'Activate');
    await atStep(d, 'activate');
    click(d, 'Activate cluster');
    expect(await screen.findByText('Cluster activated: restart kudurru on this node')).toBeInTheDocument();
    expect(bodies).toHaveLength(1);
    expect(bodies[0]).toMatchObject({ sourceDigest: '2'.repeat(64) });
  });

  it('sends no promotion when a re-read review is no longer eligible', async () => {
    let p = promotion(true, 'eligible');
    mount({ role: 'follower', enroll: enrollment('follower', false), promotion: undefined });
    server.use(http.get(`${API}/cluster/promotion`, () => HttpResponse.json(p)));
    let posts = 0;
    server.use(http.post(`${API}/cluster/promote`, () => {
      posts++;
      return HttpResponse.json({ error: { key: 'conflict', message: 'x' } }, { status: 409 });
    }));
    renderWithApp(<NodesPage />, { route: '/nodes' });
    fireEvent.click(await screen.findByRole('button', { name: 'Review promotion' }));
    const d = await dialog('Promote this follower');
    await within(d).findByText('Eligible for promotion');
    fireEvent.click(within(d).getByRole('checkbox', { name: /I reviewed/ }));
    click(d, 'Next');
    await atStep(d, 'decide');
    type(d, /^Type the node name/, 'kudurru-0');
    type(d, /^Reason/, 'node-a is lost');
    click(d, 'Next');
    await atStep(d, 'promote');
    p = promotion(false, 'awaiting_missed_pulls');
    stepper(d, 'Review');
    await atStep(d, 'review');
    click(d, 'Read again');
    expect(await within(d).findByText('Not eligible for promotion')).toBeInTheDocument();
    fireEvent.click(within(d).getByRole('checkbox', { name: /I reviewed/ }));
    stepper(d, 'Promote');
    await atStep(d, 'promote');
    click(d, 'Promote kudurru-0');
    await atStep(d, 'review');
    await waitFor(() => expect(within(d).getAllByText(/The primary still answers/).length).toBeGreaterThan(1));
    expect(posts).toBe(0);
  });

  it('offers invitation lifetimes that leave a margin below the primary maximum', async () => {
    mount({ role: 'primary', enroll: enrollment('primary', false) });
    const bodies: Record<string, unknown>[] = [];
    server.use(http.post(`${API}/cluster/join-token`, async ({ request }) => {
      bodies.push((await request.json()) as Record<string, unknown>);
      return HttpResponse.json({ committed: true, invitation: SECRET, expiresAt: new Date(Date.now() + 540_000).toISOString() });
    }));
    renderWithApp(<NodesPage />, { route: '/nodes' });
    fireEvent.click(await screen.findByRole('button', { name: 'Invite a member' }));
    const d = await dialog('Invite a member');
    await reviewAndConfirm(d, /I reviewed/);
    type(d, /^Member name/, 'node-b');
    type(d, /^Internal address/, '10.0.0.12:8082');
    type(d, /^Reason/, 'add node-b');
    fireEvent.click(within(d).getByRole('combobox', { name: /^Valid for/ }));
    const offered = screen.getAllByRole('option').map((o) => o.textContent ?? '');
    expect(offered.some((o) => /10 minutes/.test(o))).toBe(false);
    fireEvent.click(screen.getByRole('option', { name: /^9 minutes/ }));
    expect(within(d).getByRole('combobox', { name: /^Valid for/ })).toHaveAttribute('data-value', '9');
    click(d, 'Next');
    await atStep(d, 'issue');
    click(d, 'Issue invitation');
    await within(d).findByTestId('cluster-invitation');
    // A browser clock up to a minute ahead of the primary still stays within its ten minutes.
    expect(Date.parse(String(bodies[0]!.expiresAt))).toBeLessThanOrEqual(Date.now() + 600_000 - 60_000);
  });

  it('names the correctable causes when the primary refuses an invitation with 409', async () => {
    mount({ role: 'primary', enroll: enrollment('primary', false) });
    server.use(http.post(`${API}/cluster/join-token`, () => HttpResponse.json({ error: { key: 'conflict', message: 'cluster source changed or is not an enrolled primary' } }, { status: 409 })));
    renderWithApp(<NodesPage />, { route: '/nodes' });
    fireEvent.click(await screen.findByRole('button', { name: 'Invite a member' }));
    const d = await dialog('Invite a member');
    await reviewAndConfirm(d, /I reviewed/);
    type(d, /^Member name/, 'node-b');
    type(d, /^Internal address/, '10.0.0.12:8082');
    type(d, /^Reason/, 'add node-b');
    click(d, 'Next');
    await atStep(d, 'issue');
    click(d, 'Issue invitation');
    expect(await within(d).findByText('The primary refused the invitation')).toBeInTheDocument();
    expect(within(d).getByText(/name or internal address is already used or reserved/)).toBeInTheDocument();
    expect(within(d).getByText(/clock/)).toBeInTheDocument();
    expect(within(d).getByRole('button', { name: 'Issue invitation' })).toBeDisabled();
  });

  it('cannot be closed while the invitation request is in flight', async () => {
    mount({ role: 'primary', enroll: enrollment('primary', false) });
    let answer: (() => void) | undefined;
    server.use(http.post(`${API}/cluster/join-token`, async () => {
      await new Promise<void>((r) => (answer = r));
      return HttpResponse.json({ committed: true, invitation: SECRET, expiresAt: new Date(Date.now() + 300_000).toISOString() });
    }));
    renderWithApp(<NodesPage />, { route: '/nodes' });
    fireEvent.click(await screen.findByRole('button', { name: 'Invite a member' }));
    const d = await dialog('Invite a member');
    await reviewAndConfirm(d, /I reviewed/);
    type(d, /^Member name/, 'node-b');
    type(d, /^Internal address/, '10.0.0.12:8082');
    type(d, /^Reason/, 'add node-b');
    click(d, 'Next');
    await atStep(d, 'issue');
    click(d, 'Issue invitation');
    await waitFor(() => expect(answer).toBeDefined());
    expect(within(d).getByRole('button', { name: 'Cancel' })).toBeDisabled();
    fireEvent.keyDown(d, { key: 'Escape' });
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.queryByRole('alertdialog')).toBeNull();
    expect(screen.getByRole('dialog', { name: 'Invite a member' })).toBeInTheDocument();
    answer!();
    expect(await within(d).findByTestId('cluster-invitation')).toHaveTextContent(SECRET);
  });

  it('blocks the promotion of a follower that is not eligible, with the reason', async () => {
    mount({ role: 'follower', enroll: enrollment('follower', false), promotion: promotion(false, 'awaiting_missed_pulls') });
    renderWithApp(<NodesPage />, { route: '/nodes' });
    const panel = await screen.findByRole('region', { name: 'Cluster membership' });
    fireEvent.click(await within(panel).findByRole('button', { name: 'Review promotion' }));
    const d = await dialog('Promote this follower');
    expect(await within(d).findByText('Not eligible for promotion')).toBeInTheDocument();
    fireEvent.click(within(d).getByRole('checkbox', { name: /I reviewed this follower's state/ }));
    click(d, 'Next');
    await waitFor(() => expect(within(d).getAllByText(/The primary still answers/).length).toBeGreaterThan(1));
    expect(within(d).queryByRole('textbox', { name: /^Type the node name/ })).toBeNull();
  });

  it('promotes an eligible follower with the exact reviewed generation, node confirmation and one request ID', async () => {
    mount({ role: 'follower', enroll: enrollment('follower', false), promotion: promotion(true, 'eligible') });
    const bodies: Record<string, unknown>[] = [];
    server.use(http.post(`${API}/cluster/promote`, async ({ request }) => {
      const b = (await request.json()) as Record<string, unknown>;
      bodies.push(b);
      return HttpResponse.json({ committed: true, memberID: MEMBER_ID, requestID: b.requestID, term: { clusterID: CLUSTER_ID, epoch: '5', primaryID: MEMBER_ID, promotionID: 'pr-2' } });
    }));
    renderWithApp(<NodesPage />, { route: '/nodes' });
    fireEvent.click(await screen.findByRole('button', { name: 'Review promotion' }));
    const d = await dialog('Promote this follower');
    expect(await within(d).findByText('Eligible for promotion')).toBeInTheDocument();
    fireEvent.click(within(d).getByRole('checkbox', { name: /I reviewed/ }));
    click(d, 'Next');
    await atStep(d, 'decide');
    type(d, /^Type the node name/, 'kudurru-1');
    type(d, /^Reason/, 'node-a is lost');
    click(d, 'Next');
    expect(await within(d).findByText(/Type kudurru-0 exactly/)).toBeInTheDocument();
    type(d, /^Type the node name/, 'kudurru-0');
    click(d, 'Next');
    await atStep(d, 'promote');
    click(d, 'Promote kudurru-0');
    expect(await screen.findByText('Promoted: this node is the primary of term 5')).toBeInTheDocument();
    expect(bodies).toHaveLength(1);
    expect(bodies[0]).toMatchObject({ memberID: MEMBER_ID, confirmNode: 'kudurru-0', reason: 'node-a is lost', installed: promotion(true, 'eligible').installed });
    expect(String(bodies[0]!.requestID)).toMatch(/^dashboard-[0-9a-f-]{36}$/);
  });

  it('never replays a promotion whose outcome is unknown', async () => {
    mount({ role: 'follower', enroll: enrollment('follower', false), promotion: promotion(true, 'eligible') });
    let posts = 0;
    server.use(http.post(`${API}/cluster/promote`, () => {
      posts++;
      return HttpResponse.json({ error: { key: 'completion_unknown', message: 'promotion completion is unknown; inspect current state before a new decision; do not replay this command' } }, { status: 503 });
    }));
    renderWithApp(<NodesPage />, { route: '/nodes' });
    fireEvent.click(await screen.findByRole('button', { name: 'Review promotion' }));
    const d = await dialog('Promote this follower');
    await within(d).findByText('Eligible for promotion');
    fireEvent.click(within(d).getByRole('checkbox', { name: /I reviewed/ }));
    click(d, 'Next');
    await atStep(d, 'decide');
    type(d, /^Type the node name/, 'kudurru-0');
    type(d, /^Reason/, 'node-a is lost');
    click(d, 'Next');
    await atStep(d, 'promote');
    click(d, 'Promote kudurru-0');
    expect(await within(d).findByText('Outcome unknown')).toBeInTheDocument();
    expect(within(d).getByRole('button', { name: 'Promote kudurru-0' })).toBeDisabled();
    expect(posts).toBe(1);
  });

  it('shows no membership panel on a node without a SQLite role', async () => {
    renderWithApp(<NodesPage />, { route: '/nodes' });
    expect(await screen.findByText('store sqlite')).toBeInTheDocument();
    expect(screen.queryByRole('region', { name: 'Cluster membership' })).toBeNull();
    expect(screen.queryByRole('region', { name: 'Root trust anchors' })).toBeNull();
  });
});

describe('root trust anchors on the Nodes page', () => {
  it('shows a refused adoption with the refused key and the documented action', async () => {
    mount({
      role: 'follower',
      enroll: enrollment('follower', false),
      anchors: anchors({ pull: { result: 'refused', at: '2026-11-12T10:00:00Z', refusal: 'carried_revocation', refusedSince: '2026-11-12T09:00:00Z', refusedKeyTags: [20326], refusedKeyIDs: ['k-20326'] }, warnings: ['this member refuses the primary\'s root-anchor state (carried_revocation); see "A member returns with different trust-anchor state"'] }),
    });
    renderWithApp(<NodesPage />, { route: '/nodes' });
    const panel = await screen.findByRole('region', { name: 'Root trust anchors' });
    const needs = await within(panel).findByRole('list', { name: 'Trust-anchor states that need you' });
    const item = within(needs).getByText(/refuses the primary's trust-anchor state: it knows a root key to be revoked that the primary still trusts \(key tag 20326\)/).closest('li')!;
    expect(within(item).getByText(/first pull after the primary's next root-anchor check records the revocation/)).toBeInTheDocument();
    expect(within(item).getByRole('link', { name: /What to do/ })).toHaveAttribute('href', CLUSTER_DOCS.anchorsEnds);
    expect(within(panel).getByText(/refuses the primary's state \(carried_revocation\)/)).toBeInTheDocument();
    expect(within(panel).getByRole('list', { name: 'Warnings of this node' })).toHaveTextContent('carried_revocation');
  });

  it('shows a member fenced behind its term with the abandon-term action', async () => {
    mount({
      role: 'follower',
      enroll: enrollment('follower', false),
      anchors: anchors({ authority: { mode: 'fenced', memberID: MEMBER_ID, termEpoch: 6, installedTermEpoch: 4, fencedBehindTerm: true, conflict: false } }),
    });
    renderWithApp(<NodesPage />, { route: '/nodes' });
    const panel = await screen.findByRole('region', { name: 'Root trust anchors' });
    const item = (await within(panel).findByText(/fenced by a newer term \(epoch 6\) whose data it never installed \(epoch 4\)/)).closest('li')!;
    expect(within(item).getByText(/cluster resolve --offline --abandon-term/)).toBeInTheDocument();
    expect(within(item).getByRole('link', { name: /What to do/ })).toHaveAttribute('href', CLUSTER_DOCS.anchorsAbandon);
    expect(within(panel).getByText('Fenced behind its term').closest('[class]')?.parentElement?.textContent).toContain('yes');
  });

  it('says when nothing needs the operator', async () => {
    mount({ role: 'primary', enroll: enrollment('primary', false) });
    renderWithApp(<NodesPage />, { route: '/nodes' });
    const panel = await screen.findByRole('region', { name: 'Root trust anchors' });
    expect(await within(panel).findByText('No trust-anchor state needs you on this node.')).toBeInTheDocument();
    expect(within(panel).getByRole('table', { name: 'Root keys' })).toHaveTextContent('20326');
  });
});

describe('cluster helpers', () => {
  it('validates member names and internal addresses like the server', () => {
    expect(memberNameError('node-b')).toBeNull();
    expect(memberNameError('-node')).not.toBeNull();
    expect(memberNameError('Node')).not.toBeNull();
    expect(memberNameError('a'.repeat(64))).not.toBeNull();
    expect(memberAddressError('10.0.0.12:8082')).toBeNull();
    expect(memberAddressError('node-b.example:8082')).toBeNull();
    expect(memberAddressError('[2001:db8::12]:8082')).toBeNull();
    expect(memberAddressError('10.0.0.12')).not.toBeNull();
    expect(memberAddressError('2001:db8::12')).not.toBeNull();
    expect(memberAddressError('10.0.0.12:99999')).not.toBeNull();
  });

  it('classifies a failed command for the next decision', () => {
    expect(commandOutcome(new ApiError(400, 'invalid', 'x'))).toBe('refused');
    expect(commandOutcome(new ApiError(403, 'forbidden', 'x'))).toBe('refused');
    expect(commandOutcome(new ApiError(503, 'primary_required', 'x'))).toBe('refused');
    expect(commandOutcome(new ApiError(409, 'conflict', 'x'))).toBe('stale');
    expect(commandOutcome(new ApiError(503, 'completion_unknown', 'x'))).toBe('uncertain');
    expect(commandOutcome(new ApiError(503, 'committed_output_unavailable', 'x'))).toBe('uncertain');
    expect(commandOutcome(new ApiError(503, 'unavailable', 'x'))).toBe('uncertain');
    expect(commandOutcome(new ApiError(0, 'network', 'x'))).toBe('uncertain');
    expect(commandOutcome(new DOMException('timed out', 'TimeoutError'))).toBe('uncertain');
  });

  it('writes the join command and the Compose changes for the member', () => {
    expect(joinCommand('node-b', '10.0.0.12:8082', 'node-b.invitation')).toContain('--invitation-file node-b.invitation');
    expect(imageTag('v0.2.0')).toBe('0.2.0');
    expect(imageTag('dev')).toBeUndefined();
    const compose = composeSnippet('node-b', '10.0.0.12:9082', 'v0.2.0');
    expect(compose).toContain('image: ghcr.io/robertherbaugh/kudurru:0.2.0');
    expect(compose).toContain('KUDURRU_NODE__DATADIR: /var/lib/kudurru/member');
    expect(compose).toContain('"9082:8082"');
    expect(composeCommands('node-b', '10.0.0.12:8082', 'node-b.invitation')).toContain('--invitation-file /dev/stdin');
  });

  it('lists quarantine, a terminal carrier and a stopped updater with their documented actions', () => {
    const advice = anchorAdvice(anchors({ owner: { ...anchors().owner, quarantined: true }, local: { phase: 'managed', ownerGeneration: 3, retiredThrough: 3, retired: true, terminal: true, denied: [] }, updater: { state: 'stopped', error: 'invalid clock continuity' } }));
    expect(advice.map((a) => [a.id, a.href])).toEqual([
      ['quarantined', CLUSTER_DOCS.anchorsQuarantine],
      ['terminal', CLUSTER_DOCS.anchorsReplace],
      ['updater-stopped', CLUSTER_DOCS.anchorsMembers],
    ]);
  });

  it('explains a fenced member and links its documentation', () => {
    renderWithApp(<ErrorBanner error={new ApiError(503, 'cluster_fenced', 'this node observed the newer primary node-b and closed its API until it follows that primary', 'send requests to the current primary')} />, { user: null });
    expect(screen.getByText('This node is fenced')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /What to do/ })).toHaveAttribute('href', CLUSTER_DOCS.fenced);
  });
});
