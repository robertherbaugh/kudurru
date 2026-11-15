import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useId, useRef, useState } from 'react';
import { ApiError } from '../../api/client';
import { qk, useApi, useApiContext, useSession } from '../../api/context';
import type { Me, SessionRecordInfo } from '../../api/types';
import { FormDialog } from '../FormDialog';
import { Panel } from '../layout/Page';

type SessionRow = SessionRecordInfo & Record<string, unknown>;
const unknownOutcome = 'The revocation outcome is unknown. Refresh sessions before choosing another action. The request will not be retried.';

function eligible(me: Me | null | undefined): me is Me {
  return !!me && me.principal.kind === 'User' && (me.principal.authMethod ?? me.principal.auth) === 'cookie' && !!me.csrf && !me.mustChangePassword;
}

// Private comparison only: neither CSRF nor a guessed account UID enters query keys.
function sameSession(a: Me, b: Me | null | undefined): boolean {
  return !!b && eligible(b) && a.principal.id === b.principal.id && a.principal.provider === b.principal.provider && a.csrf === b.csrf && a.expiresAt === b.expiresAt;
}

export function SessionsPanel() {
  const { data: me, isPending } = useSession();
  const [lifetime, setLifetime] = useState({ me, epoch: 0 });
  if (lifetime.me !== me && (!me || !lifetime.me || !sameSession(lifetime.me, me))) {
    setLifetime({ me, epoch: lifetime.epoch + 1 });
    return null;
  }
  return <Panel title="Browser sessions" headingId="sessions-title">
    {isPending ? <Text>Loading session…</Text> : eligible(me) ? <OwnSessions key={lifetime.epoch} me={me} /> : <Text>Session management requires a cookie session with a completed password change.</Text>}
  </Panel>;
}

function readError(err: unknown): string {
  if (err instanceof ApiError && err.key === 'sessions_unavailable') return 'Session inventory is unavailable on this server.';
  if (err instanceof ApiError && err.status === 403) return 'Session inventory was refused. Your current session may no longer have access.';
  return 'Session inventory is unavailable. Refresh to try a new read.';
}

