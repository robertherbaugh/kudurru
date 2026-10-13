// The one-click approval link of the approvers' mail (/approve?token=…;
// services/approvals.ts, services/requests.ts): anonymous routes, the
// token is the credential. Preview shows the request and the terms and
// changes nothing; decide approves with the default terms or rejects with
// a note (1-1000 characters, mailed to the requester). Both are POST so the
// token stays out of URLs and logs, and so a mail scanner that opens the
// link decides nothing. Rate-limited per address (http/ratelimit.ts).
// The answer never contains the key: it goes to the requester by mail.

import type { ResponseOf } from '../../shared/api';
import { readJson, v } from '../http/body';
import { HttpError, json } from '../http/errors';
import type { PartialHandlerMap } from '../http/types';
import { decideApproval, previewApproval } from '../services/requests';

const TokenShape = v.object({ token: v.string({ min: 1, max: 200 }) });

const DecideShape = v.object({
  token: v.string({ min: 1, max: 200 }),
  decision: v.enum(['approve', 'reject']),
  note: v.optional(v.string({ min: 1, max: 1000, trim: true, multiline: true })),
});

export const approvalHandlers: PartialHandlerMap = {
  approvalPreview: async (ctx) => {
    const body = await readJson(ctx, TokenShape);
    return json<ResponseOf<'approvalPreview'>>(await previewApproval(ctx.db, ctx.config, body.token, ctx.now));
  },

  approvalDecide: async (ctx) => {
    const body = await readJson(ctx, DecideShape);
    if (body.decision === 'reject' && body.note === undefined) {
      throw new HttpError(400, 'invalid', 'A rejection needs a note for the requester (1 to 1000 characters).', { fields: ['note'] });
    }
    const decision = body.decision === 'reject' ? { decision: 'reject' as const, note: body.note ?? '' } : { decision: 'approve' as const };
    return json<ResponseOf<'approvalDecide'>>(await decideApproval(ctx, body.token, decision));
  },
};
