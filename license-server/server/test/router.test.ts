// The router and its pipeline (design spec §8, §10 "http"), with a tiny
// fake handler map: JSON 404 and 405 (with Allow), 401 and 403 (with the
// missing permission), 415 (JSON only, also for body-less routes), 413
// (chunked bodies and bodies above 64 KiB included) and the public form's
// 400-for-bad-bodies, headers and request ids on static files, the SPA
// fallback and the error hook,
// the error handler that hides messages behind the request id, Postgres
// errors mapped to 409, session resolution with signInAs (and its expiry),
// the order of the injected pipeline steps, and the handler-map checks.
import { afterAll, beforeAll, describe, expect, test } from 'bun:test';
import type { ApiErrorBody } from '../../shared/api';
import { buildPath, ROUTE_LIST } from '../../shared/routes';
import { healthHandlers } from '../api/health';
import { HttpError, json } from '../http/errors';
import { readJson, v } from '../http/body';
import { assertHandlers, mergeHandlers } from '../http/router';
import type { PartialHandlerMap, Pipeline } from '../http/types';
import { call, callJson, makeTestApp, type TestApp } from './call';
import { describeDb, useTestDb } from './db';
import { createUser, signInAs, testConfig, type TestSession } from './fixtures';

const handlers: PartialHandlerMap = {
  healthz: healthHandlers.healthz,
  readyz: healthHandlers.readyz,
  authMe: (ctx) => Promise.resolve(json({ userId: ctx.principal?.userId, roles: ctx.principal?.roles, csrf: ctx.session?.csrf })),
  licensesList: () => Promise.resolve(json({ items: [], total: 0 })),
  licenseGet: (ctx) => Promise.resolve(json({ id: ctx.params.id })),
  memberRemove: (ctx) => Promise.resolve(json({ id: ctx.params.id, email: ctx.params.email })),
  customerCreate: async (ctx) => {
    const body = await readJson(ctx, v.object({ name: v.string({ min: 1, max: 200, trim: true }), kind: v.enum(['person', 'organization']) }));
    return json(body, { status: 201 });
  },
  customerGet: async (ctx) => {
    // The same primary key twice: a unique violation that reaches the router.
    await ctx.db`insert into blocked_emails (email) values ('dup@example.org') on conflict do nothing`;
    await ctx.db`insert into blocked_emails (email) values ('dup@example.org')`;
    return json({});
  },
  system: () => Promise.reject(new Error('the database password is hunter2')),
  publicKeyRequest: async (ctx) => {
    await readJson(ctx, v.object({ name: v.string({ min: 1, max: 200 }) }));
    return json({ status: 'sent' }, { status: 202 });
  },
  publicKeyRequestVerify: async (ctx) => {
    const body = await readJson(ctx, v.object({ token: v.string({ min: 1, max: 200 }) }));
    return json({ outcome: body.token === 'ok' ? 'issued' : 'pending_review' });
  },
  userUpdate: () => Promise.reject(new HttpError(409, 'conflict', 'The last superadmin keeps the role.', { hint: 'Grant superadmin to someone else first.' })),
};

const REQUEST_ID = /^[0-9a-f-]{36}$/;
const JSON_HEADERS = { 'Content-Type': 'application/json' };

/** A body sent with Transfer-Encoding: chunked (no Content-Length for the router to check). */
function chunked(text: string, chunkSize = 1000): ReadableStream<Uint8Array> {
  const bytes = new TextEncoder().encode(text);
  let at = 0;
  return new ReadableStream({
    pull(c) {
      if (at >= bytes.length) return c.close();
      c.enqueue(bytes.slice(at, at + chunkSize));
      at += chunkSize;
    },
  });
}

async function errorOf(res: Response): Promise<ApiErrorBody['error']> {
  expect(res.headers.get('content-type')).toBe('application/json; charset=utf-8');
  return ((await res.json()) as ApiErrorBody).error;
}

