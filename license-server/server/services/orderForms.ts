// Order Forms (README, "Order Forms"; migration 0006; shared/orderForms.ts):
// Schedule A of the commercial agreement, drafted by a license admin,
// accepted electronically by one of the customer's authorized contacts and
// countersigned by a license admin. Statuses: draft → sent → accepted →
// executed; sent or declined → draft again (revise, a new version); sent →
// declined (by the customer, with a note); anything but executed → void.
//
// Sending freezes the text: the document (orderFormDocument without
// signatures) is hashed, and each acceptance records the hash it accepted,
// so a revised form can never be accepted by an answer to the old one (the
// accept and countersign bodies name the hash they saw: 409 otherwise).
// Sending also makes every authorized contact a member of the customer (an
// invite link for those without access, in their mail): only a signed-in
// member whose membership address is an authorized contact accepts or
// declines. A customer's people see a form once it was sent, never a draft,
// never the void reason or who did what in the back office.
//
// An executed form is final; one license may be issued from it (POST
// /licenses with orderFormId: lockOrderFormForLicense before issuing,
// markOrderFormLicensed after, in one transaction), which also marks the
// business request it was drafted from issued while that is still open.
// Every action locks the row (then the customer where members change, the
// order issuing takes too), audits in the same transaction and mails after
// the commit.

import { createHash } from 'node:crypto';
import type {
  CustomerKind,
  Items,
  MyOrderForm,
  NewCustomerBody,
  OrderForm,
  OrderFormListQuery,
  OrderFormSignature,
  OrderFormStatus,
  OrderFormSummary,
  OrderFormTerms,
} from '../../shared/api';
import { orderFormDocument, orderFormGaps, orderFormRules } from '../../shared/orderForms';
import { newId } from '../db/ids';
import { maybeOne, type Queryable } from '../db/sql';
import { fail, HttpError, notFound } from '../http/errors';
import { orderFormAnsweredMail, orderFormExecutedMail, orderFormSentMail, orderFormVoidedMail, type OrderFormFacts } from '../mail/templates';
import { approvalRecipients } from './approvals';
import { auditCtx } from './audit';
import { lockOpenBusinessRequest, markBusinessRequestIssued } from './businessRequests';
import { actorRef, createCustomer, customerRef, getCustomerRow, lockCustomer } from './customers';
import { ensureMember } from './members';
import { actorColumns, type ServiceCtx } from './types';

/** The stored acceptance of the customer's authorized contact. */
interface CustomerAcceptanceJson extends OrderFormSignature {
  userId: string | null;
  ip: string | null;
  userAgent: string | null;
}

/** The stored countersignature. */
interface LicensorAcceptanceJson extends OrderFormSignature {
  byId: string | null;
  byLabel: string;
}

/** An order_forms row (with its customer's name and kind for the DTOs). */
interface OrderFormRow {
  id: string;
  number: string;
  customer_id: string;
  business_request_id: string | null;
  status: OrderFormStatus;
  version: number;
  terms: OrderFormTerms;
  document_sha256: string | null;
  sent_at: Date | null;
  sent_by: string | null;
  sent_by_label: string;
  customer_acceptance: CustomerAcceptanceJson | null;
  licensor_acceptance: LicensorAcceptanceJson | null;
  declined_at: Date | null;
  declined_by_label: string;
  decline_note: string;
  voided_at: Date | null;
  voided_by: string | null;
  voided_by_label: string;
  void_reason: string;
  license_id: string | null;
  created_at: Date;
  created_by: string | null;
  created_by_label: string;
  updated_at: Date;
}

interface OrderFormListRow extends OrderFormRow {
  customer_name: string;
  customer_kind: CustomerKind;
}

