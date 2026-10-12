// The signed-in principal, the change-password dialog (POST /auth/password,
// local accounts only) and the sign-out action. A session that must change
// its password (a generated or reset one) gets the dialog right away and
// cannot dismiss it.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Link } from '@astryxdesign/core/Link';
import { HStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { KeyRound, LogOut } from 'lucide-react';
import { useState } from 'react';
import { qk, useApiContext, useSession, useSignOut } from '../api/context';
import type { Me } from '../api/types';
import { errorText, useNotify } from './Feedback';
import { CheckboxField, TextField } from './fields/TextField';
import { FormDialog } from './FormDialog';

/** The minimum length the server accepts (internal/auth/users MinPasswordLen). */
export const MIN_PASSWORD_LEN = 8;

/** Whether the principal is a local account, the only kind with a kudurru password. */
export function isLocalUser(me: Me | null | undefined): boolean {
  if (!me || me.principal.kind !== 'User') return false;
  return me.principal.provider ? me.principal.provider === 'local' : me.principal.id.startsWith('local:');
}

/** Client-side checks; the server repeats them. */
export function passwordProblems(current: string, next: string, confirm: string): Record<string, string> {
  const e: Record<string, string> = {};
  if (!current) e.current = 'Required';
  if ([...next].length < MIN_PASSWORD_LEN) e.next = `At least ${MIN_PASSWORD_LEN} characters`;
  else if (next === current) e.next = 'Must differ from the current password';
  if (confirm !== next) e.confirm = 'Does not match the new password';
  return e;
}

export function ChangePasswordDialog({ isOpen, onClose, forced = false }: { isOpen: boolean; onClose: () => void; forced?: boolean }) {
  const { api, setCsrf } = useApiContext();
  const qc = useQueryClient();
  const notify = useNotify();
  const signOut = useSignOut();
  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');
  const [confirm, setConfirm] = useState('');
  const [revokeTokens, setRevokeTokens] = useState(false);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const reset = () => {
    setCurrent('');
    setNext('');
    setConfirm('');
    setRevokeTokens(false);
    setErrors({});
  };
  const m = useMutation({
    mutationFn: () => api.auth.changePassword({ currentPassword: current, newPassword: next, ...(revokeTokens ? { revokeTokens: true } : {}) }),
    onSuccess: (me) => {
      // Every other session is revoked; this one comes back re-issued.
      if (me && typeof me === 'object' && 'principal' in me) {
        setCsrf(me.csrf);
        qc.setQueryData(qk.me, me);
      } else {
        void qc.invalidateQueries({ queryKey: qk.me });
      }
      // Everything else asked while the session had to change its password
      // was answered 403 (and 4xx answers are not retried): refetch it all,
      // the default-credentials banner's /auth/status included.
      void qc.invalidateQueries({ predicate: (q) => q.queryKey[0] !== qk.me[0] || q.queryKey[1] !== qk.me[1] });
      notify.success(revokeTokens ? 'Password changed; other sessions were signed out and your API tokens revoked' : 'Password changed; other sessions were signed out');
      reset();
      onClose();
    },
  });
  const submit = () => {
    const e = passwordProblems(current, next, confirm);
    setErrors(e);
    if (!Object.keys(e).length) m.mutate();
  };
  const close = () => {
    reset();
    m.reset();
    onClose();
  };
  return (
    <FormDialog
      isOpen={isOpen}
      onClose={close}
      isDismissable={!forced}
      title={forced ? 'Choose a new password' : 'Change password'}
      subtitle={forced ? 'This account still uses a generated or reset password.' : undefined}
      width={460}
      noValidate
      onSubmit={submit}
      footer={
        <>
          {forced ? <Button label="Sign out" variant="ghost" onClick={() => void signOut()} /> : <Button label="Cancel" variant="ghost" onClick={close} />}
          <Button type="submit" label="Change password" variant="primary" isLoading={m.isPending} />
        </>
      }
    >
      <TextField label="Current password" type="password" autoComplete="current-password" value={current} onChange={setCurrent} error={errors.current} />
      <TextField
        label="New password"
        type="password"
        autoComplete="new-password"
        value={next}
        onChange={setNext}
        help={`At least ${MIN_PASSWORD_LEN} characters`}
        error={errors.next}
      />
      <TextField label="Confirm password" type="password" autoComplete="new-password" value={confirm} onChange={setConfirm} error={errors.confirm} />
      <CheckboxField
        label="Also revoke my API tokens"
        value={revokeTokens}
        onChange={(v) => setRevokeTokens(v)}
        help="Tokens keep working after a password change unless you revoke them here, e.g. when the old password may have leaked."
      />
      <Text type="supporting" display="block">
        Changing the password signs out every other session of this account.
      </Text>
      {m.error ? <Banner status="error" title="The password was not changed" description={errorText(m.error)} /> : null}
    </FormDialog>
  );
}

/** A "Change password" button with its dialog; nothing for non-local principals. */
export function ChangePasswordButton({ iconOnly = false }: { iconOnly?: boolean }) {
  const { data } = useSession();
  const [open, setOpen] = useState(false);
  if (!isLocalUser(data)) return null;
  return (
    <>
      <Button
        label="Change password"
        variant={iconOnly ? 'ghost' : 'secondary'}
        size="sm"
        isIconOnly={iconOnly}
        icon={iconOnly ? <KeyRound size={16} aria-hidden="true" /> : undefined}
        tooltip={iconOnly ? 'Change password' : undefined}
        onClick={() => setOpen(true)}
      />
      <ChangePasswordDialog isOpen={open} onClose={() => setOpen(false)} />
    </>
  );
}

export function AccountMenu() {
  const { data } = useSession();
  const signOut = useSignOut();
  if (!data) return null;
  return (
    <HStack gap={1} vAlign="center">
      <Link href="/access" color="inherit" size="sm" tooltip={`${data.principal.kind} ${data.principal.id}; roles: ${data.roles.join(', ') || 'none'}`}>
        {data.principal.name}
      </Link>
      <ChangePasswordButton iconOnly />
      <Button
        label="Sign out"
        variant="ghost"
        size="sm"
        isIconOnly
        icon={<LogOut size={16} aria-hidden="true" />}
        tooltip="Sign out"
        onClick={() => void signOut()}
      />
      {data.mustChangePassword && isLocalUser(data) ? <ChangePasswordDialog isOpen onClose={() => undefined} forced /> : null}
    </HStack>
  );
}
