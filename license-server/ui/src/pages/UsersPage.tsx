// /users (users.read): everyone who signed in at least once, searchable by
// name and email: whether the identity provider verified the email, the
// roles it maps, the roles granted here, the last sign-in and whether the
// account is disabled. Each row opens the user's page.
import { Badge } from '@astryxdesign/core/Badge';
import { Link } from '@astryxdesign/core/Link';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { pixel, proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import type { User } from '@shared/api';
import { useUsers } from '../api/hooks';
import { RoleBadges } from '../components/badges';
import { utcDateTime } from '../lib/dates';
import { ErrorBanner, Loading } from '../ui/components/Feedback';
import { Page, PageHeader, Panel } from '../ui/components/layout/Page';
import { TextField } from '../ui/fields/TextField';
import { useSearchParams } from '../ui/lib/router';

type Row = User & Record<string, unknown>;

const columns: TableColumn<Row>[] = [
  { key: 'name', header: 'Name', width: proportional(2, { minWidth: 180 }), renderCell: (u) => <Link href={`/users/${encodeURIComponent(u.id)}`}>{u.name || u.email || u.subject}</Link> },
  {
    key: 'email',
    header: 'Email',
    width: proportional(2, { minWidth: 200 }),
    renderCell: (u) =>
      u.email ? (
        <HStack gap={1} vAlign="center" wrap="wrap">
          <Text>{u.email}</Text>
          {u.emailVerified ? null : <Badge variant="warning" label="Not verified" />}
        </HStack>
      ) : (
        <Text type="supporting">No verified email</Text>
      ),
  },
  {
    key: 'idpRoles',
    header: 'Identity provider roles',
    width: proportional(2, { minWidth: 170 }),
    renderCell: (u) => (
      <HStack gap={1} wrap="wrap">
        <RoleBadges roles={u.idpRoles} empty="None" />
      </HStack>
    ),
  },
  {
    key: 'grantedRoles',
    header: 'Granted roles',
    width: proportional(2, { minWidth: 150 }),
    renderCell: (u) => (
      <HStack gap={1} wrap="wrap">
        <RoleBadges roles={u.grantedRoles} empty="None" />
      </HStack>
    ),
  },
  { key: 'lastLoginAt', header: 'Last sign-in', width: pixel(190), renderCell: (u) => utcDateTime(u.lastLoginAt) },
  { key: 'disabled', header: 'Account', width: pixel(120), renderCell: (u) => (u.disabled ? <Badge variant="error" label="Disabled" /> : <Badge variant="success" label="Enabled" />) },
];

export default function UsersPage() {
  const [sp, setParams] = useSearchParams();
  const q = sp.get('q') ?? '';
  const list = useUsers(q.trim() ? { q: q.trim() } : {});
  return (
    <Page>
      <PageHeader title="Users" description="Everyone who signed in at least once. Roles come from the identity provider's groups at each sign-in, plus the roles a superadmin grants here." />
      <Panel title="Users">
        <VStack gap={2}>
          <HStack gap={2}>
            <TextField label="Search" startIcon="search" value={q} onChange={(v) => setParams({ q: v })} hasClear placeholder="Name or email" fieldWidth={320} />
          </HStack>
          {list.error ? <ErrorBanner error={list.error} /> : null}
          {list.isPending ? <Loading /> : null}
          {list.data ? <Table<Row> data={list.data.items as Row[]} columns={columns} idKey="id" density="compact" emptyState={<Text color="secondary">{q ? 'No user matches.' : 'Nobody has signed in yet.'}</Text>} /> : null}
        </VStack>
      </Panel>
    </Page>
  );
}
