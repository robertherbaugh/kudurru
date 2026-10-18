// Order Forms (README, "Order Forms"): drafting (from a business request,
// the licensor address of the last form, the rules), changing a draft,
// sending (the gaps, the frozen and hashed text, the authorized contacts
// made members and mailed, with an invite link when they have no access),
// the customer's view (sent forms of member customers only, never a draft),
// accepting (authorized contacts only, the hash they read, the evidence
// kept, the approvers mailed), declining and revising (a new version; an
// answer to the old text is refused), countersigning (executed, the signed
// copy mailed), issuing one license from an executed form (its customer
// only; the business request becomes issued) and voiding, each audited.
import { afterAll, beforeAll, beforeEach, describe, expect, test } from 'bun:test';
import { createHash } from 'node:crypto';
import type { OrderForm, OrderFormTerms } from '../../shared/api';
import { addDays, todayUtc } from '../../shared/dates';
import { defaultOrderFormTerms, orderFormDocument } from '../../shared/orderForms';
import { handlers } from '../api';
import { loadSigning } from '../licensing/keyring';
import type { SigningState } from '../licensing/types';
import { silentLogger } from '../log';
import { MemoryMailer } from '../mail/memory';
import { call, callJson, makeTestApp, type TestApp } from './call';
import { describeDb, hasDb, useTestDb as fileDatabase } from './db';
import { createCustomer, createUser, signInAs, testConfig, type TestSession, type TestUser } from './fixtures';

// One fresh database for this file (useTestDb, renamed: it is not a React hook).
const t = fileDatabase();
let signing: SigningState;
let app: TestApp;
let admin: TestSession;
let adminUser: TestUser;
let stranger: TestSession;

beforeAll(async () => {
  if (!hasDb) return;
  signing = await loadSigning(testConfig(), silentLogger);
  app = makeTestApp({ handlers, db: t.db, signing });
  adminUser = await createUser(t.db, { roles: ['license_admin'], name: 'Lena Admin' });
  admin = await signInAs(t.db, adminUser);
  stranger = await signInAs(t.db, await createUser(t.db, { roles: ['customer'] }));
});
afterAll(async () => {
  await app?.close();
});

let n = 0;
const addr = (tag: string): string => `${tag}-${++n}-${process.pid}@order.example`;
const today = todayUtc();

/** Terms with no gaps: everything sending needs. */
function complete(contact: string, extra: Partial<OrderFormTerms> = {}): OrderFormTerms {
  return {
    ...defaultOrderFormTerms({ edition: 'enterprise', nodes: 5, customerLegalName: 'Example GmbH', contact, today }),
    licensorContact: 'Robert Herbaugh\n1 Main Street, Springfield\nhello@kudurru.io',
    customerAddress: 'Hauptstrasse 1\n10115 Berlin, Germany',
    governingLaw: 'Delaware, USA; courts of Wilmington',
    ...extra,
  };
}

/** A customer with one linked member (who can sign in) and a draft for it. */
async function setup(extra: Partial<OrderFormTerms> = {}): Promise<{ form: OrderForm; contact: TestSession; email: string; customerId: string }> {
  const email = addr('contact');
  const user = await createUser(t.db, { roles: ['customer'], email, name: 'Carla Contact' });
  const c = await createCustomer(t.db, { name: 'Example GmbH', members: [{ email, userId: user.id }] });
  const res = await callJson(app, 'orderFormCreate', { as: admin, body: { customerId: c.id, terms: complete(email, extra) } });
  expect(res.status).toBe(200);
  return { form: res.body, contact: await signInAs(t.db, user), email, customerId: c.id };
}

async function sent(extra: Partial<OrderFormTerms> = {}) {
  const s = await setup(extra);
  const res = await callJson(app, 'orderFormSend', { as: admin, params: { id: s.form.id } });
  expect(res.status).toBe(200);
  return { ...s, form: res.body };
}

