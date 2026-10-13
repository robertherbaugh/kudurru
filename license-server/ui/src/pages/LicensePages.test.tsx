// The license detail pages: the customer's (/my/licenses/:id) and the
// admin's (/licenses/:id). Both show the current key in the key panel with
// the download link, the notices and UTC dates; a withdrawn license shows
// no key, only when it was withdrawn and how long installed keys keep
// working. A personal key in its renew window offers "Request renewal"
// (approval required, the default) or "Renew" (automatic approval), both
// behind the certification ticked again.
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { ATTESTATION_STATEMENT, KEY_IS_NOT_A_LICENSE } from '@shared/catalog';
import { apiError } from '../dev/api';
import { NONCOMMERCIAL_NOTICE } from '../components/keys';
import { renderApp } from '../test/render';
import { adminMe, catalog, customerDetail, customerMe, licenseDetail, me, myLicenseDetail, myWithdrawnDetail, TOKEN, withdrawnLicenseDetail } from '../dev/fixtures';

const inWindow = { ...myLicenseDetail, renewal: { expiresOn: '2028-10-11' } };

describe('customer license page', () => {
  test('shows the key, the download, the notices and the facts in UTC', async () => {
    renderApp({ route: '/my/licenses/lic_example12', me: customerMe, handlers: { myLicense: () => myLicenseDetail } });
    const panel = await screen.findByTestId('key-panel');
    expect(within(panel).getByText(TOKEN)).toBeInTheDocument();
    expect(within(panel).getByRole('link', { name: 'Download license.key' })).toHaveAttribute('href', '/api/v1/me/licenses/lic_example12/key');
    expect(within(panel).getByRole('button', { name: 'Copy key' })).toBeInTheDocument();
    expect(within(panel).getByTestId('noncommercial-notice')).toHaveTextContent(NONCOMMERCIAL_NOTICE);
    expect(within(panel).getByTestId('key-not-license')).toHaveTextContent(KEY_IS_NOT_A_LICENSE);
    const facts = screen.getByRole('region', { name: 'License' });
    expect(within(facts).getByText('lic_example12')).toBeInTheDocument();
    expect(within(facts).getByText('2027-10-12 (UTC)')).toBeInTheDocument();
    expect(within(facts).getByText('2027-11-11 (UTC)')).toBeInTheDocument();
    expect(within(facts).getByText('kudurru v0.1.0 or later')).toBeInTheDocument();
    expect(screen.getByRole('region', { name: 'Add the key to kudurru' })).toHaveTextContent('kubectl rollout restart');
    expect(screen.getByRole('region', { name: 'People with access' })).toHaveTextContent('charles@example.org');
  });

  test('a withdrawn license hides the key and says how long installed keys keep working', async () => {
    renderApp({ route: '/my/licenses/lic_example12', me: customerMe, handlers: { myLicense: () => myWithdrawnDetail } });
    expect(await screen.findByText('Withdrawn on 2026-09-01 (UTC). Keys already installed keep working until 2027-11-11 (UTC).')).toBeInTheDocument();
    expect(screen.getByText('This key was used commercially.')).toBeInTheDocument();
    expect(screen.queryByTestId('key-panel')).not.toBeInTheDocument();
    expect(screen.queryByText(TOKEN)).not.toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'Download license.key' })).not.toBeInTheDocument();
    expect(screen.queryByRole('region', { name: 'Add the key to kudurru' })).not.toBeInTheDocument();
    // The notices stay with the facts even without a key.
    expect(screen.getByTestId('key-not-license')).toBeInTheDocument();
  });

  test('a personal key in its renew window offers Request renewal (Renew with automatic approval); a business key offers the mailto', async () => {
    const first = renderApp({ route: '/my/licenses/lic_example12', me: customerMe, handlers: { myLicense: () => inWindow } });
    expect(await screen.findByRole('button', { name: 'Request renewal' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Renew' })).not.toBeInTheDocument();
    first.unmount();
    const second = renderApp({ route: '/my/licenses/lic_example12', me: customerMe, catalog: catalog({ approvalRequired: false }), handlers: { myLicense: () => inWindow } });
    expect(await screen.findByRole('button', { name: 'Renew' })).toBeInTheDocument();
    second.unmount();
    renderApp({ route: '/my/licenses/lic_example12', me: customerMe, handlers: { myLicense: () => ({ ...myLicenseDetail, plan: 'business', nodes: 10 }) } });
    const ask = await screen.findByRole('link', { name: 'Ask about a renewal or a change' });
    expect(ask).toHaveAttribute('href', 'mailto:hello@kudurru.io?subject=kudurru%20commercial%20license%20lic_example12');
    expect(screen.queryByRole('button', { name: /Renew|renewal/ })).not.toBeInTheDocument();
  });

  test('a renewal needs the certification again, then waits for approval', async () => {
    const { api } = renderApp({
      route: '/my/licenses/lic_example12',
      me: customerMe,
      handlers: { myLicense: () => inWindow, myLicenseRenew: () => ({ outcome: 'pending_review', requestId: 'req_renew' }) },
    });
    fireEvent.click(await screen.findByRole('button', { name: 'Request renewal' }));
    const dialog = await screen.findByRole('dialog', { name: 'Request a renewal of your personal key' });
    expect(within(dialog).getByText(/Once a license admin approves the renewal, the new key arrives by email at ada@example.org/)).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole('button', { name: 'Request renewal' }));
    expect((await within(dialog).findAllByText('Tick the certification to request a renewal')).length).toBeGreaterThan(0);
    expect(api.callsTo('myLicenseRenew')).toHaveLength(0);

    fireEvent.click(within(dialog).getByRole('checkbox', { name: ATTESTATION_STATEMENT }));
    fireEvent.click(within(dialog).getByRole('button', { name: 'Request renewal' }));
    await waitFor(() => expect(api.callsTo('myLicenseRenew')).toHaveLength(1));
    expect(api.callsTo('myLicenseRenew')[0]?.path).toBe('/api/v1/me/licenses/lic_example12/renew');
    expect(api.callsTo('myLicenseRenew')[0]?.body).toEqual({ noncommercial: true });
    expect(await within(dialog).findByText('Your request is waiting for approval')).toBeInTheDocument();
    expect(within(dialog).getByText('Your key arrives by email once it is approved.')).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole('button', { name: 'Done' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Request a renewal of your personal key' })).not.toBeInTheDocument());
  });

  test('with automatic approval Renew signs at once and closes with a toast', async () => {
    const { api } = renderApp({
      route: '/my/licenses/lic_example12',
      me: customerMe,
      catalog: catalog({ approvalRequired: false }),
      handlers: { myLicense: () => inWindow, myLicenseRenew: () => ({ outcome: 'renewed', licenseId: 'lic_example12' }) },
    });
    fireEvent.click(await screen.findByRole('button', { name: 'Renew' }));
    const dialog = await screen.findByRole('dialog', { name: 'Renew your personal key' });
    fireEvent.click(within(dialog).getByRole('checkbox', { name: ATTESTATION_STATEMENT }));
    fireEvent.click(within(dialog).getByRole('button', { name: 'Renew' }));
    await waitFor(() => expect(api.callsTo('myLicenseRenew')).toHaveLength(1));
    expect(api.callsTo('myLicenseRenew')[0]?.body).toEqual({ noncommercial: true });
    expect(await screen.findByText('Renewed: the new key is valid through 2028-10-11 (UTC)')).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Renew your personal key' })).not.toBeInTheDocument());
  });

  test('a renewal without a verified email is not offered to submit, and a refused certification shows under the box', async () => {
    const first = renderApp({ route: '/my/licenses/lic_example12', me: me(['customer'], { email: null, emailVerified: false }), handlers: { myLicense: () => inWindow } });
    fireEvent.click(await screen.findByRole('button', { name: 'Request renewal' }));
    let dialog = await screen.findByRole('dialog', { name: 'Request a renewal of your personal key' });
    expect(within(dialog).getByText('Your sign-in did not share a verified email address')).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: 'Request renewal' })).toHaveAttribute('aria-disabled', 'true');
    expect(first.api.callsTo('myLicenseRenew')).toHaveLength(0);
    first.unmount();

    renderApp({
      route: '/my/licenses/lic_example12',
      me: customerMe,
      handlers: { myLicense: () => inWindow, myLicenseRenew: () => apiError(400, 'invalid', 'Tick the certification: noncommercial must be true.', { fields: ['noncommercial'] }) },
    });
    fireEvent.click(await screen.findByRole('button', { name: 'Request renewal' }));
    dialog = await screen.findByRole('dialog', { name: 'Request a renewal of your personal key' });
    fireEvent.click(within(dialog).getByRole('checkbox', { name: ATTESTATION_STATEMENT }));
    fireEvent.click(within(dialog).getByRole('button', { name: 'Request renewal' }));
    expect((await within(dialog).findAllByText('Tick the certification: noncommercial must be true.')).length).toBeGreaterThan(0);
  });
});

