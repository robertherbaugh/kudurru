// The personal-key decision tree (design spec §6 "Personal keys", §10
// "personal flow outcomes"): issued, resent, renewed (in the window, in
// grace, expired) and pending_review for each review reason (blocked,
// withdrawn_history, auto_approve_off, daily_cap), one open request per
// address, person customers only (never an organization), portal requests
// linked to the user, web requests with an invite link in the key mail, no
// signer = nothing stored, a failed mail after the commit keeps the key;
// and the portal routes (verified email required, renew own key).
import { afterAll, beforeAll, expect, test } from 'bun:test';
import type { Attestation, PersonalOutcome } from '../../shared/api';
import { ATTESTATION_STATEMENT, KEY_IS_NOT_A_LICENSE } from '../../shared/catalog';
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
  return { source: 'kudurru.io form', license: 'PolyForm-Noncommercial-1.0.0', statement: ATTESTATION_STATEMENT, at: new Date().toISOString(), ip: '192.0.2.1', userAgent: 'test', email: addr };
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
    const r = await run({ email: addr, name: 'Zo\u00eb Example' });
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
    expect(mail.text).toContain('non-commercial use only');
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
    const first = await run({ email: addr });
    const again = await run({ email: addr, name: 'Another Name' });
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
      const first = await run({ email: addr, name: 'Original Name' });
      const lic = await licenseOf(first.licenseId!);
      const expiresOn = expiresOnOf(lic.expires_at);
      const now = new Date(`${addDays(expiresOn, offset)}T12:00:00Z`);
      const r = await run({ email: addr, name: 'Different Name' }, { now });
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
    const r = await run({ email: addr, source: 'portal', requestedBy: user.id }, { actor: { kind: 'user', userId: user.id, label: addr } });
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
    const r = await run({ email: addr });
    expect(r.outcome).toBe('issued');
    const lic = await licenseOf(r.licenseId!);
    expect(lic.customer_id).not.toBe(org.id);
    expect((await t.db<{ kind: string }[]>`select kind from customers where id = ${lic.customer_id}`)[0]!.kind).toBe('person');
  });

  async function openRequest(addr: string) {
    return (await t.db<{ id: string; review_reason: string; status: string; source: string }[]>`select id, review_reason, status, source from license_requests where requester_email = ${addr} and status = 'open'`)[0];
  }

  test('pending_review (blocked): a blocked address always waits; requester and admin are mailed', async () => {
    const addr = email('blocked');
    await t.db`insert into blocked_emails (email, reason) values (${addr}, 'test')`;
    const r = await run({ email: addr });
    expect(r.outcome).toBe('pending_review');
    expect(await openRequest(addr)).toMatchObject({ id: r.requestId, review_reason: 'blocked' });
    expect(r.mailer.sent.map((m) => m.to).sort()).toEqual(['admin@example.org', addr].sort());
    expect(r.mailer.to('admin@example.org')[0]!.text).toContain('The address is on the blocklist.');
    expect(r.mailer.to(addr)[0]!.subject).toContain('waiting for review');
  });

  test('pending_review (withdrawn_history): a withdrawn license in the person customer', async () => {
    const addr = email('withdrawn');
    const first = await run({ email: addr });
    await t.db`update licenses set status = 'withdrawn', withdrawn_at = now() where id = ${first.licenseId!}`;
    const r = await run({ email: addr });
    expect(r.outcome).toBe('pending_review');
    expect((await openRequest(addr))?.review_reason).toBe('withdrawn_history');
  });

  test('pending_review (auto_approve_off), and a second request while one waits is the same request', async () => {
    const addr = email('noauto');
    const config = testConfig({ personal: { autoApprove: false } });
    const r = await run({ email: addr }, { config });
    expect(r.outcome).toBe('pending_review');
    expect((await openRequest(addr))?.review_reason).toBe('auto_approve_off');
    expect(r.mailer.sent).toHaveLength(2);
    const again = await run({ email: addr }, { config });
    expect(again).toMatchObject({ outcome: 'pending_review', requestId: r.requestId });
    expect(again.mailer.sent).toHaveLength(0);
  });

  test('pending_review (daily_cap): beyond PERSONAL_AUTO_APPROVE_MAX_PER_DAY automatic keys today', async () => {
    const midnight = new Date(`${todayUtc()}T00:00:00Z`);
    const today = Number((await t.db<{ n: string }[]>`
      select count(*) as n from licenses l where l.plan = 'personal' and l.source in ('web', 'portal') and l.created_at >= ${midnight}
        and not exists (select 1 from license_requests r where r.resulting_license_id = l.id)`)[0]!.n);
    const config = testConfig({ personal: { autoApproveMaxPerDay: today + 1 } });
    expect((await run({ email: email('cap') }, { config })).outcome).toBe('issued');
    const capped = email('cap');
    expect((await run({ email: capped }, { config })).outcome).toBe('pending_review');
    expect((await openRequest(capped))?.review_reason).toBe('daily_cap');
    expect((await run({ email: email('cap') }, { config: testConfig({ personal: { autoApproveMaxPerDay: 0 } }) })).outcome).toBe('pending_review');
  });

  test('no signer: 503 signing_unavailable and nothing stored', async () => {
    const addr = email('nosigner');
    let err: unknown;
    try {
      await run({ email: addr }, { signing: signingUnavailable('no signing key is configured') });
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
    const r = await run({ email: email('mailfail') }, { mailer, log });
    expect(r.outcome).toBe('issued');
    expect(await licenseOf(r.licenseId!)).toBeDefined();
    expect(log.find('after-commit work failed')).toHaveLength(1);
  });
});

