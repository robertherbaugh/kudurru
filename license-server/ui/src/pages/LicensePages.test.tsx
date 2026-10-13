// The license detail pages: the customer's (/my/licenses/:id) and the
// admin's (/licenses/:id). Both show the current key in the key panel with
// the download link, the notices and UTC dates; a withdrawn license shows
// no key, only when it was withdrawn and how long installed keys keep
// working.
import { screen, within } from '@testing-library/react';
import { KEY_IS_NOT_A_LICENSE } from '@shared/catalog';
import { NONCOMMERCIAL_NOTICE } from '../components/keys';
import { renderApp } from '../test/render';
import { adminMe, customerDetail, customerMe, licenseDetail, myLicenseDetail, myWithdrawnDetail, TOKEN, withdrawnLicenseDetail } from '../dev/fixtures';

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

  test('a personal key in its renew window offers Renew; a business key offers the mailto', async () => {
    const first = renderApp({ route: '/my/licenses/lic_example12', me: customerMe, handlers: { myLicense: () => ({ ...myLicenseDetail, renewal: { expiresOn: '2028-10-11' } }) } });
    expect(await screen.findByRole('button', { name: 'Renew' })).toBeInTheDocument();
    first.unmount();
    renderApp({ route: '/my/licenses/lic_example12', me: customerMe, handlers: { myLicense: () => ({ ...myLicenseDetail, plan: 'business', nodes: 10 }) } });
    const ask = await screen.findByRole('link', { name: 'Ask about a renewal or a change' });
    expect(ask).toHaveAttribute('href', 'mailto:hello@kudurru.io?subject=kudurru%20commercial%20license%20lic_example12');
    expect(screen.queryByRole('button', { name: 'Renew' })).not.toBeInTheDocument();
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
