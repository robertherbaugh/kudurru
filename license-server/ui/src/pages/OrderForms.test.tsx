// Order Forms: the list links each form; a draft's page lists the gaps and
// keeps Send disabled while there are any, and each state offers its own
// actions; Send confirms and posts; the customer's Accept needs the
// authority checkbox and posts the SHA-256 it showed (a 409 asks for a
// reload); a contact who is not authorized is told who is; the issue
// wizard opened from an executed form posts orderFormId and customerId;
// the wizard drafts a form from a business request.
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import type { AcceptOrderFormBody, CreateOrderFormBody, IssueLicenseBody, OrderForm } from '@shared/api';
import { apiError } from '../dev/api';
import {
  acceptedOrderForm,
  adminMe,
  customerDetail,
  customerMe,
  DOCUMENT_SHA256,
  draftOrderForm,
  executedOrderForm,
  licenseDetail,
  myOrderForm,
  openBusinessRequest,
  sentOrderForm,
  summaryOf,
} from '../dev/fixtures';
import { renderApp } from '../test/render';
import { authorityStatement } from './MyOrderFormPage';

function orderPage(form: OrderForm, handlers: Record<string, () => unknown> = {}) {
  return renderApp({
    route: `/orders/${form.id}`,
    me: adminMe,
    handlers: { orderFormGet: () => form, auditList: () => ({ items: [] }), customerGet: () => customerDetail, ...handlers },
  });
}

test('the list shows each form with its state and links to it', async () => {
  const { api } = renderApp({ route: '/orders', me: adminMe, handlers: { orderFormsList: () => ({ items: [summaryOf(draftOrderForm), summaryOf(sentOrderForm)] }) } });
  expect(await screen.findByRole('link', { name: 'OF-2026-0001' })).toHaveAttribute('href', '/orders/ord_draft01');
  expect(screen.getByRole('link', { name: 'OF-2026-0002' })).toHaveAttribute('href', '/orders/ord_sent01');
  const table = screen.getByRole('table');
  expect(within(table).getByText('Draft')).toBeInTheDocument();
  expect(within(table).getByText('Sent for acceptance')).toBeInTheDocument();
  expect(screen.getAllByRole('link', { name: 'Acme GmbH' })[0]).toHaveAttribute('href', '/customers/cus_acme');
  expect(screen.getByRole('button', { name: 'New Order Form' })).toBeInTheDocument();
  expect(api.callsTo('orderFormsList')[0]?.query.get('status')).toBeNull();
});

test('a draft with gaps lists them and keeps Send disabled', async () => {
  orderPage(draftOrderForm);
  const gaps = await screen.findByTestId('order-form-gaps');
  expect(within(gaps).getByText('Customer address')).toBeInTheDocument();
  expect(within(gaps).getByText('Governing law and venue')).toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Send' })).toHaveAttribute('aria-disabled', 'true');
  expect(screen.getByRole('button', { name: 'Void' })).toBeInTheDocument();
  expect(screen.queryByRole('button', { name: 'Revise' })).not.toBeInTheDocument();
  // Draft sections are editable inline.
  expect(screen.getByRole('button', { name: 'Edit Legal terms' })).not.toHaveAttribute('aria-disabled', 'true');
});

test('a complete draft is sent after the confirmation', async () => {
  const ready = { ...draftOrderForm, terms: sentOrderForm.terms, gaps: [] };
  const { api } = orderPage(ready, { orderFormSend: () => ({ ...sentOrderForm, id: ready.id }) });
  fireEvent.click(await screen.findByRole('button', { name: 'Send' }));
  const dialog = await screen.findByRole('alertdialog', { name: /Send OF-2026-0001 for acceptance/ });
  expect(within(dialog).getByText(/ops@acme.example, cfo@acme.example/)).toBeInTheDocument();
  fireEvent.click(within(dialog).getByRole('button', { name: 'Send' }));
  await waitFor(() => expect(api.callsTo('orderFormSend')).toHaveLength(1));
  expect(api.callsTo('orderFormSend')[0]?.path).toBe('/api/v1/order-forms/ord_draft01/send');
});

test('each state offers its own actions', async () => {
  const sent = orderPage(sentOrderForm);
  expect(await screen.findByRole('button', { name: 'Revise' })).toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Void' })).toBeInTheDocument();
  expect(screen.queryByRole('button', { name: 'Send' })).not.toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Edit Legal terms' })).toHaveAttribute('aria-disabled', 'true');
  expect(screen.getByTestId('document-sha256')).toHaveTextContent(DOCUMENT_SHA256);
  sent.unmount();

  const accepted = orderPage(acceptedOrderForm);
  expect(await screen.findByRole('button', { name: 'Countersign' })).toBeInTheDocument();
  expect(screen.queryByRole('button', { name: 'Revise' })).not.toBeInTheDocument();
  expect(within(screen.getByRole('region', { name: 'Acceptance' })).getByText(/Olga Ops, Head of Operations/)).toBeInTheDocument();
  accepted.unmount();

  orderPage(executedOrderForm);
  expect(await screen.findByRole('button', { name: 'Issue license' })).toBeInTheDocument();
  expect(screen.queryByRole('button', { name: 'Void' })).not.toBeInTheDocument();
  expect(screen.getByRole('button', { name: 'Download' })).toBeInTheDocument();
});

