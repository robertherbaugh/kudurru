// The real pipeline (design spec §2 headers, §6 CSRF and CORS, rate
// limits) through the production server: the security headers on every
// kind of response (API, error, redirect, download, static, SPA, 404,
// 405), HSTS only on https, no-store on API answers unless set; the CORS
// matrix of the public form (allowed, foreign and missing Origin, the
// preflight, Expose-Headers Retry-After on errors too, 403 forbidden_origin
// without CORS headers); the CSRF matrix (Sec-Fetch-Site, Origin, the
// token, no session → 401); the client IP behind TRUSTED_PROXIES; and the
// token-bucket limiter.
import { afterAll, beforeAll, describe, expect, test } from 'bun:test';
import type { ApiErrorBody } from '../../shared/api';
import type { Config } from '../config';
import { HttpError, json, keyFile, noContent, redirect } from '../http/errors';
import { CONTENT_SECURITY_POLICY } from '../http/headers';
import { createPipeline } from '../http/pipeline';
import { TokenBuckets } from '../http/ratelimit';
import { staticUiFrom } from '../http/static';
import type { PartialHandlerMap } from '../http/types';
import { MemoryLogger } from '../log';
import { call, makeTestApp, type TestApp } from './call';
import { describeDb, useTestDb } from './db';
import { createUser, signInAs, testConfig, type TestSession } from './fixtures';

const handlers: PartialHandlerMap = {
  healthz: () => Promise.resolve(json({ ok: true })),
  authConfig: () => Promise.resolve(json({ displayName: 'x', devMode: false })),
  authLogin: () => Promise.resolve(redirect('https://idp.example/auth')),
  authMe: (ctx) => Promise.resolve(json({ ip: ctx.ip, peer: ctx.peerIp })),
  catalog: () => Promise.resolve(json({}, { headers: { 'Cache-Control': 'private, max-age=60' } })),
  myLicenseKey: () => Promise.resolve(keyFile('eyJ.test.key')),
  authLogout: () => Promise.resolve(noContent()),
  publicKeyRequest: (ctx) => {
    if (ctx.req.headers.get('x-test') === 'limit') throw new HttpError(429, 'rate_limited', 'Slow down.', { headers: { 'Retry-After': '120' } });
    return Promise.resolve(json({ status: 'sent' }, { status: 202 }));
  },
  publicKeyRequestVerify: () => Promise.resolve(json({ outcome: 'issued' })),
};

const ui = staticUiFrom(
  '/virtual',
  new Map([
    ['/index.html', new TextEncoder().encode('<!doctype html><title>portal</title>')],
    ['/assets/app-abc123.js', new TextEncoder().encode('console.log(1)')],
  ]),
);

function makeApp(config: Config, db?: TestApp['deps']['db']): TestApp {
  const log = new MemoryLogger();
  return makeTestApp({ handlers, config, log, db, pipeline: createPipeline(config, log), extraRoutes: ui.routes, fallback: ui.fallback });
}

const SECURITY = {
  'content-security-policy': CONTENT_SECURITY_POLICY,
  'x-content-type-options': 'nosniff',
  'referrer-policy': 'same-origin',
  'x-frame-options': 'DENY',
  'cross-origin-opener-policy': 'same-origin',
};

function expectSecurityHeaders(res: Response, https: boolean): void {
  for (const [k, v] of Object.entries(SECURITY)) expect(res.headers.get(k)).toBe(v);
  expect(res.headers.get('strict-transport-security')).toBe(https ? 'max-age=31536000' : null);
  expect(res.headers.get('x-request-id')).toMatch(/^[0-9a-f-]{36}$/);
}

