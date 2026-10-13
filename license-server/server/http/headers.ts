// Security headers (design spec §2) on every response the server sends:
// API answers, redirects, downloads, static files, the SPA and the 500 of
// Bun's error hook. The CSP is exactly the dashboard's web.DefaultCSP
// (web/handler.go): scripts only from the bundle, the SPA talks only to its
// own origin, no framing. HSTS only when PUBLIC_URL is https. API answers
// and downloads are never cached (Cache-Control: no-store) unless the
// handler named its own Cache-Control; static.ts names one for every file.

import type { Config } from '../config';
import type { HeaderContext } from './types';

/** web.DefaultCSP, word for word. */
export const CONTENT_SECURITY_POLICY =
  "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
  "img-src 'self' data:; font-src 'self'; connect-src 'self'; worker-src 'self'; " +
  "manifest-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'";

/** The HSTS value when PUBLIC_URL is https (one year; no includeSubDomains: the portal does not own its parent domain's policy). */
export const HSTS = 'max-age=31536000';

/** The headers every response carries, for this configuration. */
export function securityHeaders(config: Pick<Config, 'secureCookies'>): Record<string, string> {
  const h: Record<string, string> = {
    'Content-Security-Policy': CONTENT_SECURITY_POLICY,
    'X-Content-Type-Options': 'nosniff',
    'Referrer-Policy': 'same-origin',
    'X-Frame-Options': 'DENY',
    'Cross-Origin-Opener-Policy': 'same-origin',
  };
  if (config.secureCookies) h['Strict-Transport-Security'] = HSTS;
  return h;
}

/** Whether a response belongs to the API (a manifest route, or anything under /api): it gets no-store unless it says otherwise. */
function isApi(hctx: HeaderContext): boolean {
  if (hctx.route !== null) return true;
  const path = hctx.url.pathname;
  return path === '/api' || path.startsWith('/api/');
}

/**
 * Sets `headers` on the response (overwriting) and returns it; a response
 * whose headers are immutable (Response.redirect, fetch results) is copied
 * once first.
 */
export function setHeaders(res: Response, headers: Readonly<Record<string, string>>): Response {
  let out = res;
  try {
    for (const [k, v] of Object.entries(headers)) out.headers.set(k, v);
  } catch {
    out = new Response(res.body, res);
    for (const [k, v] of Object.entries(headers)) out.headers.set(k, v);
  }
  return out;
}

/** The Pipeline.headers step: the security headers, plus no-store on API answers without their own Cache-Control. */
export function headersStep(config: Pick<Config, 'secureCookies'>): (res: Response, hctx: HeaderContext) => Response {
  const base = securityHeaders(config);
  const withNoStore = { ...base, 'Cache-Control': 'no-store' };
  return (res, hctx) => setHeaders(res, isApi(hctx) && !res.headers.has('cache-control') ? withNoStore : base);
}
