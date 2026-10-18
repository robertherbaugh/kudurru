// The admin license lifecycle through the real router (design spec §6
// "Admin license actions", §10 "domain"): issue (policy, new customer,
// quota, no signer), renew, change and move, withdraw (final, cancels open
// requests, blocks addresses, keepsWorkingUntil), email key, downloads,
// the list filters, and the invariants of §4 after every step: one current
// key, its claims equal claimsFor(row) except iat and kid, expires_at = exp,
// tokens never in audit details or logs, one log line per signature.
import { afterAll, beforeAll, expect, test } from 'bun:test';
import type { LicenseDetail } from '../../shared/api';
import { addDays, expFor, graceEndsOn, todayUtc } from '../../shared/dates';
import { customerHandlers } from '../api/customers';
import { licenseHandlers } from '../api/licenses';
import { mergeHandlers } from '../http/router';
import { claimsFor } from '../licensing/claims';
import { verify } from '../licensing/jws';
import { loadSigning } from '../licensing/keyring';
import { signingUnavailable, type SigningState } from '../licensing/types';
import { containsToken, silentLogger } from '../log';
import { MemoryMailer } from '../mail/memory';
import { smtpMailer } from '../mail/smtp';
import { termsOf, type LicenseRow } from '../services/licenses';
import { call, callJson, makeTestApp, type TestApp } from './call';
import { describeDb, hasDb, useTestDb as fileDatabase } from './db';
import { createCustomer, createUser, signInAs, testConfig, type TestSession } from './fixtures';

const handlers = mergeHandlers(licenseHandlers, customerHandlers);
// One fresh database for this file (useTestDb, renamed: it is not a React hook).
const t = fileDatabase();
let app: TestApp;
let signing: SigningState;
let admin: TestSession;
let adminId: string;

const today = todayUtc();
const inDays = (n: number): string => addDays(today, n);

beforeAll(async () => {
  if (!hasDb) return;
  signing = await loadSigning(testConfig(), silentLogger);
  app = makeTestApp({ handlers, db: t.db, signing });
  const user = await createUser(t.db, { roles: ['license_admin'], name: 'Ada Admin', email: 'ada@example.org' });
  adminId = user.id;
  admin = await signInAs(t.db, user);
});
afterAll(async () => {
  await app?.close();
});

/** The §4 invariants for one license. */
async function assertInvariants(id: string): Promise<void> {
  const row = (await t.db<LicenseRow[]>`select * from licenses where id = ${id}`)[0]!;
  const current = await t.db<{ id: string; token: string; kid: string; expires_at: Date; claims: Record<string, unknown> }[]>`
    select id, token, kid, expires_at, claims from license_keys where license_id = ${id} and superseded_at is null`;
  expect(current).toHaveLength(1);
  const key = current[0]!;
  expect(key.expires_at.getTime()).toBe(row.expires_at.getTime());
  const verified = await verify(key.token, signing.keyring);
  const { iat, ...claims } = verified.claims;
  const { iat: _iat, ...expected } = claimsFor(id, termsOf(row), new Date(iat * 1000));
  expect(claims).toEqual(expected);
  expect(verified.kid).toBe(key.kid);
  expect(verified.claims.nbf).toBeUndefined();
}

async function auditOf(id: string): Promise<{ action: string; details: Record<string, unknown> }[]> {
  return t.db<{ action: string; details: Record<string, unknown> }[]>`select action, details from audit_events where target_id = ${id} order by id`;
}

async function issue(body: Record<string, unknown>, as: TestSession = admin, on: TestApp = app) {
  return callJson(on, 'licenseIssue', { as, body });
}

