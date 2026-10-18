// Members and invites (design spec §6 "Members and invites"): the people
// with access to a customer's licenses. A member row starts as an invite:
// a 32-byte token in a link (${PUBLIC_URL}/invites/<token>, 14 days), of
// which only the SHA-256 is stored. Redeeming it, signed in with any email,
// links the row to that user (user_id, linked_at; token cleared); a linked
// row is never re-linked. MEMBER_AUTO_LINK also links a new row at once
// when exactly one enabled user holds that IdP-verified email. Removing a
// row ends the access at once. Every change is audited on the customer
// (details.email names the member); a token never reaches the audit log.

import { INVITE_TTL_DAYS } from '../../shared/catalog';
import type { InvitePreview, Member, MemberInviteResult, RedeemInviteResult } from '../../shared/api';
import { hashSecret, isSecretShaped, newSecret } from '../db/ids';
import { maybeOne, type Queryable } from '../db/sql';
import { fail, notFound } from '../http/errors';
import { memberInviteMail } from '../mail/templates';
import { auditCtx } from './audit';
import { lockCustomer } from './customers';
import { actorColumns, type ServiceCtx } from './types';

/** A customer_members row. */
export interface MemberRow {
  customer_id: string;
  email: string;
  user_id: string | null;
  invite_token_hash: Buffer | null;
  invite_expires_at: Date | null;
  invited_at: Date;
  invited_by_label: string;
  linked_at: Date | null;
}

interface MemberWithUser extends MemberRow {
  user_name: string | null;
  user_email: string | null;
}

/** How long an invite link works. */
export const INVITE_TTL_MS = INVITE_TTL_DAYS * 86_400_000;

/** The invite link of a token. */
export function inviteUrl(publicUrl: string, token: string): string {
  return `${publicUrl}/invites/${token}`;
}

export function memberDto(row: MemberWithUser): Member {
  return {
    email: row.email,
    status: row.user_id ? 'linked' : 'invited',
    user: row.user_id ? { id: row.user_id, name: row.user_name ?? '', email: row.user_email } : null,
    invitedAt: row.invited_at.toISOString(),
    invitedBy: row.invited_by_label,
    linkedAt: row.linked_at ? row.linked_at.toISOString() : null,
    inviteExpiresAt: !row.user_id && row.invite_expires_at ? row.invite_expires_at.toISOString() : null,
  };
}

/** The members of a customer, linked ones with their user. */
export async function listMembers(q: Queryable, customerId: string): Promise<Member[]> {
  const rows = await q<MemberWithUser[]>`
    select m.*, u.name as user_name, u.email as user_email
      from customer_members m left join users u on u.id = m.user_id
     where m.customer_id = ${customerId}
     order by m.invited_at, m.email`;
  return rows.map(memberDto);
}

async function getMember(q: Queryable, customerId: string, email: string): Promise<Member> {
  const rows = await q<MemberWithUser[]>`
    select m.*, u.name as user_name, u.email as user_email
      from customer_members m left join users u on u.id = m.user_id
     where m.customer_id = ${customerId} and m.email = ${email}`;
  if (!rows[0]) notFound('There is no such member.');
  return memberDto(rows[0]);
}

async function lockMember(ctx: Pick<ServiceCtx, 'tx'>, customerId: string, email: string): Promise<MemberRow | null> {
  return maybeOne(await ctx.tx<MemberRow[]>`select * from customer_members where customer_id = ${customerId} and email = ${email} for update`);
}

/** Whether the user already has a linked row for this customer (customer_members_customer_user_uq). */
async function isLinkedTo(q: Queryable, customerId: string, userId: string): Promise<boolean> {
  const rows = await q<{ one: number }[]>`select 1 as one from customer_members where customer_id = ${customerId} and user_id = ${userId}`;
  return rows.length > 0;
}

/** The one enabled user holding this IdP-verified email (MEMBER_AUTO_LINK), or null when none or several do. */
async function autoLinkUser(q: Queryable, email: string): Promise<string | null> {
  const rows = await q<{ id: string }[]>`select id from users where email = ${email} and email_verified and disabled_at is null limit 2`;
  return rows.length === 1 ? rows[0]!.id : null;
}

