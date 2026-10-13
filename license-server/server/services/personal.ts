// The free personal key (design spec §6 "Personal keys"): one decision tree
// for the kudurru.io form (source web, after the mailed confirmation) and
// the portal (source portal, signed in with an IdP-verified email), and the
// portal's own renewal. requestPersonalKey, in order:
//
// 1. the email is blocked → an open request (review reason blocked);
// 2. the person customer (the oldest customer of kind person with a member
//    row for the email; never an organization) has a withdrawn license →
//    an open request (withdrawn_history);
// 3. the person customer holds an active personal license (the newest):
//    within RENEW_WINDOW_DAYS of its expiry, in grace or expired → renewed
//    to PERSONAL_TERM_DAYS after the later of today and the current expiry;
//    otherwise the current key is mailed again (resent). A hand-signed
//    import whose terms break the personal policy is never renewed on its
//    own: it is sent again too (with a log line);
// 4. PERSONAL_AUTO_APPROVE is off, or today's automatic keys reached
//    PERSONAL_AUTO_APPROVE_MAX_PER_DAY → an open request (auto_approve_off,
//    daily_cap), mails to the requester and to ADMIN_NOTIFY_EMAIL;
// 5. otherwise the person customer is created when missing (member row for
//    the email: linked to the requesting user from the portal, an invite
//    link from the form) and a key is issued (personal, 3 nodes, every
//    feature, today + PERSONAL_TERM_DAYS) and mailed.
//
// All of it runs in the caller's transaction under one advisory lock (so
// two requests cannot create two person customers or pass the daily cap
// together) with the customer and license rows locked; mail goes after the
// commit (a failure is logged; the key stays in the portal and an admin can
// email it again). At most one request per address is open: a second
// request while one waits is the same request (pending_review).

import type { Attestation, PersonalRequestResult, RequestSource, ReviewReason } from '../../shared/api';
import { PERSONAL_MAX_NODES, RENEW_WINDOW_DAYS } from '../../shared/catalog';
import { DAY_SECONDS, expiresOnOf, todayUtc } from '../../shared/dates';
import { newId } from '../db/ids';
import { maybeOne, toInt } from '../db/sql';
import { fail } from '../http/errors';
import { assertPolicy, isRenewable, personalExpiresOn, personalRenewal, type KeyTerms } from '../licensing/claims';
import { reviewNeededMail, reviewWaitingMail } from '../mail/templates';
import { auditCtx } from './audit';
import { isBlocked } from './blocklist';
import { createCustomer, type CustomerRow } from './customers';
import {
  currentKey,
  insertLicense,
  licenseKeyMail,
  lockLicense,
  lockPersonalFlow,
  mailKeyAfterCommit,
  renewLicenseRow,
  termsOf,
  type LicenseRow,
} from './licenses';
import { ensureMember } from './members';
import { requireSigner, type ServiceCtx } from './types';

/** A personal key request, validated. */
export interface PersonalRequest {
  /** The licensee printed on the key (claims.ts licensee rules). */
  name: string;
  /** Normalised (trimmed, lower-case). */
  email: string;
  use: string;
  attestation: Attestation;
  source: RequestSource;
  /** The portal user who asked (source portal). */
  requestedBy?: string | null;
}

/** The person customer of an address: the oldest customer of kind person with a member row for it (locked), or null. */
export async function lockPersonCustomer(ctx: Pick<ServiceCtx, 'tx'>, email: string): Promise<CustomerRow | null> {
  return maybeOne(
    await ctx.tx<CustomerRow[]>`
      select c.* from customers c
       where c.kind = 'person' and exists (select 1 from customer_members m where m.customer_id = c.id and m.email = ${email})
       order by c.created_at, c.id
       limit 1
       for update`,
  );
}

/** Whether any license of the customer is withdrawn. */
async function hasWithdrawnLicense(ctx: Pick<ServiceCtx, 'tx'>, customerId: string): Promise<boolean> {
  const rows = await ctx.tx<{ one: number }[]>`select 1 as one from licenses where customer_id = ${customerId} and status = 'withdrawn' limit 1`;
  return rows.length > 0;
}

/**
 * Whether the address has a withdrawn personal license outside any member
 * row: as the contact address of a person customer or the address on a
 * license's attestation. Removing a member after a withdraw must not
 * reopen automatic issue (spec §0.8).
 */
async function hasWithdrawnByEmail(ctx: Pick<ServiceCtx, 'tx'>, email: string): Promise<boolean> {
  const rows = await ctx.tx<{ one: number }[]>`
    select 1 as one from licenses l join customers c on c.id = l.customer_id
     where l.status = 'withdrawn' and (c.kind = 'person' or l.plan = 'personal')
       and (c.contact_email = ${email} or l.attestation->>'email' = ${email})
     limit 1`;
  return rows.length > 0;
}

