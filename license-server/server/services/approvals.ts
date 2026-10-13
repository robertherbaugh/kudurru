// Who approves personal keys, and the one-click approval links of their
// mail (README "The kudurru.io form"; the owner's request of 2026-10-13:
// "require approval for the license … an email with the details and a
// one-click url for admins to approve").
//
// Approvers: ADMIN_NOTIFY_EMAIL (a comma list), or when it is unset every
// enabled user holding license_admin or superadmin with a verified email: a
// granted role always counts, a role from the IdP only within
// IDP_APPROVER_FRESH_DAYS of the user's last sign-in (idp_roles change only
// at a sign-in). When a personal request starts to wait (personal.ts
// openRequest), each approver gets a mail of their own with the details,
// the terms approval signs and, for the reasons that only wait for a yes
// (auto_approve_off, daily_cap), a link ${PUBLIC_URL}/approve?token=<token>:
// 32 random bytes per recipient, only the SHA-256 stored
// (request_approval_links), APPROVAL_LINK_TTL_DAYS days, single use. A
// blocked address or a withdrawn license in the history gets no link: a
// signed-in license admin decides those in the portal.
//
// The link page decides through requests.ts (previewApproval,
// decideApproval): the link row and the request row locked, the link marked
// used, then the same approve or reject as the Requests page. A link only
// decides while its address still approves (still in ADMIN_NOTIFY_EMAIL, or
// still an enabled admin with that verified email when it is unset), so
// removing an approver ends their outstanding links. The actor is the one
// enabled admin whose verified email the link went to, else the system
// label "approval link (<address>)", whose approvals count against
// ISSUE_MAX_PER_ACTOR_PER_DAY by that label (licenses.ts
// checkIssuingQuota). Approving re-checks the review rules (personal.ts
// personalReviewReason): a request that now needs a closer look is decided
// in the portal. Opening the link changes nothing: mail scanners fetch
// links, so only the page's POST decides.

import type { Attestation, ExpiresOn, RequestSource, ReviewReason } from '../../shared/api';
import { APPROVAL_LINK_TTL_DAYS, PERSONAL_MAX_NODES } from '../../shared/catalog';
import { DAY_SECONDS, expiresOnOf, todayUtc } from '../../shared/dates';
import type { Config } from '../config';
import { hashSecret, isSecretShaped, newSecret } from '../db/ids';
import { maybeOne, type Queryable } from '../db/sql';
import { fail } from '../http/errors';
import { personalExpiresOn, personalRenewalExpiresOn } from '../licensing/claims';
import { reviewNeededMail } from '../mail/templates';
import { userLabel, type Actor, type ServiceCtx } from './types';

/** The review reasons whose mail carries a one-click approve link; the others need a signed-in admin. */
export const LINK_REASONS: readonly ReviewReason[] = ['auto_approve_off', 'daily_cap'];

/** The roles that approve personal keys. */
const APPROVER_ROLES = '{license_admin,superadmin}';

/**
 * How long a role from the IdP makes a user an approver after their last
 * sign-in. idp_roles are rewritten only when the user signs in, so someone
 * removed from the admin group at the IdP (and not disabled in the portal)
 * stops getting the approval mail, and their links stop working, this many
 * days after their last sign-in. Disabling them in the portal, or removing
 * a granted role, ends it at once.
 */
export const IDP_APPROVER_FRESH_DAYS = 30;

/** The earliest last sign-in at which an IdP role still makes an approver. */
function idpFreshSince(now: Date): Date {
  return new Date(now.getTime() - IDP_APPROVER_FRESH_DAYS * DAY_SECONDS * 1000);
}

/** Who gets the approval mail: ADMIN_NOTIFY_EMAIL, or the admin users when it is unset (fromAdmins). */
export interface ApprovalRecipients {
  recipients: string[];
  fromAdmins: boolean;
}

/** The approvers of personal requests at `now` (SystemInfo.approval, the admin mail, whether a link still decides). */
export async function approvalRecipients(q: Queryable, config: Pick<Config, 'mail'>, now: Date): Promise<ApprovalRecipients> {
  if (config.mail.adminNotify.length > 0) return { recipients: [...config.mail.adminNotify], fromAdmins: false };
  const rows = await q<{ email: string }[]>`
    select distinct email from users
     where disabled_at is null and email_verified and email is not null
       and (granted_roles && ${APPROVER_ROLES}::text[]
            or (idp_roles && ${APPROVER_ROLES}::text[] and last_login_at > ${idpFreshSince(now)}))
     order by email`;
  return { recipients: rows.map((r) => r.email), fromAdmins: true };
}