describe('security headers', () => {
  const https = testConfig({ publicUrl: 'https://licenses.example.org', publicOrigin: 'https://licenses.example.org', secureCookies: true });
  let app: TestApp;
  let httpApp: TestApp;
  beforeAll(() => {
    app = makeApp(https);
    httpApp = makeApp(testConfig());
  });
  afterAll(async () => {
    await app.close();
    await httpApp.close();
  });

  test('CSP is exactly web.DefaultCSP', () => {
    expect(CONTENT_SECURITY_POLICY).toBe(
      "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; worker-src 'self'; manifest-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'",
    );
  });

  test('on every kind of response; HSTS only on https; no-store on the API unless the handler set its own', async () => {
    const cases: [string, RequestInit | undefined, number, string | null][] = [
      ['/healthz', undefined, 200, 'no-store'],
      ['/api/v1/auth/config', undefined, 200, 'no-store'],
      ['/api/v1/catalog', undefined, 401, 'no-store'],
      ['/api/v1/auth/login', undefined, 302, 'no-store'],
      ['/api/v1/nope', undefined, 404, 'no-store'],
      ['/api/v1/auth/config', { method: 'DELETE' }, 405, 'no-store'],
      ['/', undefined, 200, 'no-cache'],
      ['/licenses/lic_x', undefined, 200, 'no-cache'],
      ['/assets/app-abc123.js', undefined, 200, 'public, max-age=31536000, immutable'],
      ['/assets/missing.js', undefined, 404, 'no-store'],
      ['/', { method: 'POST' }, 405, 'no-store'],
    ];
    for (const [path, init, status, cache] of cases) {
      for (const [a, isHttps] of [
        [app, true],
        [httpApp, false],
      ] as const) {
        const res = await a.fetch(path, init);
        expect(`${path} ${res.status}`).toBe(`${path} ${status}`);
        expectSecurityHeaders(res, isHttps);
        expect(`${path} ${res.headers.get('cache-control')}`).toBe(`${path} ${cache}`);
      }
    }
  });

  test('a handler’s own Cache-Control wins; downloads are no-store', async () => {
    const config = testConfig();
    // catalog needs a session; check the headers step directly with a route context.
    const { headersStep } = await import('../http/headers');
    const step = headersStep(config);
    const url = new URL('http://localhost/api/v1/catalog');
    const own = step(new Response('{}', { headers: { 'Cache-Control': 'private, max-age=60' } }), { req: new Request(url.href), url, config, route: null, requestId: 'r' });
    expect(own.headers.get('cache-control')).toBe('private, max-age=60');
    const immutable = step(Response.redirect('http://localhost/x', 302), { req: new Request(url.href), url, config, route: null, requestId: 'r' });
    expect(immutable.headers.get('cache-control')).toBe('no-store');
    expect(immutable.headers.get('content-security-policy')).toBe(CONTENT_SECURITY_POLICY);
    expect(keyFile('t').headers.get('cache-control')).toBe('no-store');
  });
});

describe('CORS on the public form', () => {
  let app: TestApp;
  let off: TestApp;
  const config = testConfig({ publicForm: { enabled: true, origins: ['https://kudurru.io', 'http://localhost:8792'], limitPerIpPerHour: 5, limitPerEmailPerDay: 3 } });
  beforeAll(() => {
    app = makeApp(config);
    off = makeApp(testConfig({ publicForm: { enabled: false, origins: [], limitPerIpPerHour: 5, limitPerEmailPerDay: 3 } }));
  });
  afterAll(async () => {
    await app.close();
    await off.close();
  });

  const post = (origin: string | null, extra: Record<string, string> = {}) =>
    call(app, 'publicKeyRequest', { body: { name: 'A' }, headers: { ...(origin ? { Origin: origin } : {}), ...extra } });

  test('an allowed Origin is echoed, never with credentials; Retry-After is exposed', async () => {
    const res = await post('https://kudurru.io');
    expect(res.status).toBe(202);
    expect(res.headers.get('access-control-allow-origin')).toBe('https://kudurru.io');
    expect(res.headers.get('access-control-expose-headers')).toBe('Retry-After');
    expect(res.headers.get('access-control-allow-credentials')).toBeNull();
    expect(res.headers.get('vary')).toBe('Origin');
    expect(res.headers.get('access-control-allow-methods')).toBeNull();
    expectSecurityHeaders(res, false);
    expect((await post('http://localhost:8792')).headers.get('access-control-allow-origin')).toBe('http://localhost:8792');
  });

  test('errors of the route carry the CORS headers too (the 429 with Retry-After, a 400 for a bad body)', async () => {
    const limited = await post('https://kudurru.io', { 'X-Test': 'limit' });
    expect(limited.status).toBe(429);
    expect(limited.headers.get('retry-after')).toBe('120');
    expect(limited.headers.get('access-control-allow-origin')).toBe('https://kudurru.io');
    expect(limited.headers.get('access-control-expose-headers')).toBe('Retry-After');
    const bad = await call(app, 'publicKeyRequest', { rawBody: 'name=A', headers: { Origin: 'https://kudurru.io', 'Content-Type': 'application/x-www-form-urlencoded' } });
    expect(bad.status).toBe(400);
    expect(bad.headers.get('access-control-allow-origin')).toBe('https://kudurru.io');
    expect(bad.headers.get('access-control-expose-headers')).toBe('Retry-After');
  });

  test('a foreign Origin: 403 forbidden_origin without CORS headers', async () => {
    for (const origin of ['https://evil.example', 'null', 'https://kudurru.io.evil.example', 'https://KUDURRU.io', 'http://kudurru.io']) {
      const res = await post(origin);
      expect(res.status).toBe(403);
      expect(((await res.json()) as ApiErrorBody).error.key).toBe('forbidden_origin');
      for (const h of ['access-control-allow-origin', 'access-control-expose-headers', 'access-control-allow-methods']) expect(res.headers.get(h)).toBeNull();
      expectSecurityHeaders(res, false);
    }
    const pre = await app.fetch('/api/v1/public/key-requests', { method: 'OPTIONS', headers: { Origin: 'https://evil.example', 'Access-Control-Request-Method': 'POST' } });
    expect(pre.status).toBe(403);
    expect(pre.headers.get('access-control-allow-origin')).toBeNull();
  });

  test('no Origin (curl, a server): served, without Allow-Origin', async () => {
    const res = await post(null);
    expect(res.status).toBe(202);
    expect(res.headers.get('access-control-allow-origin')).toBeNull();
    expect(res.headers.get('access-control-expose-headers')).toBe('Retry-After');
  });

  test('the preflight: 204 with methods, headers and max-age', async () => {
    const res = await app.fetch('/api/v1/public/key-requests', {
      method: 'OPTIONS',
      headers: { Origin: 'https://kudurru.io', 'Access-Control-Request-Method': 'POST', 'Access-Control-Request-Headers': 'content-type' },
    });
    expect(res.status).toBe(204);
    expect(res.headers.get('access-control-allow-origin')).toBe('https://kudurru.io');
    expect(res.headers.get('access-control-allow-methods')).toBe('POST, OPTIONS');
    expect(res.headers.get('access-control-allow-headers')).toBe('Content-Type, Accept');
    expect(res.headers.get('access-control-max-age')).toBe('600');
    expect(res.headers.get('access-control-allow-credentials')).toBeNull();
    expect(res.headers.get('vary')).toBe('Origin');
  });

  test('the verify route has no CORS; with PUBLIC_FORM_ORIGINS=- the public routes are 404', async () => {
    const res = await call(app, 'publicKeyRequestVerify', { body: { token: 'x' }, headers: { Origin: 'https://kudurru.io' } });
    expect(res.status).toBe(200);
    expect(res.headers.get('access-control-allow-origin')).toBeNull();
    const gone = await call(off, 'publicKeyRequest', { body: { name: 'A' }, headers: { Origin: 'https://kudurru.io' } });
    expect(gone.status).toBe(404);
  });
});