describe('without a database', () => {
  let app: TestApp;
  beforeAll(() => {
    app = makeTestApp({ handlers });
  });
  afterAll(() => app.close());

  test('unknown /api paths answer a JSON 404 with a request id', async () => {
    for (const path of ['/api/v1/nope', '/api/v2/licenses', '/api']) {
      const res = await app.fetch(path);
      expect(res.status).toBe(404);
      expect((await errorOf(res)).key).toBe('not_found');
      expect(res.headers.get('x-request-id')).toMatch(REQUEST_ID);
    }
  });

  test('other methods answer a JSON 405 with Allow', async () => {
    const res = await app.fetch('/api/v1/licenses', { method: 'PUT' });
    expect(res.status).toBe(405);
    expect(res.headers.get('allow')).toBe('GET, HEAD, POST');
    expect((await errorOf(res)).key).toBe('method_not_allowed');
    // The static path wins over /licenses/:id: GET /licenses/import is not a license.
    const imp = await app.fetch('/api/v1/licenses/import');
    expect(imp.status).toBe(405);
    expect(imp.headers.get('allow')).toBe('POST');
    // OPTIONS only where a preflight is declared.
    expect((await app.fetch('/api/v1/licenses', { method: 'OPTIONS' })).status).toBe(405);
  });

  test('HEAD is answered from GET', async () => {
    const res = await app.fetch('/healthz', { method: 'HEAD' });
    expect(res.status).toBe(200);
    expect(await res.text()).toBe('');
  });

  test('a session route without a session answers 401', async () => {
    const res = await call(app, 'authMe');
    expect(res.status).toBe(401);
    expect((await errorOf(res)).key).toBe('unauthenticated');
  });

  test('an unexpected error is logged and answered without its message', async () => {
    // system needs system.read: skip authorization with a pipeline that signs everyone in as a superadmin.
    const admin = makeTestApp({
      handlers,
      pipeline: {
        session: () =>
          Promise.resolve({
            principal: { userId: 'usr_x', email: null, emailVerified: false, name: 'x', roles: ['superadmin'], permissions: new Set(), can: () => true },
            session: { idHash: Buffer.alloc(32), userId: 'usr_x', csrf: 'c', authTime: new Date(), createdAt: new Date(), lastSeenAt: new Date(), idleExpiresAt: new Date(), expiresAt: new Date() },
          }),
      },
    });
    try {
      const res = await call(admin, 'system');
      expect(res.status).toBe(500);
      const text = await res.text();
      expect(text).not.toContain('hunter2');
      const body = JSON.parse(text) as ApiErrorBody;
      expect(body.error).toEqual({ key: 'internal', message: 'Internal error.', hint: res.headers.get('x-request-id')! });
      const logged = admin.log.find('internal error');
      expect(logged).toHaveLength(1);
      expect(logged[0]!.requestId).toBe(body.error.hint as string);
      expect(JSON.stringify(logged[0])).toContain('hunter2');
      // An HttpError keeps its key, message and hint.
      const conflict = await callJson(admin, 'userUpdate', { params: { id: 'usr_x' }, body: {} });
      expect(conflict.status).toBe(409);
      expect(conflict.error).toEqual({ key: 'conflict', message: 'The last superadmin keeps the role.', hint: 'Grant superadmin to someone else first.' });
    } finally {
      await admin.close();
    }
  });

  test('bodies: 415 for other content types, 413 above the route limit, 400 for bad JSON', async () => {
    const asText = await call(app, 'publicKeyRequestVerify', { rawBody: '{"token":"ok"}', headers: { 'Content-Type': 'text/plain' } });
    expect(asText.status).toBe(415);
    expect((await errorOf(asText)).key).toBe('unsupported_media_type');
    const big = await call(app, 'publicKeyRequestVerify', { body: { token: 'x'.repeat(9000) } });
    expect(big.status).toBe(413);
    expect((await errorOf(big)).key).toBe('payload_too_large');
    const broken = await call(app, 'publicKeyRequestVerify', { rawBody: '{"token":', headers: { 'Content-Type': 'application/json' } });
    expect(broken.status).toBe(400);
    expect((await errorOf(broken)).key).toBe('invalid');
    const ok = await callJson(app, 'publicKeyRequestVerify', { body: { token: 'ok', extra: 1 } });
    expect(ok.status).toBe(200);
    expect(ok.body).toEqual({ outcome: 'issued' });
  });

  // The frozen public form contract: every bad body is a 400 invalid.
  test('the public form answers 400 invalid for bad bodies, and 204 to a preflight', async () => {
    const asText = await call(app, 'publicKeyRequest', { rawBody: '{}', headers: { 'Content-Type': 'text/plain' } });
    expect(asText.status).toBe(400);
    expect((await errorOf(asText)).key).toBe('invalid');
    const big = await call(app, 'publicKeyRequest', { body: { name: 'x'.repeat(9000) } });
    expect(big.status).toBe(400);
    const missing = await callJson(app, 'publicKeyRequest', { body: { name: '' } });
    expect(missing.status).toBe(400);
    expect(missing.error?.fields).toEqual(['name']);
    expect((await call(app, 'publicKeyRequest', { body: { name: 'Hank' } })).status).toBe(202);
    expect((await call(app, 'publicKeyRequest', { method: 'OPTIONS' })).status).toBe(204);
  });

  test('JSON only: an unsafe request with a body must say application/json, checked before the session', async () => {
    const form = await call(app, 'authLogout', { rawBody: 'a=1', headers: { 'Content-Type': 'text/plain' } });
    expect(form.status).toBe(415);
    expect((await errorOf(form)).key).toBe('unsupported_media_type');
    const urlencoded = await call(app, 'memberInvite', { params: { id: 'cus_x', email: 'a@example.org' }, rawBody: 'a=1', headers: { 'Content-Type': 'application/x-www-form-urlencoded' } });
    expect(urlencoded.status).toBe(415);
    const streamed = await app.fetch(buildPath('authLogout'), { method: 'POST', headers: { 'Content-Type': 'text/plain' }, body: chunked('a=1') });
    expect(streamed.status).toBe(415);
    // Without a body any content type passes on (here to the 401 of a missing session).
    expect((await call(app, 'authLogout', { headers: { 'Content-Type': 'text/plain' } })).status).toBe(401);
    expect((await call(app, 'authLogout', { body: {} })).status).toBe(401);
    // The public form answers 400 invalid instead (frozen contract).
    const pub = await call(app, 'publicKeyRequest', { rawBody: 'name=Hank', headers: { 'Content-Type': 'application/x-www-form-urlencoded' } });
    expect(pub.status).toBe(400);
    expect((await errorOf(pub)).key).toBe('invalid');
  });

  test('oversize bodies get the JSON answer, chunked or not, and log no error', async () => {
    const errors = () => app.log.entries.filter((e) => e.level === 'error').length;
    const before = errors();
    // Chunked: readJson stops reading at the route's limit.
    const big = await app.fetch(buildPath('publicKeyRequestVerify'), { method: 'POST', headers: JSON_HEADERS, body: chunked(JSON.stringify({ token: 'x'.repeat(100_000) })) });
    expect(big.status).toBe(413);
    expect((await errorOf(big)).key).toBe('payload_too_large');
    expect(big.headers.get('x-request-id')).toMatch(REQUEST_ID);
    const pub = await app.fetch(buildPath('publicKeyRequest'), { method: 'POST', headers: JSON_HEADERS, body: chunked(JSON.stringify({ name: 'x'.repeat(100_000) })) });
    expect(pub.status).toBe(400);
    expect((await errorOf(pub)).key).toBe('invalid');
    // Above DEFAULT_BODY_LIMIT with a Content-Length: still the router's JSON 413 (Bun's cap is MAX_REQUEST_BODY).
    const large = await call(app, 'authLogout', { rawBody: 'x'.repeat(200_000), headers: JSON_HEADERS });
    expect(large.status).toBe(413);
    expect((await errorOf(large)).key).toBe('payload_too_large');
    const largePub = await call(app, 'publicKeyRequest', { rawBody: 'x'.repeat(200_000), headers: JSON_HEADERS });
    expect(largePub.status).toBe(400);
    // A chunked body within the limit is read in full.
    const ok = await app.fetch(buildPath('publicKeyRequestVerify'), { method: 'POST', headers: JSON_HEADERS, body: chunked(JSON.stringify({ token: 'ok', pad: 'y'.repeat(5000) }), 700) });
    expect(ok.status).toBe(200);
    expect(await ok.json()).toEqual({ outcome: 'issued' });
    expect(errors()).toBe(before);
  });

  test('readyz names why signing is unavailable', async () => {
    const res = await callJson(app, 'readyz');
    // No database in this app.
    expect(res.status).toBe(503);
    expect(res.body).toEqual({ ok: false, db: 'error', migrations: 'error', signing: 'unavailable: no signing key in this test app' });
  });

  test('static routes, the SPA fallback and the error hook get the headers and a request id', async () => {
    const pipeline: Partial<Pipeline> = {
      headers: (res, h) => {
        if (h.url.pathname === '/boom-headers') throw new Error('the headers step failed');
        res.headers.set('X-Test-Headers', h.route ? h.route.id : 'none');
        return res;
      },
    };
    const site = makeTestApp({
      handlers,
      pipeline,
      extraRoutes: {
        '/assets/app.js': () => new Response('js', { headers: { 'Cache-Control': 'public, max-age=31536000, immutable' } }),
        '/assets/broken.js': {
          GET: () => {
            throw new Error('the asset table is gone');
          },
        },
        '/boom-headers': () => new Response('x'),
        // The API routes win on the same path.
        '/healthz': () => new Response('static healthz'),
      },
      fallback: (req) => (new URL(req.url).pathname === '/throws' ? Promise.reject(new Error('the fallback failed')) : new Response('<!doctype html>', { headers: { 'Content-Type': 'text/html' } })),
    });
    try {
      const asset = await site.fetch('/assets/app.js');
      expect(await asset.text()).toBe('js');
      expect(asset.headers.get('x-test-headers')).toBe('none');
      expect(asset.headers.get('x-request-id')).toMatch(REQUEST_ID);
      expect(asset.headers.get('cache-control')).toBe('public, max-age=31536000, immutable');
      const page = await site.fetch('/licenses/lic_x');
      expect(await page.text()).toBe('<!doctype html>');
      expect(page.headers.get('x-test-headers')).toBe('none');
      expect(page.headers.get('x-request-id')).toMatch(REQUEST_ID);
      const health = await site.fetch('/healthz');
      expect(await health.json()).toEqual({ ok: true });
      expect(health.headers.get('x-test-headers')).toBe('healthz');
      for (const path of ['/assets/broken.js', '/throws']) {
        const res = await site.fetch(path);
        expect(res.status).toBe(500);
        const err = await errorOf(res);
        expect(err).toEqual({ key: 'internal', message: 'Internal error.', hint: res.headers.get('x-request-id')! });
        expect(res.headers.get('x-test-headers')).toBe('none');
      }
      expect(site.log.find('unhandled error').map((e) => e.path)).toEqual(['/assets/broken.js', '/throws']);
      // A failure in the headers step itself reaches Bun's error hook: still a JSON 500 with a request id and the headers.
      const hook = await site.fetch('/boom-headers');
      expect(hook.status).toBe(500);
      const err = await errorOf(hook);
      expect(err.key).toBe('internal');
      expect(err.hint).toBe(hook.headers.get('x-request-id')!);
      expect(hook.headers.get('x-test-headers')).toBe('none');
    } finally {
      await site.close();
    }
  });

  test('PUBLIC_FORM_ORIGINS=- takes the public routes away', async () => {
    const closed = makeTestApp({ handlers, config: testConfig({ publicForm: { enabled: false, origins: [] } }) });
    try {
      const res = await call(closed, 'publicKeyRequest', { body: { name: 'Hank' } });
      expect(res.status).toBe(404);
      expect((await call(closed, 'publicKeyRequest', { method: 'OPTIONS' })).status).toBe(404);
    } finally {
      await closed.close();
    }
  });

  test('a route without a handler answers 501 after authorization', async () => {
    expect((await call(app, 'catalog')).status).toBe(401);
    expect((await call(app, 'authConfig')).status).toBe(501);
  });
});

