// The personal-key decision tree (design spec §6 "Personal keys", §10
// "personal flow outcomes"). With approval required (the default): a new
// key and a renewal in the window wait (pending_review, auto_approve_off),
// the requester and every approver are mailed (each approver with a
// one-click link of their own; ADMIN_NOTIFY_EMAIL or the admin users), a
// resend stays automatic. With PERSONAL_AUTO_APPROVE=true: issued, resent,
// renewed (in the window, in grace, expired) and daily_cap. Both: blocked
// and withdrawn_history wait without a link, one open request per address,
// person customers only (never an organization), portal requests linked to
// the user, web requests with an invite link in the key mail, no signer =
// nothing stored, a failed mail after the commit keeps the key; and the
// portal routes (verified email required, renew own key with a fresh
// certification: renewed, or a request that waits).
import { afterAll, beforeAll, expect, test } from 'bun:test';
import type { Attestation, PersonalOutcome } from '../../shared/api';
import { ALL_FEATURES_LABEL, ATTESTATION_STATEMENT, KEY_IS_NOT_A_LICENSE } from '../../shared/catalog';
import { addDays, expFor, expiresOnOf, todayUtc } from '../../shared/dates';
import { meHandlers } from '../api/me';
import type { Config } from '../config';
import { mergeHandlers } from '../http/router';
import { HttpError } from '../http/errors';
import { verify } from '../licensing/jws';
import { loadSigning } from '../licensing/keyring';
import { signingUnavailable, type SigningState } from '../licensing/types';
import { MemoryLogger, silentLogger } from '../log';
import { MemoryMailer } from '../mail/memory';
import { requestPersonalKey, type PersonalRequest } from '../services/personal';
import { inTransaction, publicActor, type Actor } from '../services/types';
import { callJson, makeTestApp, type TestApp } from './call';
import { describeDb, hasDb, useTestDb as fileDatabase } from './db';
import { createCustomer, createUser, signInAs, testConfig } from './fixtures';

/** PERSONAL_AUTO_APPROVE=true: the automatic path. */
const AUTO = (): Config => testConfig({ personal: { autoApprove: true } });

/** The one-click approve links in a set of mails. */
function approveTokens(texts: string[]): string[] {
  return texts.flatMap((t) => [...t.matchAll(/\/approve\?token=([A-Za-z0-9_-]{43})/g)].map((m) => m[1]!));
}

// One fresh database for this file (useTestDb, renamed: it is not a React hook).
const t = fileDatabase();
let signing: SigningState;

beforeAll(async () => {
  if (hasDb) signing = await loadSigning(testConfig(), silentLogger);
});

let n = 0;
function email(tag: string): string {
  n += 1;
  return `${tag}-${n}-${process.pid}@personal.example`;
}

function attestation(addr: string): Attestation {
  return { source: 'kudurru.io form', license: 'LicenseRef-Kudurru-Community', statement: ATTESTATION_STATEMENT, at: new Date().toISOString(), ip: '192.0.2.1', userAgent: 'test', email: addr };
}

interface RunOptions {
  now?: Date;
  config?: Config;
  mailer?: MemoryMailer;
  log?: MemoryLogger;
  signing?: SigningState;
  actor?: Actor;
}

/** requestPersonalKey in its own transaction, as the routes call it. */
async function run(req: Partial<PersonalRequest> & { email: string }, opts: RunOptions = {}) {
  const mailer = opts.mailer ?? new MemoryMailer();
  const log = opts.log ?? new MemoryLogger();
  const full: PersonalRequest = { name: 'Homelab Hank', use: 'my homelab', attestation: attestation(req.email), source: 'web', ...req };
  const result = await inTransaction(
    { db: t.db, config: opts.config ?? testConfig(), log, signing: opts.signing ?? signing, mailer, now: opts.now ?? new Date() },
    opts.actor ?? publicActor(full.email, { ip: '192.0.2.1', userAgent: 'test' }),
    (s) => requestPersonalKey(s, full),
  );
  return { ...result, mailer, log };
}

async function licenseOf(id: string) {
  return (await t.db<{ id: string; customer_id: string; plan: string; nodes: number; features: string[] | null; expires_at: Date; licensee: string; source: string; attestation: Attestation | null }[]>`
    select * from licenses where id = ${id}`)[0]!;
}