describe('client IP behind TRUSTED_PROXIES', () => {
  test('the peer is trusted: the rightmost untrusted X-Forwarded-For entry', async () => {
    const config = testConfig({ trustedProxies: ['127.0.0.0/8'] });
    const log = new MemoryLogger();
    const app = makeTestApp({
      handlers: { authConfig: (ctx) => Promise.resolve(json({ ip: ctx.ip, peer: ctx.peerIp })) },
      config,
      log,
      pipeline: createPipeline(config, log),
    });
    try {
      const res = await app.fetch('/api/v1/auth/config', { headers: { 'X-Forwarded-For': '6.6.6.6, 203.0.113.9' } });
      expect(await res.json()).toEqual({ ip: '203.0.113.9', peer: '127.0.0.1' });
    } finally {
      await app.close();
    }
    const untrusted = testConfig();
    const app2 = makeTestApp({ handlers: { authConfig: (ctx) => Promise.resolve(json({ ip: ctx.ip })) }, config: untrusted, log, pipeline: createPipeline(untrusted, log) });
    try {
      const res = await app2.fetch('/api/v1/auth/config', { headers: { 'X-Forwarded-For': '203.0.113.9' } });
      expect(await res.json()).toEqual({ ip: '127.0.0.1' });
    } finally {
      await app2.close();
    }
  });
});

