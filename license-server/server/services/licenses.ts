// Licenses and their keys (design spec §4, §6 "Admin license actions"):
// the rows, the one place keys are signed and stored (signKey), the admin
// actions (issue, renew, change, withdraw, import, email key, download) and
// the DTOs of the licenses pages. The personal flow (personal.ts) and
// request approval (requests.ts) build on insertLicense and renewLicenseRow.
//
// Invariants (spec §4): one current key per license (the unsuperseded one,
// license_keys_current_uq); the current key's claims equal claimsFor(row)
// except iat and kid; licenses.expires_at is the current key's exp; the
// token is authoritative and never re-signed from the claims snapshot; a
// token never goes into audit details or logs. Every action runs in the
// caller's transaction with the license row locked (SELECT … FOR UPDATE),
// answers 409 license_withdrawn on a withdrawn license, signs only after
// the policy passed (licensing/claims.ts), writes one log line per
// signature and audits in the same transaction; mail waits for the commit.

import type {
  ActorRef,
  Attestation,
  ChangeBody,
  EmailKeyResult,
  ImportBody,
  KeyReason,
  KeyRecord,
  LicenseDetail,
  LicenseListQuery,
  LicenseSource,
  LicenseSummary,
  MyLicense,
  MyLicenseDetail,
  NewCustomerBody,
  Page,
  RenewBody,
  WithdrawBody,
} from '../../shared/api';
import { EXPIRY_WARNING_DAYS, GRACE_SECONDS, licenseState, MAX_NODES, type LicenseState, type Plan } from '../../shared/catalog';
import { DAY_SECONDS, expiresOnOf, graceEndsOn, unixSeconds, type ExpiresOn } from '../../shared/dates';
import { newId } from '../db/ids';
import { maybeOne, toInt, type Queryable } from '../db/sql';
import { fail, HttpError, notFound } from '../http/errors';
import { assertPolicy, claimsFor, personalRenewal, type KeyTerms } from '../licensing/claims';
import { MAX_JTI_LENGTH, newLicenseId } from '../licensing/ids';
import { verify, VerifyError } from '../licensing/jws';
import type { SigningState } from '../licensing/types';
import { mailOrUnavailable } from '../mail/smtp';
import { keyMail, type KeyMailKind } from '../mail/templates';
import type { MailMessage } from '../mail/types';
import { auditCtx } from './audit';
import { blockEmails } from './blocklist';
import { actorRef, containsPattern, createCustomer, customerRef, lockCustomer, type CustomerRow, type Fragment } from './customers';
import { actorColumns, inTransaction, requireSigner, type Actor, type ServiceCtx, type ServiceDeps } from './types';

// --- rows -----------------------------------------------------------------------

/** A licenses row. */
export interface LicenseRow {
  id: string;
  customer_id: string;
  licensee: string;
  plan: Plan;
  nodes: number;
  features: string[] | null;
  expires_at: Date;
  agreement_ref: string;
  notes: string;
  status: 'active' | 'withdrawn';
  withdrawn_at: Date | null;
  withdrawn_by: string | null;
  withdrawn_by_label: string;
  withdraw_reason: string;
  withdraw_message: string;
  attestation: Attestation | null;
  source: LicenseSource;
  created_at: Date;
  created_by: string | null;
  created_by_label: string;
  updated_at: Date;
}

/** A license_keys row. */
export interface KeyRow {
  id: string;
  license_id: string;
  kid: string;
  token: string;
  claims: Record<string, unknown>;
  issued_at: Date;
  expires_at: Date;
  issued_by: string | null;
  issued_by_label: string;
  reason: KeyReason;
  superseded_at: Date | null;
  superseded_by: string | null;
}

/** What a key says, from the license row. */
export function termsOf(row: Pick<LicenseRow, 'licensee' | 'plan' | 'nodes' | 'features' | 'expires_at'>): KeyTerms {
  return { licensee: row.licensee, plan: row.plan, nodes: row.nodes, features: row.features, expiresOn: expiresOnOf(row.expires_at) };
}

/** The state a node would report for the current key, or withdrawn. */
export function stateOf(row: Pick<LicenseRow, 'expires_at' | 'status'>, now: Date): LicenseState {
  return licenseState(row.expires_at, row.status === 'withdrawn', now);
}

/** The last day any key of a license works on nodes: the latest key expiry + grace. */
export function keepsWorkingUntilOf(latestKeyExpiry: Date): ExpiresOn {
  return graceEndsOn(expiresOnOf(latestKeyExpiry));
}

/** The license, or 404. */
export async function getLicenseRow(q: Queryable, id: string): Promise<LicenseRow> {
  const row = maybeOne(await q<LicenseRow[]>`select * from licenses where id = ${id}`);
  if (!row) notFound('There is no such license.');
  return row;
}

/** Locks the license row (SELECT … FOR UPDATE); 404 when missing. */
export async function lockLicense(ctx: Pick<ServiceCtx, 'tx'>, id: string): Promise<LicenseRow> {
  const row = maybeOne(await ctx.tx<LicenseRow[]>`select * from licenses where id = ${id} for update`);
  if (!row) notFound('There is no such license.');
  return row;
}

