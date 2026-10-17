// Business license requests from the kudurru.io form (README, "Business
// requests"; migration 0005). Unlike the personal form there is no
// confirmation link: nothing is signed until a license admin issues a
// license, so the request is stored at once and shown on the Requests page
// (Business).
//
// Submit: honeypot filled → 202 and nothing happens (a log line); per IPv4
// address or IPv6 /64 at most PUBLIC_LIMIT_PER_IP_PER_HOUR business requests
// in the last hour → 429 rate_limited with Retry-After seconds; per email at
// most PUBLIC_LIMIT_PER_EMAIL_PER_DAY in the last day → a silent 202 (a log
// line); otherwise the row, then, after the commit, the requester's "we have
// your request" mail and one mail per approver (approvals.ts
// approvalRecipients: ADMIN_NOTIFY_EMAIL, or the license admins and
// superadmins). Without mail the request is still stored (a warning): the
// Requests page shows it. The audit event names neither the address nor the
// client, as for the personal form: the address is not confirmed.
//
// Decide: a license admin issues a license from an open request (the issue
// wizard sends businessRequestId with POST /licenses: lockOpenBusinessRequest
// before issuing, markBusinessRequestIssued after, in one transaction) or
// closes it with a note (no mail: the admin replies by email themselves).

import { isIP } from 'node:net';
import type { BusinessRequest, BusinessRequestStatus, Items } from '../../shared/api';
import type { BusinessEdition } from '../../shared/catalog';
import { newId } from '../db/ids';
import { maybeOne, toInt, type Queryable } from '../db/sql';
import { fail, HttpError, notFound } from '../http/errors';
import { businessReceivedMail, businessRequestNeededMail, type BusinessRequestFacts } from '../mail/templates';
import { approvalRecipients } from './approvals';
import { audit, auditCtx } from './audit';
import { actorRef } from './customers';
import { actorColumns, inTransaction, publicActor, type Actor, type ServiceCtx } from './types';
import type { PublicRequestDeps } from './publicRequests';

/** The advisory lock namespace of the per-network limit (pg_advisory_xact_lock(7410, hashtext(network))). */
const BUSINESS_LIMIT_LOCK = 7410;
/** The advisory lock namespace of the per-email limit (pg_advisory_xact_lock(7411, hashtext(email))). */
const BUSINESS_EMAIL_LOCK = 7411;

/** The actor of a submission (the address is not confirmed). */
const PUBLIC_FORM_ACTOR: Actor = { kind: 'system', label: 'public form' };

/** A validated business submission of the form. */
export interface BusinessSubmission {
  name: string;
  email: string;
  company: string;
  edition: BusinessEdition;
  nodes: number;
  details: string;
  /** The honeypot field was filled. */
  honeypot: boolean;
}

/** A business_requests row. */
interface BusinessRequestRow {
  id: string;
  name: string;
  email: string;
  company: string;
  edition: BusinessEdition;
  nodes: number;
  details: string;
  status: BusinessRequestStatus;
  decided_by: string | null;
  decided_by_label: string;
  decided_at: Date | null;
  decision_note: string;
  resulting_license_id: string | null;
  created_at: Date;
}

function dto(r: BusinessRequestRow): BusinessRequest {
  return {
    id: r.id,
    name: r.name,
    email: r.email,
    company: r.company,
    edition: r.edition,
    nodes: r.nodes,
    details: r.details,
    status: r.status,
    decidedBy: r.decided_at ? actorRef(r.decided_by, r.decided_by_label) : null,
    decidedAt: r.decided_at ? r.decided_at.toISOString() : null,
    decisionNote: r.decision_note,
    resultingLicenseId: r.resulting_license_id,
    createdAt: r.created_at.toISOString(),
  };
}

/** The address as Postgres inet takes it (an IPv4-mapped IPv6 address as IPv4), or null. */
function inetOrNull(ip: string | null): string | null {
  if (!ip) return null;
  const mapped = /^::ffff:(\d+\.\d+\.\d+\.\d+)$/i.exec(ip);
  const addr = mapped ? mapped[1]! : ip;
  return isIP(addr) ? addr : null;
}

function domainOf(email: string): string {
  return email.slice(email.lastIndexOf('@') + 1);
}

