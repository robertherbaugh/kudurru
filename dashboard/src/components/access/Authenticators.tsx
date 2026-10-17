import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { Tooltip } from '@astryxdesign/core/Tooltip';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useRef, useState } from 'react';
import { ApiError } from '../../api/client';
import { qk, useApi, useApiContext, useCan, useSession } from '../../api/context';
import type { IamUser, Me } from '../../api/types';
import { isLocalUser } from '../AccountMenu';
import { FormDialog } from '../FormDialog';
import { TextField } from '../fields/TextField';
import { Panel } from '../layout/Page';
import { Wizard, WizardDialog } from '../Wizard';

const statusKey = ['auth', 'totp'] as const;
const sessionWarning = 'This revokes this account’s existing browser sessions. API tokens keep their current permissions.';
const unknownOutcome = 'The outcome is unknown. Secrets were discarded. Check your session and authenticator status before starting a new attempt.';
const validCode = (code: string) => /^[0-9]{6}$/.test(code);
const validPassword = (password: string) => !!password && new TextEncoder().encode(password).length <= 1024;

function cookieSession(me: Me | null | undefined): me is Me {
  return !!me && me.principal.kind === 'User' && (me.principal.authMethod ?? me.principal.auth) === 'cookie' && !!me.csrf && !me.mustChangePassword;
}
function eligible(me: Me | null | undefined): me is Me { return cookieSession(me) && isLocalUser(me); }
function sessionKey(me: Me): string { return `${me.principal.id}:${me.csrf ?? ''}:${me.expiresAt ?? ''}`; }
function Message({ text }: { text: string }) { return text ? <Banner status="warning" title={text} /> : null; }

/** The server already cleared the cookie. Do not send a second logout mutation. */
function useSessionCleanup() {
  const qc = useQueryClient();
  const { setCsrf } = useApiContext();
  return async () => {
    await qc.cancelQueries();
    setCsrf(undefined);
    qc.setQueryData(qk.me, null);
    qc.removeQueries({ predicate: (q) => q.queryKey[0] !== qk.me[0] || q.queryKey[1] !== qk.me[1] });
  };
}