/** lockLicense, plus 409 license_withdrawn when it is withdrawn. */
export async function lockActiveLicense(ctx: Pick<ServiceCtx, 'tx'>, id: string): Promise<LicenseRow> {
  const row = await lockLicense(ctx, id);
  if (row.status === 'withdrawn') fail('license_withdrawn', 'This license is withdrawn: its keys are no longer handed out, renewed or changed.');
  return row;
}

/**
 * Locks the license's customer(s), then the license: the order every writer
 * takes (customer before license), so a license action never deadlocks with
 * the personal flow or a member change. `alsoCustomer` (a move target) is
 * locked together with the current customer, in id order. 404 when the
 * license is missing, 409 license_withdrawn when it is withdrawn, 409
 * conflict when it moved to another customer in the meantime.
 */
export async function lockForAction(ctx: Pick<ServiceCtx, 'tx'>, id: string, alsoCustomer?: string): Promise<{ row: LicenseRow; customer: CustomerRow; also: CustomerRow | null }> {
  const peek = await getLicenseRow(ctx.tx, id);
  const ids = [...new Set([peek.customer_id, ...(alsoCustomer ? [alsoCustomer] : [])])].sort();
  const locked = new Map<string, CustomerRow>();
  for (const customerId of ids) locked.set(customerId, await lockCustomer(ctx, customerId));
  const row = await lockActiveLicense(ctx, id);
  const customer = locked.get(row.customer_id);
  if (!customer) fail('conflict', 'The license moved to another customer in the meantime. Try again.');
  return { row, customer, also: alsoCustomer ? (locked.get(alsoCustomer) ?? null) : null };
}

/** The advisory lock key of the personal flow (7403 migrations, 7404 test templates, 7406 public limits, 7407 user roles). */
export const PERSONAL_FLOW_LOCK = 7405;

/**
 * Serialises the personal flow (services/personal.ts), request decisions
 * and withdrawals within the transaction: they touch requests, person
 * customers and their licenses in different orders.
 */
export async function lockPersonalFlow(ctx: Pick<ServiceCtx, 'tx'>): Promise<void> {
  await ctx.tx`select pg_advisory_xact_lock(${PERSONAL_FLOW_LOCK})`;
}

/** The current (unsuperseded) key of a license. */
export async function currentKey(q: Queryable, licenseId: string): Promise<KeyRow> {
  const row = maybeOne(await q<KeyRow[]>`select * from license_keys where license_id = ${licenseId} and superseded_at is null`);
  if (!row) throw new Error(`license ${licenseId} has no current key`);
  return row;
}

/** The latest expiry of any key of the license (keepsWorkingUntil). */
export async function latestKeyExpiry(q: Queryable, licenseId: string): Promise<Date> {
  const rows = await q<{ max: Date | null }[]>`select max(expires_at) as max from license_keys where license_id = ${licenseId}`;
  const max = rows[0]?.max;
  if (!max) throw new Error(`license ${licenseId} has no keys`);
  return max;
}

// --- signing --------------------------------------------------------------------

/** A key just signed and stored. */
export interface SignedKey {
  id: string;
  kid: string;
  token: string;
  issuedAt: Date;
  expiresAt: Date;
}

/**
 * 429 issuing_quota when the acting admin signed ISSUE_MAX_PER_ACTOR_PER_DAY
 * keys (issue, renew, change) in the last 24 hours. Only user actors have a
 * quota (the personal flow has its own daily cap).
 */
/** The advisory lock namespace of the issuing quota (pg_advisory_xact_lock(7408, hashtext(user id))). */
const ISSUING_QUOTA_LOCK = 7408;

export async function checkIssuingQuota(ctx: ServiceCtx): Promise<void> {
  if (ctx.actor.kind !== 'user') return;
  // Held to the commit: one actor's parallel requests count each other's keys.
  await ctx.tx`select pg_advisory_xact_lock(${ISSUING_QUOTA_LOCK}, hashtext(${ctx.actor.userId}))`;
  const since = new Date(ctx.now.getTime() - DAY_SECONDS * 1000);
  const rows = await ctx.tx<{ n: string }[]>`
    select count(*) as n from license_keys
     where issued_by = ${ctx.actor.userId} and issued_at > ${since} and reason in ('issue', 'renew', 'change')`;
  const max = ctx.config.issuing.maxPerActorPerDay;
  if (toInt(rows[0]?.n) >= max) {
    fail('issuing_quota', `You signed ${max} keys in the last 24 hours, the most one person may (ISSUE_MAX_PER_ACTOR_PER_DAY). Try again later or ask another admin.`);
  }
}

/**
 * Signs a key for the license's terms and stores it as the current key:
 * the previous current key (if any) is superseded by it. The one place the
 * portal signs (spec §2: one log line per signature, never the token).
 */