function summaryDto(r: OrderFormListRow): OrderFormSummary {
  return {
    id: r.id,
    number: r.number,
    status: r.status,
    version: r.version,
    customer: customerRef({ id: r.customer_id, name: r.customer_name, kind: r.customer_kind }),
    edition: r.terms.edition,
    nodes: r.terms.nodes,
    annualFeeCents: r.terms.annualFeeCents,
    subscriptionStart: r.terms.subscriptionStart,
    subscriptionEnd: r.terms.subscriptionEnd,
    sentAt: r.sent_at ? r.sent_at.toISOString() : null,
    licenseId: r.license_id,
    createdAt: r.created_at.toISOString(),
    updatedAt: r.updated_at.toISOString(),
  };
}

function signatureOf(s: OrderFormSignature): OrderFormSignature {
  return { name: s.name, title: s.title, email: s.email, at: s.at, documentSha256: s.documentSha256 };
}

function detailDto(r: OrderFormListRow): OrderForm {
  const c = r.customer_acceptance;
  const l = r.licensor_acceptance;
  return {
    ...summaryDto(r),
    terms: r.terms,
    documentSha256: r.document_sha256,
    businessRequestId: r.business_request_id,
    sentBy: r.sent_at ? actorRef(r.sent_by, r.sent_by_label) : null,
    customerAcceptance: c ? { ...signatureOf(c), userId: c.userId, ip: c.ip, userAgent: c.userAgent } : null,
    licensorAcceptance: l ? { ...signatureOf(l), by: actorRef(l.byId, l.byLabel) } : null,
    declined: r.declined_at ? { at: r.declined_at.toISOString(), by: r.declined_by_label, note: r.decline_note } : null,
    voided: r.voided_at ? { at: r.voided_at.toISOString(), by: actorRef(r.voided_by, r.voided_by_label), reason: r.void_reason } : null,
    createdBy: actorRef(r.created_by, r.created_by_label),
    gaps: r.status === 'draft' ? orderFormGaps(r.terms).map((f) => `terms.${f}`) : [],
  };
}

function myDto(r: OrderFormListRow, authorized: boolean): MyOrderForm {
  return {
    id: r.id,
    number: r.number,
    status: r.status,
    version: r.version,
    customer: { id: r.customer_id, name: r.customer_name },
    terms: r.terms,
    documentSha256: r.document_sha256 ?? '',
    sentAt: (r.sent_at ?? r.created_at).toISOString(),
    customerAcceptance: r.customer_acceptance ? signatureOf(r.customer_acceptance) : null,
    licensorAcceptance: r.licensor_acceptance ? signatureOf(r.licensor_acceptance) : null,
    declined: r.declined_at ? { at: r.declined_at.toISOString(), note: r.decline_note } : null,
    voidedAt: r.voided_at ? r.voided_at.toISOString() : null,
    authorized,
    licenseId: r.license_id,
  };
}

/** SHA-256 (hex) of the unsigned document of these terms: what each party accepts. */
export function documentSha256(number: string, version: number, terms: OrderFormTerms): string {
  return createHash('sha256').update(orderFormDocument({ number, version, terms }), 'utf8').digest('hex');
}

/** The facts the mails name. */
function factsOf(r: OrderFormRow, customerName: string): OrderFormFacts {
  return { id: r.id, number: r.number, version: r.version, customerName, terms: r.terms, documentSha256: r.document_sha256 ?? '' };
}

/** 400 invalid with `terms.<field>` for every rule the terms break (shared/orderForms.ts orderFormRules). */
export function assertOrderFormRules(terms: OrderFormTerms): void {
  const problems = orderFormRules(terms);
  if (problems.length === 0) return;
  throw new HttpError(400, 'invalid', problems[0]!.message, { fields: [...new Set(problems.map((p) => `terms.${p.field}`))] });
}

// --- reads ----------------------------------------------------------------------

async function findRow(q: Queryable, id: string): Promise<OrderFormListRow | null> {
  return maybeOne(await q<OrderFormListRow[]>`select f.*, c.name as customer_name, c.kind as customer_kind from order_forms f join customers c on c.id = f.customer_id where f.id = ${id}`);
}

