// "Local users" on the Identity tab: the inventory of local accounts with
// the reviewed commands of the complete SQLite runtime (D396): create,
// change the display name and local groups, disable or enable, reset the
// password, delete. Each command names the exact UID and resourceVersion of
// the row the operator chose and is sent once; a 409 asks for a fresh read,
// an unknown outcome is never sent again. The built-in admin and accounts of
// other managers stay with `kudurru admin` on the host. A group change, a
// disable and a delete end the account's sessions and delete its tokens; the
// first disable and every delete also keep a permanent revocation record
// against the cluster's limit (FLOOR_LIMIT_TEXT). On Kubernetes
// (capabilities.localUserCommands false), on a static host and on a
// follower the controls are disabled with the reason.
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Code } from '@astryxdesign/core/Code';
import { HStack } from '@astryxdesign/core/Stack';
import { proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { qk, useApi, useCan, useSession } from '../../api/context';
import type { IamUser, UpdateLocalUserRequest } from '../../api/types';
import { formatRelative } from '../../lib/format';
import { FLOOR_LIMIT_TEXT, groupsError, passwordErrors, subdomainError } from '../../lib/iam';
import { ErrorBanner, useNotify } from '../Feedback';
import { cleanList, StringListField } from '../fields/StringListField';
import { TextField } from '../fields/TextField';
import { FormDialog } from '../FormDialog';
import { Panel } from '../layout/Page';
import { RecoverAuthenticatorButton } from './Authenticators';
import { useIamCommand, useLocalUserReason } from './iamCommand';

type UserRow = IamUser & Record<string, unknown>;
type Action = 'edit' | 'disable' | 'enable' | 'password' | 'delete';

/** Why a row's commands are refused whatever the session holds. */
function rowReason(u: IamUser): string | undefined {
  if (u.builtinAdmin) return 'The built-in admin is changed with kudurru admin on the host';
  if (!u.uid || !u.resourceVersion) return 'Read the list again: this row has no UID or resourceVersion';
  return undefined;
}

/** Whether the signed-in session is this exact local account. */
function isSelf(me: { principal: { id: string; provider?: string } } | null | undefined, u: IamUser): boolean {
  return !!me && me.principal.id === `local:${u.name}`;
}

export function LocalUsersPanel() {
  const api = useApi();
  const can = useCan();
  const reason = useLocalUserReason();
  const { data: me } = useSession();
  const users = useQuery({ queryKey: ['iam', 'users'], queryFn: () => api.iam.users(), enabled: can('users', 'read') });
  const [creating, setCreating] = useState(false);
  const [selected, setSelected] = useState<{ user: IamUser; action: Action } | null>(null);
  const writeReason = reason('write');
  const deleteReason = reason('delete');
  const choose = (user: IamUser, action: Action) => setSelected({ user: { ...user }, action });
  const columns: TableColumn<UserRow>[] = [
    {
      key: 'name',
      header: 'User',
      width: proportional(2, { minWidth: 180 }),
      renderCell: (u) => (
        <HStack gap={1} vAlign="center" wrap="wrap">
          <Text weight="semibold">{u.displayName || u.name}</Text>
          {u.displayName ? (
            <Text type="code" size="sm" color="secondary">
              {u.name}
            </Text>
          ) : null}
          {u.builtinAdmin ? <Badge variant="blue" label="admin" /> : null}
          {u.disabled ? <Badge variant="warning" label="disabled" /> : null}
          {u.mustChangePassword ? <Badge variant="orange" label="must change password" /> : null}
        </HStack>
      ),
    },
    { key: 'groups', header: 'Groups', width: proportional(1.5, { minWidth: 120 }), renderCell: (u) => <Text size="sm">{u.groups?.length ? u.groups.join(', ') : '–'}</Text> },
    { key: 'lastLoginAt', header: 'Last sign-in', width: proportional(1, { minWidth: 110 }), renderCell: (u) => <Text size="sm">{u.lastLoginAt ? formatRelative(u.lastLoginAt) : 'never'}</Text> },
    {
      key: 'passwordChangedAt',
      header: 'Password changed',
      width: proportional(1.5, { minWidth: 160 }),
      renderCell: (u) => <Text size="sm">{u.passwordChangedAt ? formatRelative(u.passwordChangedAt) : '–'}</Text>,
    },
    {
      key: 'actions',
      header: 'Actions',
      width: proportional(3, { minWidth: 320 }),
      renderCell: (u) => {
        const row = rowReason(u);
        const w = row ?? writeReason;
        const d = row ?? deleteReason;
        return (
          <HStack gap={1} wrap="wrap" vAlign="center">
            <Button label={`Edit ${u.name}`} size="sm" variant="ghost" isDisabled={!!w} tooltip={w} onClick={() => choose(u, 'edit')}>
              Edit…
            </Button>
            <Button label={`${u.disabled ? 'Enable' : 'Disable'} ${u.name}`} size="sm" variant="ghost" isDisabled={!!w} tooltip={w} onClick={() => choose(u, u.disabled ? 'enable' : 'disable')}>
              {u.disabled ? 'Enable…' : 'Disable…'}
            </Button>
            <Button label={`Reset password of ${u.name}`} size="sm" variant="ghost" isDisabled={!!w} tooltip={w} onClick={() => choose(u, 'password')}>
              Reset password…
            </Button>
            <Button label={`Delete ${u.name}`} size="sm" variant="ghost" isDisabled={!!d} tooltip={d} onClick={() => choose(u, 'delete')}>
              Delete…
            </Button>
            <RecoverAuthenticatorButton user={u} />
          </HStack>
        );
      },
    },
  ];
  return (
    <Panel
      title="Local users"
      headingId="iam-users"
      actions={<Button label="New user" variant="secondary" size="sm" isDisabled={!!writeReason} tooltip={writeReason} onClick={() => setCreating(true)} />}
      description="Accounts that sign in with a password on this cluster. Creating one grants no role: add a role binding with a User or Group subject. The built-in admin and accounts of the configuration file are changed with kudurru admin on the host."
    >
      {users.error ? <ErrorBanner error={users.error} /> : null}
      {users.data?.diagnoses?.length ? <Banner status="warning" title="Some entries were ignored" description={users.data.diagnoses.join('; ')} /> : null}
      {users.data?.items.length ? (
        <Table<UserRow> aria-label="Local users" density="compact" idKey="name" data={users.data.items} columns={columns} />
      ) : (
        <Text type="supporting" as="p">
          {users.isLoading ? 'Loading…' : 'No local users.'}
        </Text>
      )}
      {creating ? <CreateUserDialog onClose={() => setCreating(false)} /> : null}
      {selected?.action === 'edit' ? <EditUserDialog user={selected.user} self={isSelf(me, selected.user)} onClose={() => setSelected(null)} /> : null}
      {selected?.action === 'disable' || selected?.action === 'enable' ? <DisableUserDialog user={selected.user} enable={selected.action === 'enable'} self={isSelf(me, selected.user)} onClose={() => setSelected(null)} /> : null}
      {selected?.action === 'password' ? <ResetPasswordDialog user={selected.user} self={isSelf(me, selected.user)} onClose={() => setSelected(null)} /> : null}
      {selected?.action === 'delete' ? <DeleteUserDialog user={selected.user} self={isSelf(me, selected.user)} onClose={() => setSelected(null)} /> : null}
    </Panel>
  );
}

const SELF_TEXT = 'This is your own account: the change ends this session too, and you sign in again.';

/** After a command that ended the caller's own session: read /auth/me again (it answers 401 and the app shows the sign-in page). */
function useAfterSelf() {
  const qc = useQueryClient();
  return (self: boolean) => {
    if (self) void qc.invalidateQueries({ queryKey: qk.me });
  };
}

function Account({ user }: { user: IamUser }) {
  return (
    <Text as="p">
      Account <Code size="inherit">{user.name}</Code>, UID <Code size="inherit">{user.uid}</Code>, resourceVersion <Code size="inherit">{user.resourceVersion}</Code>.
    </Text>
  );
}

function Footer({ label, cmd, onClose, destructive, isDisabled }: { label: string; cmd: ReturnType<typeof useIamCommand>; onClose: () => void; destructive?: boolean; isDisabled?: boolean }) {
  return (
    <>
      <Button label={cmd.locked ? 'Close' : 'Cancel'} variant="ghost" onClick={onClose} />
      <Button type="submit" label={label} variant={destructive ? 'destructive' : 'primary'} isLoading={cmd.pending} isDisabled={cmd.locked || isDisabled} />
    </>
  );
}

function CreateUserDialog({ onClose }: { onClose: () => void }) {
  const api = useApi();
  const notify = useNotify();
  const cmd = useIamCommand('account', { retryAfterRefusal: true });
  const [name, setName] = useState('');
  const [displayName, setDisplayName] = useState('');
  const [password, setPassword] = useState('');
  const [confirm, setConfirm] = useState('');
  const [touched, setTouched] = useState(false);
  const nameErr = subdomainError(name.trim());
  const pw = passwordErrors(password, confirm);
  const displayErr = new TextEncoder().encode(displayName).length > 256 ? 'At most 256 bytes' : undefined;
  const submit = async () => {
    setTouched(true);
    if (nameErr || pw.password || pw.confirm || displayErr) return;
    const r = await cmd.run((signal) => api.iam.createUser({ name: name.trim(), password, ...(displayName.trim() ? { displayName: displayName.trim() } : {}) }, signal));
    if (r.ok) {
      notify.success(`Created local user ${r.value.name}`);
      onClose();
    }
  };
  return (
    <FormDialog isOpen onClose={onClose} title="New local user" width={480} onSubmit={() => void submit()} footer={<Footer label="Create user" cmd={cmd} onClose={onClose} />}>
      <TextField label="Username" value={name} onChange={setName} mono isRequired autoComplete="off" placeholder="alice" error={touched ? (nameErr ?? undefined) : undefined} help="Lower-case letters, digits, “-” and “.”. A later account with the same name never inherits a deleted one’s sessions or tokens." />
      <TextField label="Display name" value={displayName} onChange={setDisplayName} placeholder="Alice" error={displayErr} />
      <TextField label="First password" type="password" autoComplete="new-password" value={password} onChange={setPassword} isRequired error={touched ? pw.password : undefined} help="At least 8 characters. The user must change it at the first sign-in." />
      <TextField label="Confirm password" type="password" autoComplete="new-password" value={confirm} onChange={setConfirm} isRequired error={touched ? pw.confirm : undefined} />
      <Text type="supporting" as="p">
        Creating an account grants no role and enrolls no authenticator. Add a role binding with this user as the subject.
      </Text>
      {cmd.outcome}
    </FormDialog>
  );
}

function EditUserDialog({ user, self, onClose }: { user: IamUser; self: boolean; onClose: () => void }) {
  const api = useApi();
  const notify = useNotify();
  const afterSelf = useAfterSelf();
  const cmd = useIamCommand('account');
  const [displayName, setDisplayName] = useState(user.displayName ?? '');
  const [groups, setGroups] = useState<string[]>(user.groups ?? []);
  const clean = cleanList(groups);
  const groupsChanged = JSON.stringify(clean) !== JSON.stringify(user.groups ?? []);
  const nameChanged = displayName !== (user.displayName ?? '');
  const gErr = groupsError(clean);
  const displayErr = new TextEncoder().encode(displayName).length > 256 ? 'At most 256 bytes' : undefined;
  const submit = async () => {
    if (gErr || displayErr || (!groupsChanged && !nameChanged)) return;
    const req: UpdateLocalUserRequest = { uid: user.uid, resourceVersion: user.resourceVersion ?? '', ...(nameChanged ? { displayName } : {}), ...(groupsChanged ? { groups: clean } : {}) };
    const r = await cmd.run((signal) => api.iam.updateUser(user.name, req, signal));
    if (r.ok) {
      notify.success(`Updated local user ${user.name}`);
      afterSelf(self && groupsChanged);
      onClose();
    }
  };
  return (
    <FormDialog isOpen onClose={onClose} title={`Edit ${user.name}`} width={520} onSubmit={() => void submit()} footer={<Footer label="Save changes" cmd={cmd} onClose={onClose} isDisabled={!groupsChanged && !nameChanged} />}>
      <Account user={user} />
      <TextField label="Display name" value={displayName} onChange={setDisplayName} error={displayErr} help="An empty name clears it. Changing it ends nothing." />
      <StringListField label="Local groups" value={groups} onChange={setGroups} itemError={(g) => groupsError([g])} addLabel="Add group" error={gErr ?? undefined} description="The complete new list (at most 64). A local group has no colon, so it never looks like a group of an identity provider. Role bindings with a Group subject grant through it." />
      {groupsChanged ? <Banner status="warning" title="Changing the groups signs the account out" description={`It ends every session of the account and deletes its API tokens in the same commit. ${FLOOR_LIMIT_TEXT}${self ? ` ${SELF_TEXT}` : ''}`} /> : null}
      {cmd.outcome}
    </FormDialog>
  );
}

function DisableUserDialog({ user, enable, self, onClose }: { user: IamUser; enable: boolean; self: boolean; onClose: () => void }) {
  const api = useApi();
  const notify = useNotify();
  const afterSelf = useAfterSelf();
  const cmd = useIamCommand('account');
  const submit = async () => {
    const r = await cmd.run((signal) => api.iam.updateUser(user.name, { uid: user.uid, resourceVersion: user.resourceVersion ?? '', disabled: !enable }, signal));
    if (r.ok) {
      notify.success(`${enable ? 'Enabled' : 'Disabled'} local user ${user.name}`);
      afterSelf(self && !enable);
      onClose();
    }
  };
  return (
    <FormDialog isOpen onClose={onClose} title={`${enable ? 'Enable' : 'Disable'} ${user.name}?`} width={480} onSubmit={() => void submit()} footer={<Footer label={enable ? 'Enable account' : 'Disable account'} cmd={cmd} onClose={onClose} destructive={!enable} />}>
      <Account user={user} />
      {enable ? (
        <Text as="p">The account can sign in again. Sessions and API tokens ended by the disable stay ended: it signs in anew and issues new tokens.</Text>
      ) : (
        <>
          <Text as="p">The account can no longer sign in. Its sessions end and its API tokens are deleted in the same commit; enabling it later restores none of them.</Text>
          <Banner status="warning" title="Permanent revocation record" description={`${FLOOR_LIMIT_TEXT}${self ? ` ${SELF_TEXT}` : ''}`} />
        </>
      )}
      {cmd.outcome}
    </FormDialog>
  );
}

function ResetPasswordDialog({ user, self, onClose }: { user: IamUser; self: boolean; onClose: () => void }) {
  const api = useApi();
  const notify = useNotify();
  const afterSelf = useAfterSelf();
  const cmd = useIamCommand('account');
  const [password, setPassword] = useState('');
  const [confirm, setConfirm] = useState('');
  const [touched, setTouched] = useState(false);
  const pw = passwordErrors(password, confirm);
  const submit = async () => {
    setTouched(true);
    if (pw.password || pw.confirm) return;
    const r = await cmd.run((signal) => api.iam.resetUserPassword(user.name, { uid: user.uid, resourceVersion: user.resourceVersion ?? '', newPassword: password }, signal));
    if (r.ok) {
      notify.success(`Reset the password of ${user.name}`);
      afterSelf(self);
      onClose();
    }
  };
  return (
    <FormDialog isOpen onClose={onClose} title={`Reset the password of ${user.name}`} width={480} onSubmit={() => void submit()} footer={<Footer label="Reset password" cmd={cmd} onClose={onClose} destructive />}>
      <Account user={user} />
      <TextField label="New password" type="password" autoComplete="new-password" value={password} onChange={setPassword} isRequired error={touched ? pw.password : undefined} help="At least 8 characters. Give it to the user over a private channel." />
      <TextField label="Confirm password" type="password" autoComplete="new-password" value={confirm} onChange={setConfirm} isRequired error={touched ? pw.confirm : undefined} />
      <Text type="supporting" as="p">
        The account is signed out, its personal API tokens are revoked, and it must change this password at the next sign-in. Its authenticator and its disabled state stay; this does not recover a lost authenticator.{self ? ` ${SELF_TEXT}` : ''}
      </Text>
      <Banner status="warning" title="Permanent revocation record" description={FLOOR_LIMIT_TEXT} />
      {cmd.outcome}
    </FormDialog>
  );
}

function DeleteUserDialog({ user, self, onClose }: { user: IamUser; self: boolean; onClose: () => void }) {
  const api = useApi();
  const notify = useNotify();
  const afterSelf = useAfterSelf();
  const cmd = useIamCommand('account');
  const submit = async () => {
    const r = await cmd.run((signal) => api.iam.deleteUser(user.name, { uid: user.uid, resourceVersion: user.resourceVersion ?? '' }, signal));
    if (r.ok) {
      notify.success(`Deleted local user ${user.name}`);
      afterSelf(self);
      onClose();
    }
  };
  return (
    <FormDialog isOpen onClose={onClose} title={`Delete ${user.name}?`} width={480} onSubmit={() => void submit()} footer={<Footer label="Delete account" cmd={cmd} onClose={onClose} destructive />}>
      <Account user={user} />
      <Text as="p">The account, its sessions and its API tokens end in one commit. A later account with the same name gets a new UID and inherits nothing. Role bindings that name it stay until you delete them.</Text>
      <Banner status="warning" title="Permanent revocation record" description={`${FLOOR_LIMIT_TEXT}${self ? ` ${SELF_TEXT}` : ''}`} />
      {cmd.outcome}
    </FormDialog>
  );
}