export async function signKey(ctx: ServiceCtx, licenseId: string, terms: KeyTerms, reason: Exclude<KeyReason, 'import'>): Promise<SignedKey> {
  const signer = requireSigner(ctx);
  const claims = claimsFor(licenseId, terms, ctx.now);
  if (claims.exp <= unixSeconds(ctx.now) + DAY_SECONDS) throw new HttpError(400, 'invalid', 'The key must stay valid for more than a day from now.', { fields: ['expiresOn'] });
  let token: string;
  try {
    token = await signer.sign(claims);
  } catch (e) {
    ctx.log.error('signing.refused', { jti: licenseId, reason, err: e });
    throw e;
  }
  const keyId = newId('key');
  const issuedAt = new Date(claims.iat * 1000);
  const expiresAt = new Date(claims.exp * 1000);
  const by = actorColumns(ctx.actor);
  // The partial unique index allows one current key: supersede first, then
  // insert, then point the old key at the new one (superseded_by is a
  // foreign key to the new row).
  const previous = await ctx.tx<{ id: string }[]>`
    update license_keys set superseded_at = ${ctx.now} where license_id = ${licenseId} and superseded_at is null returning id`;
  const snapshot: Record<string, unknown> = { ...claims, features: claims.features ? [...claims.features] : undefined };
  if (snapshot.features === undefined) delete snapshot.features;
  await ctx.tx`
    insert into license_keys (id, license_id, kid, token, claims, issued_at, expires_at, issued_by, issued_by_label, reason)
    values (${keyId}, ${licenseId}, ${signer.kid}, ${token}, ${snapshot}, ${issuedAt}, ${expiresAt}, ${by.id}, ${by.label}, ${reason})`;
  if (previous.length > 0) {
    await ctx.tx`update license_keys set superseded_by = ${keyId} where id = ${previous[0]!.id}`;
  }
  ctx.log.info('license key signed', {
    jti: licenseId,
    keyId,
    kid: signer.kid,
    reason,
    plan: claims.plan,
    nodes: claims.nodes,
    features: claims.features ?? null,
    exp: claims.exp,
    actor: by.label,
  });
  return { id: keyId, kid: signer.kid, token, issuedAt, expiresAt };
}

export interface NewLicense {
  /** Default: a new lic_ id. */
  id?: string;
  customerId: string;
  terms: KeyTerms;
  agreementRef?: string;
  notes?: string;
  attestation: Attestation | null;
  source: LicenseSource;
  /** Extra facts for the license.issued audit event (requestId, autoApproved). */
  auditDetails?: Record<string, string | number | boolean | null>;
}

/** Creates a license with its first key (reason issue), audited license.issued. The caller checked the policy. */
export async function insertLicense(ctx: ServiceCtx, input: NewLicense): Promise<{ row: LicenseRow; key: SignedKey }> {
  requireSigner(ctx);
  const id = input.id ?? newLicenseId();
  const by = actorColumns(ctx.actor);
  const t = input.terms;
  const expiresAt = new Date((claimsFor(id, t, ctx.now).exp) * 1000);
  await ctx.tx`
    insert into licenses (id, customer_id, licensee, plan, nodes, features, expires_at, agreement_ref, notes, attestation, source,
                          created_at, created_by, created_by_label, updated_at)
    values (${id}, ${input.customerId}, ${t.licensee}, ${t.plan}, ${t.nodes}, ${t.features === null ? null : ctx.tx.array([...t.features], 'TEXT')},
            ${expiresAt}, ${input.agreementRef ?? ''}, ${input.notes ?? ''}, ${input.attestation}, ${input.source},
            ${ctx.now}, ${by.id}, ${by.label}, ${ctx.now})`;
  const key = await signKey(ctx, id, t, 'issue');
  await auditCtx(ctx, {
    action: 'license.issued',
    targetType: 'license',
    targetId: id,
    details: {
      customerId: input.customerId,
      licensee: t.licensee,
      plan: t.plan,
      nodes: t.nodes,
      features: t.features === null ? null : [...t.features],
      expiresOn: t.expiresOn,
      source: input.source,
      keyId: key.id,
      kid: key.kid,
      ...input.auditDetails,
    },
  });
  return { row: await getLicenseRow(ctx.tx, id), key };
}

/** Signs a renewal (same claims, later expiry) for a locked, active license; audited license.renewed. */
export async function renewLicenseRow(ctx: ServiceCtx, row: LicenseRow, expiresOn: ExpiresOn, auditDetails: Record<string, string | null> = {}): Promise<{ row: LicenseRow; key: SignedKey }> {
  const terms: KeyTerms = { ...termsOf(row), expiresOn };
  const key = await signKey(ctx, row.id, terms, 'renew');
  const rows = await ctx.tx<LicenseRow[]>`update licenses set expires_at = ${key.expiresAt}, updated_at = ${ctx.now} where id = ${row.id} returning *`;
  await auditCtx(ctx, {
    action: 'license.renewed',
    targetType: 'license',
    targetId: row.id,
    details: { from: expiresOnOf(row.expires_at), to: expiresOn, keyId: key.id, kid: key.kid, ...auditDetails },
  });
  return { row: rows[0]!, key };
}

/** The key mail for a license (spec §6 "Key mail"). */
export function licenseKeyMail(ctx: Pick<ServiceCtx, 'config'>, row: LicenseRow, token: string, kind: KeyMailKind, to: string, inviteUrl: string | null = null): MailMessage {
  return keyMail({
    to,
    kind,
    license: { id: row.id, licensee: row.licensee, plan: row.plan, nodes: row.nodes, features: row.features, expiresOn: expiresOnOf(row.expires_at) },
    token,
    inviteUrl,
    publicUrl: ctx.config.publicUrl,
  });
}

/** Queues the key mail for after the commit (a failure is logged; the key stays in the portal). */
export function mailKeyAfterCommit(ctx: ServiceCtx, msg: MailMessage): void {
  if (!ctx.mailer.configured) {
    ctx.log.warn('mail is not configured: key not mailed', { toDomain: msg.to.slice(msg.to.lastIndexOf('@') + 1) });
    return;
  }
  ctx.afterCommit(() => ctx.mailer.send(msg));
}