/** GET /order-forms: open first (draft, sent, accepted), then the newest; at most 500. */
export async function listOrderForms(q: Queryable, query: OrderFormListQuery): Promise<Items<OrderFormSummary>> {
  const status = query.status ? q`and f.status = ${query.status}` : q``;
  const customer = query.customerId ? q`and f.customer_id = ${query.customerId}` : q``;
  const rows = await q<OrderFormListRow[]>`
    select f.*, c.name as customer_name, c.kind as customer_kind from order_forms f join customers c on c.id = f.customer_id
     where true ${status} ${customer}
     order by (f.status in ('draft', 'sent', 'accepted')) desc, f.created_at desc, f.id limit 500`;
  return { items: rows.map(summaryDto) };
}

/** One Order Form (admin), or 404. */
export async function getOrderForm(q: Queryable, id: string): Promise<OrderForm> {
  const row = await findRow(q, id);
  if (!row) notFound('There is no such Order Form.');
  return detailDto(row);
}

/** The forms of a customer (the customer page). */
export async function orderFormsOfCustomer(q: Queryable, customerId: string): Promise<OrderFormSummary[]> {
  return (await listOrderForms(q, { customerId })).items;
}

/** The membership addresses the user is linked with at a customer. */
async function memberEmailsOf(q: Queryable, customerId: string, userId: string): Promise<string[]> {
  const rows = await q<{ email: string }[]>`select email from customer_members where customer_id = ${customerId} and user_id = ${userId}`;
  return rows.map((r) => r.email);
}

/** The authorized contact address the user acts as, or null. */
function authorizedAddress(terms: OrderFormTerms, memberEmails: readonly string[]): string | null {
  return memberEmails.find((e) => terms.authorizedContacts.includes(e)) ?? null;
}

/** GET /me/order-forms: the sent forms of every customer the user belongs to, open first. */
export async function listMyOrderForms(q: Queryable, userId: string): Promise<Items<MyOrderForm>> {
  const rows = await q<(OrderFormListRow & { member_emails: string[] })[]>`
    select f.*, c.name as customer_name, c.kind as customer_kind,
           array(select m.email from customer_members m where m.customer_id = f.customer_id and m.user_id = ${userId}) as member_emails
      from order_forms f join customers c on c.id = f.customer_id
     where f.document_sha256 is not null
       and exists (select 1 from customer_members m where m.customer_id = f.customer_id and m.user_id = ${userId})
     order by (f.status in ('sent', 'accepted')) desc, f.sent_at desc, f.id
     limit 500`;
  return { items: rows.map((r) => myDto(r, authorizedAddress(r.terms, r.member_emails) !== null)) };
}

/** GET /me/order-forms/:id: 404 unless the user belongs to its customer and it was sent. */
export async function getMyOrderForm(q: Queryable, userId: string, id: string): Promise<MyOrderForm> {
  const row = await findRow(q, id);
  if (!row || row.document_sha256 === null) notFound('There is no such Order Form.');
  const emails = await memberEmailsOf(q, row.customer_id, userId);
  if (emails.length === 0) notFound('There is no such Order Form.');
  return myDto(row, authorizedAddress(row.terms, emails) !== null);
}

// --- locks ----------------------------------------------------------------------

async function lockRow(ctx: Pick<ServiceCtx, 'tx'>, id: string): Promise<OrderFormRow> {
  const row = maybeOne(await ctx.tx<OrderFormRow[]>`select * from order_forms where id = ${id} for update`);
  if (!row) notFound('There is no such Order Form.');
  return row;
}

const STATUS_WORDS: Record<OrderFormStatus, string> = {
  draft: 'a draft',
  sent: 'sent for acceptance',
  accepted: 'accepted and waits for the countersignature',
  executed: 'executed',
  declined: 'declined',
  void: 'void',
};

/** 409 conflict unless the form is in one of `allowed`. */
function expectStatus(row: OrderFormRow, allowed: readonly OrderFormStatus[], doing: string): void {
  if (!allowed.includes(row.status)) fail('conflict', `This Order Form is ${STATUS_WORDS[row.status]}: it cannot be ${doing}.`);
}

