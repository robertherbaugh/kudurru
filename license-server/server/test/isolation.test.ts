// Customer isolation (design spec §9, §10 "customer isolation"): every
// self.* route sees only the customers the user is a linked member of
// (foreign ids in paths answer 404, as if they did not exist), invited but
// unlinked rows give no access, withdrawn licenses hide their key, and the
// customer DTOs carry exactly the documented fields (no notes, agreement
// references, withdraw reasons or actors). Back-office routes refuse
// customers (403 with the missing permission).
import { afterAll, beforeAll, expect, test } from 'bun:test';
import { addDays, todayUtc } from '../../shared/dates';
import { customerHandlers } from '../api/customers';
import { licenseHandlers } from '../api/licenses';
import { meHandlers } from '../api/me';
import { requestHandlers } from '../api/requests';
import { mergeHandlers } from '../http/router';
import { loadSigning } from '../licensing/keyring';
import { silentLogger } from '../log';
import { call, callJson, makeTestApp, type TestApp } from './call';
import { describeDb, hasDb, useTestDb as fileDatabase } from './db';
import { createCustomer, createUser, signInAs, testConfig, type TestSession } from './fixtures';

// One fresh database for this file (useTestDb, renamed: it is not a React hook).
const t = fileDatabase();
let app: TestApp;
let admin: TestSession;
let alice: TestSession;
let aliceId: string;
let bob: TestSession;
let licA = '';
let licB = '';
let licWithdrawn = '';

beforeAll(async () => {
  if (!hasDb) return;
  app = makeTestApp({ handlers: mergeHandlers(meHandlers, licenseHandlers, customerHandlers, requestHandlers), db: t.db, signing: await loadSigning(testConfig(), silentLogger) });
  admin = await signInAs(t.db, await createUser(t.db, { roles: ['license_admin'] }));
  const a = await createUser(t.db, { email: 'alice@iso.example' });
  aliceId = a.id;
  const b = await createUser(t.db, { email: 'bob@iso.example' });
  alice = await signInAs(t.db, a);
  bob = await signInAs(t.db, b);
  const custA = await createCustomer(t.db, { name: 'Alice Org', members: [{ email: a.email!, userId: a.id }, 'invited.only@iso.example'] });
  const custB = await createCustomer(t.db, { name: 'Bob Org', members: [{ email: b.email!, userId: b.id }, 'alice@iso.example'] });
  const issue = async (customerId: string, licensee: string) =>
    (await callJson(app, 'licenseIssue', { as: admin, body: { customerId, licensee, plan: 'business', nodes: 2, features: null, expiresOn: addDays(todayUtc(), 90), agreementRef: 'SECRET-AGR', notes: 'internal note' } })).body;
  licA = (await issue(custA.id, 'Alice Org')).id;
  licB = (await issue(custB.id, 'Bob Org')).id;
  const w = await issue(custA.id, 'Alice Withdrawn');
  licWithdrawn = w.id;
  await callJson(app, 'licenseWithdraw', { as: admin, params: { id: licWithdrawn }, body: { reason: 'internal reason text', customerMessage: 'Call us.' } });
});
afterAll(async () => {
  await app?.close();
});

