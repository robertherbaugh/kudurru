// Regression tests for the findings of the stream B review (customer
// isolation, the address syntax, renewal and withdraw rules, the limits
// under parallel requests, the confirmation mail): each test reproduces
// the probe that found the problem and asserts the fixed behavior.
import { afterAll, beforeAll, describe, expect, test } from 'bun:test';
import type { Attestation } from '../../shared/api';
import { ATTESTATION_STATEMENT } from '../../shared/catalog';
import { addDays, todayUtc } from '../../shared/dates';
import { customerHandlers } from '../api/customers';
import { licenseHandlers } from '../api/licenses';
import { meHandlers } from '../api/me';
import { publicHandlers } from '../api/public';
import { isEmailSyntax } from '../http/body';
import { mergeHandlers } from '../http/router';
import { loadSigning } from '../licensing/keyring';
import type { SigningState } from '../licensing/types';
import { MemoryLogger, silentLogger } from '../log';
import { MemoryMailer } from '../mail/memory';
import { requestPersonalKey, type PersonalRequest } from '../services/personal';
import { inTransaction, publicActor } from '../services/types';
import { callJson, makeTestApp, type TestApp } from './call';
import { describeDb, hasDb, useTestDb as fileDatabase } from './db';
import { createUser, signInAs, testConfig } from './fixtures';

const t = fileDatabase();
let signing: SigningState;
let app: TestApp;

beforeAll(async () => {
  if (!hasDb) return;
  signing = await loadSigning(testConfig(), silentLogger);
  app = makeTestApp({ handlers: mergeHandlers(meHandlers, licenseHandlers, customerHandlers, publicHandlers), db: t.db, signing });
});
afterAll(async () => {
  await app?.close();
});

let n = 0;
const addr = (tag: string): string => `${tag}-${++n}-${process.pid}@fixes.example`;

function attestation(email: string): Attestation {
  return { source: 'kudurru.io form', license: 'PolyForm-Noncommercial-1.0.0', statement: ATTESTATION_STATEMENT, at: new Date().toISOString(), ip: '192.0.2.1', userAgent: 'test', email };
}

/** The form's confirmed request for `email`, as verifyPublicRequest runs it. */
async function webRequest(email: string, overrides: Partial<PersonalRequest> = {}) {
  const req: PersonalRequest = { name: 'Homelab Hank', email, use: 'my homelab', attestation: attestation(email), source: 'web', ...overrides };
  return inTransaction(
    { db: t.db, config: testConfig(), log: new MemoryLogger(), signing, mailer: new MemoryMailer(), now: new Date() },
    publicActor(email, { ip: '192.0.2.1', userAgent: 'test' }),
    (s) => requestPersonalKey(s, req),
  );
}

describe('isEmailSyntax: one plain deliverable address', () => {
  test.each(['hank@example.org', 'first.last+tag@sub.example.co.uk', "o'brien@example.ie", 'zoë@exämple.de', 'a_b-c@x-y.io'])('accepts %s', (s) => {
    expect(isEmailSyntax(s)).toBe(true);
  });
  test.each(['bad@x.com,z', 'bad@x.com;z', 'bad@x.com,z.com', 'a<bad@x.com>', '"q"@x.com', 'a b@x.com', 'a@x', 'a..b@x.com', '.a@x.com', 'a@-x.com', 'a@x.123', 'a@x.com.', '(c)a@x.com', 'a@[127.0.0.1]', 'a@b@x.com'])(
    'refuses %s',
    (s) => {
      expect(isEmailSyntax(s)).toBe(false);
    },
  );
});

