// The service seam: domain code (server/services/*.ts) never sees the
// request; it throws HttpError (http/errors.ts fail) for the answers it
// means, such as 409 license_withdrawn or 503 signing_unavailable
// (requireSigner). A handler turns its RequestContext into a ServiceCtx with
// inTransaction and calls services with it; the CLI and background jobs
// build one with a system actor. Everything a service does happens in
// ctx.tx, one transaction per request, and work that must wait for the
// commit (mail) is queued with ctx.afterCommit: it runs only once the
// transaction committed, and a failure there is logged, not thrown (the
// key stays in the portal and an admin can email it again).

import type { Config } from '../config';
import { withTx, type Db, type Tx } from '../db/sql';
import { fail } from '../http/errors';
import type { RequestContext } from '../http/types';
import type { Signer, SigningState } from '../licensing/types';
import type { Logger } from '../log';
import type { Mailer } from '../mail/types';

/** Who does something: a signed-in user, the public form, or the system (CLI, jobs). */
export type Actor =
  | {
      kind: 'user';
      userId: string;
      /** "Name (email)": stored next to the id so the record survives renames. */
      label: string;
    }
  | {
      kind: 'public';
      /** The address the public form gave (after confirmation). */
      email: string;
      ip: string | null;
      userAgent: string | null;
    }
  | {
      kind: 'system';
      /** "cli", "system", "scheduler", "approval link (<address>)". */
      label: string;
      /** The keys it signs count against ISSUE_MAX_PER_ACTOR_PER_DAY by label (an approval link that names no single admin; licenses.ts checkIssuingQuota). */
      issuingQuota?: boolean;
    };

/**
 * Which rows a read may see. member: only customers the user is linked to
 * (customer_members.user_id), enforced in the SQL of every self.* query;
 * all: the back office (licenses.read and friends).
 */
export type Scope = { kind: 'all' } | { kind: 'member'; userId: string };

/** What every service function receives. */
export interface ServiceCtx {
  tx: Tx;
  actor: Actor;
  /** The request's time; use it for every "now" (iat, created_at, expiry checks). */
  now: Date;
  /** The signer (requireSigner) and the trusted keyring (import). */
  signing: SigningState;
  mailer: Mailer;
  config: Config;
  log: Logger;
  requestId?: string;
  ip?: string | null;
  userAgent?: string | null;
  /** Queue work for after the commit (mail). Runs in order; failures are logged. Dropped on rollback. */
  afterCommit(fn: () => Promise<void>): void;
}

/**
 * The signer of a key-producing action, or 503 signing_unavailable with the
 * reason (no key configured, …). Call it before any write so nothing is
 * stored when no key can be signed. The public form answers its own 503
 * unavailable instead (the frozen contract).
 */
export function requireSigner(ctx: Pick<ServiceCtx, 'signing'>): Signer {
  const signer = ctx.signing.signer;
  if (!signer) fail('signing_unavailable', 'Keys cannot be signed right now: the portal has no usable signing key.', { hint: ctx.signing.unavailableReason ?? undefined });
  return signer;
}

/** The label of a user in records: "Name (email)", or whichever of the two exists, or the id. */
export function userLabel(user: { id: string; name: string; email: string | null }): string {
  const name = user.name.trim();
  if (name && user.email) return `${name} (${user.email})`;
  return user.email ?? (name || user.id);
}

/** The user actor of a signed-in request. Throws on a request without a principal (a programming error: authorize runs first). */
export function actorFromCtx(ctx: Pick<RequestContext, 'principal'>): Actor {
  const p = ctx.principal;
  if (!p) throw new Error('actorFromCtx: the request has no principal');
  return { kind: 'user', userId: p.userId, label: userLabel({ id: p.userId, name: p.name, email: p.email }) };
}

/** The actor of a confirmed public form request. */
export function publicActor(email: string, ctx: Pick<RequestContext, 'ip' | 'userAgent'>): Actor {
  return { kind: 'public', email, ip: ctx.ip, userAgent: ctx.userAgent };
}

/** The id and label an actor leaves in *_by / *_by_label columns and audit_events. */
export function actorColumns(actor: Actor): { id: string | null; label: string } {
  switch (actor.kind) {
    case 'user':
      return { id: actor.userId, label: actor.label };
    case 'public':
      return { id: null, label: actor.email };
    case 'system':
      return { id: null, label: actor.label };
  }
}

/** What inTransaction needs besides the actor (a RequestContext has all of it). */
export interface ServiceDeps {
  db: Db;
  config: Config;
  log: Logger;
  signing: SigningState;
  mailer: Mailer;
  now?: Date;
  requestId?: string;
  ip?: string | null;
  userAgent?: string | null;
}

/**
 * Runs `fn` in one transaction with a ServiceCtx, then the afterCommit
 * queue. For handlers: `inTransaction(ctx, actorFromCtx(ctx), (s) => issue(s, body))`.
 */
export async function inTransaction<T>(deps: ServiceDeps, actor: Actor, fn: (ctx: ServiceCtx) => Promise<T>): Promise<T> {
  const queue: (() => Promise<void>)[] = [];
  const result = await withTx(deps.db, (tx) =>
    fn({
      tx,
      actor,
      now: deps.now ?? new Date(),
      signing: deps.signing,
      mailer: deps.mailer,
      config: deps.config,
      log: deps.log,
      requestId: deps.requestId,
      ip: deps.ip,
      userAgent: deps.userAgent,
      afterCommit: (job) => {
        queue.push(job);
      },
    }),
  );
  for (const job of queue) {
    try {
      await job();
    } catch (e) {
      deps.log.error('after-commit work failed', { err: e, requestId: deps.requestId });
    }
  }
  return result;
}