describeDb('CSRF', () => {
  const t = useTestDb();
  let app: TestApp;
  let session: TestSession;
  const config = testConfig();
  beforeAll(async () => {
    app = makeApp(config, t.db);
    session = await signInAs(t.db, await createUser(t.db, { roles: ['customer'] }), config);
  });
  afterAll(() => app.close());

  const logout = (headers: Record<string, string>, as: TestSession | null = session) =>
    call(app, 'authLogout', { as: null, headers: { ...(as ? { Cookie: as.cookie } : {}), ...headers } });

  test.each([
    ['token, no fetch metadata (old browser or script)', { token: true }, 204],
    ['token + Sec-Fetch-Site same-origin', { token: true, 'Sec-Fetch-Site': 'same-origin' }, 204],
    ['token + Sec-Fetch-Site none', { token: true, 'Sec-Fetch-Site': 'none' }, 204],
    ['token + Origin = PUBLIC_URL', { token: true, Origin: 'http://localhost:8790' }, 204],
    ['token + Sec-Fetch-Site same-site', { token: true, 'Sec-Fetch-Site': 'same-site' }, 403],
    ['token + Sec-Fetch-Site cross-site', { token: true, 'Sec-Fetch-Site': 'cross-site' }, 403],
    ['token + Sec-Fetch-Site same-origin wins over a foreign Origin', { token: true, 'Sec-Fetch-Site': 'same-origin', Origin: 'https://evil.example' }, 204],
    ['token + foreign Origin', { token: true, Origin: 'https://evil.example' }, 403],
    ['token + Origin null', { token: true, Origin: 'null' }, 403],
    ['token + Origin with another port', { token: true, Origin: 'http://localhost:8791' }, 403],
    ['no token, same-origin', { 'Sec-Fetch-Site': 'same-origin' }, 403],
    ['wrong token', { wrong: true }, 403],
    ['token of another length', { short: true }, 403],
  ] as const)('%s → %i', async (_label, h, status) => {
    const headers: Record<string, string> = {};
    for (const [k, v] of Object.entries(h)) if (typeof v === 'string') headers[k] = v;
    if ('token' in h) headers['X-Kudurru-CSRF'] = session.csrf;
    if ('wrong' in h) headers['X-Kudurru-CSRF'] = session.csrf.replace(/^./, (c) => (c === 'A' ? 'B' : 'A'));
    if ('short' in h) headers['X-Kudurru-CSRF'] = session.csrf.slice(1);
    const res = await logout(headers);
    expect(res.status).toBe(status);
    if (status === 403) expect(((await res.json()) as ApiErrorBody).error.key).toBe('csrf');
  });

  test('no session: 401, not 403; GET needs no token', async () => {
    const res = await logout({ 'Sec-Fetch-Site': 'cross-site' }, null);
    expect(res.status).toBe(401);
    const me = await app.fetch('/api/v1/auth/me', { headers: { Cookie: session.cookie, 'Sec-Fetch-Site': 'cross-site' } });
    expect(me.status).toBe(200);
    expect(app.log.find('csrf.denied').length).toBeGreaterThan(0);
    expect(app.log.text()).not.toContain(session.csrf);
  });
});

describe('TokenBuckets', () => {
  test('a burst of max, then one per window/max; Retry-After rounds up; one refusal logged per burst', () => {
    const b = new TokenBuckets({ max: 20, windowMs: 60_000 });
    for (let i = 0; i < 20; i++) expect(b.take('k', 0).ok).toBe(true);
    const r1 = b.take('k', 0);
    expect(r1).toEqual({ ok: false, retryAfter: 3, firstRefusal: true });
    expect(b.take('k', 1000)).toEqual({ ok: false, retryAfter: 2, firstRefusal: false });
    expect(b.take('k', 3000).ok).toBe(true);
    expect(b.take('k', 3000).ok).toBe(false);
    expect(b.take('other', 3000).ok).toBe(true);
    // A minute later the bucket is full again.
    for (let i = 0; i < 20; i++) expect(b.take('k', 63_000).ok).toBe(true);
    expect(b.take('k', 63_000).ok).toBe(false);
  });

  test('memory stays bounded: full buckets are dropped first, then the least recently used', () => {
    const b = new TokenBuckets({ max: 2, windowMs: 1000 }, 3);
    b.take('a', 0);
    b.take('b', 0);
    b.take('c', 0);
    expect(b.size).toBe(3);
    b.take('d', 5000);
    expect(b.size).toBe(1);
    b.take('e', 5000);
    b.take('f', 5000);
    b.take('g', 5000);
    expect(b.size).toBe(3);
    // e is used again (its last token), so f is now the least recently used and goes first.
    expect(b.take('e', 5000).ok).toBe(true);
    b.take('h', 5000);
    expect(b.size).toBe(3);
    expect(b.take('e', 5000).ok).toBe(false);
  });

  test('at the cap the sweep frees a tenth, so it does not run on every new address', () => {
    const b = new TokenBuckets({ max: 5, windowMs: 60_000 }, 100);
    for (let i = 0; i < 100; i++) b.take(`k${i}`, 0);
    expect(b.size).toBe(100);
    b.take('new', 0);
    expect(b.size).toBe(91);
    for (let i = 0; i < 9; i++) b.take(`more${i}`, 0);
    expect(b.size).toBe(100);
  });
});
