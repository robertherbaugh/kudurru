// Sign-in routes (design spec §6 "Sign-in", §8): authConfig, authLogin,
// authCallback, authMe and authLogout, built on server/auth (oidc.ts for
// the IdP, identity.ts for claims → user and roles, linking.ts for
// MEMBER_AUTO_LINK, sessions.ts for the cookie and the row).
//
//   GET  /api/v1/auth/login?next=   SafeNext, cap on sign-ins in flight
//                                   (503 login_busy), binding cookie,
//                                   oidc_logins row, 302 to the IdP; a page
//                                   load that cannot start gets a 302 to
//                                   /login?error=<key>&next= instead of the
//                                   JSON error (the button is a plain link)
//   GET  /api/v1/auth/callback      single-use state bound to the cookie,
//                                   code exchange, user, roles, session,
//                                   audit auth.login, 302 to next; every
//                                   failure: log the cause, 302 to
//                                   /login?error=<LoginErrorKey>
//   GET  /api/v1/auth/me            Me (the CSRF token included)
//   POST /api/v1/auth/logout        delete the session, clear the cookie, 204
//
// The router rate-limits login and callback per address (ratelimit.ts, which
// redirects the same way) and never logs query strings; nothing here logs
// the code, state, tokens or cookies.

import { Cookie } from 'bun';
import { timingSafeEqual } from 'node:crypto';
import type { AuthConfig, Me } from '../../shared/api';
import { effectiveRoles, permissionsFor } from '../../shared/permissions';
import { mapIdpRoles, identityFromClaims, upsertUser } from '../auth/identity';
import { autoLinkOnSignIn } from '../auth/linking';
import { isPageLoad, loginErrorRedirect, type LoginErrorKey } from '../auth/loginErrors';
import { consumePendingLogin, countPendingLogins, describeError, insertPendingLogin, LOGIN_TTL_MS, LoginError, MAX_PENDING_LOGINS, oidcClientFor } from '../auth/oidc';
import { locationOf, safeNext } from '../auth/safeNext';
import { clearSessionCookie, createSession, deleteSession, loginCookieName, serializeSessionCookie, sessionCookieName } from '../auth/sessions';
import type { Config } from '../config';
import { hashSecret, isSecretShaped, newSecret } from '../db/ids';
import { HttpError, json, noContent, redirect } from '../http/errors';
import type { PartialHandlerMap, RequestContext } from '../http/types';
import { auditCtx } from '../services/audit';
import { actorFromCtx, inTransaction, userLabel, type Actor } from '../services/types';

/** The binding cookie of the sign-in: HttpOnly, SameSite=Lax, Path=/, Secure and __Host- on https, 10 minutes. */
export function serializeLoginCookie(config: Pick<Config, 'secureCookies'>, value: string): string {
  return new Cookie(loginCookieName(config), value, {
    httpOnly: true,
    secure: config.secureCookies,
    sameSite: 'lax',
    path: '/',
    maxAge: LOGIN_TTL_MS / 1000,
  }).serialize();
}

function sameHash(a: Buffer, b: Buffer): boolean {
  return a.length === b.length && timingSafeEqual(a, b);
}

/** The callback after the state row: everything that can fail with a LoginError. Returns the redirect. */
async function completeSignIn(ctx: RequestContext<'authCallback'>, state: string): Promise<Response> {
  const login = await consumePendingLogin(ctx.db, state);
  if (!login) throw new LoginError('expired', 'unknown or already used state');
  if (login.expiresAt.getTime() <= ctx.now.getTime()) throw new LoginError('expired', 'the sign-in expired');
  const binding = ctx.cookies.get(loginCookieName(ctx.config));
  if (!isSecretShaped(binding)) throw new LoginError('expired', 'no binding cookie');
  if (!sameHash(hashSecret(binding), login.bindingHash)) throw new LoginError('expired', 'the binding cookie does not match');

  const verified = await oidcClientFor(ctx.config).exchange(ctx.log, ctx.url.search, { state: login.state, nonce: login.nonce, codeVerifier: login.codeVerifier });
  const identity = identityFromClaims(verified.claims, ctx.config.oidc.groupsClaim, ctx.now);
  const mapped = mapIdpRoles(identity, verified.claims, ctx.config);
  if (mapped.adminGroupsIgnored) ctx.log.info('admin groups ignored: OIDC_ADMIN_CLAIM_RULES did not match', { issuer: identity.issuer, subject: identity.subject });

  const system: Actor = { kind: 'system', label: 'sign-in' };
  const outcome = await inTransaction(ctx, system, async (s) => {
    const user = await upsertUser(s.tx, identity, mapped.roles, ctx.now);
    if (user.disabledAt !== null) return { kind: 'refused' as const, key: 'disabled' as const, user };
    const roles = effectiveRoles(user.idpRoles, user.grantedRoles);
    if (roles.length === 0) return { kind: 'refused' as const, key: 'no_role' as const, user };
    await s.tx`update users set last_login_at = ${ctx.now} where id = ${user.id}`;
    // A session the browser still had is ended: one sign-in, one session.
    const previous = ctx.cookies.get(sessionCookieName(ctx.config));
    if (isSecretShaped(previous)) await deleteSession(s.tx, hashSecret(previous));
    const actor: Actor = { kind: 'user', userId: user.id, label: userLabel(user) };
    const linked = await autoLinkOnSignIn({ ...s, actor }, ctx.config, user);
    const session = await createSession(s.tx, {
      userId: user.id,
      authTime: identity.authTime ?? ctx.now,
      ip: ctx.ip,
      userAgent: ctx.userAgent,
      config: ctx.config,
      now: ctx.now,
    });
    await auditCtx(
      { ...s, actor },
      {
        action: 'auth.login',
        targetType: 'user',
        targetId: user.id,
        details: { issuer: user.issuer, subject: user.subject, roles, idpRoles: user.idpRoles, firstSignIn: user.created, linkedCustomers: linked },
      },
    );
    return { kind: 'signed-in' as const, user, session };
  });
  if (outcome.kind === 'refused') {
    throw new LoginError(outcome.key, outcome.key === 'disabled' ? 'the user is disabled' : 'no role maps to this account', { cause: { userId: outcome.user.id } });
  }

  ctx.log.info('signed in', { userId: outcome.user.id, created: outcome.user.created });
  return redirect(locationOf(login.next), 302, [
    ['Set-Cookie', serializeSessionCookie(ctx.config, outcome.session.cookieValue)],
    ['Cache-Control', 'no-store'],
  ]);
}

