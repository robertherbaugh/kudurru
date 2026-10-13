// The portal's request pipeline (server/http/types.ts Pipeline): the real
// steps behind the router's pass-through defaults. main.ts passes
// createPipeline(config, log) to startServer; tests pass it to
// makeTestApp({ pipeline }) to run the production behavior.
//
//   clientIp   ip.ts         peer, or rightmost untrusted X-Forwarded-For hop
//   headers    headers.ts    CSP & co. on every response, no-store on the API
//   cors       cors.ts       PUBLIC_FORM_ORIGINS on the public form route
//   session    sessions.ts   the session cookie (router default)
//   csrf       csrf.ts       Sec-Fetch-Site/Origin plus X-Kudurru-CSRF
//   rateLimit  ratelimit.ts  sign-in start and callback, 20/min per address

import type { RouteId } from '../../shared/routes';
import { resolveSession, sessionCookieName } from '../auth/sessions';
import type { Config } from '../config';
import type { Logger } from '../log';
import { corsStep } from './cors';
import { csrfStep } from './csrf';
import { headersStep } from './headers';
import { clientIpResolver } from './ip';
import { DEFAULT_RATE_LIMITS, rateLimitStep, type RateLimit } from './ratelimit';
import type { Pipeline } from './types';

export interface PipelineOptions {
  /** Default DEFAULT_RATE_LIMITS (tests lower them). */
  rateLimits?: Partial<Record<RouteId, RateLimit>>;
}

/** Every step of the pipeline for this configuration. Each call has its own rate-limit state. */
export function createPipeline(config: Config, log: Logger, opts: PipelineOptions = {}): Pipeline {
  const clientIp = clientIpResolver(config, { log });
  return {
    clientIp: (req, peer) => clientIp(req, peer),
    headers: headersStep(config),
    cors: corsStep(config),
    session: (ctx) => resolveSession(ctx.db, ctx.cookies.get(sessionCookieName(ctx.config)), ctx.now, ctx.config),
    csrf: csrfStep(config),
    rateLimit: rateLimitStep(opts.rateLimits ?? DEFAULT_RATE_LIMITS),
  };
}