/** Gives the row a fresh invite token (the old link stops working) and returns the token. */
async function newInvite(ctx: ServiceCtx, customerId: string, email: string): Promise<{ token: string; expiresAt: Date }> {
  const token = newSecret();
  const expiresAt = new Date(ctx.now.getTime() + INVITE_TTL_MS);
  const by = actorColumns(ctx.actor);
  await ctx.tx`
    update customer_members
       set invite_token_hash = ${hashSecret(token)}, invite_expires_at = ${expiresAt}, invited_at = ${ctx.now}, invited_by_label = ${by.label}
     where customer_id = ${customerId} and email = ${email}`;
  return { token, expiresAt };
}

/** Links an unlinked row to a user (audited member.linked). */
async function linkRow(ctx: ServiceCtx, customerId: string, email: string, userId: string, how: 'invite' | 'auto' | 'request'): Promise<void> {
  await ctx.tx`
    update customer_members set user_id = ${userId}, linked_at = ${ctx.now}, invite_token_hash = null, invite_expires_at = null
     where customer_id = ${customerId} and email = ${email} and user_id is null`;
  const user = maybeOne(await ctx.tx<{ issuer: string; subject: string }[]>`select issuer, subject from users where id = ${userId}`);
  await auditCtx(ctx, { action: 'member.linked', targetType: 'customer', targetId: customerId, details: { email, userId, how, issuer: user?.issuer ?? null, subject: user?.subject ?? null } });
}

/** Mails an invite after the commit when mail is configured (a failure is logged; the admin has the link). */
function mailInvite(ctx: ServiceCtx, to: string, customerName: string, url: string, expiresAt: Date | null): void {
  if (!ctx.mailer.configured) return;
  const msg = memberInviteMail({ to, customerName, url, expiresAt, publicUrl: ctx.config.publicUrl });
  ctx.afterCommit(() => ctx.mailer.send(msg, ctx.log));
}

/**
 * Adds a member with an invite link (shown once in the answer and mailed
 * when SMTP is configured), or linked at once under MEMBER_AUTO_LINK. 409
 * when the address already has a row.
 */
export async function addMember(ctx: ServiceCtx, customerId: string, email: string): Promise<MemberInviteResult> {
  const customer = await lockCustomer(ctx, customerId);
  if (await lockMember(ctx, customerId, email)) fail('conflict', 'This address already has access to the customer. Send the invite again instead.');
  const by = actorColumns(ctx.actor);
  await ctx.tx`
    insert into customer_members (customer_id, email, invited_at, invited_by_label)
    values (${customerId}, ${email}, ${ctx.now}, ${by.label})`;
  await auditCtx(ctx, { action: 'member.added', targetType: 'customer', targetId: customerId, details: { email } });

  const autoUser = ctx.config.roles.memberAutoLink ? await autoLinkUser(ctx.tx, email) : null;
  if (autoUser && !(await isLinkedTo(ctx.tx, customerId, autoUser))) {
    await linkRow(ctx, customerId, email, autoUser, 'auto');
    const url = `${ctx.config.publicUrl}/my/licenses`;
    mailInvite(ctx, email, customer.name, url, null);
    return { member: await getMember(ctx.tx, customerId, email), inviteUrl: url };
  }
  const invite = await newInvite(ctx, customerId, email);
  const url = inviteUrl(ctx.config.publicUrl, invite.token);
  mailInvite(ctx, email, customer.name, url, invite.expiresAt);
  return { member: await getMember(ctx.tx, customerId, email), inviteUrl: url };
}

/** A fresh invite link for an invited (not yet linked) member; 409 for a linked one. */
export async function reinviteMember(ctx: ServiceCtx, customerId: string, email: string): Promise<MemberInviteResult> {
  const customer = await lockCustomer(ctx, customerId);
  const row = await lockMember(ctx, customerId, email);
  if (!row) notFound('There is no such member.');
  if (row.user_id) fail('conflict', 'This member already accepted the invite.');
  const invite = await newInvite(ctx, customerId, email);
  await auditCtx(ctx, { action: 'member.reinvited', targetType: 'customer', targetId: customerId, details: { email } });
  const url = inviteUrl(ctx.config.publicUrl, invite.token);
  mailInvite(ctx, email, customer.name, url, invite.expiresAt);
  return { member: await getMember(ctx.tx, customerId, email), inviteUrl: url };
}