describeDb('issue', () => {
  test('a business license for a new customer: detail, key, audit, one log line per signature', async () => {
    app.log.clear();
    const res = await issue({
      newCustomer: { name: 'Example GmbH', kind: 'organization', contactEmail: 'Ops@Example.org' },
      licensee: '  Example GmbH ',
      plan: 'business',
      nodes: 25,
      features: ['oidc', 'edge', 'audit'],
      expiresOn: inDays(365),
      agreementRef: 'AGR-1',
      notes: 'signed 2026',
    });
    expect(res.status).toBe(200);
    const d = res.body;
    expect(d.id).toMatch(/^lic_[A-Za-z0-9_-]{12}$/);
    expect(d).toMatchObject({ licensee: 'Example GmbH', plan: 'business', nodes: 25, features: ['oidc', 'edge', 'audit'], expiresOn: inDays(365), state: 'active', source: 'admin', agreementRef: 'AGR-1', notes: 'signed 2026', withdrawn: null, attestation: null });
    expect(d.customer.kind).toBe('organization');
    expect(d.keepsWorkingUntil).toBe(graceEndsOn(inDays(365)));
    expect(d.keys).toHaveLength(1);
    expect(d.keys[0]).toMatchObject({ reason: 'issue', current: true, issuedBy: { id: adminId, label: 'Ada Admin (ada@example.org)' } });
    expect(d.currentKey.kid).toBe('server-test-1');
    const v = await verify(d.currentKey.token, signing.keyring);
    expect(v.claims.exp).toBe(expFor(inDays(365)));
    await assertInvariants(d.id);
    const customer = (await t.db<{ contact_email: string }[]>`select contact_email from customers where id = ${d.customer.id}`)[0]!;
    expect(customer.contact_email).toBe('ops@example.org');

    const signed = app.log.find('license key signed');
    expect(signed).toHaveLength(1);
    expect(signed[0]).toMatchObject({ jti: d.id, keyId: d.currentKey.id, kid: 'server-test-1', reason: 'issue', plan: 'business', nodes: 25, actor: 'Ada Admin (ada@example.org)' });
    expect(app.log.text()).not.toContain(d.currentKey.token);
    const events = await auditOf(d.id);
    expect(events.map((e) => e.action)).toEqual(['license.issued']);
    expect(JSON.stringify(events)).not.toContain(d.currentKey.token.slice(-40));
  });

  test('a personal key issued by an admin records the admin attestation', async () => {
    const c = await createCustomer(t.db, { kind: 'person' });
    const res = await issue({ customerId: c.id, licensee: 'Hank', plan: 'personal', nodes: 3, features: null, expiresOn: inDays(365) });
    expect(res.status).toBe(200);
    expect(res.body.attestation).toMatchObject({ source: 'admin', by: adminId });
    expect(res.body.features).toBeNull();
    await assertInvariants(res.body.id);
  });

  const policy: [string, Record<string, unknown>, string[]][] = [
    ['personal with 5 nodes', { plan: 'personal', nodes: 5, features: null, expiresOn: inDays(365) }, ['nodes']],
    ['personal with a feature list', { plan: 'personal', nodes: 3, features: ['oidc'], expiresOn: inDays(365) }, ['features']],
    ['personal longer than 395 days', { plan: 'personal', nodes: 3, features: null, expiresOn: inDays(396) }, ['expiresOn']],
    ['trial longer than 60 days', { plan: 'trial', nodes: 3, features: null, expiresOn: inDays(61) }, ['expiresOn']],
    ['expiring today', { plan: 'business', nodes: 3, features: null, expiresOn: today }, ['expiresOn']],
    ['more than ten years', { plan: 'business', nodes: 3, features: null, expiresOn: inDays(3654) }, ['expiresOn']],
    ['features: [] (never issued)', { plan: 'business', nodes: 3, features: [], expiresOn: inDays(30) }, ['features']],
    ['an unknown feature', { plan: 'business', nodes: 3, features: ['teleport'], expiresOn: inDays(30) }, ['features']],
    ['duplicate features', { plan: 'business', nodes: 3, features: ['oidc', 'oidc'], expiresOn: inDays(30) }, ['features']],
    ['an unknown plan', { plan: 'pro', nodes: 3, features: null, expiresOn: inDays(30) }, ['plan']],
    ['0 nodes', { plan: 'business', nodes: 0, features: null, expiresOn: inDays(30) }, ['nodes']],
    ['a blank licensee (Go whitespace)', { licensee: ' \u00a0\u3000', plan: 'business', nodes: 3, features: null, expiresOn: inDays(30) }, ['licensee']],
    ['a licensee with a control character', { licensee: 'A\u0007B', plan: 'business', nodes: 3, features: null, expiresOn: inDays(30) }, ['licensee']],
    ['features missing (must be null or a list)', { plan: 'business', nodes: 3, expiresOn: inDays(30) }, ['features']],
  ];
  for (const [name, body, fields] of policy) {
    test(`refuses ${name}`, async () => {
      const c = await createCustomer(t.db);
      const before = (await t.db<{ n: string }[]>`select count(*) as n from licenses`)[0]!.n;
      const res = await issue({ customerId: c.id, licensee: 'Someone', ...body });
      expect(res.status).toBe(400);
      expect(res.error?.key).toBe('invalid');
      expect(res.error?.fields).toEqual(fields);
      expect((await t.db<{ n: string }[]>`select count(*) as n from licenses`)[0]!.n).toBe(before);
    });
  }

  test('a trial of exactly 60 days and a key of exactly ten years pass', async () => {
    const c = await createCustomer(t.db);
    expect((await issue({ customerId: c.id, licensee: 'T', plan: 'trial', nodes: 1, features: null, expiresOn: inDays(60) })).status).toBe(200);
    expect((await issue({ customerId: c.id, licensee: 'T', plan: 'enterprise', nodes: 100000, features: null, expiresOn: inDays(3653) })).status).toBe(200);
  });

  test('customerId and newCustomer together, or neither, are refused; an unknown customer is 404', async () => {
    const c = await createCustomer(t.db);
    const base = { licensee: 'X', plan: 'business', nodes: 1, features: null, expiresOn: inDays(30) };
    expect((await issue({ ...base, customerId: c.id, newCustomer: { name: 'N', kind: 'person' } })).status).toBe(400);
    expect((await issue(base)).status).toBe(400);
    expect((await issue({ ...base, customerId: 'cus_doesnotexist' })).status).toBe(404);
  });

  test('emailCustomer mails the key after the commit, with the attachment', async () => {
    app.mailer.clear();
    const res = await issue({ newCustomer: { name: 'Mail Co', kind: 'organization', contactEmail: 'keys@mail.example' }, licensee: 'Mail Co', plan: 'business', nodes: 2, features: null, expiresOn: inDays(90), emailCustomer: true });
    expect(res.status).toBe(200);
    const mail = app.mailer.to('keys@mail.example');
    expect(mail).toHaveLength(1);
    expect(mail[0]!.attachments?.[0]).toMatchObject({ filename: 'license.key', content: res.body.currentKey.token + '\n', contentType: 'text/plain; charset=us-ascii' });
    expect(mail[0]!.text).toContain(res.body.currentKey.token);
    expect(mail[0]!.text).toContain('A license key does not give you a license.');
    expect(mail[0]!.text).not.toContain('community-development use only');
  });

  test('emailCustomer without a contact email is refused before anything is signed', async () => {
    const c = await createCustomer(t.db, { contactEmail: null });
    const res = await issue({ customerId: c.id, licensee: 'X', plan: 'business', nodes: 1, features: null, expiresOn: inDays(30), emailCustomer: true });
    expect(res.status).toBe(400);
    expect(res.error?.fields).toEqual(['emailCustomer']);
  });

  test('no signer: 503 signing_unavailable and nothing stored', async () => {
    const other = makeTestApp({ handlers, db: t.db, signing: signingUnavailable('no signing key is configured') });
    try {
      const res = await issue({ newCustomer: { name: 'Nobody Ltd', kind: 'organization' }, licensee: 'X', plan: 'business', nodes: 1, features: null, expiresOn: inDays(30) }, admin, other);
      expect(res.status).toBe(503);
      expect(res.error).toMatchObject({ key: 'signing_unavailable', hint: 'no signing key is configured' });
      expect(await t.db`select 1 from customers where name = 'Nobody Ltd'`).toHaveLength(0);
    } finally {
      await other.close();
    }
  });

  test('ISSUE_MAX_PER_ACTOR_PER_DAY: 429 issuing_quota', async () => {
    const quota = makeTestApp({ handlers, db: t.db, signing, config: testConfig({ issuing: { maxPerActorPerDay: 2 } }) });
    try {
      const u = await createUser(t.db, { roles: ['license_admin'] });
      const s = await signInAs(t.db, u);
      const c = await createCustomer(t.db);
      const body = { customerId: c.id, licensee: 'Q', plan: 'business', nodes: 1, features: null, expiresOn: inDays(30) };
      expect((await issue(body, s, quota)).status).toBe(200);
      expect((await issue(body, s, quota)).status).toBe(200);
      const third = await issue(body, s, quota);
      expect(third.status).toBe(429);
      expect(third.error?.key).toBe('issuing_quota');
    } finally {
      await quota.close();
    }
  });
});

