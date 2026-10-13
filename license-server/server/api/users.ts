// Users (design spec §8 users.*): thin handlers over services/users.ts. The
// acting session is passed along so role, disable and revoke changes end
// the target's sessions but never the one making the change.

import type { ResponseOf } from '../../shared/api';
import { ROLES } from '../../shared/permissions';
import { q, readJson, v } from '../http/body';
import { json } from '../http/errors';
import type { PartialHandlerMap, RequestContext } from '../http/types';
import { getUserDetail, listUsers, revokeUserSessions, updateUser, type ActingUser } from '../services/users';
import { actorFromCtx, inTransaction } from '../services/types';
import { requirePrincipal } from './me';

const UpdateShape = v.object({
  grantedRoles: v.optional(v.array(v.enum(ROLES), { unique: true, max: ROLES.length })),
  disabled: v.optional(v.bool()),
});

function acting(ctx: RequestContext): ActingUser {
  return { userId: requirePrincipal(ctx).userId, sessionHash: ctx.session?.idHash ?? null };
}

export const userHandlers: PartialHandlerMap = {
  usersList: async (ctx) => json<ResponseOf<'usersList'>>(await listUsers(ctx.db, q.string(ctx.query, 'q'))),

  userGet: async (ctx) => json<ResponseOf<'userGet'>>(await getUserDetail(ctx.db, ctx.params.id, ctx.now)),

  userUpdate: async (ctx) => {
    const body = await readJson(ctx, UpdateShape);
    const detail = await inTransaction(ctx, actorFromCtx(ctx), async (s) => {
      await updateUser(s, ctx.params.id, body, acting(ctx));
      return getUserDetail(s.tx, ctx.params.id, s.now);
    });
    return json<ResponseOf<'userUpdate'>>(detail);
  },

  userRevokeSessions: async (ctx) => {
    const revoked = await inTransaction(ctx, actorFromCtx(ctx), (s) => revokeUserSessions(s, ctx.params.id, acting(ctx)));
    return json<ResponseOf<'userRevokeSessions'>>({ revoked });
  },
};