test('countersigning posts the hash the customer accepted', async () => {
  const { api } = orderPage(acceptedOrderForm, { orderFormCountersign: () => executedOrderForm });
  fireEvent.click(await screen.findByRole('button', { name: 'Countersign' }));
  const dialog = await screen.findByRole('dialog', { name: 'Countersign OF-2026-0003' });
  fireEvent.change(within(dialog).getByRole('textbox', { name: /^Title/ }), { target: { value: 'Licensor' } });
  fireEvent.click(within(dialog).getByRole('button', { name: 'Countersign' }));
  await waitFor(() => expect(api.callsTo('orderFormCountersign')).toHaveLength(1));
  expect(api.callsTo('orderFormCountersign')[0]?.body).toEqual({ name: 'Alan Admin', title: 'Licensor', documentSha256: DOCUMENT_SHA256 });
});

function myPage(handlers: Record<string, () => unknown> = {}) {
  return renderApp({ route: `/my/orders/${myOrderForm.id}`, me: { ...customerMe, user: { ...customerMe.user, name: 'Olga Ops', email: 'ops@acme.example' } }, handlers: { myOrderForm: () => myOrderForm, ...handlers } });
}

test('accepting needs the authority checkbox and posts the hash shown', async () => {
  const { api } = myPage({ myOrderFormAccept: () => ({ ...myOrderForm, status: 'accepted' }) });
  fireEvent.click(await screen.findByRole('button', { name: 'Accept' }));
  const dialog = await screen.findByRole('dialog', { name: /Accept Order Form OF-2026-0002/ });
  expect(within(dialog).getByText(DOCUMENT_SHA256)).toBeInTheDocument();
  expect(within(dialog).getByRole('textbox', { name: /Full name/ })).toHaveValue('Olga Ops');
  fireEvent.change(within(dialog).getByRole('textbox', { name: /^Title/ }), { target: { value: 'CTO' } });
  fireEvent.click(within(dialog).getByRole('button', { name: 'Accept Order Form' }));
  expect((await within(dialog).findAllByText('Tick the statement to accept the Order Form')).length).toBeGreaterThan(0);
  expect(api.callsTo('myOrderFormAccept')).toHaveLength(0);
  fireEvent.click(within(dialog).getByRole('checkbox', { name: new RegExp(`^${authorityStatement('Acme GmbH')}`) }));
  fireEvent.click(within(dialog).getByRole('button', { name: 'Accept Order Form' }));
  await waitFor(() => expect(api.callsTo('myOrderFormAccept')).toHaveLength(1));
  expect(api.callsTo('myOrderFormAccept')[0]?.body as AcceptOrderFormBody).toEqual({ name: 'Olga Ops', title: 'CTO', documentSha256: DOCUMENT_SHA256, authorized: true });
});

test('a form revised in the meantime asks for a reload', async () => {
  myPage({ myOrderFormAccept: () => apiError(409, 'conflict', 'The Order Form changed') });
  fireEvent.click(await screen.findByRole('button', { name: 'Accept' }));
  const dialog = await screen.findByRole('dialog', { name: /Accept Order Form/ });
  fireEvent.change(within(dialog).getByRole('textbox', { name: /^Title/ }), { target: { value: 'CTO' } });
  fireEvent.click(within(dialog).getByRole('checkbox', { name: /I am authorized to bind/ }));
  fireEvent.click(within(dialog).getByRole('button', { name: 'Accept Order Form' }));
  expect(await within(dialog).findByText('This Order Form changed')).toBeInTheDocument();
  expect(within(dialog).getByRole('button', { name: 'Reload' })).toBeInTheDocument();
});