describeDb('renew, change, move', () => {
  let lic: LicenseDetail;
  beforeAll(async () => {
    const c = await createCustomer(t.db, { contactEmail: 'owner@renew.example' });
    lic = (await issue({ customerId: c.id, licensee: 'Renew Ltd', plan: 'business', nodes: 5, features: null, expiresOn: inDays(100) })).body;
  });

  test('renew: later than the current expiry only; the old key is superseded by the new one', async () => {
    expect((await callJson(app, 'licenseRenew', { as: admin, params: { id: lic.id }, body: { expiresOn: inDays(100) } })).status).toBe(400);
    const res = await callJson(app, 'licenseRenew', { as: admin, params: { id: lic.id }, body: { expiresOn: inDays(465) } });
    expect(res.status).toBe(200);
    expect(res.body.expiresOn).toBe(inDays(465));
    expect(res.body.keys).toHaveLength(2);
    const [newest, old] = res.body.keys;
    expect(newest).toMatchObject({ reason: 'renew', current: true, supersededBy: null });
    expect(old).toMatchObject({ reason: 'issue', current: false, supersededBy: newest!.id });
    expect(res.body.currentKey.id).toBe(newest!.id);
    expect(res.body.keepsWorkingUntil).toBe(graceEndsOn(inDays(465)));
    await assertInvariants(lic.id);
  });

  test('two renews at once leave exactly one current key', async () => {
    const [a, b] = await Promise.all([
      call(app, 'licenseRenew', { as: admin, params: { id: lic.id }, body: { expiresOn: inDays(500) } }),
      call(app, 'licenseRenew', { as: admin, params: { id: lic.id }, body: { expiresOn: inDays(510) } }),
    ]);
    expect([a.status, b.status]).toContain(200);
    await assertInvariants(lic.id);
  });

  test('change: a new key with the changed claims, audited; nothing to change is 400', async () => {
    expect((await callJson(app, 'licenseChange', { as: admin, params: { id: lic.id }, body: {} })).status).toBe(400);
    const res = await callJson(app, 'licenseChange', { as: admin, params: { id: lic.id }, body: { nodes: 10, features: ['rbac', 'oidc'], licensee: 'Renew Holding Ltd' } });
    expect(res.status).toBe(200);
    expect(res.body).toMatchObject({ nodes: 10, features: ['rbac', 'oidc'], licensee: 'Renew Holding Ltd' });
    expect(res.body.keys[0]!.reason).toBe('change');
    await assertInvariants(lic.id);
    const changed = (await auditOf(lic.id)).find((e) => e.action === 'license.changed')!;
    expect(changed.details.changes).toMatchObject({ nodes: { from: 5, to: 10 }, features: { from: null, to: ['rbac', 'oidc'] } });
  });

  test('change re-runs the policy on the result', async () => {
    const res = await callJson(app, 'licenseChange', { as: admin, params: { id: lic.id }, body: { plan: 'personal' } });
    expect(res.status).toBe(400);
    expect(res.error?.fields?.sort()).toEqual(['expiresOn', 'features', 'nodes']);
  });

  test('a move to another customer is audited as license.moved and needs no new key', async () => {
    const target = await createCustomer(t.db);
    const before = (await callJson(app, 'licenseGet', { as: admin, params: { id: lic.id } })).body.keys.length;
    const res = await callJson(app, 'licenseChange', { as: admin, params: { id: lic.id }, body: { customerId: target.id } });
    expect(res.status).toBe(200);
    expect(res.body.customer.id).toBe(target.id);
    expect(res.body.keys).toHaveLength(before);
    expect((await auditOf(lic.id)).some((e) => e.action === 'license.moved' && e.details.to === target.id)).toBe(true);
  });
});