// --- DTOs -----------------------------------------------------------------------

interface SummaryRow {
  id: string;
  customer_id: string;
  customer_name: string;
  customer_kind: CustomerRow['kind'];
  licensee: string;
  plan: Plan;
  nodes: number;
  features: string[] | null;
  expires_at: Date;
  status: LicenseRow['status'];
  source: LicenseSource;
  created_at: Date;
  updated_at: Date;
  latest_key_expires_at: Date | null;
}

function summaryDto(r: SummaryRow, now: Date): LicenseSummary {
  return {
    id: r.id,
    customer: { id: r.customer_id, name: r.customer_name, kind: r.customer_kind },
    licensee: r.licensee,
    plan: r.plan,
    nodes: r.nodes,
    features: r.features,
    expiresOn: expiresOnOf(r.expires_at),
    state: stateOf(r, now),
    keepsWorkingUntil: keepsWorkingUntilOf(r.latest_key_expires_at ?? r.expires_at),
    source: r.source,
    createdAt: r.created_at.toISOString(),
    updatedAt: r.updated_at.toISOString(),
  };
}

/** The license list's filter in SQL, the state computed as shared/catalog licenseState does (whole seconds). */
function licenseFilter(q: Queryable, query: Omit<LicenseListQuery, 'limit' | 'offset'>, now: Date): Fragment {
  const nowSec = unixSeconds(now);
  const at = (sec: number): Date => new Date(sec * 1000);
  const nowAt = at(nowSec);
  const warnAt = at(nowSec + EXPIRY_WARNING_DAYS * DAY_SECONDS);
  const graceAt = at(nowSec - GRACE_SECONDS);
  const pattern = query.q ? containsPattern(query.q) : null;
  const state = (): Fragment => {
    switch (query.state) {
      case undefined:
        return q``;
      case 'withdrawn':
        return q`and l.status = 'withdrawn'`;
      case 'active':
        return q`and l.status = 'active' and l.expires_at > ${warnAt}`;
      case 'expiring':
        return q`and l.status = 'active' and l.expires_at > ${nowAt} and l.expires_at <= ${warnAt}`;
      case 'grace':
        return q`and l.status = 'active' and l.expires_at <= ${nowAt} and l.expires_at > ${graceAt}`;
      case 'expired':
        return q`and l.status = 'active' and l.expires_at <= ${graceAt}`;
    }
  };
  const within = query.expiringWithinDays;
  return q`where true
    ${pattern ? q`and (l.id ilike ${pattern} escape '\\' or l.licensee ilike ${pattern} escape '\\' or c.name ilike ${pattern} escape '\\')` : q``}
    ${state()}
    ${query.plan ? q`and l.plan = ${query.plan}` : q``}
    ${query.customerId ? q`and l.customer_id = ${query.customerId}` : q``}
    ${within !== undefined ? q`and l.status = 'active' and l.expires_at > ${nowAt} and l.expires_at <= ${at(nowSec + within * DAY_SECONDS)}` : q``}`;
}

/** GET /licenses: the paged, filtered list (newest first). */
export async function listLicenses(q: Queryable, query: LicenseListQuery & { limit: number; offset: number }, now: Date): Promise<Page<LicenseSummary>> {
  const rows = await q<SummaryRow[]>`
    select l.id, l.customer_id, c.name as customer_name, c.kind as customer_kind, l.licensee, l.plan, l.nodes, l.features,
           l.expires_at, l.status, l.source, l.created_at, l.updated_at,
           (select max(k.expires_at) from license_keys k where k.license_id = l.id) as latest_key_expires_at
      from licenses l join customers c on c.id = l.customer_id
      ${licenseFilter(q, query, now)}
     order by l.created_at desc, l.id
     limit ${query.limit} offset ${query.offset}`;
  const total = await q<{ n: string }[]>`select count(*) as n from licenses l join customers c on c.id = l.customer_id ${licenseFilter(q, query, now)}`;
  return { items: rows.map((r) => summaryDto(r, now)), total: toInt(total[0]?.n) };
}

/** Every license of a customer (the customer page), newest first. */
export async function licensesOfCustomer(q: Queryable, customerId: string, now: Date): Promise<LicenseSummary[]> {
  const page = await listLicenses(q, { customerId, limit: 10_000, offset: 0 }, now);
  return page.items;
}

function keyRecord(k: KeyRow): KeyRecord {
  return {
    id: k.id,
    kid: k.kid,
    reason: k.reason,
    issuedAt: k.issued_at.toISOString(),
    expiresOn: expiresOnOf(k.expires_at),
    issuedBy: actorRef(k.issued_by, k.issued_by_label),
    current: k.superseded_at === null,
    supersededAt: k.superseded_at ? k.superseded_at.toISOString() : null,
    supersededBy: k.superseded_by,
  };
}

/**
 * The keys newest first, following the superseded_by chain back from the
 * current key (iat has whole seconds, so two keys signed in one second
 * cannot be ordered by time); keys outside the chain follow by issued_at.
 */
function keyHistory(keys: KeyRow[]): KeyRow[] {
  const byReplacement = new Map<string, KeyRow>();
  for (const k of keys) if (k.superseded_by) byReplacement.set(k.superseded_by, k);
  const ordered: KeyRow[] = [];
  const seen = new Set<string>();
  for (let k = keys.find((x) => x.superseded_at === null); k && !seen.has(k.id); k = byReplacement.get(k.id)) {
    ordered.push(k);
    seen.add(k.id);
  }
  return [...ordered, ...keys.filter((k) => !seen.has(k.id))];
}