function OwnSessions({ me }: { me: Me }) {
  const api = useApi();
  const { setCsrf } = useApiContext();
  const qc = useQueryClient();
  const scope = useId();
  const alive = useRef(false);
  const flight = useRef<AbortController | null>(null);
  const [selected, setSelected] = useState<SessionRecordInfo | null>(null);
  const [pending, setPending] = useState(false);
  const [notice, setNotice] = useState('');
  const [needsObservation, setNeedsObservation] = useState(false);
  const [committed, setCommitted] = useState<string | null>(null);
  const current = () => alive.current && sameSession(me, qc.getQueryData<Me | null>(qk.me));

  useEffect(() => {
    alive.current = true;
    return () => { alive.current = false; flight.current?.abort(); };
  }, []);

  const q = useQuery({
    queryKey: ['auth', 'sessions', scope],
    queryFn: async ({ signal }) => {
      const rows = await api.auth.sessions(signal);
      signal.throwIfAborted();
      if (!current()) throw new DOMException('Retired session read', 'AbortError');
      return rows;
    },
    retry: false, staleTime: 0, gcTime: 0, refetchOnWindowFocus: false,
  });

  const clearCurrent = async () => {
    if (!current()) return;
    await qc.cancelQueries();
    // Cancellation can yield to a new login. Never clear that successor's CSRF.
    if (!current()) return;
    setCsrf(undefined);
    qc.setQueryData(qk.me, null);
    qc.removeQueries({ predicate: (entry) => entry.queryKey[0] !== qk.me[0] || entry.queryKey[1] !== qk.me[1] });
  };

  useEffect(() => {
    if (q.error instanceof ApiError && q.error.status === 401) void clearCurrent();
  });

  const observe = async (committedID: string | null) => {
    const result = await q.refetch();
    if (!current()) return;
    if (result.error) {
      if (result.error instanceof ApiError && result.error.status === 401) { await clearCurrent(); return; }
      setNeedsObservation(true);
      setNotice(readError(result.error));
      return;
    }
    const stillVisible = committedID && result.data?.items.some((s) => s.id === committedID && !s.revoked);
    setNeedsObservation(!!stillVisible);
    setCommitted(stillVisible ? committedID : null);
    setNotice(stillVisible ? 'Revocation committed. This snapshot has not shown the change yet. Refresh sessions to observe it.' : committedID ? 'Session revocation is reflected in this snapshot.' : 'Session inventory refreshed. Select a session explicitly to make another change.');
  };

  const revoke = async () => {
    if (!current() || flight.current || !selected || needsObservation) return;
    const target = { ...selected };
    const controller = new AbortController();
    flight.current = controller;
    setPending(true);
    setNotice('');
    try {
      await api.auth.revokeSession(target.id, controller.signal);
      if (!current()) return;
      if (controller.signal.aborted) { setNotice(unknownOutcome); setNeedsObservation(true); return; }
      setSelected(null);
      if (target.current) { await clearCurrent(); return; }
      setCommitted(target.id);
      setNeedsObservation(true);
      setNotice('Revocation committed. Reading the published session inventory…');
      await observe(target.id);
    } catch (err) {
      if (!current()) return;
      setSelected(null);
      if (err instanceof ApiError && err.status === 401) { await clearCurrent(); return; }
      setNeedsObservation(true);
      if (err instanceof ApiError && [404, 409, 412].includes(err.status)) setNotice('The selected session changed or is unavailable. Refresh sessions and select again.');
      else if (err instanceof ApiError && err.status === 403) setNotice('Session revocation was refused. Refresh sessions before another action.');
      else if (err instanceof ApiError && err.status === 429) setNotice(`Too many requests.${err.retryAfter ? ` Retry after ${err.retryAfter} seconds.` : ''} Refresh sessions before another action.`);
      else setNotice(unknownOutcome);
    } finally {
      if (flight.current === controller) flight.current = null;
      if (current()) setPending(false);
    }
  };

  const close = () => {
    setSelected(null);
    if (flight.current) {
      flight.current.abort();
      setNeedsObservation(true);
      setNotice(unknownOutcome);
    }
  };
  const columns: TableColumn<SessionRow>[] = [
    { key: 'id', header: 'Session', width: proportional(2, { minWidth: 150 }), renderCell: (s) => <Text type="code" size="sm">{s.id}</Text> },
    { key: 'issuedAt', header: 'Issued', width: proportional(1, { minWidth: 170 }), renderCell: (s) => <Text>{s.issuedAt}</Text> },
    { key: 'expiresAt', header: 'Lifetime ends', width: proportional(1, { minWidth: 170 }), renderCell: (s) => <Text>{s.expiresAt}</Text> },
    { key: 'idleExpiresAt', header: 'Idle deadline', width: proportional(1, { minWidth: 170 }), renderCell: (s) => <Text>{s.idleExpiresAt}</Text> },
    { key: 'state', header: 'Flags', renderCell: (s) => <Text>{[s.current ? 'Current session' : '', s.revoked ? 'Revoked' : ''].filter(Boolean).join(', ') || '—'}</Text> },
    { key: 'actions', header: 'Actions', width: proportional(1, { minWidth: 160 }), renderCell: (s) => <Button label={s.current ? 'Revoke this session' : `Revoke session ${s.id}`} size="sm" variant="ghost" isDisabled={s.revoked || pending || q.isFetching || needsObservation} onClick={() => { if (current() && !flight.current) { setNotice(''); setSelected({ ...s }); } }} /> },
  ];
  return <VStack gap={3}>
    <Text>Only sessions retained for your current account are listed. Unrevoked rows may still be expired or invalidated by security changes.</Text>
    {notice ? <Banner status="warning" title={notice} /> : null}
    {q.isPending ? <Text>Loading browser sessions…</Text> : q.error ? <Banner status="warning" title={readError(q.error)} /> : q.data?.items.length ? <>
      {!q.data.items.some((s) => s.current) ? <Text>This snapshot has no current-session row. Refresh sessions to observe a newer snapshot.</Text> : null}
      <Table<SessionRow> aria-label="Your browser sessions" density="compact" idKey="id" data={q.data.items} columns={columns} />
    </> : <Text>No retained sessions in this snapshot.</Text>}
    <HStack gap={2}><Button label="Refresh sessions" variant="secondary" isDisabled={pending || q.isFetching} isLoading={q.isFetching} onClick={() => { if (current() && !flight.current) { setSelected(null); void observe(committed); } }} /></HStack>
    {selected ? <FormDialog isOpen onClose={close} title={selected.current ? 'Revoke this session?' : 'Revoke selected session?'} onSubmit={() => { void revoke(); }} footer={<>
      <Button label="Cancel" variant="secondary" onClick={close} />
      <Button type="submit" label={selected.current ? 'Revoke this session' : 'Revoke selected session'} isDisabled={pending} isLoading={pending} />
    </>}>
      <Text>Session: {selected.id}</Text>
      <Text>Issued: {selected.issuedAt}</Text>
      <Text>{selected.current ? 'This revokes the session in this browser and returns you to sign in.' : 'This revokes only the selected session. This browser stays signed in.'}</Text>
    </FormDialog> : null}
  </VStack>;
}
