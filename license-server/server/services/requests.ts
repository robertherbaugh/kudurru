// Personal key requests that wait for an admin (design spec §6 "Requests
// (admin)"; approval is required by default): the admin list, approve and
// reject (from the Requests page, or through the one-click link of the
// approvers' mail: previewApproval, decideApproval), and the requester's
// own view. Approval goes through the personal flow's pieces with the
// deciding admin as actor: the person customer (an existing person
// customer, never an organization; created when missing), the requester as
// its member (linked when the request came from the portal, else an invite
// link in the key mail), then a new key, or a renewal when the person
// customer holds an active personal license (`renews` in the list); the
// key is mailed to the requester. Rejection mails the note.

import type {
  ApprovalPreview,
  ApprovalResult,
  ApproveBody,
  Attestation,
  Items,
  JsonObject,
  LicenseRequest,
  MyRequest,
  RequestSource,
  RequestStatus,
  ReviewReason,
} from '../../shared/api';
import { PERSONAL_MAX_NODES } from '../../shared/catalog';
import { expiresOnOf } from '../../shared/dates';
import type { Config } from '../config';
import { maybeOne, type Queryable } from '../db/sql';
import { fail, HttpError, notFound } from '../http/errors';
import { assertPolicy, latestExpiresOn, personalExpiresOn, personalRenewalExpiresOn, type KeyTerms } from '../licensing/claims';
import { rejectedMail, reviewReasonText } from '../mail/templates';
import { approvalTerms, findApprovalLink, linkActor, linkSpent, lockApprovalLink, markLinkUsed, renewalTarget, stillApproves } from './approvals';
import { auditCtx } from './audit';
import { actorRef, createCustomer, lockCustomer, type CustomerRow, type Fragment } from './customers';
import { checkIssuingQuota, insertLicense, licenseKeyMail, lockPersonalFlow, mailKeyAfterCommit, renewLicenseRow, termsOf, type LicenseRow, type SignedKey } from './licenses';
import { ensureMember } from './members';
import { lockNewestActivePersonal, lockPersonCustomer, personalReviewReason } from './personal';
import { actorColumns, inTransaction, requireSigner, type Actor, type ServiceCtx, type ServiceDeps } from './types';

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
  renews_id: string | null;
  renews_expires_at: Date | null;
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
    renews: r.status === 'open' && r.renews_id && r.renews_expires_at ? { licenseId: r.renews_id, expiresOn: expiresOnOf(r.renews_expires_at) } : null,
  };
}

/**
 * The columns of requestDto: the request, its portal user, the person
 * customer approval would use and the license it would renew (that
 * customer's newest active personal license; approvals.ts renewalTarget).
 */
