// Server-side sessions (design spec §6 "Sign-in"). The browser holds a
// random 32-byte secret in an HttpOnly, SameSite=Lax session cookie
// (`__Host-kls_session`, or `kls_session` when PUBLIC_URL is http); the
// sessions table holds its SHA-256 with the CSRF token, the user and two
// expiries: idle (SESSION_IDLE_TIMEOUT after the last request, bumped at
// most once a minute) and absolute (SESSION_LIFETIME after sign-in). A
// disabled user's sessions resolve to nothing. Roles are read from the
// users row on every request, so a role change applies at once; role and
// disable changes also delete the user's sessions (deleteUserSessions).

import { Cookie } from 'bun';
import { permissionsFor, effectiveRoles, type Permission, type Role } from '../../shared/permissions';
import type { Config } from '../config';
import { hashSecret, isSecretShaped, newSecret } from '../db/ids';
import { toInt, type Queryable } from '../db/sql';
import type { Principal, ResolvedSession, SessionInfo } from '../http/types';

/** The idle expiry moves at most this often (one UPDATE per minute per session). */
export const LAST_SEEN_INTERVAL_MS = 60_000;

type CookieConfig = Pick<Config, 'secureCookies'>;

/** The session cookie's name: `__Host-kls_session` when PUBLIC_URL is https, else `kls_session`. */
export function sessionCookieName(config: CookieConfig): string {
  return config.secureCookies ? '__Host-kls_session' : 'kls_session';
}

/** The sign-in binding cookie's name: `__Host-kls_login` when PUBLIC_URL is https, else `kls_login`. */
export function loginCookieName(config: CookieConfig): string {
  return config.secureCookies ? '__Host-kls_login' : 'kls_login';
}

/** The Set-Cookie value that stores a session: HttpOnly, SameSite=Lax, Path=/, Secure on https, no Max-Age (a browser-session cookie). */
export function serializeSessionCookie(config: CookieConfig, cookieValue: string): string {
  return new Cookie(sessionCookieName(config), cookieValue, { httpOnly: true, secure: config.secureCookies, sameSite: 'lax', path: '/' }).serialize();
}

/** The Set-Cookie value that removes the session cookie. */
export function clearSessionCookie(config: CookieConfig): string {
  return new Cookie(sessionCookieName(config), '', { httpOnly: true, secure: config.secureCookies, sameSite: 'lax', path: '/', maxAge: 0 }).serialize();
}

export interface NewSession {
  /** The cookie secret: set it with serializeSessionCookie, never store or log it. */
  cookieValue: string;
  csrf: string;
  idHash: Buffer;
  expiresAt: Date;
  idleExpiresAt: Date;
}

export interface CreateSessionOptions {
  userId: string;
  /** auth_time from the ID token, else now. */
  authTime: Date;
  ip: string | null;
  userAgent: string | null;
  config: Pick<Config, 'session'>;
  now?: Date;
}

/** Stores a new session and returns the cookie secret and the CSRF token. */
export async function createSession(db: Queryable, opts: CreateSessionOptions): Promise<NewSession> {
  const now = opts.now ?? new Date();
  const cookieValue = newSecret();
  const csrf = newSecret();
  const idHash = hashSecret(cookieValue);
  const expiresAt = new Date(now.getTime() + opts.config.session.lifetimeMs);
  const idleExpiresAt = new Date(Math.min(now.getTime() + opts.config.session.idleTimeoutMs, expiresAt.getTime()));
  await db`
    insert into sessions (id_hash, user_id, csrf, created_at, last_seen_at, idle_expires_at, expires_at, auth_time, ip, user_agent)
    values (${idHash}, ${opts.userId}, ${csrf}, ${now}, ${now}, ${idleExpiresAt}, ${expiresAt}, ${opts.authTime}, ${opts.ip}, ${opts.userAgent})`;
  return { cookieValue, csrf, idHash, expiresAt, idleExpiresAt };
}