/** The sign-in start: the cap, discovery, the binding cookie, the oidc_logins row, the 302 to the IdP. */
async function startSignIn(ctx: RequestContext<'authLogin'>, next: string): Promise<Response> {
  if ((await countPendingLogins(ctx.db, ctx.now)) > MAX_PENDING_LOGINS) {
    ctx.log.warn('sign-in refused: too many sign-ins in flight', { max: MAX_PENDING_LOGINS });
    throw new HttpError(503, 'login_busy', 'Too many sign-ins are in progress. Try again in a few minutes.');
  }
  const client = oidcClientFor(ctx.config);
  const state = newSecret();
  const nonce = newSecret();
  const codeVerifier = newSecret();
  // Discovery first (503 idp_unavailable), so nothing is stored for an IdP that is down.
  const url = await client.authorizationUrl(ctx.log, { state, nonce, codeVerifier });
  const existing = ctx.cookies.get(loginCookieName(ctx.config));
  const binding = isSecretShaped(existing) ? existing : newSecret();
  await insertPendingLogin(ctx.db, { state, bindingHash: hashSecret(binding), nonce, codeVerifier, next, expiresAt: new Date(ctx.now.getTime() + LOGIN_TTL_MS) }, ctx.now);
  return redirect(url.href, 302, [
    ['Set-Cookie', serializeLoginCookie(ctx.config, binding)],
    ['Cache-Control', 'no-store'],
  ]);
}

/** The /login?error= key of a sign-in start that threw: the two expected refusals by name, anything else 'failed'. */
function startRefusalKey(e: unknown): LoginErrorKey {
  if (e instanceof HttpError && (e.key === 'idp_unavailable' || e.key === 'login_busy')) return e.key;
  return 'failed';
}

export const authHandlers: PartialHandlerMap = {
  authConfig: (ctx) => {
    const body: AuthConfig = { displayName: ctx.config.oidc.displayName, devMode: ctx.config.devMode };
    return Promise.resolve(json(body));
  },

  authLogin: async (ctx) => {
    const next = safeNext(ctx.query.get('next'));
    try {
      return await startSignIn(ctx, next);
    } catch (e) {
      // A fetch caller gets the JSON error; a page load lands on the login page with the fixed key.
      if (!isPageLoad(ctx.req)) throw e;
      const key = startRefusalKey(e);
      if (key === 'failed') ctx.log.error('sign-in start failed', { err: e });
      return loginErrorRedirect(key, next);
    }
  },

  authCallback: async (ctx) => {
    const q = ctx.query;
    let key: LoginErrorKey;
    try {
      const state = q.get('state');
      const idpError = q.get('error');
      if (idpError !== null) {
        // Single use even when the IdP said no.
        if (state) await consumePendingLogin(ctx.db, state);
        throw new LoginError('idp_error', 'the IdP answered with an error', { cause: { error: idpError.slice(0, 100), description: (q.get('error_description') ?? '').slice(0, 300) } });
      }
      if (!state || !q.get('code')) throw new LoginError('expired', 'the callback has no state or code');
      return await completeSignIn(ctx, state);
    } catch (e) {
      if (e instanceof LoginError) {
        key = e.key;
        ctx.log.warn('auth.failed', { key: e.key, reason: e.reason, cause: e.cause instanceof Error ? describeError(e.cause) : e.cause });
      } else {
        key = 'failed';
        ctx.log.error('auth.failed', { key, reason: 'unexpected error', err: e });
      }
    }
    return loginErrorRedirect(key);
  },

  authMe: (ctx) => {
    const p = ctx.principal;
    const s = ctx.session;
    if (!p || !s) throw new HttpError(401, 'unauthenticated', 'Sign in to continue.');
    const body: Me = {
      user: { id: p.userId, name: p.name, email: p.email, emailVerified: p.emailVerified },
      roles: [...p.roles],
      permissions: permissionsFor(p.roles),
      csrf: s.csrf,
      devMode: ctx.config.devMode,
    };
    return Promise.resolve(json(body));
  },

  authLogout: async (ctx) => {
    const s = ctx.session;
    if (!s) throw new HttpError(401, 'unauthenticated', 'Sign in to continue.');
    await inTransaction(ctx, actorFromCtx(ctx), async (svc) => {
      await deleteSession(svc.tx, s.idHash);
      await auditCtx(svc, { action: 'auth.logout', targetType: 'user', targetId: s.userId });
    });
    return noContent({ 'Set-Cookie': clearSessionCookie(ctx.config) });
  },
};
