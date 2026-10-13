// The portal's "Request a free personal key" dialog: the certification
// must be ticked (nothing is sent until it is), the body carries
// noncommercial: true, the outcome is shown in the dialog, and a sign-in
// without a verified email is told to use the public form instead.
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { ATTESTATION_STATEMENT } from '@shared/catalog';
import { renderApp } from '../test/render';
import { customerMe, me } from '../dev/fixtures';

const empty = { myLicenses: () => ({ items: [] }), myRequests: () => ({ items: [] }) };

async function openDialog(): Promise<HTMLElement> {
  fireEvent.click(await screen.findByRole('button', { name: 'Request a free personal key' }));
  return screen.findByRole('dialog', { name: 'Request a free personal key' });
}

test('the certification is required before anything is sent', async () => {
  const { api } = renderApp({ route: '/my/licenses', me: customerMe, handlers: { ...empty, myRequestCreate: () => ({ outcome: 'issued', licenseId: 'lic_new' }) } });
  const dialog = await openDialog();
  expect(within(dialog).getByRole('textbox', { name: /Name on the key/ })).toHaveValue('Ada Lovelace');
  fireEvent.change(within(dialog).getByRole('textbox', { name: /What do you run kudurru for/ }), { target: { value: 'DNS for my home' } });
  fireEvent.click(within(dialog).getByRole('button', { name: 'Request key' }));
  expect((await within(dialog).findAllByText('Tick the certification to request a free personal key')).length).toBeGreaterThan(0);
  expect(api.callsTo('myRequestCreate')).toHaveLength(0);

  fireEvent.click(within(dialog).getByRole('checkbox', { name: ATTESTATION_STATEMENT }));
  fireEvent.click(within(dialog).getByRole('button', { name: 'Request key' }));
  await waitFor(() => expect(api.callsTo('myRequestCreate')).toHaveLength(1));
  expect(api.callsTo('myRequestCreate')[0]?.body).toEqual({ name: 'Ada Lovelace', use: 'DNS for my home', noncommercial: true });
  expect(await within(dialog).findByText('Your free personal key is ready')).toBeInTheDocument();
  expect(within(dialog).getByRole('link', { name: 'Open the license' })).toHaveAttribute('href', '/my/licenses/lic_new');
});

test('a request that waits for review says so', async () => {
  renderApp({ route: '/my/licenses', me: customerMe, handlers: { ...empty, myRequestCreate: () => ({ outcome: 'pending_review', requestId: 'req_1' }) } });
  const dialog = await openDialog();
  fireEvent.change(within(dialog).getByRole('textbox', { name: /What do you run kudurru for/ }), { target: { value: 'A school lab' } });
  fireEvent.click(within(dialog).getByRole('checkbox', { name: ATTESTATION_STATEMENT }));
  fireEvent.click(within(dialog).getByRole('button', { name: 'Request key' }));
  expect(await within(dialog).findByText('Your request waits for review')).toBeInTheDocument();
});

test('without a verified email the dialog points at the public form', async () => {
  const { api } = renderApp({ route: '/my/licenses', me: me(['customer'], { email: null, emailVerified: false }), handlers: empty });
  const dialog = await openDialog();
  expect(within(dialog).getByText('Your sign-in did not share a verified email address')).toBeInTheDocument();
  expect(within(dialog).getByRole('link', { name: /Public form/ })).toHaveAttribute('href', 'https://kudurru.io/license');
  expect(within(dialog).getByRole('button', { name: 'Request key' })).toHaveAttribute('aria-disabled', 'true');
  expect(api.callsTo('myRequestCreate')).toHaveLength(0);
});

test('the empty page offers the ways in', async () => {
  renderApp({ route: '/my/licenses', me: customerMe, handlers: empty });
  expect(await screen.findByText('No licenses yet')).toBeInTheDocument();
  expect(screen.getByText(/ask us \(hello@kudurru.io\) to add ada@example.org/)).toBeInTheDocument();
  expect(screen.getByRole('link', { name: 'Ask about a business license' })).toHaveAttribute('href', 'mailto:hello@kudurru.io?subject=Business%20License');
});
