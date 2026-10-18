// Linking member rows to users by email (design spec §6 "Members and
// invites", MEMBER_AUTO_LINK). The default is invite links only: a member
// row gets a user only when a signed-in person redeems its invite
// (services/members.ts). With MEMBER_AUTO_LINK=true the portal also links
// a row whose user_id is null to the one user who holds that address as an
// IdP-verified email, at sign-in (autoLinkOnSignIn) and when an admin adds
// the member (autoLinkMember). The rules, against account pre-hijacking at
// IdPs that pass unverified addresses through:
// - only when exactly one enabled user holds the verified address (two
//   subjects with the same email link nobody);
// - an already-linked row is never re-linked;
// - a user already linked to that customer under another address is left
//   alone (one row per user and customer);
// - every link is audited as member.linked with the issuer and subject.
// Only for IdPs that verify every address themselves (README).

import type { Config } from '../config';
import { toInt, type Queryable } from '../db/sql';
import { auditCtx } from '../services/audit';
import type { ServiceCtx } from '../services/types';

interface Holder {
  id: string;
  issuer: string;
  subject: string;
}

/** The one enabled user holding `email` as a verified address, or null when there is none or more than one. */
export async function soleVerifiedHolder(q: Queryable, email: string): Promise<Holder | null> {
  const rows = await q<(Holder & { n: string })[]>`
    select id, issuer, subject, count(*) over () as n
      from users
     where email = ${email} and email_verified and disabled_at is null
     limit 2`;
  return rows.length === 1 && toInt(rows[0]!.n) === 1 ? { id: rows[0]!.id, issuer: rows[0]!.issuer, subject: rows[0]!.subject } : null;
}

async function link(ctx: ServiceCtx, holder: Holder, email: string, customerId: string | null): Promise<string[]> {
  const rows = customerId
    ? await ctx.tx<{ customer_id: string }[]>`
        update customer_members m
           set user_id = ${holder.id}, linked_at = ${ctx.now}, invite_token_hash = null, invite_expires_at = null
         where m.customer_id = ${customerId} and m.email = ${email} and m.user_id is null
           and not exists (select 1 from customer_members o where o.customer_id = m.customer_id and o.user_id = ${holder.id})
        returning m.customer_id`
    : await ctx.tx<{ customer_id: string }[]>`
        update customer_members m
           set user_id = ${holder.id}, linked_at = ${ctx.now}, invite_token_hash = null, invite_expires_at = null
         where m.email = ${email} and m.user_id is null
           and not exists (select 1 from customer_members o where o.customer_id = m.customer_id and o.user_id = ${holder.id})
        returning m.customer_id`;
  for (const r of rows) {
    await auditCtx(ctx, {
      action: 'member.linked',
      targetType: 'customer',
      targetId: r.customer_id,
      details: { email, userId: holder.id, issuer: holder.issuer, subject: holder.subject, via: 'auto_link' },
    });
  }
  return rows.map((r) => r.customer_id);
}

/**
 * At sign-in: links every unlinked member row of the user's verified email
 * to the user, when MEMBER_AUTO_LINK is on and the user is the only enabled
 * holder of the address. Returns the customer ids linked.
 */
export async function autoLinkOnSignIn(
  ctx: ServiceCtx,
  config: Pick<Config, 'roles'>,
  user: { id: string; email: string | null; emailVerified: boolean; disabledAt: Date | null },
): Promise<string[]> {
  if (!config.roles.memberAutoLink || !user.email || !user.emailVerified || user.disabledAt !== null) return [];
  const holder = await soleVerifiedHolder(ctx.tx, user.email);
  if (!holder || holder.id !== user.id) return [];
  return link(ctx, holder, user.email, null);
}

/**
 * When an admin adds (or re-invites) a member: links that one row to the
 * sole enabled holder of the verified address, when MEMBER_AUTO_LINK is on.
 * Returns the linked user's id, or null (the row stays an invite). Call it
 * inside the transaction that wrote the row.
 */
export async function autoLinkMember(ctx: ServiceCtx, customerId: string, email: string): Promise<string | null> {
  if (!ctx.config.roles.memberAutoLink) return null;
  const holder = await soleVerifiedHolder(ctx.tx, email);
  if (!holder) return null;
  const linked = await link(ctx, holder, email, customerId);
  return linked.length > 0 ? holder.id : null;
}
