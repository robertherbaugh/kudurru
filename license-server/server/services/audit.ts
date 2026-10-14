// The audit log (design spec §4): one append-only audit_events row per
// change and per key download, written in the same transaction as the
// change. Details are a JSON object of facts (ids, old and new values,
// counts) and must never contain a key token: audit() refuses anything
// shaped like a compact JWS (log.ts TOKEN_SOURCE: eyJ….eyJ….<signature>),
// anywhere in the details, by throwing (a bug to fix, not a condition to
// handle). Names, notes and licensees with dots ("MoneyJar.co.uk") pass. Security failures
// (csrf.denied, authz.denied, …) are log lines, not audit rows.

import type { AuditAction, AuditTargetType, JsonObject, JsonValue } from '../../shared/api';
import type { Queryable } from '../db/sql';
import { containsToken } from '../log';
import { actorColumns, type Actor, type ServiceCtx } from './types';

export interface AuditInput {
  actor: Actor;
  action: AuditAction;
  targetType: AuditTargetType;
  targetId: string;
  details?: JsonObject;
  /** Default: the public actor's ip, else null. */
  ip?: string | null;
  /** Default: the public actor's user agent, else null. */
  userAgent?: string | null;
  requestId?: string | null;
}

/** The path of the first string (key or value) that looks like a token (log.ts containsToken), or null. */
export function findToken(value: JsonValue | undefined, path = 'details'): string | null {
  if (typeof value === 'string') return containsToken(value) ? path : null;
  if (Array.isArray(value)) {
    for (let i = 0; i < value.length; i++) {
      const hit = findToken(value[i], `${path}[${i}]`);
      if (hit) return hit;
    }
    return null;
  }
  if (value !== null && typeof value === 'object') {
    for (const [k, v] of Object.entries(value)) {
      if (containsToken(k)) return `${path}.<key>`;
      const hit = findToken(v, `${path}.${k}`);
      if (hit) return hit;
    }
  }
  return null;
}

/** Writes one audit event. Throws when the details contain a token. */
export async function audit(q: Queryable, input: AuditInput): Promise<void> {
  const details = input.details ?? {};
  const hit = findToken(details);
  if (hit) throw new Error(`audit ${input.action}: ${hit} looks like a key token; tokens never go into the audit log`);
  const actor = actorColumns(input.actor);
  const ip = input.ip !== undefined ? input.ip : input.actor.kind === 'public' ? input.actor.ip : null;
  const userAgent = input.userAgent !== undefined ? input.userAgent : input.actor.kind === 'public' ? input.actor.userAgent : null;
  await q`
    insert into audit_events (actor_id, actor_label, action, target_type, target_id, details, ip, user_agent, request_id)
    values (${actor.id}, ${actor.label}, ${input.action}, ${input.targetType}, ${input.targetId}, ${details}, ${ip}, ${userAgent}, ${input.requestId ?? null})`;
}

/** audit() inside a service: actor, transaction, ip, user agent and request id come from the ServiceCtx. */
export function auditCtx(ctx: ServiceCtx, input: Omit<AuditInput, 'actor' | 'ip' | 'userAgent' | 'requestId'>): Promise<void> {
  return audit(ctx.tx, {
    ...input,
    actor: ctx.actor,
    ip: ctx.ip ?? (ctx.actor.kind === 'public' ? ctx.actor.ip : null),
    userAgent: ctx.userAgent ?? (ctx.actor.kind === 'public' ? ctx.actor.userAgent : null),
    requestId: ctx.requestId ?? null,
  });
}