const accept = (as: TestSession, form: Pick<OrderForm, 'id' | 'documentSha256'>, extra: Record<string, unknown> = {}) =>
  callJson(app, 'myOrderFormAccept', { as, params: { id: form.id }, body: { name: 'Carla Contact', title: 'CTO', documentSha256: form.documentSha256, authorized: true, ...extra } });

async function auditOf(id: string): Promise<{ action: string; details: Record<string, unknown> }[]> {
  return t.db<{ action: string; details: Record<string, unknown> }[]>`select action, details from audit_events where target_type = 'order_form' and target_id = ${id} order by id`;
}

beforeEach(() => {
  app?.mailer.clear();
});

describeDb('drafting', () => {
  test('from a business request, for a new organization: numbered, open, with the gaps; the licensor address of the last form', async () => {
    const email = addr('biz');
    expect((await call(app, 'publicBusinessRequest', { body: { name: 'Ada', email, company: 'Draft Co', edition: 'core', nodes: 3, website: '' } })).status).toBe(202);
    const requestId = (await t.db<{ id: string }[]>`select id from business_requests where email = ${email}`)[0]!.id;
    // A form with a licensor address first, so the new one can take it.
    await setup();
    const terms = { ...defaultOrderFormTerms({ edition: 'core', nodes: 3, customerLegalName: 'Draft Co', contact: email, today }) };
    const res = await callJson(app, 'orderFormCreate', { as: admin, body: { newCustomer: { name: 'Draft Co', kind: 'organization', contactEmail: email }, businessRequestId: requestId, terms } });
    expect(res.status).toBe(200);
    const f = res.body;
    expect(f.number).toMatch(new RegExp(`^OF-${new Date().getUTCFullYear()}-\\d{4,}$`));
    expect(f).toMatchObject({ status: 'draft', version: 1, documentSha256: null, businessRequestId: requestId, edition: 'core', nodes: 3, annualFeeCents: 9_900 + 3 * 9_900 });
    expect(f.customer).toMatchObject({ name: 'Draft Co', kind: 'organization' });
    expect(f.terms.licensorContact).toContain('1 Main Street');
    expect(f.gaps).toEqual(['terms.customerAddress', 'terms.governingLaw']);
    // The request shows the form, and stays open until a license answers it.
    const list = await callJson(app, 'businessRequestsList', { as: admin, query: { status: 'open' } });
    expect(list.body.items.find((r) => r.id === requestId)?.orderForm).toEqual({ id: f.id, number: f.number, status: 'draft' });
    expect((await auditOf(f.id)).map((e) => e.action)).toEqual(['order_form.created']);
  });

  test.each([
    ['core with all modules', { edition: 'core', modules: 'all' }, ['terms.modules']],
    ['enterprise with none', { modules: 'none' }, ['terms.modules']],
    ['named modules without a name', { modules: 'named', namedModules: [] }, ['terms.namedModules']],
    ['an end before the start', { subscriptionEnd: addDays(today, -1) }, ['terms.subscriptionEnd']],
    ['eleven years', { subscriptionEnd: addDays(today, 4100) }, ['terms.subscriptionEnd']],
    ['a negative fee', { annualFeeCents: -1 }, ['terms.annualFeeCents']],
    ['a bad contact', { authorizedContacts: ['nope'] }, ['terms.authorizedContacts']],
    ['an unknown module', { modules: 'named', namedModules: ['gold'] }, ['terms.namedModules']],
  ] as const)('400 invalid for %s', async (_label, extra, fields) => {
    const c = await createCustomer(t.db, {});
    const res = await callJson(app, 'orderFormCreate', { as: admin, body: { customerId: c.id, terms: { ...complete(addr('bad')), ...extra } as OrderFormTerms } });
    expect(res.status).toBe(400);
    expect(res.error?.fields).toEqual(fields as unknown as string[]);
  });

  test('an existing customer or a new one, not both; only license admins', async () => {
    const c = await createCustomer(t.db, {});
    const both = await callJson(app, 'orderFormCreate', { as: admin, body: { customerId: c.id, newCustomer: { name: 'X', kind: 'organization' }, terms: complete(addr('x')) } });
    expect(both.status).toBe(400);
    const denied = await callJson(app, 'orderFormCreate', { as: stranger, body: { customerId: c.id, terms: complete(addr('x')) } });
    expect(denied.status).toBe(403);
    expect(denied.error?.hint).toEqual({ missing: 'orders.write' });
    expect((await callJson(app, 'orderFormsList', { as: stranger })).status).toBe(403);
  });

  test('a draft changes by PATCH (merged, checked as a whole, audited with the changed fields); nothing else does', async () => {
    const { form } = await setup();
    const res = await callJson(app, 'orderFormUpdate', { as: admin, params: { id: form.id }, body: { terms: { nodes: 9, specialTerms: '  Net 45.  ' } } });
    expect(res.status).toBe(200);
    expect(res.body.terms).toMatchObject({ nodes: 9, specialTerms: 'Net 45.', edition: 'enterprise' });
    expect(res.body.nodes).toBe(9);
    const bad = await callJson(app, 'orderFormUpdate', { as: admin, params: { id: form.id }, body: { terms: { subscriptionEnd: addDays(today, -5) } } });
    expect(bad.status).toBe(400);
    expect(bad.error?.fields).toEqual(['terms.subscriptionEnd']);
    expect((await callJson(app, 'orderFormUpdate', { as: admin, params: { id: form.id }, body: { terms: 'x' } })).status).toBe(400);
    const events = await auditOf(form.id);
    expect(events.at(-1)).toEqual({ action: 'order_form.updated', details: { number: form.number, changed: ['nodes', 'specialTerms'] } });
    expect((await callJson(app, 'orderFormSend', { as: admin, params: { id: form.id } })).status).toBe(200);
    expect((await callJson(app, 'orderFormUpdate', { as: admin, params: { id: form.id }, body: { terms: { nodes: 2 } } })).status).toBe(409);
  });

  test('parallel PATCHes of different terms all apply: each reads the terms under the row lock', async () => {
    const { form } = await setup();
    const edits: Partial<OrderFormTerms>[] = [
      { nodes: 7 },
      { specialTerms: 'Net 45.' },
      { environments: 'eu-west' },
      { support: 'Business hours' },
      { renewal: 'By a new Order Form' },
      { keyPolicy: 'Delivered by mail' },
    ];
    const answers = await Promise.all(edits.map((terms) => callJson(app, 'orderFormUpdate', { as: admin, params: { id: form.id }, body: { terms } })));
    expect(answers.map((a) => a.status)).toEqual(edits.map(() => 200));
    const stored = (await callJson(app, 'orderFormGet', { as: admin, params: { id: form.id } })).body.terms;
    expect(stored).toMatchObject(Object.assign({}, ...edits) as Partial<OrderFormTerms>);
  });
});

