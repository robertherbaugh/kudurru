import { useQuery } from '@tanstack/react-query';
import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { useApi } from '../api/context';
import { describe, expect, it } from 'vitest';
import { mockDB, signIn } from '../mocks/handlers';
import { renderWithApp } from '../test/utils';
import { AccountMenu, isLocalUser, passwordProblems } from './AccountMenu';

function fill(dialog: HTMLElement, current: string, next: string, confirm = next) {
  fireEvent.change(within(dialog).getByLabelText(/^Current password/), { target: { value: current } });
  fireEvent.change(within(dialog).getByLabelText(/^New password/), { target: { value: next } });
  fireEvent.change(within(dialog).getByLabelText(/^Confirm password/), { target: { value: confirm } });
}

describe('change password', () => {
  it('validates before sending', () => {
    expect(passwordProblems('', 'short', 'other')).toEqual({ current: 'Required', next: 'At least 8 characters', confirm: 'Does not match the new password' });
    expect(passwordProblems('same-pass', 'same-pass', 'same-pass')).toEqual({ next: 'Must differ from the current password' });
    expect(passwordProblems('old', 'new-password', 'new-password')).toEqual({});
  });

  it('only offers it to local accounts', () => {
    const base = { roles: [], permissions: {} };
    expect(isLocalUser({ ...base, principal: { kind: 'User', id: 'local:admin', name: 'admin', groups: [] } })).toBe(true);
    expect(isLocalUser({ ...base, principal: { kind: 'User', id: 'oidc:x', name: 'x', groups: [], provider: 'dex' } })).toBe(false);
    expect(isLocalUser({ ...base, principal: { kind: 'ServiceAccount', id: 'sa:x', name: 'x', groups: [] } })).toBe(false);
  });

  it('changes the password from the account menu', async () => {
    renderWithApp(<AccountMenu />);
    fireEvent.click(await screen.findByRole('button', { name: 'Change password' }));
    const dialog = await screen.findByRole('dialog');
    fill(dialog, 'wrong-one', 'a-new-password');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Change password' }));
    expect(await within(dialog).findByText('the current password is wrong')).toBeInTheDocument();
    fill(dialog, 'admin', 'a-new-password');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Change password' }));
    await waitFor(() => expect(mockDB().users.find((u) => u.username === 'admin')?.password).toBe('a-new-password'));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
  });

  it('forces the change while the session must change its password', async () => {
    const admin = mockDB().users.find((u) => u.username === 'admin')!;
    admin.mustChangePassword = true;
    signIn('admin');
    renderWithApp(<AccountMenu />);
    // A required dialog is an alertdialog: no Escape, no backdrop click.
    const dialog = await screen.findByRole('alertdialog');
    expect(within(dialog).getByText('Choose a new password')).toBeInTheDocument();
    // No close button and no cancel: only changing it or signing out.
    expect(within(dialog).queryByRole('button', { name: 'Cancel' })).toBeNull();
    expect(within(dialog).getByRole('button', { name: 'Sign out' })).toBeInTheDocument();
    fill(dialog, 'admin', 'a-better-password');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Change password' }));
    await waitFor(() => expect(screen.queryByText('Choose a new password')).toBeNull());
    expect(admin.mustChangePassword).toBe(false);
  });

  it('refetches the answers the forced session got before the change', async () => {
    const admin = mockDB().users.find((u) => u.username === 'admin')!;
    admin.mustChangePassword = true;
    let calls = 0;
    function Probe() {
      const api = useApi();
      const q = useQuery({
        queryKey: ['probe'],
        queryFn: async () => {
          await api.cluster.version();
          return ++calls;
        },
      });
      return <span>{q.data ? `probe ${q.data}` : 'pending'}</span>;
    }
    renderWithApp(
      <>
        <AccountMenu />
        <Probe />
      </>,
    );
    expect(await screen.findByText('probe 1')).toBeInTheDocument();
    const dialog = await screen.findByRole('alertdialog');
    fill(dialog, 'admin', 'a-better-password');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Change password' }));
    expect(await screen.findByText('probe 2')).toBeInTheDocument();
  });
});
