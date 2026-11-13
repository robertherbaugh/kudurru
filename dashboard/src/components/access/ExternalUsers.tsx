import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useId, useRef, useState } from 'react';
import { ApiError } from '../../api/client';
import { qk, useApi, useCan, usePrimaryOnlyPermission, useSession } from '../../api/context';
import type { ExternalUser, Me } from '../../api/types';
import { FormDialog } from '../FormDialog';
import { Panel } from '../layout/Page';

type Action = 'disable' | 'enable' | 'logout';
type Row = ExternalUser & Record<string, unknown>;
const labels: Record<Action, string> = { disable: 'Disable account', enable: 'Enable account', logout: 'Log out everywhere' };
const unknownOutcome = 'The outcome is unknown. Refresh accounts before choosing a new action. The request will not be retried.';
function sameSession(a: Me, b: Me | null | undefined) {
  return !!b && a.principal.id === b.principal.id && a.principal.provider === b.principal.provider && a.csrf === b.csrf && a.expiresAt === b.expiresAt && !b.mustChangePassword;
}
export function ExternalUsersPanel() {
  const { data: me } = useSession();
  const can = useCan();
  const [lifetime, setLifetime] = useState({ me, epoch: 0 });
  if (lifetime.me !== me && (!me || !lifetime.me || !sameSession(lifetime.me, me))) {
    setLifetime({ me, epoch: lifetime.epoch + 1 });
    return null;
  }
  return <Panel title="External accounts" headingId="external-accounts-title">
    {me && can('users', 'read') && !me.mustChangePassword ? <ExternalAccounts key={lifetime.epoch} me={me} /> : <Text>Reading external accounts requires users.read permission.</Text>}
  </Panel>;
}
function ExternalAccounts({ me }: { me: Me }) {
  const api = useApi();
  const can = useCan();
  const primaryOnly = usePrimaryOnlyPermission();
  const qc = useQueryClient();
  const scope = useId();
  const alive = useRef(false);
  const flight = useRef<AbortController | null>(null);
  const [cursor, setCursor] = useState('');
  const [selection, setSelection] = useState<{ row: ExternalUser; action: Action } | null>(null);
  const [notice, setNotice] = useState('');
  const [pending, setPending] = useState(false);
  const [needsRead, setNeedsRead] = useState(false);
  const [committed, setCommitted] = useState<ExternalUser | null>(null);
  const current = () => alive.current && sameSession(me, qc.getQueryData<Me | null>(qk.me));
  useEffect(() => { alive.current = true; return () => { alive.current = false; flight.current?.abort(); }; }, []);
  const q = useQuery({
    queryKey: ['iam', 'external-users', scope, cursor],
    queryFn: async ({ signal }) => {
      const page = await api.iam.externalUsers(cursor, signal);
      signal.throwIfAborted();
      if (!current()) throw new DOMException('Retired account read', 'AbortError');
      return page;
    },
    retry: false, gcTime: 0, staleTime: 0, refetchOnWindowFocus: false,
  });
  const observe = async (changed: ExternalUser | null) => {
    setSelection(null);
    const result = await q.refetch();
    if (!current()) return;
    if (result.error) { setNeedsRead(true); setNotice('Accounts could not be refreshed. No action was retried.'); return; }
    const old = changed && result.data?.items.some((row) => row.name === changed.name && row.uid === changed.uid && row.revision === changed.revision);
    setNeedsRead(!!old); setCommitted(old ? changed : null);
    setNotice(old ? 'The change committed, but this snapshot still shows the earlier revision. Refresh accounts to observe it.' : 'Accounts refreshed. Review an account before choosing another action.');
  };
  const mutate = async () => {
    if (!current() || !can('users', 'write') || flight.current || !selection || needsRead) return;
    const { row, action } = selection;
    const controller = new AbortController(); flight.current = controller;
    setPending(true); setNotice('');
    try {
      await api.iam.administerExternalUser(row.name, action, { uid: row.uid, revision: row.revision }, controller.signal);
      if (!current()) return;
      setSelection(null); setNeedsRead(true);
      if (controller.signal.aborted) { setNotice(unknownOutcome); return; }
      setCommitted(row);
      setNotice('Change committed. Reading the published account inventory…');
      await observe(row);
    } catch (err) {
      if (!current()) return;
      setSelection(null); setNeedsRead(true);
      setNotice(err instanceof ApiError && [404, 409].includes(err.status) ? 'The selected account changed. Refresh accounts and review it again.' : err instanceof ApiError && [401, 403].includes(err.status) ? 'Account administration was refused. Refresh your access before another action.' : unknownOutcome);
    } finally {
      if (flight.current === controller) flight.current = null;
      if (current()) setPending(false);
    }
  };
  const close = () => { setSelection(null); if (flight.current) { flight.current.abort(); setNeedsRead(true); setNotice(unknownOutcome); } };
  const choose = (row: ExternalUser, action: Action) => { if (current() && !flight.current && !needsRead) setSelection({ row: { ...row }, action }); };
  const columns: TableColumn<Row>[] = [
    { key: 'username', header: 'Account', width: proportional(1), renderCell: (row) => <Text>{row.username}</Text> },
    { key: 'provider', header: 'Provider / subject', width: proportional(2), renderCell: (row) => <VStack gap={1}><Text>{row.provider}</Text><Text type="code">{row.subject}</Text></VStack> },
    { key: 'uid', header: 'Account UID', width: proportional(2), renderCell: (row) => <Text type="code">{row.uid}</Text> },
    { key: 'disabled', header: 'State', renderCell: (row) => <Text>{row.disabled ? 'Disabled' : 'Enabled'}</Text> },
    { key: 'actions', header: 'Actions', width: proportional(2), renderCell: (row) => <HStack gap={2}>
      <Button label={row.disabled ? 'Enable account' : 'Disable account'} size="sm" variant="ghost" isDisabled={!can('users', 'write') || pending || q.isFetching || needsRead} tooltip={primaryOnly('users', 'write')} onClick={() => choose(row, row.disabled ? 'enable' : 'disable')} />
      <Button label="Log out everywhere" size="sm" variant="ghost" isDisabled={!can('users', 'write') || pending || q.isFetching || needsRead} tooltip={primaryOnly('users', 'write')} onClick={() => choose(row, 'logout')} />
    </HStack> },
  ];
  const unavailable = q.error instanceof ApiError && q.error.key === 'external_users_unavailable';
  return <VStack gap={3}>
    <Text>These accounts come from identity providers. Disable and log out everywhere revoke their Kudurru sessions and personal tokens. Identity-provider sessions remain unchanged.</Text>
    {notice ? <Banner status="warning" title={notice} /> : null}
    {q.isPending ? <Text>Loading external accounts…</Text> : q.error ? <Banner status="warning" title={unavailable ? 'External account administration is unavailable on this server.' : 'External accounts could not be read. Refresh to start a new read.'} /> : q.data?.items.length ? <Table<Row> aria-label="External accounts" idKey="name" density="compact" data={q.data.items} columns={columns} /> : <Text>No persisted external accounts.</Text>}
    <HStack gap={2}>
      <Button label="Refresh accounts" variant="secondary" isDisabled={pending || q.isFetching} onClick={() => { if (current() && !flight.current) void observe(committed); }} />
      {cursor ? <Button label="First account page" variant="ghost" isDisabled={pending || q.isFetching} onClick={() => { setSelection(null); setCursor(''); }} /> : null}
      {q.data?.continue && !q.error ? <Button label="Next account page" variant="ghost" isDisabled={pending || needsRead || q.isFetching} onClick={() => { setSelection(null); setCursor(q.data.continue ?? ''); }} /> : null}
    </HStack>
    {selection ? <FormDialog isOpen onClose={close} title={`${labels[selection.action]}?`} onSubmit={() => { void mutate(); }} footer={<><Button label="Cancel" variant="secondary" onClick={close} /><Button label={labels[selection.action]} type="submit" isLoading={pending} isDisabled={pending} /></>}>
      <VStack gap={2}>
        <Text>{selection.row.username}</Text><Text>Provider: {selection.row.provider}</Text><Text>Issuer: {selection.row.issuer}</Text><Text>Subject: {selection.row.subject}</Text><Text>Account UID: {selection.row.uid}</Text>
        <Text>{selection.action === 'enable' ? 'Previously revoked sessions and tokens will stay revoked. This does not grant roles.' : 'This revokes every Kudurru session and personal token of this exact account.'}</Text>
      </VStack>
    </FormDialog> : null}
  </VStack>;
}
