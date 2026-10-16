// Licenses (design spec §8 licenses.*): thin handlers over
// services/licenses.ts. Bodies are validated with the v kit (unknown fields
// dropped, every failing field reported); the issue policy itself is the
// service's (licensing/claims.ts), so the personal flow and approvals apply
// the same rules. Every action answers the LicenseDetail read inside its
// transaction; downloads are license.key attachments.

import type { ResponseOf } from '../../shared/api';
import { FEATURES, MAX_NODES, MAX_TERM_DAYS, PLANS, type Feature, type LicenseState, type Plan } from '../../shared/catalog';
import { hasBody, q, readJson, v } from '../http/body';
import { HttpError, json, keyFile } from '../http/errors';
import type { PartialHandlerMap } from '../http/types';
import { licenseeField } from '../licensing/claims';
import { lockOpenBusinessRequest, markBusinessRequestIssued } from '../services/businessRequests';
import {
  changeLicense,
  downloadLicenseKey,
  emailLicenseKey,
  importLicense,
  issueLicense,
  licenseDetail,
  listLicenses,
  renewLicense,
  withdrawLicense,
} from '../services/licenses';
import { actorFromCtx, inTransaction } from '../services/types';
import { NewCustomerShape, notesField } from './customers';

const PLAN_KEYS: readonly Plan[] = PLANS.map((p) => p.key);
const FEATURE_KEYS: readonly Feature[] = FEATURES.map((f) => f.key);
const STATES: readonly LicenseState[] = ['active', 'expiring', 'grace', 'expired', 'withdrawn'];

const featuresField = v.nullable(v.array(v.enum(FEATURE_KEYS), { min: 1, unique: true }));
const nodesField = v.int({ min: 1, max: MAX_NODES });

const IssueShape = v.object({
  customerId: v.optional(v.id('customer')),
  newCustomer: v.optional(NewCustomerShape),
  licensee: licenseeField,
  plan: v.enum(PLAN_KEYS),
  nodes: nodesField,
  features: featuresField,
  expiresOn: v.expiresOn(),
  agreementRef: v.optional(v.string({ max: 200, trim: true })),
  notes: v.optional(notesField),
  emailCustomer: v.optional(v.bool()),
  businessRequestId: v.optional(v.id('businessRequest')),
});

const ImportShape = v.object({
  // Line breaks and tabs from a mail or a file are fine: the verifier removes whitespace as the nodes do.
  token: v.string({ min: 1, max: 16 * 1024, multiline: true }),
  customerId: v.id('customer'),
});

const RenewShape = v.object({ expiresOn: v.expiresOn(), emailCustomer: v.optional(v.bool()) });

const ChangeShape = v.object({
  licensee: v.optional(licenseeField),
  plan: v.optional(v.enum(PLAN_KEYS)),
  nodes: v.optional(nodesField),
  features: v.optional(featuresField),
  expiresOn: v.optional(v.expiresOn()),
  customerId: v.optional(v.id('customer')),
  emailCustomer: v.optional(v.bool()),
});

const WithdrawShape = v.object({
  reason: v.string({ min: 1, max: 1000, trim: true, multiline: true }),
  customerMessage: v.optional(v.string({ max: 2000, trim: true, multiline: true })),
  blockPersonal: v.optional(v.bool()),
});

const EmailShape = v.object({ to: v.optional(v.email()) });

export const licenseHandlers: PartialHandlerMap = {
  licensesList: async (ctx) => {
    const query = ctx.query;
    const page = q.page(query);
    const result = await listLicenses(
      ctx.db,
      {
        ...page,
        q: q.string(query, 'q'),
        state: q.enum(query, 'state', STATES),
        plan: q.enum(query, 'plan', PLAN_KEYS),
        customerId: q.string(query, 'customerId', { max: 100 }),
        expiringWithinDays: q.int(query, 'expiringWithinDays', { min: 1, max: MAX_TERM_DAYS }),
      },
      ctx.now,
    );
    return json<ResponseOf<'licensesList'>>(result);
  },

  licenseIssue: async (ctx) => {
    const body = await readJson(ctx, IssueShape);
    if ((body.customerId === undefined) === (body.newCustomer === undefined)) {
      throw new HttpError(400, 'invalid', 'Choose an existing customer or a new one, not both.', { fields: ['customerId', 'newCustomer'] });
    }
    const detail = await inTransaction(ctx, actorFromCtx(ctx), async (s) => {
      // From a business request: it must still be open, and becomes issued with the license.
      if (body.businessRequestId) await lockOpenBusinessRequest(s, body.businessRequestId);
      const id = await issueLicense(s, body);
      if (body.businessRequestId) await markBusinessRequestIssued(s, body.businessRequestId, id);
      return licenseDetail(s.tx, id, s.now, s.signing);
    });
    return json<ResponseOf<'licenseIssue'>>(detail);
  },

  licenseImport: async (ctx) => {
    const body = await readJson(ctx, ImportShape);
    const detail = await inTransaction(ctx, actorFromCtx(ctx), async (s) => {
      const id = await importLicense(s, body);
      return licenseDetail(s.tx, id, s.now, s.signing);
    });
    return json<ResponseOf<'licenseImport'>>(detail);
  },

  licenseGet: async (ctx) => json<ResponseOf<'licenseGet'>>(await licenseDetail(ctx.db, ctx.params.id, ctx.now, ctx.signing)),

  licenseKeyDownload: async (ctx) => {
    const token = await inTransaction(ctx, actorFromCtx(ctx), (s) => downloadLicenseKey(s, ctx.params.id, ctx.params.keyId));
    return keyFile(token);
  },

  licenseRenew: async (ctx) => {
    const body = await readJson(ctx, RenewShape);
    const detail = await inTransaction(ctx, actorFromCtx(ctx), async (s) => {
      await renewLicense(s, ctx.params.id, body);
      return licenseDetail(s.tx, ctx.params.id, s.now, s.signing);
    });
    return json<ResponseOf<'licenseRenew'>>(detail);
  },

  licenseChange: async (ctx) => {
    const body = await readJson(ctx, ChangeShape);
    const detail = await inTransaction(ctx, actorFromCtx(ctx), async (s) => {
      await changeLicense(s, ctx.params.id, body);
      return licenseDetail(s.tx, ctx.params.id, s.now, s.signing);
    });
    return json<ResponseOf<'licenseChange'>>(detail);
  },

  licenseWithdraw: async (ctx) => {
    const body = await readJson(ctx, WithdrawShape);
    const detail = await inTransaction(ctx, actorFromCtx(ctx), async (s) => {
      await withdrawLicense(s, ctx.params.id, body);
      return licenseDetail(s.tx, ctx.params.id, s.now, s.signing);
    });
    return json<ResponseOf<'licenseWithdraw'>>(detail);
  },

  licenseEmail: async (ctx) => {
    const body = hasBody(ctx.req) ? await readJson(ctx, EmailShape) : {};
    const result = await emailLicenseKey(ctx, actorFromCtx(ctx), ctx.params.id, body.to);
    return json<ResponseOf<'licenseEmail'>>(result);
  },
};
