// The MSW service worker for `bun run dev:mock`: the whole /api/v1 surface
// answered in the browser from the fixtures. Production builds never import
// this module (main.tsx guards it with import.meta.env.KUDURRU_MOCK).
import { setupWorker } from 'msw/browser';
import { handlers } from './handlers';

export async function startMockWorker(basePath: string): Promise<void> {
  const worker = setupWorker(...handlers);
  await worker.start({
    serviceWorker: { url: `${basePath}mockServiceWorker.js`, options: { scope: basePath } },
    onUnhandledFrame: 'bypass',
    quiet: true,
  });
}