/** POST /api/v1/public/business-requests. Resolves for every 202. */
export async function submitBusinessRequest(deps: PublicRequestDeps, input: BusinessSubmission): Promise<void> {
  const ip = inetOrNull(deps.ip);
  if (input.honeypot) {
    deps.log.warn('public.business_honeypot', { ip, emailDomain: domainOf(input.email) });
    return;
  }
  await inTransaction(deps, publicActor(input.email, { ip, userAgent: deps.userAgent }), async (ctx) => {
    if (ip) {
      // One network at a time, so parallel submissions cannot pass the limit together.
      const net = await ctx.tx<{ net: string }[]>`select network(set_masklen(${ip}::inet, case when family(${ip}::inet) = 6 then 64 else 32 end))::text as net`;
      const network = net[0]!.net;
      await ctx.tx`select pg_advisory_xact_lock(${BUSINESS_LIMIT_LOCK}, hashtext(${network}))`;
      const since = new Date(ctx.now.getTime() - 3_600_000);
      const rows = await ctx.tx<{ n: string; oldest: Date | null }[]>`
        select count(*) as n, min(created_at) as oldest from business_requests
         where ip <<= ${network}::inet and created_at > ${since}`;
      const count = toInt(rows[0]?.n);
      if (count >= ctx.config.publicForm.limitPerIpPerHour) {
        const oldest = rows[0]?.oldest ?? ctx.now;
        const retryAfter = Math.max(1, Math.ceil((oldest.getTime() + 3_600_000 - ctx.now.getTime()) / 1000));
        ctx.log.warn('public.business_rate_limited', { network, count });
        throw new HttpError(429, 'rate_limited', 'Too many requests from your network. Try again later.', { headers: { 'Retry-After': String(retryAfter) } });
      }
    }
    await ctx.tx`select pg_advisory_xact_lock(${BUSINESS_EMAIL_LOCK}, hashtext(${input.email}))`;
    const dayAgo = new Date(ctx.now.getTime() - 86_400_000);
    const perEmail = await ctx.tx<{ n: string }[]>`select count(*) as n from business_requests where email = ${input.email} and created_at > ${dayAgo}`;
    if (toInt(perEmail[0]?.n) >= ctx.config.publicForm.limitPerEmailPerDay) {
      ctx.log.warn('public.business_email_limited', { emailDomain: domainOf(input.email) });
      return;
    }
    const id = newId('businessRequest');
    await ctx.tx`
      insert into business_requests (id, name, email, company, edition, nodes, details, ip, user_agent, created_at)
      values (${id}, ${input.name}, ${input.email}, ${input.company}, ${input.edition}, ${input.nodes}, ${input.details}, ${ip}, ${deps.userAgent}, ${ctx.now})`;
    await audit(ctx.tx, {
      actor: PUBLIC_FORM_ACTOR,
      action: 'business_request.received',
      targetType: 'business_request',
      targetId: id,
      details: { edition: input.edition, nodes: input.nodes },
      ip: null,
      userAgent: null,
      requestId: ctx.requestId ?? null,
    });
    await notify(ctx, { id, name: input.name, email: input.email, company: input.company, edition: input.edition, nodes: input.nodes, details: input.details });
  });
}

/** Queues the requester's mail and the approvers' mails (after the commit); without mail, a warning. */
async function notify(ctx: ServiceCtx, request: BusinessRequestFacts): Promise<void> {
  if (!ctx.mailer.configured) {
    ctx.log.warn('business request stored, nobody mailed', { requestId: request.id, why: 'mail is not configured' });
    return;
  }
  const received = businessReceivedMail({ to: request.email, request, publicUrl: ctx.config.publicUrl });
  ctx.afterCommit(() => ctx.mailer.send(received, ctx.log));
  const { recipients } = await approvalRecipients(ctx.tx, ctx.config, ctx.now);
  if (recipients.length === 0) {
    ctx.log.warn('business request stored, no admin mailed', { requestId: request.id, why: 'ADMIN_NOTIFY_EMAIL is unset and no enabled license admin or superadmin has a verified email' });
    return;
  }
  for (const to of recipients) {
    const msg = businessRequestNeededMail({ to, request, publicUrl: ctx.config.publicUrl });
    ctx.afterCommit(() => ctx.mailer.send(msg, ctx.log));
  }
}

/** GET /business-requests: open requests first, then the newest (at most 500). */
export async function listBusinessRequests(q: Queryable, status: BusinessRequestStatus | undefined): Promise<Items<BusinessRequest>> {
  const where = status ? q`where status = ${status}` : q``;
  const rows = await q<BusinessRequestRow[]>`select * from business_requests ${where} order by (status = 'open') desc, created_at desc, id limit 500`;
  return { items: rows.map(dto) };
}

/** One business request, or 404. */
export async function getBusinessRequest(q: Queryable, id: string): Promise<BusinessRequest> {
  const row = maybeOne(await q<BusinessRequestRow[]>`select * from business_requests where id = ${id}`);
  if (!row) notFound('There is no such business request.');
  return dto(row);
}

/** The request locked for a decision, or 404 / 409 when it is not open. */
export async function lockOpenBusinessRequest(ctx: Pick<ServiceCtx, 'tx'>, id: string): Promise<BusinessRequest> {
  const row = maybeOne(await ctx.tx<BusinessRequestRow[]>`select * from business_requests where id = ${id} for update`);
  if (!row) notFound('There is no such business request.');
  if (row.status !== 'open') fail('conflict', `This business request is already ${row.status}.`);
  return dto(row);
}

/** After issuing from a request (locked with lockOpenBusinessRequest): issued, with the license. */
export async function markBusinessRequestIssued(ctx: ServiceCtx, id: string, licenseId: string): Promise<void> {
  const by = actorColumns(ctx.actor);
  await ctx.tx`
    update business_requests set status = 'issued', decided_by = ${by.id}, decided_by_label = ${by.label}, decided_at = ${ctx.now}, resulting_license_id = ${licenseId}
     where id = ${id}`;
  await auditCtx(ctx, { action: 'business_request.issued', targetType: 'business_request', targetId: id, details: { licenseId } });
}

/** POST /business-requests/:id/close: closed with the admin's note (nothing is mailed). */
export async function closeBusinessRequest(ctx: ServiceCtx, id: string, note: string): Promise<void> {
  const req = await lockOpenBusinessRequest(ctx, id);
  const by = actorColumns(ctx.actor);
  await ctx.tx`
    update business_requests set status = 'closed', decided_by = ${by.id}, decided_by_label = ${by.label}, decided_at = ${ctx.now}, decision_note = ${note}
     where id = ${id}`;
  await auditCtx(ctx, { action: 'business_request.closed', targetType: 'business_request', targetId: id, details: { email: req.email, company: req.company } });
}
