// The router: Bun.serve's routes object built from shared/routes.ts
// (design spec §2, §8). Every route runs the same pipeline:
//
//   request id + client IP → headers → CORS (routes with `cors`, OPTIONS
//   included) → body limit → JSON-only content type (unsafe methods with a
//   body) → session (session and permission routes) → CSRF (unsafe methods
//   on session and permission routes) → rate limit → authorize → handler
//
// Errors thrown anywhere become JSON (errors.ts toResponse) where they are
// thrown, so the outer steps (headers, CORS) decorate error answers too.
// Bun.serve has no automatic 405: every path answers its other methods with
// a JSON 405 and an Allow header, and anything else under /api answers a
// JSON 404. The pipeline steps stream A owns are injected (Pipeline) with
// pass-through defaults; main.ts asserts that every route has a handler.
// startServer also runs the static routes, the SPA fallback and Bun's error
// hook through the headers step, so every response carries the security
// headers and X-Request-Id (spec §2).

import type { BunRequest, Serve, Server } from 'bun';
import { CookieMap } from 'bun';
import type { ApiErrorBody } from '../../shared/api';
import { ROUTE_LIST, UNSAFE_METHODS, type RouteDef, type RouteId } from '../../shared/routes';
import { resolveSession, sessionCookieName } from '../auth/sessions';
import { badBody, bodyLimitOf, hasBody, isJsonContentType } from './body';
import { HttpError, internalError, json, noContent, toResponse } from './errors';
import type { AppDeps, Handler, HandlerMap, HeaderContext, Middleware, PartialHandlerMap, Pipeline, RequestContext } from './types';

/** A Bun route handler as the routes object takes it. */
export type BunHandler = (req: BunRequest, server: Server<undefined>) => Response | Promise<Response>;

/**
 * Bun.serve's maxRequestBodySize: well above every route's limit
 * (DEFAULT_BODY_LIMIT, PUBLIC_BODY_LIMIT), which the router and readJson
 * enforce themselves so an oversize body gets the JSON 413 (the public
 * form's 400 invalid) with CORS and the security headers. Only a body
 * declared larger than this gets Bun's own empty 413 before any route runs.
 */
export const MAX_REQUEST_BODY = 1024 * 1024;

/** Bun.serve's routes object. */
export type BunRoutes = Record<string, BunHandler | Partial<Record<Serve.HTTPMethod, BunHandler>>>;

/** The pass-through pipeline (sessions resolve from the cookie; everything else passes). */
export const defaultPipeline: Pipeline = {
  clientIp: (_req, peer) => peer,
  headers: (res) => res,
  cors: (ctx, next) => (ctx.req.method === 'OPTIONS' ? Promise.resolve(noContent()) : next()),
  session: (ctx) => resolveSession(ctx.db, ctx.cookies.get(sessionCookieName(ctx.config)), ctx.now, ctx.config),
  csrf: (_ctx, next) => next(),
  rateLimit: (_ctx, next) => next(),
};

/** The 404 of anything unknown under /api. */
export function notFoundResponse(): Response {
  const body: ApiErrorBody = { error: { key: 'not_found', message: 'There is no such API route.' } };
  return json(body, { status: 404 });
}

function methodNotAllowed(allow: string[]): Response {
  const body: ApiErrorBody = { error: { key: 'method_not_allowed', message: `This route answers ${allow.join(', ')}.` } };
  return json(body, { status: 405, headers: { Allow: allow.join(', ') } });
}

/** Sets a header, cloning the response when its headers are immutable. */
export function withHeader(res: Response, name: string, value: string): Response {
  try {
    res.headers.set(name, value);
    return res;
  } catch {
    const copy = new Response(res.body, res);
    copy.headers.set(name, value);
    return copy;
  }
}

/** The socket peer's address, IPv4-mapped IPv6 unwrapped. */
function peerOf(req: Request, server: Server<undefined>): string | null {
  const addr = server.requestIP(req)?.address ?? null;
  if (addr && addr.startsWith('::ffff:') && addr.includes('.')) return addr.slice(7);
  return addr;
}

/** Runs the steps around `final`; an error in any step becomes a response at that step. */
function compose(steps: readonly Middleware[], final: (ctx: RequestContext) => Promise<Response>): (ctx: RequestContext) => Promise<Response> {
  const dispatch = async (i: number, ctx: RequestContext): Promise<Response> => {
    try {
      const step = steps[i];
      if (!step) return await final(ctx);
      return await step(ctx, () => dispatch(i + 1, ctx));
    } catch (e) {
      return toResponse(e, ctx.requestId, ctx.log);
    }
  };
  return (ctx) => dispatch(0, ctx);
}

const isOpen = (route: RouteDef): boolean => route.access === 'anonymous' || route.access === 'public';

/** 413 (or the route's 400) when Content-Length already says the body is too large. */
const bodyLimitStep: Middleware = (ctx, next) => {
  const length = Number(ctx.req.headers.get('content-length') ?? '0');
  const limit = bodyLimitOf(ctx);
  if (Number.isFinite(length) && length > limit) throw badBody(ctx, 413, `The body is larger than ${limit} bytes.`);
  return next();
};

