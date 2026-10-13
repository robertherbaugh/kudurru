// Personal key requests that wait for review (design spec §6 "Requests
// (admin)"): the admin list, approve and reject, and the requester's own
// view. Approval goes through the personal flow's pieces with the admin as
// actor: the person customer (an existing person customer, never an
// organization; created when missing), the requester as its member (linked
// when the request came from the portal, else an invite link in the key
// mail), then a new key, or a renewal when the person customer meanwhile
// holds an active personal license. Rejection mails the note.

import type { ApproveBody, Attestation, Items, LicenseRequest, MyRequest, RequestSource, RequestStatus, ReviewReason } from '../../shared/api';
import { PERSONAL_MAX_NODES } from '../../shared/catalog';
import { expiresOnOf } from '../../shared/dates';
import { maybeOne, type Queryable } from '../db/sql';
import { fail, HttpError, notFound } from '../http/errors';
import { assertPolicy, latestExpiresOn, personalExpiresOn, personalRenewalExpiresOn, type KeyTerms } from '../licensing/claims';
import { rejectedMail } from '../mail/templates';
import { auditCtx } from './audit';
import { actorRef, createCustomer, lockCustomer, type CustomerRow, type Fragment } from './customers';
import { checkIssuingQuota, insertLicense, licenseKeyMail, lockPersonalFlow, mailKeyAfterCommit, renewLicenseRow, termsOf, type LicenseRow, type SignedKey } from './licenses';
import { ensureMember } from './members';
import { lockNewestActivePersonal, lockPersonCustomer } from './personal';
import { actorColumns, requireSigner, type ServiceCtx } from './types';

/** A license_requests row. */
export interface RequestRow {
  id: string;
  kind: 'personal';
  source: RequestSource;
  requested_by: string | null;
  requester_email: string;
  name: string;
  use_description: string;
  attestation: Attestation;
  review_reason: ReviewReason | null;
  status: RequestStatus;
  decided_by: string | null;
  decided_by_label: string;
  decided_at: Date | null;
  decision_note: string;
  resulting_license_id: string | null;
  resulting_key_id: string | null;
  created_at: Date;
}

interface RequestListRow extends RequestRow {
  user_name: string | null;
  user_email: string | null;
  person_id: string | null;
  person_name: string | null;
}

function requestDto(r: RequestListRow): LicenseRequest {
  return {
    id: r.id,
    kind: 'personal',
    source: r.source,
    requestedBy: r.requested_by ? { id: r.requested_by, name: r.user_name ?? '', email: r.user_email } : null,
    requesterEmail: r.requester_email,
    name: r.name,
    use: r.use_description,
    attestation: r.attestation,
    reviewReason: r.review_reason,
    status: r.status,
    decidedBy: r.decided_at ? actorRef(r.decided_by, r.decided_by_label) : null,
    decidedAt: r.decided_at ? r.decided_at.toISOString() : null,
    decisionNote: r.decision_note,
    resultingLicenseId: r.resulting_license_id,
    resultingKeyId: r.resulting_key_id,
    createdAt: r.created_at.toISOString(),
    personCustomer: r.person_id ? { id: r.person_id, name: r.person_name ?? '', kind: 'person' } : null,
  };
}

/** The columns of requestDto: the request, its portal user and the person customer approval would use. */
function selectRequests(q: Queryable, where: Fragment, tail: Fragment): Promise<RequestListRow[]> {
  return q<RequestListRow[]>`
    select r.*, u.name as user_name, u.email as user_email, p.id as person_id, p.name as person_name
      from license_requests r
      left join users u on u.id = r.requested_by
      left join lateral (
        select c.id, c.name from customers c
         where c.kind = 'person' and exists (select 1 from customer_members m where m.customer_id = c.id and m.email = r.requester_email)
         order by c.created_at, c.id limit 1
      ) p on true
     ${where} ${tail}`;
}

/** GET /requests: open requests first, then the newest (at most 500). */
export async function listRequests(q: Queryable, status: RequestStatus | undefined): Promise<Items<LicenseRequest>> {
  const where = status ? q`where r.status = ${status}` : q``;
  const rows = await selectRequests(q, where, q`order by (r.status = 'open') desc, r.created_at desc, r.id limit 500`);
  return { items: rows.map(requestDto) };
}

/** One request as admins see it, or 404. */
export async function getRequest(q: Queryable, id: string): Promise<LicenseRequest> {
  const rows = await selectRequests(q, q`where r.id = ${id}`, q``);
  if (!rows[0]) notFound('There is no such request.');
  return requestDto(rows[0]);
}

/** The personal requests of a customer's people (member emails) or that led to its licenses, newest first. */
export async function requestsOfCustomer(q: Queryable, customerId: string): Promise<LicenseRequest[]> {
  const rows = await selectRequests(
    q,
    q`where r.requester_email in (select email from customer_members where customer_id = ${customerId})
          or r.resulting_license_id in (select id from licenses where customer_id = ${customerId})`,
    q`order by r.created_at desc, r.id limit 200`,
  );
  return rows.map(requestDto);
}

