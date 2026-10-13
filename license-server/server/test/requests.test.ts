// Requests that wait for review (design spec §6 "Requests (admin)", §10):
// the list (open first, with the person customer approval would use),
// approve (person customer created or reused, never an organization; the
// requester a member, linked from the portal or invited from the form; a
// new key, or a renewal when the person meanwhile holds a personal key;
// request closed with the resulting ids; key mailed), reject (note mailed)
// and decisions on closed requests (409).
import { afterAll, beforeAll, expect, test } from 'bun:test';
import type { Attestation } from '../../shared/api';
import { addDays, todayUtc } from '../../shared/dates';
import { requestHandlers } from '../api/requests';
import { mergeHandlers } from '../http/router';
import { loadSigning } from '../licensing/keyring';
import type { SigningState } from '../licensing/types';
import { silentLogger } from '../log';
import { MemoryMailer } from '../mail/memory';
import { requestPersonalKey } from '../services/personal';
import { inTransaction, publicActor } from '../services/types';
import { callJson, makeTestApp, type TestApp } from './call';
import { describeDb, hasDb, useTestDb as fileDatabase } from './db';
import { createCustomer, createUser, signInAs, testConfig, type TestSession } from './fixtures';

// One fresh database for this file (useTestDb, renamed: it is not a React hook).
const t = fileDatabase();
let app: TestApp;
let signing: SigningState;
let admin: TestSession;
let adminId: string;

beforeAll(async () => {
  if (!hasDb) return;
  signing = await loadSigning(testConfig(), silentLogger);
  app = makeTestApp({ handlers: mergeHandlers(requestHandlers), db: t.db, signing });
  const u = await createUser(t.db, { roles: ['license_admin'], name: 'Rita Reviewer', email: 'rita@example.org' });
  adminId = u.id;
  admin = await signInAs(t.db, u);
});
afterAll(async () => {
  await app?.close();
});

let n = 0;
const addr = (tag: string): string => `${tag}-${++n}-${process.pid}@requests.example`;

/** A request that waits for review (automatic approval off), as the form or the portal makes it. */
async function pending(email: string, opts: { source?: 'web' | 'portal'; requestedBy?: string; name?: string } = {}): Promise<string> {
  const attestation: Attestation = { source: opts.source === 'portal' ? 'portal' : 'kudurru.io form', at: new Date().toISOString(), email };
  const r = await inTransaction(
    { db: t.db, config: testConfig({ personal: { autoApprove: false } }), log: silentLogger, signing, mailer: new MemoryMailer() },
    publicActor(email, { ip: null, userAgent: null }),
    (s) => requestPersonalKey(s, { name: opts.name ?? 'Pending Person', email, use: 'homelab', attestation, source: opts.source ?? 'web', requestedBy: opts.requestedBy ?? null }),
  );
  expect(r.outcome).toBe('pending_review');
  return r.requestId!;
}