async function currentToken(id: string): Promise<string> {
  return (await t.db<{ token: string }[]>`select token from license_keys where license_id = ${id} and superseded_at is null`)[0]!.token;
}

describeDb('requestPersonalKey', () => {
  test('issued: a person customer, a member invite, a personal key for today + 365 days, mailed with an invite link', async () => {
    const addr = email('new');
    const r = await run({ email: addr, name: 'Zo\u00eb Example' }, { config: AUTO() });
    expect(r.outcome).toBe('issued');
    const lic = await licenseOf(r.licenseId!);
    expect(lic).toMatchObject({ plan: 'personal', nodes: 3, features: null, licensee: 'Zo\u00eb Example', source: 'web' });
    expect(expiresOnOf(lic.expires_at)).toBe(addDays(todayUtc(), 365));
    expect(lic.attestation).toMatchObject({ source: 'kudurru.io form', statement: ATTESTATION_STATEMENT, email: addr });
    const customer = (await t.db<{ kind: string; contact_email: string }[]>`select kind, contact_email from customers where id = ${lic.customer_id}`)[0]!;
    expect(customer).toEqual({ kind: 'person', contact_email: addr });
    const member = (await t.db<{ user_id: string | null; invite_token_hash: Buffer | null }[]>`select user_id, invite_token_hash from customer_members where customer_id = ${lic.customer_id} and email = ${addr}`)[0]!;
    expect(member.user_id).toBeNull();
    expect(member.invite_token_hash).not.toBeNull();

    const token = await currentToken(r.licenseId!);
    const claims = (await verify(token, signing.keyring)).claims;
    expect(claims).toMatchObject({ plan: 'personal', nodes: 3, sub: 'Zo\u00eb Example' });
    expect(claims.features).toBeUndefined();
    expect(claims.exp).toBe(expFor(addDays(todayUtc(), 365)));

    expect(r.mailer.sent).toHaveLength(1);
    const mail = r.mailer.sent[0]!;
    expect(mail.to).toBe(addr);
    expect(mail.subject).toBe('Your kudurru license key');
    expect(mail.text).toContain(token);
    expect(mail.text).toContain('community-development use only');
    expect(mail.text).toContain(KEY_IS_NOT_A_LICENSE);
    expect(mail.text).toContain('KUDURRU_LICENSE_KEY_FILE=/run/secrets/license_key');
    expect(mail.text).toContain('restart');
    expect(mail.text).toMatch(/http:\/\/localhost:8790\/invites\/[A-Za-z0-9_-]{43}/);
    const attachment = mail.attachments![0]!;
    expect(attachment).toEqual({ filename: 'license.key', content: token + '\n', contentType: 'text/plain; charset=us-ascii' });
    expect(/^[\x21-\x7e]+\n$/.test(attachment.content as string)).toBe(true);
  });

  test('resent: an active key far from expiry is mailed again, no new key', async () => {
    const addr = email('resend');
    const first = await run({ email: addr }, { config: AUTO() });
    const again = await run({ email: addr, name: 'Another Name' }, { config: AUTO() });
    expect(again.outcome).toBe('resent');
    expect(again.licenseId).toBe(first.licenseId);
    expect(await t.db`select 1 from license_keys where license_id = ${first.licenseId!}`).toHaveLength(1);
    expect(again.mailer.sent[0]!.subject).toBe('Your kudurru license key, sent again');
    expect(again.mailer.sent[0]!.text).toContain(await currentToken(first.licenseId!));
  });

  const renewals: [string, number, (expiresOn: string, today: string) => string][] = [
    ['within the renew window', -10, (e) => addDays(e, 365)],
    ['in grace', 5, (_e, today) => addDays(today, 365)],
    ['expired', 40, (_e, today) => addDays(today, 365)],
  ];
  for (const [name, offset, expected] of renewals) {
    test(`renewed ${name}: PERSONAL_TERM_DAYS after the later of today and the current expiry`, async () => {
      const addr = email('renew');
      const first = await run({ email: addr, name: 'Original Name' }, { config: AUTO() });
      const lic = await licenseOf(first.licenseId!);
      const expiresOn = expiresOnOf(lic.expires_at);
      const now = new Date(`${addDays(expiresOn, offset)}T12:00:00Z`);
      const r = await run({ email: addr, name: 'Different Name' }, { now, config: AUTO() });
      expect(r.outcome).toBe('renewed');
      expect(r.licenseId).toBe(first.licenseId);
      const after = await licenseOf(first.licenseId!);
      expect(expiresOnOf(after.expires_at)).toBe(expected(expiresOn, todayUtc(now)));
      expect(after.licensee).toBe('Original Name');
      const keys = await t.db<{ reason: string; superseded_at: Date | null }[]>`select reason, superseded_at from license_keys where license_id = ${first.licenseId!} order by issued_at`;
      expect(keys.map((k) => k.reason)).toEqual(['issue', 'renew']);
      expect(r.mailer.sent[0]!.subject).toBe('Your renewed kudurru license key');
    });
  }

  test('a hand-signed personal key outside the policy (10 nodes) is never renewed on its own: sent again', async () => {
    const addr = email('import');
    const c = await createCustomer(t.db, { kind: 'person', members: [addr] });
    const exp = new Date(Date.now() + 10 * 86_400_000);
    const id = `lic_imp${c.id.slice(4, 13)}`;
    await t.db`
      insert into licenses (id, customer_id, licensee, plan, nodes, expires_at, source, created_by_label)
      values (${id}, ${c.id}, 'Big Homelab', 'personal', 10, ${exp}, 'import', 'test')`;
    await t.db`
      insert into license_keys (id, license_id, kid, token, claims, issued_at, expires_at, issued_by_label, reason)
      values (${'key_' + id.slice(4)}, ${id}, 'server-test-1', 'imported.key.token', ${{}}, now(), ${exp}, 'test', 'import')`;
    const r = await run({ email: addr });
    expect(r).toMatchObject({ outcome: 'resent', licenseId: id });
    expect(r.log.find('personal renewal refused by the policy: the key is sent again')).toHaveLength(1);
    expect(await t.db`select 1 from license_keys where license_id = ${id}`).toHaveLength(1);
  });

  test('portal: the member row is linked to the requesting user; no invite link in the mail', async () => {
    const addr = email('portal');
    const user = await createUser(t.db, { email: addr });
    const r = await run({ email: addr, source: 'portal', requestedBy: user.id }, { actor: { kind: 'user', userId: user.id, label: addr }, config: AUTO() });
    expect(r.outcome).toBe('issued');
    const lic = await licenseOf(r.licenseId!);
    expect(lic.source).toBe('portal');
    const member = (await t.db<{ user_id: string | null; linked_at: Date | null }[]>`select user_id, linked_at from customer_members where customer_id = ${lic.customer_id}`)[0]!;
    expect(member.user_id).toBe(user.id);
    expect(r.mailer.sent[0]!.text).not.toContain('/invites/');
    expect(r.mailer.sent[0]!.text).toContain('/my/licenses');
  });

  test('never an organization: a member of an organization gets a person customer of their own', async () => {
    const addr = email('org');
    const org = await createCustomer(t.db, { kind: 'organization', members: [addr] });
    const r = await run({ email: addr }, { config: AUTO() });
    expect(r.outcome).toBe('issued');
    const lic = await licenseOf(r.licenseId!);
    expect(lic.customer_id).not.toBe(org.id);
    expect((await t.db<{ kind: string }[]>`select kind from customers where id = ${lic.customer_id}`)[0]!.kind).toBe('person');
  });

  async function openRequest(addr: string) {
    return (await t.db<{ id: string; review_reason: string; status: string; source: string }[]>`select id, review_reason, status, source from license_requests where requester_email = ${addr} and status = 'open'`)[0];
  }

  test('pending_review (blocked): a blocked address always waits, even with automatic approval; the admin mail has no approve link', async () => {
    const addr = email('blocked');
    await t.db`insert into blocked_emails (email, reason) values (${addr}, 'test')`;
    const r = await run({ email: addr }, { config: AUTO() });
    expect(r.outcome).toBe('pending_review');
    expect(await openRequest(addr)).toMatchObject({ id: r.requestId, review_reason: 'blocked' });
    expect(r.mailer.sent.map((m) => m.to).sort()).toEqual(['admin@example.org', addr].sort());
    const admin = r.mailer.to('admin@example.org')[0]!;
    expect(admin.text).toContain('The address is on the blocklist.');
    expect(admin.text).toContain('http://localhost:8790/requests');
    expect(admin.text).not.toContain('/approve?token=');
    expect(await t.db`select 1 from request_approval_links where request_id = ${r.requestId!}`).toHaveLength(0);
    const waiting = r.mailer.to(addr)[0]!;
    expect(waiting.subject).toContain('waiting for approval');
    // With automatic approval only this request waits: the mail does not claim every key does.
    expect(waiting.text).toContain('An administrator needs to approve this request before a key is sent');
    expect(waiting.text).not.toContain('approves every key');
  });

  test('pending_review (withdrawn_history): a withdrawn license in the person customer; no approve link', async () => {
    const addr = email('withdrawn');
    const first = await run({ email: addr }, { config: AUTO() });
    await t.db`update licenses set status = 'withdrawn', withdrawn_at = now() where id = ${first.licenseId!}`;
    const r = await run({ email: addr });
    expect(r.outcome).toBe('pending_review');
    expect((await openRequest(addr))?.review_reason).toBe('withdrawn_history');
    expect(approveTokens(r.mailer.sent.map((m) => m.text))).toEqual([]);
    expect(r.mailer.to('admin@example.org')[0]!.text).toContain('A license of this person was withdrawn before.');
  });

  test('approval required (the default): nothing is signed; the requester and each approver are mailed, each approver with a link of their own', async () => {
    const addr = email('approval');
    const config = testConfig({ mail: { adminNotify: ['first-admin@example.org', 'second-admin@example.org'] } });
    const now = new Date();
    const r = await run({ email: addr, name: 'Waiting Wendy', use: 'Pi-hole for the family' }, { config, now });
    expect(r.outcome).toBe('pending_review');
    expect(r.licenseId).toBeUndefined();
    expect((await openRequest(addr))?.review_reason).toBe('auto_approve_off');
    expect(await t.db`select 1 from customer_members where email = ${addr}`).toHaveLength(0);

    const waiting = r.mailer.to(addr);
    expect(waiting).toHaveLength(1);
    expect(waiting[0]!.subject).toBe('Your request for a kudurru personal key is waiting for approval');
    expect(waiting[0]!.text).toContain('arrives by email once it is approved');
    expect(waiting[0]!.text).toContain('An administrator approves every key before it is sent');

    const links = await t.db<{ token_hash: Buffer; sent_to: string; expires_at: Date; used_at: Date | null }[]>`
      select token_hash, sent_to, expires_at, used_at from request_approval_links where request_id = ${r.requestId!} order by sent_to`;
    expect(links.map((l) => l.sent_to)).toEqual(['first-admin@example.org', 'second-admin@example.org']);
    for (const l of links) {
      expect(l.used_at).toBeNull();
      expect(l.expires_at.getTime()).toBe(now.getTime() + 7 * 86_400_000);
    }
    for (const [i, to] of ['first-admin@example.org', 'second-admin@example.org'].entries()) {
      const mails = r.mailer.to(to);
      expect(mails).toHaveLength(1);
      const mail = mails[0]!;
      expect(mail.subject).toBe('Approve a free personal key for Waiting Wendy?');
      for (const part of [r.requestId!, 'Waiting Wendy', addr, 'Pi-hole for the family', 'the kudurru.io form', ATTESTATION_STATEMENT, '192.0.2.1', "Every personal key needs an admin's approval", 'Personal', ALL_FEATURES_LABEL, `Expires:    ${addDays(todayUtc(now), 365)} (UTC) if approved today`, 'http://localhost:8790/requests']) {
        expect(mail.text).toContain(part);
      }
      expect(mail.text).toContain('Nodes:      3');
      expect(mail.text).not.toContain('Renews:');
      const tokens = approveTokens([mail.text]);
      expect(tokens).toHaveLength(1);
      expect(mail.text).toContain(`http://localhost:8790/approve?token=${tokens[0]}`);
      // Only the SHA-256 is stored, and it is this recipient's own link.
      expect(links[i]!.token_hash.equals(new Bun.CryptoHasher('sha256').update(tokens[0]!).digest())).toBe(true);
    }
    expect(JSON.stringify(await t.db`select * from request_approval_links`)).not.toContain(approveTokens(r.mailer.sent.map((m) => m.text))[0]!);
  });

  test('a second request while one waits is the same request, and mails nobody', async () => {
    const addr = email('noauto');
    const r = await run({ email: addr });
    expect(r.outcome).toBe('pending_review');
    expect(r.mailer.sent).toHaveLength(2);
    const again = await run({ email: addr });
    expect(again).toMatchObject({ outcome: 'pending_review', requestId: r.requestId });
    expect(again.mailer.sent).toHaveLength(0);
    expect(await t.db`select 1 from request_approval_links where request_id = ${r.requestId!}`).toHaveLength(1);
  });

  test('approval required: a renewal in the window waits too (the mail names the license and the new expiry); a resend stays automatic', async () => {
    const addr = email('renewwait');
    const first = await run({ email: addr, name: 'Renewing Rae' }, { config: AUTO() });
    const id = first.licenseId!;
    const expiresOn = expiresOnOf((await licenseOf(id)).expires_at);

    const early = await run({ email: addr });
    expect(early).toMatchObject({ outcome: 'resent', licenseId: id });
    expect(early.mailer.to(addr)[0]!.subject).toBe('Your kudurru license key, sent again');
    expect(await t.db`select 1 from request_approval_links l join license_requests r on r.id = l.request_id where r.requester_email = ${addr}`).toHaveLength(0);

    const now = new Date(`${addDays(expiresOn, -10)}T12:00:00Z`);
    const r = await run({ email: addr, name: 'Renewing Rae' }, { now });
    expect(r.outcome).toBe('pending_review');
    expect(await t.db`select 1 from license_keys where license_id = ${id}`).toHaveLength(1);
    expect(expiresOnOf((await licenseOf(id)).expires_at)).toBe(expiresOn);
    const admin = r.mailer.to('admin@example.org')[0]!;
    expect(admin.subject).toBe(`Approve the renewal of ${id} for Renewing Rae?`);
    expect(admin.text).toContain(`Renews:     ${id} (expires ${expiresOn} UTC)`);
    // The renewal runs from the current expiry whenever it is approved by then.
    expect(admin.text).toContain(`Expires:    ${addDays(expiresOn, 365)} (UTC) if approved by ${expiresOn}`);
    expect(approveTokens([admin.text])).toHaveLength(1);
    expect(r.mailer.to(addr)[0]!.subject).toBe('Your renewal of a kudurru personal key is waiting for approval');
  });

  test('approvers fall back to the enabled license admins and superadmins with a verified email; nobody to tell is a warning', async () => {
    const config = testConfig({ mail: { adminNotify: [] } });
    const lonely = await run({ email: email('nobody') }, { config });
    expect(lonely.outcome).toBe('pending_review');
    expect(lonely.log.find('request waiting for review, nobody notified')).toHaveLength(1);
    expect(lonely.mailer.sent).toHaveLength(1);

    const admin = await createUser(t.db, { roles: ['license_admin'], email: `la-${process.pid}@admins.example` });
    const superadmin = await createUser(t.db, { roles: ['customer'], idpRoles: ['superadmin'], email: `sa-${process.pid}@admins.example` });
    await createUser(t.db, { roles: ['license_admin'], email: `off-${process.pid}@admins.example`, disabled: true });
    await createUser(t.db, { roles: ['license_admin'], email: null });
    // A granted role counts however long ago the user signed in; a role
    // from the IdP only within 30 days of the last sign-in (idp_roles change
    // only at a sign-in: someone removed from the group at the IdP).
    await t.db`update users set last_login_at = now() - interval '400 days' where id = ${admin.id}`;
    const stale = await createUser(t.db, { roles: ['customer'], idpRoles: ['license_admin'], email: `stale-${process.pid}@admins.example` });
    await t.db`update users set last_login_at = now() - interval '31 days' where id = ${stale.id}`;
    const never = await createUser(t.db, { roles: ['customer'], idpRoles: ['superadmin'], email: `never-${process.pid}@admins.example` });
    await t.db`update users set last_login_at = null where id = ${never.id}`;
    const addr = email('fallback');
    const r = await run({ email: addr }, { config });
    expect(r.mailer.sent.map((m) => m.to).sort()).toEqual([addr, admin.email!, superadmin.email!].sort());
    expect(approveTokens(r.mailer.sent.map((m) => m.text))).toHaveLength(2);
    expect(r.log.find('request waiting for review, nobody notified')).toHaveLength(0);
  });

  test('without mail the request still waits: no links, a warning', async () => {
    const r = await run({ email: email('nomail') }, { mailer: new MemoryMailer(false) });
    expect(r.outcome).toBe('pending_review');
    expect(await t.db`select 1 from request_approval_links where request_id = ${r.requestId!}`).toHaveLength(0);
    expect(r.log.find('request waiting for review, nobody notified')).toHaveLength(1);
  });

  test('pending_review (daily_cap): beyond PERSONAL_AUTO_APPROVE_MAX_PER_DAY automatic keys today', async () => {
    const midnight = new Date(`${todayUtc()}T00:00:00Z`);
    const today = Number((await t.db<{ n: string }[]>`
      select count(*) as n from licenses l where l.plan = 'personal' and l.source in ('web', 'portal') and l.created_at >= ${midnight}
        and not exists (select 1 from license_requests r where r.resulting_license_id = l.id)`)[0]!.n);
    const config = testConfig({ personal: { autoApprove: true, autoApproveMaxPerDay: today + 1 } });
    expect((await run({ email: email('cap') }, { config })).outcome).toBe('issued');
    const capped = email('cap');
    const r = await run({ email: capped }, { config });
    expect(r.outcome).toBe('pending_review');
    expect((await openRequest(capped))?.review_reason).toBe('daily_cap');
    // The daily cap only waits for a yes: the admin mail has an approve link.
    expect(approveTokens(r.mailer.to('admin@example.org').map((m) => m.text))).toHaveLength(1);
    expect((await run({ email: email('cap') }, { config: testConfig({ personal: { autoApprove: true, autoApproveMaxPerDay: 0 } }) })).outcome).toBe('pending_review');
  });

  test('no signer: 503 signing_unavailable and nothing stored', async () => {
    const addr = email('nosigner');
    let err: unknown;
    try {
      await run({ email: addr }, { signing: signingUnavailable('no signing key is configured'), config: AUTO() });
    } catch (e) {
      err = e;
    }
    expect(err).toBeInstanceOf(HttpError);
    expect((err as HttpError).key).toBe('signing_unavailable');
    expect(await t.db`select 1 from customer_members where email = ${addr}`).toHaveLength(0);
  });

  test('a mail that fails after the commit is logged; the key stays', async () => {
    const mailer = new MemoryMailer();
    mailer.failNext();
    const log = new MemoryLogger();
    const r = await run({ email: email('mailfail') }, { mailer, log, config: AUTO() });
    expect(r.outcome).toBe('issued');
    expect(await licenseOf(r.licenseId!)).toBeDefined();
    expect(log.find('after-commit work failed')).toHaveLength(1);
  });
});

