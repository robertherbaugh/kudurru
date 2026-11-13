// Loading, error and toast helpers shared by every page.
import { Banner } from '@astryxdesign/core/Banner';
import { Center } from '@astryxdesign/core/Center';
import { Link } from '@astryxdesign/core/Link';
import { Spinner } from '@astryxdesign/core/Spinner';
import { useToast } from '@astryxdesign/core/Toast';
import { useCallback, useMemo } from 'react';
import { ApiError } from '../api/client';
import { CLUSTER_DOCS } from '../lib/cluster';
import { primaryFromMessage } from '../lib/follower';

/** A centered spinner for the whole viewport (session check, lazy pages). */
export function FullPageSpinner({ label }: { label: string }) {
  return (
    <Center minHeight="100dvh" role="status" aria-live="polite">
      <Spinner size="lg" label={label} />
    </Center>
  );
}

/** An inline spinner for a panel that is still loading. */
export function Loading({ label = 'Loading' }: { label?: string }) {
  return (
    <Center minHeight={160} role="status" aria-live="polite">
      <Spinner size="md" label={label} />
    </Center>
  );
}

/** A human sentence for any error thrown by a query or mutation. */
export function errorText(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.key === 'primary_required') return primaryRequiredText(err);
    if (err.key === 'completion_unknown') return completionUnknownText(err);
    return err.describe();
  }
  if (err instanceof Error) return err.message;
  return String(err);
}

/** 503 primary_required (D368): an enrolled follower neither forwards nor
 * runs this change. Say where it can be made instead. */
function primaryRequiredText(err: ApiError): string {
  const primary = primaryFromMessage(err.message);
  const who = primary ? `the primary, ${primary},` : 'the primary';
  const base = `Only ${who} can make this change: this node is a follower and does not forward it.`;
  const hint = typeof err.hint === 'string' ? err.hint : '';
  // The general hint repeats the base sentence; a specific one says more.
  if (hint && !hint.startsWith('send it to the API of the primary')) return `${base} ${capitalize(hint)}.`;
  return `${base} Open the dashboard on ${primary ?? 'the primary'} to make it there.`;
}

/** 503 completion_unknown: the primary may or may not have committed it. */
function completionUnknownText(err: ApiError): string {
  const hint = typeof err.hint === 'string' && err.hint ? ` ${capitalize(err.hint)}.` : '';
  return `${capitalize(err.message)}.${hint} Reload to check what is in effect before you try again.`;
}

function capitalize(s: string): string {
  const t = s.replace(/\.$/, '');
  return t.charAt(0).toUpperCase() + t.slice(1);
}

/** The title of an error banner: what kind of failure it is. */
export function errorTitle(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 403) return 'Not permitted';
    if (err.key === 'primary_required') return 'Needs the primary';
    if (err.key === 'completion_unknown') return 'Outcome unknown';
    if (err.key === 'cluster_fenced') return 'This node is fenced';
    if (err.key === 'cluster_conflict') return 'This node holds a promotion conflict';
  }
  return 'Something went wrong';
}

/** An error banner for a failed query; 403s name the missing permission, a
 * follower's refusals name the primary. */
export function ErrorBanner({ error, title }: { error: unknown; title?: string }) {
  const warning = error instanceof ApiError && (error.status === 403 || error.key === 'primary_required' || error.key === 'completion_unknown');
  const docs = clusterStateDocs(error);
  return <Banner status={warning ? 'warning' : 'error'} title={title ?? errorTitle(error)} description={errorText(error)} endContent={docs ? <Link href={docs} isExternalLink>What to do</Link> : undefined} />;
}

/** The operator documentation for a member whose API its authority closed
 * (503 cluster_fenced or cluster_conflict), undefined for any other error. */
export function clusterStateDocs(err: unknown): string | undefined {
  if (!(err instanceof ApiError)) return undefined;
  if (err.key === 'cluster_fenced') return CLUSTER_DOCS.fenced;
  if (err.key === 'cluster_conflict') return CLUSTER_DOCS.conflict;
  return undefined;
}

export interface Notify {
  success: (body: string) => void;
  error: (err: unknown, prefix?: string) => void;
}

/** Toasts for mutation results (Astryx Toast, polite live region). */
export function useNotify(): Notify {
  const show = useToast();
  const success = useCallback((body: string) => void show({ body, type: 'info', isAutoHide: true }), [show]);
  const error = useCallback(
    (err: unknown, prefix?: string) =>
      void show({ body: prefix ? `${prefix}: ${errorText(err)}` : errorText(err), type: 'error', isAutoHide: true, autoHideDuration: 8000 }),
    [show],
  );
  return useMemo(() => ({ success, error }), [success, error]);
}
