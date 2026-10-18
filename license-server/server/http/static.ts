// The UI (design spec §2 "UI"): the Vite build in dist/ui, read into memory
// once at startup and served from that map. Request paths are only ever
// looked up in the map, never joined onto the filesystem, so no spelling
// of "..", no encoded slash and no symlink reaches a file outside the
// build. Caching: content-hashed files under assets/ are immutable for a
// year; index.html and the other top-level files are revalidated every
// time (no-cache, with an ETag). Every other path outside /api/
// is a client-side route and gets index.html (SPA fallback); a
// missing file (anything under assets/, or a top-level name with a dot) is
// a plain 404, never index.html, so a stale script URL cannot get HTML
// back. Deeper paths are routes even with a dot (/licenses/acme.2024).
//
// Where the build is: next to the bundle (dist/server.js → dist/ui, the
// image's /app/server.js → /app/ui), or dist/ui of the checkout when the
// server runs from source (bun server/main.ts).

import { createHash } from 'node:crypto';
import { existsSync, readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import type { Logger } from '../log';
import { notFoundResponse, type BunHandler, type BunRoutes } from './router';

/** One file of the build, ready to send. */
export interface StaticFile {
  readonly body: Uint8Array;
  readonly contentType: string;
  readonly cacheControl: string;
  readonly etag: string;
}

/** The UI: its files by URL path and the handlers main.ts gives startServer. */
export interface StaticUi {
  /** The directory it was read from. */
  readonly dir: string;
  /** URL path ('/index.html', '/assets/index-abc.js') → file. */
  readonly files: ReadonlyMap<string, StaticFile>;
  /** One GET route per file (ServerOptions.extraRoutes); HEAD is answered from GET. */
  readonly routes: BunRoutes;
  /** Everything no route matched (ServerOptions.fallback): index.html for client routes, 404 for missing files. */
  readonly fallback: (req: Request) => Response;
}

/** Cache-Control of the content-hashed assets/ files. */
export const IMMUTABLE = 'public, max-age=31536000, immutable';
/** Cache-Control of index.html and the other unhashed files. */
export const REVALIDATE = 'no-cache';

const TYPES: Readonly<Record<string, string>> = {
  html: 'text/html; charset=utf-8',
  js: 'text/javascript; charset=utf-8',
  mjs: 'text/javascript; charset=utf-8',
  css: 'text/css; charset=utf-8',
  json: 'application/json; charset=utf-8',
  map: 'application/json; charset=utf-8',
  webmanifest: 'application/manifest+json; charset=utf-8',
  txt: 'text/plain; charset=utf-8',
  svg: 'image/svg+xml',
  png: 'image/png',
  jpg: 'image/jpeg',
  jpeg: 'image/jpeg',
  gif: 'image/gif',
  webp: 'image/webp',
  avif: 'image/avif',
  ico: 'image/x-icon',
  woff: 'font/woff',
  woff2: 'font/woff2',
  ttf: 'font/ttf',
  otf: 'font/otf',
  wasm: 'application/wasm',
};

function contentTypeOf(path: string): string {
  const dot = path.lastIndexOf('.');
  const ext = dot > path.lastIndexOf('/') ? path.slice(dot + 1).toLowerCase() : '';
  return TYPES[ext] ?? 'application/octet-stream';
}

/** Route paths Bun's router takes literally (no :param, no *, nothing to percent-encode). Other files are still served, by the fallback. */
const ROUTE_SAFE = /^\/[A-Za-z0-9._~\-/]*$/;

/** The candidate build directories, most specific first. */
export function defaultUiDirs(): string[] {
  return [join(import.meta.dir, 'ui'), join(import.meta.dir, '..', '..', 'dist', 'ui')];
}

/**
 * Reads every regular file under `dir` (symlinks and dot-files are
 * skipped) into URL path → bytes. Throws when `dir` cannot be read.
 */
export function readBuild(dir: string): Map<string, Uint8Array> {
  const out = new Map<string, Uint8Array>();
  const walk = (abs: string, urlPath: string): void => {
    for (const entry of readdirSync(abs, { withFileTypes: true })) {
      if (entry.name.startsWith('.')) continue;
      const childAbs = join(abs, entry.name);
      const childUrl = `${urlPath}/${entry.name}`;
      if (entry.isDirectory()) walk(childAbs, childUrl);
      else if (entry.isFile()) out.set(childUrl, new Uint8Array(readFileSync(childAbs)));
    }
  };
  walk(dir, '');
  return out;
}

function etagOf(body: Uint8Array): string {
  return `"${createHash('sha256').update(body).digest('hex').slice(0, 24)}"`;
}

function textResponse(status: number, text: string, headers: Record<string, string> = {}): Response {
  return new Response(text, { status, headers: { 'Content-Type': 'text/plain; charset=utf-8', 'Cache-Control': 'no-store', ...headers } });
}

/** Whether the request's If-None-Match names this ETag. */
function notModified(req: Request, etag: string): boolean {
  const inm = req.headers.get('if-none-match');
  if (!inm) return false;
  return inm.trim() === '*' || inm.split(',').some((t) => t.trim().replace(/^W\//, '') === etag);
}

/** The response for one file (304 when the client has it; no body for HEAD). */
function serveFile(req: Request, f: StaticFile): Response {
  const headers = { 'Content-Type': f.contentType, 'Cache-Control': f.cacheControl, ETag: f.etag };
  if (notModified(req, f.etag)) return new Response(null, { status: 304, headers: { 'Cache-Control': f.cacheControl, ETag: f.etag } });
  return new Response(req.method === 'HEAD' ? null : f.body, { headers });
}

/**
 * Whether a missing path names a file rather than a client-side route:
 * anything under assets/, or a top-level name with a dot (/favicon.ico,
 * /robots.txt). A deeper path is a route even with a dot in it: an imported
 * license keeps its own id, so /licenses/acme.2024 must reload into the SPA.
 */
export function looksLikeFile(path: string): boolean {
  if (path === '/assets' || path.startsWith('/assets/')) return true;
  return path.lastIndexOf('/') === 0 && path.includes('.');
}

/** Builds the StaticUi from files already read (tests pass a map; loadStaticUi reads the build). */
export function staticUiFrom(dir: string, build: ReadonlyMap<string, Uint8Array>): StaticUi {
  const files = new Map<string, StaticFile>();
  for (const [path, body] of build) {
    if (path === '/api' || path.startsWith('/api/')) continue;
    files.set(path, {
      body,
      contentType: contentTypeOf(path),
      cacheControl: path.startsWith('/assets/') ? IMMUTABLE : REVALIDATE,
      etag: etagOf(body),
    });
  }
  const index = files.get('/index.html') ?? null;

  const routes: BunRoutes = {};
  for (const [path, f] of files) {
    if (!ROUTE_SAFE.test(path)) continue;
    const handler: BunHandler = (req) => serveFile(req, f);
    routes[path] = { GET: handler };
  }
  if (index) routes['/'] = { GET: (req) => serveFile(req, index) };

  const fallback = (req: Request): Response => {
    const path = new URL(req.url).pathname;
    if (path === '/api' || path.startsWith('/api/')) return notFoundResponse();
    if (req.method !== 'GET' && req.method !== 'HEAD') return textResponse(405, 'Method not allowed.', { Allow: 'GET, HEAD' });
    const f = files.get(path) ?? (path === '/' ? index : null);
    if (f) return serveFile(req, f);
    if (looksLikeFile(path)) return textResponse(404, 'Not found.');
    if (!index) return textResponse(404, 'The UI is not part of this build (bun run build:ui).');
    return serveFile(req, index);
  };

  return { dir, files, routes, fallback };
}

/**
 * Reads the UI build from the first of `dirs` (default defaultUiDirs())
 * that has an index.html. Null, with a warning, when there is none: the API
 * still works, and the fallback answers 404 (development runs the UI from
 * Vite instead).
 */
export function loadStaticUi(log: Logger, dirs: readonly string[] = defaultUiDirs()): StaticUi | null {
  const dir = dirs.find((d) => existsSync(join(d, 'index.html')));
  if (!dir) {
    log.warn('the UI build is missing; serving the API only', { looked: dirs });
    return null;
  }
  const ui = staticUiFrom(dir, readBuild(dir));
  log.info('serving the UI', { dir, files: ui.files.size });
  return ui;
}

/** The fallback without a UI build: JSON 404 under /api, a plain 404 elsewhere. */
export function noUiFallback(req: Request): Response {
  const path = new URL(req.url).pathname;
  if (path === '/api' || path.startsWith('/api/')) return notFoundResponse();
  return textResponse(404, 'The UI is not part of this build (bun run build:ui).');
}
