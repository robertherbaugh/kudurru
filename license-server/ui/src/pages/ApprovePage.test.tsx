// The /approve page of the one-click approval link: reachable without a
// session (it never asks for one, sends no CSRF header), opening it only
// previews (mail scanners open links), and only a click decides. Approve
// posts the decision and says where the key went (never the key); Reject
// needs a note; a used, expired or already decided link still shows the
// request and says why it can no longer decide.
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import type { ApprovalPreview } from '@shared/api';
import { apiError } from '../dev/api';
import { approvalPreview, TOKEN } from '../dev/fixtures';
import { renderApp } from '../test/render';

const route = '/approve?token=tok_approve_1';

test('opening the link previews the request and the terms, and decides nothing', async () => {
  const { api } = renderApp({ route, me: null, handlers: { approvalPreview: () => approvalPreview } });
  expect(await screen.findByRole('button', { name: 'Approve and send the key' })).toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Reject' })).toBeInTheDocument();
  expect(api.callsTo('approvalPreview')).toHaveLength(1);
  expect(api.callsTo('approvalPreview')[0]?.body).toEqual({ token: 'tok_approve_1' });
  expect(api.callsTo('approvalDecide')).toHaveLength(0);
  // Outside the session gate: no session was asked for, no CSRF header sent.
  expect(api.callsTo('authMe')).toHaveLength(0);
  expect(api.callsTo('approvalPreview')[0]?.headers.get('X-Kudurru-CSRF')).toBeNull();

  expect(screen.getByText('Grace Hopper')).toBeInTheDocument();
  expect(screen.getByText('grace@example.org')).toBeInTheDocument();
  expect(screen.getByText('DNS for my homelab')).toBeInTheDocument();
  expect(screen.getByText('kudurru.io form')).toBeInTheDocument();
  expect(screen.getByText('Approval required')).toBeInTheDocument();
  expect(screen.getByText('A new license')).toBeInTheDocument();
  expect(screen.getByText('2027-10-12 (UTC)')).toBeInTheDocument();
  expect(screen.getByText('2027-11-11 (UTC)')).toBeInTheDocument();
  expect(screen.getByText(/This link was mailed to admin@example.org and works once/)).toBeInTheDocument();
});

test('Approve posts the decision and says the key went by mail', async () => {
  const { api } = renderApp({
    route,
    me: null,
    handlers: { approvalPreview: () => approvalPreview, approvalDecide: () => ({ status: 'approved', licenseId: 'lic_new01', outcome: 'issued' }) },
  });
  fireEvent.click(await screen.findByRole('button', { name: 'Approve and send the key' }));
  expect(await screen.findByText('Approved: the key is on its way')).toBeInTheDocument();
  expect(api.callsTo('approvalDecide')).toHaveLength(1);
  expect(api.callsTo('approvalDecide')[0]?.body).toEqual({ token: 'tok_approve_1', decision: 'approve' });
  expect(api.callsTo('approvalDecide')[0]?.headers.get('X-Kudurru-CSRF')).toBeNull();
  expect(screen.getByText(/Issued lic_new01\. The key goes to grace@example.org by email/)).toBeInTheDocument();
  expect(screen.getByRole('link', { name: 'Open the license in the portal' })).toHaveAttribute('href', '/licenses/lic_new01');
  expect(screen.queryByText(TOKEN)).not.toBeInTheDocument();
  // Decided: no second click.
  expect(screen.queryByRole('button', { name: 'Approve and send the key' })).not.toBeInTheDocument();
  expect(screen.queryByRole('button', { name: 'Reject' })).not.toBeInTheDocument();
});

