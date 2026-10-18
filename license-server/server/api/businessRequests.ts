// Business license requests (README, "Business requests";
// services/businessRequests.ts): the kudurru.io form's POST
// /api/v1/public/business-requests (202 {"status":"sent"} for every valid
// input, also beyond the per-email limit and for a filled honeypot; 400
// invalid with `fields`, also for a wrong content type, bad JSON and
// oversize bodies; 429 rate_limited with Retry-After), and the Requests
// page's list and close. Issuing from a request is POST /licenses with
// businessRequestId (api/licenses.ts).

import type { BusinessRequestStatus, ResponseOf } from '../../shared/api';
import { BUSINESS_EDITIONS, BUSINESS_REQUEST_MAX_NODES, type BusinessEdition } from '../../shared/catalog';
import { q, readJson, v, validate, type Infer } from '../http/body';
import { HttpError, json } from '../http/errors';
import type { PartialHandlerMap } from '../http/types';
import { licenseeField } from '../licensing/claims';
import { closeBusinessRequest, getBusinessRequest, listBusinessRequests, submitBusinessRequest } from '../services/businessRequests';
import { actorFromCtx, inTransaction } from '../services/types';

const EDITIONS = Object.keys(BUSINESS_EDITIONS) as BusinessEdition[];
const STATUSES: readonly BusinessRequestStatus[] = ['open', 'issued', 'closed'];

const BusinessShape = v.object({
  name: licenseeField,
  email: v.email(),
  company: v.string({ min: 1, max: 160, trim: true }),
  edition: v.enum(EDITIONS),
  nodes: v.int({ min: 1, max: BUSINESS_REQUEST_MAX_NODES }),
  details: v.optional(v.string({ max: 1500, trim: true, multiline: true })),
});

const FORM_FIELDS = ['name', 'email', 'company', 'edition', 'nodes', 'details'];

const CloseShape = v.object({ note: v.string({ min: 1, max: 2000, trim: true, multiline: true }) });

function isObject(raw: unknown): raw is Record<string, unknown> {
  return raw !== null && typeof raw === 'object' && !Array.isArray(raw);
}

/** The form's fields and whether the honeypot (`website`) was filled; a body that is not an object misses all of them. */
function parseForm(raw: unknown): { body: Infer<typeof BusinessShape>; honeypot: boolean } {
  if (!isObject(raw)) throw new HttpError(400, 'invalid', 'The request body must be a JSON object.', { fields: FORM_FIELDS });
  const honeypot = raw.website !== undefined && raw.website !== null && raw.website !== '';
  return { body: validate(BusinessShape, raw), honeypot };
}

export const businessRequestHandlers: PartialHandlerMap = {
  publicBusinessRequest: async (ctx) => {
    const { body, honeypot } = await readJson(ctx, parseForm);
    await submitBusinessRequest(ctx, { ...body, details: body.details ?? '', honeypot });
    return json<ResponseOf<'publicBusinessRequest'>>({ status: 'sent' }, { status: 202 });
  },

  businessRequestsList: async (ctx) => json<ResponseOf<'businessRequestsList'>>(await listBusinessRequests(ctx.db, q.enum(ctx.query, 'status', STATUSES))),

  businessRequestClose: async (ctx) => {
    const body = await readJson(ctx, CloseShape);
    const request = await inTransaction(ctx, actorFromCtx(ctx), async (s) => {
      await closeBusinessRequest(s, ctx.params.id, body.note);
      return getBusinessRequest(s.tx, ctx.params.id);
    });
    return json<ResponseOf<'businessRequestClose'>>({ request });
  },
};