/** The license approving renews, and the nodes the renewal keeps. */
export interface RenewalTarget {
  licenseId: string;
  expiresOn: ExpiresOn;
  nodes: number;
}

/** What approving a request of this address renews: the newest active personal license of its person customer (as requests.ts approveRequest picks it), or null. */
export async function renewalTarget(q: Queryable, email: string): Promise<RenewalTarget | null> {
  const row = maybeOne(
    await q<{ id: string; expires_at: Date; nodes: number }[]>`
      select l.id, l.expires_at, l.nodes from licenses l
       where l.plan = 'personal' and l.status = 'active'
         and l.customer_id = (
           select c.id from customers c
            where c.kind = 'person' and exists (select 1 from customer_members m where m.customer_id = c.id and m.email = ${email})
            order by c.created_at, c.id limit 1)
       order by l.created_at desc, l.id desc limit 1`,
  );
  return row ? { licenseId: row.id, expiresOn: expiresOnOf(row.expires_at), nodes: row.nodes } : null;
}

/**
 * The terms approving at `now` signs by default: a renewal of `renews`
 * (its nodes kept, PERSONAL_TERM_DAYS after the later of today and its
 * expiry), or a new personal key (PERSONAL_MAX_NODES, today +
 * PERSONAL_TERM_DAYS). A personal key always grants every feature.
 */
export function approvalTerms(renews: Pick<RenewalTarget, 'expiresOn' | 'nodes'> | null, now: Date): { plan: 'personal'; nodes: number; features: null; expiresOn: ExpiresOn } {
  return renews
    ? { plan: 'personal', nodes: renews.nodes, features: null, expiresOn: personalRenewalExpiresOn(renews.expiresOn, now) }
    : { plan: 'personal', nodes: PERSONAL_MAX_NODES, features: null, expiresOn: personalExpiresOn(now) };
}

/**
 * The last approval day (UTC) that still gives approvalTerms' expiry, or
 * null when only today does: a new key, or a renewal of a key that expires
 * today or earlier, runs from the approval day; a renewal of a key that
 * expires later runs from that expiry whenever it is approved by then.
 */
export function termsHoldUntil(renews: Pick<RenewalTarget, 'expiresOn'> | null, now: Date): ExpiresOn | null {
  return renews && renews.expiresOn > todayUtc(now) ? renews.expiresOn : null;
}

/** A request that just started to wait, as the approvers' mail describes it. */
export interface WaitingRequest {
  id: string;
  name: string;
  email: string;
  use: string;
  source: RequestSource;
  attestation: Attestation;
  reason: ReviewReason;
}

/**
 * Mails every approver about a request that just started to wait (after
 * the commit), with an approve link of their own when the reason allows
 * one. Nobody to tell (mail not configured, no approvers): a warning.
 */
export async function notifyApprovers(ctx: ServiceCtx, req: WaitingRequest): Promise<void> {
  if (!ctx.mailer.configured) {
    ctx.log.warn('request waiting for review, nobody notified', { requestId: req.id, why: 'mail is not configured' });
    return;
  }
  const { recipients } = await approvalRecipients(ctx.tx, ctx.config, ctx.now);
  if (recipients.length === 0) {
    ctx.log.warn('request waiting for review, nobody notified', { requestId: req.id, why: 'ADMIN_NOTIFY_EMAIL is unset and no enabled license admin or superadmin has a verified email' });
    return;
  }
  const renews = await renewalTarget(ctx.tx, req.email);
  const terms = approvalTerms(renews, ctx.now);
  const withLink = LINK_REASONS.includes(req.reason);
  const expiresAt = new Date(ctx.now.getTime() + APPROVAL_LINK_TTL_DAYS * DAY_SECONDS * 1000);
  for (const to of recipients) {
    let approveUrl: string | null = null;
    if (withLink) {
      const token = newSecret();
      await ctx.tx`
        insert into request_approval_links (token_hash, request_id, sent_to, created_at, expires_at)
        values (${hashSecret(token)}, ${req.id}, ${to}, ${ctx.now}, ${expiresAt})`;
      approveUrl = `${ctx.config.publicUrl}/approve?token=${token}`;
    }
    const msg = reviewNeededMail({
      to,
      request: {
        id: req.id,
        name: req.name,
        email: req.email,
        use: req.use,
        reason: req.reason,
        source: req.source,
        attestation: { statement: req.attestation.statement ?? null, at: req.attestation.at, ip: req.attestation.ip ?? null },
      },
      renews: renews ? { licenseId: renews.licenseId, expiresOn: renews.expiresOn } : null,
      terms: { nodes: terms.nodes, expiresOn: terms.expiresOn, holdsUntil: termsHoldUntil(renews, ctx.now) },
      approveUrl,
      publicUrl: ctx.config.publicUrl,
    });
    ctx.afterCommit(() => ctx.mailer.send(msg));
  }
}