function selectRequests(q: Queryable, where: Fragment, tail: Fragment): Promise<RequestListRow[]> {
  return q<RequestListRow[]>`
    select r.*, u.name as user_name, u.email as user_email, p.id as person_id, p.name as person_name,
           rl.id as renews_id, rl.expires_at as renews_expires_at
      from license_requests r
      left join users u on u.id = r.requested_by
      left join lateral (
        select c.id, c.name from customers c
         where c.kind = 'person' and exists (select 1 from customer_members m where m.customer_id = c.id and m.email = r.requester_email)
         order by c.created_at, c.id limit 1
      ) p on true
      left join lateral (
        select l.id, l.expires_at from licenses l
         where l.customer_id = p.id and l.plan = 'personal' and l.status = 'active'
         order by l.created_at desc, l.id desc limit 1
      ) rl on true
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

/** How a decision was made, for the audit details: through a one-click link of the approvers' mail, and to whom it went. */
export interface DecisionVia {
  via: 'approval_link';
  sentTo: string;
}

/**
 * POST /requests/:id/approve (and the approval link): issues (or renews)
 * the requester's personal key with the deciding admin as actor and mails
 * it. Returns the license and whether it was issued or renewed.
 */
export async function approveRequest(ctx: ServiceCtx, id: string, body: ApproveBody, via?: DecisionVia): Promise<{ licenseId: string; outcome: 'issued' | 'renewed' }> {
  requireSigner(ctx);
  await lockPersonalFlow(ctx);
  const req = await lockOpenRequest(ctx, id);
  await checkIssuingQuota(ctx);

  let customer: CustomerRow;
  if (body.customerId) {
    customer = await lockCustomer(ctx, body.customerId);
    if (customer.kind !== 'person') throw new HttpError(400, 'invalid', 'A personal key belongs to a person customer, never to an organization.', { fields: ['customerId'] });
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
    result = await renewLicenseRow(ctx, existing, expiresOn, { requestId: id, ...(via ? { via: via.via } : {}) });
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
    result = await insertLicense(ctx, { customerId: customer.id, terms, attestation: req.attestation, source: req.source, auditDetails: { requestId: id, ...(via ? { via: via.via } : {}) } });
    kind = 'issued';
  }
  const by = actorColumns(ctx.actor);
  await ctx.tx`
    update license_requests set status = 'approved', decided_by = ${by.id}, decided_by_label = ${by.label}, decided_at = ${ctx.now},
           resulting_license_id = ${result.row.id}, resulting_key_id = ${result.key.id}
     where id = ${id}`;
  await auditCtx(ctx, {
    action: 'request.approved',
    targetType: 'request',
    targetId: id,
    details: { licenseId: result.row.id, keyId: result.key.id, outcome: kind, customerId: customer.id, ...viaDetails(via) },
  });
  mailKeyAfterCommit(ctx, licenseKeyMail(ctx, result.row, result.key.token, kind, req.requester_email, inviteUrl, { approved: true }));
  return { licenseId: result.row.id, outcome: kind };
}

function viaDetails(via: DecisionVia | undefined): JsonObject {
  return via ? { via: via.via, sentTo: via.sentTo } : {};
}

/** POST /requests/:id/reject (and the approval link): closes the request and mails the note to the requester. */
export async function rejectRequest(ctx: ServiceCtx, id: string, note: string, via?: DecisionVia): Promise<void> {
  const req = await lockOpenRequest(ctx, id);
  const by = actorColumns(ctx.actor);
  await ctx.tx`
    update license_requests set status = 'rejected', decided_by = ${by.id}, decided_by_label = ${by.label}, decided_at = ${ctx.now}, decision_note = ${note}
     where id = ${id}`;
  await auditCtx(ctx, { action: 'request.rejected', targetType: 'request', targetId: id, details: { email: req.requester_email, ...viaDetails(via) } });
  if (ctx.mailer.configured) {
    const msg = rejectedMail({ to: req.requester_email, name: req.name, note, publicUrl: ctx.config.publicUrl });
    ctx.afterCommit(() => ctx.mailer.send(msg, ctx.log));
  } else {
    ctx.log.warn('mail is not configured: rejection not mailed', { requestId: id });
  }
}

// --- the one-click approval link (approvals.ts) ------------------------------------

/**
 * POST /api/v1/public/approvals/preview: what the /approve page shows for a
 * link, before anyone decides. Changes nothing. A used or expired link (or
 * one whose address no longer approves) still previews: expired, status
 * and decidedBy say why it cannot decide. An unknown token is 400
 * invalid_token. The terms are what approving now signs (a renewal keeps
 * the license's nodes).
 */
export async function previewApproval(q: Queryable, config: Pick<Config, 'mail'>, token: string, now: Date): Promise<ApprovalPreview> {
  const link = await findApprovalLink(q, token);
  const r = await getRequest(q, link.request_id);
  const renews = r.status === 'open' ? await renewalTarget(q, r.requesterEmail) : null;
  return {
    request: {
      id: r.id,
      name: r.name,
      email: r.requesterEmail,
      use: r.use,
      source: r.source,
      createdAt: r.createdAt,
      reviewReason: r.reviewReason,
      attestation: { at: r.attestation.at ?? null, statement: r.attestation.statement ?? null },
    },
    renews: renews ? { licenseId: renews.licenseId, expiresOn: renews.expiresOn } : null,
    terms: approvalTerms(renews, now),
    status: r.status,
    decidedBy: r.status !== 'open' && r.decidedBy ? r.decidedBy.label : null,
    expired: linkSpent(link, now) || !(await stillApproves(q, config, link.sent_to, now)),
    sentTo: link.sent_to,
  };
}

/** The actor until the link's own is known (decideApproval replaces it before anything is written). */
const LINK_PENDING_ACTOR: Actor = { kind: 'system', label: 'approval link' };

/**
 * POST /api/v1/public/approvals/decide: approves (default terms) or
 * rejects the link's request, in one transaction: the personal-flow lock,
 * the link locked (unused, unexpired and its address still an approver,
 * else 400 invalid_token), the request locked and still open (else 409
 * conflict), for an approval the review rules checked again (a blocked
 * address or a withdrawn license since the mail: 409 conflict, decide it in
 * the portal), the link marked used, then approveRequest or rejectRequest
 * with the link's actor (approvals.ts linkActor, with its issuing quota)
 * and `via: approval_link` in the audit details. A failure rolls the whole
 * decision back, link included. Never answers the key.
 */
export async function decideApproval(deps: ServiceDeps, token: string, decision: { decision: 'approve' } | { decision: 'reject'; note: string }): Promise<ApprovalResult> {
  return inTransaction(deps, LINK_PENDING_ACTOR, async (base) => {
    await lockPersonalFlow(base);
    const link = await lockApprovalLink(base, token);
    const request = maybeOne(await base.tx<{ status: RequestStatus; requester_email: string }[]>`
      select status, requester_email from license_requests where id = ${link.request_id} for update`);
    if (!request) notFound('There is no such request.');
    if (request.status !== 'open') fail('conflict', `This request is already decided (${request.status}): the link changes nothing.`);
    if (decision.decision === 'approve') {
      // The mail went out when the request was clean; the address may have
      // been blocked, or a license of the person withdrawn, since.
      const person = await lockPersonCustomer(base, request.requester_email);
      const reason = await personalReviewReason(base, [request.requester_email], person?.id ?? null);
      if (reason) {
        fail('conflict', `This request can no longer be approved through a link: ${reviewReasonText(reason)} A license admin decides it in the portal after signing in.`, {
          hint: `${base.config.publicUrl}/requests`,
        });
      }
    }
    const ctx: ServiceCtx = { ...base, actor: await linkActor(base.tx, link.sent_to, base.now) };
    await markLinkUsed(ctx, link);
    const via: DecisionVia = { via: 'approval_link', sentTo: link.sent_to };
    if (decision.decision === 'approve') {
      const approved = await approveRequest(ctx, link.request_id, {}, via);
      return { status: 'approved', licenseId: approved.licenseId, outcome: approved.outcome };
    }
    await rejectRequest(ctx, link.request_id, decision.note, via);
    return { status: 'rejected' };
  });
}