test('declining needs a note; a contact who is not authorized is told who is', async () => {
  const first = myPage({ myOrderFormDecline: () => ({ ...myOrderForm, status: 'declined' }) });
  fireEvent.click(await screen.findByRole('button', { name: 'Decline' }));
  const dialog = await screen.findByRole('dialog', { name: /Decline Order Form/ });
  fireEvent.click(within(dialog).getByRole('button', { name: 'Decline Order Form' }));
  expect((await within(dialog).findAllByText('Write a note for the licensor')).length).toBeGreaterThan(0);
  fireEvent.change(within(dialog).getByRole('textbox', { name: /Note to the licensor/ }), { target: { value: 'Too many nodes.' } });
  fireEvent.click(within(dialog).getByRole('button', { name: 'Decline Order Form' }));
  await waitFor(() => expect(first.api.callsTo('myOrderFormDecline')).toHaveLength(1));
  expect(first.api.callsTo('myOrderFormDecline')[0]?.body).toEqual({ note: 'Too many nodes.' });
  first.unmount();

  myPage({ myOrderForm: () => ({ ...myOrderForm, authorized: false }) });
  expect(await screen.findByText('Only its authorized contacts can accept or decline it')).toBeInTheDocument();
  expect(screen.getByText(/The authorized contacts are: ops@acme.example, cfo@acme.example/)).toBeInTheDocument();
  expect(screen.queryByRole('button', { name: 'Accept' })).not.toBeInTheDocument();
});

test('my licenses lists my Order Forms even without a license', async () => {
  renderApp({ route: '/my/licenses', me: customerMe, handlers: { myLicenses: () => ({ items: [] }), myRequests: () => ({ items: [] }), myOrderForms: () => ({ items: [myOrderForm] }) } });
  const panel = await screen.findByRole('region', { name: 'Order Forms' });
  expect(within(panel).getByRole('link', { name: 'OF-2026-0002' })).toHaveAttribute('href', '/my/orders/ord_sent01');
  expect(within(panel).getByText('Waiting for your acceptance')).toBeInTheDocument();
  expect(screen.getByText('No licenses yet')).toBeInTheDocument();
});

test('issuing from an executed form fixes its customer and posts orderFormId and customerId', async () => {
  const { api } = orderPage(executedOrderForm, {
    licenseIssue: () => ({ ...licenseDetail, id: 'lic_new123', orderForm: { id: executedOrderForm.id, number: executedOrderForm.number } }),
    licenseGet: () => ({ ...licenseDetail, id: 'lic_new123' }),
  });
  fireEvent.click(await screen.findByRole('button', { name: 'Issue license' }));
  const form = await screen.findByRole('form', { name: 'Issue license' });
  await within(form).findByRole('heading', { level: 3, name: 'Customer' });
  expect(within(form).queryByRole('radio', { name: /New customer/ })).not.toBeInTheDocument();
  expect(within(form).getByText('OF-2026-0004 (version 1)')).toBeInTheDocument();
  fireEvent.click(within(form).getByRole('button', { name: 'Next' }));
  await within(form).findByRole('heading', { level: 3, name: 'Plan and limits' });
  expect(within(form).getByRole('radio', { name: /^Enterprise/ })).toBeChecked();
  expect(within(form).getByRole('textbox', { name: /Licensee/ })).toHaveValue('Acme GmbH');
  expect(within(form).getByRole('spinbutton', { name: /Nodes/ })).toHaveValue('10');
  fireEvent.click(within(form).getByRole('button', { name: 'Next' }));
  await within(form).findByRole('heading', { level: 3, name: 'Term' });
  expect(within(form).getByRole('textbox', { name: /Agreement reference/ })).toHaveValue('OF-2026-0004');
  fireEvent.click(within(form).getByRole('button', { name: 'Next' }));
  await within(form).findByRole('heading', { level: 3, name: 'Review' });
  fireEvent.click(within(form).getByRole('button', { name: 'Sign and issue' }));
  await waitFor(() => expect(api.callsTo('licenseIssue')).toHaveLength(1));
  const body = api.callsTo('licenseIssue')[0]?.body as IssueLicenseBody;
  expect(body).toMatchObject({
    customerId: 'cus_acme',
    orderFormId: 'ord_executed01',
    plan: 'enterprise',
    nodes: 10,
    features: null,
    expiresOn: executedOrderForm.terms.subscriptionEnd,
    agreementRef: 'OF-2026-0004',
  });
  expect(body.businessRequestId).toBeUndefined();
  expect(body.newCustomer).toBeUndefined();
});

test('a form without modules leaves the features to the admin', async () => {
  const core = { ...executedOrderForm, edition: 'core' as const, terms: { ...executedOrderForm.terms, edition: 'core' as const, modules: 'none' as const } };
  orderPage(core);
  fireEvent.click(await screen.findByRole('button', { name: 'Issue license' }));
  const form = await screen.findByRole('form', { name: 'Issue license' });
  fireEvent.click(within(form).getByRole('button', { name: 'Next' }));
  await within(form).findByRole('heading', { level: 3, name: 'Plan and limits' });
  expect(within(form).getByRole('radio', { name: /^Business/ })).toBeChecked();
  expect(within(form).getByText(/includes no Enterprise modules/)).toBeInTheDocument();
  expect(within(form).getByRole('radio', { name: /Only these features/ })).toBeChecked();
});