describeDb('withdraw', () => {
  test('final: hides nothing from admins, refuses renew/change/email, cancels open requests, blocks addresses', async () => {
    const c = await createCustomer(t.db, { kind: 'person', members: ['wd@example.org', 'wd2@example.org'], contactEmail: 'wd@example.org' });
    const lic = (await issue({ customerId: c.id, licensee: 'Withdrawn Person', plan: 'personal', nodes: 3, features: null, expiresOn: inDays(200) })).body;
    await t.db`
      insert into license_requests (id, source, requester_email, name, use_description, attestation, review_reason)
      values ('req_wdopen00001', 'web', 'wd2@example.org', 'W', 'homelab', ${{ source: 'kudurru.io form', at: new Date().toISOString() }}, 'auto_approve_off')`;
    const res = await callJson(app, 'licenseWithdraw', { as: admin, params: { id: lic.id }, body: { reason: 'chargeback', customerMessage: 'Please contact us.', blockPersonal: true } });
    expect(res.status).toBe(200);
    expect(res.body.state).toBe('withdrawn');
    expect(res.body.withdrawn).toMatchObject({ reason: 'chargeback', customerMessage: 'Please contact us.', keepsWorkingUntil: graceEndsOn(inDays(200)), by: { id: adminId } });
    const req = (await t.db<{ status: string; decision_note: string }[]>`select status, decision_note from license_requests where id = 'req_wdopen00001'`)[0]!;
    expect(req).toEqual({ status: 'canceled', decision_note: 'license withdrawn' });
    const blocked = await t.db<{ email: string }[]>`select email from blocked_emails where email like 'wd%@example.org' order by email`;
    expect(blocked.map((b) => b.email)).toEqual(['wd2@example.org', 'wd@example.org']);

    for (const [route, body] of [
      ['licenseRenew', { expiresOn: inDays(300) }],
      ['licenseChange', { nodes: 2 }],
      ['licenseWithdraw', { reason: 'again' }],
      ['licenseEmail', {}],
    ] as const) {
      const r = await callJson(app, route, { as: admin, params: { id: lic.id }, body });
      expect(r.status).toBe(409);
      expect(r.error?.key).toBe('license_withdrawn');
    }
    // The admin still downloads its keys (history).
    const dl = await call(app, 'licenseKeyDownload', { as: admin, params: { id: lic.id, keyId: lic.currentKey.id } });
    expect(dl.status).toBe(200);
  });
});

