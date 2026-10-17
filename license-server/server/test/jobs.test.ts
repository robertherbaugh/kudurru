// The background cleanup (design spec §2 "Background"): expired sessions
// and sign-ins in flight go, unconfirmed public form requests go 7 days
// after their link expired (confirmed ones stay), approval links go 30 days
// after they expired, IP addresses and user agents older than 90 days are
// forgotten, a pass is skipped while one runs, and stop() waits for it.
import { expect, test } from 'bun:test';
import { createSession } from '../auth/sessions';
import { newId } from '../db/ids';
import { cleanup, startJobs } from '../jobs';
import { MemoryLogger } from '../log';
import { describeDb, useTestDb } from './db';
import { createUser, testConfig } from './fixtures';

describeDb('cleanup', () => {
  const t = useTestDb();
  const config = testConfig();
  const day = 86_400_000;

  async function publicRequest(createdAt: Date, expiresAt: Date, verifiedAt: Date | null): Promise<string> {
    const id = newId('publicRequest');
    await t.db`
      insert into public_key_requests (id, name, email, use_description, noncommercial_attested_at, ip, user_agent, token_hash, created_at, expires_at, verified_at)
      values (${id}, 'Ana', 'ana@example.org', 'home lab', ${createdAt}, '192.0.2.1', 'Firefox', ${Buffer.from(id)}, ${createdAt}, ${expiresAt}, ${verifiedAt})`;
    return id;
  }

  test('one pass', async () => {
    const now = new Date('2026-10-12T12:00:00Z');
    const user = await createUser(t.db);
    const live = await createSession(t.db, { userId: user.id, authTime: now, ip: '192.0.2.9', userAgent: 'ua', config, now: new Date(now.getTime() - 60_000) });
    const idle = await createSession(t.db, { userId: user.id, authTime: now, ip: null, userAgent: null, config, now: new Date(now.getTime() - 31 * 60_000) });
    await t.db`
      insert into oidc_logins (state, binding_hash, nonce, code_verifier, next, created_at, expires_at) values
        ('fresh', '\\x01'::bytea, 'n', 'v', '/', ${now}, ${new Date(now.getTime() + 60_000)}),
        ('stale', '\\x01'::bytea, 'n', 'v', '/', ${new Date(now.getTime() - 20 * 60_000)}, ${new Date(now.getTime() - 10 * 60_000)})`;
    const recentUnverified = await publicRequest(new Date(now.getTime() - 2 * day), new Date(now.getTime() - day), null);
    const oldUnverified = await publicRequest(new Date(now.getTime() - 9 * day), new Date(now.getTime() - 8 * day), null);
    const oldVerified = await publicRequest(new Date(now.getTime() - 100 * day), new Date(now.getTime() - 99 * day), new Date(now.getTime() - 99.5 * day));
    const requestId = newId('request');
    await t.db`
      insert into license_requests (id, source, requester_email, name, use_description, attestation, review_reason)
      values (${requestId}, 'web', 'ana@example.org', 'Ana', 'home lab', ${{ source: 'kudurru.io form', at: now.toISOString() }}, 'auto_approve_off')`;
    const link = (tag: string, expiredDaysAgo: number) => t.db`
      insert into request_approval_links (token_hash, request_id, sent_to, created_at, expires_at)
      values (${Buffer.from(tag)}, ${requestId}, 'admin@example.org', ${new Date(now.getTime() - (expiredDaysAgo + 7) * day)}, ${new Date(now.getTime() - expiredDaysAgo * day)})`;
    await link('kept', 29);
    await link('gone', 31);
    const business = async (createdDaysAgo: number) => {
      const id = newId('businessRequest');
      await t.db`
        insert into business_requests (id, name, email, company, edition, nodes, ip, user_agent, created_at)
        values (${id}, 'Ana', 'ana@example.org', 'Example GmbH', 'core', 5, '192.0.2.7', 'Firefox', ${new Date(now.getTime() - createdDaysAgo * day)})`;
      return id;
    };
    const oldBusiness = await business(100);
    const recentBusiness = await business(1);

    const r = await cleanup(t.db, now);
    expect(r).toEqual({ sessions: 1, logins: 1, publicRequests: 1, approvalLinks: 1, anonymizedPublicRequests: 1, anonymizedBusinessRequests: 1, anonymizedSessions: 0 });
    expect((await t.db<{ token_hash: Buffer }[]>`select token_hash from request_approval_links`).map((l) => l.token_hash.toString())).toEqual(['kept']);

    const sessions = await t.db<{ id_hash: Buffer }[]>`select id_hash from sessions where user_id = ${user.id}`;
    expect(sessions.map((s) => s.id_hash.toString('hex'))).toEqual([live.idHash.toString('hex')]);
    expect(idle.idHash.length).toBe(32);
    expect((await t.db<{ state: string }[]>`select state from oidc_logins order by state`).map((r) => r.state)).toEqual(['fresh']);
    const pkr = await t.db<{ id: string; ip: string | null; user_agent: string | null }[]>`select id, host(ip) as ip, user_agent from public_key_requests order by created_at`;
    expect(pkr).toEqual([
      { id: oldVerified, ip: null, user_agent: null },
      { id: recentUnverified, ip: '192.0.2.1', user_agent: 'Firefox' },
    ]);
    expect(pkr.find((p) => p.id === oldUnverified)).toBeUndefined();
    const brq = await t.db<{ id: string; ip: string | null; user_agent: string | null }[]>`select id, host(ip) as ip, user_agent from business_requests order by created_at`;
    expect(brq).toEqual([
      { id: oldBusiness, ip: null, user_agent: null },
      { id: recentBusiness, ip: '192.0.2.7', user_agent: 'Firefox' },
    ]);

    // Nothing left to do.
    expect(await cleanup(t.db, now)).toEqual({ sessions: 0, logins: 0, publicRequests: 0, approvalLinks: 0, anonymizedPublicRequests: 0, anonymizedBusinessRequests: 0, anonymizedSessions: 0 });
  });

  test('sessions older than 90 days lose ip and user agent', async () => {
    const now = new Date();
    const user = await createUser(t.db);
    const long = { session: { idleTimeoutMs: 200 * day, lifetimeMs: 200 * day } };
    await createSession(t.db, { userId: user.id, authTime: now, ip: '192.0.2.10', userAgent: 'old', config: long, now: new Date(now.getTime() - 91 * day) });
    const r = await cleanup(t.db, now);
    expect(r.anonymizedSessions).toBe(1);
    const rows = await t.db<{ ip: string | null; user_agent: string | null }[]>`select host(ip) as ip, user_agent from sessions where user_id = ${user.id}`;
    expect(rows).toEqual([{ ip: null, user_agent: null }]);
  });

  test('the timer: runs at start, one pass at a time, failures logged, stop waits', async () => {
    const log = new MemoryLogger();
    const jobs = startJobs({ db: t.db, log, intervalMs: 60_000, runAtStart: true });
    // The pass started at startJobs is running: another is skipped.
    expect(await jobs.runOnce()).toBeNull();
    await jobs.stop();
    expect(await jobs.runOnce()).toBeNull();
    expect(log.find('cleanup failed')).toHaveLength(0);

    const broken = new Proxy(t.db, {
      apply: () => Promise.reject(new Error('connection refused')),
      get: (target, prop, receiver): unknown => (prop === 'begin' ? () => Promise.reject(new Error('connection refused')) : Reflect.get(target, prop, receiver)),
    });
    const failing = startJobs({ db: broken, log, intervalMs: 60_000, runAtStart: false });
    expect(await failing.runOnce()).toBeNull();
    expect(log.find('cleanup failed')).toHaveLength(1);
    await failing.stop();
  });
});

test('the cleanup interval is 10 minutes', async () => {
  const { CLEANUP_INTERVAL_MS, UNVERIFIED_RETENTION_MS, NETWORK_DATA_RETENTION_MS } = await import('../jobs');
  expect([CLEANUP_INTERVAL_MS, UNVERIFIED_RETENTION_MS, NETWORK_DATA_RETENTION_MS]).toEqual([600_000, 7 * 86_400_000, 90 * 86_400_000]);
});