describeDb('portal routes', () => {
  let app: TestApp;
  beforeAll(() => {
    app = makeTestApp({ handlers: mergeHandlers(meHandlers), db: t.db, signing });
  });
  afterAll(async () => {
    await app?.close();
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

  test('a portal request issues a key the person then sees; renew only in the window', async () => {
    const addr = email('route');
    const user = await createUser(t.db, { email: addr });
    const s = await signInAs(t.db, user);
    const res = await callJson(app, 'myRequestCreate', { as: s, body: { name: 'Route Person', use: 'homelab', noncommercial: true } });
    expect(res.status).toBe(200);
    expect(res.body.outcome).toBe('issued' satisfies PersonalOutcome);
    const id = res.body.licenseId!;
    const mine = await callJson(app, 'myLicenses', { as: s });
    expect(mine.body.items.map((l) => l.id)).toContain(id);
    expect(mine.body.items.find((l) => l.id === id)!.renewal).toBeNull();
    const early = await callJson(app, 'myLicenseRenew', { as: s, params: { id } });
    expect(early.status).toBe(409);
    expect(early.error?.key).toBe('conflict');

    // Ten days before expiry the renewal is offered and works.
    const lic = await licenseOf(id);
    const later = new Date(`${addDays(expiresOnOf(lic.expires_at), -10)}T08:00:00Z`);
    const timeApp = makeTestApp({ handlers: mergeHandlers(meHandlers), db: t.db, signing, clock: () => later });
    try {
      const s2 = await signInAs(t.db, user, testConfig(), later);
      const offered = (await callJson(timeApp, 'myLicenses', { as: s2 })).body.items.find((l) => l.id === id)!;
      expect(offered.state).toBe('expiring');
      expect(offered.renewal).toEqual({ expiresOn: addDays(expiresOnOf(lic.expires_at), 365) });
      const renewed = await callJson(timeApp, 'myLicenseRenew', { as: s2, params: { id } });
      expect(renewed.status).toBe(200);
      expect(renewed.body.expiresOn).toBe(addDays(expiresOnOf(lic.expires_at), 365));
      expect(renewed.body.key?.token).toBe(await currentToken(id));
      expect(timeApp.mailer.to(addr)[0]?.subject).toBe('Your renewed kudurru license key');
    } finally {
      await timeApp.close();
    }
  });
});
