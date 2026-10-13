// /users/:id (users.read): one person. Their identity (issuer, subject,
// email), the roles the identity provider maps (read-only) and the roles
// granted here (a CheckboxList with Save, superadmins only), Disable or
// Enable and Revoke sessions (each confirmed; both end the person's
// sessions), the customers they belong to and the recent audit events
// about them. The server refuses a superadmin removing their own grant,
// disabling themselves or the last superadmin losing the role; the page
// shows its answer.
import { AlertDialog } from '@astryxdesign/core/AlertDialog';
import { Badge } from '@astryxdesign/core/Badge';
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { CheckboxList, CheckboxListItem } from '@astryxdesign/core/CheckboxList';
import { Link } from '@astryxdesign/core/Link';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { pixel, proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useState } from 'react';
import type { UserDetail } from '@shared/api';
import { isRole, ROLE_INFO, type Role } from '@shared/permissions';
import { useCan, useMe } from '../api/context';
import { useRouteMutation, useUser } from '../api/hooks';
import { RecentAudit } from '../components/AuditTable';
import { RoleBadges } from '../components/badges';
import type { PageProps } from '../routes';
import { utcDateTime, utcDay } from '../lib/dates';
import { KIND_LABEL } from '../lib/words';
import { errorText, ErrorBanner, Loading, useNotify } from '../ui/components/Feedback';
import { Page, PageHeader, Panel } from '../ui/components/layout/Page';
import { FieldShell } from '../ui/fields/FieldHelp';

type MembershipRow = UserDetail['memberships'][number] & { key: string } & Record<string, unknown>;

const membershipColumns: TableColumn<MembershipRow>[] = [
  { key: 'customer', header: 'Customer', width: proportional(2, { minWidth: 180 }), renderCell: (m) => <Link href={`/customers/${encodeURIComponent(m.customer.id)}`}>{m.customer.name}</Link> },
  { key: 'kind', header: 'Kind', width: pixel(130), renderCell: (m) => KIND_LABEL[m.customer.kind] },
  { key: 'email', header: 'Member email', width: proportional(2, { minWidth: 180 }) },
  { key: 'linkedAt', header: 'Linked (UTC)', width: pixel(170), renderCell: (m) => utcDay(m.linkedAt) },
];

export default function UserPage({ params }: PageProps) {
  const id = params.id ?? '';
  const can = useCan();
  const me = useMe();
  const notify = useNotify();
  const q = useUser(id);
  const self = id === me.user.id;
  const update = useRouteMutation('userUpdate', { refreshSession: self });
  const revoke = useRouteMutation('userRevokeSessions');
  const [confirm, setConfirm] = useState<'disable' | 'enable' | 'revoke' | null>(null);
  const u = q.data;
  const write = can('users.write');

  const act = () => {
    if (!u || !confirm) return;
    if (confirm === 'revoke') {
      revoke.mutate(
        { params: { id: u.id } },
        {
          onSuccess: (r) => {
            notify.success(`Ended ${r.revoked} session${r.revoked === 1 ? '' : 's'}`);
            setConfirm(null);
          },
          onError: (err) => {
            notify.error(err, 'Could not end the sessions');
            setConfirm(null);
          },
        },
      );
      return;
    }
    update.mutate(
      { params: { id: u.id }, body: { disabled: confirm === 'disable' } },
      {
        onSuccess: () => {
          notify.success(confirm === 'disable' ? `Disabled ${u.name || u.email}` : `Enabled ${u.name || u.email}`);
          setConfirm(null);
        },
        onError: (err) => {
          notify.error(err, confirm === 'disable' ? 'Could not disable the account' : 'Could not enable the account');
          setConfirm(null);
        },
      },
    );
  };

  return (
    <Page>
      <PageHeader
        back={{ href: '/users', label: 'Users' }}
        title={u ? u.name || u.email || u.subject : 'User'}
        meta={
          u ? (
            <>
              {u.disabled ? <Badge variant="error" label="Disabled" /> : <Badge variant="success" label="Enabled" />}
              {self ? <Badge variant="info" label="You" /> : null}
            </>
          ) : null
        }
        actions={
          u && write ? (
            <>
              <Button label="Revoke sessions" variant="secondary" isDisabled={u.activeSessions === 0} tooltip={u.activeSessions === 0 ? 'No active sessions' : undefined} onClick={() => setConfirm('revoke')} />
              {u.disabled ? (
                <Button label="Enable" variant="secondary" onClick={() => setConfirm('enable')} />
              ) : (
                <Button label="Disable" variant="destructive" isDisabled={self} tooltip={self ? 'You cannot disable yourself' : undefined} onClick={() => setConfirm('disable')} />
              )}
            </>
          ) : null
        }
      />
      {q.error ? <ErrorBanner error={q.error} /> : null}
      {q.isPending ? <Loading /> : null}
      {u ? (
        <>
          <Panel title="Identity">
              <MetadataList>
                <MetadataListItem label="Email">
                  {u.email ?? 'None shared'}
                  {u.email && !u.emailVerified ? ' (not verified)' : ''}
                </MetadataListItem>
                <MetadataListItem label="Issuer">{u.issuer}</MetadataListItem>
                <MetadataListItem label="Subject">
                  <Text type="code">{u.subject}</Text>
                </MetadataListItem>
                <MetadataListItem label="First sign-in">{utcDateTime(u.createdAt)}</MetadataListItem>
                <MetadataListItem label="Last sign-in">{utcDateTime(u.lastLoginAt)}</MetadataListItem>
                <MetadataListItem label="Active sessions">{u.activeSessions}</MetadataListItem>
                {u.disabledAt ? <MetadataListItem label="Disabled">{utcDateTime(u.disabledAt)}</MetadataListItem> : null}
              </MetadataList>
          </Panel>
          <RolesSection user={u} isSelf={self} canWrite={write} />
          <Panel title="Memberships" description="The customers whose licenses this person sees.">
            <Table<MembershipRow>
              data={u.memberships.map((m) => ({ ...m, key: `${m.customer.id}:${m.email}` }))}
              columns={membershipColumns}
              idKey="key"
              density="compact"
              emptyState={<Text color="secondary">Not a member of any customer.</Text>}
            />
          </Panel>
          <RecentAudit targetType="user" targetId={u.id} />
          <AlertDialog
            isOpen={confirm !== null}
            onOpenChange={(open) => {
              if (!open) setConfirm(null);
            }}
            title={confirm === 'revoke' ? 'End every session?' : confirm === 'disable' ? `Disable ${u.name || u.email || 'this account'}?` : `Enable ${u.name || u.email || 'this account'}?`}
            description={
              confirm === 'revoke'
                ? `Ends ${u.activeSessions} active session${u.activeSessions === 1 ? '' : 's'}; the person has to sign in again.`
                : confirm === 'disable'
                  ? 'The person loses every role and is signed out at once. Their memberships stay.'
                  : 'The person gets their roles back at the next sign-in.'
            }
            actionLabel={confirm === 'revoke' ? 'End sessions' : confirm === 'disable' ? 'Disable' : 'Enable'}
            actionVariant={confirm === 'enable' ? 'primary' : 'destructive'}
            isActionLoading={update.isPending || revoke.isPending}
            onAction={act}
          />
        </>
      ) : null}
    </Page>
  );
}

