// Users (design spec §5): the people who signed in, their roles and
// sessions. Effective roles are idp_roles ∪ granted_roles (none while
// disabled); only granted roles and the disabled flag change here
// (users.write), idp_roles are the IdP's. Guards: a superadmin cannot
// remove their own superadmin grant or disable themselves, and the last
// enabled superadmin cannot lose superadmin (granted or idp) through the
// API. Role and disable changes end the user's sessions except the acting
// one. Changes are serialised by an advisory lock so two superadmins cannot
// demote each other at the same moment.

import type { Items, UpdateUserBody, User, UserDetail } from '../../shared/api';
import { effectiveRoles, isRole, ROLES, type Role } from '../../shared/permissions';
import { deleteUserSessions } from '../auth/sessions';
import { maybeOne, textArray, toInt, type Queryable } from '../db/sql';
import { fail, notFound } from '../http/errors';
import { auditCtx } from './audit';
import { containsPattern } from './customers';
import type { ServiceCtx } from './types';

/** The advisory lock of role and disable changes. */
const USER_ROLES_LOCK = 7407;

/** A users row. */
export interface UserRow {
  id: string;
  issuer: string;
  subject: string;
  email: string | null;
  email_verified: boolean;
  name: string;
  idp_roles: string[];
  granted_roles: string[];
  disabled_at: Date | null;
  created_at: Date;
  last_login_at: Date | null;
}

function roles(list: readonly string[]): Role[] {
  return ROLES.filter((r) => list.includes(r));
}

export function userDto(r: UserRow): User {
  return {
    id: r.id,
    name: r.name,
    email: r.email,
    emailVerified: r.email_verified,
    issuer: r.issuer,
    subject: r.subject,
    idpRoles: roles(r.idp_roles),
    grantedRoles: roles(r.granted_roles),
    roles: effectiveRoles(r.idp_roles, r.granted_roles, r.disabled_at !== null),
    disabled: r.disabled_at !== null,
    disabledAt: r.disabled_at ? r.disabled_at.toISOString() : null,
    createdAt: r.created_at.toISOString(),
    lastLoginAt: r.last_login_at ? r.last_login_at.toISOString() : null,
  };
}

/** GET /users: q matches the name and the email (at most 500, by name). */
export async function listUsers(q: Queryable, search: string | undefined): Promise<Items<User>> {
  const pattern = search ? containsPattern(search) : null;
  const rows = pattern
    ? await q<UserRow[]>`select * from users where name ilike ${pattern} escape '\\' or email ilike ${pattern} escape '\\' order by lower(name), id limit 500`
    : await q<UserRow[]>`select * from users order by lower(name), id limit 500`;
  return { items: rows.map(userDto) };
}

/** GET /users/:id: the user, their memberships and their unexpired sessions. */
export async function getUserDetail(q: Queryable, id: string, now: Date): Promise<UserDetail> {
  const row = maybeOne(await q<UserRow[]>`select * from users where id = ${id}`);
  if (!row) notFound('There is no such user.');
  const memberships = await q<{ customer_id: string; name: string; kind: 'person' | 'organization'; email: string; linked_at: Date | null }[]>`
    select m.customer_id, c.name, c.kind, m.email, m.linked_at from customer_members m join customers c on c.id = m.customer_id
     where m.user_id = ${id} order by lower(c.name), c.id`;
  const sessions = await q<{ n: string }[]>`select count(*) as n from sessions where user_id = ${id} and expires_at > ${now} and idle_expires_at > ${now}`;
  return {
    ...userDto(row),
    memberships: memberships.map((m) => ({ customer: { id: m.customer_id, name: m.name, kind: m.kind }, email: m.email, linkedAt: m.linked_at ? m.linked_at.toISOString() : null })),
    activeSessions: toInt(sessions[0]?.n),
  };
}

async function lockUser(ctx: Pick<ServiceCtx, 'tx'>, id: string): Promise<UserRow> {
  const row = maybeOne(await ctx.tx<UserRow[]>`select * from users where id = ${id} for update`);
  if (!row) notFound('There is no such user.');
  return row;
}

/** Who acts: their user id and the session to keep. */
export interface ActingUser {
  userId: string;
  /** sessions.id_hash of the acting session (kept when the acting user changes themselves). */
  sessionHash: Buffer | null;
}

/** PATCH /users/:id: granted roles and/or disabled, with the guards of spec §5. */
export async function updateUser(ctx: ServiceCtx, id: string, body: UpdateUserBody, acting: ActingUser): Promise<void> {
  await ctx.tx`select pg_advisory_xact_lock(${USER_ROLES_LOCK})`;
  const user = await lockUser(ctx, id);
  const self = id === acting.userId;
  const fromGranted = roles(user.granted_roles);
  const toGranted = body.grantedRoles !== undefined ? roles(body.grantedRoles.filter(isRole)) : fromGranted;
  const wasDisabled = user.disabled_at !== null;
  const disable = body.disabled ?? wasDisabled;

  if (self && fromGranted.includes('superadmin') && !toGranted.includes('superadmin')) fail('conflict', 'You cannot remove your own superadmin role. Ask another superadmin.');
  if (self && disable && !wasDisabled) fail('conflict', 'You cannot disable yourself.');
  const wasSuper = effectiveRoles(user.idp_roles, fromGranted, wasDisabled).includes('superadmin');
  const willBeSuper = effectiveRoles(user.idp_roles, toGranted, disable).includes('superadmin');
  if (wasSuper && !willBeSuper) {
    const others = await ctx.tx<{ n: string }[]>`
      select count(*) as n from users
       where id <> ${id} and disabled_at is null and ('superadmin' = any(idp_roles) or 'superadmin' = any(granted_roles))`;
    if (toInt(others[0]?.n) === 0) fail('conflict', 'This is the last enabled superadmin. Make someone else a superadmin first.');
  }

  const rolesChanged = JSON.stringify(fromGranted) !== JSON.stringify(toGranted);
  const disabledChanged = disable !== wasDisabled;
  if (!rolesChanged && !disabledChanged) return;
  await ctx.tx`
    update users set granted_roles = ${textArray(ctx.tx, toGranted)}, disabled_at = ${disable ? (user.disabled_at ?? ctx.now) : null}
     where id = ${id}`;
  const revoked = await deleteUserSessions(ctx.tx, id, acting.sessionHash);
  if (rolesChanged) await auditCtx(ctx, { action: 'user.roles_changed', targetType: 'user', targetId: id, details: { from: fromGranted, to: toGranted, sessionsRevoked: revoked } });
  if (disabledChanged) await auditCtx(ctx, { action: disable ? 'user.disabled' : 'user.enabled', targetType: 'user', targetId: id, details: { sessionsRevoked: revoked } });
}

/** POST /users/:id/revoke-sessions: ends the user's sessions (except the acting one). */
export async function revokeUserSessions(ctx: ServiceCtx, id: string, acting: ActingUser): Promise<number> {
  await lockUser(ctx, id);
  const revoked = await deleteUserSessions(ctx.tx, id, acting.sessionHash);
  await auditCtx(ctx, { action: 'user.sessions_revoked', targetType: 'user', targetId: id, details: { revoked } });
  return revoked;
}
