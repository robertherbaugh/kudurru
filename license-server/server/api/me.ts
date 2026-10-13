// The signed-in person's own licenses and requests (design spec §8 self.*):
// thin handlers over services/licenses.ts (customer views, isolated in SQL
// by membership), services/personal.ts (personal request and renewal) and
// services/members.ts (invites). The router has already authorised the
// route; customer DTOs never carry internal fields.

import type { Attestation, ResponseOf } from '../../shared/api';
import { ATTESTATION_LICENSE, ATTESTATION_STATEMENT, LICENSE_PAGE_URL } from '../../shared/catalog';
import { readJson, v } from '../http/body';
import { fail, json, keyFile } from '../http/errors';
import type { PartialHandlerMap, Principal, RequestContext } from '../http/types';
import { licenseeField } from '../licensing/claims';
import { myLicenseDetail, myLicenseKey, myLicenses } from '../services/licenses';
import { previewInvite, redeemInvite } from '../services/members';
import { renewMyLicense, requestPersonalKey } from '../services/personal';
import { myRequests } from '../services/requests';
import { actorFromCtx, inTransaction } from '../services/types';

/** The signed-in principal of a route the router authorised (a programming error without one). */
export function requirePrincipal(ctx: Pick<RequestContext, 'principal'>): Principal {
  if (!ctx.principal) throw new Error('the route has no principal: authorize must run first');
  return ctx.principal;
}

/** The public form's "use" rules: 1-1000 UTF-16 code units, not blank (line breaks allowed). */
export const useField = v.string({ min: 1, max: 1000, trim: true, multiline: true });

const PersonalRequestShape = v.object({ name: licenseeField, use: useField, noncommercial: v.literal(true) });
const TokenShape = v.object({ token: v.string({ min: 1, max: 200 }) });

export const meHandlers: PartialHandlerMap = {
  myLicenses: async (ctx) => {
    const p = requirePrincipal(ctx);
    return json<ResponseOf<'myLicenses'>>({ items: await myLicenses(ctx.db, p.userId, ctx.now) });
  },

  myLicense: async (ctx) => {
    const p = requirePrincipal(ctx);
    return json<ResponseOf<'myLicense'>>(await myLicenseDetail(ctx.db, p.userId, ctx.params.id, ctx.now, ctx.signing));
  },

  myLicenseKey: async (ctx) => {
    const p = requirePrincipal(ctx);
    const token = await inTransaction(ctx, actorFromCtx(ctx), (s) => myLicenseKey(s, p.userId, ctx.params.id));
    return keyFile(token);
  },

  myRequests: async (ctx) => {
    const p = requirePrincipal(ctx);
    return json<ResponseOf<'myRequests'>>({ items: await myRequests(ctx.db, p.userId, p.emailVerified ? p.email : null) });
  },

  myRequestCreate: async (ctx) => {
    const p = requirePrincipal(ctx);
    const body = await readJson(ctx, PersonalRequestShape);
    if (!p.email || !p.emailVerified) {
      fail('email_unverified', 'Your sign-in has no verified email address, so the portal cannot send you a key. Request a free personal key with the form on kudurru.io instead.', {
        hint: LICENSE_PAGE_URL,
      });
    }
    const email = p.email;
    const attestation: Attestation = {
      source: 'portal',
      license: ATTESTATION_LICENSE,
      statement: ATTESTATION_STATEMENT,
      at: ctx.now.toISOString(),
      ip: ctx.ip,
      userAgent: ctx.userAgent,
      email,
    };
    const result = await inTransaction(ctx, actorFromCtx(ctx), (s) =>
      requestPersonalKey(s, { name: body.name, email, use: body.use, attestation, source: 'portal', requestedBy: p.userId }),
    );
    return json<ResponseOf<'myRequestCreate'>>(result);
  },

  myLicenseRenew: async (ctx) => {
    const p = requirePrincipal(ctx);
    const mailTo = p.emailVerified ? p.email : null;
    const detail = await inTransaction(ctx, actorFromCtx(ctx), async (s) => {
      await renewMyLicense(s, ctx.params.id, p.userId, mailTo);
      return myLicenseDetail(s.tx, p.userId, ctx.params.id, s.now, s.signing);
    });
    return json<ResponseOf<'myLicenseRenew'>>(detail);
  },

  myInvitePreview: async (ctx) => {
    const body = await readJson(ctx, TokenShape);
    return json<ResponseOf<'myInvitePreview'>>(await previewInvite(ctx.db, body.token, ctx.now));
  },

  myInviteRedeem: async (ctx) => {
    const p = requirePrincipal(ctx);
    const body = await readJson(ctx, TokenShape);
    const result = await inTransaction(ctx, actorFromCtx(ctx), (s) => redeemInvite(s, body.token, p.userId));
    return json<ResponseOf<'myInviteRedeem'>>(result);
  },
};