describeDb('review fixes', () => {
  test('a portal request never links an existing member row to another account that claims the same verified email', async () => {
    const victim = addr('victim');
    const first = await webRequest(victim);
    expect(first.outcome).toBe('issued');
    // A second account at a lenient IdP asserts the same address as verified.
    const intruder = await createUser(t.db, { email: victim });
    const session = await signInAs(t.db, intruder);
    const before = await callJson(app, 'myLicenses', { as: session });
    expect(before.body.items).toHaveLength(0);
    const res = await callJson(app, 'myRequestCreate', { as: session, body: { name: 'Intruder', use: 'nothing', noncommercial: true } });
    expect(res.status).toBe(200);
    expect(res.body.outcome).toBe('resent');
    const after = await callJson(app, 'myLicenses', { as: session });
    expect(after.body.items).toHaveLength(0);
    const member = (await t.db<{ user_id: string | null; invite_token_hash: Buffer | null }[]>`
      select m.user_id, m.invite_token_hash from customer_members m join licenses l on l.customer_id = m.customer_id where l.id = ${first.licenseId!}`)[0]!;
    expect(member.user_id).toBeNull();
    // The key went to the address itself, with an invite link.
    expect(member.invite_token_hash).not.toBeNull();
  });

  test('a portal request from a new person links the row it creates', async () => {
    const email = addr('newperson');
    const user = await createUser(t.db, { email });
    const session = await signInAs(t.db, user);
    const res = await callJson(app, 'myRequestCreate', { as: session, body: { name: 'New Person', use: 'homelab', noncommercial: true } });
    expect(res.body.outcome).toBe('issued');
    expect((await callJson(app, 'myLicenses', { as: session })).body.items).toHaveLength(1);
  });

  test('the public form refuses an address that a mail library would deliver elsewhere', async () => {
    const res = await callJson(app, 'publicKeyRequest', { body: { name: 'Bad', email: 'a<bad@x.com>', use: 'x', noncommercial: true, website: '' } });
    expect(res.status).toBe(400);
    expect(res.error?.fields).toEqual(['email']);
  });

  test('the portal renewal follows the block list and withdrawn history, and offers no Renew button then', async () => {
    const email = addr('renewblock');
    const user = await createUser(t.db, { email });
    const session = await signInAs(t.db, user);
    const issued = await callJson(app, 'myRequestCreate', { as: session, body: { name: 'Renew Person', use: 'homelab', noncommercial: true } });
    const id = issued.body.licenseId!;
    // Move the key into the renew window.
    const soon = new Date(`${addDays(todayUtc(), 10)}T23:59:59Z`);
    await t.db`update licenses set expires_at = ${soon} where id = ${id}`;
    await t.db`update license_keys set expires_at = ${soon} where license_id = ${id}`;
    expect((await callJson(app, 'myLicense', { as: session, params: { id } })).body.renewal).not.toBeNull();
    await t.db`insert into blocked_emails (email, reason, blocked_by_label) values (${email}, 'test', 'test')`;
    expect((await callJson(app, 'myLicense', { as: session, params: { id } })).body.renewal).toBeNull();
    const res = await callJson(app, 'myLicenseRenew', { as: session, params: { id } });
    expect(res.status).toBe(409);
    expect(res.error?.key).toBe('conflict');
  });

  test('withdraw, then removing the member, does not reopen automatic issue', async () => {
    const email = addr('withdrawn');
    const first = await webRequest(email);
    const admin = await createUser(t.db, { roles: ['license_admin'] });
    const as = await signInAs(t.db, admin);
    expect((await callJson(app, 'licenseWithdraw', { as, params: { id: first.licenseId! }, body: { reason: 'commercial use' } })).status).toBe(200);
    const customerId = (await t.db<{ customer_id: string }[]>`select customer_id from licenses where id = ${first.licenseId!}`)[0]!.customer_id;
    expect((await callJson(app, 'memberRemove', { as, params: { id: customerId, email } })).status).toBe(204);
    const again = await webRequest(email);
    expect(again.outcome).toBe('pending_review');
  });

  test('the issuing quota holds under parallel requests', async () => {
    const quotaApp = makeTestApp({ handlers: mergeHandlers(licenseHandlers), db: t.db, signing, config: testConfig({ issuing: { maxPerActorPerDay: 2 } }) });
    try {
      const admin = await createUser(t.db, { roles: ['license_admin'] });
      const as = await signInAs(t.db, admin);
      const body = (i: number) => ({ newCustomer: { name: `Quota ${i}`, kind: 'organization' as const }, licensee: `Quota ${i}`, plan: 'business' as const, nodes: 5, features: null, expiresOn: addDays(todayUtc(), 365) });
      const statuses = await Promise.all([0, 1, 2, 3, 4, 5].map((i) => callJson(quotaApp, 'licenseIssue', { as, body: body(i) }).then((r) => r.status)));
      expect(statuses.filter((s) => s === 200)).toHaveLength(2);
      expect(statuses.filter((s) => s === 429)).toHaveLength(4);
    } finally {
      await quotaApp.close();
    }
  });

  test('the confirmation mail does not repeat the name typed into the anonymous form', async () => {
    app.mailer.clear();
    const email = addr('greeting');
    const res = await callJson(app, 'publicKeyRequest', { body: { name: 'Visit https://phish.example now', email, use: 'x', noncommercial: true, website: '' } });
    expect(res.status).toBe(202);
    const mail = app.mailer.to(email)[0]!;
    expect(mail.text).not.toContain('phish');
    expect(mail.text.startsWith('Hello,')).toBe(true);
  });
});
