// Business license requests (README, "Business requests"): the kudurru.io
// form's POST /api/v1/public/business-requests (202 for every valid input;
// 400 invalid with fields; the honeypot; the per-IP limit with Retry-After;
// the silent per-email limit; stored without mail), the "we have your
// request" mail and the approvers' mail, the audit event without the
// unconfirmed address, the CORS preflight, and the Requests page's list,
// close and issue (POST /licenses with businessRequestId) with their
// permissions.
import { afterAll, beforeAll, beforeEach, expect, test } from 'bun:test';
import { addDays, todayUtc } from '../../shared/dates';
import { businessRequestHandlers } from '../api/businessRequests';
import { licenseHandlers } from '../api/licenses';
import { createPipeline } from '../http/pipeline';
import { mergeHandlers } from '../http/router';
import { loadSigning } from '../licensing/keyring';
import type { SigningState } from '../licensing/types';
import { silentLogger } from '../log';
import { MemoryMailer } from '../mail/memory';
import { call, callJson, makeTestApp, type TestApp } from './call';
import { describeDb, hasDb, useTestDb as fileDatabase } from './db';
import { createUser, signInAs, testConfig, type TestSession } from './fixtures';

const handlers = mergeHandlers(businessRequestHandlers, licenseHandlers);
// One fresh database for this file (useTestDb, renamed: it is not a React hook).
const t = fileDatabase();
let signing: SigningState;
let app: TestApp;
let admin: TestSession;
let customer: TestSession;

beforeAll(async () => {
  if (!hasDb) return;
  signing = await loadSigning(testConfig(), silentLogger);
  app = makeTestApp({ handlers, db: t.db, signing });
  admin = await signInAs(t.db, await createUser(t.db, { roles: ['license_admin'], name: 'Lena Admin' }));
  customer = await signInAs(t.db, await createUser(t.db, { roles: ['customer'] }));
});
afterAll(async () => {
  await app?.close();
});

let n = 0;
const addr = (tag: string): string => `${tag}-${++n}-${process.pid}@business.example`;
const form = (email: string, extra: Record<string, unknown> = {}) => ({
  name: 'Ada Admin',
  email,
  company: 'Example GmbH',
  edition: 'enterprise',
  nodes: 12,
  details: 'OIDC and edge.\nBy Q3.',
  website: '',
  ...extra,
});

async function stored(email: string) {
  return t.db<{ id: string; name: string; company: string; edition: string; nodes: number; details: string; status: string; ip: string | null; user_agent: string | null }[]>`
    select id, name, company, edition, nodes, details, status, host(ip) as ip, user_agent from business_requests where email = ${email} order by created_at`;
}