/** "Works with kudurru <version> or later" for a kid, when it is the signer's and LICENSE_SIGNING_TRUSTED_SINCE is set. */
export function worksWithKudurru(signing: SigningState, kid: string): string | null {
  const signer = signing.signer;
  return signer && signer.kid === kid && signer.trustedSince ? signer.trustedSince : null;
}

/** GET /licenses/:id and the answer of every license action. */
export async function licenseDetail(q: Queryable, id: string, now: Date, signing: SigningState): Promise<LicenseDetail> {
  const rows = await q<(LicenseRow & { customer_name: string; customer_kind: CustomerRow['kind'] })[]>`
    select l.*, c.name as customer_name, c.kind as customer_kind from licenses l join customers c on c.id = l.customer_id where l.id = ${id}`;
  const row = rows[0];
  if (!row) notFound('There is no such license.');
  const keys = keyHistory(await q<KeyRow[]>`select * from license_keys where license_id = ${id} order by issued_at desc, id desc`);
  const current = keys[0];
  if (!current || current.superseded_at !== null) throw new Error(`license ${id} has no current key`);
  const latest = keys.reduce((m, k) => (k.expires_at > m ? k.expires_at : m), keys[0]!.expires_at);
  const keepsWorkingUntil = keepsWorkingUntilOf(latest);
  const createdBy: ActorRef = actorRef(row.created_by, row.created_by_label);
  return {
    id: row.id,
    customer: customerRef({ id: row.customer_id, name: row.customer_name, kind: row.customer_kind }),
    licensee: row.licensee,
    plan: row.plan,
    nodes: row.nodes,
    features: row.features,
    expiresOn: expiresOnOf(row.expires_at),
    state: stateOf(row, now),
    keepsWorkingUntil,
    source: row.source,
    createdAt: row.created_at.toISOString(),
    updatedAt: row.updated_at.toISOString(),
    currentKey: { id: current.id, kid: current.kid, token: current.token, issuedAt: current.issued_at.toISOString(), expiresOn: expiresOnOf(current.expires_at) },
    keys: keys.map(keyRecord),
    attestation: row.attestation,
    agreementRef: row.agreement_ref,
    notes: row.notes,
    createdBy,
    worksWithKudurru: worksWithKudurru(signing, current.kid),
    withdrawn:
      row.status === 'withdrawn' && row.withdrawn_at
        ? {
            at: row.withdrawn_at.toISOString(),
            by: actorRef(row.withdrawn_by, row.withdrawn_by_label),
            reason: row.withdraw_reason,
            customerMessage: row.withdraw_message,
            keepsWorkingUntil,
          }
        : null,
  };
}

// --- admin actions --------------------------------------------------------------

/** The validated body of POST /licenses (api/licenses.ts). */
export interface IssueInput extends KeyTerms {
  customerId?: string;
  newCustomer?: NewCustomerBody;
  agreementRef?: string;
  notes?: string;
  emailCustomer?: boolean;
}

/** The address "email the customer" goes to, or 400 when there is none or mail is not configured. */
function customerMailTarget(ctx: ServiceCtx, contactEmail: string | null): string {
  if (!ctx.mailer.configured) throw new HttpError(400, 'invalid', 'Mail is not configured on this portal: issue without emailing and hand the key over yourself.', { fields: ['emailCustomer'] });
  if (!contactEmail) throw new HttpError(400, 'invalid', 'The customer has no contact email to send the key to.', { fields: ['emailCustomer'] });
  return contactEmail;
}

/** POST /licenses: a license with its first key, for an existing customer or a new one. Returns the license id. */
export async function issueLicense(ctx: ServiceCtx, input: IssueInput): Promise<string> {
  requireSigner(ctx);
  const terms: KeyTerms = { licensee: input.licensee, plan: input.plan, nodes: input.nodes, features: input.features, expiresOn: input.expiresOn };
  assertPolicy(terms, ctx.now);
  await checkIssuingQuota(ctx);
  let customer: CustomerRow;
  if (input.customerId) customer = await lockCustomer(ctx, input.customerId);
  else if (input.newCustomer) customer = await createCustomer(ctx, input.newCustomer);
  else throw new HttpError(400, 'invalid', 'Choose a customer or create one.', { fields: ['customerId'] });
  const to = input.emailCustomer ? customerMailTarget(ctx, customer.contact_email) : null;
  const attestation: Attestation | null = input.plan === 'personal' && ctx.actor.kind === 'user' ? { source: 'admin', by: ctx.actor.userId, at: ctx.now.toISOString() } : null;
  const { row, key } = await insertLicense(ctx, {
    customerId: customer.id,
    terms,
    agreementRef: input.agreementRef,
    notes: input.notes,
    attestation,
    source: 'admin',
  });
  if (to) mailKeyAfterCommit(ctx, licenseKeyMail(ctx, row, key.token, 'issued', to));
  return row.id;
}