/**
 * Steps 1 and 2 of the decision tree, shared with the portal renewal: why a
 * personal key for these addresses (and this person customer) needs a
 * person, or null.
 */
export async function personalReviewReason(ctx: Pick<ServiceCtx, 'tx'>, emails: readonly string[], customerId: string | null): Promise<ReviewReason | null> {
  for (const email of emails) if (await isBlocked(ctx.tx, email)) return 'blocked';
  if (customerId && (await hasWithdrawnLicense(ctx, customerId))) return 'withdrawn_history';
  for (const email of emails) if (await hasWithdrawnByEmail(ctx, email)) return 'withdrawn_history';
  return null;
}

/** The customer's newest active personal license, locked, or null. */
export async function lockNewestActivePersonal(ctx: Pick<ServiceCtx, 'tx'>, customerId: string): Promise<LicenseRow | null> {
  const row = maybeOne(
    await ctx.tx<{ id: string }[]>`
      select id from licenses where customer_id = ${customerId} and plan = 'personal' and status = 'active'
       order by created_at desc, id desc limit 1`,
  );
  return row ? lockLicense(ctx, row.id) : null;
}

/** Personal keys issued today (UTC) without review: licenses from the form or the portal that no request led to. */
async function autoApprovedToday(ctx: ServiceCtx): Promise<number> {
  const midnight = new Date(`${todayUtc(ctx.now)}T00:00:00Z`);
  const rows = await ctx.tx<{ n: string }[]>`
    select count(*) as n from licenses l
     where l.plan = 'personal' and l.source in ('web', 'portal') and l.created_at >= ${midnight}
       and not exists (select 1 from license_requests r where r.resulting_license_id = l.id)`;
  return toInt(rows[0]?.n);
}

/** Opens a request that waits for review (or finds the one already open for the address). */
async function openRequest(ctx: ServiceCtx, req: PersonalRequest, reason: ReviewReason): Promise<PersonalRequestResult> {
  const id = newId('request');
  const inserted = await ctx.tx<{ id: string }[]>`
    insert into license_requests (id, kind, source, requested_by, requester_email, name, use_description, attestation, review_reason, status, created_at)
    values (${id}, 'personal', ${req.source}, ${req.requestedBy ?? null}, ${req.email}, ${req.name}, ${req.use}, ${req.attestation}, ${reason}, 'open', ${ctx.now})
    on conflict (requester_email) where status = 'open' do nothing
    returning id`;
  if (inserted.length === 0) {
    const open = maybeOne(await ctx.tx<{ id: string }[]>`select id from license_requests where requester_email = ${req.email} and status = 'open'`);
    if (!open) throw new Error('an open request vanished');
    ctx.log.info('personal request already waits for review', { requestId: open.id });
    return { outcome: 'pending_review', requestId: open.id };
  }
  await auditCtx(ctx, { action: 'request.created', targetType: 'request', targetId: id, details: { email: req.email, source: req.source, reviewReason: reason } });
  if (ctx.mailer.configured) {
    const publicUrl = ctx.config.publicUrl;
    const waiting = reviewWaitingMail({ to: req.email, name: req.name, publicUrl });
    ctx.afterCommit(() => ctx.mailer.send(waiting));
    const admin = ctx.config.mail.adminNotify;
    if (admin) {
      const notice = reviewNeededMail({ to: admin, request: { id, name: req.name, email: req.email, use: req.use, reason, source: req.source }, publicUrl });
      ctx.afterCommit(() => ctx.mailer.send(notice));
    }
  }
  return { outcome: 'pending_review', requestId: id };
}

