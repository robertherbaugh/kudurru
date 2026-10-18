// Order Forms (README, "Order Forms"; services/orderForms.ts): the back
// office's list, draft, change, send, revise, countersign and void, and the
// customer's own list, detail, accept and decline. Terms are validated as a
// whole with the v kit (every failing field as `terms.<field>`), then the
// cross-field rules of shared/orderForms.ts; a PATCH merges the given terms
// into the stored ones first. Issuing a license from an executed form is
// POST /licenses with orderFormId (api/licenses.ts).

import type { OrderFormStatus, ResponseOf } from '../../shared/api';
import { BUSINESS_EDITIONS, FEATURES, MAX_NODES, ORDER_FORM_MAX_CONTACTS, ORDER_FORM_MAX_FEE_CENTS, type BusinessEdition, type Feature } from '../../shared/catalog';
import { q, readJson, v, validate } from '../http/body';
import { HttpError, json } from '../http/errors';
import type { PartialHandlerMap } from '../http/types';
import {
  acceptOrderForm,
  countersignOrderForm,
  createOrderForm,
  declineOrderForm,
  getMyOrderForm,
  getOrderForm,
  listMyOrderForms,
  listOrderForms,
  lockTermsForUpdate,
  reviseOrderForm,
  sendOrderForm,
  updateOrderForm,
  voidOrderForm,
} from '../services/orderForms';
import { actorFromCtx, inTransaction } from '../services/types';
import { NewCustomerShape } from './customers';
import { requirePrincipal } from './me';

const STATUSES: readonly OrderFormStatus[] = ['draft', 'sent', 'accepted', 'executed', 'declined', 'void'];
const EDITIONS = Object.keys(BUSINESS_EDITIONS) as BusinessEdition[];
const FEATURE_KEYS: readonly Feature[] = FEATURES.map((f) => f.key);

/** A text field of the form: trimmed, line breaks allowed, may be empty in a draft (sending refuses the gaps). */
const text = (max: number) => v.string({ max, trim: true, multiline: true });
const fee = v.int({ min: 0, max: ORDER_FORM_MAX_FEE_CENTS });

const TermsShape = v.object({
  effectiveDate: v.nullable(v.expiresOn()),
  licensorContact: text(1000),
  customerLegalName: v.string({ max: 200, trim: true }),
  customerAddress: text(1000),
  billingContact: text(500),
  authorizedContacts: v.array(v.email(), { max: ORDER_FORM_MAX_CONTACTS, unique: true }),
  edition: v.enum(EDITIONS),
  modules: v.enum(['all', 'named', 'none'] as const),
  namedModules: v.array(v.enum(FEATURE_KEYS), { unique: true }),
  nodes: v.int({ min: 1, max: MAX_NODES }),
  environments: text(500),
  permittedScope: text(1000),
  nonproductionNodes: v.int({ min: 0, max: MAX_NODES }),
  nonproductionTerms: text(1000),
  serviceProvider: v.enum(['not_authorized', 'authorized'] as const),
  serviceProviderScope: text(1000),
  government: v.enum(['no', 'yes'] as const),
  governmentScope: text(1000),
  subscriptionStart: v.expiresOn(),
  subscriptionEnd: v.expiresOn(),
  platformFeeCents: fee,
  perNodeFeeCents: fee,
  annualFeeCents: fee,
  paymentTerms: text(1000),
  keyPolicy: text(1000),
  support: text(1000),
  renewal: text(1000),
  governingLaw: text(500),
  specialTerms: text(4000),
});

const CreateShape = v.object({
  terms: TermsShape,
  customerId: v.optional(v.id('customer')),
  newCustomer: v.optional(NewCustomerShape),
  businessRequestId: v.optional(v.id('businessRequest')),
});

const WholeTerms = v.object({ terms: TermsShape });

/** The PATCH body: an object of terms, checked only once merged with the stored ones. */
function parsePatch(raw: unknown): Record<string, unknown> {
  const terms = raw !== null && typeof raw === 'object' && !Array.isArray(raw) ? (raw as { terms?: unknown }).terms : undefined;
  if (terms === null || typeof terms !== 'object' || Array.isArray(terms)) throw new HttpError(400, 'invalid', 'Send the terms to change as an object.', { fields: ['terms'] });
  return terms as Record<string, unknown>;
}

const sha = v.refine(v.string({ min: 64, max: 64 }), (s) => /^[0-9a-f]{64}$/.test(s));
const signatoryName = v.string({ min: 1, max: 200, trim: true });
const signatoryTitle = v.string({ min: 1, max: 200, trim: true });