/** POST /licenses/:id/renew: the same claims with a later expiry. */
export async function renewLicense(ctx: ServiceCtx, id: string, body: RenewBody): Promise<void> {
  requireSigner(ctx);
  const { row, customer } = await lockForAction(ctx, id);
  const current = expiresOnOf(row.expires_at);
  if (body.expiresOn <= current) throw new HttpError(400, 'invalid', `A renewal must expire after the current expiry (${current} UTC).`, { fields: ['expiresOn'] });
  assertPolicy({ ...termsOf(row), expiresOn: body.expiresOn }, ctx.now);
  await checkIssuingQuota(ctx);
  const to = body.emailCustomer ? customerMailTarget(ctx, customer.contact_email) : null;
  const renewed = await renewLicenseRow(ctx, row, body.expiresOn);
  if (to) mailKeyAfterCommit(ctx, licenseKeyMail(ctx, renewed.row, renewed.key.token, 'renewed', to));
}

function sameFeatures(a: readonly string[] | null, b: readonly string[] | null): boolean {
  return JSON.stringify(a) === JSON.stringify(b);
}

/** POST /licenses/:id/change: a new key with the changed claims (the issue policy applies to the result), and/or a move to another customer. */
export async function changeLicense(ctx: ServiceCtx, id: string, body: ChangeBody): Promise<void> {
  const { row, customer, also } = await lockForAction(ctx, id, body.customerId);
  const before = termsOf(row);
  const next: KeyTerms = {
    licensee: body.licensee ?? before.licensee,
    plan: body.plan ?? before.plan,
    nodes: body.nodes ?? before.nodes,
    features: body.features !== undefined ? body.features : before.features,
    expiresOn: body.expiresOn ?? before.expiresOn,
  };
  const changes: Record<string, { from: string | number | string[] | null; to: string | number | string[] | null }> = {};
  if (next.licensee !== before.licensee) changes.licensee = { from: before.licensee, to: next.licensee };
  if (next.plan !== before.plan) changes.plan = { from: before.plan, to: next.plan };
  if (next.nodes !== before.nodes) changes.nodes = { from: before.nodes, to: next.nodes };
  if (!sameFeatures(next.features, before.features)) changes.features = { from: before.features && [...before.features], to: next.features && [...next.features] };
  if (next.expiresOn !== before.expiresOn) changes.expiresOn = { from: before.expiresOn, to: next.expiresOn };
  const resign = Object.keys(changes).length > 0;
  const move = body.customerId !== undefined && body.customerId !== row.customer_id;
  if (!resign && !move) throw new HttpError(400, 'invalid', 'Nothing changes: give at least one new value.');

  if (resign) {
    requireSigner(ctx);
    assertPolicy(next, ctx.now);
    await checkIssuingQuota(ctx);
  }
  let owner = customer;
  if (move && also) {
    owner = also;
    await ctx.tx`update licenses set customer_id = ${also.id}, updated_at = ${ctx.now} where id = ${id}`;
    await auditCtx(ctx, { action: 'license.moved', targetType: 'license', targetId: id, details: { from: row.customer_id, to: also.id } });
  }
  const to = body.emailCustomer ? customerMailTarget(ctx, owner.contact_email) : null;
  let token: string;
  if (resign) {
    const key = await signKey(ctx, id, next, 'change');
    await ctx.tx`
      update licenses set licensee = ${next.licensee}, plan = ${next.plan}, nodes = ${next.nodes},
             features = ${next.features === null ? null : ctx.tx.array([...next.features], 'TEXT')}, expires_at = ${key.expiresAt}, updated_at = ${ctx.now}
       where id = ${id}`;
    await auditCtx(ctx, { action: 'license.changed', targetType: 'license', targetId: id, details: { changes, keyId: key.id, kid: key.kid } });
    token = key.token;
  } else {
    token = (await currentKey(ctx.tx, id)).token;
  }
  if (to) mailKeyAfterCommit(ctx, licenseKeyMail(ctx, await getLicenseRow(ctx.tx, id), token, resign ? 'issued' : 'resent', to));
}

/**
 * POST /licenses/:id/withdraw (final): records the end, hides the key from
 * customers, blocks renewal and automatic re-issue, cancels the open
 * personal requests of the customer's people and, with blockPersonal,
 * blocks their addresses. Keys already installed keep working until
 * keepsWorkingUntil (spec §0.8).
 */
export async function withdrawLicense(ctx: ServiceCtx, id: string, body: WithdrawBody): Promise<void> {
  await lockPersonalFlow(ctx);
  const { row } = await lockForAction(ctx, id);
  const by = actorColumns(ctx.actor);
  await ctx.tx`
    update licenses set status = 'withdrawn', withdrawn_at = ${ctx.now}, withdrawn_by = ${by.id}, withdrawn_by_label = ${by.label},
           withdraw_reason = ${body.reason}, withdraw_message = ${body.customerMessage ?? ''}, updated_at = ${ctx.now}
     where id = ${id}`;
  const emails = (await ctx.tx<{ email: string }[]>`select email from customer_members where customer_id = ${row.customer_id} order by email`).map((r) => r.email);
  const cancelled = await ctx.tx<{ id: string; requester_email: string }[]>`
    update license_requests set status = 'cancelled', decision_note = 'license withdrawn', decided_at = ${ctx.now},
           decided_by = ${by.id}, decided_by_label = ${by.label}
     where status = 'open' and requester_email = any(${ctx.tx.array(emails, 'TEXT')})
     returning id, requester_email`;
  for (const r of cancelled) {
    await auditCtx(ctx, { action: 'request.cancelled', targetType: 'request', targetId: r.id, details: { email: r.requester_email, licenseId: id, note: 'license withdrawn' } });
  }
  const blocked = body.blockPersonal ? await blockEmails(ctx, emails, `license ${id} withdrawn`) : [];
  const keepsWorkingUntil = keepsWorkingUntilOf(await latestKeyExpiry(ctx.tx, id));
  await auditCtx(ctx, {
    action: 'license.withdrawn',
    targetType: 'license',
    targetId: id,
    details: { reason: body.reason, customerMessage: body.customerMessage ?? '', blockPersonal: body.blockPersonal ?? false, blocked, cancelledRequests: cancelled.map((r) => r.id), keepsWorkingUntil },
  });
}

