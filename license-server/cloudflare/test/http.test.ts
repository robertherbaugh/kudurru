// What the container receives and what the Worker answers on its own
// (src/http.ts).

import { describe, expect, test } from 'bun:test';
import { forContainer, fromPlatform, httpsRedirect, PUBLIC_FORM_PATH, unavailable, type Settings } from '../src/http';

const PROD: Settings = { publicUrl: 'https://licenses.kudurru.io', formOrigins: 'https://kudurru.io' };

describe('forContainer', () => {
  test('replaces every client-sent address with the edge address and keeps the rest', async () => {
    const body = JSON.stringify({ name: 'Ada' });
    const req = new Request('https://licenses.kudurru.io/api/v1/me/licenses/7?x=1', {
      method: 'PATCH',
      body,
      headers: {
        'Content-Type': 'application/json',
        'Content-Length': String(body.length),
        Cookie: '__Host-kls_session=abc',
        Origin: 'https://licenses.kudurru.io',
        'Sec-Fetch-Site': 'same-origin',
        'X-Kudurru-CSRF': 'token',
        'CF-Connecting-IP': '2001:db8::7',
        'X-Forwarded-For': '203.0.113.9, 198.51.100.1',
        'X-Client-IP': '203.0.113.9',
        'X-Real-IP': '203.0.113.9',
        Forwarded: 'for=203.0.113.9',
        'True-Client-IP': '203.0.113.9',
        'cf-container-target-port': '22',
      },
    });
    const out = await forContainer(req);
    expect(out.method).toBe('PATCH');
    expect(out.url).toBe('https://licenses.kudurru.io/api/v1/me/licenses/7?x=1');
    expect(out.redirect).toBe('manual');
    expect(out.headers.get('x-client-ip')).toBe('2001:db8::7');
    expect(out.headers.get('x-forwarded-for')).toBeNull();
    expect(out.headers.get('x-real-ip')).toBeNull();
    expect(out.headers.get('forwarded')).toBeNull();
    expect(out.headers.get('true-client-ip')).toBeNull();
    expect(out.headers.get('cf-container-target-port')).toBeNull();
    expect(out.headers.get('cookie')).toBe('__Host-kls_session=abc');
    expect(out.headers.get('origin')).toBe('https://licenses.kudurru.io');
    expect(out.headers.get('sec-fetch-site')).toBe('same-origin');
    expect(out.headers.get('content-type')).toBe('application/json');
    expect(out.headers.get('x-kudurru-csrf')).toBe('token');
    expect(await out.text()).toBe(body);
  });

  test('sets no address without a usable CF-Connecting-IP', async () => {
    const plain = await forContainer(new Request('https://licenses.kudurru.io/', { headers: { 'X-Forwarded-For': '203.0.113.9' } }));
    expect(plain.headers.get('x-client-ip')).toBeNull();
    expect(plain.headers.get('x-forwarded-for')).toBeNull();
    const odd = await forContainer(new Request('https://licenses.kudurru.io/', { headers: { 'CF-Connecting-IP': '203.0.113.9, 10.0.0.1' } }));
    expect(odd.headers.get('x-client-ip')).toBeNull();
  });

  test('streams a body over 64 KiB through instead of reading it', async () => {
    const big = 'x'.repeat(70 * 1024);
    const out = await forContainer(new Request('https://licenses.kudurru.io/api/v1/x', { method: 'POST', body: big, headers: { 'Content-Length': String(big.length) } }));
    expect((await out.text()).length).toBe(big.length);
  });
});

describe('fromPlatform', () => {
  test('tells the Container class answers from the server ones', () => {
    expect(fromPlatform(new Response('There is no Container instance available at this time.', { status: 503 }))).toBe(true);
    expect(fromPlatform(new Response('Failed to start container: boom', { status: 500 }))).toBe(true);
    expect(fromPlatform(new Response('rate limited', { status: 429 }))).toBe(true);
    const server = { 'X-Content-Type-Options': 'nosniff', 'Content-Type': 'application/json' };
    expect(fromPlatform(new Response('{}', { status: 503, headers: server }))).toBe(false);
    expect(fromPlatform(new Response('{}', { status: 429, headers: server }))).toBe(false);
    expect(fromPlatform(new Response('nope', { status: 404 }))).toBe(false);
    expect(fromPlatform(new Response('ok', { status: 200 }))).toBe(false);
  });
});

