// Server-side sessions and the audit writer (design spec §4, §6): cookie
// names follow PUBLIC_URL's scheme, the idle expiry moves at most once a
// minute and never past the absolute expiry, role changes apply at once,
// deleteUserSessions keeps the acting session, and audit() writes the
// actor's columns but refuses anything that looks like a key token.
import { describe, expect, test } from 'bun:test';
import { audit, findToken } from '../services/audit';
import { clearSessionCookie, createSession, deleteUserSessions, loginCookieName, resolveSession, serializeSessionCookie, sessionCookieName } from '../auth/sessions';
import { describeDb, settle, useTestDb } from './db';
import { createUser, GOLDEN_VECTORS, testConfig } from './fixtures';

describe('cookies', () => {
  test('__Host- names and Secure only on https', () => {
    const https = testConfig({ publicUrl: 'https://licenses.example.org', secureCookies: true });
    const http = testConfig();
    expect(sessionCookieName(https)).toBe('__Host-kls_session');
    expect(loginCookieName(https)).toBe('__Host-kls_login');
    expect(sessionCookieName(http)).toBe('kls_session');
    expect(serializeSessionCookie(https, 'abc')).toBe('__Host-kls_session=abc; Path=/; Secure; HttpOnly; SameSite=Lax');
    expect(serializeSessionCookie(http, 'abc')).toBe('kls_session=abc; Path=/; HttpOnly; SameSite=Lax');
    expect(clearSessionCookie(http)).toContain('Max-Age=0');
  });
});

describe('audit details', () => {
  test('findToken finds a key anywhere', () => {
    const key = GOLDEN_VECTORS[0]!.key;
    expect(findToken({ jti: 'lic_x', exp: 1 })).toBeNull();
    expect(findToken({ a: { b: [1, `key ${key}`] } })).toBe('details.a.b[1]');
    for (const v of GOLDEN_VECTORS) expect(findToken({ licensee: v.key })).toBe('details.licensee');
    expect(findToken({ [key]: 1 })).toBe('details.<key>');
  });

  test('ordinary names with dots are not tokens', () => {
    for (const name of ['MoneyJar.co.uk', 'HockeyJersey.co.uk', 'MoneyJar.co.', 'eyJ.eyJ.x', 'Joey Jr. & Co. Ltd.', 'heyJude.example.org']) {
      expect(findToken({ licensee: name, notes: `renewed for ${name}` })).toBeNull();
    }
  });
});

describeDb('sessions and audit in Postgres', () => {
  const t = useTestDb();
  const config = testConfig();
  const at = (iso: string) => new Date(iso);

  test('idle expiry moves at most once a minute, never past the lifetime', async () => {
    const user = await createUser(t.db, { roles: ['customer'] });
    const start = at('2026-10-12T08:00:00Z');
    const s = await createSession(t.db, { userId: user.id, authTime: start, ip: '192.0.2.1', userAgent: 'test', config, now: start });
    expect(s.idleExpiresAt.toISOString()).toBe('2026-10-12T08:30:00.000Z');
    expect(s.expiresAt.toISOString()).toBe('2026-10-12T20:00:00.000Z');

    const r1 = await resolveSession(t.db, s.cookieValue, at('2026-10-12T08:00:30Z'), config);
    expect(r1?.session.idleExpiresAt.toISOString()).toBe('2026-10-12T08:30:00.000Z');
    expect(r1?.principal.can('self.read')).toBe(true);
    const r2 = await resolveSession(t.db, s.cookieValue, at('2026-10-12T08:20:00Z'), config);
    expect(r2?.session.idleExpiresAt.toISOString()).toBe('2026-10-12T08:50:00.000Z');
    // Idle past the bumped expiry.
    expect(await resolveSession(t.db, s.cookieValue, at('2026-10-12T08:50:00Z'), config)).toBeNull();

    // Active all day still ends at the absolute expiry.
    const busy = await createSession(t.db, { userId: user.id, authTime: start, ip: null, userAgent: null, config, now: start });
    for (let now = start.getTime() + 20 * 60_000; now < at('2026-10-12T19:50:00Z').getTime(); now += 20 * 60_000) {
      expect(await resolveSession(t.db, busy.cookieValue, new Date(now), config)).not.toBeNull();
    }
    const late = await resolveSession(t.db, busy.cookieValue, at('2026-10-12T19:59:00Z'), config);
    expect(late?.session.idleExpiresAt.toISOString()).toBe('2026-10-12T20:00:00.000Z');
    expect(await resolveSession(t.db, busy.cookieValue, at('2026-10-12T20:00:00Z'), config)).toBeNull();
  });

  test('roles are read on every request; deleteUserSessions keeps the acting session', async () => {
    const user = await createUser(t.db, { roles: ['customer'] });
    const now = new Date();
    const a = await createSession(t.db, { userId: user.id, authTime: now, ip: null, userAgent: null, config, now });
    const b = await createSession(t.db, { userId: user.id, authTime: now, ip: null, userAgent: null, config, now });
    await t.db`update users set granted_roles = '{license_admin}' where id = ${user.id}`;
    const r = await resolveSession(t.db, a.cookieValue, now, config);
    expect(r?.principal.roles).toEqual(['license_admin']);
    expect(r?.principal.can('licenses.issue')).toBe(true);
    expect(await deleteUserSessions(t.db, user.id, a.idHash)).toBe(1);
    expect(await resolveSession(t.db, a.cookieValue, now, config)).not.toBeNull();
    expect(await resolveSession(t.db, b.cookieValue, now, config)).toBeNull();
    expect(await deleteUserSessions(t.db, user.id)).toBe(1);
  });

  test('audit writes the actor and refuses tokens', async () => {
    const user = await createUser(t.db, { name: 'Ada', email: 'ada@example.org', roles: ['license_admin'] });
    await audit(t.db, {
      actor: { kind: 'user', userId: user.id, label: 'Ada (ada@example.org)' },
      action: 'license.issued',
      targetType: 'license',
      targetId: 'lic_x',
      details: { plan: 'business', nodes: 5, licensee: 'MoneyJar.co.uk' },
      ip: '192.0.2.7',
      requestId: 'r-1',
    });
    await audit(t.db, {
      actor: { kind: 'public', email: 'p@example.org', ip: '2001:db8::1', userAgent: 'curl' },
      action: 'public_request.confirmed',
      targetType: 'public_request',
      targetId: 'pkr_x',
    });
    const rows = await t.db<{ actor_id: string | null; actor_label: string; details: unknown; ip: string | null; user_agent: string | null }[]>`
      select actor_id, actor_label, details, ip, user_agent from audit_events where target_id in ('lic_x', 'pkr_x') order by id`;
    expect(rows).toEqual([
      { actor_id: user.id, actor_label: 'Ada (ada@example.org)', details: { plan: 'business', nodes: 5, licensee: 'MoneyJar.co.uk' }, ip: '192.0.2.7', user_agent: null },
      { actor_id: null, actor_label: 'p@example.org', details: {}, ip: '2001:db8::1', user_agent: 'curl' },
    ]);
    const key = GOLDEN_VECTORS[0]!.key;
    await expect(
      settle(audit(t.db, { actor: { kind: 'system', label: 'cli' }, action: 'license.key_emailed', targetType: 'license', targetId: 'lic_x', details: { body: key } })),
    ).rejects.toThrow('looks like a key token');
  });
});
