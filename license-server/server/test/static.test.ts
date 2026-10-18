// The UI files (design spec §2 "UI"): the build is read once into a map
// (dot-files and symlinks skipped, nothing outside the build reachable,
// whatever the request path spells: "..", %2e%2e, encoded slashes,
// backslashes), hashed assets/ are immutable for a year, index.html and
// the other files revalidate (ETag, 304), client-side routes get
// index.html, missing files and anything under /api/ do not, and only GET
// and HEAD are answered. Raw request lines go over a socket so no client
// normalizes the path first.
import { afterAll, beforeAll, describe, expect, test } from 'bun:test';
import { mkdirSync, mkdtempSync, symlinkSync, writeFileSync } from 'node:fs';
import { connect } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createPipeline } from '../http/pipeline';
import { IMMUTABLE, loadStaticUi, looksLikeFile, noUiFallback, readBuild, REVALIDATE, type StaticUi } from '../http/static';
import { MemoryLogger } from '../log';
import { makeTestApp, type TestApp } from './call';
import { testConfig } from './fixtures';

const INDEX = '<!doctype html><html><head><base href="/"><script type="module" src="./assets/index-abc123.js"></script></head><body></body></html>';

/** One raw HTTP/1.1 request; the status line and the body. */
function raw(port: number, requestLine: string): Promise<{ status: number; text: string }> {
  return new Promise((resolve, reject) => {
    const socket = connect(port, '127.0.0.1', () => socket.write(`${requestLine}\r\nHost: localhost\r\nConnection: close\r\n\r\n`));
    let data = '';
    socket.on('data', (d: Buffer) => (data += d.toString('latin1')));
    socket.on('end', () => resolve({ status: Number(data.split(' ')[1]), text: data }));
    socket.on('error', reject);
  });
}

