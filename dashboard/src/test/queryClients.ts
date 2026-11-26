// The QueryClients of the running test, and the teardown that leaves nothing
// of them behind (src/test/setup.ts runs it after every test). The license
// portal's license-server/ui/src/test/queryClients.ts is a copy; keep the two
// in step by hand.
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
//
// The same goes for React's own scheduler: a render outside act() (Testing
// Library's waitFor and findBy* turn act off while they wait) commits from a
// scheduler task and queues the passive effects as another one, which reads
// `window.event` when it runs, with or without effects left to flush. So the
// settling also waits until that scheduler has run every task queued so far.
import { createRequire } from 'node:module';
import { defaultScheduler, notifyManager, type QueryClient } from '@tanstack/react-query';
import { getConfig, waitFor } from '@testing-library/react';
import { expect } from 'vitest';

// The scheduler module react-dom itself loads (react-dom depends on it; the
// dashboard does not).
const requireFromReactDOM = createRequire(createRequire(import.meta.url).resolve('react-dom'));
const reactScheduler = requireFromReactDOM('scheduler') as {
  unstable_IdlePriority: number;
  unstable_scheduleCallback(priority: number, callback: () => void): unknown;
};

/**
 * Resolves once React's scheduler has nothing more urgent to run: an idle
 * task runs only after every task of a higher priority, including those
 * queued after it. Rejects at the deadline (a performance.now() time) when
 * React keeps queuing work, such as a component that renders in a loop.
 */
function reactSchedulerIdle(deadline: number): Promise<void> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('React scheduler still busy at the end of the test')), Math.max(deadline - performance.now(), 0));
    reactScheduler.unstable_scheduleCallback(reactScheduler.unstable_IdlePriority, () => {
      clearTimeout(timer);
      resolve();
    });
  });
}

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

function expectSettled(): void {
  for (const client of clients) {
    expect(client.isFetching(), 'queries still fetching at the end of the test').toBe(0);
    expect(client.isMutating(), 'mutations still running at the end of the test').toBe(0);
  }
  expect(queuedBatches, 'React Query notifications still queued at the end of the test').toBe(0);
}

/**
 * Waits until no query fetches, no mutation runs, no notification batch is
 * still queued and React's scheduler is idle. Fails the test when something
 * does not settle within the findBy* timeout (asyncUtilTimeout, for the whole
 * settling, well inside Vitest's hook timeout), rather than leaving it to
 * fire after the environment is gone.
 *
 * waitFor lets one more task run after its last passing check (Testing
 * Library drains with a setTimeout(0) before it returns), and that task, or
 * the scheduler tasks after it, can start new work, such as a fetch from an
 * effect of the last render; so the state is checked again once React's
 * scheduler is idle, and waited for again if it changed.
 */
export async function settleQueryClients(): Promise<void> {
  const deadline = performance.now() + getConfig().asyncUtilTimeout;
  for (;;) {
    await waitFor(expectSettled, { timeout: Math.max(deadline - performance.now(), 0) });
    await reactSchedulerIdle(deadline);
    try {
      expectSettled();
      return;
    } catch (err) {
      if (performance.now() >= deadline) throw err;
    }
  }
}

/** Drops every cached query and mutation of the test's clients (after unmount). */
export function clearQueryClients(): void {
  for (const client of clients) client.clear();
  clients.clear();
}

/**
 * Checks that neither the unmount nor the clearing queued a notification
 * batch. Separate from clearQueryClients so that it runs only after a test
 * that settled and cannot replace the error of one that did not.
 */
export function expectNoQueuedBatches(): void {
  expect(queuedBatches, 'React Query notifications queued by the unmount').toBe(0);
}