describeDb('with a database', () => {
  const t = useTestDb();
  let app: TestApp;
  let customer: TestSession;
  let admin: TestSession;
  let adminId: string;

  beforeAll(async () => {
    app = makeTestApp({ handlers, db: t.db });
    customer = await signInAs(t.db, await createUser(t.db, { roles: ['customer'] }));
    const a = await createUser(t.db, { roles: ['license_admin'] });
    adminId = a.id;
    admin = await signInAs(t.db, a);
  });
  afterAll(() => app.close());

  test('signInAs gives a session the router resolves', async () => {
    // The fake authMe answers {userId, roles, csrf}, not Me.
    const me = await call(app, 'authMe', { as: admin });
    expect(me.status).toBe(200);
    expect(await me.json()).toEqual({ userId: adminId, roles: ['license_admin'], csrf: admin.csrf });
  });

  test('a missing permission answers 403 with the permission as hint, and is logged', async () => {
    const res = await call(app, 'licensesList', { as: customer });
    expect(res.status).toBe(403);
    expect(await errorOf(res)).toEqual({ key: 'forbidden', message: 'You do not have permission to do this.', hint: { missing: 'licenses.read' } });
    expect(app.log.find('authz.denied').at(-1)).toMatchObject({ missing: 'licenses.read', route: 'licensesList' });
    expect((await call(app, 'licensesList', { as: admin })).status).toBe(200);
  });

  test('a disabled user and an idle session are signed out', async () => {
    const disabled = await signInAs(t.db, await createUser(t.db, { roles: ['superadmin'], disabled: true }));
    expect((await call(app, 'authMe', { as: disabled })).status).toBe(401);
    // Signed in 31 minutes ago and idle since: past SESSION_IDLE_TIMEOUT (30m).
    const idle = await signInAs(t.db, await createUser(t.db), testConfig(), new Date(Date.now() - 31 * 60_000));
    expect((await call(app, 'authMe', { as: idle })).status).toBe(401);
    expect((await call(app, 'authMe', { as: { ...admin, cookie: 'kls_session=forged' } })).status).toBe(401);
  });

  test('field errors list every failing field', async () => {
    const res = await callJson(app, 'customerCreate', { as: admin, body: { name: ' \u00a0 ', kind: 'company' } });
    expect(res.status).toBe(400);
    expect(res.error?.key).toBe('invalid');
    expect(res.error?.fields).toEqual(['name', 'kind']);
    const ok = await call(app, 'customerCreate', { as: admin, body: { name: '  Example GmbH ', kind: 'organization', ignored: true } });
    expect(ok.status).toBe(201);
    expect(await ok.json()).toEqual({ name: 'Example GmbH', kind: 'organization' });
  });

  test('path parameters arrive decoded', async () => {
    const res = await call(app, 'memberRemove', { as: admin, params: { id: 'cus_x', email: 'a+b@example.org' } });
    expect(await res.json()).toEqual({ id: 'cus_x', email: 'a+b@example.org' });
  });

  test('a unique violation that reaches the router is a 409 conflict without detail', async () => {
    const res = await call(app, 'customerGet', { as: admin, params: { id: 'cus_x' } });
    expect(res.status).toBe(409);
    const error = await errorOf(res);
    expect(error.key).toBe('conflict');
    expect(JSON.stringify(error)).not.toContain('blocked_emails');
  });

  test('the pipeline runs in order: session, CSRF (unsafe methods only), rate limit, authorize', async () => {
    const seen: string[] = [];
    const pipeline: Partial<Pipeline> = {
      headers: (res) => {
        seen.push('headers');
        res.headers.set('X-Test-Headers', '1');
        return res;
      },
      csrf: (ctx, next) => {
        seen.push(`csrf:${ctx.session ? 'session' : 'none'}`);
        if (ctx.req.headers.get('x-kudurru-csrf') !== ctx.session?.csrf) throw new HttpError(403, 'csrf', 'The request was not sent by the portal.');
        return next();
      },
      rateLimit: (_ctx, next) => {
        seen.push('rateLimit');
        return next();
      },
    };
    const strict = makeTestApp({ handlers, db: t.db, pipeline });
    try {
      const get = await call(strict, 'licensesList', { as: admin });
      expect(get.status).toBe(200);
      expect(seen).toEqual(['rateLimit', 'headers']);
      seen.length = 0;
      const forged = await call(strict, 'customerCreate', { as: { ...admin, csrf: 'wrong' }, body: { name: 'X', kind: 'person' } });
      expect(forged.status).toBe(403);
      expect((await errorOf(forged)).key).toBe('csrf');
      // Error answers get the headers too.
      expect(forged.headers.get('x-test-headers')).toBe('1');
      expect(seen).toEqual(['csrf:session', 'headers']);
    } finally {
      await strict.close();
    }
  });
});

describe('handler maps', () => {
  test('mergeHandlers refuses a route handled twice', () => {
    expect(() => mergeHandlers({ healthz: handlers.healthz }, { healthz: handlers.healthz })).toThrow('route healthz has two handlers');
  });

  test('assertHandlers names every route without a handler', () => {
    expect(() => assertHandlers(handlers)).toThrow(/routes without a handler: .*authConfig/);
    const all = Object.fromEntries(ROUTE_LIST.map((r) => [r.id, () => Promise.resolve(json({}))])) as PartialHandlerMap;
    expect(() => assertHandlers(all)).not.toThrow();
  });
});