const CountersignShape = v.object({ name: signatoryName, title: signatoryTitle, documentSha256: sha });
const VoidShape = v.object({ reason: v.string({ min: 1, max: 1000, trim: true, multiline: true }) });
const AcceptShape = v.object({ name: signatoryName, title: signatoryTitle, documentSha256: sha, authorized: v.literal(true) });
const DeclineShape = v.object({ note: v.string({ min: 1, max: 2000, trim: true, multiline: true }) });

export const orderFormHandlers: PartialHandlerMap = {
  orderFormsList: async (ctx) =>
    json<ResponseOf<'orderFormsList'>>(await listOrderForms(ctx.db, { status: q.enum(ctx.query, 'status', STATUSES), customerId: q.string(ctx.query, 'customerId', { max: 100 }) })),

  orderFormCreate: async (ctx) => {
    const body = await readJson(ctx, CreateShape);
    if ((body.customerId === undefined) === (body.newCustomer === undefined)) {
      throw new HttpError(400, 'invalid', 'Choose an existing customer or a new one, not both.', { fields: ['customerId', 'newCustomer'] });
    }
    const form = await inTransaction(ctx, actorFromCtx(ctx), async (s) => {
      const id = await createOrderForm(s, body);
      return getOrderForm(s.tx, id);
    });
    return json<ResponseOf<'orderFormCreate'>>(form);
  },

  orderFormGet: async (ctx) => json<ResponseOf<'orderFormGet'>>(await getOrderForm(ctx.db, ctx.params.id)),

  orderFormUpdate: async (ctx) => {
    const patch = await readJson(ctx, parsePatch);
    const form = await inTransaction(ctx, actorFromCtx(ctx), async (s) => {
      const merged = { ...(await lockTermsForUpdate(s, ctx.params.id)), ...patch };
      const { terms } = validate(WholeTerms, { terms: merged });
      await updateOrderForm(s, ctx.params.id, terms);
      return getOrderForm(s.tx, ctx.params.id);
    });
    return json<ResponseOf<'orderFormUpdate'>>(form);
  },

  orderFormSend: async (ctx) => {
    const form = await inTransaction(ctx, actorFromCtx(ctx), async (s) => {
      await sendOrderForm(s, ctx.params.id);
      return getOrderForm(s.tx, ctx.params.id);
    });
    return json<ResponseOf<'orderFormSend'>>(form);
  },

  orderFormRevise: async (ctx) => {
    const form = await inTransaction(ctx, actorFromCtx(ctx), async (s) => {
      await reviseOrderForm(s, ctx.params.id);
      return getOrderForm(s.tx, ctx.params.id);
    });
    return json<ResponseOf<'orderFormRevise'>>(form);
  },

  orderFormCountersign: async (ctx) => {
    const body = await readJson(ctx, CountersignShape);
    const p = requirePrincipal(ctx);
    const form = await inTransaction(ctx, actorFromCtx(ctx), async (s) => {
      await countersignOrderForm(s, ctx.params.id, { ...body, email: p.email && p.emailVerified ? p.email : null });
      return getOrderForm(s.tx, ctx.params.id);
    });
    return json<ResponseOf<'orderFormCountersign'>>(form);
  },

  orderFormVoid: async (ctx) => {
    const body = await readJson(ctx, VoidShape);
    const form = await inTransaction(ctx, actorFromCtx(ctx), async (s) => {
      await voidOrderForm(s, ctx.params.id, body.reason);
      return getOrderForm(s.tx, ctx.params.id);
    });
    return json<ResponseOf<'orderFormVoid'>>(form);
  },

  myOrderForms: async (ctx) => json<ResponseOf<'myOrderForms'>>(await listMyOrderForms(ctx.db, requirePrincipal(ctx).userId)),

  myOrderForm: async (ctx) => json<ResponseOf<'myOrderForm'>>(await getMyOrderForm(ctx.db, requirePrincipal(ctx).userId, ctx.params.id)),

  myOrderFormAccept: async (ctx) => {
    const body = await readJson(ctx, AcceptShape);
    const p = requirePrincipal(ctx);
    const form = await inTransaction(ctx, actorFromCtx(ctx), async (s) => {
      await acceptOrderForm(s, ctx.params.id, p.userId, body);
      return getMyOrderForm(s.tx, p.userId, ctx.params.id);
    });
    return json<ResponseOf<'myOrderFormAccept'>>(form);
  },

  myOrderFormDecline: async (ctx) => {
    const body = await readJson(ctx, DeclineShape);
    const p = requirePrincipal(ctx);
    const form = await inTransaction(ctx, actorFromCtx(ctx), async (s) => {
      await declineOrderForm(s, ctx.params.id, p.userId, body.note, p.name);
      return getMyOrderForm(s.tx, p.userId, ctx.params.id);
    });
    return json<ResponseOf<'myOrderFormDecline'>>(form);
  },
};
