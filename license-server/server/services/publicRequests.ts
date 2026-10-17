// The kudurru.io form (design spec §6 "Public form", a FROZEN contract the
// website client is coded to) and its confirmation page.
//
// Submit: honeypot filled → 202 and nothing happens (a log line); no mail
// or no signer → 503 unavailable (a request that cannot end in a key is not
// taken, even though approval signs later); per IPv4 address or IPv6 /64 at most
// PUBLIC_LIMIT_PER_IP_PER_HOUR rows in the last hour → 429 rate_limited with
// Retry-After seconds; per email at most PUBLIC_LIMIT_PER_EMAIL_PER_DAY rows
// in the last day → a silent 202 (a log line); otherwise a row with the
// SHA-256 of a 32-byte token (24 hours) and the confirmation link mailed
// before the 202. The row is committed first (so parallel submissions count
// it) and the mail goes out after the commit, holding no connection or lock
// while the SMTP server answers; when it fails the row is deleted and the
// answer is 503. The audit event of an unconfirmed submission names neither
// the address nor the client.
//
// Verify: the token is consumed atomically (verified_at set where it was
// null and the link has not expired; none → 400 invalid_token), the
// personal flow runs with source web and the attestation given on the form,
// and the outcome is recorded on the row: pending_review by default (the
// approvers are mailed; personal.ts), issued, renewed or resent otherwise.
// The key itself only ever goes by mail.

import { isIP } from 'node:net';
import type { Attestation, PersonalOutcome } from '../../shared/api';
import { ATTESTATION_LICENSE, ATTESTATION_STATEMENT, CONFIRM_TTL_HOURS } from '../../shared/catalog';
import { hashSecret, isSecretShaped, newId, newSecret } from '../db/ids';
import { maybeOne, toInt } from '../db/sql';
import { fail, HttpError } from '../http/errors';
import { mailOrUnavailable } from '../mail/smtp';
import { confirmationMail } from '../mail/templates';
import { audit, auditCtx } from './audit';
import { requestPersonalKey } from './personal';
import { inTransaction, publicActor, type Actor, type ServiceDeps } from './types';

/** The advisory lock namespace of the per-network limit (pg_advisory_xact_lock(7406, hashtext(network))). */
const PUBLIC_LIMIT_LOCK = 7406;
/** The advisory lock namespace of the per-email limit (pg_advisory_xact_lock(7409, hashtext(email))). */
const PUBLIC_EMAIL_LOCK = 7409;

/** A validated submission of the form. */
export interface PublicSubmission {
  name: string;
  email: string;
  use: string;
  /** The honeypot field was filled. */
  honeypot: boolean;
}

/** What the request adds: the client address and user agent. */
export interface PublicRequestDeps extends ServiceDeps {
  now: Date;
  ip: string | null;
  userAgent: string | null;
}

/** The actor of what happens before an address is confirmed. */
const PUBLIC_FORM_ACTOR: Actor = { kind: 'system', label: 'public form' };

const UNAVAILABLE = 'Free personal keys cannot be requested right now. Try again later, or write to hello@kudurru.io.';

/**
 * The address as Postgres inet takes it (an IPv4-mapped IPv6 address as
 * IPv4, so it is limited per address and not per ::/64), or null for
 * anything that is not an IP address.
 */
function inetOrNull(ip: string | null): string | null {
  if (!ip) return null;
  const mapped = /^::ffff:(\d+\.\d+\.\d+\.\d+)$/i.exec(ip);
  const addr = mapped ? mapped[1]! : ip;
  return isIP(addr) ? addr : null;
}

function domainOf(email: string): string {
  return email.slice(email.lastIndexOf('@') + 1);
}

