// Customers and their members (design spec §8 customers.*): thin handlers
// over services/customers.ts and services/members.ts. The detail page
// composes the customer row with its members, licenses and requests here,
// so the services stay free of import cycles.

import type { CustomerDetail, ResponseOf } from '../../shared/api';
import { isEmailSyntax, normalizeEmail, q, readJson, v } from '../http/body';
import { json, noContent, notFound } from '../http/errors';
import type { PartialHandlerMap } from '../http/types';
import type { Queryable } from '../db/sql';
import { actorRef, createCustomer, customerDto, getCustomerRow, listCustomers, updateCustomer } from '../services/customers';
import { licensesOfCustomer } from '../services/licenses';
import { addMember, listMembers, reinviteMember, removeMember } from '../services/members';
import { requestsOfCustomer } from '../services/requests';
import { actorFromCtx, inTransaction } from '../services/types';

const KINDS = ['person', 'organization'] as const;

export const customerNameField = v.string({ min: 1, max: 200, trim: true });
export const notesField = v.string({ max: 10_000, multiline: true });

export const NewCustomerShape = v.object({
  name: customerNameField,
  kind: v.enum(KINDS),
  contactEmail: v.optional(v.nullable(v.email())),
});

const CreateShape = v.object({
  name: customerNameField,
  kind: v.enum(KINDS),
  contactEmail: v.optional(v.nullable(v.email())),
  notes: v.optional(notesField),
});

const UpdateShape = v.object({
  name: v.optional(customerNameField),
  kind: v.optional(v.enum(KINDS)),
  contactEmail: v.optional(v.nullable(v.email())),
  notes: v.optional(notesField),
});

const AddMemberShape = v.object({ email: v.email() });

/** GET /customers/:id and the answer of customer writes. */
export async function customerDetail(db: Queryable, id: string, now: Date): Promise<CustomerDetail> {
  const row = await getCustomerRow(db, id);
  const members = await listMembers(db, id);
  const licenses = await licensesOfCustomer(db, id, now);
  const requests = await requestsOfCustomer(db, id);
  return {
    ...customerDto(row, licenses.length, members.length),
    notes: row.notes,
    createdBy: actorRef(row.created_by, row.created_by_label),
    members,
    licenses,
    requests,
  };
}

/** A member email from the path: normalised, or 404 when it cannot be an address. */
function memberEmail(raw: string): string {
  const email = normalizeEmail(raw);
  if (!isEmailSyntax(email)) notFound('There is no such member.');
  return email;
}

export const customerHandlers: PartialHandlerMap = {
  customersList: async (ctx) => {
    const page = q.page(ctx.query);
    return json<ResponseOf<'customersList'>>(await listCustomers(ctx.db, { ...page, q: q.string(ctx.query, 'q') }));
  },

  customerCreate: async (ctx) => {
    const body = await readJson(ctx, CreateShape);
    const detail = await inTransaction(ctx, actorFromCtx(ctx), async (s) => {
      const row = await createCustomer(s, body);
      return customerDetail(s.tx, row.id, s.now);
    });
    return json<ResponseOf<'customerCreate'>>(detail);
  },

  customerGet: async (ctx) => json<ResponseOf<'customerGet'>>(await customerDetail(ctx.db, ctx.params.id, ctx.now)),

  customerUpdate: async (ctx) => {
    const body = await readJson(ctx, UpdateShape);
    const detail = await inTransaction(ctx, actorFromCtx(ctx), async (s) => {
      await updateCustomer(s, ctx.params.id, body);
      return customerDetail(s.tx, ctx.params.id, s.now);
    });
    return json<ResponseOf<'customerUpdate'>>(detail);
  },

  memberAdd: async (ctx) => {
    const body = await readJson(ctx, AddMemberShape);
    const result = await inTransaction(ctx, actorFromCtx(ctx), (s) => addMember(s, ctx.params.id, body.email));
    return json<ResponseOf<'memberAdd'>>(result);
  },

  memberInvite: async (ctx) => {
    const email = memberEmail(ctx.params.email);
    const result = await inTransaction(ctx, actorFromCtx(ctx), (s) => reinviteMember(s, ctx.params.id, email));
    return json<ResponseOf<'memberInvite'>>(result);
  },

  memberRemove: async (ctx) => {
    const email = memberEmail(ctx.params.email);
    await inTransaction(ctx, actorFromCtx(ctx), (s) => removeMember(s, ctx.params.id, email));
    return noContent();
  },
};
