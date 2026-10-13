// The OIDC building blocks (design spec §2 "OIDC", §6 "Sign-in", §10):
// SafeNext (the next.go table), the claim helpers (email_verified,
// JSON-text claim rules, groups, names, roles), the http-issuer gate, the
// ID token algorithm rules of discovery (pin, discovery list without none
// and HS*), and a sign-in with an https PUBLIC_URL (__Host- cookies,
// Secure, only the prefixed binding cookie counts) against the fake OP
// posing as an https issuer.
import { afterAll, beforeAll, describe, expect, test } from 'bun:test';
import type { ApiErrorBody } from '../../shared/api';
import { authHandlers } from '../api/auth';
import { adminRulesMatch, claimEquals, cleanName, emailVerifiedClaim, groupsOf, hasDistributedGroups, identityFromClaims, mapIdpRoles } from '../auth/identity';
import { describeError, insecureRequestsAllowed, OidcClient, redirectUri, setOidcClient } from '../auth/oidc';
import { locationOf, safeNext } from '../auth/safeNext';
import type { Config } from '../config';
import { createPipeline } from '../http/pipeline';
import { MemoryLogger } from '../log';
import { makeTestApp, type TestApp } from './call';
import { describeDb, useTestDb } from './db';
import { FakeOp } from './fakeop';
import { testConfig } from './fixtures';

describe('safeNext (ee/oidc/next.go)', () => {
  test.each([
    ['/my/licenses', '/my/licenses'],
    ['/licenses/lic_x?tab=keys#history', '/licenses/lic_x?tab=keys#history'],
    ['/a:b/c', '/a:b/c'],
    ['/search?q=%zz', '/search?q=%zz'],
    ['/caf%C3%A9', '/caf%C3%A9'],
    ['/', '/'],
    ['', '/'],
    ['licenses', '/'],
    ['https://evil.example/', '/'],
    ['//evil.example/x', '/'],
    ['/\\evil.example', '/'],
    ['/a\\b', '/'],
    ['/a\tb', '/'],
    ['/a\nb', '/'],
    ['/a\u007fb', '/'],
    ['/a%zzb', '/'],
    ['/a#%zz', '/'],
    ['/a%2', '/'],
    ['/\ud800', '/'],
    ['/' + 'a'.repeat(2047), '/' + 'a'.repeat(2047)],
    ['/' + 'a'.repeat(2048), '/'],
    ['/' + 'é'.repeat(1024), '/'],
  ])('%j → %j', (input, want) => expect(safeNext(input)).toBe(want));

  test('null and undefined → /', () => {
    expect(safeNext(null)).toBe('/');
    expect(safeNext(undefined)).toBe('/');
  });

  test('locationOf encodes only what a header cannot carry, and never resolves dot segments', () => {
    expect(locationOf('/café?x=1 2')).toBe('/caf%C3%A9?x=1%202');
    expect(locationOf('/.//evil.example')).toBe('/.//evil.example');
    expect(locationOf('//evil.example')).toBe('/');
  });
});

