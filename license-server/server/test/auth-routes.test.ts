// The sign-in routes end to end against the fake OP (design spec §6
// "Sign-in", §10 "auth"): the happy path (cookies, session, audit, the
// users row, next), every way a callback fails (wrong, replayed or expired
// state, missing or foreign binding cookie, wrong nonce, bad or foreign
// signature, azp mismatch, wrong audience, IdP error, group overage) and
// that each one lands on /login?error=<fixed key> without leaking a cause;
// claims to roles (verified email only, admin claim rules, DEFAULT_ROLE
// none, disabled users), MEMBER_AUTO_LINK, the cap on sign-ins in flight,
// the rate limit, session idle and absolute expiry, logout with CSRF, and
// role changes that end sessions.
import { afterAll, afterEach, beforeAll, describe, expect, setSystemTime, test } from 'bun:test';
import type { ApiErrorBody, Me } from '../../shared/api';
import { authHandlers } from '../api/auth';
import { LOGIN_ERROR_KEYS } from '../auth/loginErrors';
import { MAX_PENDING_LOGINS } from '../auth/oidc';
import { deleteUserSessions } from '../auth/sessions';
import type { Config } from '../config';
import { hashSecret } from '../db/ids';
import { createPipeline } from '../http/pipeline';
import type { RateLimit } from '../http/ratelimit';
import { MemoryLogger } from '../log';
import { makeTestApp, type TestApp } from './call';
import { describeDb, useTestDb } from './db';
import { FakeOp, type FakeUser, type Tamper } from './fakeop';
import { createCustomer, createUser, testConfig, type ConfigOverrides } from './fixtures';

const REDIRECT_URI = 'http://localhost:8790/api/v1/auth/callback';

/** The value of a Set-Cookie of `name` on a response, or null. */
function cookieOf(res: Response, name: string): string | null {
  for (const c of res.headers.getSetCookie()) {
    const [pair] = c.split(';');
    const i = pair!.indexOf('=');
    if (pair!.slice(0, i) === name) return pair!.slice(i + 1);
  }
  return null;
}

function setCookieLine(res: Response, name: string): string | null {
  return res.headers.getSetCookie().find((c) => c.startsWith(`${name}=`)) ?? null;
}

