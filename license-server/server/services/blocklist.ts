// The blocklist (design spec §4 blocked_emails, §6 "Withdraw"): addresses
// whose personal key requests always wait for review. Withdrawing a license
// with "block personal" adds the customer's member emails; a superadmin
// removes entries (blocklist.write). Both are audited.

import type { BlockedEmail } from '../../shared/api';
import type { Queryable } from '../db/sql';
import { notFound } from '../http/errors';
import { auditCtx } from './audit';
import { actorRef } from './customers';
import { actorColumns, type ServiceCtx } from './types';

interface BlockedRow {
  email: string;
  reason: string;
  blocked_by: string | null;
  blocked_by_label: string;
  blocked_at: Date;
}

function blockedDto(r: BlockedRow): BlockedEmail {
  return { email: r.email, reason: r.reason, blockedBy: actorRef(r.blocked_by, r.blocked_by_label), blockedAt: r.blocked_at.toISOString() };
}

/** Whether an address is blocked. */
export async function isBlocked(q: Queryable, email: string): Promise<boolean> {
  const rows = await q<{ one: number }[]>`select 1 as one from blocked_emails where email = ${email}`;
  return rows.length > 0;
}

/** Every blocked address, newest first. */
export async function listBlocked(q: Queryable): Promise<BlockedEmail[]> {
  return (await q<BlockedRow[]>`select * from blocked_emails order by blocked_at desc, email`).map(blockedDto);
}

/** Blocks the addresses not blocked yet (audited blocklist.added each); returns the ones added. */
export async function blockEmails(ctx: ServiceCtx, emails: readonly string[], reason: string): Promise<string[]> {
  if (emails.length === 0) return [];
  const by = actorColumns(ctx.actor);
  const added = await ctx.tx<{ email: string }[]>`
    insert into blocked_emails (email, reason, blocked_by, blocked_by_label, blocked_at)
    select e, ${reason}, ${by.id}, ${by.label}, ${ctx.now} from unnest(${ctx.tx.array([...emails], 'TEXT')}::text[]) as e
    on conflict (email) do nothing
    returning email`;
  for (const r of added) await auditCtx(ctx, { action: 'blocklist.added', targetType: 'blocked_email', targetId: r.email, details: { reason } });
  return added.map((r) => r.email);
}

/** Removes an address from the blocklist (blocklist.write); 404 when it is not there. */
export async function unblockEmail(ctx: ServiceCtx, email: string): Promise<void> {
  const rows = await ctx.tx<BlockedRow[]>`delete from blocked_emails where email = ${email} returning *`;
  if (!rows[0]) notFound('That address is not blocked.');
  await auditCtx(ctx, { action: 'blocklist.removed', targetType: 'blocked_email', targetId: email, details: { reason: rows[0].reason } });
}