describe('claims', () => {
  test('email_verified: true and "true" only', () => {
    expect([true, 'true', false, 'false', 1, 'TRUE', undefined, null].map(emailVerifiedClaim)).toEqual([true, true, false, false, false, false, false, false]);
  });

  test('claimEquals compares by JSON text', () => {
    expect(claimEquals('example.org', 'example.org')).toBe(true);
    expect(claimEquals('Example.org', 'example.org')).toBe(false);
    expect(claimEquals(true, 'true')).toBe(true);
    expect(claimEquals(false, 'true')).toBe(false);
    expect(claimEquals(42, '42')).toBe(true);
    expect(claimEquals(['example.org'], 'example.org')).toBe(false);
    expect(claimEquals({ a: 1 }, '{"a":1}')).toBe(false);
    expect(claimEquals(undefined, '')).toBe(false);
    expect(adminRulesMatch({ hd: 'example.org', tid: 't1' }, [{ claim: 'hd', value: 'example.org' }, { claim: 'tid', value: 't1' }])).toBe(true);
    expect(adminRulesMatch({ hd: 'example.org' }, [{ claim: 'hd', value: 'example.org' }, { claim: 'tid', value: 't1' }])).toBe(false);
    expect(adminRulesMatch({}, [])).toBe(true);
  });

  test('groups: one claim, string or array of strings', () => {
    expect(groupsOf({ groups: ['a', 2, '', 'b', null] }, 'groups')).toEqual(['a', 'b']);
    expect(groupsOf({ groups: 'a' }, 'groups')).toEqual(['a']);
    expect(groupsOf({ groups: { a: 1 } }, 'groups')).toEqual([]);
    expect(groupsOf({ roles: ['admins'] }, 'groups')).toEqual([]);
    expect(groupsOf({ 'https://example.org/groups': ['x'] }, 'https://example.org/groups')).toEqual(['x']);
    expect(groupsOf({ groups: Array.from({ length: 5000 }, (_, i) => `g${i}`) }, 'groups')).toHaveLength(1000);
    expect(hasDistributedGroups({ _claim_names: { groups: 'src1' } }, 'groups')).toBe(true);
    expect(hasDistributedGroups({ _claim_names: { other: 'src1' } }, 'groups')).toBe(false);
    expect(hasDistributedGroups({}, 'groups')).toBe(false);
  });

  test('names are cleaned and capped without splitting a character', () => {
    expect(cleanName('  Ana\u0000 Lima\u0085 ')).toBe('Ana Lima');
    expect(cleanName(42)).toBe('');
    expect(cleanName('x'.repeat(250))).toHaveLength(200);
    const emoji = '😀'.repeat(150);
    const capped = cleanName(emoji);
    expect(capped.length).toBe(200);
    expect(capped.endsWith('😀')).toBe(true);
  });

  test('identityFromClaims keeps the email only when verified and syntactically an address', () => {
    const now = new Date('2026-10-12T10:00:00Z');
    const base = { iss: 'https://idp.example', sub: 's1' };
    expect(identityFromClaims({ ...base, email: ' Ana@Example.ORG ', email_verified: true }, 'groups', now)).toMatchObject({ email: 'ana@example.org', emailVerified: true, name: 'ana@example.org' });
    expect(identityFromClaims({ ...base, email: 'not an address', email_verified: true }, 'groups', now)).toMatchObject({ email: null, emailVerified: false });
    expect(identityFromClaims({ ...base, email: 'ana@example.org' }, 'groups', now)).toMatchObject({ email: null, name: '' });
    const at = identityFromClaims({ ...base, auth_time: now.getTime() / 1000 - 60 }, 'groups', now).authTime;
    expect(at?.toISOString()).toBe('2026-10-12T09:59:00.000Z');
    expect(identityFromClaims({ ...base, auth_time: now.getTime() / 1000 + 3600 }, 'groups', now).authTime).toBeNull();
    expect(identityFromClaims({ ...base, auth_time: 'yesterday' }, 'groups', now).authTime).toBeNull();
  });

  test('mapIdpRoles: admin groups behind the claim rules, DEFAULT_ROLE when nothing maps', () => {
    const groups = { superadmin: ['sa'], license_admin: ['la'], customer: ['cu'] };
    const ruled = testConfig({ roles: { groups, defaultRole: 'customer', memberAutoLink: false }, oidc: { adminClaimRules: [{ claim: 'hd', value: 'example.org' }] } });
    expect(mapIdpRoles({ groups: ['sa', 'la'] }, { hd: 'example.org' }, ruled)).toEqual({ roles: ['license_admin', 'superadmin'], adminGroupsIgnored: false });
    expect(mapIdpRoles({ groups: ['sa'] }, { hd: 'evil.example' }, ruled)).toEqual({ roles: ['customer'], adminGroupsIgnored: true });
    expect(mapIdpRoles({ groups: ['sa', 'cu'] }, {}, ruled)).toEqual({ roles: ['customer'], adminGroupsIgnored: true });
    expect(mapIdpRoles({ groups: [] }, {}, ruled)).toEqual({ roles: ['customer'], adminGroupsIgnored: false });
    const none = testConfig({ roles: { groups, defaultRole: 'none', memberAutoLink: false } });
    expect(mapIdpRoles({ groups: ['x'] }, {}, none).roles).toEqual([]);
    expect(mapIdpRoles({ groups: ['la'] }, {}, none).roles).toEqual(['license_admin']);
  });
});