test('a renewal shows the license it renews', async () => {
  const preview: ApprovalPreview = { ...approvalPreview, renews: { licenseId: 'lic_example12', expiresOn: '2026-10-30' }, terms: { ...approvalPreview.terms, expiresOn: '2027-10-30' } };
  renderApp({
    route,
    me: null,
    handlers: { approvalPreview: () => preview, approvalDecide: () => ({ status: 'approved', licenseId: 'lic_example12', outcome: 'renewed' }) },
  });
  expect(await screen.findByText('Approving renews')).toBeInTheDocument();
  expect(screen.getByText('lic_example12')).toBeInTheDocument();
  expect(screen.getByText('2026-10-30 (UTC)')).toBeInTheDocument();
  expect(screen.getByText('2027-10-30 (UTC)')).toBeInTheDocument();
  fireEvent.click(screen.getByRole('button', { name: 'Approve and send the key' }));
  expect(await screen.findByText(/Renewed lic_example12\./)).toBeInTheDocument();
});

test('Reject needs a note, which is posted with the decision', async () => {
  const { api } = renderApp({ route, me: null, handlers: { approvalPreview: () => approvalPreview, approvalDecide: () => ({ status: 'rejected' }) } });
  fireEvent.click(await screen.findByRole('button', { name: 'Reject' }));
  const dialog = await screen.findByRole('dialog', { name: 'Reject request' });
  fireEvent.click(within(dialog).getByRole('button', { name: 'Reject and send note' }));
  expect((await within(dialog).findAllByText('Write a note for the requester')).length).toBeGreaterThan(0);
  expect(api.callsTo('approvalDecide')).toHaveLength(0);
  fireEvent.change(within(dialog).getByRole('textbox', { name: /Note to the requester/ }), { target: { value: '  Business use is not covered.  ' } });
  fireEvent.click(within(dialog).getByRole('button', { name: 'Reject and send note' }));
  await waitFor(() => expect(api.callsTo('approvalDecide')).toHaveLength(1));
  expect(api.callsTo('approvalDecide')[0]?.body).toEqual({ token: 'tok_approve_1', decision: 'reject', note: 'Business use is not covered.' });
  expect(await screen.findByText('Rejected')).toBeInTheDocument();
  expect(screen.getByText(/We mailed your note to grace@example.org/)).toBeInTheDocument();
  await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Reject request' })).not.toBeInTheDocument());
  expect(screen.queryByRole('button', { name: 'Approve and send the key' })).not.toBeInTheDocument();
});

test('a note that is too long is refused before anything is sent', async () => {
  const { api } = renderApp({ route, me: null, handlers: { approvalPreview: () => approvalPreview } });
  fireEvent.click(await screen.findByRole('button', { name: 'Reject' }));
  const dialog = await screen.findByRole('dialog', { name: 'Reject request' });
  fireEvent.change(within(dialog).getByRole('textbox', { name: /Note to the requester/ }), { target: { value: 'x'.repeat(1001) } });
  fireEvent.click(within(dialog).getByRole('button', { name: 'Reject and send note' }));
  expect((await within(dialog).findAllByText('At most 1000 characters')).length).toBeGreaterThan(0);
  expect(api.callsTo('approvalDecide')).toHaveLength(0);
});

test('an expired or used link shows the request but cannot decide', async () => {
  renderApp({ route, me: null, handlers: { approvalPreview: () => ({ ...approvalPreview, expired: true }) } });
  expect(await screen.findByText('This approval link no longer works')).toBeInTheDocument();
  expect(screen.getByText(/older than 7 days\. The request is still open/)).toBeInTheDocument();
  expect(screen.getByRole('link', { name: 'Open the requests in the portal' })).toHaveAttribute('href', '/requests');
  expect(screen.getByText('Grace Hopper')).toBeInTheDocument();
  expect(screen.queryByRole('button', { name: 'Approve and send the key' })).not.toBeInTheDocument();
  expect(screen.queryByRole('button', { name: 'Reject' })).not.toBeInTheDocument();
});

test('a request decided meanwhile says who decided and offers no buttons', async () => {
  renderApp({ route, me: null, handlers: { approvalPreview: () => ({ ...approvalPreview, status: 'approved', decidedBy: 'Alan Admin (admin@example.org)', expired: true }) } });
  expect(await screen.findByText('This request was already approved by Alan Admin (admin@example.org)')).toBeInTheDocument();
  expect(screen.queryByRole('button', { name: 'Approve and send the key' })).not.toBeInTheDocument();
  expect(screen.queryByText('This approval link no longer works')).not.toBeInTheDocument();
});