describeDb('requests', () => {
  test('the list: open first, with the person customer approval would use; status filter', async () => {
    const email = addr('list');
    const person = await createCustomer(t.db, { kind: 'person', members: [email] });
    await createCustomer(t.db, { kind: 'organization', members: [email] });
    const id = await pending(email);
    const all = await callJson(app, 'requestsList', { as: admin });
    expect(all.status).toBe(200);
    const item = all.body.items.find((r) => r.id === id)!;
    expect(item).toMatchObject({ status: 'open', reviewReason: 'auto_approve_off', requesterEmail: email, source: 'web', personCustomer: { id: person.id, kind: 'person' }, decidedBy: null });
    expect(all.body.items[0]!.status).toBe('open');
    const closed = await callJson(app, 'requestsList', { as: admin, query: { status: 'rejected' } });
    expect(closed.body.items.every((r) => r.status === 'rejected')).toBe(true);
  });

  test('approve a form request: a new person customer, the requester invited, a personal key mailed with the invite link', async () => {
    const email = addr('approve');
    const id = await pending(email, { name: 'Approved Person' });
    app.mailer.clear();
    const res = await callJson(app, 'requestApprove', { as: admin, params: { id }, body: {} });
    expect(res.status).toBe(200);
    expect(res.body.request).toMatchObject({ status: 'approved', decidedBy: { id: adminId, label: 'Rita Reviewer (rita@example.org)' }, resultingLicenseId: res.body.license.id, resultingKeyId: res.body.license.currentKey.id });
    expect(res.body.license).toMatchObject({ plan: 'personal', nodes: 3, features: null, licensee: 'Approved Person', source: 'web', expiresOn: addDays(todayUtc(), 365) });
    expect(res.body.license.customer.kind).toBe('person');
    expect(res.body.license.keys[0]!.issuedBy.id).toBe(adminId);
    const mail = app.mailer.to(email)[0]!;
    expect(mail.subject).toBe('Your kudurru license key');
    expect(mail.text).toContain('/invites/');
    const again = await callJson(app, 'requestApprove', { as: admin, params: { id }, body: {} });
    expect(again.status).toBe(409);
  });

  test('approve a portal request: the member row is linked to the requesting user; licensee and expiry overridable', async () => {
    const email = addr('portal');
    const user = await createUser(t.db, { email });
    const id = await pending(email, { source: 'portal', requestedBy: user.id });
    const res = await callJson(app, 'requestApprove', { as: admin, params: { id }, body: { licensee: 'Override Name', expiresOn: addDays(todayUtc(), 200) } });
    expect(res.status).toBe(200);
    expect(res.body.license).toMatchObject({ licensee: 'Override Name', expiresOn: addDays(todayUtc(), 200), source: 'portal' });
    const member = (await t.db<{ user_id: string | null }[]>`select user_id from customer_members where customer_id = ${res.body.license.customer.id} and email = ${email}`)[0]!;
    expect(member.user_id).toBe(user.id);
    expect(app.mailer.to(email).at(-1)!.text).not.toContain('/invites/');
  });

  test('approve onto an organization is refused; onto an existing person customer works; policy applies', async () => {
    const email = addr('target');
    const id = await pending(email);
    const org = await createCustomer(t.db, { kind: 'organization' });
    const refused = await callJson(app, 'requestApprove', { as: admin, params: { id }, body: { customerId: org.id } });
    expect(refused.status).toBe(400);
    expect(refused.error?.fields).toEqual(['customerId']);
    const tooLong = await callJson(app, 'requestApprove', { as: admin, params: { id }, body: { expiresOn: addDays(todayUtc(), 500) } });
    expect(tooLong.status).toBe(400);
    const person = await createCustomer(t.db, { kind: 'person' });
    const ok = await callJson(app, 'requestApprove', { as: admin, params: { id }, body: { customerId: person.id } });
    expect(ok.status).toBe(200);
    expect(ok.body.license.customer.id).toBe(person.id);
    expect(await t.db`select 1 from customer_members where customer_id = ${person.id} and email = ${email}`).toHaveLength(1);
  });

  test('approve when the person meanwhile holds an active personal key: a renewal', async () => {
    const email = addr('renew');
    const first = await pending(email);
    const issued = (await callJson(app, 'requestApprove', { as: admin, params: { id: first }, body: {} })).body.license;
    // A second request (e.g. after the address was blocked) waits again.
    await t.db`insert into blocked_emails (email, reason) values (${email}, 'test')`;
    const second = await pending(email);
    // The key runs for a year yet: renewing it now would break the personal term.
    const early = await callJson(app, 'requestApprove', { as: admin, params: { id: second }, body: {} });
    expect(early.status).toBe(409);
    // Twenty days before it expires the approval renews it.
    const soon = addDays(todayUtc(), 20);
    await t.db`update licenses set expires_at = ${new Date(`${soon}T23:59:59Z`)} where id = ${issued.id}`;
    await t.db`update license_keys set expires_at = ${new Date(`${soon}T23:59:59Z`)} where license_id = ${issued.id}`;
    const res = await callJson(app, 'requestApprove', { as: admin, params: { id: second }, body: {} });
    expect(res.status).toBe(200);
    expect(res.body.license.id).toBe(issued.id);
    expect(res.body.license.keys.map((k) => k.reason)).toEqual(['renew', 'issue']);
    expect(res.body.license.expiresOn).toBe(addDays(soon, 365));
    expect(app.mailer.to(email).at(-1)!.subject).toBe('Your renewed kudurru license key');
  });

  test('reject: closed with the note, which is mailed; deciding again is 409', async () => {
    const email = addr('reject');
    const id = await pending(email, { name: 'Rejected Person' });
    app.mailer.clear();
    const res = await callJson(app, 'requestReject', { as: admin, params: { id }, body: { note: 'This looks like a company use.' } });
    expect(res.status).toBe(200);
    expect(res.body.request).toMatchObject({ status: 'rejected', decisionNote: 'This looks like a company use.', resultingLicenseId: null });
    const mail = app.mailer.to(email)[0]!;
    expect(mail.text).toContain('This looks like a company use.');
    expect(mail.text).toContain('Hello Rejected Person,');
    expect((await callJson(app, 'requestReject', { as: admin, params: { id }, body: { note: 'again' } })).status).toBe(409);
    expect((await callJson(app, 'requestReject', { as: admin, params: { id }, body: { note: '  ' } })).status).toBe(400);
    expect((await callJson(app, 'requestApprove', { as: admin, params: { id: 'req_doesnotexist' }, body: {} })).status).toBe(404);
  });
});
