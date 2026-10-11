// The signed-in principal, the change-password dialog (POST /auth/password,
// local accounts only) and the sign-out action. A session that must change
// its password (a generated or reset one) gets the dialog right away and
// cannot dismiss it.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Dialog, DialogHeader } from '@astryxdesign/core/Dialog';
import { Layout, LayoutContent, LayoutFooter } from '@astryxdesign/core/Layout';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { KeyRound, LogOut } from 'lucide-react';
import { useId, useState } from 'react';
import { qk, useApiContext, useSession, useSignOut } from '../api/context';
import type { Me } from '../api/types';
import { Link } from '../lib/router';
import { errorText, useNotify } from './Feedback';

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
  const formId = useId();
  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');
  const [confirm, setConfirm] = useState('');
  const [errors, setErrors] = useState<Record<string, string>>({});
  const reset = () => {
    setCurrent('');
    setNext('');
    setConfirm('');
    setErrors({});
  };
  const m = useMutation({
    mutationFn: () => api.auth.changePassword({ currentPassword: current, newPassword: next }),
    onSuccess: (me) => {
      // Every other session is revoked; this one comes back re-issued.
      if (me && typeof me === 'object' && 'principal' in me) {
        setCsrf(me.csrf);
        qc.setQueryData(qk.me, me);
      } else {
        void qc.invalidateQueries({ queryKey: qk.me });
      }
      notify.success('Password changed; other sessions were signed out');
      reset();
      onClose();
    },
  });
  const submit = () => {
    const e = passwordProblems(current, next, confirm);
    setErrors(e);
    if (!Object.keys(e).length) m.mutate();
  };
  const status = (k: string) => (errors[k] ? { type: 'error' as const, message: errors[k] } : undefined);
  const close = () => {
    reset();
    m.reset();
    onClose();
  };
  return (
    <Dialog isOpen={isOpen} onOpenChange={(o) => !o && !forced && close()} purpose={forced ? 'required' : 'form'} width={460} maxHeight="90dvh">
      <Layout
        header={
          <DialogHeader
            title={forced ? 'Choose a new password' : 'Change password'}
            subtitle={forced ? 'This account still uses a generated or reset password.' : undefined}
            onOpenChange={forced ? undefined : (o) => !o && close()}
          />
        }
        content={
          <LayoutContent>
            <form
              id={formId}
              className="k-stack-lg"
              noValidate
              onSubmit={(e) => {
                e.preventDefault();
                submit();
              }}
            >
              <TextInput label="Current password" type="password" autoComplete="current-password" value={current} onChange={setCurrent} status={status('current')} />
              <TextInput
                label="New password"
                type="password"
                autoComplete="new-password"
                value={next}
                onChange={setNext}
                description={`At least ${MIN_PASSWORD_LEN} characters`}
                status={status('next')}
              />
              <TextInput label="Confirm password" type="password" autoComplete="new-password" value={confirm} onChange={setConfirm} status={status('confirm')} />
              <p className="k-muted k-small">Changing the password signs out every other session of this account.</p>
              {m.error ? <Banner status="error" title="The password was not changed" description={errorText(m.error)} /> : null}
            </form>
          </LayoutContent>
        }
        footer={
          <LayoutFooter hasDivider>
            <div className="k-row">
              <span className="k-spacer" />
              {forced ? <Button label="Sign out" variant="ghost" onClick={() => void signOut()} /> : <Button label="Cancel" variant="ghost" onClick={close} />}
              <Button type="submit" form={formId} label="Change password" variant="primary" isLoading={m.isPending} />
            </div>
          </LayoutFooter>
        }
      />
    </Dialog>
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
    <span className="k-row k-account">
      <Link href="/access" className="k-account-name" title={`${data.principal.kind} ${data.principal.id}; roles: ${data.roles.join(', ') || 'none'}`}>
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
    </span>
  );
}