/** 409 conflict when the hash the person saw is not the one sent (the form was revised in the meantime). */
function expectHash(row: OrderFormRow, seen: string): void {
  if (row.document_sha256 !== seen) fail('conflict', 'This Order Form changed since it was opened. Reload it and read it again.');
}

// --- writes ---------------------------------------------------------------------

export interface NewOrderForm {
  terms: OrderFormTerms;
  customerId?: string;
  newCustomer?: NewCustomerBody;
  businessRequestId?: string;
}

/** The next Order Form No.: OF-<year>-<number>. */
async function nextNumber(ctx: ServiceCtx): Promise<string> {
  const rows = await ctx.tx<{ n: string }[]>`select nextval('order_form_number_seq')::text as n`;
  return `OF-${ctx.now.getUTCFullYear()}-${rows[0]!.n.padStart(4, '0')}`;
}

/** POST /order-forms: a draft, for an existing customer or a new one created with it. An empty licensor address takes the last form's. */
export async function createOrderForm(ctx: ServiceCtx, input: NewOrderForm): Promise<string> {
  assertOrderFormRules(input.terms);
  if (input.businessRequestId) await lockOpenBusinessRequest(ctx, input.businessRequestId);
  let customerId: string;
  if (input.newCustomer) {
    const c = await createCustomer(ctx, { name: input.newCustomer.name, kind: input.newCustomer.kind, contactEmail: input.newCustomer.contactEmail ?? null });
    customerId = c.id;
  } else {
    customerId = (await getCustomerRow(ctx.tx, input.customerId!)).id;
  }
  const terms = { ...input.terms };
  if (terms.licensorContact.trim() === '') {
    const last = maybeOne(await ctx.tx<{ contact: string }[]>`
      select terms->>'licensorContact' as contact from order_forms
       where coalesce(terms->>'licensorContact', '') <> '' order by created_at desc, id desc limit 1`);
    if (last) terms.licensorContact = last.contact;
  }
  const id = newId('orderForm');
  const number = await nextNumber(ctx);
  const by = actorColumns(ctx.actor);
  await ctx.tx`
    insert into order_forms (id, number, customer_id, business_request_id, terms, created_at, created_by, created_by_label, updated_at)
    values (${id}, ${number}, ${customerId}, ${input.businessRequestId ?? null}, ${terms}, ${ctx.now}, ${by.id}, ${by.label}, ${ctx.now})`;
  await auditCtx(ctx, {
    action: 'order_form.created',
    targetType: 'order_form',
    targetId: id,
    details: { number, customerId, businessRequestId: input.businessRequestId ?? null, edition: terms.edition, nodes: terms.nodes, annualFeeCents: terms.annualFeeCents },
  });
  return id;
}

/** PATCH /order-forms/:id (draft only): the merged terms, checked by the caller's validator and the rules. */
export async function updateOrderForm(ctx: ServiceCtx, id: string, terms: OrderFormTerms): Promise<void> {
  const row = await lockRow(ctx, id);
  expectStatus(row, ['draft'], 'changed (revise it first)');
  assertOrderFormRules(terms);
  const changed = (Object.keys(terms) as (keyof OrderFormTerms)[]).filter((k) => JSON.stringify(terms[k]) !== JSON.stringify(row.terms[k]));
  if (changed.length === 0) return;
  await ctx.tx`update order_forms set terms = ${terms}, updated_at = ${ctx.now} where id = ${id}`;
  await auditCtx(ctx, { action: 'order_form.updated', targetType: 'order_form', targetId: id, details: { number: row.number, changed } });
}

/** The stored terms of a form, for the PATCH merge. */
export async function storedTerms(q: Queryable, id: string): Promise<OrderFormTerms> {
  const row = maybeOne(await q<{ terms: OrderFormTerms }[]>`select terms from order_forms where id = ${id}`);
  if (!row) notFound('There is no such Order Form.');
  return row.terms;
}

/**
 * POST /order-forms/:id/send (draft only, no gaps): freezes and hashes the
 * text, makes every authorized contact a member of the customer and mails
 * each one the form (with an invite link when they have no access yet).
 */