describeDb('customer isolation', () => {
  test('the list holds only the licenses of linked memberships (an unlinked row with the same email gives nothing)', async () => {
    const mine = await callJson(app, 'myLicenses', { as: alice });
    expect(mine.body.items.map((l) => l.id).sort()).toEqual([licA, licWithdrawn].sort());
    const bobs = await callJson(app, 'myLicenses', { as: bob });
    expect(bobs.body.items.map((l) => l.id)).toEqual([licB]);
  });

  test('foreign ids in paths answer 404: detail, key download, renew', async () => {
    for (const [route, method] of [['myLicense', 'GET'], ['myLicenseKey', 'GET'], ['myLicenseRenew', 'POST']] as const) {
      // The renewal's body is valid (a fresh certification), so only the id decides.
      const res = await call(app, route, { as: alice, params: { id: licB }, method, ...(method === 'POST' ? { body: { noncommercial: true } } : {}) });
      expect(res.status).toBe(404);
    }
    expect((await call(app, 'myLicense', { as: alice, params: { id: 'lic_doesnotexist' } })).status).toBe(404);
  });

  test('customer DTOs carry exactly the documented fields', async () => {
    const list = (await callJson(app, 'myLicenses', { as: alice })).body.items;
    for (const item of list) {
      expect(Object.keys(item).sort()).toEqual(['customer', 'expiresOn', 'features', 'id', 'keepsWorkingUntil', 'licensee', 'nodes', 'plan', 'renewal', 'state'].sort());
      expect(Object.keys(item.customer).sort()).toEqual(['id', 'name']);
    }
    const detail = await call(app, 'myLicense', { as: alice, params: { id: licA } });
    const text = await detail.text();
    const body = JSON.parse(text) as Record<string, unknown>;
    expect(Object.keys(body).sort()).toEqual(
      ['attestation', 'customer', 'expiresOn', 'features', 'id', 'keepsWorkingUntil', 'key', 'licensee', 'nodes', 'people', 'plan', 'renewal', 'state', 'withdrawn', 'worksWithKudurru'].sort(),
    );
    for (const secret of ['SECRET-AGR', 'internal note', 'internal reason text', 'issuedBy', 'createdBy', 'agreementRef', 'notes']) expect(text).not.toContain(secret);
    expect(body.people).toEqual([
      { email: 'alice@iso.example', status: 'linked' },
      { email: 'invited.only@iso.example', status: 'invited' },
    ]);
  });

  test('a withdrawn license: listed as withdrawn with the customer message, no key, no download', async () => {
    const res = await callJson(app, 'myLicense', { as: alice, params: { id: licWithdrawn } });
    expect(res.body.state).toBe('withdrawn');
    expect(res.body.key).toBeNull();
    expect(res.body.withdrawn).toMatchObject({ customerMessage: 'Call us.' });
    expect(JSON.stringify(res.body)).not.toContain('internal reason text');
    const dl = await callJson(app, 'myLicenseKey', { as: alice, params: { id: licWithdrawn } });
    expect(dl.status).toBe(409);
    expect(dl.error?.key).toBe('license_withdrawn');
  });

  test('the own key downloads as license.key and is audited', async () => {
    const res = await call(app, 'myLicenseKey', { as: alice, params: { id: licA } });
    expect(res.status).toBe(200);
    expect(res.headers.get('content-disposition')).toBe('attachment; filename="license.key"');
    expect((await res.text()).endsWith('\n')).toBe(true);
    const audit = await t.db`select 1 from audit_events where action = 'license.key_downloaded' and target_id = ${licA} and actor_id = ${aliceId}`;
    expect(audit).toHaveLength(1);
  });

  test('own requests only', async () => {
    await t.db`
      insert into license_requests (id, source, requested_by, requester_email, name, use_description, attestation, review_reason)
      values ('req_isobob00001', 'portal', (select id from users where email = 'bob@iso.example'), 'bob@iso.example', 'Bob', 'lab', ${{ source: 'portal', at: new Date().toISOString() }}, 'auto_approve_off')`;
    expect((await callJson(app, 'myRequests', { as: alice })).body.items).toHaveLength(0);
    expect((await callJson(app, 'myRequests', { as: bob })).body.items.map((r) => r.id)).toEqual(['req_isobob00001']);
  });

  test('back-office routes refuse a customer with the missing permission', async () => {
    const cases = [
      ['licensesList', {}, 'licenses.read'],
      ['licenseGet', { id: licA }, 'licenses.read'],
      ['customersList', {}, 'customers.read'],
      ['customerGet', { id: 'cus_x' }, 'customers.read'],
      ['requestsList', {}, 'requests.read'],
    ] as const;
    for (const [route, params, missing] of cases) {
      const res = await callJson(app, route, { as: alice, params: params as never });
      expect(res.status).toBe(403);
      expect(res.error?.hint).toEqual({ missing });
    }
    expect((await call(app, 'licenseIssue', { as: alice, body: {} })).status).toBe(403);
  });
});