describe('admin license page', () => {
  const handlers = { customerGet: () => customerDetail, auditList: () => ({ items: [] }) };

  test('shows the current key with its download, the actions and the key history', async () => {
    renderApp({ route: '/licenses/lic_business01', me: adminMe, handlers: { ...handlers, licenseGet: () => licenseDetail } });
    const panel = await screen.findByTestId('key-panel');
    expect(within(panel).getByText(TOKEN)).toBeInTheDocument();
    expect(within(panel).getByRole('link', { name: 'Download license.key' })).toHaveAttribute('href', '/api/v1/licenses/lic_business01/keys/key_current/download');
    expect(within(panel).getByTestId('key-not-license')).toBeInTheDocument();
    expect(within(panel).queryByTestId('noncommercial-notice')).not.toBeInTheDocument();
    for (const action of ['Renew', 'Withdraw', 'Email key']) expect(screen.getByRole('button', { name: action })).toBeInTheDocument();
    const history = screen.getByRole('region', { name: 'Key history' });
    expect(within(history).getByText('Current key')).toBeInTheDocument();
    expect(within(history).getByRole('link', { name: 'Download the key signed 2025-10-12 (UTC)' })).toHaveAttribute('href', '/api/v1/licenses/lic_business01/keys/key_old/download');
    const terms = screen.getByRole('region', { name: 'Terms' });
    expect(within(terms).getByText('Roles and access control, Single sign-on')).toBeInTheDocument();
  });

  test('a withdrawn license hides the key and the actions', async () => {
    renderApp({ route: '/licenses/lic_business01', me: adminMe, handlers: { ...handlers, licenseGet: () => withdrawnLicenseDetail } });
    expect(await screen.findByText(/Withdrawn on 2026-09-01 \(UTC\) by Alan Admin/)).toHaveTextContent('Keys already installed keep working until 2027-11-11 (UTC).');
    expect(screen.queryByTestId('key-panel')).not.toBeInTheDocument();
    expect(screen.queryByText(TOKEN)).not.toBeInTheDocument();
    for (const action of ['Renew', 'Withdraw', 'Email key']) expect(screen.queryByRole('button', { name: action })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Edit terms' })).toHaveAttribute('aria-disabled', 'true');
  });
});