/** The latest exp the portal stores (9999-12-31T23:59:59Z; ExpiresOn has four-digit years). */
const MAX_STORABLE_EXP = 253_402_300_799;

/** POST /licenses/import: a key signed before the portal existed (licensegen), verified against the trusted keyring. Returns the license id (= jti). */
export async function importLicense(ctx: ServiceCtx, body: ImportBody): Promise<string> {
  let verified;
  try {
    verified = await verify(body.token, ctx.signing.keyring);
  } catch (e) {
    if (!(e instanceof VerifyError)) throw e;
    const message =
      e.code === 'unknown_key'
        ? 'The key is signed with a key that is not in LICENSE_TRUSTED_KEYS_DIR, so no kudurru release this portal knows accepts it.'
        : `The key does not verify: ${e.message}.`;
    throw new HttpError(400, 'invalid', message, { fields: ['token'] });
  }
  const c = verified.claims;
  if (c.jti.length > MAX_JTI_LENGTH) throw new HttpError(400, 'invalid', `The license ID (jti) is longer than ${MAX_JTI_LENGTH} characters.`, { fields: ['token'] });
  if (c.nodes > MAX_NODES) throw new HttpError(400, 'invalid', `The key allows ${c.nodes} nodes; the portal stores at most ${MAX_NODES}.`, { fields: ['token'] });
  if (c.exp < 0 || c.exp > MAX_STORABLE_EXP) throw new HttpError(400, 'invalid', 'The key expires outside the dates the portal stores (1970 to 9999).', { fields: ['token'] });
  if (maybeOne(await ctx.tx<{ id: string }[]>`select id from licenses where id = ${c.jti}`)) {
    fail('conflict', `A license with the ID ${c.jti} already exists.`);
  }
  const customer = await lockCustomer(ctx, body.customerId);
  const by = actorColumns(ctx.actor);
  const features = c.features && c.features.length > 0 ? [...c.features] : null;
  const expiresAt = new Date(c.exp * 1000);
  await ctx.tx`
    insert into licenses (id, customer_id, licensee, plan, nodes, features, expires_at, source, created_at, created_by, created_by_label, updated_at)
    values (${c.jti}, ${customer.id}, ${c.sub}, ${c.plan}, ${c.nodes}, ${features === null ? null : ctx.tx.array(features, 'TEXT')}, ${expiresAt},
            'import', ${ctx.now}, ${by.id}, ${by.label}, ${ctx.now})`;
  const keyId = newId('key');
  const snapshot: Record<string, unknown> = { ...c, ...(features ? { features } : {}) };
  await ctx.tx`
    insert into license_keys (id, license_id, kid, token, claims, issued_at, expires_at, issued_by, issued_by_label, reason)
    values (${keyId}, ${c.jti}, ${verified.kid}, ${verified.token}, ${snapshot}, ${new Date(c.iat * 1000)}, ${expiresAt}, ${by.id}, ${by.label}, 'import')`;
  await auditCtx(ctx, {
    action: 'license.imported',
    targetType: 'license',
    targetId: c.jti,
    details: { customerId: customer.id, licensee: c.sub, plan: c.plan, nodes: c.nodes, features, expiresOn: expiresOnOf(c.exp), keyId, kid: verified.kid, ...(c.nbf ? { nbf: c.nbf } : {}) },
  });
  ctx.log.info('license key imported', { jti: c.jti, keyId, kid: verified.kid, plan: c.plan, nodes: c.nodes, features, exp: c.exp, actor: by.label });
  return c.jti;
}

/**
 * POST /licenses/:id/email: mails the current key now (503 unavailable when
 * the mail cannot go out). Three steps so no connection or row lock is held
 * while the SMTP server answers: read the key (a short transaction that
 * also refuses withdrawn licenses), send, then audit license.key_emailed
 * only once the mail went out.
 */
export async function emailLicenseKey(deps: ServiceDeps, actor: Actor, id: string, to: string | undefined): Promise<EmailKeyResult> {
  const plan = await inTransaction(deps, actor, async (ctx) => {
    const { row, customer } = await lockForAction(ctx, id);
    const target = to ?? customer.contact_email;
    if (!target) throw new HttpError(400, 'invalid', 'The customer has no contact email: give an address.', { fields: ['to'] });
    if (!ctx.mailer.configured) fail('unavailable', 'Mail is not configured on this portal.');
    const key = await currentKey(ctx.tx, id);
    return { target, keyId: key.id, message: licenseKeyMail(ctx, row, key.token, 'resent', target) };
  });
  await mailOrUnavailable(deps.mailer, plan.message, deps.log, 'The key');
  await inTransaction(deps, actor, (ctx) => auditCtx(ctx, { action: 'license.key_emailed', targetType: 'license', targetId: id, details: { to: plan.target, keyId: plan.keyId } }));
  return { sentTo: plan.target };
}