describeDb('sending', () => {
  test('gaps refuse it with every missing field', async () => {
    const { form } = await setup({ governingLaw: '', customerAddress: ' ', serviceProvider: 'authorized' });
    expect(form.gaps).toEqual(['terms.customerAddress', 'terms.governingLaw', 'terms.serviceProviderScope']);
    const res = await callJson(app, 'orderFormSend', { as: admin, params: { id: form.id } });
    expect(res.status).toBe(400);
    expect(res.error?.fields).toEqual(form.gaps);
    expect(app.mailer.sent).toHaveLength(0);
  });

  test('freezes and hashes the text, makes the contacts members, mails each (an invite link for a new one)', async () => {
    const { form, email, customerId } = await setup();
    const newcomer = addr('newcomer');
    await callJson(app, 'orderFormUpdate', { as: admin, params: { id: form.id }, body: { terms: { authorizedContacts: [email, newcomer] } } });
    const res = await callJson(app, 'orderFormSend', { as: admin, params: { id: form.id } });
    expect(res.status).toBe(200);
    const f = res.body;
    const expected = createHash('sha256').update(orderFormDocument({ number: f.number, version: 1, terms: f.terms }), 'utf8').digest('hex');
    expect(f).toMatchObject({ status: 'sent', documentSha256: expected, gaps: [], sentBy: { id: adminUser.id } });
    // The text is stored with its hash, and the hash is the text's.
    expect(f.documentText).toBe(orderFormDocument({ number: f.number, version: 1, terms: f.terms }));
    expect(createHash('sha256').update(f.documentText!, 'utf8').digest('hex')).toBe(f.documentSha256!);
    expect(f.sentAt).not.toBeNull();
    const members = await t.db<{ email: string; user_id: string | null }[]>`select email, user_id from customer_members where customer_id = ${customerId} order by email`;
    expect(members.map((m) => m.email).sort()).toEqual([email, newcomer].sort());

    const linked = app.mailer.to(email);
    expect(linked).toHaveLength(1);
    expect(linked[0]!.subject).toBe(`Kudurru Order Form ${f.number} for Example GmbH is ready for review`);
    expect(linked[0]!.text).toContain(`http://localhost:8790/my/orders/${f.id}`);
    expect(linked[0]!.text).not.toContain('/invites/');
    expect(linked[0]!.text).toContain('Plan:       Enterprise');
    const invited = app.mailer.to(newcomer);
    expect(invited).toHaveLength(1);
    expect(invited[0]!.text).toMatch(/http:\/\/localhost:8790\/invites\/[A-Za-z0-9_-]{43}/);
    const sentEvent = (await auditOf(f.id)).find((e) => e.action === 'order_form.sent')!;
    expect(sentEvent.details).toEqual({ number: f.number, version: 1, documentSha256: expected, to: [email, newcomer] });
    // Sent once: a second send is 409.
    expect((await callJson(app, 'orderFormSend', { as: admin, params: { id: f.id } })).status).toBe(409);
  });

  test('without mail it is still sent, with a warning', async () => {
    const noMail = makeTestApp({ handlers, db: t.db, signing, mailer: new MemoryMailer(false) });
    try {
      const { form } = await setup();
      expect((await callJson(noMail, 'orderFormSend', { as: admin, params: { id: form.id } })).status).toBe(200);
      expect(noMail.log.find('order form sent, nobody mailed')).toHaveLength(1);
    } finally {
      await noMail.close();
    }
  });
});