describe('the http issuer gate', () => {
  test('loopback literal issuer and http PUBLIC_URL only', () => {
    const http = testConfig();
    const ok = (issuer: string, cfg: Config = http) => insecureRequestsAllowed({ ...cfg, oidc: { ...cfg.oidc, issuer } });
    expect(ok('http://localhost:5556/dex')).toBe(true);
    expect(ok('http://127.0.0.1:5556/dex')).toBe(true);
    expect(ok('http://127.8.9.10/dex')).toBe(true);
    expect(ok('http://[::1]:5556/dex')).toBe(true);
    expect(ok('http://dex.localhost:5556/dex')).toBe(false);
    expect(ok('http://dex:5556/dex')).toBe(false);
    expect(ok('http://10.0.0.1/dex')).toBe(false);
    expect(ok('https://idp.example')).toBe(false);
    expect(ok('http://localhost:5556/dex', testConfig({ publicUrl: 'https://l.example', publicOrigin: 'https://l.example', secureCookies: true }))).toBe(false);
    expect(redirectUri(http)).toBe('http://localhost:8790/api/v1/auth/callback');
  });

  test('describeError keeps codes and messages, never bodies', () => {
    const e = Object.assign(new Error('outer', { cause: new Error('JWT signature verification failed') }), { code: 'OAUTH_X', body: { id_token: 'eyJ...' } });
    expect(describeError(e)).toEqual({ name: 'Error', message: 'outer', code: 'OAUTH_X', detail: 'JWT signature verification failed' });
  });
});

describeDb('discovery and https', () => {
  const t = useTestDb();
  const redirect = 'http://localhost:8790/api/v1/auth/callback';
  const httpsRedirect = 'https://licenses.example.org/api/v1/auth/callback';
  const ops: FakeOp[] = [];
  const apps: TestApp[] = [];
  afterAll(async () => {
    for (const a of apps) await a.close();
    for (const op of ops) op.close();
  });

  async function opWith(opts: Partial<Parameters<typeof FakeOp.start>[0]> = {}): Promise<FakeOp> {
    const op = await FakeOp.start({ redirectUri: redirect, ...opts });
    ops.push(op);
    return op;
  }

  function appFor(op: FakeOp, overrides: Partial<Config> = {}, oidc: Partial<Config['oidc']> = {}): TestApp {
    const base = testConfig(overrides);
    const config: Config = { ...base, oidc: { ...base.oidc, issuer: op.issuer, clientId: op.clientId, clientSecret: op.clientSecret, ...oidc } };
    const log = new MemoryLogger();
    setOidcClient(config, new OidcClient(config, { fetch: op.fetch }));
    const a = makeTestApp({ handlers: authHandlers, db: t.db, config, log, pipeline: createPipeline(config, log) });
    apps.push(a);
    return a;
  }

  async function flow(a: TestApp, op: FakeOp, cookieName = 'kls_login'): Promise<Response> {
    const login = await a.fetch('/api/v1/auth/login');
    if (login.status !== 302) return login;
    const binding = /=([^;]+)/.exec(login.headers.getSetCookie()[0]!)![1]!;
    const back = await op.authorize(login.headers.get('location')!);
    return a.fetch(back.pathname + back.search, { headers: { Cookie: `${cookieName}=${binding}` } });
  }

  test('discovery offering only none and HS256 is refused (503 idp_unavailable)', async () => {
    const op = await opWith({ advertisedAlgs: ['none', 'HS256'] });
    const a = appFor(op);
    const res = await a.fetch('/api/v1/auth/login');
    expect(res.status).toBe(503);
    expect(((await res.json()) as ApiErrorBody).error.key).toBe('idp_unavailable');
    expect(JSON.stringify(a.log.find('oidc discovery failed; sign-in answers 503 until it works'))).toContain('no asymmetric ID token algorithm');
  });

  test('none and HS* are dropped from the discovery list; no list falls back to RS256', async () => {
    const mixed = await opWith({ advertisedAlgs: ['HS256', 'none', 'RS256'] });
    mixed.user = { sub: 'mixed-1', email: 'mixed@example.org', email_verified: true };
    expect((await flow(appFor(mixed), mixed)).headers.get('location')).toBe('/');
    const unlisted = await opWith({ advertisedAlgs: null });
    unlisted.user = { sub: 'unlisted-1' };
    expect((await flow(appFor(unlisted), unlisted)).headers.get('location')).toBe('/');
    // EdDSA without a list: the RS256 fallback refuses it.
    const ed = await opWith({ advertisedAlgs: null, alg: 'EdDSA' });
    ed.user = { sub: 'ed-1' };
    expect((await flow(appFor(ed), ed)).headers.get('location')).toBe('/login?error=failed');
  });

  test('OIDC_ID_TOKEN_ALG pins the algorithm', async () => {
    const ed = await opWith({ alg: 'EdDSA', advertisedAlgs: ['RS256', 'EdDSA'] });
    ed.user = { sub: 'pinned-1' };
    expect((await flow(appFor(ed, {}, { idTokenAlg: 'EdDSA' }), ed)).headers.get('location')).toBe('/');
    const rs = await opWith({ alg: 'RS256', advertisedAlgs: ['RS256', 'EdDSA'] });
    rs.user = { sub: 'pinned-2' };
    const a = appFor(rs, {}, { idTokenAlg: 'EdDSA' });
    expect((await flow(a, rs)).headers.get('location')).toBe('/login?error=failed');
    expect(JSON.stringify(a.log.find('auth.failed'))).toContain('\\"alg\\" header parameter');
  });

  test('https PUBLIC_URL: __Host- cookies, Secure, HSTS, and only the __Host- binding counts', async () => {
    const op = await opWith({ redirectUri: httpsRedirect, publicIssuer: 'https://idp.example.org/dex' });
    op.user = { sub: 'https-1', email: 'secure@example.org', email_verified: true };
    const a = appFor(op, { publicUrl: 'https://licenses.example.org', publicOrigin: 'https://licenses.example.org', secureCookies: true });

    const login = await a.fetch('/api/v1/auth/login?next=/users');
    expect(login.status).toBe(302);
    const loginCookie = login.headers.getSetCookie()[0]!;
    expect(loginCookie).toMatch(/^__Host-kls_login=[A-Za-z0-9_-]{43}; Path=\/; Max-Age=600; Secure; HttpOnly; SameSite=Lax$/);
    expect(new URL(login.headers.get('location')!).searchParams.get('redirect_uri')).toBe(httpsRedirect);
    expect(login.headers.get('strict-transport-security')).toBe('max-age=31536000');
    const binding = /=([^;]+)/.exec(loginCookie)![1]!;
    const back = await op.authorize(login.headers.get('location')!);
    expect(back.origin).toBe('https://licenses.example.org');

    // The unprefixed name (settable by a sibling subdomain) is not read.
    const plain = await a.fetch(back.pathname + back.search, { headers: { Cookie: `kls_login=${binding}` } });
    expect(plain.headers.get('location')).toBe('/login?error=expired');

    const login2 = await a.fetch('/api/v1/auth/login?next=/users');
    const binding2 = /=([^;]+)/.exec(login2.headers.getSetCookie()[0]!)![1]!;
    const back2 = await op.authorize(login2.headers.get('location')!);
    const ok = await a.fetch(back2.pathname + back2.search, { headers: { Cookie: `__Host-kls_login=${binding2}` } });
    expect(ok.headers.get('location')).toBe('/users');
    expect(ok.headers.getSetCookie()[0]).toMatch(/^__Host-kls_session=[A-Za-z0-9_-]{43}; Path=\/; Secure; HttpOnly; SameSite=Lax$/);
    expect(ok.headers.get('strict-transport-security')).toBe('max-age=31536000');
  });
});

