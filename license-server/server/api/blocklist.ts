// The blocklist (design spec §8 blocked-emails): addresses whose personal
// requests always wait for review. Listing needs blocklist.read, removing
// blocklist.write (superadmin); both are thin handlers over
// services/blocklist.ts.

import type { ResponseOf } from '../../shared/api';
import { isEmailSyntax, normalizeEmail } from '../http/body';
import { json, noContent, notFound } from '../http/errors';
import type { PartialHandlerMap } from '../http/types';
import { listBlocked, unblockEmail } from '../services/blocklist';
import { actorFromCtx, inTransaction } from '../services/types';

export const blocklistHandlers: PartialHandlerMap = {
  blockedEmailsList: async (ctx) => json<ResponseOf<'blockedEmailsList'>>({ items: await listBlocked(ctx.db) }),

  blockedEmailRemove: async (ctx) => {
    const email = normalizeEmail(ctx.params.email);
    if (!isEmailSyntax(email)) notFound('That address is not blocked.');
    await inTransaction(ctx, actorFromCtx(ctx), (s) => unblockEmail(s, email));
    return noContent();
  },
};