/**
 * JSON bodies only (spec §6): an unsafe request that carries a body must
 * send it as application/json (415 unsupported_media_type, or the route's
 * 400). Requests without a body pass whatever Content-Type they name.
 */
const contentTypeStep: Middleware = (ctx, next) => {
  if (UNSAFE_METHODS.includes(ctx.req.method) && hasBody(ctx.req) && !isJsonContentType(ctx.req.headers.get('content-type'))) {
    throw badBody(ctx, 415, 'Send the body as application/json.');
  }
  return next();
};

/** The access rule of the route: anonymous/public pass, session needs a principal, a permission needs principal.can. */
const authorizeStep: Middleware = (ctx, next) => {
  const access = ctx.route.access;
  if (access === 'anonymous' || access === 'public') return next();
  if (!ctx.principal) throw new HttpError(401, 'unauthenticated', 'Sign in to continue.');
  if (access === 'session' || ctx.principal.can(access)) return next();
  ctx.log.warn('authz.denied', { userId: ctx.principal.userId, missing: access });
  throw new HttpError(403, 'forbidden', 'You do not have permission to do this.', { hint: { missing: access } });
};

/** Answers OPTIONS on preflight routes once CORS let it through (never reaches the handler). */
const preflightStep: Middleware = (ctx, next) => (ctx.req.method === 'OPTIONS' ? Promise.resolve(noContent()) : next());

function notImplemented(route: RouteDef): Handler {
  return () => Promise.reject(new HttpError(501, 'internal', `The route ${route.id} has no handler yet.`));
}

/**
 * Bun.serve's routes for every route of the manifest that applies (the
 * public routes are left out when PUBLIC_FORM_ORIGINS is "-", so they answer
 * 404), plus the JSON 404 for the rest of /api. A route without a handler
 * answers 501 after authorization (tests pass partial maps; main.ts calls
 * assertHandlers first).
 */
export function buildRoutes(handlers: PartialHandlerMap, deps: AppDeps, overrides: Partial<Pipeline> = {}): BunRoutes {
  const pipeline: Pipeline = { ...defaultPipeline, ...overrides };
  const clock = deps.clock ?? (() => new Date());
  const decorate = decorator(pipeline);

  const routeHandler = (route: RouteDef): BunHandler => {
    const handler = (handlers[route.id] as Handler | undefined) ?? notImplemented(route);
    const steps: Middleware[] = [
      async (ctx, next) => decorate(await next(), ctx),
      ...(route.cors ? [pipeline.cors] : []),
      ...(route.preflight ? [preflightStep] : []),
      bodyLimitStep,
      contentTypeStep,
    ];
    if (!isOpen(route)) {
      steps.push(async (ctx, next) => {
        const resolved = await pipeline.session(ctx);
        ctx.principal = resolved?.principal ?? null;
        ctx.session = resolved?.session ?? null;
        return next();
      });
      steps.push((ctx, next) => (UNSAFE_METHODS.includes(ctx.req.method) ? pipeline.csrf(ctx, next) : next()));
    }
    steps.push(pipeline.rateLimit, authorizeStep);
    const run = compose(steps, (ctx) => handler(ctx));

    return async (req, server) => {
      const requestId = crypto.randomUUID();
      const url = new URL(req.url);
      const peerIp = peerOf(req, server);
      const userAgent = req.headers.get('user-agent');
      const log = deps.log.child({ requestId, route: route.id });
      let ip: string | null;
      let cookies: CookieMap;
      try {
        ip = pipeline.clientIp(req, peerIp, deps.config);
        // The public routes never read cookies (spec §6).
        cookies = new CookieMap(route.access === 'public' ? '' : (req.headers.get('cookie') ?? ''));
      } catch (e) {
        log.error('request setup failed', { err: e });
        return decorate(internalError(requestId), { req, url, config: deps.config, route, requestId });
      }
      const ctx: RequestContext = {
        req,
        url,
        params: req.params as RequestContext['params'],
        query: url.searchParams,
        cookies,
        ip,
        peerIp,
        requestId,
        userAgent: userAgent === null ? null : userAgent.slice(0, 512),
        now: clock(),
        principal: null,
        session: null,
        db: deps.db,
        config: deps.config,
        log,
        signing: deps.signing,
        mailer: deps.mailer,
        route,
      };
      return run(ctx);
    };
  };

  const plain = (make: () => Response): BunHandler => {
    return (req) => {
      const requestId = crypto.randomUUID();
      return Promise.resolve(decorate(make(), { req, url: new URL(req.url), config: deps.config, route: null, requestId }));
    };
  };

  const byPath = new Map<string, RouteDef[]>();
  for (const route of ROUTE_LIST) {
    if (route.access === 'public' && !deps.config.publicForm.enabled) continue;
    const list = byPath.get(route.path) ?? [];
    list.push(route);
    byPath.set(route.path, list);
  }

  const routes: BunRoutes = {};
  const ALL: Serve.HTTPMethod[] = ['GET', 'HEAD', 'POST', 'PUT', 'PATCH', 'DELETE', 'OPTIONS'];
  for (const [path, list] of byPath) {
    const methods: Partial<Record<Serve.HTTPMethod, BunHandler>> = {};
    const allow: string[] = [];
    for (const route of list) {
      const h = routeHandler(route);
      methods[route.method] = h;
      allow.push(route.method);
      if (route.method === 'GET') allow.push('HEAD');
      if (route.preflight) {
        methods.OPTIONS = h;
        allow.push('OPTIONS');
      }
    }
    const refuse = plain(() => methodNotAllowed(allow));
    for (const m of ALL) {
      // Bun answers HEAD from GET itself.
      if (m === 'HEAD' && methods.GET) continue;
      methods[m] ??= refuse;
    }
    routes[path] = methods;
  }
  const notFound = plain(notFoundResponse);
  routes['/api'] = notFound;
  routes['/api/*'] = notFound;
  return routes;
}