describeDb('POST /api/v1/public/business-requests', () => {
  beforeEach(async () => {
    await t.db`delete from business_requests`;
    app.mailer.clear();
  });

  test('202 {"status":"sent"}: stored at once, the requester and the approvers mailed, audited without the address', async () => {
    const email = addr('ok');
    const res = await callJson(app, 'publicBusinessRequest', { body: form(email.toUpperCase(), { extra: 'ignored' }), headers: { 'User-Agent': 'Firefox' } });
    expect(res.status).toBe(202);
    expect(res.body).toEqual({ status: 'sent' });
    const rows = await stored(email);
    expect(rows).toEqual([
      { id: expect.stringMatching(/^brq_/) as string, name: 'Ada Admin', company: 'Example GmbH', edition: 'enterprise', nodes: 12, details: 'OIDC and edge.\nBy Q3.', status: 'open', ip: '127.0.0.1', user_agent: 'Firefox' },
    ]);

    const toRequester = app.mailer.to(email);
    expect(toRequester).toHaveLength(1);
    expect(toRequester[0]!.subject).toBe('We have your kudurru business license request');
    expect(toRequester[0]!.text).toContain('Edition:    Enterprise');
    expect(toRequester[0]!.text).toContain('Nodes:      12');
    // The address is unconfirmed: nothing the submitter wrote is repeated to it.
    for (const written of ['Ada Admin', 'Example GmbH', 'OIDC and edge.', email]) {
      expect(toRequester[0]!.text).not.toContain(written);
      expect(toRequester[0]!.html).not.toContain(written);
    }
    const toAdmin = app.mailer.to('admin@example.org');
    expect(toAdmin).toHaveLength(1);
    expect(toAdmin[0]!.subject).toBe('Business license request from Example GmbH');
    expect(toAdmin[0]!.text).toContain(`Request:    ${rows[0]!.id}`);
    expect(toAdmin[0]!.text).toContain('about the Enterprise edition for 12 nodes.');
    expect(toAdmin[0]!.text).toContain('OIDC and edge.\nBy Q3.');
    expect(toAdmin[0]!.text).toContain('http://localhost:8790/requests?tab=business');

    const event = (await t.db<{ actor_label: string; ip: string | null; details: unknown }[]>`
      select actor_label, ip, details from audit_events where action = 'business_request.received' and target_id = ${rows[0]!.id}`)[0]!;
    expect(event).toEqual({ actor_label: 'public form', ip: null, details: { edition: 'enterprise', nodes: 12 } });
  });

  test('details are optional; the approvers are the license admins when ADMIN_NOTIFY_EMAIL is unset', async () => {
    const fromAdmins = makeTestApp({ handlers, db: t.db, signing, config: testConfig({ mail: { adminNotify: [] } }) });
    try {
      const adminUser = await createUser(t.db, { roles: ['license_admin'], email: addr('approver') });
      const email = addr('nodetails');
      expect((await call(fromAdmins, 'publicBusinessRequest', { body: form(email, { details: undefined, edition: 'core', nodes: 1 }) })).status).toBe(202);
      expect((await stored(email))[0]).toMatchObject({ edition: 'core', nodes: 1, details: '' });
      const mail = fromAdmins.mailer.to(adminUser.email!);
      expect(mail).toHaveLength(1);
      expect(mail[0]!.text).toContain('about the Commercial Core edition for 1 node.');
      expect(mail[0]!.text).toContain('They gave no further details.');
    } finally {
      await fromAdmins.close();
    }
  });

  test.each([
    ['an unknown edition', { edition: 'gold' }, ['edition']],
    ['no nodes', { nodes: 0 }, ['nodes']],
    ['too many nodes', { nodes: 10_001 }, ['nodes']],
    ['nodes as text', { nodes: '5' }, ['nodes']],
    ['a blank company', { company: '   ' }, ['company']],
    ['a long company', { company: 'x'.repeat(161) }, ['company']],
    ['long details', { details: 'x'.repeat(1501) }, ['details']],
    ['a bad email', { email: 'not-an-address' }, ['email']],
  ])('400 invalid for %s', async (_label, extra, fields) => {
    const res = await callJson(app, 'publicBusinessRequest', { body: form(addr('bad'), extra) });
    expect(res.status).toBe(400);
    expect(res.error?.key).toBe('invalid');
    expect(res.error?.fields).toEqual(fields);
    expect(app.mailer.sent).toHaveLength(0);
  });

  test('400 invalid with every field for a body that is not an object, a wrong content type or bad JSON', async () => {
    const arr = await callJson(app, 'publicBusinessRequest', { body: ['name'] });
    expect(arr.status).toBe(400);
    expect(arr.error?.fields).toEqual(['name', 'email', 'company', 'edition', 'nodes', 'details']);
    const text = await call(app, 'publicBusinessRequest', { rawBody: JSON.stringify(form(addr('ct'))), headers: { 'Content-Type': 'text/plain' } });
    expect(text.status).toBe(400);
    const bad = await call(app, 'publicBusinessRequest', { rawBody: '{"name":', headers: { 'Content-Type': 'application/json' } });
    expect(bad.status).toBe(400);
  });

  test('the honeypot: 202, nothing stored or sent, a log line', async () => {
    app.log.clear();
    const email = addr('bot');
    expect((await call(app, 'publicBusinessRequest', { body: form(email, { website: 'http://spam.example' }) })).status).toBe(202);
    expect(await stored(email)).toHaveLength(0);
    expect(app.mailer.sent).toHaveLength(0);
    expect(app.log.find('public.business_honeypot')).toHaveLength(1);
  });

  test('per IP: 429 rate_limited with Retry-After after PUBLIC_LIMIT_PER_IP_PER_HOUR', async () => {
    for (let i = 0; i < 5; i++) expect((await call(app, 'publicBusinessRequest', { body: form(addr('ip')) })).status).toBe(202);
    const res = await callJson(app, 'publicBusinessRequest', { body: form(addr('ip')) });
    expect(res.status).toBe(429);
    expect(res.error?.key).toBe('rate_limited');
    expect(Number(res.res.headers.get('retry-after'))).toBeGreaterThan(3500);
  });

  test('per email: silent 202 beyond PUBLIC_LIMIT_PER_EMAIL_PER_DAY (nothing stored or sent, a log line)', async () => {
    const email = addr('mail');
    app.log.clear();
    const limited = makeTestApp({ handlers, db: t.db, signing, config: testConfig({ publicForm: { limitPerIpPerHour: 100 } }), log: app.log, mailer: app.mailer });
    try {
      for (let i = 0; i < 4; i++) expect((await call(limited, 'publicBusinessRequest', { body: form(email) })).status).toBe(202);
      expect(await stored(email)).toHaveLength(3);
      expect(app.mailer.to(email)).toHaveLength(3);
      expect(app.log.find('public.business_email_limited')).toHaveLength(1);
    } finally {
      await limited.close();
    }
  });

  test('without mail: still stored (the Requests page shows it), a warning', async () => {
    const noMail = makeTestApp({ handlers, db: t.db, signing, mailer: new MemoryMailer(false) });
    try {
      const email = addr('nomail');
      expect((await call(noMail, 'publicBusinessRequest', { body: form(email) })).status).toBe(202);
      expect(await stored(email)).toHaveLength(1);
      expect(noMail.log.find('business request stored, nobody mailed')).toHaveLength(1);
    } finally {
      await noMail.close();
    }
  });

  test('a failing mail does not lose the request', async () => {
    const email = addr('mailfail');
    app.mailer.failNext();
    expect((await call(app, 'publicBusinessRequest', { body: form(email) })).status).toBe(202);
    expect(await stored(email)).toHaveLength(1);
  });

  test('CORS as for the personal form: the preflight from kudurru.io, other origins refused', async () => {
    const config = testConfig();
    const real = makeTestApp({ handlers, db: t.db, signing, config, pipeline: createPipeline(config, silentLogger) });
    try {
      const res = await real.fetch('/api/v1/public/business-requests', {
        method: 'OPTIONS',
        headers: { Origin: 'https://kudurru.io', 'Access-Control-Request-Method': 'POST', 'Access-Control-Request-Headers': 'content-type' },
      });
      expect(res.status).toBe(204);
      expect(res.headers.get('access-control-allow-origin')).toBe('https://kudurru.io');
      expect(res.headers.get('access-control-allow-methods')).toBe('POST, OPTIONS');
      const ok = await call(real, 'publicBusinessRequest', { body: form(addr('cors')), headers: { Origin: 'https://kudurru.io' } });
      expect(ok.status).toBe(202);
      expect(ok.headers.get('access-control-allow-origin')).toBe('https://kudurru.io');
      const other = await call(real, 'publicBusinessRequest', { body: form(addr('cors')), headers: { Origin: 'https://evil.example' } });
      expect(other.status).toBe(403);
    } finally {
      await real.close();
    }
  });
});