describe('the UI build', () => {
  let dir: string;
  let outside: string;
  let ui: StaticUi;
  let app: TestApp;
  const log = new MemoryLogger();

  beforeAll(() => {
    const root = mkdtempSync(join(tmpdir(), 'kls-static-'));
    dir = join(root, 'ui');
    outside = join(root, 'secret.txt');
    mkdirSync(join(dir, 'assets'), { recursive: true });
    writeFileSync(join(dir, 'index.html'), INDEX);
    writeFileSync(join(dir, 'assets', 'index-abc123.js'), 'export const x = 1;');
    writeFileSync(join(dir, 'assets', 'index-def456.css'), 'body{}');
    writeFileSync(join(dir, 'assets', 'logo-1a2b3c.svg'), '<svg/>');
    writeFileSync(join(dir, 'third-party-licenses.txt'), 'MIT');
    writeFileSync(join(dir, '.env'), 'SECRET=1');
    writeFileSync(outside, 'the signing key');
    symlinkSync(outside, join(dir, 'link.txt'));
    mkdirSync(join(dir, 'api'));
    writeFileSync(join(dir, 'api', 'x.json'), '{}');
    ui = loadStaticUi(log, [join(root, 'nope'), dir])!;
    const config = testConfig();
    app = makeTestApp({ handlers: {}, config, log, pipeline: createPipeline(config, log), extraRoutes: ui.routes, fallback: ui.fallback });
  });
  afterAll(() => app.close());

  test('only regular files of the build, no dot-files, no symlinks, nothing under /api', () => {
    expect([...ui.files.keys()].sort()).toEqual(['/assets/index-abc123.js', '/assets/index-def456.css', '/assets/logo-1a2b3c.svg', '/index.html', '/third-party-licenses.txt']);
    expect(ui.dir).toBe(dir);
    expect(readBuild(dir).has('/link.txt')).toBe(false);
    expect(log.find('serving the UI')).toHaveLength(1);
  });

  test('index.html revalidates, hashed assets are immutable, types are right', async () => {
    const index = await app.fetch('/');
    expect(index.status).toBe(200);
    expect(await index.text()).toBe(INDEX);
    expect(index.headers.get('content-type')).toBe('text/html; charset=utf-8');
    expect(index.headers.get('cache-control')).toBe(REVALIDATE);
    expect(index.headers.get('content-security-policy')).toContain("script-src 'self'");
    expect((await app.fetch('/index.html')).headers.get('cache-control')).toBe(REVALIDATE);

    const js = await app.fetch('/assets/index-abc123.js');
    expect(js.headers.get('content-type')).toBe('text/javascript; charset=utf-8');
    expect(js.headers.get('cache-control')).toBe(IMMUTABLE);
    expect(js.headers.get('cache-control')).toBe('public, max-age=31536000, immutable');
    expect((await app.fetch('/assets/index-def456.css')).headers.get('content-type')).toBe('text/css; charset=utf-8');
    expect((await app.fetch('/assets/logo-1a2b3c.svg')).headers.get('content-type')).toBe('image/svg+xml');
    const txt = await app.fetch('/third-party-licenses.txt');
    expect(txt.headers.get('content-type')).toBe('text/plain; charset=utf-8');
    expect(txt.headers.get('cache-control')).toBe(REVALIDATE);
  });

  test('ETag and 304; HEAD has no body', async () => {
    const first = await app.fetch('/');
    const etag = first.headers.get('etag')!;
    expect(etag).toMatch(/^"[0-9a-f]{24}"$/);
    const again = await app.fetch('/', { headers: { 'If-None-Match': etag } });
    expect(again.status).toBe(304);
    expect(await again.text()).toBe('');
    expect(again.headers.get('cache-control')).toBe(REVALIDATE);
    expect((await app.fetch('/my/licenses', { headers: { 'If-None-Match': `W/${etag}` } })).status).toBe(304);
    expect((await app.fetch('/', { headers: { 'If-None-Match': '"other"' } })).status).toBe(200);
    const head = await app.fetch('/assets/index-abc123.js', { method: 'HEAD' });
    expect(head.status).toBe(200);
    expect(await head.text()).toBe('');
    const headSpa = await app.fetch('/licenses', { method: 'HEAD' });
    expect(headSpa.status).toBe(200);
    expect(await headSpa.text()).toBe('');
  });

  test('client routes get index.html; missing files and /api do not', async () => {
    // Deeper paths are routes even with a dot: an imported license keeps its own id (acme.2024).
    for (const path of ['/login', '/my/licenses', '/licenses/lic_abcDEF123456', '/invites/abc_DEF-123', '/verify?token=abc', '/customers/cus_x/', '/licenses/acme.2024', '/my/licenses/acme.2024', '/my/app.js']) {
      const res = await app.fetch(path);
      expect(`${path} ${res.status}`).toBe(`${path} 200`);
      expect(await res.text()).toBe(INDEX);
      expect(res.headers.get('cache-control')).toBe(REVALIDATE);
    }
    for (const path of ['/assets/index-old999.js', '/assets/nothing', '/assets/sub/x', '/favicon.ico', '/robots.txt', '/.env', '/link.txt']) {
      const res = await app.fetch(path);
      expect(`${path} ${res.status}`).toBe(`${path} 404`);
      expect(await res.text()).not.toContain('<!doctype');
    }
    const api = await app.fetch('/api/v1/unknown');
    expect(api.status).toBe(404);
    expect(((await api.json()) as { error: { key: string } }).error.key).toBe('not_found');
    expect((await app.fetch('/api/x.json')).status).toBe(404);
  });

  test('only GET and HEAD', async () => {
    for (const [path, method] of [
      ['/', 'POST'],
      ['/my/licenses', 'PUT'],
      ['/assets/index-abc123.js', 'DELETE'],
    ] as const) {
      const res = await app.fetch(path, { method });
      expect(res.status).toBe(405);
      expect(res.headers.get('allow')).toBe('GET, HEAD');
    }
  });

  test('no request path reaches outside the build', async () => {
    const port = app.server.port!;
    for (const line of [
      'GET /../secret.txt HTTP/1.1',
      'GET /assets/../../secret.txt HTTP/1.1',
      'GET /%2e%2e/secret.txt HTTP/1.1',
      'GET /assets/%2e%2e/%2e%2e/secret.txt HTTP/1.1',
      'GET /..%2fsecret.txt HTTP/1.1',
      'GET /assets/..%2f..%2fsecret.txt HTTP/1.1',
      'GET /..%5csecret.txt HTTP/1.1',
      'GET /link.txt HTTP/1.1',
      'GET /%2elink.txt HTTP/1.1',
      'GET //etc/passwd HTTP/1.1',
      'GET /assets%2findex-abc123.js HTTP/1.1',
    ]) {
      const res = await raw(port, line);
      expect(res.text).not.toContain('the signing key');
      expect(res.text).not.toContain('root:');
      expect([200, 400, 404]).toContain(res.status);
    }
  });

  test('looksLikeFile', () => {
    expect(looksLikeFile('/assets/x')).toBe(true);
    expect(looksLikeFile('/favicon.ico')).toBe(true);
    expect(looksLikeFile('/robots.txt')).toBe(true);
    expect(looksLikeFile('/.env')).toBe(true);
    expect(looksLikeFile('/assets')).toBe(true);
    expect(looksLikeFile('/a/b.c')).toBe(false);
    expect(looksLikeFile('/licenses/acme.2024')).toBe(false);
    expect(looksLikeFile('/licenses/lic_x')).toBe(false);
    expect(looksLikeFile('/login')).toBe(false);
    expect(looksLikeFile('/')).toBe(false);
  });
});

describe('no UI build', () => {
  test('a warning, a plain 404 outside /api and the JSON 404 inside', async () => {
    const log = new MemoryLogger();
    expect(loadStaticUi(log, [join(tmpdir(), 'kls-no-such-dir')])).toBeNull();
    expect(log.find('the UI build is missing; serving the API only')).toHaveLength(1);
    const config = testConfig();
    const app = makeTestApp({ handlers: {}, config, log, pipeline: createPipeline(config, log), fallback: noUiFallback });
    try {
      const page = await app.fetch('/my/licenses');
      expect(page.status).toBe(404);
      expect(page.headers.get('content-type')).toBe('text/plain; charset=utf-8');
      expect(page.headers.get('x-frame-options')).toBe('DENY');
      const api = await app.fetch('/api/v1/nope');
      expect(api.status).toBe(404);
      expect(api.headers.get('content-type')).toContain('application/json');
    } finally {
      await app.close();
    }
  });

  test('the real build next to the bundle or in dist/ui is found when present', () => {
    const dirs = loadStaticUi(new MemoryLogger());
    if (dirs) expect(dirs.files.has('/index.html')).toBe(true);
  });
});
