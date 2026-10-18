// The audit log (design spec §8 GET /api/v1/audit): newest first, filtered
// by action, target and actor, paged by a keyset cursor (the event id) so a
// page never shifts while new events arrive. Read-only: audit_events is
// append-only (triggers refuse UPDATE, DELETE and TRUNCATE) and holds no
// key tokens.

import type { AuditAction, AuditEvent, AuditTargetType, JsonObject, ResponseOf } from '../../shared/api';
import { AUDIT_TARGET_TYPES, DEFAULT_PAGE_LIMIT, MAX_PAGE_LIMIT } from '../../shared/api';
import type { Queryable } from '../db/sql';
import { HttpError, json } from '../http/errors';
import { q } from '../http/body';
import type { PartialHandlerMap } from '../http/types';

interface AuditRow {
  id: string;
  at: Date;
  actor_id: string | null;
  actor_label: string;
  action: AuditAction;
  target_type: AuditTargetType;
  target_id: string;
  details: JsonObject;
  ip: string | null;
  user_agent: string | null;
  request_id: string | null;
}

export interface AuditFilter {
  action?: string;
  targetType?: AuditTargetType;
  targetId?: string;
  actorId?: string;
  /** Events older than this id. */
  before?: string;
  limit: number;
}

/** One page of the audit log, newest first; `next` is the cursor of the following page. */
export async function listAuditEvents(db: Queryable, f: AuditFilter): Promise<{ items: AuditEvent[]; next?: string }> {
  const rows = await db<AuditRow[]>`
    select id, at, actor_id, actor_label, action, target_type, target_id, details, host(ip) as ip, user_agent, request_id
      from audit_events
     where true
       ${f.action ? db`and action = ${f.action}` : db``}
       ${f.targetType ? db`and target_type = ${f.targetType}` : db``}
       ${f.targetId ? db`and target_id = ${f.targetId}` : db``}
       ${f.actorId ? db`and actor_id = ${f.actorId}` : db``}
       ${f.before ? db`and id < ${f.before}::bigint` : db``}
     order by id desc
     limit ${f.limit + 1}`;
  const page = rows.slice(0, f.limit);
  const items: AuditEvent[] = page.map((r) => ({
    id: String(r.id),
    at: r.at.toISOString(),
    actor: { id: r.actor_id, label: r.actor_label },
    action: r.action,
    targetType: r.target_type,
    targetId: r.target_id,
    details: r.details,
    ip: r.ip,
    userAgent: r.user_agent,
    requestId: r.request_id,
  }));
  return rows.length > f.limit && page.length > 0 ? { items, next: String(page[page.length - 1]!.id) } : { items };
}

export const auditHandlers: PartialHandlerMap = {
  auditList: async (ctx) => {
    const query = ctx.query;
    const before = q.string(query, 'before', { max: 19 });
    if (before !== undefined && !/^[1-9]\d{0,18}$/.test(before)) throw new HttpError(400, 'invalid', 'The query parameter before is not valid.', { fields: ['before'] });
    const page = await listAuditEvents(ctx.db, {
      action: q.string(query, 'action', { max: 64 }),
      targetType: q.enum(query, 'targetType', AUDIT_TARGET_TYPES),
      targetId: q.string(query, 'targetId', { max: 200 }),
      actorId: q.string(query, 'actorId', { max: 100 }),
      before,
      limit: q.int(query, 'limit', { min: 1, max: MAX_PAGE_LIMIT, fallback: DEFAULT_PAGE_LIMIT })!,
    });
    return json<ResponseOf<'auditList'>>(page);
  },
};