test('drafting from a business request starts from the request and posts businessRequestId', async () => {
  const { api } = renderApp({
    route: '/requests?tab=business',
    me: adminMe,
    handlers: {
      businessRequestsList: () => ({ items: [{ ...openBusinessRequest, orderForm: { id: 'ord_sent01', number: 'OF-2026-0002', status: 'sent' } }] }),
      orderFormCreate: () => ({ ...draftOrderForm, id: 'ord_new1' }),
      orderFormGet: () => ({ ...draftOrderForm, id: 'ord_new1' }),
      auditList: () => ({ items: [] }),
    },
  });
  // The State column links the newest form drafted from the request.
  expect(await screen.findByRole('link', { name: 'Order Form OF-2026-0002' })).toHaveAttribute('href', '/orders/ord_sent01');
  fireEvent.click(screen.getByRole('button', { name: 'Draft an Order Form for Example GmbH' }));
  const form = await screen.findByRole('form', { name: 'New Order Form' });
  await within(form).findByRole('heading', { level: 3, name: 'Customer' });
  expect(within(form).getByRole('radio', { name: /New organization/ })).toBeChecked();
  expect(within(form).getByRole('textbox', { name: /Organization name/ })).toHaveValue('Example GmbH');
  const next = () => fireEvent.click(within(form).getByRole('button', { name: 'Next' }));
  next();
  await within(form).findByRole('heading', { level: 3, name: 'Contacts and parties' });
  expect(within(form).getByRole('textbox', { name: /Customer legal name/ })).toHaveValue('Example GmbH');
  expect(within(form).getByRole('textbox', { name: 'Authorized contacts 1' })).toHaveValue('ada@example.org');
  next();
  await within(form).findByRole('heading', { level: 3, name: 'Plan and scope' });
  expect(within(form).getByRole('radio', { name: /^Enterprise/ })).toBeChecked();
  next();
  await within(form).findByRole('heading', { level: 3, name: 'Term and fees' });
  // Enterprise, 12 nodes: $499 + 12 × $299 = $4,087.
  expect(within(form).getByRole('textbox', { name: /^Annual fee/ })).toHaveValue('4087.00');
  next();
  await within(form).findByRole('heading', { level: 3, name: 'Legal terms' });
  next();
  await within(form).findByRole('heading', { level: 3, name: 'Review' });
  expect(within(form).getByText(/Customer address, Governing law and venue/)).toBeInTheDocument();
  fireEvent.click(within(form).getByRole('button', { name: 'Create draft' }));
  await waitFor(() => expect(api.callsTo('orderFormCreate')).toHaveLength(1));
  const body = api.callsTo('orderFormCreate')[0]?.body as CreateOrderFormBody;
  expect(body).toMatchObject({ businessRequestId: 'brq_one', newCustomer: { name: 'Example GmbH', kind: 'organization', contactEmail: 'ada@example.org' } });
  expect(body.terms).toMatchObject({ edition: 'enterprise', modules: 'all', nodes: 12, annualFeeCents: 408_700, authorizedContacts: ['ada@example.org'], customerLegalName: 'Example GmbH' });
  await waitFor(() => expect(window.location.pathname).toBe('/orders/ord_new1'));
});

test('a draft section saves only its own terms', async () => {
  const { api } = orderPage(draftOrderForm, { orderFormUpdate: () => draftOrderForm });
  fireEvent.click(await screen.findByRole('button', { name: 'Edit Legal terms' }));
  const section = screen.getByRole('region', { name: 'Legal terms' });
  fireEvent.change(within(section).getByRole('textbox', { name: /Governing law and venue/ }), { target: { value: '  The laws of Delaware.  ' } });
  fireEvent.click(within(section).getByRole('button', { name: 'Save Legal terms' }));
  await waitFor(() => expect(api.callsTo('orderFormUpdate')).toHaveLength(1));
  const body = api.callsTo('orderFormUpdate')[0]?.body as { terms: Record<string, unknown> };
  expect(Object.keys(body.terms).sort()).toEqual(['governingLaw', 'keyPolicy', 'renewal', 'specialTerms', 'support']);
  expect(body.terms.governingLaw).toBe('The laws of Delaware.');
});

test('the customer page lists its Order Forms with New Order Form', async () => {
  renderApp({
    route: '/customers/cus_acme',
    me: adminMe,
    handlers: { customerGet: () => customerDetail, auditList: () => ({ items: [] }), orderFormsList: () => ({ items: [summaryOf(sentOrderForm)] }) },
  });
  const panel = await screen.findByRole('region', { name: 'Order Forms' });
  expect(await within(panel).findByRole('link', { name: 'OF-2026-0002' })).toHaveAttribute('href', '/orders/ord_sent01');
  expect(within(panel).getByRole('button', { name: 'New Order Form' })).toBeInTheDocument();
});
