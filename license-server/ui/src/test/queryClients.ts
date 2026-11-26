// The QueryClients of the running test, and the teardown that leaves nothing
// of them behind (ui/src/test/setup.ts runs it after every test).
//
// React Query hands its notifications to components (and to cache listeners)
// in a batch it schedules with setTimeout(0). A test that ends while a fetch
// or a mutation is still in flight, or right after one settled, leaves such a
// batch queued; when it was the file's last test, the batch can fire after
// Vitest has torn down jsdom and calls into React with no `window`
// ("ReferenceError: window is not defined" in resolveEventTimeStamp, an
// unhandled error that fails the run). So the scheduler is counted here, and
// a test only ends once every query and mutation has settled and every batch
// has reached React, while its UI is still mounted.
import { defaultScheduler, notifyManager, type QueryClient } from '@tanstack/react-query';
import { waitFor } from '@testing-library/react';
import { expect } from 'vitest';

const clients = new Set<QueryClient>();
let queuedBatches = 0;

notifyManager.setScheduler((callback) => {
  queuedBatches++;
  defaultScheduler(() => {
    queuedBatches--;
    callback();
  });
});

/** Registers a test's QueryClient for the teardown below. */
export function trackQueryClient(client: QueryClient): QueryClient {
  clients.add(client);
  return client;
}

/**
 * Waits until no query fetches, no mutation runs and no notification batch is
 * still queued. Fails the test (after the findBy* timeout) when something
 * never settles, rather than leaving it to fire after the environment is gone.
 */
export async function settleQueryClients(): Promise<void> {
  await waitFor(() => {
    for (const client of clients) {
      expect(client.isFetching(), 'queries still fetching at the end of the test').toBe(0);
      expect(client.isMutating(), 'mutations still running at the end of the test').toBe(0);
    }
    expect(queuedBatches, 'React Query notifications still queued at the end of the test').toBe(0);
  });
}

/**
 * Drops every cached query and mutation of the test's clients (after
 * unmount), and checks that neither the unmount nor the clearing queued a
 * notification batch.
 */
export function clearQueryClients(): void {
  for (const client of clients) client.clear();
  clients.clear();
  expect(queuedBatches, 'React Query notifications queued by the unmount').toBe(0);
}