describe('unavailable', () => {
  test('answers the API in the server error shape, without the cause', async () => {
    const res = unavailable(new Request('https://licenses.kudurru.io/api/v1/auth/me'), PROD);
    expect(res.status).toBe(503);
    expect(res.headers.get('content-type')).toBe('application/json; charset=utf-8');
    expect(res.headers.get('retry-after')).toBe('30');
    expect(res.headers.get('cache-control')).toBe('no-store');
    expect(res.headers.get('x-content-type-options')).toBe('nosniff');
    expect(res.headers.get('strict-transport-security')).toBe('max-age=31536000');
    expect(res.headers.get('access-control-allow-origin')).toBeNull();
    const body = (await res.json()) as { error: { key: string; message: string } };
    expect(body.error.key).toBe('unavailable');
    expect(Object.keys(body.error)).toEqual(['key', 'message']);
  });

  test('answers pages with HTML and HEAD without a body', async () => {
    const page = unavailable(new Request('https://licenses.kudurru.io/licenses'), PROD);
    expect(page.headers.get('content-type')).toBe('text/html; charset=utf-8');
    expect(await page.text()).toContain('not available right now');
    const head = unavailable(new Request('https://licenses.kudurru.io/', { method: 'HEAD' }), PROD);
    expect(head.status).toBe(503);
    expect(await head.text()).toBe('');
  });

  test('lets kudurru.io read the public form answer', async () => {
    const url = `https://licenses.kudurru.io${PUBLIC_FORM_PATH}`;
    const post = unavailable(new Request(url, { method: 'POST', headers: { Origin: 'https://kudurru.io' } }), PROD);
    expect(post.status).toBe(503);
    expect(post.headers.get('access-control-allow-origin')).toBe('https://kudurru.io');
    expect(post.headers.get('access-control-expose-headers')).toBe('Retry-After');
    expect(post.headers.get('vary')).toBe('Origin');
    expect(((await post.json()) as { error: { key: string } }).error.key).toBe('unavailable');

    const preflight = unavailable(new Request(url, { method: 'OPTIONS', headers: { Origin: 'https://kudurru.io' } }), PROD);
    expect(preflight.status).toBe(204);
    expect(preflight.headers.get('access-control-allow-methods')).toBe('POST, OPTIONS');
    expect(preflight.headers.get('access-control-allow-headers')).toBe('Content-Type, Accept');
  });

  test('gives other origins no CORS headers', () => {
    const url = `https://licenses.kudurru.io${PUBLIC_FORM_PATH}`;
    const other = unavailable(new Request(url, { method: 'OPTIONS', headers: { Origin: 'https://evil.example' } }), PROD);
    expect(other.status).toBe(503);
    expect(other.headers.get('access-control-allow-origin')).toBeNull();
    const off = unavailable(new Request(url, { method: 'POST', headers: { Origin: 'https://kudurru.io' } }), { ...PROD, formOrigins: '-' });
    expect(off.headers.get('access-control-allow-origin')).toBeNull();
    const verify = unavailable(new Request(`${url}/verify`, { method: 'POST', headers: { Origin: 'https://kudurru.io' } }), PROD);
    expect(verify.headers.get('access-control-allow-origin')).toBeNull();
  });

  test('defaults the form origin to https://kudurru.io like the server', () => {
    const url = `https://licenses.kudurru.io${PUBLIC_FORM_PATH}`;
    const res = unavailable(new Request(url, { method: 'POST', headers: { Origin: 'https://kudurru.io' } }), { ...PROD, formOrigins: undefined });
    expect(res.headers.get('access-control-allow-origin')).toBe('https://kudurru.io');
  });
});

describe('httpsRedirect', () => {
  test('moves plain http to https only when PUBLIC_URL is https', () => {
    const res = httpsRedirect(new Request('http://licenses.kudurru.io/login?next=%2F'), PROD.publicUrl);
    expect(res?.status).toBe(308);
    expect(res?.headers.get('location')).toBe('https://licenses.kudurru.io/login?next=%2F');
    expect(httpsRedirect(new Request('https://licenses.kudurru.io/'), PROD.publicUrl)).toBeNull();
    expect(httpsRedirect(new Request('http://localhost:8787/'), 'http://localhost:8787')).toBeNull();
  });
});