describeDb('business requests on the Requests page', () => {
  beforeEach(async () => {
    await t.db`delete from business_requests`;
    app.mailer.clear();
  });

  async function submit(company: string): Promise<string> {
    const email = addr('page');
    expect((await call(app, 'publicBusinessRequest', { body: form(email, { company }) })).status).toBe(202);
    return (await stored(email))[0]!.id;
  }

  test('the list: open first, newest first, status filter; customers may not read it', async () => {
    const first = await submit('First Co');
    const second = await submit('Second Co');
    await callJson(app, 'businessRequestClose', { as: admin, params: { id: first }, body: { note: 'Not a fit.' } });
    const list = await callJson(app, 'businessRequestsList', { as: admin });
    expect(list.body.items.map((r) => [r.id, r.status])).toEqual([
      [second, 'open'],
      [first, 'closed'],
    ]);
    const closed = await callJson(app, 'businessRequestsList', { as: admin, query: { status: 'closed' } });
    expect(closed.body.items).toHaveLength(1);
    expect(closed.body.items[0]).toMatchObject({ id: first, company: 'First Co', decisionNote: 'Not a fit.', decidedBy: { label: expect.stringContaining('Lena Admin') as string } });
    const denied = await callJson(app, 'businessRequestsList', { as: customer });
    expect(denied.status).toBe(403);
    expect(denied.error?.hint).toEqual({ missing: 'requests.read' });
  });

  test('close: a note, audited, nothing mailed; a second close is 409', async () => {
    const id = await submit('Close Co');
    app.mailer.clear();
    const res = await callJson(app, 'businessRequestClose', { as: admin, params: { id }, body: { note: '  Replied by email.  ' } });
    expect(res.status).toBe(200);
    expect(res.body.request).toMatchObject({ status: 'closed', decisionNote: 'Replied by email.', decidedAt: expect.any(String) as string });
    expect(app.mailer.sent).toHaveLength(0);
    expect(await t.db`select 1 from audit_events where action = 'business_request.closed' and target_id = ${id}`).toHaveLength(1);
    expect((await callJson(app, 'businessRequestClose', { as: admin, params: { id }, body: { note: 'again' } })).status).toBe(409);
    expect((await callJson(app, 'businessRequestClose', { as: admin, params: { id }, body: { note: '' } })).status).toBe(400);
    expect((await callJson(app, 'businessRequestClose', { as: admin, params: { id: 'brq_nothere' }, body: { note: 'x' } })).status).toBe(404);
    expect((await callJson(app, 'businessRequestClose', { as: customer, params: { id }, body: { note: 'x' } })).status).toBe(403);
  });

  test('issue from a request: the license is made and the request becomes issued with it; once only', async () => {
    const id = await submit('Issue Co');
    const body = {
      newCustomer: { name: 'Issue Co', kind: 'organization' as const, contactEmail: 'ops@issue.example' },
      licensee: 'Issue Co',
      plan: 'enterprise' as const,
      nodes: 12,
      features: null,
      expiresOn: addDays(todayUtc(), 365),
      businessRequestId: id,
    };
    const res = await callJson(app, 'licenseIssue', { as: admin, body });
    expect(res.status).toBe(200);
    const list = await callJson(app, 'businessRequestsList', { as: admin, query: { status: 'issued' } });
    expect(list.body.items).toHaveLength(1);
    expect(list.body.items[0]).toMatchObject({ id, status: 'issued', resultingLicenseId: res.body.id });
    const audit = (await t.db<{ details: Record<string, unknown> }[]>`select details from audit_events where action = 'business_request.issued' and target_id = ${id}`)[0]!;
    expect(audit.details).toEqual({ licenseId: res.body.id });

    // The request is decided: issuing from it again is 409, and nothing is created.
    const licenses = async () => Number((await t.db<{ n: string }[]>`select count(*) as n from licenses`)[0]!.n);
    const before = await licenses();
    const again = await callJson(app, 'licenseIssue', { as: admin, body: { ...body, newCustomer: { name: 'Issue Co 2', kind: 'organization' as const } } });
    expect(again.status).toBe(409);
    expect(await licenses()).toBe(before);
    expect((await callJson(app, 'licenseIssue', { as: admin, body: { ...body, businessRequestId: 'not-an-id' } })).status).toBe(400);
  });
});
