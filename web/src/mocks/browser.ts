// The MSW service worker for `bun run dev:mock`: the whole /api/v1 surface
// answered in the browser from the fixtures. Production builds never import
// this module (main.tsx guards it with import.meta.env.KUDURRU_MOCK).
import { setupWorker } from 'msw/browser';
import { handlers, signIn } from './handlers';

/**
 * Starts the worker. `?mockUser=admin` (or viewer) signs in without the
 * form, which screenshots and quick checks use; the mock database lives in
 * the page, so a full reload signs out again.
 */
export async function startMockWorker(basePath: string): Promise<void> {
  const user = new URLSearchParams(window.location.search).get('mockUser');
  if (user) signIn(user);
  const worker = setupWorker(...handlers);
  await worker.start({
    serviceWorker: { url: `${basePath}mockServiceWorker.js`, options: { scope: basePath } },
    onUnhandledFrame: 'bypass',
    quiet: true,
  });
}