/** The columns makePrincipal reads (users row). */
export interface PrincipalRow {
  id: string;
  email: string | null;
  email_verified: boolean;
  name: string;
  idp_roles: string[];
  granted_roles: string[];
  disabled_at: Date | null;
}

/** The Principal of a users row: effective roles (none while disabled) and their permissions. */
export function makePrincipal(user: PrincipalRow): Principal {
  const roles: readonly Role[] = effectiveRoles(user.idp_roles, user.granted_roles, user.disabled_at !== null);
  const permissions: ReadonlySet<Permission> = new Set(permissionsFor(roles));
  return {
    userId: user.id,
    email: user.email,
    emailVerified: user.email_verified,
    name: user.name,
    roles,
    permissions,
    can: (p: Permission) => permissions.has(p),
  };
}

interface SessionRow extends Omit<PrincipalRow, 'id'> {
  id_hash: Buffer;
  user_id: string;
  csrf: string;
  created_at: Date;
  last_seen_at: Date;
  idle_expires_at: Date;
  expires_at: Date;
  auth_time: Date;
}

/**
 * The session of a cookie value, or null when there is none, it expired
 * (idle or absolute) or its user is disabled. Moves the idle expiry when
 * the last bump is a minute old or more.
 */
export async function resolveSession(
  db: Queryable,
  cookieValue: string | null | undefined,
  now: Date,
  config: Pick<Config, 'session'>,
): Promise<ResolvedSession | null> {
  if (!isSecretShaped(cookieValue)) return null;
  const idHash = hashSecret(cookieValue);
  const rows = await db<SessionRow[]>`
    select s.id_hash, s.user_id, s.csrf, s.created_at, s.last_seen_at, s.idle_expires_at, s.expires_at, s.auth_time,
           u.email, u.email_verified, u.name, u.idp_roles, u.granted_roles, u.disabled_at
      from sessions s
      join users u on u.id = s.user_id
     where s.id_hash = ${idHash} and s.expires_at > ${now} and s.idle_expires_at > ${now}`;
  const row = rows[0];
  if (!row || row.disabled_at !== null) return null;
  let lastSeenAt = row.last_seen_at;
  let idleExpiresAt = row.idle_expires_at;
  if (now.getTime() - row.last_seen_at.getTime() >= LAST_SEEN_INTERVAL_MS) {
    lastSeenAt = now;
    idleExpiresAt = new Date(Math.min(now.getTime() + config.session.idleTimeoutMs, row.expires_at.getTime()));
    await db`update sessions set last_seen_at = ${lastSeenAt}, idle_expires_at = ${idleExpiresAt} where id_hash = ${idHash}`;
  }
  const session: SessionInfo = {
    idHash: row.id_hash,
    userId: row.user_id,
    csrf: row.csrf,
    authTime: row.auth_time,
    createdAt: row.created_at,
    lastSeenAt,
    idleExpiresAt,
    expiresAt: row.expires_at,
  };
  const principal = makePrincipal({ ...row, id: row.user_id });
  return { principal, session };
}

/** Deletes one session (logout). */
export async function deleteSession(db: Queryable, idHash: Buffer): Promise<void> {
  await db`delete from sessions where id_hash = ${idHash}`;
}

/** Deletes every session of a user except `exceptHash` (the acting session); returns how many went. */
export async function deleteUserSessions(db: Queryable, userId: string, exceptHash?: Buffer | null): Promise<number> {
  const rows = exceptHash
    ? await db<{ n: string }[]>`with d as (delete from sessions where user_id = ${userId} and id_hash <> ${exceptHash} returning 1) select count(*) as n from d`
    : await db<{ n: string }[]>`with d as (delete from sessions where user_id = ${userId} returning 1) select count(*) as n from d`;
  return toInt(rows[0]?.n);
}

/** Deletes expired sessions (the background job); returns how many went. */
export async function deleteExpiredSessions(db: Queryable, now: Date): Promise<number> {
  const rows = await db<{ n: string }[]>`
    with d as (delete from sessions where expires_at <= ${now} or idle_expires_at <= ${now} returning 1) select count(*) as n from d`;
  return toInt(rows[0]?.n);
}
