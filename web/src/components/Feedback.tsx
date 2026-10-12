// Loading, error and toast helpers shared by every page.
import { Banner } from '@astryxdesign/core/Banner';
import { Center } from '@astryxdesign/core/Center';
import { Spinner } from '@astryxdesign/core/Spinner';
import { useToast } from '@astryxdesign/core/Toast';
import { useCallback, useMemo } from 'react';
import { ApiError } from '../api/client';

/** A centred spinner for the whole viewport (session check, lazy pages). */
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
  if (err instanceof ApiError) return err.describe();
  if (err instanceof Error) return err.message;
  return String(err);
}

/** An error banner for a failed query; 403s name the missing permission. */
export function ErrorBanner({ error, title }: { error: unknown; title?: string }) {
  const forbidden = error instanceof ApiError && error.status === 403;
  return (
    <Banner
      status={forbidden ? 'warning' : 'error'}
      title={title ?? (forbidden ? 'Not permitted' : 'Something went wrong')}
      description={errorText(error)}
    />
  );
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
