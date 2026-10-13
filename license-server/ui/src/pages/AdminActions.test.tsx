// The licensor's short actions: Withdraw (reason required, the
// keeps-working sentence, a destructive submit), Renew (presets counted
// from the current expiry), Add member (the invite link shown once) and
// Approve (a new person customer by default, the claims in the body).
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import type { ApproveBody, RenewBody, WithdrawBody } from '@shared/api';
import { renderApp } from '../test/render';
import { adminMe, customerDetail, licenseDetail, openRequest, superMe, withdrawnLicenseDetail } from '../dev/fixtures';

const licenseHandlers = { customerGet: () => customerDetail, auditList: () => ({ items: [] }) };

test('withdraw needs a reason, says how long keys keep working and posts the body', async () => {
  const { api } = renderApp({ route: '/licenses/lic_business01', me: adminMe, handlers: { ...licenseHandlers, licenseGet: () => licenseDetail, licenseWithdraw: () => withdrawnLicenseDetail } });
  fireEvent.click(await screen.findByRole('button', { name: 'Withdraw' }));
  const dialog = await screen.findByRole('dialog', { name: 'Withdraw license' });
  expect(within(dialog).getByText('Keys already installed keep working until 2027-11-11 (UTC).')).toBeInTheDocument();
  expect(within(dialog).getByText(/It is final; to undo it, issue a new license/)).toBeInTheDocument();
  fireEvent.click(within(dialog).getByRole('button', { name: 'Withdraw license' }));
  expect((await within(dialog).findAllByText('Say why, for the record')).length).toBeGreaterThan(0);
  expect(api.callsTo('licenseWithdraw')).toHaveLength(0);
  fireEvent.change(within(dialog).getByRole('textbox', { name: /^Reason/ }), { target: { value: 'Contract ended' } });
  fireEvent.change(within(dialog).getByRole('textbox', { name: /Message to the customer/ }), { target: { value: 'Your contract ended.' } });
  fireEvent.click(within(dialog).getByRole('checkbox', { name: /Refuse new personal keys/ }));
  fireEvent.click(within(dialog).getByRole('button', { name: 'Withdraw license' }));
  await waitFor(() => expect(api.callsTo('licenseWithdraw')).toHaveLength(1));
  expect(api.callsTo('licenseWithdraw')[0]?.body as WithdrawBody).toEqual({ reason: 'Contract ended', customerMessage: 'Your contract ended.', blockPersonal: true });
});

test('renew presets count from the current expiry and post a later day', async () => {
  const { api } = renderApp({ route: '/licenses/lic_business01', me: adminMe, handlers: { ...licenseHandlers, licenseGet: () => licenseDetail, licenseRenew: () => licenseDetail } });
  fireEvent.click(await screen.findByRole('button', { name: 'Renew' }));
  const dialog = await screen.findByRole('dialog', { name: 'Renew license' });
  // Current expiry 2027-10-12 is later than today (2026-10-12): one year on is 2028-10-11.
  fireEvent.click(within(dialog).getByRole('button', { name: '1 year' }));
  expect(within(dialog).getByText(/Keeps working on nodes until 2028-11-10 \(UTC\)/)).toBeInTheDocument();
  fireEvent.click(within(dialog).getByRole('button', { name: 'Sign renewed key' }));
  await waitFor(() => expect(api.callsTo('licenseRenew')).toHaveLength(1));
  expect(api.callsTo('licenseRenew')[0]?.body as RenewBody).toEqual({ expiresOn: '2028-10-11' });
});

test('adding a member shows the invite link once', async () => {
  const { api } = renderApp({
    route: '/customers/cus_acme',
    me: adminMe,
    handlers: {
      customerGet: () => customerDetail,
      auditList: () => ({ items: [] }),
      memberAdd: () => ({ member: { ...customerDetail.members[0], email: 'new@acme.example', status: 'invited', user: null }, inviteUrl: 'https://portal.example/invites/secret-token' }),
    },
  });
  fireEvent.click(await screen.findByRole('button', { name: 'Add member' }));
  const dialog = await screen.findByRole('dialog', { name: 'Add a member' });
  fireEvent.change(within(dialog).getByRole('textbox', { name: /Email/ }), { target: { value: 'new@acme.example' } });
  fireEvent.click(within(dialog).getByRole('button', { name: 'Add member' }));
  expect(await within(dialog).findByText('https://portal.example/invites/secret-token')).toBeInTheDocument();
  expect(within(dialog).getByText(/shown only once/)).toBeInTheDocument();
  expect(api.callsTo('memberAdd')[0]?.body).toEqual({ email: 'new@acme.example' });
});

test('approving a request defaults to a new person customer and posts the claims', async () => {
  const { api } = renderApp({
    route: '/requests',
    me: superMe,
    handlers: { requestsList: () => ({ items: [openRequest] }), requestApprove: () => ({ request: { ...openRequest, status: 'approved' }, license: licenseDetail }) },
  });
  fireEvent.click(await screen.findByRole('button', { name: 'Approve the request of Grace Hopper' }));
  const form = await screen.findByRole('form', { name: 'Approve request' });
  expect(within(form).getByRole('radio', { name: /New person customer/ })).toBeChecked();
  fireEvent.click(within(form).getByRole('button', { name: 'Next' }));
  await within(form).findByRole('heading', { level: 3, name: 'Key' });
  expect(within(form).getByRole('textbox', { name: /Licensee/ })).toHaveValue('Grace Hopper');
  fireEvent.click(within(form).getByRole('button', { name: 'Next' }));
  await within(form).findByRole('heading', { level: 3, name: 'Review' });
  expect(within(form).getByText('2027-10-12 (UTC)')).toBeInTheDocument();
  expect(within(form).getByText('2027-11-11 (UTC)')).toBeInTheDocument();
  fireEvent.click(within(form).getByRole('button', { name: 'Approve and send key' }));
  await waitFor(() => expect(api.callsTo('requestApprove')).toHaveLength(1));
  expect(api.callsTo('requestApprove')[0]?.body as ApproveBody).toEqual({ licensee: 'Grace Hopper', expiresOn: '2027-10-12' });
});