/** Removes a member; their access ends at once. */
export async function removeMember(ctx: ServiceCtx, customerId: string, email: string): Promise<void> {
  await lockCustomer(ctx, customerId);
  const rows = await ctx.tx<MemberRow[]>`delete from customer_members where customer_id = ${customerId} and email = ${email} returning *`;
  const row = rows[0];
  if (!row) notFound('There is no such member.');
  await auditCtx(ctx, { action: 'member.removed', targetType: 'customer', targetId: customerId, details: { email, userId: row.user_id } });
}

const INVALID_INVITE = 'This invite link is not valid. It may have been used already; ask for a new one.';

interface InviteLookup extends MemberRow {
  customer_name: string;
}

async function inviteByToken(q: Queryable, token: unknown, lock: boolean): Promise<InviteLookup> {
  if (!isSecretShaped(token)) fail('invalid_token', INVALID_INVITE);
  const hash = hashSecret(token);
  const rows = lock
    ? await q<InviteLookup[]>`
        select m.*, c.name as customer_name from customer_members m join customers c on c.id = m.customer_id
         where m.invite_token_hash = ${hash} and m.user_id is null for update of m`
    : await q<InviteLookup[]>`
        select m.*, c.name as customer_name from customer_members m join customers c on c.id = m.customer_id
         where m.invite_token_hash = ${hash} and m.user_id is null`;
  const row = rows[0];
  if (!row) fail('invalid_token', INVALID_INVITE);
  return row;
}

/** What the invite page shows before the person accepts; changes nothing. An expired invite still names the customer. */
export async function previewInvite(q: Queryable, token: unknown, now: Date): Promise<InvitePreview> {
  const row = await inviteByToken(q, token, false);
  const expiresAt = row.invite_expires_at ?? now;
  return { customer: { id: row.customer_id, name: row.customer_name }, email: row.email, expiresAt: expiresAt.toISOString(), expired: expiresAt.getTime() <= now.getTime() };
}

/** Links the invite's row to the signed-in user. 400 invalid_token when unknown, used or expired; 409 when they already have access. */
export async function redeemInvite(ctx: ServiceCtx, token: unknown, userId: string): Promise<RedeemInviteResult> {
  const row = await inviteByToken(ctx.tx, token, true);
  if (!row.invite_expires_at || row.invite_expires_at.getTime() <= ctx.now.getTime()) {
    fail('invalid_token', `This invite link has expired. Ask ${row.customer_name} or the licensor for a new one.`);
  }
  if (await isLinkedTo(ctx.tx, row.customer_id, userId)) fail('conflict', `You already have access to the licenses of ${row.customer_name}.`);
  await linkRow(ctx, row.customer_id, row.email, userId, 'invite');
  return { customer: { id: row.customer_id, name: row.customer_name } };
}

/**
 * For the personal flow: makes sure `email` is a member of the customer.
 * A portal request (`linkUserId`, that user's IdP-verified email) links the
 * row only when this call created it, or under MEMBER_AUTO_LINK when the
 * user is the sole enabled holder of the address: an existing unlinked row
 * may belong to someone else, and an IdP's email_verified alone never
 * grants access to it (spec §6 "Members and invites"). Otherwise, and with
 * `invite`, an unlinked row gets a fresh invite link for the key mail,
 * which goes to the address itself. Returns that link, or null.
 */
export async function ensureMember(ctx: ServiceCtx, customerId: string, email: string, opts: { linkUserId?: string | null; invite: boolean }): Promise<string | null> {
  let row = await lockMember(ctx, customerId, email);
  let created = false;
  if (!row) {
    const by = actorColumns(ctx.actor);
    await ctx.tx`
      insert into customer_members (customer_id, email, invited_at, invited_by_label)
      values (${customerId}, ${email}, ${ctx.now}, ${by.label})`;
    await auditCtx(ctx, { action: 'member.added', targetType: 'customer', targetId: customerId, details: { email } });
    row = (await lockMember(ctx, customerId, email))!;
    created = true;
  }
  if (row.user_id) return null;
  if (opts.linkUserId && !(await isLinkedTo(ctx.tx, customerId, opts.linkUserId))) {
    const mayLink = created || (ctx.config.roles.memberAutoLink && (await autoLinkUser(ctx.tx, email)) === opts.linkUserId);
    if (mayLink) {
      await linkRow(ctx, customerId, email, opts.linkUserId, created ? 'request' : 'auto');
      return null;
    }
  } else if (!opts.invite) {
    return null;
  }
  const invite = await newInvite(ctx, customerId, email);
  return inviteUrl(ctx.config.publicUrl, invite.token);
}