type Outcome<T> = { ok: true; value: T } | { ok: false };
/** Ordinary promises deliberately keep credential DTOs out of MutationCache. */
function useAttempt(onDiscard: (message: string) => void) {
  const api = useApi();
  const qc = useQueryClient();
  const { setCsrf } = useApiContext();
  const clearSession = useSessionCleanup();
  const flight = useRef<AbortController | null>(null);
  const alive = useRef(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState('');
  const [retryUntil, setRetryUntil] = useState(0);
  useEffect(() => {
    alive.current = true;
    return () => { alive.current = false; flight.current?.abort(); };
  }, []);
  useEffect(() => {
    if (!retryUntil) return;
    const timer = setTimeout(() => setRetryUntil(0), Math.max(0, retryUntil - Date.now()));
    return () => clearTimeout(timer);
  }, [retryUntil]);
  const observeSession = async () => {
    try {
      const current = await api.auth.me();
      if (alive.current) { setCsrf(current.csrf); qc.setQueryData(qk.me, current); }
    } catch (err) {
      if (alive.current && err instanceof ApiError && err.status === 401) await clearSession();
    }
  };
  const run = async <T,>(request: (signal: AbortSignal) => Promise<T>): Promise<Outcome<T>> => {
    if (flight.current || retryUntil > Date.now() || !alive.current) return { ok: false };
    const controller = new AbortController();
    flight.current = controller; setPending(true); setError('');
    try {
      const value = await request(controller.signal);
      return alive.current ? { ok: true, value } : { ok: false };
    } catch (err) {
      if (!alive.current) return { ok: false };
      const status = err instanceof ApiError ? err.status : 0;
      if (status === 401) {
        setError('The password or authenticator code was not accepted. Enter fresh proof to try again.');
        await observeSession();
      } else if (status === 429) {
        const seconds = err instanceof ApiError ? err.retryAfter : undefined;
        const wait = Math.min(3600, Math.max(1, seconds ?? 1));
        setRetryUntil(Date.now() + wait * 1000);
        setError(`Too many attempts. Wait ${wait} seconds, then submit a fresh code explicitly.`);
      } else if (status === 409) {
        onDiscard('The account or setup changed. Refresh and explicitly select the account or start setup again.');
        void qc.invalidateQueries({ queryKey: ['iam', 'users'] });
        void qc.invalidateQueries({ queryKey: statusKey });
      } else if (status === 0 || status >= 500) {
        onDiscard(unknownOutcome);
        void qc.invalidateQueries({ queryKey: qk.me });
        void qc.invalidateQueries({ queryKey: statusKey });
      } else setError(status === 403 ? 'This action is not authorized by the current session.' : 'The request was not accepted. Check the fields before trying again.');
      return { ok: false };
    } finally { flight.current = null; if (alive.current) setPending(false); }
  };
  const close = (done: () => void) => {
    const dispatched = !!flight.current;
    alive.current = false;
    flight.current?.abort();
    if (dispatched) {
      onDiscard(unknownOutcome);
      void qc.invalidateQueries({ queryKey: qk.me });
      void qc.invalidateQueries({ queryKey: statusKey });
    } else done();
  };
  return { run, close, error, setError, blocked: pending || retryUntil > Date.now() };
}

export function AuthenticatorPanel() {
  const { data: me } = useSession();
  return <Panel title="Authenticator" headingId="authenticator-title">
    {eligible(me) ? <LocalAuthenticator key={sessionKey(me)} me={me} /> : <Text>Authenticator management requires a local account cookie session with a completed password change.</Text>}
  </Panel>;
}
function LocalAuthenticator({ me }: { me: Me }) {
  const api = useApi();
  const [flow, setFlow] = useState<'enroll' | 'disable' | null>(null);
  const [notice, setNotice] = useState('');
  const q = useQuery({ queryKey: [...statusKey, me.principal.provider, me.principal.id], queryFn: ({ signal }) => api.auth.totp(signal), retry: false, staleTime: 0, refetchOnMount: 'always', refetchOnWindowFocus: false });
  const discard = (message: string) => { setFlow(null); setNotice(message); };
  return <VStack gap={3}>
    <Message text={notice} />
    {q.isPending || q.isFetching ? <Text>Loading authenticator status…</Text> : q.isError ? <><Message text="Authenticator status is unavailable. No change can be started." /><Button label="Refresh authenticator status" onClick={() => { void q.refetch(); }} /></> : <>
      <Text>{q.data.enabled ? 'An authenticator is enabled.' : 'No authenticator is enabled.'}</Text>
      <HStack gap={2} wrap="wrap">
        <Button label={q.data.enabled ? 'Replace authenticator' : 'Set up authenticator'} onClick={() => { setNotice(''); setFlow('enroll'); }} />
        {q.data.enabled ? <Button label="Disable authenticator" variant="secondary" onClick={() => { setNotice(''); setFlow('disable'); }} /> : null}
      </HStack>
      {flow === 'enroll' ? <Enrollment me={me} replacing={q.data.enabled} onClose={() => setFlow(null)} onDiscard={discard} /> : null}
      {flow === 'disable' ? <Disable onClose={() => setFlow(null)} onDiscard={discard} /> : null}
    </>}
  </VStack>;
}

type Candidate = { challenge: string; secret: string; expiresAt: string };
function Enrollment({ me, replacing, onClose, onDiscard }: { me: Me; replacing: boolean; onClose: () => void; onDiscard: (message: string) => void }) {
  const api = useApi();
  const clearSession = useSessionCleanup();
  const action = useAttempt(onDiscard);
  const [password, setPassword] = useState('');
  const [oldCode, setOldCode] = useState('');
  const [confirmPassword, setConfirmPassword] = useState('');
  const [confirmOldCode, setConfirmOldCode] = useState('');
  const [newCode, setNewCode] = useState('');
  const [candidate, setCandidate] = useState<Candidate | null>(null);
  const { setError } = action;
  useEffect(() => {
    if (!candidate) return;
    const timer = setTimeout(() => {
      setCandidate(null); setPassword(''); setOldCode(''); setConfirmPassword(''); setConfirmOldCode(''); setNewCode('');
      setError('Setup expired. Return to Verify account to start a new setup explicitly.');
    }, Math.max(0, Date.parse(candidate.expiresAt) - Date.now()));
    return () => clearTimeout(timer);
  }, [candidate, setError]);
  const candidateValid = () => !!candidate && Date.parse(candidate.expiresAt) > Date.now();
  const begin = async () => {
    if (action.blocked) return 'An attempt is already pending or temporarily limited.';
    if (candidateValid()) return null;
    if (!validPassword(password) || (replacing && !validCode(oldCode))) return 'Enter your current password and, when replacing, the six-digit current authenticator code.';
    const result = await action.run((signal) => api.auth.beginTOTP({ currentPassword: password, ...(replacing ? { totpCode: oldCode } : {}) }, signal).finally(() => { setPassword(''); setOldCode(''); }));
    if (!result.ok) return 'Setup did not advance. Read the message below before another attempt.';
    const { challenge, secret, expiresAt } = result.value;
    if (!challenge || !secret || !Number.isFinite(Date.parse(expiresAt)) || Date.parse(expiresAt) <= Date.now()) {
      onDiscard('The setup response is expired or invalid. Start a new setup explicitly.'); return 'Setup is unavailable.';
    }
    setCandidate({ challenge, secret, expiresAt }); return null;
  };
  const confirm = async () => {
    if (action.blocked) return;
    if (!candidateValid() || !candidate) { action.setError('Setup expired. Return to Verify account.'); return; }
    if (!validPassword(confirmPassword) || !validCode(newCode) || (replacing && !validCode(confirmOldCode))) { action.setError('Enter your password and all required six-digit codes.'); return; }
    const result = await action.run((signal) => api.auth.confirmTOTP({ currentPassword: confirmPassword, ...(replacing ? { totpCode: confirmOldCode } : {}), challenge: candidate.challenge, code: newCode }, signal).finally(() => { setConfirmPassword(''); setConfirmOldCode(''); setNewCode(''); }));
    if (result.ok) { setCandidate(null); await clearSession(); onClose(); }
  };
  return <WizardDialog isOpen onClose={() => action.close(onClose)} title={replacing ? 'Replace authenticator' : 'Set up authenticator'} what="authenticator setup">
    <Wizard label="Authenticator setup" finishLabel="Confirm authenticator" onFinish={confirm} isFinishing={action.blocked} error={<Message text={action.error} />} steps={[
      { id: 'verify', label: 'Verify account', validate: begin, content: <>
        <Text>Verify your current account. Setup does not activate the new authenticator.</Text>
        <TextField label="Current password" type="password" autoComplete="current-password" value={password} isDisabled={action.blocked} onChange={(v) => { setPassword(v); setCandidate(null); }} />
        {replacing ? <TextField label="Current authenticator code (old authenticator)" autoComplete="one-time-code" value={oldCode} isDisabled={action.blocked} onChange={(v) => { setOldCode(v); setCandidate(null); }} /> : null}
      </> },
      { id: 'add', label: 'Add account', validate: () => candidateValid() ? null : 'Setup expired. Return to Verify account.', content: candidate ? <>
        <Text>Enter this manual key in your authenticator. Keep it private.</Text>
        <TextField label="Authenticator key" value={candidate.secret} isReadOnly mono />
        <Text>Account: kudurru:{me.principal.name}. Issuer: kudurru. Time-based, SHA1, six digits, every 30 seconds.</Text>
        <Text>Setup expires at {candidate.expiresAt}.</Text>
      </> : <Text>Setup expired. Return to Verify account.</Text> },
      { id: 'confirm', label: 'Confirm', content: <>
        <Text>{sessionWarning} You must sign in again after confirmation. The new code is consumed; wait for the next code before signing in.</Text>
        {replacing ? <Text>Setup consumed an old authenticator code. Wait for its next code before confirming.</Text> : null}
        <TextField label="Current password again" type="password" autoComplete="current-password" value={confirmPassword} isDisabled={action.blocked} onChange={setConfirmPassword} />
        {replacing ? <TextField label="Fresh current code (old authenticator)" autoComplete="one-time-code" value={confirmOldCode} isDisabled={action.blocked} onChange={setConfirmOldCode} /> : null}
        <TextField label="New authenticator code" autoComplete="one-time-code" value={newCode} isDisabled={action.blocked} onChange={setNewCode} />
      </> },
    ]} />
  </WizardDialog>;
}
function Disable({ onClose, onDiscard }: { onClose: () => void; onDiscard: (message: string) => void }) {
  const api = useApi(); const clearSession = useSessionCleanup(); const action = useAttempt(onDiscard);
  const [password, setPassword] = useState(''); const [code, setCode] = useState('');
  const submit = async () => {
    if (action.blocked) return;
    if (!validPassword(password) || !validCode(code)) { action.setError('Enter your current password and six-digit authenticator code.'); return; }
    const result = await action.run((signal) => api.auth.disableTOTP({ currentPassword: password, totpCode: code }, signal).finally(() => { setPassword(''); setCode(''); }));
    if (result.ok) { await clearSession(); onClose(); }
  };
  return <FormDialog isOpen onClose={() => action.close(onClose)} title="Disable authenticator" onSubmit={() => { void submit(); }} footer={<Button type="submit" label="Disable authenticator" variant="destructive" isLoading={action.blocked} />}>
    <Text>{sessionWarning} You must sign in again after disabling.</Text>
    <TextField label="Current password" type="password" autoComplete="current-password" value={password} isDisabled={action.blocked} onChange={setPassword} />
    <TextField label="Current authenticator code" autoComplete="one-time-code" value={code} isDisabled={action.blocked} onChange={setCode} />
    <Message text={action.error} />
  </FormDialog>;
}
export function RecoverAuthenticatorButton({ user }: { user: IamUser }) {
  const can = useCan(); const { data: me } = useSession();
  const [selected, setSelected] = useState<{ name: string; uid: string; session: string } | null>(null); const [notice, setNotice] = useState('');
  const allowed = cookieSession(me) && can('users', 'write') && !!user.uid;
  return <VStack gap={1}>
    <Tooltip content={!can('users', 'write') ? 'Needs users.write at cluster scope' : !user.uid ? 'Refresh the inventory to obtain the account UID' : 'Remove the selected account’s authenticator and revoke its browser sessions'}>
      <Button label={`Recover authenticator for ${user.name}`} size="sm" variant="ghost" isDisabled={!allowed} onClick={() => { setNotice(''); if (cookieSession(me)) setSelected({ name: user.name, uid: user.uid, session: sessionKey(me) }); }}>Recover authenticator…</Button>
    </Tooltip>
    <Message text={notice} />
    {selected && cookieSession(me) && selected.session === sessionKey(me) && can('users', 'write') ? <Recovery key={`${sessionKey(me)}:${selected.uid}`} selected={selected} me={me} onClose={() => setSelected(null)} onDiscard={(message) => { setSelected(null); setNotice(message); }} /> : null}
  </VStack>;
}
function Recovery({ selected, me, onClose, onDiscard }: { selected: { name: string; uid: string }; me: Me; onClose: () => void; onDiscard: (message: string) => void }) {
  const api = useApi(); const qc = useQueryClient(); const clearSession = useSessionCleanup(); const action = useAttempt(onDiscard);
  const [verified, setVerified] = useState(false); const [inventoryError, setInventoryError] = useState('');
  useEffect(() => {
    let alive = true;
    void api.iam.users().then((data) => {
      if (!alive) return;
      if (data.items.some((u) => u.name === selected.name && u.uid === selected.uid)) setVerified(true);
      else setInventoryError('The account UID changed or is missing. Close, refresh the inventory, and explicitly select the account again.');
    }, () => { if (alive) setInventoryError('The current account UID could not be verified. Close and refresh the inventory.'); });
    return () => { alive = false; };
  }, [api, selected]);
  const submit = async () => {
    if (!verified || action.blocked) return;
    const result = await action.run((signal) => api.iam.recoverTOTP(selected.name, { uid: selected.uid }, signal));
    if (!result.ok) return;
    const self = isLocalUser(me) && (me.principal.id === `local:${selected.name}` || me.principal.name === selected.name);
    if (self) await clearSession();
    else { void qc.invalidateQueries({ queryKey: ['iam', 'users'] }); void qc.invalidateQueries({ queryKey: statusKey }); }
    onClose();
  };
  return <FormDialog isOpen onClose={() => action.close(onClose)} title={`Recover authenticator for ${selected.name}`} onSubmit={() => { void submit(); }} footer={<Button type="submit" label="Recover authenticator" variant="destructive" isDisabled={!verified} isLoading={action.blocked} />}>
    <Text>This removes the selected account’s authenticator, if present. {sessionWarning}</Text>
    <Text>Account: {selected.name}. Selected UID: {selected.uid}.</Text>
    <Text>{verified ? 'The selected UID matches the freshly loaded inventory.' : 'Checking the selected UID against the current inventory…'}</Text>
    <Message text={inventoryError || action.error} />
  </FormDialog>;
}