test('a decided request shows the request but not the terms approving would sign', async () => {
  renderApp({ route, me: null, handlers: { approvalPreview: () => ({ ...approvalPreview, status: 'rejected', decidedBy: 'Alan Admin (admin@example.org)', expired: true }) } });
  expect(await screen.findByText('This request was already rejected by Alan Admin (admin@example.org)')).toBeInTheDocument();
  expect(screen.getByText('Grace Hopper')).toBeInTheDocument();
  expect(screen.queryByText('Approving signs')).not.toBeInTheDocument();
  expect(screen.queryByText('Approving renews')).not.toBeInTheDocument();
  expect(screen.queryByText('A new license')).not.toBeInTheDocument();
  expect(screen.queryByText('2027-10-12 (UTC)')).not.toBeInTheDocument();
});

test('a link used meanwhile is explained after the click, and the page shows the state', async () => {
  let decided = false;
  const { api } = renderApp({
    route,
    me: null,
    handlers: {
      approvalPreview: () => (decided ? { ...approvalPreview, status: 'rejected', decidedBy: 'Sam Super (superadmin@example.org)', expired: true } : approvalPreview),
      approvalDecide: () => {
        decided = true;
        return apiError(409, 'conflict', 'This request is already rejected.');
      },
    },
  });
  fireEvent.click(await screen.findByRole('button', { name: 'Approve and send the key' }));
  expect(await screen.findByText('This request was already rejected by Sam Super (superadmin@example.org)')).toBeInTheDocument();
  expect(api.callsTo('approvalPreview')).toHaveLength(2);
  expect(screen.queryByRole('button', { name: 'Approve and send the key' })).not.toBeInTheDocument();
  expect(screen.queryByRole('button', { name: 'Reject' })).not.toBeInTheDocument();
  expect(screen.queryByText('Approving signs')).not.toBeInTheDocument();
});

test('a 409 on a request still open says why approving failed and keeps Reject', async () => {
  const tooFar = 'The person already holds a personal key until 2027-09-30 (UTC), too far ahead to renew it now. Reject this request, or approve it with an earlier expiry.';
  const preview: ApprovalPreview = { ...approvalPreview, renews: { licenseId: 'lic_far01', expiresOn: '2027-09-30' }, terms: { ...approvalPreview.terms, expiresOn: '2028-09-29' } };
  const { api } = renderApp({
    route,
    me: null,
    handlers: {
      approvalPreview: () => preview,
      approvalDecide: (req) => ((req.body as { decision: string }).decision === 'approve' ? apiError(409, 'conflict', tooFar) : { status: 'rejected' }),
    },
  });
  fireEvent.click(await screen.findByRole('button', { name: 'Approve and send the key' }));
  expect(await screen.findByText('The request was not approved')).toBeInTheDocument();
  expect(screen.getByText(tooFar)).toBeInTheDocument();
  // The preview was loaded again and the request is still open: not "already decided".
  await waitFor(() => expect(api.callsTo('approvalPreview')).toHaveLength(2));
  expect(screen.queryByText(/already decided|already approved|already rejected/)).not.toBeInTheDocument();
  expect(screen.getByRole('link', { name: 'Open the requests in the portal' })).toHaveAttribute('href', '/requests');
  expect(screen.getByText('Approving renews')).toBeInTheDocument();
  // Approving with the default terms cannot work; rejecting, as the server suggests, still can.
  expect(screen.getByRole('button', { name: 'Approve and send the key' })).toBeDisabled();
  const reject = screen.getByRole('button', { name: 'Reject' });
  expect(reject).toBeEnabled();
  fireEvent.click(reject);
  const dialog = await screen.findByRole('dialog', { name: 'Reject request' });
  fireEvent.change(within(dialog).getByRole('textbox', { name: /Note to the requester/ }), { target: { value: 'Your key runs well into next year already.' } });
  fireEvent.click(within(dialog).getByRole('button', { name: 'Reject and send note' }));
  expect(await screen.findByText('Rejected')).toBeInTheDocument();
  expect(api.callsTo('approvalDecide')[1]?.body).toEqual({ token: 'tok_approve_1', decision: 'reject', note: 'Your key runs well into next year already.' });
});