describeDb('email key and downloads', () => {
  test('Cloudflare submissions need a provider receipt before HTTP success and an emailed audit', async () => {
    const customer = await createCustomer(t.db, { contactEmail: 'contact@receipt.example' });
    const lic = (await issue({ customerId: customer.id, licensee: 'Receipt Co', plan: 'business', nodes: 1, features: null, expiresOn: inDays(30) })).body;
    let response = '250 2.0.0 Ok';
    const mailer = smtpMailer(testConfig({ mail: { smtpUrl: 'smtps://api_token:test-password@smtp.mx.cloudflare.net:465' } }), app.log, () => ({
      sendMail: () => Promise.resolve({ response, messageId: '<local@kudurru.io>', accepted: ['contact@receipt.example'], rejected: [], pending: [], envelope: { from: 'licenses@kudurru.io', to: ['contact@receipt.example'] } }),
    }));
    const smtpApp = makeTestApp({ handlers, db: t.db, signing, mailer, log: app.log });
    try {
      const failed = await callJson(smtpApp, 'licenseEmail', { as: admin, params: { id: lic.id }, body: {} });
      expect(failed.status).toBe(503);
      expect(failed.error?.key).toBe('unavailable');
      expect((await auditOf(lic.id)).filter((e) => e.action === 'license.key_emailed')).toHaveLength(0);

      response = '250 2.0.0 Ok <provider@kudurru.io>';
      const sent = await callJson(smtpApp, 'licenseEmail', { as: admin, params: { id: lic.id }, body: {} });
      expect(sent.status).toBe(200);
      expect(sent.body.sentTo).toBe('contact@receipt.example');
      expect((await auditOf(lic.id)).filter((e) => e.action === 'license.key_emailed')).toHaveLength(1);
      expect(app.log.find('mail accepted by SMTP').at(-1)).toMatchObject({ licenseId: lic.id, keyId: lic.currentKey.id, requestId: sent.res.headers.get('X-Request-Id') });
    } finally {
      await smtpApp.close();
    }
  });

  test('email key: default the contact email; 400 without one; 503 when the mail fails, with no audit row', async () => {
    const c = await createCustomer(t.db, { contactEmail: 'contact@email.example' });
    const lic = (await issue({ customerId: c.id, licensee: 'Email Co', plan: 'business', nodes: 1, features: null, expiresOn: inDays(30) })).body;
    app.mailer.clear();
    const ok = await callJson(app, 'licenseEmail', { as: admin, params: { id: lic.id }, body: {} });
    expect(ok.status).toBe(200);
    expect(ok.body).toEqual({ sentTo: 'contact@email.example' });
    expect(app.mailer.last()?.attachments?.[0]?.content).toBe(lic.currentKey.token + '\n');
    const other = await callJson(app, 'licenseEmail', { as: admin, params: { id: lic.id }, body: { to: 'Other@Email.example' } });
    expect(other.body.sentTo).toBe('other@email.example');
    // A POST without a body uses the default too.
    expect((await call(app, 'licenseEmail', { as: admin, params: { id: lic.id } })).status).toBe(200);

    const audits = async (): Promise<number> => (await auditOf(lic.id)).filter((e) => e.action === 'license.key_emailed').length;
    const before = await audits();
    app.mailer.failNext();
    const failed = await callJson(app, 'licenseEmail', { as: admin, params: { id: lic.id }, body: {} });
    expect(failed.status).toBe(503);
    expect(failed.error?.key).toBe('unavailable');
    expect(await audits()).toBe(before);

    const none = await createCustomer(t.db, { contactEmail: null });
    const lic2 = (await issue({ customerId: none.id, licensee: 'No Mail', plan: 'business', nodes: 1, features: null, expiresOn: inDays(30) })).body;
    const missing = await callJson(app, 'licenseEmail', { as: admin, params: { id: lic2.id }, body: {} });
    expect(missing.status).toBe(400);
    expect(missing.error?.fields).toEqual(['to']);
  });

  test('email key without SMTP: 503 unavailable', async () => {
    const noMail = makeTestApp({ handlers, db: t.db, signing, mailer: new MemoryMailer(false) });
    try {
      const c = await createCustomer(t.db, { contactEmail: 'x@nomail.example' });
      const lic = (await issue({ customerId: c.id, licensee: 'N', plan: 'business', nodes: 1, features: null, expiresOn: inDays(30) }, admin, noMail)).body;
      const res = await callJson(noMail, 'licenseEmail', { as: admin, params: { id: lic.id }, body: {} });
      expect(res.status).toBe(503);
    } finally {
      await noMail.close();
    }
  });

  test('download: license.key, ASCII, trailing newline, attachment, audited; a key of another license is 404', async () => {
    const c = await createCustomer(t.db);
    const lic = (await issue({ customerId: c.id, licensee: 'Download Co', plan: 'business', nodes: 1, features: null, expiresOn: inDays(30) })).body;
    const res = await call(app, 'licenseKeyDownload', { as: admin, params: { id: lic.id, keyId: lic.currentKey.id } });
    expect(res.status).toBe(200);
    expect(res.headers.get('content-type')).toBe('text/plain; charset=us-ascii');
    expect(res.headers.get('content-disposition')).toBe('attachment; filename="license.key"');
    expect(res.headers.get('cache-control')).toBe('no-store');
    const bytes = new Uint8Array(await res.arrayBuffer());
    expect(bytes[0]).not.toBe(0xef);
    expect(new TextDecoder().decode(bytes)).toBe(lic.currentKey.token + '\n');
    expect((await auditOf(lic.id)).some((e) => e.action === 'license.key_downloaded')).toBe(true);
    const other = (await issue({ customerId: c.id, licensee: 'Other', plan: 'business', nodes: 1, features: null, expiresOn: inDays(30) })).body;
    expect((await call(app, 'licenseKeyDownload', { as: admin, params: { id: lic.id, keyId: other.currentKey.id } })).status).toBe(404);
  });
});

