// Business requests on the Requests page (?tab=business): the list says who
// asked for which edition and how many nodes; Issue license opens the issue
// wizard with the request filled in (a new organization customer named
// after the company, its contact email, the edition's plan, the nodes) and
// posts businessRequestId; Close needs a note and posts it. Also Delete user
// on a user's page: confirmed, never for yourself, back to the list.
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import type { CloseBusinessRequestBody, IssueLicenseBody } from '@shared/api';
import { reply } from '../dev/api';
import { renderApp } from '../test/render';
import { adminMe, issuedBusinessRequest, licenseDetail, openBusinessRequest, superMe, userDetail } from '../dev/fixtures';

function businessPage(handlers: Record<string, () => unknown> = {}) {
  return renderApp({
    route: '/requests?tab=business',
    me: adminMe,
    handlers: { businessRequestsList: () => ({ items: [openBusinessRequest, issuedBusinessRequest] }), ...handlers },
  });
}

test('the business tab lists who asked for what, with the license an issued one led to', async () => {
  businessPage();
  expect(await screen.findByText('Example GmbH')).toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Business' })).toHaveAttribute('aria-current');
  expect(screen.getByText('12 nodes')).toBeInTheDocument();
  expect(screen.getByText('Enterprise')).toBeInTheDocument();
  expect(screen.getByText('Commercial Core')).toBeInTheDocument();
  expect(screen.getByRole('link', { name: 'ada@example.org' })).toHaveAttribute('href', 'mailto:ada@example.org');
  expect(screen.getByRole('link', { name: 'lic_example12' })).toHaveAttribute('href', '/licenses/lic_example12');
  // Only the open request can be decided.
  expect(screen.getByRole('button', { name: 'Issue a license for Example GmbH' })).toBeInTheDocument();
  expect(screen.queryByRole('button', { name: 'Issue a license for Initech' })).not.toBeInTheDocument();
});

test('issue license starts from the request and posts businessRequestId', async () => {
  const { api } = businessPage({
    licenseIssue: () => ({ ...licenseDetail, id: 'lic_new123', licensee: 'Example GmbH' }),
    licenseGet: () => ({ ...licenseDetail, id: 'lic_new123', licensee: 'Example GmbH' }),
    customerGet: () => licenseDetail.customer,
    auditList: () => ({ items: [] }),
  });
  fireEvent.click(await screen.findByRole('button', { name: 'Issue a license for Example GmbH' }));
  const form = await screen.findByRole('form', { name: 'Issue license' });
  await within(form).findByRole('heading', { level: 3, name: 'Customer' });
  expect(within(form).getByRole('radio', { name: /New customer/ })).toBeChecked();
  expect(within(form).getByRole('textbox', { name: /Customer name/ })).toHaveValue('Example GmbH');
  expect(within(form).getByRole('textbox', { name: /Contact email/ })).toHaveValue('ada@example.org');
  fireEvent.click(within(form).getByRole('button', { name: 'Next' }));
  await within(form).findByRole('heading', { level: 3, name: 'Plan and limits' });
  expect(within(form).getByRole('radio', { name: /^Enterprise/ })).toBeChecked();
  expect(within(form).getByRole('textbox', { name: /Licensee/ })).toHaveValue('Example GmbH');
  expect(within(form).getByRole('spinbutton', { name: /Nodes/ })).toHaveValue('12');
  fireEvent.click(within(form).getByRole('button', { name: 'Next' }));
  await within(form).findByRole('heading', { level: 3, name: 'Term' });
  fireEvent.click(within(form).getByRole('button', { name: 'Next' }));
  await within(form).findByRole('heading', { level: 3, name: 'Review' });
  expect(within(form).getByText('Example GmbH (ada@example.org): Enterprise, 12 nodes. Issuing marks it issued.')).toBeInTheDocument();
  fireEvent.click(within(form).getByRole('button', { name: 'Sign and issue' }));
  await waitFor(() => expect(api.callsTo('licenseIssue')).toHaveLength(1));
  expect(api.callsTo('licenseIssue')[0]?.body as IssueLicenseBody).toMatchObject({
    licensee: 'Example GmbH',
    plan: 'enterprise',
    nodes: 12,
    features: null,
    newCustomer: { name: 'Example GmbH', kind: 'organization', contactEmail: 'ada@example.org' },
    businessRequestId: 'brq_one',
  });
});

test('close needs a note and posts it', async () => {
  const { api } = businessPage({ businessRequestClose: () => ({ request: { ...openBusinessRequest, status: 'closed', decisionNote: 'Replied by email.' } }) });
  fireEvent.click(await screen.findByRole('button', { name: 'Close the request of Example GmbH' }));
  const dialog = await screen.findByRole('dialog', { name: 'Close business request' });
  fireEvent.click(within(dialog).getByRole('button', { name: 'Close request' }));
  expect((await within(dialog).findAllByText(/Write a note/)).length).toBeGreaterThan(0);
  expect(api.callsTo('businessRequestClose')).toHaveLength(0);
  fireEvent.change(within(dialog).getByRole('textbox', { name: /Note/ }), { target: { value: '  Replied by email.  ' } });
  fireEvent.click(within(dialog).getByRole('button', { name: 'Close request' }));
  await waitFor(() => expect(api.callsTo('businessRequestClose')).toHaveLength(1));
  expect(api.callsTo('businessRequestClose')[0]?.body as CloseBusinessRequestBody).toEqual({ note: 'Replied by email.' });
  expect(api.callsTo('businessRequestClose')[0]?.path).toBe('/api/v1/business-requests/brq_one/close');
});

test('delete user is confirmed, deletes and returns to the list', async () => {
  const { api } = renderApp({
    route: '/users/usr_ops',
    me: superMe,
    handlers: { userGet: () => userDetail, auditList: () => ({ items: [] }), usersList: () => ({ items: [] }), userDelete: () => reply(204) },
  });
  fireEvent.click(await screen.findByRole('button', { name: 'Delete user' }));
  const dialog = await screen.findByRole('alertdialog', { name: 'Delete Olga Ops?' });
  expect(within(dialog).getByText(/removes it from 1 customer/)).toBeInTheDocument();
  expect(within(dialog).getByText(/disable them instead/)).toBeInTheDocument();
  fireEvent.click(within(dialog).getByRole('button', { name: 'Delete user' }));
  await waitFor(() => expect(api.callsTo('userDelete')).toHaveLength(1));
  expect(api.callsTo('userDelete')[0]?.path).toBe('/api/v1/users/usr_ops');
  await waitFor(() => expect(window.location.pathname).toBe('/users'));
});

test('nobody deletes themselves', async () => {
  renderApp({ route: `/users/${superMe.user.id}`, me: superMe, handlers: { userGet: () => ({ ...userDetail, id: superMe.user.id }), auditList: () => ({ items: [] }) } });
  // Disabled with a tooltip that says why (aria-disabled keeps it focusable for the tooltip).
  expect(await screen.findByRole('button', { name: 'Delete user' })).toHaveAttribute('aria-disabled', 'true');
});