describeDb("the customer's side", () => {
  test('members see sent forms of their customers, never a draft or another customer’s', async () => {
    const { form, contact } = await setup();
    expect((await callJson(app, 'myOrderForm', { as: contact, params: { id: form.id } })).status).toBe(404);
    expect((await callJson(app, 'myOrderForms', { as: contact })).body.items).toEqual([]);
    await callJson(app, 'orderFormSend', { as: admin, params: { id: form.id } });
    const mine = await callJson(app, 'myOrderForm', { as: contact, params: { id: form.id } });
    expect(mine.status).toBe(200);
    expect(mine.body).toMatchObject({ status: 'sent', authorized: true, customer: { name: 'Example GmbH' }, customerAcceptance: null, voidedAt: null });
    expect(Object.keys(mine.body)).not.toContain('sentBy');
    expect((await callJson(app, 'myOrderForms', { as: contact })).body.items.map((f) => f.id)).toEqual([form.id]);
    expect((await callJson(app, 'myOrderForm', { as: stranger, params: { id: form.id } })).status).toBe(404);
    expect((await accept(stranger, { id: form.id, documentSha256: mine.body.documentSha256 })).status).toBe(404);
  });

  test('accept: only an authorized contact, only the text they read; the evidence is kept and the approvers mailed', async () => {
    const { form, contact, email, customerId } = await sent();
    // A member who is not an authorized contact sees the form but cannot answer it.
    const other = await createUser(t.db, { roles: ['customer'] });
    await t.db`insert into customer_members (customer_id, email, user_id, invited_by_label, linked_at) values (${customerId}, ${other.email!}, ${other.id}, 'test', now())`;
    const otherSession = await signInAs(t.db, other);
    expect((await callJson(app, 'myOrderForm', { as: otherSession, params: { id: form.id } })).body.authorized).toBe(false);
    const refused = await accept(otherSession, form);
    expect(refused.status).toBe(403);
    expect(refused.error?.message).toContain(email);

    expect((await accept(contact, form, { authorized: false })).status).toBe(400);
    expect((await accept(contact, form, { title: '' })).status).toBe(400);
    expect((await accept(contact, form, { documentSha256: 'a'.repeat(64) })).status).toBe(409);

    const res = await accept(contact, form, { name: '  Carla Contact ' });
    expect(res.status).toBe(200);
    expect(res.body.status).toBe('accepted');
    expect(res.body.customerAcceptance).toMatchObject({ name: 'Carla Contact', title: 'CTO', email, documentSha256: form.documentSha256 });
    const detail = (await callJson(app, 'orderFormGet', { as: admin, params: { id: form.id } })).body;
    expect(detail.customerAcceptance).toMatchObject({ ip: '127.0.0.1', userAgent: expect.any(String) as string, email });
    const mail = app.mailer.to('admin@example.org');
    expect(mail).toHaveLength(1);
    expect(mail[0]!.subject).toBe(`Countersign Order Form ${form.number}: accepted by Example GmbH`);
    expect(mail[0]!.text).toContain(`Carla Contact (CTO, ${email}) accepted`);
    expect(mail[0]!.text).toContain(`Document SHA-256: ${form.documentSha256}`);
    expect((await accept(contact, form)).status).toBe(409);
  });

  test('decline with a note, then revise: a new version the old answer cannot accept', async () => {
    const { form, contact } = await sent();
    expect((await callJson(app, 'myOrderFormDecline', { as: contact, params: { id: form.id }, body: { note: '' } })).status).toBe(400);
    const declined = await callJson(app, 'myOrderFormDecline', { as: contact, params: { id: form.id }, body: { note: 'We need 10 nodes.' } });
    expect(declined.status).toBe(200);
    expect(declined.body.declined).toMatchObject({ note: 'We need 10 nodes.' });
    const mail = app.mailer.to('admin@example.org');
    expect(mail[0]!.subject).toBe(`Order Form ${form.number} declined by Example GmbH`);
    expect(mail[0]!.text).toContain('Their note:\nWe need 10 nodes.');

    const revised = await callJson(app, 'orderFormRevise', { as: admin, params: { id: form.id } });
    expect(revised.status).toBe(200);
    expect(revised.body).toMatchObject({ status: 'draft', version: 2, documentSha256: null, declined: null, sentAt: null });
    expect((await callJson(app, 'myOrderForm', { as: contact, params: { id: form.id } })).status).toBe(404);
    await callJson(app, 'orderFormUpdate', { as: admin, params: { id: form.id }, body: { terms: { nodes: 10 } } });
    const again = (await callJson(app, 'orderFormSend', { as: admin, params: { id: form.id } })).body;
    expect(again.documentSha256).not.toBe(form.documentSha256);
    expect(orderFormDocument({ number: again.number, version: again.version, terms: again.terms })).toContain('(version 2)');
    expect((await accept(contact, form)).status).toBe(409);
    expect((await accept(contact, again)).status).toBe(200);
    expect((await auditOf(form.id)).map((e) => e.action)).toEqual([
      'order_form.created',
      'order_form.sent',
      'order_form.declined',
      'order_form.revised',
      'order_form.updated',
      'order_form.sent',
      'order_form.accepted',
    ]);
  });
});

