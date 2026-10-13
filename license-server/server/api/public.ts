// The kudurru.io form and its confirmation page (design spec §6 "Public
// form", FROZEN contract): POST /api/v1/public/key-requests answers 202
// {"status":"sent"} for every valid input (also beyond the per-email limit
// and for a filled honeypot), 400 invalid with `fields` (also for a wrong
// content type, bad JSON and oversize bodies: the route's badBody), 429
// rate_limited with Retry-After, and 503 unavailable without mail or a
// signer. Unknown fields are ignored; there is no nodes field. POST
// …/verify consumes the mailed token and answers the outcome, never the key
// (pending_review by default: an admin approves before a key is signed).

import type { ResponseOf } from '../../shared/api';
import { readJson, v, validate, type Infer } from '../http/body';
import { HttpError, json } from '../http/errors';
import type { PartialHandlerMap } from '../http/types';
import { licenseeField } from '../licensing/claims';
import { submitPublicRequest, verifyPublicRequest } from '../services/publicRequests';
import { useField } from './me';

const PublicShape = v.object({ name: licenseeField, email: v.email(), use: useField, noncommercial: v.literal(true) });
const VerifyShape = v.object({ token: v.string({ min: 1, max: 200 }) });

const FORM_FIELDS = ['name', 'email', 'use', 'noncommercial'];

function isObject(raw: unknown): raw is Record<string, unknown> {
  return raw !== null && typeof raw === 'object' && !Array.isArray(raw);
}

/** The honeypot (`website`) was filled: anything but absent, null or "". */
function honeypotFilled(raw: Record<string, unknown>): boolean {
  return raw.website !== undefined && raw.website !== null && raw.website !== '';
}

/** The form's fields; a body that is not an object misses all of them. */
function parseForm(raw: unknown): { body: Infer<typeof PublicShape>; honeypot: boolean } {
  if (!isObject(raw)) throw new HttpError(400, 'invalid', 'The request body must be a JSON object.', { fields: FORM_FIELDS });
  return { body: validate(PublicShape, raw), honeypot: honeypotFilled(raw) };
}

export const publicHandlers: PartialHandlerMap = {
  publicKeyRequest: async (ctx) => {
    const { body, honeypot } = await readJson(ctx, parseForm);
    await submitPublicRequest(ctx, { name: body.name, email: body.email, use: body.use, honeypot });
    return json<ResponseOf<'publicKeyRequest'>>({ status: 'sent' }, { status: 202 });
  },

  publicKeyRequestVerify: async (ctx) => {
    const body = await readJson(ctx, VerifyShape);
    return json<ResponseOf<'publicKeyRequestVerify'>>({ outcome: await verifyPublicRequest(ctx, body.token) });
  },
};
