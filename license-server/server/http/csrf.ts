// CSRF protection (design spec §6 "CSRF and CORS"): two layers on every
// unsafe method of a session or permission route (the router calls this
// step only there, after session resolution):
//   (a) where the request comes from: Sec-Fetch-Site same-origin or none
//       passes and any other value fails (same-site included); without
//       Sec-Fetch-Site, an Origin must equal PUBLIC_URL's origin; a request
//       with neither header passes (old browsers, non-browser clients,
//       which still need (b));
//   (b) X-Kudurru-CSRF must equal the session's token (GET /auth/me hands
//       it to the SPA), compared in constant time.
// A port of Go's http.CrossOriginProtection plus kudurru's token header.
// Without a session the step passes, so the answer is 401, not 403.
// Failures answer 403 csrf and log csrf.denied (never the header values).

import type { Config } from '../config';
import { safeEqual } from '../db/ids';
import { HttpError } from './errors';
import type { Middleware } from './types';

/** The header the SPA echoes the session's CSRF token in. */
export const CSRF_HEADER = 'X-Kudurru-CSRF';

/** Why a request failed the origin layer, or null when it passes. */
export function originCheck(headers: Headers, publicOrigin: string): 'sec-fetch-site' | 'origin' | null {
  const site = headers.get('sec-fetch-site');
  if (site !== null) return site === 'same-origin' || site === 'none' ? null : 'sec-fetch-site';
  const origin = headers.get('origin');
  if (origin === null) return null;
  return origin === publicOrigin ? null : 'origin';
}

/** The Pipeline.csrf step. */
export function csrfStep(config: Pick<Config, 'publicOrigin'>): Middleware {
  const refuse = (): never => {
    throw new HttpError(403, 'csrf', 'The request was refused by the CSRF check. Reload the page and try again.');
  };
  return (ctx, next) => {
    if (!ctx.session) return next();
    const failed = originCheck(ctx.req.headers, config.publicOrigin);
    if (failed) {
      ctx.log.warn('csrf.denied', { reason: failed, userId: ctx.session.userId, secFetchSite: ctx.req.headers.get('sec-fetch-site')?.slice(0, 40) ?? null });
      refuse();
    }
    const token = ctx.req.headers.get(CSRF_HEADER);
    if (token === null || !safeEqual(token, ctx.session.csrf)) {
      ctx.log.warn('csrf.denied', { reason: token === null ? 'token-missing' : 'token-mismatch', userId: ctx.session.userId });
      refuse();
    }
    return next();
  };
}
