// Where a failed sign-in lands (design spec §6 "Sign-in": "302 to
// /login?error=<short fixed message> (never plain text, never the cause)").
// The callback always redirects there. The sign-in start is a plain link in
// the UI, so it is a page load too: when it is refused (rate limit, IdP down,
// too many sign-ins in flight) a page load gets the same redirect and only a
// fetch caller gets the JSON error. The cause is only ever logged.
//
// The keys belong in shared/api.ts so the UI can word them without
// importing server code (frozen change request); until then they are here.

import { redirect } from '../http/errors';

/**
 * The fixed reasons of /login?error=<key> (the login page words them; no
 * cause ever reaches the browser):
 * - idp_error: the IdP answered with an error (the person canceled, the IdP refused);
 * - expired: the sign-in is unknown, used, expired or was started in another browser;
 * - failed: the code exchange or the ID token checks failed, or the server failed;
 * - idp_unavailable: the IdP cannot be reached;
 * - disabled: the account is disabled in the portal;
 * - no_role: the account maps to no role (DEFAULT_ROLE=none);
 * - rate_limited: too many sign-in requests from this network (Retry-After says when to retry);
 * - login_busy: too many sign-ins are in flight on the server.
 */
export const LOGIN_ERROR_KEYS = ['idp_error', 'expired', 'failed', 'idp_unavailable', 'disabled', 'no_role', 'rate_limited', 'login_busy'] as const;
export type LoginErrorKey = (typeof LOGIN_ERROR_KEYS)[number];

export function isLoginErrorKey(v: unknown): v is LoginErrorKey {
  return typeof v === 'string' && (LOGIN_ERROR_KEYS as readonly string[]).includes(v);
}

/** /login?error=<key>, plus next= when it is not '/' (`next` must already have passed safeNext). */
export function loginErrorLocation(key: LoginErrorKey, next = '/'): string {
  const q = new URLSearchParams({ error: key });
  if (next !== '/') q.set('next', next);
  return `/login?${q.toString()}`;
}

/** The 302 to /login?error=<key>, never cached; `headers` adds to it (Retry-After). */
export function loginErrorRedirect(key: LoginErrorKey, next = '/', headers: Record<string, string> = {}): Response {
  return redirect(loginErrorLocation(key, next), 302, { ...headers, 'Cache-Control': 'no-store' });
}

/**
 * Whether the request is a top-level page load rather than a fetch:
 * Sec-Fetch-Mode navigate, or, from a client that does not send
 * Sec-Fetch-* at all, an Accept naming text/html.
 */
export function isPageLoad(req: Request): boolean {
  const mode = req.headers.get('sec-fetch-mode');
  if (mode !== null) return mode === 'navigate';
  return (req.headers.get('accept') ?? '').toLowerCase().includes('text/html');
}