test('a reject refused because the request was decided meanwhile closes the dialog and shows the state', async () => {
  let decided = false;
  const { api } = renderApp({
    route,
    me: null,
    handlers: {
      approvalPreview: () => (decided ? { ...approvalPreview, status: 'approved', decidedBy: 'Alan Admin (admin@example.org)', expired: true } : approvalPreview),
      approvalDecide: () => {
        decided = true;
        return apiError(409, 'conflict', 'This request is already decided (approved): the link changes nothing.');
      },
    },
  });
  fireEvent.click(await screen.findByRole('button', { name: 'Reject' }));
  const dialog = await screen.findByRole('dialog', { name: 'Reject request' });
  fireEvent.change(within(dialog).getByRole('textbox', { name: /Note to the requester/ }), { target: { value: 'Business use is not covered.' } });
  fireEvent.click(within(dialog).getByRole('button', { name: 'Reject and send note' }));
  expect(await screen.findByText('This request was already approved by Alan Admin (admin@example.org)')).toBeInTheDocument();
  expect(api.callsTo('approvalPreview')).toHaveLength(2);
  await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Reject request' })).not.toBeInTheDocument());
  expect(screen.queryByRole('button', { name: 'Reject' })).not.toBeInTheDocument();
});

test('a link that expired while the page was open says so after the click', async () => {
  let spent = false;
  const { api } = renderApp({
    route,
    me: null,
    handlers: {
      approvalPreview: () => (spent ? { ...approvalPreview, expired: true } : approvalPreview),
      approvalDecide: () => {
        spent = true;
        return apiError(400, 'invalid_token', 'This approval link expired: links work 7 days. Decide the request in the portal instead.', { hint: 'http://localhost/requests' });
      },
    },
  });
  fireEvent.click(await screen.findByRole('button', { name: 'Approve and send the key' }));
  expect(await screen.findByText(/older than 7 days\. The request is still open/)).toBeInTheDocument();
  expect(api.callsTo('approvalPreview')).toHaveLength(2);
  expect(screen.queryByRole('button', { name: 'Approve and send the key' })).not.toBeInTheDocument();
});

test('a failure that can be retried keeps the buttons', async () => {
  renderApp({
    route,
    me: null,
    handlers: { approvalPreview: () => approvalPreview, approvalDecide: () => apiError(503, 'signing_unavailable', 'No key can be signed right now.') },
  });
  fireEvent.click(await screen.findByRole('button', { name: 'Approve and send the key' }));
  expect(await screen.findByText('Keys cannot be signed right now')).toBeInTheDocument();
  expect(screen.getByText('No key can be signed right now.')).toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Approve and send the key' })).toBeEnabled();
});

test('an unknown token is explained, with the way to the portal', async () => {
  const { api } = renderApp({ route: '/approve?token=nope', me: null, handlers: { approvalPreview: () => apiError(400, 'invalid_token', 'Invalid token') } });
  expect(await screen.findByText('This approval link does not work')).toBeInTheDocument();
  expect(screen.getByRole('link', { name: 'Open the requests in the portal' })).toHaveAttribute('href', '/requests');
  expect(screen.queryByRole('button', { name: 'Approve and send the key' })).not.toBeInTheDocument();
  expect(api.callsTo('approvalDecide')).toHaveLength(0);
});

test('without a token the page asks for the mailed link and asks the API nothing', async () => {
  const { api } = renderApp({ route: '/approve', me: null });
  expect(await screen.findByText('This page needs the link from your mail')).toBeInTheDocument();
  expect(api.calls).toHaveLength(0);
});