/** The decision tree of spec §6 "Personal keys" (see the top of this file). */
export async function requestPersonalKey(ctx: ServiceCtx, req: PersonalRequest): Promise<PersonalRequestResult> {
  await lockPersonalFlow(ctx);

  // 1. Blocked addresses always wait for a person. 2. So does a withdrawn
  // license in the person's history.
  const person = await lockPersonCustomer(ctx, req.email);
  const review = await personalReviewReason(ctx, [req.email], person?.id ?? null);
  if (review) return openRequest(ctx, req, review);

  const linkUserId = req.source === 'portal' ? (req.requestedBy ?? null) : null;

  // 3. An active personal license: renew it in the window, else send it again.
  if (person) {
    const license = await lockNewestActivePersonal(ctx, person.id);
    if (license) {
      const exp = Math.floor(license.expires_at.getTime() / 1000);
      const renewal = personalRenewal(termsOf(license), exp, ctx.now);
      if (renewal === null && isRenewable(exp, ctx.now)) {
        ctx.log.warn('personal renewal refused by the policy: the key is sent again', { licenseId: license.id });
      }
      if (renewal !== null) {
        requireSigner(ctx);
        const inviteUrl = await ensureMember(ctx, person.id, req.email, { linkUserId, invite: req.source === 'web' });
        const renewed = await renewLicenseRow(ctx, license, renewal, { via: req.source });
        mailKeyAfterCommit(ctx, licenseKeyMail(ctx, renewed.row, renewed.key.token, 'renewed', req.email, inviteUrl));
        return { outcome: 'renewed', licenseId: license.id };
      }
      const inviteUrl = await ensureMember(ctx, person.id, req.email, { linkUserId, invite: req.source === 'web' });
      const key = await currentKey(ctx.tx, license.id);
      if (ctx.mailer.configured) {
        await auditCtx(ctx, { action: 'license.key_emailed', targetType: 'license', targetId: license.id, details: { to: req.email, keyId: key.id, via: req.source } });
      }
      mailKeyAfterCommit(ctx, licenseKeyMail(ctx, license, key.token, 'resent', req.email, inviteUrl));
      return { outcome: 'resent', licenseId: license.id };
    }
  }

  // 4. Automatic approval off, or today's cap reached: a person decides.
  if (!ctx.config.personal.autoApprove) return openRequest(ctx, req, 'auto_approve_off');
  if ((await autoApprovedToday(ctx)) >= ctx.config.personal.autoApproveMaxPerDay) return openRequest(ctx, req, 'daily_cap');

  // 5. Issue.
  requireSigner(ctx);
  const terms: KeyTerms = { licensee: req.name, plan: 'personal', nodes: PERSONAL_MAX_NODES, features: null, expiresOn: personalExpiresOn(ctx.now) };
  assertPolicy(terms, ctx.now, (f) => (f === 'licensee' ? 'name' : f));
  const customer = person ?? (await createCustomer(ctx, { name: req.name, kind: 'person', contactEmail: req.email }));
  const inviteUrl = await ensureMember(ctx, customer.id, req.email, { linkUserId, invite: req.source === 'web' });
  const { row, key } = await insertLicense(ctx, {
    customerId: customer.id,
    terms,
    attestation: req.attestation,
    source: req.source,
    auditDetails: { autoApproved: true },
  });
  mailKeyAfterCommit(ctx, licenseKeyMail(ctx, row, key.token, 'issued', req.email, inviteUrl));
  return { outcome: 'issued', licenseId: row.id };
}

/**
 * POST /me/licenses/:id/renew: the person renews their own personal key
 * (member of its customer, personal plan, active, within the renew window,
 * in grace or expired), as step 3 does. 404 for a license they cannot see.
 * The key is mailed to `mailTo` when given.
 */
export async function renewMyLicense(ctx: ServiceCtx, licenseId: string, userId: string, mailTo: string | null): Promise<void> {
  const visible = maybeOne(
    await ctx.tx<{ id: string }[]>`
      select l.id from licenses l
       where l.id = ${licenseId}
         and exists (select 1 from customer_members m where m.customer_id = l.customer_id and m.user_id = ${userId})`,
  );
  if (!visible) fail('not_found', 'There is no such license.');
  await lockPersonalFlow(ctx);
  const row = await lockLicense(ctx, licenseId);
  if (row.status === 'withdrawn') fail('license_withdrawn', 'This license is withdrawn and cannot be renewed.');
  if (row.plan !== 'personal') fail('conflict', 'Only free personal keys are renewed here. For a business license, write to us.');
  // The same review rules as a request (steps 1 and 2): a blocked address
  // or a withdrawn license in the history needs a person.
  const memberEmails = (await ctx.tx<{ email: string }[]>`select email from customer_members where customer_id = ${row.customer_id}`).map((m) => m.email);
  if (await personalReviewReason(ctx, mailTo ? [...memberEmails, mailTo] : memberEmails, row.customer_id)) {
    fail('conflict', 'This key cannot be renewed here. Write to us to renew it.');
  }
  const exp = Math.floor(row.expires_at.getTime() / 1000);
  if (!isRenewable(exp, ctx.now)) {
    // The first whole UTC day inside the window (it opens at exp - window).
    const from = expiresOnOf(exp - RENEW_WINDOW_DAYS * DAY_SECONDS + 1);
    fail('conflict', `This key can be renewed from ${from} (UTC), about ${RENEW_WINDOW_DAYS} days before it expires.`);
  }
  const renewal = personalRenewal(termsOf(row), exp, ctx.now);
  if (renewal === null) fail('conflict', 'This key does not match the rules of a free personal key, so it cannot be renewed here. Write to us to renew it.');
  requireSigner(ctx);
  const renewed = await renewLicenseRow(ctx, row, renewal, { via: 'portal' });
  if (mailTo) mailKeyAfterCommit(ctx, licenseKeyMail(ctx, renewed.row, renewed.key.token, 'renewed', mailTo));
}