/** POST /api/v1/public/key-requests (the frozen contract). Resolves for every 202. */
export async function submitPublicRequest(deps: PublicRequestDeps, input: PublicSubmission): Promise<void> {
  const ip = inetOrNull(deps.ip);
  if (input.honeypot) {
    deps.log.warn('public.honeypot', { ip, emailDomain: domainOf(input.email) });
    return;
  }
  if (!deps.mailer.configured || !deps.signing.signer) fail('unavailable', UNAVAILABLE);

  const created = await inTransaction(deps, publicActor(input.email, { ip, userAgent: deps.userAgent }), async (ctx): Promise<{ id: string; token: string } | null> => {
    if (ip) {
      // One network at a time, so parallel submissions cannot pass the limit together.
      const net = await ctx.tx<{ net: string }[]>`select network(set_masklen(${ip}::inet, case when family(${ip}::inet) = 6 then 64 else 32 end))::text as net`;
      const network = net[0]!.net;
      await ctx.tx`select pg_advisory_xact_lock(${PUBLIC_LIMIT_LOCK}, hashtext(${network}))`;
      const since = new Date(ctx.now.getTime() - 3_600_000);
      const rows = await ctx.tx<{ n: string; oldest: Date | null }[]>`
        select count(*) as n, min(created_at) as oldest from public_key_requests
         where ip <<= ${network}::inet and created_at > ${since}`;
      const count = toInt(rows[0]?.n);
      if (count >= ctx.config.publicForm.limitPerIpPerHour) {
        const oldest = rows[0]?.oldest ?? ctx.now;
        const retryAfter = Math.max(1, Math.ceil((oldest.getTime() + 3_600_000 - ctx.now.getTime()) / 1000));
        ctx.log.warn('public.rate_limited', { network, count });
        throw new HttpError(429, 'rate_limited', 'Too many requests from your network. Try again later.', { headers: { 'Retry-After': String(retryAfter) } });
      }
    }
    // One address at a time too: submissions from several networks for the
    // same address cannot pass its limit together.
    await ctx.tx`select pg_advisory_xact_lock(${PUBLIC_EMAIL_LOCK}, hashtext(${input.email}))`;
    const dayAgo = new Date(ctx.now.getTime() - 86_400_000);
    const perEmail = await ctx.tx<{ n: string }[]>`select count(*) as n from public_key_requests where email = ${input.email} and created_at > ${dayAgo}`;
    if (toInt(perEmail[0]?.n) >= ctx.config.publicForm.limitPerEmailPerDay) {
      ctx.log.warn('public.email_limited', { emailDomain: domainOf(input.email) });
      return null;
    }
    const id = newId('publicRequest');
    const token = newSecret();
    const expiresAt = new Date(ctx.now.getTime() + CONFIRM_TTL_HOURS * 3_600_000);
    await ctx.tx`
      insert into public_key_requests (id, name, email, use_description, noncommercial_attested_at, ip, user_agent, token_hash, created_at, expires_at)
      values (${id}, ${input.name}, ${input.email}, ${input.use}, ${ctx.now}, ${ip}, ${deps.userAgent}, ${hashSecret(token)}, ${ctx.now}, ${expiresAt})`;
    // The address is not confirmed yet: the append-only log does not keep
    // it (or the client address); the row has both until the cleanup job.
    await audit(ctx.tx, { actor: PUBLIC_FORM_ACTOR, action: 'public_request.received', targetType: 'public_request', targetId: id, ip: null, userAgent: null, requestId: ctx.requestId ?? null });
    return { id, token };
  });
  if (!created) return;
  const url = `${deps.config.publicUrl}/verify?token=${created.token}`;
  try {
    await mailOrUnavailable(
      deps.mailer,
      confirmationMail({ to: input.email, url, publicUrl: deps.config.publicUrl, approvalRequired: !deps.config.personal.autoApprove }),
      deps.log,
      'The confirmation link',
    );
  } catch (e) {
    // Nothing was sent: the row goes (the append-only audit event stays; it
    // names neither the address nor the client).
    await deps.db`delete from public_key_requests where id = ${created.id} and verified_at is null`;
    throw e;
  }
}

interface PublicRow {
  id: string;
  name: string;
  email: string;
  use_description: string;
  noncommercial_attested_at: Date;
  ip: string | null;
  user_agent: string | null;
}

const INVALID_LINK = 'This confirmation link is not valid: it may have expired or been used already. Send the form on kudurru.io again.';

/** POST /api/v1/public/key-requests/verify: consumes the token and runs the personal flow. */
export async function verifyPublicRequest(deps: PublicRequestDeps, token: string): Promise<PersonalOutcome> {
  if (!isSecretShaped(token)) fail('invalid_token', INVALID_LINK);
  const hash = hashSecret(token);
  // The actor is the confirmed address; the system label covers the consume itself.
  return inTransaction(deps, PUBLIC_FORM_ACTOR, async (base) => {
    const row = maybeOne(
      await base.tx<PublicRow[]>`
        update public_key_requests set verified_at = ${base.now}
         where token_hash = ${hash} and verified_at is null and expires_at > ${base.now}
         returning id, name, email, use_description, noncommercial_attested_at, host(ip) as ip, user_agent`,
    );
    if (!row) fail('invalid_token', INVALID_LINK);
    const ctx = { ...base, actor: publicActor(row.email, { ip: inetOrNull(deps.ip), userAgent: deps.userAgent }) };
    const attestation: Attestation = {
      source: 'kudurru.io form',
      license: ATTESTATION_LICENSE,
      statement: ATTESTATION_STATEMENT,
      at: row.noncommercial_attested_at.toISOString(),
      ip: row.ip,
      userAgent: row.user_agent,
      email: row.email,
    };
    const result = await requestPersonalKey(ctx, { name: row.name, email: row.email, use: row.use_description, attestation, source: 'web' });
    await ctx.tx`
      update public_key_requests set outcome = ${result.outcome}, license_request_id = ${result.requestId ?? null}, license_id = ${result.licenseId ?? null}
       where id = ${row.id}`;
    await auditCtx(ctx, {
      action: 'public_request.confirmed',
      targetType: 'public_request',
      targetId: row.id,
      details: { outcome: result.outcome, licenseId: result.licenseId ?? null, requestId: result.requestId ?? null },
    });
    return result.outcome;
  });
}