describe('fake OP', () => {
  let op: FakeOp;
  beforeAll(async () => {
    op = await FakeOp.start({ redirectUri: 'http://localhost:8790/api/v1/auth/callback' });
  });
  afterAll(() => op.close());

  test('the token endpoint enforces client auth, redirect_uri, PKCE and single use', async () => {
    const verifier = 'v'.repeat(43);
    const challenge = new Bun.CryptoHasher('sha256').update(verifier).digest('base64url');
    const auth = new URL(`${op.issuer}/auth`);
    for (const [k, v] of Object.entries({
      response_type: 'code',
      client_id: op.clientId,
      redirect_uri: 'http://localhost:8790/api/v1/auth/callback',
      scope: 'openid',
      state: 's',
      nonce: 'n',
      code_challenge: challenge,
      code_challenge_method: 'S256',
    }))
      auth.searchParams.set(k, v);
    const code = (await op.authorize(auth.href)).searchParams.get('code')!;
    const token = (body: Record<string, string>) =>
      fetch(`${op.issuer}/token`, { method: 'POST', headers: { 'Content-Type': 'application/x-www-form-urlencoded' }, body: new URLSearchParams(body).toString() });
    const base = { grant_type: 'authorization_code', code, redirect_uri: 'http://localhost:8790/api/v1/auth/callback', client_id: op.clientId, client_secret: op.clientSecret };
    expect((await token({ ...base, client_secret: 'wrong' })).status).toBe(401);
    // A failed client authentication does not burn the code; a wrong verifier does.
    expect((await token({ ...base, code_verifier: 'w'.repeat(43) })).status).toBe(400);
    expect((await token({ ...base, code_verifier: verifier })).status).toBe(400);

    const code2 = (await op.authorize(auth.href)).searchParams.get('code')!;
    const ok = await token({ ...base, code: code2, code_verifier: verifier });
    expect(ok.status).toBe(200);
    expect(((await ok.json()) as { id_token: string }).id_token.split('.')).toHaveLength(3);
    expect((await token({ ...base, code: code2, code_verifier: verifier })).status).toBe(400);
  });
});