export async function sendOrderForm(ctx: ServiceCtx, id: string): Promise<void> {
  // The form before its customer, as issuing a license from a form locks them.
  const row = await lockRow(ctx, id);
  expectStatus(row, ['draft'], 'sent');
  const customer = await lockCustomer(ctx, row.customer_id);
  const gaps = orderFormGaps(row.terms);
  if (gaps.length > 0) throw new HttpError(400, 'invalid', 'Fill in every term before sending the Order Form.', { fields: gaps.map((f) => `terms.${f}`) });
  assertOrderFormRules(row.terms);
  const sha = documentSha256(row.number, row.version, row.terms);
  const by = actorColumns(ctx.actor);
  const rows = await ctx.tx<OrderFormRow[]>`
    update order_forms set status = 'sent', document_sha256 = ${sha}, sent_at = ${ctx.now}, sent_by = ${by.id}, sent_by_label = ${by.label}, updated_at = ${ctx.now}
     where id = ${id} returning *`;
  const sent = rows[0]!;
  const invites: { to: string; inviteUrl: string | null }[] = [];
  for (const to of row.terms.authorizedContacts) invites.push({ to, inviteUrl: await ensureMember(ctx, row.customer_id, to, { invite: true }) });
  await auditCtx(ctx, { action: 'order_form.sent', targetType: 'order_form', targetId: id, details: { number: row.number, version: row.version, documentSha256: sha, to: [...row.terms.authorizedContacts] } });
  if (!ctx.mailer.configured) {
    ctx.log.warn('order form sent, nobody mailed', { orderFormId: id, why: 'mail is not configured' });
    return;
  }
  const facts = factsOf(sent, customer.name);
  for (const i of invites) {
    const msg = orderFormSentMail({ to: i.to, form: facts, inviteUrl: i.inviteUrl, publicUrl: ctx.config.publicUrl });
    ctx.afterCommit(() => ctx.mailer.send(msg, ctx.log));
  }
}

/** POST /order-forms/:id/revise (sent or declined): back to a draft, as the next version; the sent text and any decline are cleared (the audit trail keeps them). */
export async function reviseOrderForm(ctx: ServiceCtx, id: string): Promise<void> {
  const row = await lockRow(ctx, id);
  expectStatus(row, ['sent', 'declined'], 'revised');
  await ctx.tx`
    update order_forms set status = 'draft', version = version + 1, document_sha256 = null, sent_at = null, sent_by = null, sent_by_label = '',
                           declined_at = null, declined_by_label = '', decline_note = '', updated_at = ${ctx.now}
     where id = ${id}`;
  await auditCtx(ctx, {
    action: 'order_form.revised',
    targetType: 'order_form',
    targetId: id,
    details: { number: row.number, from: row.status, version: row.version + 1, previousSha256: row.document_sha256, declineNote: row.decline_note || null },
  });
}

/** POST /order-forms/:id/void (anything but executed): final; the contacts of a sent form are told it was withdrawn (never the reason). */
export async function voidOrderForm(ctx: ServiceCtx, id: string, reason: string): Promise<void> {
  const row = await lockRow(ctx, id);
  expectStatus(row, ['draft', 'sent', 'accepted', 'declined'], 'voided');
  const by = actorColumns(ctx.actor);
  await ctx.tx`
    update order_forms set status = 'void', voided_at = ${ctx.now}, voided_by = ${by.id}, voided_by_label = ${by.label}, void_reason = ${reason}, updated_at = ${ctx.now}
     where id = ${id}`;
  await auditCtx(ctx, { action: 'order_form.voided', targetType: 'order_form', targetId: id, details: { number: row.number, from: row.status, reason } });
  if (row.document_sha256 === null || !ctx.mailer.configured) return;
  const customer = await getCustomerRow(ctx.tx, row.customer_id);
  for (const to of row.terms.authorizedContacts) {
    const msg = orderFormVoidedMail({ to, number: row.number, customerName: customer.name, publicUrl: ctx.config.publicUrl });
    ctx.afterCommit(() => ctx.mailer.send(msg, ctx.log));
  }
}