/** GET /licenses/:id/keys/:keyId/download: any key of the license (audited license.key_downloaded). */
export async function downloadLicenseKey(ctx: ServiceCtx, id: string, keyId: string): Promise<string> {
  const key = maybeOne(await ctx.tx<KeyRow[]>`select * from license_keys where id = ${keyId} and license_id = ${id}`);
  if (!key) notFound('There is no such key.');
  await auditCtx(ctx, { action: 'license.key_downloaded', targetType: 'license', targetId: id, details: { keyId, current: key.superseded_at === null, by: 'admin' } });
  return key.token;
}

// --- the customer's own view (self.*) ---------------------------------------------
//
// Customer isolation is enforced in the SQL of every query here: a license
// is visible only through a customer_members row linked to the user. The
// DTOs carry no notes, agreement references, withdraw reasons or actors.

interface MyLicenseRow extends LicenseRow {
  customer_name: string;
  latest_key_expires_at: Date | null;
  /** A member address is blocked or the customer has a withdrawn license: renewal needs a person (personal.ts personalReviewReason). */
  renewal_needs_review: boolean;
}

function myLicenseDto(r: MyLicenseRow, now: Date): MyLicense {
  const renewal = r.status === 'active' && !r.renewal_needs_review ? personalRenewal(termsOf(r), unixSeconds(r.expires_at), now) : null;
  return {
    id: r.id,
    customer: { id: r.customer_id, name: r.customer_name },
    licensee: r.licensee,
    plan: r.plan,
    nodes: r.nodes,
    features: r.features,
    expiresOn: expiresOnOf(r.expires_at),
    state: stateOf(r, now),
    keepsWorkingUntil: keepsWorkingUntilOf(r.latest_key_expires_at ?? r.expires_at),
    renewal: renewal ? { expiresOn: renewal } : null,
  };
}

function selectMyLicenses(q: Queryable, userId: string, extra: Fragment): Promise<MyLicenseRow[]> {
  return q<MyLicenseRow[]>`
    select l.*, c.name as customer_name,
           (select max(k.expires_at) from license_keys k where k.license_id = l.id) as latest_key_expires_at,
           (exists (select 1 from licenses w where w.customer_id = l.customer_id and w.status = 'withdrawn')
            or exists (select 1 from customer_members bm join blocked_emails b on b.email = bm.email where bm.customer_id = l.customer_id)) as renewal_needs_review
      from licenses l join customers c on c.id = l.customer_id
     where exists (select 1 from customer_members m where m.customer_id = l.customer_id and m.user_id = ${userId})
       ${extra}
     order by l.created_at desc, l.id`;
}

/** GET /me/licenses: the licenses of every customer the user is a linked member of. */
export async function myLicenses(q: Queryable, userId: string, now: Date): Promise<MyLicense[]> {
  return (await selectMyLicenses(q, userId, q``)).map((r) => myLicenseDto(r, now));
}

async function myLicenseRow(q: Queryable, userId: string, id: string): Promise<MyLicenseRow> {
  const row = (await selectMyLicenses(q, userId, q`and l.id = ${id}`))[0];
  if (!row) notFound('There is no such license.');
  return row;
}

/** GET /me/licenses/:id: the license with its current key (none when withdrawn); 404 unless the user is a member. */
export async function myLicenseDetail(q: Queryable, userId: string, id: string, now: Date, signing: SigningState): Promise<MyLicenseDetail> {
  const row = await myLicenseRow(q, userId, id);
  const base = myLicenseDto(row, now);
  const people = await q<{ email: string; user_id: string | null }[]>`select email, user_id from customer_members where customer_id = ${row.customer_id} order by email`;
  const key = row.status === 'withdrawn' ? null : await currentKey(q, id);
  const kid = key?.kid ?? (await currentKey(q, id)).kid;
  const a = row.attestation;
  return {
    ...base,
    key: key ? { kid: key.kid, token: key.token, issuedAt: key.issued_at.toISOString(), expiresOn: expiresOnOf(key.expires_at) } : null,
    worksWithKudurru: worksWithKudurru(signing, kid),
    people: people.map((p) => ({ email: p.email, status: p.user_id ? 'linked' : 'invited' })),
    withdrawn:
      row.status === 'withdrawn' && row.withdrawn_at ? { at: row.withdrawn_at.toISOString(), keepsWorkingUntil: base.keepsWorkingUntil, customerMessage: row.withdraw_message } : null,
    attestation: a ? { source: a.source, at: a.at, statement: a.statement ?? null, license: a.license ?? null } : null,
  };
}

/** GET /me/licenses/:id/key: the current key (409 when withdrawn), audited license.key_downloaded. */
export async function myLicenseKey(ctx: ServiceCtx, userId: string, id: string): Promise<string> {
  const row = await myLicenseRow(ctx.tx, userId, id);
  if (row.status === 'withdrawn') fail('license_withdrawn', 'This license is withdrawn: its key is no longer handed out.');
  const key = await currentKey(ctx.tx, id);
  await auditCtx(ctx, { action: 'license.key_downloaded', targetType: 'license', targetId: id, details: { keyId: key.id, current: true, by: 'member' } });
  return key.token;
}
