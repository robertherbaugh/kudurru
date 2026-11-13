// The plumbing every IAM write dialog of the Identity tab shares (D294,
// D396): one request per decision, never repeated; an abort when the dialog
// closes mid-flight; the reviewed read a replace or delete is bound to; and
// the reason a control is disabled (permission, static mode, follower,
// driver capability).
import { Banner } from '@astryxdesign/core/Banner';
import { useQueryClient } from '@tanstack/react-query';
import { useCallback, useEffect, useRef, useState } from 'react';
import { useAuthCapabilities, useCannotReason } from '../../api/context';
import { iamOutcomeUnknown, iamRefusalText, iamUnknownText } from '../../lib/iam';
import { ErrorBanner, errorText } from '../Feedback';

export type IamCommandState = 'idle' | 'pending' | 'refused' | 'unknown';

/**
 * One reviewed IAM command. `run` sends at most one request at a time, and
 * none after an unknown outcome or (unless retryAfterRefusal) a refusal. Closing the dialog (unmount) aborts a
 * request in flight; its outcome is then unknown and the list is read again.
 * The IAM lists are invalidated after every answer.
 */
export function useIamCommand(what: string, { retryAfterRefusal = false }: { retryAfterRefusal?: boolean } = {}) {
  const qc = useQueryClient();
  const [state, setState] = useState<IamCommandState>('idle');
  const [error, setError] = useState<unknown>();
  const flight = useRef<AbortController | null>(null);
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
      if (flight.current) {
        flight.current.abort();
        void qc.invalidateQueries({ queryKey: ['iam'] });
      }
    };
  }, [qc]);
  const run = useCallback(
    async <T,>(send: (signal: AbortSignal) => Promise<T>): Promise<{ ok: true; value: T } | { ok: false }> => {
      if (flight.current || state === 'unknown' || (state === 'refused' && !retryAfterRefusal)) return { ok: false };
      const controller = new AbortController();
      flight.current = controller;
      setState('pending');
      setError(undefined);
      try {
        const value = await send(controller.signal);
        if (alive.current) setState('idle');
        return { ok: true, value };
      } catch (err) {
        if (alive.current) {
          setError(err);
          setState(iamOutcomeUnknown(err) ? 'unknown' : 'refused');
        }
        return { ok: false };
      } finally {
        if (flight.current === controller) flight.current = null;
        void qc.invalidateQueries({ queryKey: ['iam'] });
      }
    },
    [qc, state, retryAfterRefusal],
  );
  const outcome = state === 'refused' || state === 'unknown' ? <IamOutcome error={error} unknown={state === 'unknown'} what={what} /> : null;
  // A reviewed command is bound to one read: after a refusal it is not sent
  // again (close, read the list again, decide again). A create may be sent
  // again after a refusal (409 exists, 400 invalid) once the draft changed.
  const locked = state === 'unknown' || (state === 'refused' && !retryAfterRefusal);
  return { run, state, pending: state === 'pending', locked, outcome };
}

function IamOutcome({ error, unknown, what }: { error: unknown; unknown: boolean; what: string }) {
  if (unknown) return <Banner status="warning" title="Outcome unknown" description={`${iamUnknownText(what)} The server said: ${errorText(error)}`} data-testid="iam-outcome" />;
  const text = iamRefusalText(error, what);
  return text ? <Banner status="error" title="Refused" description={text} data-testid="iam-outcome" /> : <ErrorBanner error={error} />;
}

/**
 * The fresh read a replace, delete or token issuance is bound to: the UID
 * and resourceVersion the command sends are the ones this read returned,
 * never the list's. Read once per dialog; a refusal asks for a new dialog.
 */
export function useReviewRead<T>(read: (signal: AbortSignal) => Promise<T>): { data: T | undefined; error: unknown } {
  const [result, setResult] = useState<{ data: T | undefined; error: unknown }>({ data: undefined, error: undefined });
  const once = useRef(read);
  useEffect(() => {
    const controller = new AbortController();
    once.current(controller.signal).then(
      (data) => {
        if (!controller.signal.aborted) setResult({ data, error: undefined });
      },
      (error: unknown) => {
        if (!controller.signal.aborted) setResult({ data: undefined, error });
      },
    );
    return () => controller.abort();
  }, []);
  return result;
}

/** Why the local-account commands are not offered here, undefined when they are. */
export function useLocalUserReason(): (verb: 'write' | 'delete') => string | undefined {
  const cannot = useCannotReason();
  const { localUserCommands } = useAuthCapabilities();
  return (verb) => (!localUserCommands ? 'This server does not change local accounts through the API (Kubernetes driver): use kudurru admin on the host, or your identity provider' : cannot('users', verb));
}

/** Why a ServiceAccount, IdentityProvider or RoleBinding command is not offered here. */
export function useIamObjectReason(): (resource: 'serviceaccounts' | 'identityproviders' | 'rolebindings' | 'tokens', verb: 'write' | 'delete') => string | undefined {
  const cannot = useCannotReason();
  const { iamObjectCommands } = useAuthCapabilities();
  return (resource, verb) => (!iamObjectCommands ? 'This server does not change IAM objects through the API (Kubernetes driver): use kubectl or your GitOps tool' : cannot(resource, verb));
}