describeDb('countersigning, issuing and voiding', () => {
  test('countersign: executed, final; the contacts get the signed copy', async () => {
    const { form, contact, email } = await sent();
    expect((await callJson(app, 'orderFormCountersign', { as: admin, params: { id: form.id }, body: { name: 'Robert Herbaugh', title: 'Owner', documentSha256: form.documentSha256! } })).status).toBe(409);
    await accept(contact, form);
    app.mailer.clear();
    expect((await callJson(app, 'orderFormCountersign', { as: admin, params: { id: form.id }, body: { name: 'Robert Herbaugh', title: 'Owner', documentSha256: 'b'.repeat(64) } })).status).toBe(409);
    const res = await callJson(app, 'orderFormCountersign', { as: admin, params: { id: form.id }, body: { name: 'Robert Herbaugh', title: 'Owner', documentSha256: form.documentSha256! } });
    expect(res.status).toBe(200);
    expect(res.body.status).toBe('executed');
    expect(res.body.licensorAcceptance).toMatchObject({ name: 'Robert Herbaugh', title: 'Owner', email: adminUser.email, by: { id: adminUser.id } });
    const mail = app.mailer.to(email);
    expect(mail).toHaveLength(1);
    expect(mail[0]!.subject).toBe(`Kudurru Order Form ${form.number} is executed`);
    const copy = mail[0]!.attachments?.find((a) => a.filename === `${form.number}.md`);
    expect(copy?.contentType).toBe('text/markdown; charset=utf-8');
    expect(String(copy?.content)).toContain('Customer acceptance: Carla Contact, CTO');
    expect(String(copy?.content)).toContain('Licensor acceptance: Robert Herbaugh, Owner');
    expect(String(copy?.content)).toContain(`Document SHA-256: ${form.documentSha256}`);
    expect(String(copy?.content).startsWith(form.documentText!)).toBe(true);
    for (const route of ['orderFormRevise', 'orderFormSend'] as const) expect((await callJson(app, route, { as: admin, params: { id: form.id } })).status).toBe(409);
    expect((await callJson(app, 'orderFormVoid', { as: admin, params: { id: form.id }, body: { reason: 'x' } })).status).toBe(409);
  });

  test('the kept copy is the text as sent, even when a later rendering would differ', async () => {
    const { form, contact, email } = await sent();
    await accept(contact, form);
    // A changed template (or anything else that renders the terms differently) after sending.
    await t.db`update order_forms set terms = jsonb_set(terms, '{specialTerms}', '"Rendered differently today."') where id = ${form.id}`;
    app.mailer.clear();
    await callJson(app, 'orderFormCountersign', { as: admin, params: { id: form.id }, body: { name: 'R H', title: 'Owner', documentSha256: form.documentSha256! } });
    const copy = String(app.mailer.to(email)[0]!.attachments?.find((a) => a.filename === `${form.number}.md`)?.content);
    expect(copy.startsWith(form.documentText!)).toBe(true);
    expect(copy).not.toContain('Rendered differently today.');
    const mine = (await callJson(app, 'myOrderForm', { as: contact, params: { id: form.id } })).body;
    expect(mine.documentText).toBe(form.documentText!);
    expect(createHash('sha256').update(mine.documentText, 'utf8').digest('hex')).toBe(mine.documentSha256);
  });

  test('issue one license from an executed form, for its customer only; the business request becomes issued', async () => {
    const email = addr('bizissue');
    expect((await call(app, 'publicBusinessRequest', { body: { name: 'Ada', email, company: 'Issue Co', edition: 'enterprise', nodes: 5, website: '' } })).status).toBe(202);
    const requestId = (await t.db<{ id: string }[]>`select id from business_requests where email = ${email}`)[0]!.id;
    const user = await createUser(t.db, { roles: ['customer'], email });
    const c = await createCustomer(t.db, { name: 'Issue Co', members: [{ email, userId: user.id }] });
    const created = (await callJson(app, 'orderFormCreate', { as: admin, body: { customerId: c.id, businessRequestId: requestId, terms: complete(email) } })).body;
    const form = (await callJson(app, 'orderFormSend', { as: admin, params: { id: created.id } })).body;
    const contact = await signInAs(t.db, user);
    const license = {
      licensee: 'Issue Co',
      plan: 'enterprise' as const,
      nodes: 5,
      features: null,
      expiresOn: form.terms.subscriptionEnd,
      agreementRef: form.number,
      orderFormId: form.id,
    };
    // Not executed yet.
    expect((await callJson(app, 'licenseIssue', { as: admin, body: { ...license, customerId: c.id } })).status).toBe(409);
    await accept(contact, form);
    await callJson(app, 'orderFormCountersign', { as: admin, params: { id: form.id }, body: { name: 'R H', title: 'Owner', documentSha256: form.documentSha256! } });
    const elsewhere = await createCustomer(t.db, {});
    const wrong = await callJson(app, 'licenseIssue', { as: admin, body: { ...license, customerId: elsewhere.id } });
    expect(wrong.status).toBe(400);
    expect(wrong.error?.fields).toEqual(['customerId']);
    expect((await callJson(app, 'licenseIssue', { as: admin, body: { ...license, newCustomer: { name: 'New', kind: 'organization' } } })).status).toBe(400);
    expect((await callJson(app, 'licenseIssue', { as: admin, body: { ...license, customerId: c.id, businessRequestId: requestId } })).status).toBe(400);
    // The key must stay within the countersigned terms.
    for (const [extra, field] of [
      [{ plan: 'business' }, 'plan'],
      [{ nodes: 6 }, 'nodes'],
      [{ features: ['oidc'] }, 'features'],
      [{ expiresOn: addDays(form.terms.subscriptionEnd, 1) }, 'expiresOn'],
    ] as const) {
      const outside = await callJson(app, 'licenseIssue', { as: admin, body: { ...license, ...extra, customerId: c.id } });
      expect(outside.status).toBe(400);
      expect(outside.error?.fields).toEqual([field]);
    }
    expect((await callJson(app, 'orderFormGet', { as: admin, params: { id: form.id } })).body.licenseId).toBeNull();

    const issued = await callJson(app, 'licenseIssue', { as: admin, body: { ...license, customerId: c.id } });
    expect(issued.status).toBe(200);
    expect(issued.body.orderForm).toEqual({ id: form.id, number: form.number });
    expect((await callJson(app, 'orderFormGet', { as: admin, params: { id: form.id } })).body.licenseId).toBe(issued.body.id);
    expect((await callJson(app, 'myOrderForm', { as: contact, params: { id: form.id } })).body.licenseId).toBe(issued.body.id);
    const request = (await callJson(app, 'businessRequestsList', { as: admin, query: { status: 'issued' } })).body.items.find((r) => r.id === requestId);
    expect(request).toMatchObject({ status: 'issued', resultingLicenseId: issued.body.id, orderForm: { id: form.id, status: 'executed' } });
    expect((await auditOf(form.id)).at(-1)).toEqual({ action: 'order_form.licensed', details: { licenseId: issued.body.id } });
    // One license per form.
    const twice = await callJson(app, 'licenseIssue', { as: admin, body: { ...license, customerId: c.id } });
    expect(twice.status).toBe(409);
    expect(twice.error?.message).toContain(issued.body.id);
  });

  test('void: a draft quietly; a sent form tells its contacts, never the reason; the customer sees it withdrawn', async () => {
    const draft = (await setup()).form;
    const voidedDraft = await callJson(app, 'orderFormVoid', { as: admin, params: { id: draft.id }, body: { reason: 'Duplicate.' } });
    expect(voidedDraft.body).toMatchObject({ status: 'void', voided: { reason: 'Duplicate.', by: { id: adminUser.id } } });
    expect(app.mailer.sent).toHaveLength(0);

    const { form, contact, email } = await sent();
    app.mailer.clear();
    expect((await callJson(app, 'orderFormVoid', { as: admin, params: { id: form.id }, body: { reason: '' } })).status).toBe(400);
    expect((await callJson(app, 'orderFormVoid', { as: admin, params: { id: form.id }, body: { reason: 'Pricing error, internal.' } })).status).toBe(200);
    const mail = app.mailer.to(email);
    expect(mail).toHaveLength(1);
    expect(mail[0]!.subject).toBe(`Kudurru Order Form ${form.number} was withdrawn`);
    expect(mail[0]!.text).not.toContain('Pricing error');
    const mine = await callJson(app, 'myOrderForm', { as: contact, params: { id: form.id } });
    expect(mine.body.status).toBe('void');
    expect(mine.body.voidedAt).not.toBeNull();
    expect(JSON.stringify(mine.body)).not.toContain('Pricing error');
    expect((await accept(contact, form)).status).toBe(409);
    expect((await callJson(app, 'orderFormVoid', { as: admin, params: { id: form.id }, body: { reason: 'again' } })).status).toBe(409);
  });

  test('void a declined form: the customer sees it withdrawn, the audit names where it came from', async () => {
    const { form, contact, email } = await sent();
    expect((await callJson(app, 'myOrderFormDecline', { as: contact, params: { id: form.id }, body: { note: 'Not now.' } })).status).toBe(200);
    app.mailer.clear();
    const voided = await callJson(app, 'orderFormVoid', { as: admin, params: { id: form.id }, body: { reason: 'They went elsewhere.' } });
    expect(voided.status).toBe(200);
    expect(voided.body).toMatchObject({ status: 'void', declined: { note: 'Not now.' }, voided: { reason: 'They went elsewhere.' } });
    expect(app.mailer.to(email)).toHaveLength(1);
    const mine = (await callJson(app, 'myOrderForm', { as: contact, params: { id: form.id } })).body;
    expect(mine.status).toBe('void');
    expect(mine.voidedAt).not.toBeNull();
    expect((await auditOf(form.id)).at(-1)).toEqual({ action: 'order_form.voided', details: { number: form.number, from: 'declined', reason: 'They went elsewhere.' } });
  });

  test('the list: open first, by status and customer', async () => {
    const { form, customerId } = await setup();
    const list = await callJson(app, 'orderFormsList', { as: admin, query: { customerId } });
    expect(list.body.items.map((f) => f.id)).toEqual([form.id]);
    const drafts = await callJson(app, 'orderFormsList', { as: admin, query: { status: 'draft' } });
    expect(drafts.body.items.every((f) => f.status === 'draft')).toBe(true);
    const all = (await callJson(app, 'orderFormsList', { as: admin })).body.items;
    const firstClosed = all.findIndex((f) => !['draft', 'sent', 'accepted'].includes(f.status));
    if (firstClosed >= 0) expect(all.slice(firstClosed).every((f) => !['draft', 'sent', 'accepted'].includes(f.status))).toBe(true);
  });
});

