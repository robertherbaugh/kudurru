// The issue wizard: an explicit features list is never empty (Next stays
// put with the message and the request never carries features: []), all
// seven ticked shows the fixed-list note, the review states every claim
// with Expires (UTC) and Keeps working until (UTC), and signing posts the
// claims and opens the new license.
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import type { IssueLicenseBody } from '@shared/api';
import { FEATURES } from '@shared/catalog';
import { EMPTY_LIST_ERROR, FIXED_LIST_NOTE } from '../ui/fields/FeaturesField';
import { renderApp } from '../test/render';
import { adminMe, customerDetail, licenseDetail } from '../dev/fixtures';

function wizard(): HTMLElement {
  return screen.getByRole('form', { name: 'Issue license' });
}

async function stepHeading(name: string): Promise<HTMLElement> {
  const form = await screen.findByRole('form', { name: 'Issue license' });
  return within(form).findByRole('heading', { level: 3, name });
}

function next() {
  fireEvent.click(within(wizard()).getByRole('button', { name: 'Next' }));
}

function open() {
  return renderApp({
    route: '/licenses?new=1',
    me: adminMe,
    handlers: {
      licensesList: () => ({ items: [], total: 0 }),
      customersList: () => ({ items: [customerDetail], total: 1 }),
      licenseIssue: () => ({ ...licenseDetail, id: 'lic_new123', licensee: 'Example GmbH' }),
      licenseGet: () => ({ ...licenseDetail, id: 'lic_new123', licensee: 'Example GmbH' }),
      customerGet: () => customerDetail,
      auditList: () => ({ items: [] }),
    },
  });
}

async function newCustomerStep() {
  await stepHeading('Customer');
  fireEvent.click(within(wizard()).getByRole('radio', { name: /New customer/ }));
  fireEvent.change(within(wizard()).getByRole('textbox', { name: /Customer name/ }), { target: { value: 'Example GmbH' } });
  next();
  await stepHeading('Plan and limits');
}

test('an explicit features list is never empty, and all seven is a fixed list', async () => {
  const { api } = open();
  await newCustomerStep();
  fireEvent.click(within(wizard()).getByRole('radio', { name: /Only these features/ }));
  // Nothing ticked yet: the field says so and Next stays on the step.
  expect((await within(wizard()).findAllByText(EMPTY_LIST_ERROR)).length).toBeGreaterThan(0);
  next();
  await waitFor(() => expect(within(wizard()).getAllByText(EMPTY_LIST_ERROR).length).toBeGreaterThan(0));
  expect(within(wizard()).getByRole('heading', { level: 3, name: 'Plan and limits' })).toBeInTheDocument();
  expect(within(wizard()).queryByRole('heading', { level: 3, name: 'Term' })).not.toBeInTheDocument();
  // Every feature ticked: the note says later features are not included.
  for (const f of FEATURES) fireEvent.click(within(wizard()).getByRole('checkbox', { name: new RegExp(`^${f.label}`) }));
  expect(within(wizard()).getByTestId('features-fixed-note')).toHaveTextContent(FIXED_LIST_NOTE);
  // Unticking all again brings the error back.
  for (const f of FEATURES) fireEvent.click(within(wizard()).getByRole('checkbox', { name: new RegExp(`^${f.label}`) }));
  expect(within(wizard()).getAllByText(EMPTY_LIST_ERROR).length).toBeGreaterThan(0);
  next();
  await waitFor(() => expect(within(wizard()).getByRole('heading', { level: 3, name: 'Plan and limits' })).toBeInTheDocument());
  expect(api.callsTo('licenseIssue')).toHaveLength(0);
});

test('the review shows every claim with UTC dates, and signing posts them', async () => {
  const { api } = open();
  await newCustomerStep();
  // Licensee follows the new customer's name; pick one feature.
  expect(within(wizard()).getByRole('textbox', { name: /Licensee/ })).toHaveValue('Example GmbH');
  fireEvent.click(within(wizard()).getByRole('radio', { name: /Only these features/ }));
  fireEvent.click(within(wizard()).getByRole('checkbox', { name: /^Single sign-on/ }));
  next();
  await stepHeading('Term');
  fireEvent.click(within(wizard()).getByRole('button', { name: '2 years' }));
  next();
  await stepHeading('Review');
  expect(within(wizard()).getByTestId('review-expires')).toHaveTextContent('2028-10-11 (UTC)');
  expect(within(wizard()).getByTestId('review-keeps-working')).toHaveTextContent('2028-11-10 (UTC)');
  expect(within(wizard()).getByText('Single sign-on')).toBeInTheDocument();
  expect(within(wizard()).getByText('Business')).toBeInTheDocument();
  expect(within(wizard()).getByText(/Example GmbH \(new organization\)/)).toBeInTheDocument();
  fireEvent.click(within(wizard()).getByRole('button', { name: 'Sign and issue' }));
  await waitFor(() => expect(api.callsTo('licenseIssue')).toHaveLength(1));
  const body = api.callsTo('licenseIssue')[0]?.body as IssueLicenseBody;
  expect(body).toMatchObject({ licensee: 'Example GmbH', plan: 'business', nodes: 1, features: ['oidc'], expiresOn: '2028-10-11', newCustomer: { name: 'Example GmbH', kind: 'organization', contactEmail: null } });
  expect(body.customerId).toBeUndefined();
  expect(api.callsTo('licenseIssue')[0]?.headers.get('X-Kudurru-CSRF')).toBe('csrf-token-1');
  await waitFor(() => expect(window.location.pathname).toBe('/licenses/lic_new123'));
});

test('a personal key keeps every feature and 3 nodes; all features sends null', async () => {
  const { api } = open();
  await newCustomerStep();
  fireEvent.click(within(wizard()).getByRole('radio', { name: /^Personal/ }));
  expect(within(wizard()).getByRole('radio', { name: /Only these features/ })).toBeDisabled();
  next();
  await stepHeading('Term');
  fireEvent.click(within(wizard()).getByRole('button', { name: '1 year' }));
  next();
  await stepHeading('Review');
  expect(within(wizard()).getByTestId('review-expires')).toHaveTextContent('2027-10-12 (UTC)');
  fireEvent.click(within(wizard()).getByRole('button', { name: 'Sign and issue' }));
  await waitFor(() => expect(api.callsTo('licenseIssue')).toHaveLength(1));
  expect(api.callsTo('licenseIssue')[0]?.body).toMatchObject({ plan: 'personal', nodes: 3, features: null, expiresOn: '2027-10-12' });
});
