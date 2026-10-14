// CORS for the kudurru.io form (design spec §6 "CSRF and CORS"; the router
// runs this step only on routes with `cors: 'publicForm'`, their OPTIONS
// preflight included). Allowed origins are exactly PUBLIC_FORM_ORIGINS:
// - an allowed Origin gets Access-Control-Allow-Origin echoing it (never
//   *, never credentials); the preflight also gets Allow-Methods POST,
//   OPTIONS, Allow-Headers Content-Type, Accept and Max-Age 600;
// - any other Origin is refused with 403 forbidden_origin and no CORS
//   headers, before the body is read;
// - no Origin (curl, a server) passes without Allow-Origin.
// Every response of the route that is not refused, errors included,
// exposes Retry-After (the 429 of the per-IP limit) and varies on Origin.

import type { Config } from '../config';
import { HttpError } from './errors';
import { setHeaders } from './headers';
import type { Middleware } from './types';

export const CORS_ALLOW_METHODS = 'POST, OPTIONS';
export const CORS_ALLOW_HEADERS = 'Content-Type, Accept';
export const CORS_MAX_AGE = '600';

/** Adds Origin to Vary, keeping what the response already varies on. */
function varyOrigin(res: Response): Response {
  const vary = res.headers.get('vary');
  if (vary && vary.split(',').some((v) => v.trim().toLowerCase() === 'origin')) return res;
  return setHeaders(res, { Vary: vary ? `${vary}, Origin` : 'Origin' });
}

/** The Pipeline.cors step for PUBLIC_FORM_ORIGINS. */
export function corsStep(config: Pick<Config, 'publicForm'>): Middleware {
  const allowed = new Set(config.publicForm.origins);
  return async (ctx, next) => {
    const origin = ctx.req.headers.get('origin');
    if (origin !== null && !allowed.has(origin)) {
      ctx.log.warn('cors.denied', { origin: origin.slice(0, 200) });
      // Thrown, so the router answers the JSON envelope; nothing below adds CORS headers to it.
      throw new HttpError(403, 'forbidden_origin', 'This form may not be sent from that site.');
    }
    const res = await next();
    const headers: Record<string, string> = { 'Access-Control-Expose-Headers': 'Retry-After' };
    if (origin !== null) {
      headers['Access-Control-Allow-Origin'] = origin;
      if (ctx.req.method === 'OPTIONS') {
        headers['Access-Control-Allow-Methods'] = CORS_ALLOW_METHODS;
        headers['Access-Control-Allow-Headers'] = CORS_ALLOW_HEADERS;
        headers['Access-Control-Max-Age'] = CORS_MAX_AGE;
      }
    }
    return varyOrigin(setHeaders(res, headers));
  };
}