function RolesSection({ user: u, isSelf, canWrite }: { user: UserDetail; isSelf: boolean; canWrite: boolean }) {
  const notify = useNotify();
  const m = useRouteMutation('userUpdate', { refreshSession: isSelf });
  const [draft, setDraft] = useState<Role[] | null>(null);
  const granted = draft ?? u.grantedRoles;
  const changed = draft !== null && [...draft].sort().join(',') !== [...u.grantedRoles].sort().join(',');
  return (
    <Panel title="Roles" description="Effective roles are the identity provider's plus the granted ones; a disabled account has none.">
      <VStack gap={3}>
        <MetadataList>
          <MetadataListItem label="Effective">
            <HStack gap={1} wrap="wrap">
              <RoleBadges roles={u.roles} />
            </HStack>
          </MetadataListItem>
          <MetadataListItem label="From the identity provider">
            <HStack gap={1} wrap="wrap">
              <RoleBadges roles={u.idpRoles} empty="None" />
            </HStack>
          </MetadataListItem>
        </MetadataList>
        <FieldShell help={canWrite ? "Role changes end the person's other sessions." : 'Only a superadmin grants roles.'} describes="group">
          {() => (
            <CheckboxList label="Granted in the portal" value={granted} onChange={(v) => setDraft(v.filter(isRole))} isReadOnly={!canWrite}>
              {ROLE_INFO.map((r) => (
                <CheckboxListItem
                  key={r.key}
                  value={r.key}
                  label={r.label}
                  description={r.description}
                  isDisabled={isSelf && r.key === 'superadmin' && u.grantedRoles.includes('superadmin')}
                />
              ))}
            </CheckboxList>
          )}
        </FieldShell>
        {m.error ? <Banner status="error" title="The roles were not saved" description={errorText(m.error)} /> : null}
        {canWrite ? (
          <HStack gap={2}>
            <Button
              label="Save roles"
              variant="primary"
              isDisabled={!changed}
              isLoading={m.isPending}
              onClick={() =>
                m.mutate(
                  { params: { id: u.id }, body: { grantedRoles: granted } },
                  {
                    onSuccess: () => {
                      setDraft(null);
                      notify.success('Roles saved');
                    },
                  },
                )
              }
            />
            {changed ? <Button label="Cancel" variant="ghost" onClick={() => setDraft(null)} /> : null}
          </HStack>
        ) : null}
      </VStack>
    </Panel>
  );
}