/** Queues one mail per approver (ADMIN_NOTIFY_EMAIL, or the license admins and superadmins). */
async function mailApprovers(ctx: ServiceCtx, build: (to: string) => ReturnType<typeof orderFormAnsweredMail>, orderFormId: string): Promise<void> {
  if (!ctx.mailer.configured) {
    ctx.log.warn('order form answered, nobody mailed', { orderFormId, why: 'mail is not configured' });
    return;
  }
  const { recipients } = await approvalRecipients(ctx.tx, ctx.config, ctx.now);
  if (recipients.length === 0) ctx.log.warn('order form answered, no admin mailed', { orderFormId, why: 'ADMIN_NOTIFY_EMAIL is unset and no enabled license admin or superadmin has a verified email' });
  for (const to of recipients) {
    const msg = build(to);
    ctx.afterCommit(() => ctx.mailer.send(msg, ctx.log));
  }
}

/** The locked form and the address the signed-in member answers as; 404 for a non-member or a draft, 403 for a member who is no authorized contact. */
async function lockForCustomer(ctx: ServiceCtx, id: string, userId: string): Promise<{ row: OrderFormRow; email: string }> {
  const row = await lockRow(ctx, id);
  const emails = row.document_sha256 === null ? [] : await memberEmailsOf(ctx.tx, row.customer_id, userId);
  if (emails.length === 0) notFound('There is no such Order Form.');
  const email = authorizedAddress(row.terms, emails);
  if (!email) fail('forbidden', `Only the authorized contacts of this Order Form can answer it: ${row.terms.authorizedContacts.join(', ')}.`);
  return { row, email };
}

export interface CustomerAnswer {
  name: string;
  title: string;
  documentSha256: string;
}

/** POST /me/order-forms/:id/accept (sent only, an authorized contact): the customer's electronic acceptance of the hash they read. */
export async function acceptOrderForm(ctx: ServiceCtx, id: string, userId: string, answer: CustomerAnswer): Promise<void> {
  const { row, email } = await lockForCustomer(ctx, id, userId);
  expectStatus(row, ['sent'], 'accepted');
  expectHash(row, answer.documentSha256);
  const acceptance: CustomerAcceptanceJson = {
    name: answer.name,
    title: answer.title,
    email,
    at: ctx.now.toISOString(),
    documentSha256: row.document_sha256!,
    userId,
    ip: ctx.ip ?? null,
    userAgent: ctx.userAgent ?? null,
  };
  await ctx.tx`update order_forms set status = 'accepted', customer_acceptance = ${acceptance}, updated_at = ${ctx.now} where id = ${id}`;
  await auditCtx(ctx, { action: 'order_form.accepted', targetType: 'order_form', targetId: id, details: { number: row.number, version: row.version, documentSha256: row.document_sha256, name: answer.name, title: answer.title, email } });
  const customer = await getCustomerRow(ctx.tx, row.customer_id);
  const facts = factsOf(row, customer.name);
  await mailApprovers(ctx, (to) => orderFormAnsweredMail({ to, form: facts, by: { name: answer.name, title: answer.title, email }, answer: { kind: 'accepted' }, publicUrl: ctx.config.publicUrl }), id);
}

/** POST /me/order-forms/:id/decline (sent only, an authorized contact): with a note for the licensor. */
export async function declineOrderForm(ctx: ServiceCtx, id: string, userId: string, note: string, name: string): Promise<void> {
  const { row, email } = await lockForCustomer(ctx, id, userId);
  expectStatus(row, ['sent'], 'declined');
  const label = name.trim() ? `${name.trim()} (${email})` : email;
  await ctx.tx`
    update order_forms set status = 'declined', declined_at = ${ctx.now}, declined_by_label = ${label}, decline_note = ${note}, updated_at = ${ctx.now}
     where id = ${id}`;
  await auditCtx(ctx, { action: 'order_form.declined', targetType: 'order_form', targetId: id, details: { number: row.number, version: row.version, email, note } });
  const customer = await getCustomerRow(ctx.tx, row.customer_id);
  const facts = factsOf(row, customer.name);
  await mailApprovers(ctx, (to) => orderFormAnsweredMail({ to, form: facts, by: { name: name.trim() || email, title: '', email }, answer: { kind: 'declined', note }, publicUrl: ctx.config.publicUrl }), id);
}

