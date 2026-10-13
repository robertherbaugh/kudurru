// Calling the real server in tests. makeTestApp starts the production
// Bun.serve (startServer: the same routes, pipeline, body limit and error
// hook) on 127.0.0.1 with a free port; call() sends one request to a route
// by RouteId, signed in or not:
//
//   const app = makeTestApp({ handlers, db: t.db });
//   afterAll(() => app.close());
//   const res = await call(app, 'licenseGet', { as: await signInAs(t.db, admin), params: { id } });
//
// The app logs into a MemoryLogger and mails into a MemoryMailer unless
// told otherwise, so tests can assert on both.

import type { Server } from 'bun';
import type { ApiErrorBody, ResponseOf } from '../../shared/api';
import { buildPath, ROUTES, type QueryInit, type RouteId, type RouteParams } from '../../shared/routes';
import type { Config } from '../config';
import type { Db } from '../db/sql';
import { startServer, type ServerOptions } from '../http/router';
import type { AppDeps, PartialHandlerMap, Pipeline } from '../http/types';
import { signingUnavailable, type SigningState } from '../licensing/types';
import { MemoryLogger, type Logger } from '../log';
import { MemoryMailer } from '../mail/memory';
import type { Mailer } from '../mail/types';
import { testConfig, type TestSession } from './fixtures';

export interface TestAppOptions {
  handlers: PartialHandlerMap;
  /** Default: a stand-in that throws when used (for tests that need no database). */
  db?: Db;
  config?: Config;
  /** Default: signing unavailable ("no signing key in this test app") with an empty keyring. */
  signing?: SigningState;
  mailer?: Mailer;
  log?: Logger;
  /** Stream A's pipeline steps; default the pass-through pipeline. */
  pipeline?: Partial<Pipeline>;
  clock?: () => Date;
  /** Static routes and the SPA fallback, as main.ts passes them (ServerOptions). */
  extraRoutes?: ServerOptions['extraRoutes'];
  fallback?: ServerOptions['fallback'];
}

export interface TestApp {
  readonly server: Server<undefined>;
  /** http://127.0.0.1:<port> */
  readonly baseUrl: string;
  readonly deps: AppDeps;
  /** The app's logger when it is the default MemoryLogger (else a fresh, unused one). */
  readonly log: MemoryLogger;
  /** The app's mailer when it is the default MemoryMailer (else a fresh, unused one). */
  readonly mailer: MemoryMailer;
  /** A raw request to a path on the app. */
  fetch(path: string, init?: RequestInit): Promise<Response>;
  close(): Promise<void>;
}

/** A Db that throws on any use. */
function noDb(): Db {
  const fail = (): never => {
    throw new Error('this test app has no database (pass db to makeTestApp)');
  };
  return new Proxy(fail, { apply: fail, get: fail }) as unknown as Db;
}

/** Starts the server for a test file; close() it in afterAll. */
export function makeTestApp(opts: TestAppOptions): TestApp {
  const log = opts.log instanceof MemoryLogger ? opts.log : new MemoryLogger();
  const mailer = opts.mailer instanceof MemoryMailer ? opts.mailer : new MemoryMailer();
  const deps: AppDeps = {
    db: opts.db ?? noDb(),
    config: opts.config ?? testConfig(),
    log: opts.log ?? log,
    signing: opts.signing ?? signingUnavailable('no signing key in this test app'),
    mailer: opts.mailer ?? mailer,
    clock: opts.clock,
  };
  const server = startServer(deps, opts.handlers, { hostname: '127.0.0.1', port: 0, extraRoutes: opts.extraRoutes, fallback: opts.fallback }, opts.pipeline);
  const baseUrl = `http://127.0.0.1:${server.port}`;
  return {
    server,
    baseUrl,
    deps,
    log,
    mailer,
    fetch: (path, init) => fetch(baseUrl + path, { redirect: 'manual', ...init }),
    close: () => server.stop(true),
  };
}

export interface CallOptions<Id extends RouteId> {
  /** Sign in as this session (Cookie and X-Kudurru-CSRF headers). */
  as?: TestSession | null;
  params?: RouteParams<Id>;
  query?: QueryInit<Id>;
  /** Sent as JSON with Content-Type application/json. */
  body?: unknown;
  /** Sent as is (set Content-Type yourself through headers). Wins over body. */
  rawBody?: string | Uint8Array;
  headers?: Record<string, string>;
  /** Default: the route's method. */
  method?: string;
}

/** One request to a route. Redirects are not followed. */
export function call<Id extends RouteId>(app: TestApp, id: Id, opts: CallOptions<Id> = {}): Promise<Response> {
  const path = (buildPath as (id: RouteId, params?: object, query?: object) => string)(id, opts.params, opts.query);
  const headers: Record<string, string> = {};
  if (opts.as) {
    headers.Cookie = opts.as.cookie;
    headers['X-Kudurru-CSRF'] = opts.as.csrf;
  }
  let body: string | Uint8Array | undefined;
  if (opts.rawBody !== undefined) body = opts.rawBody;
  else if (opts.body !== undefined) {
    body = JSON.stringify(opts.body);
    headers['Content-Type'] = 'application/json';
  }
  return app.fetch(path, { method: opts.method ?? ROUTES[id].method, headers: { ...headers, ...opts.headers }, body });
}

/** The parsed answer of callJson. */
export interface JsonResult<T> {
  status: number;
  /** The success body as the route's ResponseOf types it (check status first). */
  body: T;
  /** The error envelope's content when the answer is an error (status ≥ 400), else null. */
  error: ApiErrorBody['error'] | null;
  res: Response;
}

/** call() and parse the JSON answer, typed by the route's response (shared/api.ts RouteTypes). */
export async function callJson<Id extends RouteId>(app: TestApp, id: Id, opts: CallOptions<Id> = {}): Promise<JsonResult<ResponseOf<Id>>> {
  const res = await call(app, id, opts);
  const text = await res.text();
  const parsed: unknown = text ? JSON.parse(text) : undefined;
  const error = res.status >= 400 && parsed && typeof parsed === 'object' && 'error' in parsed ? (parsed as ApiErrorBody).error : null;
  return { status: res.status, body: parsed as ResponseOf<Id>, error, res };
}
