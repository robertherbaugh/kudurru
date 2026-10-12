// The HTTP seams: what a handler receives (RequestContext), the signed-in
// principal, the handler map keyed by RouteId, and the Pipeline of
// middleware stream A owns (headers, CORS, session resolution, CSRF, rate
// limits, client IP). server/http/router.ts runs them in this order:
//
//   request id + client IP → headers → CORS → body limit → JSON-only
//   content type → session → CSRF → rate limit → authorize → handler
//
// and turns every thrown error into a JSON response (errors.ts toResponse),
// so outer middleware (headers, CORS) also decorate error responses.

import type { CookieMap } from 'bun';
import type { Permission, Role } from '../../shared/permissions';
import type { RouteDef, RouteId, RouteParams } from '../../shared/routes';
import type { Config } from '../config';
import type { Db } from '../db/sql';
import type { SigningState } from '../licensing/types';
import type { Logger } from '../log';
import type { Mailer } from '../mail/types';

/** The signed-in user of a request. */
export interface Principal {
  readonly userId: string;
  /** Kept only when IdP-verified (users.email). */
  readonly email: string | null;
  readonly emailVerified: boolean;
  readonly name: string;
  /** Effective roles (idp ∪ granted), in ROLES order. */
  readonly roles: readonly Role[];
  readonly permissions: ReadonlySet<Permission>;
  can(permission: Permission): boolean;
}

/** The session row behind a request (never sent to the client except csrf through /auth/me). */
export interface SessionInfo {
  /** SHA-256 of the cookie secret (sessions.id_hash). */
  readonly idHash: Buffer;
  readonly userId: string;
  /** The X-Kudurru-CSRF value of this session. */
  readonly csrf: string;
  readonly authTime: Date;
  readonly createdAt: Date;
  readonly lastSeenAt: Date;
  readonly idleExpiresAt: Date;
  readonly expiresAt: Date;
}

export interface ResolvedSession {
  principal: Principal;
  session: SessionInfo;
}

/** What the server is built from (main.ts; makeTestApp in tests). */
export interface AppDeps {
  db: Db;
  config: Config;
  log: Logger;
  /** The signer (or why there is none) and the trusted keyring, decided at startup. */
  signing: SigningState;
  mailer: Mailer;
  /** The clock of RequestContext.now; default () => new Date() (setSystemTime works in tests). */
  clock?: () => Date;
}

/** Everything a handler needs about one request. */
export interface RequestContext<Id extends RouteId = RouteId> {
  readonly req: Request;
  readonly url: URL;
  /** The route's path parameters, URL-decoded by Bun. */
  readonly params: RouteParams<Id>;
  readonly query: URLSearchParams;
  /** The request's cookies (parsed; setting them here does nothing: return Set-Cookie headers). */
  readonly cookies: CookieMap;
  /** The client address after TRUSTED_PROXIES (Pipeline.clientIp); null when unknown. */
  readonly ip: string | null;
  /** The socket peer's address. */
  readonly peerIp: string | null;
  /** Random per request; in every log line, in X-Request-Id and in the hint of a 500. */
  readonly requestId: string;
  /** The User-Agent header, at most 512 characters. */
  readonly userAgent: string | null;
  /** When the request arrived; use it for every "now" of the request. */
  readonly now: Date;
  /** Set by session resolution on session and permission routes; null on anonymous and public routes. */
  principal: Principal | null;
  session: SessionInfo | null;
  readonly db: Db;
  readonly config: Config;
  /** Carries requestId and route. */
  readonly log: Logger;
  /** AppDeps.signing: ctx.signing.signer is null while signing is unavailable (ctx.signing.unavailableReason says why). */
  readonly signing: SigningState;
  readonly mailer: Mailer;
  readonly route: RouteDef<Id>;
}

/** Answers one route. Throw an HttpError (errors.ts fail) for error responses. */
export type Handler<Id extends RouteId = RouteId> = (ctx: RequestContext<Id>) => Promise<Response>;

/** One step of the pipeline: call next() to continue (it never throws: errors arrive as responses), or answer without it. */
export type Middleware = (ctx: RequestContext, next: () => Promise<Response>) => Promise<Response>;

/** A handler for every route (main.ts asserts it at startup). */
export type HandlerMap = { [K in RouteId]: Handler<K> };

/** Some handlers: what each stream exports (server/api/*.ts) and what tests pass. */
export type PartialHandlerMap = { [K in RouteId]?: Handler<K> };

/**
 * What the headers step sees. Also used for responses without a manifest
 * route: the router's own 404 and 405, the static files and SPA fallback
 * (ServerOptions extraRoutes and fallback), and the 500 of Bun's error hook,
 * which Bun calls without the request (req and url are then a synthetic GET
 * of PUBLIC_URL).
 */
export interface HeaderContext {
  readonly req: Request;
  readonly url: URL;
  readonly config: Config;
  /** null for responses without a manifest route (see above). */
  readonly route: RouteDef | null;
  readonly requestId: string;
}

/**
 * The pipeline steps stream A implements (headers.ts, cors.ts, sessions.ts,
 * csrf.ts, ratelimit.ts, ip.ts). Every step has a pass-through default
 * (router.ts defaultPipeline), so other streams can test before A lands.
 */
export interface Pipeline {
  /** The client address from the socket peer and, when the peer is a trusted proxy, X-Forwarded-For. Default: the peer. */
  clientIp(req: Request, peer: string | null, config: Config): string | null;
  /**
   * Adds the security headers (CSP, nosniff, …) to every response, and
   * Cache-Control no-store to API answers and downloads unless the response
   * already names a Cache-Control (static.ts sets its own). It may mutate
   * and return `res`; router.ts withHeader shows how to handle immutable
   * headers. Default: unchanged.
   */
  headers(res: Response, ctx: HeaderContext): Response;
  /** Runs on routes with `cors` only, OPTIONS preflights included. Default: OPTIONS → 204, otherwise next(). */
  cors: Middleware;
  /** Resolves the session cookie on session and permission routes. Default: auth/sessions.ts resolveSession. */
  session(ctx: RequestContext): Promise<ResolvedSession | null>;
  /**
   * Runs for unsafe methods on session and permission routes, after session
   * resolution: 403 csrf on a failed origin or token check. With no session
   * (ctx.session null) it calls next(), so the answer is 401 unauthenticated.
   * Default: next().
   */
  csrf: Middleware;
  /** Runs on every route before authorization. Default: next(). */
  rateLimit: Middleware;
}
