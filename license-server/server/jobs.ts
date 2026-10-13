// Background cleanup (design spec §2 "Background"): every 10 minutes the
// server deletes expired sessions and sign-ins in flight, deletes
// unconfirmed public form requests 7 days after their link expired (rate
// limits count rows, so they are kept that long), and forgets the IP
// address and user agent of public form requests and sessions older than
// 90 days. One pass at a time; a failing pass is logged and the next one
// runs as usual. main.ts starts it and stops it on SIGTERM.

import { deleteExpiredLogins } from './auth/oidc';
import { deleteExpiredSessions } from './auth/sessions';
import { toInt, type Db } from './db/sql';
import type { Logger } from './log';

/** How often the cleanup runs. */
export const CLEANUP_INTERVAL_MS = 10 * 60_000;

/** Unconfirmed public form requests are deleted this long after their link expired. */
export const UNVERIFIED_RETENTION_MS = 7 * 86_400_000;

/** IP addresses and user agents are kept this long. */
export const NETWORK_DATA_RETENTION_MS = 90 * 86_400_000;

/** What one pass did. */
export interface CleanupResult {
  sessions: number;
  logins: number;
  publicRequests: number;
  anonymizedPublicRequests: number;
  anonymizedSessions: number;
}

/** One cleanup pass at `now`. */
export async function cleanup(db: Db, now: Date): Promise<CleanupResult> {
  const unverifiedBefore = new Date(now.getTime() - UNVERIFIED_RETENTION_MS);
  const networkBefore = new Date(now.getTime() - NETWORK_DATA_RETENTION_MS);
  const sessions = await deleteExpiredSessions(db, now);
  const logins = await deleteExpiredLogins(db, now);
  const pkr = await db<{ n: string }[]>`
    with d as (delete from public_key_requests where verified_at is null and expires_at < ${unverifiedBefore} returning 1)
    select count(*) as n from d`;
  const anonPkr = await db<{ n: string }[]>`
    with u as (update public_key_requests set ip = null, user_agent = null
                where created_at < ${networkBefore} and (ip is not null or user_agent is not null) returning 1)
    select count(*) as n from u`;
  const anonSessions = await db<{ n: string }[]>`
    with u as (update sessions set ip = null, user_agent = null
                where created_at < ${networkBefore} and (ip is not null or user_agent is not null) returning 1)
    select count(*) as n from u`;
  return {
    sessions,
    logins,
    publicRequests: toInt(pkr[0]?.n),
    anonymizedPublicRequests: toInt(anonPkr[0]?.n),
    anonymizedSessions: toInt(anonSessions[0]?.n),
  };
}

export interface Jobs {
  /** Runs one pass now (skipped, resolving null, while another runs). */
  runOnce(): Promise<CleanupResult | null>;
  /** Stops the timer; resolves when a running pass has finished. */
  stop(): Promise<void>;
}

export interface JobsOptions {
  db: Db;
  log: Logger;
  /** Default CLEANUP_INTERVAL_MS. */
  intervalMs?: number;
  /** Default () => new Date(). */
  clock?: () => Date;
  /** Run a first pass right away (default true). */
  runAtStart?: boolean;
}

/** Starts the cleanup timer (unref'd: it never keeps the process alive). */
export function startJobs(opts: JobsOptions): Jobs {
  const clock = opts.clock ?? (() => new Date());
  let running: Promise<CleanupResult | null> | null = null;
  let stopped = false;

  const runOnce = (): Promise<CleanupResult | null> => {
    if (running || stopped) return Promise.resolve(null);
    const pass = (async () => {
      try {
        const r = await cleanup(opts.db, clock());
        const total = r.sessions + r.logins + r.publicRequests + r.anonymizedPublicRequests + r.anonymizedSessions;
        if (total > 0) opts.log.info('cleanup done', { ...r });
        return r;
      } catch (e) {
        opts.log.error('cleanup failed', { err: e });
        return null;
      } finally {
        running = null;
      }
    })();
    running = pass;
    return pass;
  };

  const timer = setInterval(() => void runOnce(), opts.intervalMs ?? CLEANUP_INTERVAL_MS);
  timer.unref();
  if (opts.runAtStart ?? true) void runOnce();

  return {
    runOnce,
    stop: async () => {
      stopped = true;
      clearInterval(timer);
      if (running) await running;
    },
  };
}