export interface Countersignature {
  name: string;
  title: string;
  documentSha256: string;
  /** The admin's verified email, if any. */
  email: string | null;
}

/** POST /order-forms/:id/countersign (accepted only): executed; the contacts get the signed copy. */
export async function countersignOrderForm(ctx: ServiceCtx, id: string, sig: Countersignature): Promise<void> {
  const row = await lockRow(ctx, id);
  expectStatus(row, ['accepted'], 'countersigned');
  expectHash(row, sig.documentSha256);
  const by = actorColumns(ctx.actor);
  const acceptance: LicensorAcceptanceJson = { name: sig.name, title: sig.title, email: sig.email, at: ctx.now.toISOString(), documentSha256: row.document_sha256!, byId: by.id, byLabel: by.label };
  const rows = await ctx.tx<OrderFormRow[]>`update order_forms set status = 'executed', licensor_acceptance = ${acceptance}, updated_at = ${ctx.now} where id = ${id} returning *`;
  const done = rows[0]!;
  await auditCtx(ctx, { action: 'order_form.countersigned', targetType: 'order_form', targetId: id, details: { number: row.number, version: row.version, documentSha256: row.document_sha256, name: sig.name, title: sig.title } });
  if (!ctx.mailer.configured) {
    ctx.log.warn('order form executed, nobody mailed', { orderFormId: id, why: 'mail is not configured' });
    return;
  }
  const customer = await getCustomerRow(ctx.tx, row.customer_id);
  const document = orderFormDocument({
    number: done.number,
    version: done.version,
    terms: done.terms,
    signatures: { customer: done.customer_acceptance, licensor: done.licensor_acceptance, documentSha256: done.document_sha256! },
  });
  const facts = factsOf(done, customer.name);
  for (const to of done.terms.authorizedContacts) {
    const msg = orderFormExecutedMail({ to, form: facts, document, publicUrl: ctx.config.publicUrl });
    ctx.afterCommit(() => ctx.mailer.send(msg, ctx.log));
  }
}

// --- issuing a license from a form -------------------------------------------------

/** For POST /licenses with orderFormId: the executed form, locked, without a license yet, for this customer. */
export async function lockOrderFormForLicense(ctx: ServiceCtx, id: string, customerId: string | undefined): Promise<{ businessRequestId: string | null }> {
  const row = await lockRow(ctx, id);
  if (row.status !== 'executed') fail('conflict', `This Order Form is ${STATUS_WORDS[row.status]}: a license is issued only from an executed Order Form.`);
  if (row.license_id) fail('conflict', `A license was already issued from this Order Form (${row.license_id}).`);
  if (customerId !== row.customer_id) throw new HttpError(400, 'invalid', 'A license from an Order Form goes to the Order Form’s customer.', { fields: ['customerId'] });
  if (!row.business_request_id) return { businessRequestId: null };
  const open = maybeOne(await ctx.tx<{ id: string }[]>`select id from business_requests where id = ${row.business_request_id} and status = 'open' for update`);
  return { businessRequestId: open ? open.id : null };
}

/** After issuing (lockOrderFormForLicense first): the form records the license; its open business request becomes issued. */
export async function markOrderFormLicensed(ctx: ServiceCtx, id: string, licenseId: string, businessRequestId: string | null): Promise<void> {
  await ctx.tx`update order_forms set license_id = ${licenseId}, updated_at = ${ctx.now} where id = ${id}`;
  await auditCtx(ctx, { action: 'order_form.licensed', targetType: 'order_form', targetId: id, details: { licenseId } });
  if (businessRequestId) await markBusinessRequestIssued(ctx, businessRequestId, licenseId);
}