describe('the document', () => {
  test('Schedule A with every agreed term; the signatures only in the kept copy', () => {
    const terms = complete('a@b.example', { specialTerms: 'Pipe | escaped\nand joined', serviceProvider: 'authorized', serviceProviderScope: 'Pooled: 3 clients' });
    const text = orderFormDocument({ number: 'OF-2026-0007', version: 3, terms });
    expect(text).toContain('Order Form No.: OF-2026-0007 (version 3)');
    expect(text).toContain('Licensor: Robert Herbaugh\n  Robert Herbaugh\n  1 Main Street, Springfield');
    expect(text).toContain('| License plan | Enterprise |');
    expect(text).toContain('| Included Enterprise modules | All current `ee/` modules |');
    expect(text).toContain('| Licensed Production Nodes | 5 concurrent running Nodes across all covered environments and clusters |');
    expect(text).toContain('| Service Provider Use | Authorized: Pooled: 3 clients |');
    expect(text).toContain('| Annual fees / Node fees | $1,994.00 per year (platform fee $499.00; Node fee $299.00 per Node).');
    expect(text).toContain('| Special terms | Pipe \\| escaped; and joined |');
    expect(text).not.toContain('Acceptance');
    expect(text).not.toMatch(/\n{3}/);
    const signed = orderFormDocument({
      number: 'OF-2026-0007',
      version: 3,
      terms,
      signatures: { customer: { name: 'C', title: 'CTO', email: 'a@b.example', at: '2026-10-18T09:41:00.000Z', documentSha256: 'f'.repeat(64) }, licensor: null, documentSha256: 'f'.repeat(64) },
    });
    expect(signed.startsWith(text)).toBe(true);
    expect(signed).toContain('Customer acceptance: C, CTO (a@b.example), accepted electronically on 2026-10-18 09:41 UTC');
    expect(signed).toContain('Licensor acceptance: not yet accepted');
  });
});