/** The signed-in person's own requests: made from the portal, or for their IdP-verified email from the form. */
export async function myRequests(q: Queryable, userId: string, verifiedEmail: string | null): Promise<MyRequest[]> {
  const rows = await q<RequestRow[]>`
    select * from license_requests
     where requested_by = ${userId} or (${verifiedEmail}::text is not null and requester_email = ${verifiedEmail})
     order by created_at desc, id limit 200`;
  return rows.map((r) => ({
    id: r.id,
    status: r.status,
    name: r.name,
    use: r.use_description,
    createdAt: r.created_at.toISOString(),
    decidedAt: r.decided_at ? r.decided_at.toISOString() : null,
    decisionNote: r.decision_note,
    licenseId: r.resulting_license_id,
  }));
}

async function lockOpenRequest(ctx: ServiceCtx, id: string): Promise<RequestRow> {
  const row = maybeOne(await ctx.tx<RequestRow[]>`select * from license_requests where id = ${id} for update`);
  if (!row) notFound('There is no such request.');
  if (row.status !== 'open') fail('conflict', `This request is already ${row.status}.`);
  return row;
}

/**
 * POST /requests/:id/approve: issues (or renews) the requester's personal
 * key with the admin as actor. Returns the license id.
 */
export async function approveRequest(ctx: ServiceCtx, id: string, body: ApproveBody): Promise<string> {
  requireSigner(ctx);
  await lockPersonalFlow(ctx);
  const req = await lockOpenRequest(ctx, id);
  await checkIssuingQuota(ctx);

  let customer: CustomerRow;
  if (body.customerId) {
    customer = await lockCustomer(ctx, body.customerId);
    if (customer.kind !== 'person') throw new HttpError(400, 'invalid', 'A personal key belongs to a person customer, never to an organisation.', { fields: ['customerId'] });
  } else {
    customer = (await lockPersonCustomer(ctx, req.requester_email)) ?? (await createCustomer(ctx, { name: req.name, kind: 'person', contactEmail: req.requester_email }));
  }
  const inviteUrl = await ensureMember(ctx, customer.id, req.requester_email, { linkUserId: req.requested_by, invite: req.requested_by === null });

  const existing = await lockNewestActivePersonal(ctx, customer.id);
  let result: { row: LicenseRow; key: SignedKey };
  let kind: 'issued' | 'renewed';
  if (existing) {
    const current = expiresOnOf(existing.expires_at);
    const expiresOn = body.expiresOn ?? personalRenewalExpiresOn(current, ctx.now);
    if (expiresOn <= current) throw new HttpError(400, 'invalid', `The person already holds a personal key until ${current} (UTC); a renewal must expire later.`, { fields: ['expiresOn'] });
    if (body.expiresOn === undefined && expiresOn > latestExpiresOn('personal', ctx.now)) {
      fail('conflict', `The person already holds a personal key until ${current} (UTC), too far ahead to renew it now. Reject this request, or approve it with an earlier expiry.`);
    }
    assertPolicy({ ...termsOf(existing), expiresOn }, ctx.now);
    result = await renewLicenseRow(ctx, existing, expiresOn, { requestId: id });
    kind = 'renewed';
  } else {
    const terms: KeyTerms = {
      licensee: body.licensee ?? req.name,
      plan: 'personal',
      nodes: PERSONAL_MAX_NODES,
      features: null,
      expiresOn: body.expiresOn ?? personalExpiresOn(ctx.now),
    };
    assertPolicy(terms, ctx.now);
    result = await insertLicense(ctx, { customerId: customer.id, terms, attestation: req.attestation, source: req.source, auditDetails: { requestId: id } });
    kind = 'issued';
  }
  const by = actorColumns(ctx.actor);
  await ctx.tx`
    update license_requests set status = 'approved', decided_by = ${by.id}, decided_by_label = ${by.label}, decided_at = ${ctx.now},
           resulting_license_id = ${result.row.id}, resulting_key_id = ${result.key.id}
     where id = ${id}`;
  await auditCtx(ctx, { action: 'request.approved', targetType: 'request', targetId: id, details: { licenseId: result.row.id, keyId: result.key.id, outcome: kind, customerId: customer.id } });
  mailKeyAfterCommit(ctx, licenseKeyMail(ctx, result.row, result.key.token, kind, req.requester_email, inviteUrl));
  return result.row.id;
}

/** POST /requests/:id/reject: closes the request and mails the note to the requester. */
export async function rejectRequest(ctx: ServiceCtx, id: string, note: string): Promise<void> {
  const req = await lockOpenRequest(ctx, id);
  const by = actorColumns(ctx.actor);
  await ctx.tx`
    update license_requests set status = 'rejected', decided_by = ${by.id}, decided_by_label = ${by.label}, decided_at = ${ctx.now}, decision_note = ${note}
     where id = ${id}`;
  await auditCtx(ctx, { action: 'request.rejected', targetType: 'request', targetId: id, details: { email: req.requester_email } });
  if (ctx.mailer.configured) {
    const msg = rejectedMail({ to: req.requester_email, name: req.name, note, publicUrl: ctx.config.publicUrl });
    ctx.afterCommit(() => ctx.mailer.send(msg));
  } else {
    ctx.log.warn('mail is not configured: rejection not mailed', { requestId: id });
  }
}