/** A request_approval_links row. */
export interface ApprovalLinkRow {
  token_hash: Buffer;
  request_id: string;
  sent_to: string;
  created_at: Date;
  expires_at: Date;
  used_at: Date | null;
}

const UNKNOWN_LINK = 'This approval link is not valid. Decide the request in the portal instead.';

/** The link of a token (used and expired ones too), or 400 invalid_token. */
export async function findApprovalLink(q: Queryable, token: string): Promise<ApprovalLinkRow> {
  if (!isSecretShaped(token)) fail('invalid_token', UNKNOWN_LINK);
  const row = maybeOne(await q<ApprovalLinkRow[]>`select * from request_approval_links where token_hash = ${hashSecret(token)}`);
  if (!row) fail('invalid_token', UNKNOWN_LINK);
  return row;
}

/** The link is used, or past its expiry. */
export function linkSpent(link: Pick<ApprovalLinkRow, 'used_at' | 'expires_at'>, now: Date): boolean {
  return link.used_at !== null || link.expires_at.getTime() <= now.getTime();
}

/** Whether the address a link went to still approves personal keys at `now` (approvalRecipients). */
export async function stillApproves(q: Queryable, config: Pick<Config, 'mail'>, sentTo: string, now: Date): Promise<boolean> {
  return (await approvalRecipients(q, config, now)).recipients.includes(sentTo);
}

/**
 * The link of a token locked for a decision (SELECT … FOR UPDATE), or 400
 * invalid_token: unknown, used already, expired, or its address no longer
 * approves. Call it under the personal-flow lock, which every request
 * decision takes first.
 */
export async function lockApprovalLink(ctx: Pick<ServiceCtx, 'tx' | 'now' | 'config'>, token: string): Promise<ApprovalLinkRow> {
  if (!isSecretShaped(token)) fail('invalid_token', UNKNOWN_LINK);
  const row = maybeOne(await ctx.tx<ApprovalLinkRow[]>`select * from request_approval_links where token_hash = ${hashSecret(token)} for update`);
  if (!row) fail('invalid_token', UNKNOWN_LINK);
  const hint = `${ctx.config.publicUrl}/requests`;
  if (row.used_at !== null) fail('invalid_token', 'This approval link was used already: each link decides once. See the request in the portal.', { hint });
  if (row.expires_at.getTime() <= ctx.now.getTime()) {
    fail('invalid_token', `This approval link expired: links work ${APPROVAL_LINK_TTL_DAYS} days. Decide the request in the portal instead.`, { hint });
  }
  if (!(await stillApproves(ctx.tx, ctx.config, row.sent_to, ctx.now))) {
    fail('invalid_token', 'This approval link no longer works: the address it was sent to no longer approves personal keys. Decide the request in the portal instead.', { hint });
  }
  return row;
}

/** Marks a locked link used. */
export async function markLinkUsed(ctx: Pick<ServiceCtx, 'tx' | 'now'>, link: Pick<ApprovalLinkRow, 'token_hash'>): Promise<void> {
  await ctx.tx`update request_approval_links set used_at = ${ctx.now} where token_hash = ${link.token_hash}`;
}

/**
 * Who decides through a link at `now`: the one enabled user holding
 * license_admin or superadmin (granted, or from the IdP within
 * IDP_APPROVER_FRESH_DAYS of their last sign-in) whose verified email the
 * link went to, else the system label "approval link (<address>)" (a
 * shared mailbox, or no such user), with an issuing quota of its own.
 */
export async function linkActor(q: Queryable, sentTo: string, now: Date): Promise<Actor> {
  const users = await q<{ id: string; name: string; email: string }[]>`
    select id, name, email from users
     where email = ${sentTo} and email_verified and disabled_at is null
       and (granted_roles && ${APPROVER_ROLES}::text[]
            or (idp_roles && ${APPROVER_ROLES}::text[] and last_login_at > ${idpFreshSince(now)}))
     limit 2`;
  const only = users.length === 1 ? users[0]! : null;
  return only ? { kind: 'user', userId: only.id, label: userLabel(only) } : { kind: 'system', label: `approval link (${sentTo})`, issuingQuota: true };
}