/** The headers step plus X-Request-Id: what every response goes through on its way out. */
function decorator(pipeline: Pipeline): (res: Response, hctx: HeaderContext) => Response {
  return (res, hctx) => withHeader(pipeline.headers(res, hctx), 'X-Request-Id', hctx.requestId);
}

/** Combines the handler maps of the streams; a route handled twice is a programming error. */
export function mergeHandlers(...maps: PartialHandlerMap[]): PartialHandlerMap {
  const out: Record<string, Handler> = {};
  for (const map of maps) {
    for (const [id, h] of Object.entries(map) as [RouteId, Handler | undefined][]) {
      if (!h) continue;
      if (out[id]) throw new Error(`route ${id} has two handlers`);
      out[id] = h;
    }
  }
  return out;
}

/** Throws unless every RouteId has a handler (main.ts, at startup). */
export function assertHandlers(handlers: PartialHandlerMap): asserts handlers is HandlerMap {
  const missing = ROUTE_LIST.filter((r) => !handlers[r.id]).map((r) => r.id);
  if (missing.length > 0) throw new Error(`routes without a handler: ${missing.join(', ')}`);
}

export interface ServerOptions {
  hostname: string;
  port: number;
  /**
   * More routes (the UI's static files, server/http/static.ts). The API
   * routes win on the same path. Their responses go through the headers step
   * (route null) and get X-Request-Id; a Cache-Control they set is theirs.
   */
  extraRoutes?: BunRoutes;
  /** Requests no route matched (the SPA fallback); default a JSON 404. Decorated like extraRoutes. */
  fallback?: (req: Request, server: Server<undefined>) => Response | Promise<Response>;
}

/**
 * Starts Bun.serve with the API routes (spec §2: development false, JSON
 * 500 from the error hook; MAX_REQUEST_BODY explains the body limit). The
 * static routes and the fallback run behind a guard that turns a throw into
 * the JSON 500 and adds the headers; Bun's error hook is the last resort for
 * a failure in the guard itself. Used by main.ts and by makeTestApp, so
 * tests run the production server.
 */
export function startServer(deps: AppDeps, handlers: PartialHandlerMap, opts: ServerOptions, overrides: Partial<Pipeline> = {}): Server<undefined> {
  const pipeline: Pipeline = { ...defaultPipeline, ...overrides };
  const decorate = decorator(pipeline);

  const guard = <R extends Request>(fn: (req: R, server: Server<undefined>) => Response | Promise<Response>) => {
    return async (req: R, server: Server<undefined>): Promise<Response> => {
      const requestId = crypto.randomUUID();
      const url = new URL(req.url);
      let res: Response;
      try {
        res = await fn(req, server);
      } catch (e) {
        deps.log.error('unhandled error', { requestId, path: url.pathname, err: e });
        res = internalError(requestId);
      }
      return decorate(res, { req, url, config: deps.config, route: null, requestId });
    };
  };

  const extra: BunRoutes = {};
  for (const [path, entry] of Object.entries(opts.extraRoutes ?? {})) {
    if (typeof entry === 'function') {
      extra[path] = guard(entry);
      continue;
    }
    const methods: Partial<Record<Serve.HTTPMethod, BunHandler>> = {};
    for (const [method, h] of Object.entries(entry) as [Serve.HTTPMethod, BunHandler | undefined][]) if (h) methods[method] = guard(h);
    extra[path] = methods;
  }

  return Bun.serve({
    hostname: opts.hostname,
    port: opts.port,
    development: false,
    maxRequestBodySize: MAX_REQUEST_BODY,
    routes: { ...extra, ...buildRoutes(handlers, deps, overrides) },
    fetch: guard(opts.fallback ?? (() => notFoundResponse())),
    // Bun calls this without the request: the headers step sees a synthetic GET of PUBLIC_URL.
    error: (err) => {
      const requestId = crypto.randomUUID();
      deps.log.error('unhandled error', { requestId, err });
      const res = internalError(requestId);
      try {
        const url = new URL(deps.config.publicUrl + '/');
        return decorate(res, { req: new Request(url.href), url, config: deps.config, route: null, requestId });
      } catch {
        return withHeader(res, 'X-Request-Id', requestId);
      }
    },
  });
}