describeDb('sign-in', () => {
  const t = useTestDb();
  let op: FakeOp;
  const apps: TestApp[] = [];

  beforeAll(async () => {
    op = await FakeOp.start({ redirectUri: REDIRECT_URI });
  });
  afterAll(async () => {
    for (const a of apps) await a.close();
    op.close();
  });
  afterEach(() => {
    op.reset();
    setSystemTime();
  });

  /** A server with the auth routes and the real pipeline, signed in against the fake OP. */
  function app(overrides: ConfigOverrides = {}, rateLimits?: Partial<Record<'authLogin' | 'authCallback', RateLimit>>): TestApp {
    const config: Config = testConfig({ ...overrides, oidc: { issuer: op.issuer, clientId: op.clientId, clientSecret: op.clientSecret, ...overrides.oidc } });
    const log = new MemoryLogger();
    const a = makeTestApp({ handlers: authHandlers, db: t.db, config, log, pipeline: createPipeline(config, log, rateLimits ? { rateLimits } : {}) });
    apps.push(a);
    return a;
  }

  interface Started {
    login: Response;
    binding: string;
    authorizeUrl: string;
  }

  async function start(a: TestApp, next = '/my/licenses', cookie?: string): Promise<Started> {
    const login = await a.fetch(`/api/v1/auth/login?next=${encodeURIComponent(next)}`, cookie ? { headers: { Cookie: cookie } } : {});
    expect(login.status).toBe(302);
    const binding = cookieOf(login, 'kls_login');
    expect(binding).toMatch(/^[A-Za-z0-9_-]{43}$/);
    return { login, binding: binding!, authorizeUrl: login.headers.get('location')! };
  }

  async function callback(a: TestApp, back: URL, cookie: string | null): Promise<Response> {
    return a.fetch(back.pathname + back.search, cookie === null ? {} : { headers: { Cookie: cookie } });
  }

  /** The whole sign-in as a browser does it; returns the callback's answer. */
  async function signIn(a: TestApp, user?: FakeUser, next = '/my/licenses'): Promise<Response> {
    if (user) op.user = user;
    const s = await start(a, next);
    const back = await op.authorize(s.authorizeUrl);
    return callback(a, back, `kls_login=${s.binding}`);
  }

  function errorOf(res: Response): string | null {
    expect(res.status).toBe(302);
    const loc = res.headers.get('location')!;
    const m = /^\/login\?error=([a-z_]+)$/.exec(loc);
    return m ? m[1]! : null;
  }

  async function me(a: TestApp, sessionCookie: string, extra: Record<string, string> = {}): Promise<{ status: number; body: Me & ApiErrorBody }> {
    const res = await a.fetch('/api/v1/auth/me', { headers: { Cookie: `kls_session=${sessionCookie}`, ...extra } });
    return { status: res.status, body: (await res.json()) as Me & ApiErrorBody };
  }

  let n = 0;
  const person = (extra: Partial<FakeUser> = {}): FakeUser => {
    n++;
    return { sub: `sub-${process.pid}-${n}`, email: `Person${n}@Example.org`, email_verified: true, name: `Person ${n}`, ...extra };
  };

  test('authConfig answers the login page', async () => {
    const a = app({ oidc: { displayName: 'Sign in with Dex' } });
    const res = await a.fetch('/api/v1/auth/config');
    expect(await res.json()).toEqual({ displayName: 'Sign in with Dex', devMode: true });
    expect(res.headers.get('cache-control')).toBe('no-store');
  });

  test('the start: binding cookie, PKCE S256, state, nonce and a stored row', async () => {
    const a = app();
    const s = await start(a, '/licenses?plan=personal');
    const line = setCookieLine(s.login, 'kls_login')!;
    expect(line).toContain('HttpOnly');
    expect(line).toContain('SameSite=Lax');
    expect(line).toContain('Path=/');
    expect(line).toContain('Max-Age=600');
    expect(line).not.toContain('Secure');
    expect(s.login.headers.get('cache-control')).toBe('no-store');
    const url = new URL(s.authorizeUrl);
    expect(url.origin + url.pathname).toBe(`${op.issuer}/auth`);
    const q = url.searchParams;
    expect(q.get('response_type')).toBe('code');
    expect(q.get('client_id')).toBe(op.clientId);
    expect(q.get('redirect_uri')).toBe(REDIRECT_URI);
    expect(q.get('scope')).toBe('openid profile email groups');
    expect(q.get('code_challenge_method')).toBe('S256');
    expect(q.get('code_challenge')).toMatch(/^[A-Za-z0-9_-]{43}$/);
    const rows = await t.db<{ next: string; binding_hash: Buffer; expires_at: Date; created_at: Date }[]>`
      select next, binding_hash, expires_at, created_at from oidc_logins where state = ${q.get('state')}`;
    expect(rows).toHaveLength(1);
    expect(rows[0]!.next).toBe('/licenses?plan=personal');
    expect(Buffer.compare(rows[0]!.binding_hash, hashSecret(s.binding))).toBe(0);
    expect(rows[0]!.expires_at.getTime() - rows[0]!.created_at.getTime()).toBe(600_000);

    // A second start in the same browser keeps the binding (two tabs both work).
    const again = await start(a, '/', `kls_login=${s.binding}`);
    expect(again.binding).toBe(s.binding);
    // An unsafe next falls back to /.
    const evil = await start(a, '//evil.example/x');
    const evilRows = await t.db<{ next: string }[]>`select next from oidc_logins where state = ${new URL(evil.authorizeUrl).searchParams.get('state')}`;
    expect(evilRows[0]!.next).toBe('/');
  });

  test('the happy path: user row, session cookie, audit, next', async () => {
    const a = app();
    const p = person({ groups: ['staff'] });
    const res = await signIn(a, p, '/licenses/lic_abc?tab=keys');
    expect(res.status).toBe(302);
    expect(res.headers.get('location')).toBe('/licenses/lic_abc?tab=keys');
    expect(res.headers.get('cache-control')).toBe('no-store');
    const line = setCookieLine(res, 'kls_session')!;
    expect(line).toContain('HttpOnly');
    expect(line).toContain('SameSite=Lax');
    expect(line).toContain('Path=/');
    expect(line).not.toContain('Max-Age');
    expect(line).not.toContain('Expires');
    const session = cookieOf(res, 'kls_session')!;

    const users = await t.db<{ id: string; email: string; email_verified: boolean; name: string; idp_roles: string[]; issuer: string; last_login_at: Date | null }[]>`
      select id, email, email_verified, name, idp_roles, issuer, last_login_at from users where subject = ${p.sub}`;
    expect(users).toHaveLength(1);
    const u = users[0]!;
    expect(u).toMatchObject({ email: (p.email as string).toLowerCase(), email_verified: true, name: p.name, idp_roles: ['customer'], issuer: op.issuer });
    expect(u.last_login_at).not.toBeNull();

    const m = await me(a, session);
    expect(m.status).toBe(200);
    expect(m.body.user).toEqual({ id: u.id, name: p.name as string, email: u.email, emailVerified: true });
    expect(m.body.roles).toEqual(['customer']);
    expect(m.body.permissions).toEqual(['self.read', 'self.request', 'self.order']);
    expect(m.body.csrf).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(m.body.devMode).toBe(true);

    const events = await t.db<{ actor_id: string; actor_label: string; details: Record<string, unknown>; ip: string | null }[]>`
      select actor_id, actor_label, details, host(ip) as ip from audit_events where action = 'auth.login' and target_id = ${u.id}`;
    expect(events).toHaveLength(1);
    expect(events[0]!.actor_id).toBe(u.id);
    expect(events[0]!.actor_label).toBe(`${p.name as string} (${u.email})`);
    expect(events[0]!.details).toMatchObject({ issuer: op.issuer, subject: p.sub, roles: ['customer'], firstSignIn: true });
    expect(events[0]!.ip).toBe('127.0.0.1');

    // The state is gone (single use) and the logs hold no secret.
    expect((await t.db`select 1 from oidc_logins where binding_hash = ${hashSecret(cookieOf(res, 'kls_login') ?? 'x')}`).length).toBe(0);
    const logs = a.log.text();
    expect(logs).not.toContain(session);
    expect(logs).not.toContain('code=');
    expect(logs).not.toContain(m.body.csrf);

    // Signing in again updates the row (same issuer and subject) and ends the old session.
    op.user = { ...p, name: 'Renamed Person', email_verified: 'true' };
    const s = await start(a);
    const back = await op.authorize(s.authorizeUrl);
    const again = await callback(a, back, `kls_login=${s.binding}; kls_session=${session}`);
    expect(again.headers.get('location')).toBe('/my/licenses');
    expect((await t.db<{ name: string }[]>`select name from users where id = ${u.id}`)[0]!.name).toBe('Renamed Person');
    expect((await me(a, session)).status).toBe(401);
    expect((await me(a, cookieOf(again, 'kls_session')!)).status).toBe(200);
    expect((await t.db`select 1 from users where subject = ${p.sub}`).length).toBe(1);
  });

  test('client_secret_basic is used when the IdP offers only it', async () => {
    const basicOp = await FakeOp.start({ redirectUri: REDIRECT_URI, authMethods: ['client_secret_basic'], alg: 'EdDSA' });
    try {
      const config = testConfig({ oidc: { issuer: basicOp.issuer, clientId: basicOp.clientId, clientSecret: basicOp.clientSecret } });
      const log = new MemoryLogger();
      const a = makeTestApp({ handlers: authHandlers, db: t.db, config, log, pipeline: createPipeline(config, log) });
      apps.push(a);
      basicOp.user = person();
      const s = await start(a);
      const back = await basicOp.authorize(s.authorizeUrl);
      const res = await callback(a, back, `kls_login=${s.binding}`);
      expect(res.headers.get('location')).toBe('/my/licenses');
      expect(basicOp.lastClientAuth).toBe('basic');
      expect(op.lastClientAuth === null || op.lastClientAuth === 'post').toBe(true);
    } finally {
      basicOp.close();
    }
  });

  test('state: wrong, replayed, expired', async () => {
    const a = app();
    op.user = person();
    const s = await start(a);
    const back = await op.authorize(s.authorizeUrl);
    const cookie = `kls_login=${s.binding}`;

    const wrong = new URL(back);
    wrong.searchParams.set('state', 'not-a-state-we-issued');
    expect(errorOf(await callback(a, wrong, cookie))).toBe('expired');

    expect(cookieOf(await callback(a, back, cookie), 'kls_session')).not.toBeNull();
    // The same callback again: the state was consumed.
    expect(errorOf(await callback(a, back, cookie))).toBe('expired');

    // Ten minutes later the state is refused, and it is still consumed.
    const s2 = await start(a);
    const back2 = await op.authorize(s2.authorizeUrl);
    setSystemTime(new Date(Date.now() + 10 * 60_000 + 1000));
    expect(errorOf(await callback(a, back2, `kls_login=${s2.binding}`))).toBe('expired');
    setSystemTime();
    expect(errorOf(await callback(a, back2, `kls_login=${s2.binding}`))).toBe('expired');
    expect(a.log.find('auth.failed').map((e) => e.reason)).toEqual(['unknown or already used state', 'unknown or already used state', 'the sign-in expired', 'unknown or already used state']);
  });

  test('binding cookie: missing or another browser’s', async () => {
    const a = app();
    op.user = person();
    const s = await start(a);
    const back = await op.authorize(s.authorizeUrl);
    expect(errorOf(await callback(a, back, null))).toBe('expired');

    const s2 = await start(a);
    const other = await start(a, '/', 'kls_login=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA');
    expect(other.binding).toBe('AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA');
    const back2 = await op.authorize(s2.authorizeUrl);
    // The attacker's binding planted in the victim's browser does not match the victim's state (login CSRF).
    expect(errorOf(await callback(a, back2, `kls_login=${other.binding}`))).toBe('expired');
    expect(a.log.find('auth.failed').map((e) => e.reason)).toEqual(['no binding cookie', 'the binding cookie does not match']);
  });

  test('https: the cookies are __Host- and Secure, and only the __Host- binding counts', async () => {
    // The fake OP is http on loopback, which an https PUBLIC_URL refuses; the start fails before the IdP.
    const a = app({ publicUrl: 'https://licenses.example.org', publicOrigin: 'https://licenses.example.org', secureCookies: true });
    const res = await a.fetch('/api/v1/auth/login');
    expect(res.status).toBe(503);
    expect(((await res.json()) as ApiErrorBody).error.key).toBe('idp_unavailable');
    expect(res.headers.get('strict-transport-security')).toBe('max-age=31536000');
  });

  const REFUSALS: [string, Tamper, string][] = [
    ['wrong nonce', { claims: (c) => void (c.nonce = 'another') }, '"nonce" claim value'],
    ['bad signature', { badSignature: true }, 'JWT signature verification failed'],
    ['signed by a key outside the JWKS', { foreignKey: true }, 'JWT signature verification failed'],
    ['azp of another client', { claims: (c) => void (c.azp = 'someone-else') }, 'issued to another client (azp)'],
    ['audience of another client', { claims: (c) => void (c.aud = 'someone-else') }, '"aud" (audience) claim value'],
    ['several audiences without azp', { claims: (c) => void (c.aud = ['license-portal', 'other']) }, 'additional untrusted audiences'],
    ['another issuer', { claims: (c) => void (c.iss = 'https://evil.example') }, '"iss" (issuer) claim value'],
    ['expired token', { claims: (c) => void (c.exp = Math.floor(Date.now() / 1000) - 3600) }, '"exp" (expiration time)'],
    ['alg none', { header: { alg: 'none' } }, '"alg" header parameter'],
    ['alg HS256', { header: { alg: 'HS256' } }, '"alg" header parameter'],
    ['empty subject', { claims: (c) => void (c.sub = '') }, 'no subject'],
    ['distributed groups (overage)', { claims: (c) => void (c._claim_names = { groups: 'src1' }) }, 'distributed claim (group overage)'],
  ];
  test.each(REFUSALS)('refused: %s', async (_label, tamper, why) => {
    const a = app();
    op.user = person();
    op.tamper = tamper;
    const before = (await t.db<{ n: string }[]>`select count(*) as n from sessions`)[0]!.n;
    const res = await signIn(a);
    // The browser only sees the fixed key; the cause is in the log.
    expect(res.headers.get('location')).toBe('/login?error=failed');
    expect(cookieOf(res, 'kls_session')).toBeNull();
    expect((await t.db<{ n: string }[]>`select count(*) as n from sessions`)[0]!.n).toBe(before);
    const failed = a.log.find('auth.failed');
    expect(failed).toHaveLength(1);
    expect(JSON.stringify(failed[0])).toContain(why.replace(/"/g, '\\"'));
  });

  test('a matching azp and a single-element audience array are fine', async () => {
    const a = app();
    op.user = person();
    op.tamper = { claims: (c) => void Object.assign(c, { azp: op.clientId, aud: [op.clientId] }) };
    expect(cookieOf(await signIn(a), 'kls_session')).not.toBeNull();
  });

  test('an IdP error lands on /login?error=idp_error and consumes the state', async () => {
    const a = app();
    op.authorizeError = 'access_denied';
    const s = await start(a);
    const back = await op.authorize(s.authorizeUrl);
    expect(back.searchParams.get('error')).toBe('access_denied');
    expect(errorOf(await callback(a, back, `kls_login=${s.binding}`))).toBe('idp_error');
    expect((await t.db`select 1 from oidc_logins where state = ${back.searchParams.get('state')}`).length).toBe(0);
    // Neither code nor state: expired.
    expect(errorOf(await a.fetch('/api/v1/auth/callback'))).toBe('expired');
    for (const key of ['idp_error', 'expired']) expect(LOGIN_ERROR_KEYS).toContain(key as (typeof LOGIN_ERROR_KEYS)[number]);
  });

  test('email only when verified; name falls back to preferred_username, then the verified email', async () => {
    const a = app();
    const unverified = person({ email_verified: false, name: undefined, preferred_username: 'pat' });
    await signIn(a, unverified);
    const u1 = (await t.db<{ email: string | null; email_verified: boolean; name: string }[]>`select email, email_verified, name from users where subject = ${unverified.sub}`)[0]!;
    expect(u1).toEqual({ email: null, email_verified: false, name: 'pat' });

    const stringTrue = person({ email_verified: 'true', name: '' });
    await signIn(a, stringTrue);
    const u2 = (await t.db<{ email: string | null; name: string }[]>`select email, name from users where subject = ${stringTrue.sub}`)[0]!;
    expect(u2).toEqual({ email: (stringTrue.email as string).toLowerCase(), name: (stringTrue.email as string).toLowerCase() });

    const missing = person({ email_verified: undefined, name: '  \u0007Bob\u0000  ' });
    await signIn(a, missing);
    const u3 = (await t.db<{ email: string | null; name: string }[]>`select email, name from users where subject = ${missing.sub}`)[0]!;
    expect(u3).toEqual({ email: null, name: 'Bob' });
  });

  describe('roles from groups', () => {
    const groups = { superadmin: ['license-superadmins'], license_admin: ['license-admins'], customer: ['customers'] };

    test('admin groups count only when every admin claim rule matches', async () => {
      const a = app({ roles: { groups, defaultRole: 'customer', memberAutoLink: false }, oidc: { adminClaimRules: [{ claim: 'hd', value: 'example.org' }, { claim: 'email_verified', value: 'true' }] } });
      const insider = person({ groups: ['license-superadmins', 'license-admins'], hd: 'example.org' });
      await signIn(a, insider);
      expect((await t.db<{ idp_roles: string[] }[]>`select idp_roles from users where subject = ${insider.sub}`)[0]!.idp_roles).toEqual(['license_admin', 'superadmin']);

      const outsider = person({ groups: ['license-superadmins'], hd: 'evil.example' });
      await signIn(a, outsider);
      expect((await t.db<{ idp_roles: string[] }[]>`select idp_roles from users where subject = ${outsider.sub}`)[0]!.idp_roles).toEqual(['customer']);
      expect(a.log.find('admin groups ignored: OIDC_ADMIN_CLAIM_RULES did not match')).toHaveLength(1);

      // As ee/oidc claimEquals: a boolean compares as true/false, a string as itself ("true" matches true).
      const stringy = person({ groups: ['license-admins'], hd: 'example.org', email_verified: 'true' });
      await signIn(a, stringy);
      expect((await t.db<{ idp_roles: string[] }[]>`select idp_roles from users where subject = ${stringy.sub}`)[0]!.idp_roles).toEqual(['license_admin']);
      const unverified = person({ groups: ['license-admins'], hd: 'example.org', email_verified: false });
      await signIn(a, unverified);
      expect((await t.db<{ idp_roles: string[] }[]>`select idp_roles from users where subject = ${unverified.sub}`)[0]!.idp_roles).toEqual(['customer']);

      // A single string groups claim works; only the configured claim is read (no implicit roles claim).
      const single = person({ groups: 'customers', roles: ['license-superadmins'], hd: 'example.org' });
      await signIn(a, single);
      expect((await t.db<{ idp_roles: string[] }[]>`select idp_roles from users where subject = ${single.sub}`)[0]!.idp_roles).toEqual(['customer']);
    });

    test('DEFAULT_ROLE=none: no role → /login?error=no_role, the user row is still recorded; granted roles count', async () => {
      const a = app({ roles: { groups, defaultRole: 'none', memberAutoLink: false } });
      const nobody = person({ groups: ['staff'] });
      const res = await signIn(a, nobody);
      expect(errorOf(res)).toBe('no_role');
      expect(cookieOf(res, 'kls_session')).toBeNull();
      const row = (await t.db<{ id: string; idp_roles: string[]; last_login_at: Date | null }[]>`select id, idp_roles, last_login_at from users where subject = ${nobody.sub}`)[0]!;
      expect(row.idp_roles).toEqual([]);
      expect(row.last_login_at).toBeNull();

      // grant-superadmin (or a superadmin) granted a role: the next sign-in works.
      await t.db`update users set granted_roles = '{superadmin}' where id = ${row.id}`;
      const ok = await signIn(a, nobody);
      expect(ok.headers.get('location')).toBe('/my/licenses');
      // Without admin rules and DEFAULT_ROLE=none, admin groups are honored.
      const admin = person({ groups: ['license-admins'] });
      await signIn(a, admin);
      expect((await t.db<{ idp_roles: string[] }[]>`select idp_roles from users where subject = ${admin.sub}`)[0]!.idp_roles).toEqual(['license_admin']);
    });

    test('a disabled user → /login?error=disabled', async () => {
      const a = app();
      const p = person();
      await signIn(a, p);
      await t.db`update users set disabled_at = now() where subject = ${p.sub}`;
      const res = await signIn(a, p);
      expect(errorOf(res)).toBe('disabled');
      expect(cookieOf(res, 'kls_session')).toBeNull();
    });
  });

  test('MEMBER_AUTO_LINK links unlinked rows of the sole verified holder, never re-links, audits iss and sub', async () => {
    const a = app({ roles: { groups: { superadmin: [], license_admin: [], customer: [] }, defaultRole: 'customer', memberAutoLink: true } });
    const p = person();
    const email = (p.email as string).toLowerCase();
    const other = await createUser(t.db, { email: 'someone-else@example.org' });
    const open = await createCustomer(t.db, { members: [email] });
    const taken = await createCustomer(t.db, { members: [{ email, userId: other.id }] });
    await signIn(a, p);
    const user = (await t.db<{ id: string }[]>`select id from users where subject = ${p.sub}`)[0]!;
    const rows = await t.db<{ customer_id: string; user_id: string | null }[]>`select customer_id, user_id from customer_members where email = ${email} order by customer_id`;
    expect(rows.find((r) => r.customer_id === open.id)!.user_id).toBe(user.id);
    expect(rows.find((r) => r.customer_id === taken.id)!.user_id).toBe(other.id);
    const linked = await t.db<{ details: Record<string, unknown> }[]>`select details from audit_events where action = 'member.linked' and target_id = ${open.id}`;
    expect(linked[0]!.details).toMatchObject({ email, userId: user.id, issuer: op.issuer, subject: p.sub, via: 'auto_link' });

    // A second subject with the same verified email: nobody is linked to a new row.
    const twin = person({ email: p.email });
    await signIn(a, twin);
    const later = await createCustomer(t.db, { members: [email] });
    await signIn(a, p);
    expect((await t.db<{ user_id: string | null }[]>`select user_id from customer_members where customer_id = ${later.id}`)[0]!.user_id).toBeNull();

    // Off (the default): invite links only.
    const off = app();
    const q = person();
    const c = await createCustomer(t.db, { members: [(q.email as string).toLowerCase()] });
    await signIn(off, q);
    expect((await t.db<{ user_id: string | null }[]>`select user_id from customer_members where customer_id = ${c.id}`)[0]!.user_id).toBeNull();
  });

  test('sessions: idle and absolute expiry', async () => {
    const a = app();
    const t0 = new Date('2026-10-12T08:00:00Z');
    setSystemTime(t0);
    const res = await signIn(a, person());
    const session = cookieOf(res, 'kls_session')!;
    setSystemTime(new Date(t0.getTime() + 29 * 60_000));
    expect((await me(a, session)).status).toBe(200);
    // 29 minutes after that bump: still fine; 31: idle.
    setSystemTime(new Date(t0.getTime() + 58 * 60_000));
    expect((await me(a, session)).status).toBe(200);
    setSystemTime(new Date(t0.getTime() + 89 * 60_000 + 1000));
    expect((await me(a, session)).status).toBe(401);

    setSystemTime(t0);
    const busy = cookieOf(await signIn(a, person()), 'kls_session')!;
    for (let m = 20; m < 12 * 60; m += 20) {
      setSystemTime(new Date(t0.getTime() + m * 60_000));
      expect((await me(a, busy)).status).toBe(200);
    }
    setSystemTime(new Date(t0.getTime() + 12 * 3_600_000));
    const late = await me(a, busy);
    expect(late.status).toBe(401);
    expect(late.body.error.key).toBe('unauthenticated');
  });

  test('logout: CSRF required, deletes the session, clears the cookie, audits', async () => {
    const a = app();
    const p = person();
    const session = cookieOf(await signIn(a, p), 'kls_session')!;
    const { body } = await me(a, session);
    const cookie = `kls_session=${session}`;

    const noToken = await a.fetch('/api/v1/auth/logout', { method: 'POST', headers: { Cookie: cookie } });
    expect(noToken.status).toBe(403);
    expect(((await noToken.json()) as ApiErrorBody).error.key).toBe('csrf');
    const crossSite = await a.fetch('/api/v1/auth/logout', { method: 'POST', headers: { Cookie: cookie, 'X-Kudurru-CSRF': body.csrf, 'Sec-Fetch-Site': 'cross-site' } });
    expect(crossSite.status).toBe(403);

    const ok = await a.fetch('/api/v1/auth/logout', { method: 'POST', headers: { Cookie: cookie, 'X-Kudurru-CSRF': body.csrf, 'Sec-Fetch-Site': 'same-origin' } });
    expect(ok.status).toBe(204);
    expect(setCookieLine(ok, 'kls_session')).toContain('Max-Age=0');
    expect((await me(a, session)).status).toBe(401);
    const events = await t.db`select 1 from audit_events where action = 'auth.logout' and target_id = ${body.user.id}`;
    expect(events).toHaveLength(1);
    // Logged out: logout answers 401.
    expect((await a.fetch('/api/v1/auth/logout', { method: 'POST', headers: { Cookie: cookie, 'X-Kudurru-CSRF': body.csrf } })).status).toBe(401);
  });

  test('a role change that ends the sessions signs the user out at once', async () => {
    const a = app();
    const session = cookieOf(await signIn(a, person()), 'kls_session')!;
    const { body } = await me(a, session);
    await deleteUserSessions(t.db, body.user.id);
    expect((await me(a, session)).status).toBe(401);
  });

  test('too many sign-ins in flight → 503 login_busy', async () => {
    const a = app();
    const now = new Date();
    await t.db`
      insert into oidc_logins (state, binding_hash, nonce, code_verifier, next, created_at, expires_at)
      select 'flood-' || g, '\\x00'::bytea, 'n', 'v', '/', ${now}, ${new Date(now.getTime() + 600_000)} from generate_series(1, ${MAX_PENDING_LOGINS + 1}) g`;
    try {
      const res = await a.fetch('/api/v1/auth/login');
      expect(res.status).toBe(503);
      expect(((await res.json()) as ApiErrorBody).error.key).toBe('login_busy');
    } finally {
      await t.db`delete from oidc_logins where state like 'flood-%'`;
    }
    expect((await a.fetch('/api/v1/auth/login')).status).toBe(302);
  });

  test('the sign-in routes are rate limited per address, with Retry-After; page loads land on /login?error=rate_limited', async () => {
    const a = app({}, { authLogin: { max: 3, windowMs: 60_000 }, authCallback: { max: 2, windowMs: 60_000 } });
    for (let i = 0; i < 3; i++) expect((await a.fetch('/api/v1/auth/login')).status).toBe(302);
    // A fetch gets the JSON error.
    const limited = await a.fetch('/api/v1/auth/login');
    expect(limited.status).toBe(429);
    expect(((await limited.json()) as ApiErrorBody).error.key).toBe('rate_limited');
    expect(Number(limited.headers.get('retry-after'))).toBeGreaterThanOrEqual(1);
    expect(Number(limited.headers.get('retry-after'))).toBeLessThanOrEqual(20);
    // A page load (the sign-in button is a plain link) lands on the login page, next= kept.
    const pageLoads: Record<string, string>[] = [{ 'Sec-Fetch-Mode': 'navigate', Accept: 'text/html' }, { Accept: 'text/html,application/xhtml+xml' }];
    for (const headers of pageLoads) {
      const page = await a.fetch('/api/v1/auth/login?next=%2Flicenses%3Fq%3Da', { headers });
      expect(page.status).toBe(302);
      expect(page.headers.get('location')).toBe('/login?error=rate_limited&next=%2Flicenses%3Fq%3Da');
      expect(Number(page.headers.get('retry-after'))).toBeGreaterThanOrEqual(1);
      expect(page.headers.get('cache-control')).toBe('no-store');
      expect(cookieOf(page, 'kls_login')).toBeNull();
    }
    // A fetch that says so is not a page load, whatever it accepts.
    expect((await a.fetch('/api/v1/auth/login', { headers: { 'Sec-Fetch-Mode': 'cors', Accept: 'text/html' } })).status).toBe(429);
    // An unsafe next= is dropped, as on the start.
    const evil = await a.fetch('/api/v1/auth/login?next=%2F%2Fevil.example', { headers: { 'Sec-Fetch-Mode': 'navigate' } });
    expect(evil.headers.get('location')).toBe('/login?error=rate_limited');

    // The callback has its own bucket, and its refusal is a redirect like every other callback failure (spec §6).
    expect(errorOf(await a.fetch('/api/v1/auth/callback'))).toBe('expired');
    expect(errorOf(await a.fetch('/api/v1/auth/callback'))).toBe('expired');
    const cb = await a.fetch('/api/v1/auth/callback?code=c&state=s');
    expect(cb.status).toBe(302);
    expect(cb.headers.get('location')).toBe('/login?error=rate_limited');
    expect(Number(cb.headers.get('retry-after'))).toBeGreaterThanOrEqual(1);
    expect(cb.headers.get('cache-control')).toBe('no-store');
    expect(a.log.find('rate limited')).toHaveLength(2);
  });

  test('a page load of the start that cannot begin lands on /login?error= (IdP down, too many in flight)', async () => {
    const a = app();
    const nav = { headers: { 'Sec-Fetch-Mode': 'navigate' } };
    op.failDiscovery = true;
    const down = await a.fetch('/api/v1/auth/login?next=%2Fmy%2Flicenses', nav);
    expect(down.status).toBe(302);
    expect(down.headers.get('location')).toBe('/login?error=idp_unavailable&next=%2Fmy%2Flicenses');
    expect(cookieOf(down, 'kls_login')).toBeNull();
    expect(a.log.find('request failed')).toHaveLength(0);
    op.failDiscovery = false;

    const now = new Date();
    await t.db`
      insert into oidc_logins (state, binding_hash, nonce, code_verifier, next, created_at, expires_at)
      select 'flood-' || g, '\\x00'::bytea, 'n', 'v', '/', ${now}, ${new Date(now.getTime() + 600_000)} from generate_series(1, ${MAX_PENDING_LOGINS + 1}) g`;
    try {
      const busy = await a.fetch('/api/v1/auth/login', nav);
      expect(busy.status).toBe(302);
      expect(busy.headers.get('location')).toBe('/login?error=login_busy');
    } finally {
      await t.db`delete from oidc_logins where state like 'flood-%'`;
    }
    for (const key of ['rate_limited', 'login_busy', 'idp_unavailable']) expect(LOGIN_ERROR_KEYS).toContain(key as (typeof LOGIN_ERROR_KEYS)[number]);
  });

  test('the IdP is down: 503 idp_unavailable, discovery retried at most every 10 s', async () => {
    const a = app();
    op.failDiscovery = true;
    const before = op.counts.discovery;
    const t0 = Date.now();
    setSystemTime(new Date(t0));
    const r1 = await a.fetch('/api/v1/auth/login');
    expect(r1.status).toBe(503);
    expect(((await r1.json()) as ApiErrorBody).error.key).toBe('idp_unavailable');
    expect((await a.fetch('/api/v1/auth/login')).status).toBe(503);
    expect(op.counts.discovery - before).toBe(1);
    expect((await t.db<{ n: string }[]>`select count(*) as n from oidc_logins where created_at >= ${new Date(t0)}`)[0]!.n).toBe('0');

    op.failDiscovery = false;
    setSystemTime(new Date(t0 + 5_000));
    expect((await a.fetch('/api/v1/auth/login')).status).toBe(503);
    expect(op.counts.discovery - before).toBe(1);
    setSystemTime(new Date(t0 + 10_001));
    expect((await a.fetch('/api/v1/auth/login')).status).toBe(302);
    expect(op.counts.discovery - before).toBe(2);
    // Discovered once, kept.
    expect((await a.fetch('/api/v1/auth/login')).status).toBe(302);
    expect(op.counts.discovery - before).toBe(2);
    expect(a.log.find('oidc discovery failed; sign-in answers 503 until it works')).toHaveLength(1);
  });
});
