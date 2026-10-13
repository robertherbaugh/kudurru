// Personal key requests that wait for an admin (design spec §8 requests.*):
// thin handlers over services/requests.ts. Approve answers the request and
// the license it led to; reject answers the request. The one-click link of
// the approvers' mail decides through api/approvals.ts instead.

import type { RequestStatus, ResponseOf } from '../../shared/api';
import { hasBody, q, readJson, v } from '../http/body';
import { json } from '../http/errors';
import type { PartialHandlerMap } from '../http/types';
import { licenseeField } from '../licensing/claims';
import { licenseDetail } from '../services/licenses';
import { approveRequest, getRequest, listRequests, rejectRequest } from '../services/requests';
import { actorFromCtx, inTransaction } from '../services/types';

const STATUSES: readonly RequestStatus[] = ['open', 'approved', 'rejected', 'canceled'];

const ApproveShape = v.object({
  customerId: v.optional(v.id('customer')),
  licensee: v.optional(licenseeField),
  expiresOn: v.optional(v.expiresOn()),
});

const RejectShape = v.object({ note: v.string({ min: 1, max: 2000, trim: true, multiline: true }) });

export const requestHandlers: PartialHandlerMap = {
  requestsList: async (ctx) => json<ResponseOf<'requestsList'>>(await listRequests(ctx.db, q.enum(ctx.query, 'status', STATUSES))),

  requestApprove: async (ctx) => {
    const body = hasBody(ctx.req) ? await readJson(ctx, ApproveShape) : {};
    const result = await inTransaction(ctx, actorFromCtx(ctx), async (s) => {
      const { licenseId } = await approveRequest(s, ctx.params.id, body);
      return { request: await getRequest(s.tx, ctx.params.id), license: await licenseDetail(s.tx, licenseId, s.now, s.signing) };
    });
    return json<ResponseOf<'requestApprove'>>(result);
  },

  requestReject: async (ctx) => {
    const body = await readJson(ctx, RejectShape);
    const request = await inTransaction(ctx, actorFromCtx(ctx), async (s) => {
      await rejectRequest(s, ctx.params.id, body.note);
      return getRequest(s.tx, ctx.params.id);
    });
    return json<ResponseOf<'requestReject'>>({ request });
  },
};
