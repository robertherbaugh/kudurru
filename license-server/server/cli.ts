// The grant-superadmin command (design spec §5 "First superadmin and
// break-glass"):
//
//   bun dist/server.js grant-superadmin --email <verified email of an existing user> [--revoke]
//   bun dist/server.js grant-superadmin --issuer <iss> --subject <sub> [--revoke]
//
// The person signs in once first (the users row records them even when no
// role maps, DEFAULT_ROLE=none). The command prints the matched user,
// adds superadmin to (or with --revoke removes it from) granted_roles,
// ends the user's sessions (a role change always does) and writes a
// user.roles_changed audit event with actor_label "cli". --email matches
// only an IdP-verified address and refuses when several users hold it
// (one person at two IdP connectors is two users): use --issuer and
// --subject then. The API's guards (last superadmin, self-demotion) do not
// apply: this is the way back in when they lock everyone out.

import { ROLES, type Role } from '../shared/permissions';
import { deleteUserSessions } from './auth/sessions';
import { textArray, withTx, type Db } from './db/sql';
import { normalizeEmail } from './http/body';
import { audit } from './services/audit';
import type { Actor } from './services/types';

export const GRANT_USAGE = 'usage: grant-superadmin (--email <verified email> | --issuer <issuer> --subject <subject>) [--revoke]';

/** A command line the command cannot run (exit 2 with the usage). */
export class UsageError extends Error {
  override readonly name = 'UsageError';
}

export interface GrantArgs {
  email: string | null;
  issuer: string | null;
  subject: string | null;
  revoke: boolean;
}

/** Parses the arguments after the command name (`--flag value` or `--flag=value`). */
export function parseGrantArgs(argv: readonly string[]): GrantArgs {
  const out: GrantArgs = { email: null, issuer: null, subject: null, revoke: false };
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i]!;
    if (arg === '--revoke') {
      out.revoke = true;
      continue;
    }
    const m = /^--(email|issuer|subject)(?:=(.*))?$/.exec(arg);
    if (!m) throw new UsageError(`unknown argument ${JSON.stringify(arg)}`);
    const name = m[1] as 'email' | 'issuer' | 'subject';
    let value = m[2];
    if (value === undefined) {
      value = argv[i + 1];
      i++;
    }
    if (value === undefined || value.trim() === '' || value.startsWith('--')) throw new UsageError(`--${name} needs a value`);
    if (out[name] !== null) throw new UsageError(`--${name} is given twice`);
    out[name] = value.trim();
  }
  const byEmail = out.email !== null;
  const bySubject = out.issuer !== null || out.subject !== null;
  if (byEmail === bySubject) throw new UsageError('give either --email or --issuer and --subject');
  if (bySubject && (out.issuer === null || out.subject === null)) throw new UsageError('--issuer and --subject go together');
  return out;
}

interface UserRow {
  id: string;
  issuer: string;
  subject: string;
  email: string | null;
  email_verified: boolean;
  name: string;
  idp_roles: string[];
  granted_roles: string[];
  disabled_at: Date | null;
  last_login_at: Date | null;
}

function describe(u: UserRow): string[] {
  const roles = (xs: string[]) => (xs.length > 0 ? xs.join(', ') : '(none)');
  return [
    `user     ${u.id}`,
    `name     ${u.name || '(none)'}`,
    `email    ${u.email ? `${u.email} (verified)` : '(none verified)'}`,
    `issuer   ${u.issuer}`,
    `subject  ${u.subject}`,
    `roles    from the IdP: ${roles(u.idp_roles)}; granted: ${roles(u.granted_roles)}`,
    `status   ${u.disabled_at ? `disabled since ${u.disabled_at.toISOString()}` : 'enabled'}; last sign-in ${u.last_login_at ? u.last_login_at.toISOString() : 'never'}`,
  ];
}

export interface CliIo {
  out(line: string): void;
  err(line: string): void;
}

/** The CLI actor of audit events. */
export const CLI_ACTOR: Actor = { kind: 'system', label: 'cli' };

/**
 * Runs grant-superadmin against the database and returns the exit code:
 * 0 done (or nothing to change), 1 no such user or an ambiguous email.
 */
export async function grantSuperadmin(db: Db, args: GrantArgs, io: CliIo, now: Date = new Date()): Promise<number> {
  const matches =
    args.email !== null
      ? await db<UserRow[]>`
          select id, issuer, subject, email, email_verified, name, idp_roles, granted_roles, disabled_at, last_login_at
            from users where email = ${normalizeEmail(args.email)} and email_verified order by created_at`
      : await db<UserRow[]>`
          select id, issuer, subject, email, email_verified, name, idp_roles, granted_roles, disabled_at, last_login_at
            from users where issuer = ${args.issuer} and subject = ${args.subject}`;
  if (matches.length === 0) {
    io.err(
      args.email !== null
        ? `no user holds ${normalizeEmail(args.email)} as a verified email: sign in once with that account first (or use --issuer and --subject)`
        : `no user has issuer ${JSON.stringify(args.issuer)} and subject ${JSON.stringify(args.subject)}: sign in once with that account first`,
    );
    return 1;
  }
  if (matches.length > 1) {
    io.err(`${matches.length} users hold ${normalizeEmail(args.email ?? '')} as a verified email; nothing was changed. Pick one with --issuer and --subject:`);
    for (const u of matches) {
      for (const line of describe(u)) io.err(`  ${line}`);
      io.err('');
    }
    return 1;
  }

  const target = matches[0]!;
  return await withTx(db, async (tx) => {
    const locked = (
      await tx<UserRow[]>`
        select id, issuer, subject, email, email_verified, name, idp_roles, granted_roles, disabled_at, last_login_at
          from users where id = ${target.id} for update`
    )[0];
    if (!locked) {
      io.err(`user ${target.id} disappeared`);
      return 1;
    }
    for (const line of describe(locked)) io.out(line);
    const before = ROLES.filter((r) => locked.granted_roles.includes(r));
    const after: Role[] = args.revoke ? before.filter((r) => r !== 'superadmin') : ROLES.filter((r) => r === 'superadmin' || before.includes(r));
    if (after.length === before.length) {
      io.out(args.revoke ? 'superadmin is not granted to this user; nothing changed' : 'superadmin is already granted to this user; nothing changed');
      if (args.revoke && locked.idp_roles.includes('superadmin')) io.out('note: the IdP maps this user to superadmin (ROLE_SUPERADMIN_GROUPS); change the group membership at the IdP');
      return 0;
    }
    await tx`update users set granted_roles = ${textArray(tx, after)} where id = ${locked.id}`;
    const ended = await deleteUserSessions(tx, locked.id);
    await audit(tx, {
      actor: CLI_ACTOR,
      action: 'user.roles_changed',
      targetType: 'user',
      targetId: locked.id,
      details: { grantedRoles: { before, after }, change: args.revoke ? 'superadmin revoked' : 'superadmin granted', via: 'grant-superadmin', sessionsEnded: ended, at: now.toISOString() },
    });
    io.out(`${args.revoke ? 'revoked superadmin from' : 'granted superadmin to'} ${locked.id}; granted roles now ${after.length > 0 ? after.join(', ') : '(none)'}; ${ended} session(s) ended`);
    if (locked.disabled_at) io.out('note: the user is disabled; a disabled user has no roles until a superadmin enables them');
    if (args.revoke && locked.idp_roles.includes('superadmin')) io.out('note: the IdP still maps this user to superadmin (ROLE_SUPERADMIN_GROUPS); change the group membership at the IdP');
    return 0;
  });
}