describeDb('portal routes', () => {
  let app: TestApp;
  let auto: TestApp;
  beforeAll(() => {
    app = makeTestApp({ handlers: mergeHandlers(meHandlers), db: t.db, signing });
    auto = makeTestApp({ handlers: mergeHandlers(meHandlers), db: t.db, signing, config: AUTO() });
  });
  afterAll(async () => {
    await app?.close();
    await auto?.close();
  });

  test('POST /me/requests needs an IdP-verified email', async () => {
    const user = await createUser(t.db, { email: null });
    const res = await callJson(app, 'myRequestCreate', { as: await signInAs(t.db, user), body: { name: 'No Mail', use: 'homelab', noncommercial: true } });
    expect(res.status).toBe(400);
    expect(res.error?.key).toBe('email_unverified');
  });

  test('POST /me/requests: noncommercial must be exactly true', async () => {
    const user = await createUser(t.db);
    const s = await signInAs(t.db, user);
    for (const noncommercial of [false, 'true', undefined]) {
      const res = await callJson(app, 'myRequestCreate', { as: s, body: { name: 'X', use: 'homelab', noncommercial } });
      expect(res.status).toBe(400);
      expect(res.error?.fields).toEqual(['noncommercial']);
    }
  });

  test('POST /me/requests with approval required (the default): pending_review, the request is the user\'s own', async () => {
    const addr = email('portalwait');
    const user = await createUser(t.db, { email: addr });
    const s = await signInAs(t.db, user);
    const res = await callJson(app, 'myRequestCreate', { as: s, body: { name: 'Portal Pat', use: 'homelab', noncommercial: true } });
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ outcome: 'pending_review', requestId: res.body.requestId! });
    const mine = await callJson(app, 'myRequests', { as: s });
    expect(mine.body.items.map((r) => r.id)).toEqual([res.body.requestId!]);
    expect(app.mailer.to('admin@example.org').at(-1)!.text).toContain('From:       the kudurru license portal');
  });

  test('automatic approval: a portal request issues a key the person then sees; renew only in the window, with a fresh certification', async () => {
    const addr = email('route');
    const user = await createUser(t.db, { email: addr });
    const s = await signInAs(t.db, user);
    const res = await callJson(auto, 'myRequestCreate', { as: s, body: { name: 'Route Person', use: 'homelab', noncommercial: true } });
    expect(res.status).toBe(200);
    expect(res.body.outcome).toBe('issued' satisfies PersonalOutcome);
    const id = res.body.licenseId!;
    const mine = await callJson(auto, 'myLicenses', { as: s });
    expect(mine.body.items.map((l) => l.id)).toContain(id);
    expect(mine.body.items.find((l) => l.id === id)!.renewal).toBeNull();
    const early = await callJson(auto, 'myLicenseRenew', { as: s, params: { id }, body: { noncommercial: true } });
    expect(early.status).toBe(409);
    expect(early.error?.key).toBe('conflict');

    // Ten days before expiry the renewal is offered and works.
    const lic = await licenseOf(id);
    const later = new Date(`${addDays(expiresOnOf(lic.expires_at), -10)}T08:00:00Z`);
    const timeApp = makeTestApp({ handlers: mergeHandlers(meHandlers), db: t.db, signing, clock: () => later, config: AUTO() });
    try {
      const s2 = await signInAs(t.db, user, testConfig(), later);
      const offered = (await callJson(timeApp, 'myLicenses', { as: s2 })).body.items.find((l) => l.id === id)!;
      expect(offered.state).toBe('expiring');
      expect(offered.renewal).toEqual({ expiresOn: addDays(expiresOnOf(lic.expires_at), 365) });
      const renewed = await callJson(timeApp, 'myLicenseRenew', { as: s2, params: { id }, body: { noncommercial: true } });
      expect(renewed.status).toBe(200);
      expect(renewed.body).toEqual({ outcome: 'renewed', licenseId: id });
      expect(expiresOnOf((await licenseOf(id)).expires_at)).toBe(addDays(expiresOnOf(lic.expires_at), 365));
      expect(timeApp.mailer.to(addr)[0]?.subject).toBe('Your renewed kudurru license key');
      expect(timeApp.mailer.to(addr)[0]?.text).toContain(await currentToken(id));
      const event = (await t.db<{ details: Record<string, unknown> }[]>`select details from audit_events where action = 'license.renewed' and target_id = ${id}`)[0]!;
      expect(event.details).toMatchObject({ via: 'portal', attestedAt: later.toISOString() });
    } finally {
      await timeApp.close();
    }
  });

  test('POST /me/licenses/:id/renew needs {"noncommercial": true} and a verified email', async () => {
    const user = await createUser(t.db);
    const s = await signInAs(t.db, user);
    const params = { id: 'lic_AAAAAAAAAAAA' };
    for (const body of [undefined, {}, { noncommercial: false }, { noncommercial: 'true' }]) {
      const res = await callJson(app, 'myLicenseRenew', { as: s, params, body });
      expect(res.status).toBe(400);
      expect(res.error?.fields).toEqual(['noncommercial']);
    }
    const noMail = await signInAs(t.db, await createUser(t.db, { email: null }));
    const res = await callJson(app, 'myLicenseRenew', { as: noMail, params, body: { noncommercial: true } });
    expect(res.status).toBe(400);
    expect(res.error?.key).toBe('email_unverified');
    expect((await callJson(app, 'myLicenseRenew', { as: s, params, body: { noncommercial: true } })).status).toBe(404);
  });

  test('approval required: the portal\'s Renew opens a request (name = licensee, use "Renewal of <id>", a fresh certification); nothing is signed', async () => {
    const addr = email('renewreq');
    const user = await createUser(t.db, { email: addr });
    const s = await signInAs(t.db, user);
    const issued = await callJson(auto, 'myRequestCreate', { as: s, body: { name: 'Licensee Lou', use: 'homelab', noncommercial: true } });
    const id = issued.body.licenseId!;
    const soon = new Date(`${addDays(todayUtc(), 10)}T23:59:59Z`);
    await t.db`update licenses set expires_at = ${soon} where id = ${id}`;
    await t.db`update license_keys set expires_at = ${soon} where license_id = ${id}`;
    app.mailer.clear();

    const res = await callJson(app, 'myLicenseRenew', { as: s, params: { id }, body: { noncommercial: true } });
    expect(res.status).toBe(200);
    expect(res.body.outcome).toBe('pending_review');
    const request = (await t.db<{ id: string; name: string; use_description: string; source: string; requested_by: string; review_reason: string; attestation: Attestation }[]>`
      select * from license_requests where id = ${res.body.requestId!}`)[0]!;
    expect(request).toMatchObject({ name: 'Licensee Lou', use_description: `Renewal of ${id}`, source: 'portal', requested_by: user.id, review_reason: 'auto_approve_off' });
    expect(request.attestation).toMatchObject({ source: 'portal', statement: ATTESTATION_STATEMENT, email: addr, license: 'LicenseRef-Kudurru-Community' });
    expect(await t.db`select 1 from license_keys where license_id = ${id}`).toHaveLength(1);
    expect(app.mailer.to(addr)[0]!.subject).toBe('Your renewal of a kudurru personal key is waiting for approval');
    const admin = app.mailer.to('admin@example.org')[0]!;
    expect(admin.subject).toBe(`Approve the renewal of ${id} for Licensee Lou?`);
    expect(approveTokens([admin.text])).toHaveLength(1);
    // Asking again while it waits is the same request.
    const again = await callJson(app, 'myLicenseRenew', { as: s, params: { id }, body: { noncommercial: true } });
    expect(again.body).toEqual({ outcome: 'pending_review', requestId: res.body.requestId! });
  });

  test('approval required: a license an approval would not renew (not the person customer\'s newest personal key) cannot be asked for here', async () => {
    const addr = email('notnewest');
    const user = await createUser(t.db, { email: addr });
    const s = await signInAs(t.db, user);
    const org = await createCustomer(t.db, { kind: 'organization', members: [{ email: addr, userId: user.id }] });
    const soon = new Date(`${addDays(todayUtc(), 10)}T23:59:59Z`);
    const id = `lic_org${org.id.slice(4, 13)}`;
    await t.db`insert into licenses (id, customer_id, licensee, plan, nodes, expires_at, source, created_by_label) values (${id}, ${org.id}, 'Org Personal', 'personal', 3, ${soon}, 'admin', 'test')`;
    await t.db`
      insert into license_keys (id, license_id, kid, token, claims, issued_at, expires_at, issued_by_label, reason)
      values (${'key_' + id.slice(4)}, ${id}, 'server-test-1', 'org.key.token', ${{}}, now(), ${soon}, 'test', 'issue')`;
    const res = await callJson(app, 'myLicenseRenew', { as: s, params: { id }, body: { noncommercial: true } });
    expect(res.status).toBe(409);
    expect(res.error?.key).toBe('conflict');
    expect(await t.db`select 1 from license_requests where requester_email = ${addr}`).toHaveLength(0);
  });
});