describeDb('list', () => {
  test('filters: q, plan, customerId, state, expiringWithinDays; no tokens in the list', async () => {
    const c = await createCustomer(t.db, { name: 'Filter Customer Zeta' });
    const soon = (await issue({ customerId: c.id, licensee: 'Soon Expiring', plan: 'trial', nodes: 1, features: null, expiresOn: inDays(10) })).body;
    const later = (await issue({ customerId: c.id, licensee: 'Far Future', plan: 'enterprise', nodes: 1, features: null, expiresOn: inDays(1000) })).body;
    await t.db`update licenses set expires_at = ${new Date(Date.now() - 5 * 86_400_000)} where id = ${later.id}`;
    const list = async (query: Record<string, string | number>) => (await callJson(app, 'licensesList', { as: admin, query })).body;

    const byCustomer = await list({ customerId: c.id });
    expect(byCustomer.total).toBe(2);
    expect(JSON.stringify(byCustomer)).not.toContain('eyJ');
    expect((await list({ q: 'zeta' })).items.map((i) => i.id).sort()).toEqual([soon.id, later.id].sort());
    expect((await list({ customerId: c.id, plan: 'trial' })).items.map((i) => i.id)).toEqual([soon.id]);
    expect((await list({ customerId: c.id, state: 'expiring' })).items.map((i) => i.id)).toEqual([soon.id]);
    expect((await list({ customerId: c.id, state: 'grace' })).items.map((i) => i.id)).toEqual([later.id]);
    expect((await list({ customerId: c.id, expiringWithinDays: 30 })).items.map((i) => i.id)).toEqual([soon.id]);
    expect((await list({ customerId: c.id, limit: 1 })).items).toHaveLength(1);
    expect((await callJson(app, 'licensesList', { as: admin, query: { state: 'bogus' } })).status).toBe(400);
  });

  test('the audit log of a full lifecycle holds no token', async () => {
    const rows = await t.db<{ details: unknown }[]>`select details from audit_events`;
    expect(rows.length).toBeGreaterThan(5);
    for (const r of rows) expect(containsToken(JSON.stringify(r.details))).toBe(false);
  });
});
