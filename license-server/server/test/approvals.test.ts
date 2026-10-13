// Approval before a personal key is cut (the owner's request of
// 2026-10-13; services/approvals.ts, requests.ts previewApproval and
// decideApproval): the default path from the kudurru.io form (confirm →
// waiting for approval → the approvers' mail with a link each → the
// /approve page's preview → approve → the key mailed, never answered), the
// actor (the admin the link went to, else "approval link (<address>)"),
// reject with a note, single use, one recipient's decision ends the
// others' links, a decision in the portal ends them too, expiry, an
// address that no longer approves (an IdP role only within 30 days of the
// last sign-in), unknown tokens, no link for blocked addresses, the review
// rules checked again on approval, a renewal through the link (the
// license's nodes kept), the issuing quota of a shared address, a failed
// signature that keeps the link usable, and the per-address limit.
import { afterAll, beforeAll, expect, test } from 'bun:test';
import { ATTESTATION_STATEMENT } from '../../shared/catalog';
import { addDays, todayUtc } from '../../shared/dates';
import { approvalHandlers } from '../api/approvals';
import { publicHandlers } from '../api/public';
import { requestHandlers } from '../api/requests';
import type { Config } from '../config';
import { createPipeline } from '../http/pipeline';
import { DEFAULT_RATE_LIMITS } from '../http/ratelimit';
import { mergeHandlers } from '../http/router';
import { verify } from '../licensing/jws';
import { loadSigning } from '../licensing/keyring';
import { signingUnavailable, type SigningState } from '../licensing/types';
import { MemoryLogger, silentLogger } from '../log';
import { call, callJson, makeTestApp, type TestApp } from './call';
import { describeDb, hasDb, useTestDb as fileDatabase } from './db';
import { createUser, signInAs, testConfig } from './fixtures';

const handlers = mergeHandlers(publicHandlers, approvalHandlers, requestHandlers);
// One fresh database for this file (useTestDb, renamed: it is not a React hook).
const t = fileDatabase();
let signing: SigningState;
let app: TestApp;

/** The default configuration (approval required, ADMIN_NOTIFY_EMAIL=admin@example.org) without the per-network form limit in the way. */
function config(overrides: Parameters<typeof testConfig>[0] = {}): Config {
  return testConfig({ ...overrides, publicForm: { limitPerIpPerHour: 10_000, limitPerEmailPerDay: 100 } });
}

beforeAll(async () => {
  if (!hasDb) return;
  signing = await loadSigning(testConfig(), silentLogger);
  app = makeTestApp({ handlers, db: t.db, signing, config: config() });
});
afterAll(async () => {
  await app?.close();
});

let n = 0;
const addr = (tag: string): string => `${tag}-${++n}-${process.pid}@approvals.example`;

/** The form and its confirmation, as a visitor does them; the request that now waits. */
async function formRequest(a: TestApp, email: string, name = 'Homelab Hank'): Promise<string> {
  expect((await call(a, 'publicKeyRequest', { body: { name, email, use: 'DNS filtering for my home', noncommercial: true, website: '' } })).status).toBe(202);
  const confirm = a.mailer.to(email).at(-1)!;
  const token = /\/verify\?token=([A-Za-z0-9_-]{43})/.exec(confirm.text)![1]!;
  const verified = await callJson(a, 'publicKeyRequestVerify', { body: { token } });
  expect(verified.body).toEqual({ outcome: 'pending_review' });
  const row = (await t.db<{ license_request_id: string }[]>`
    select license_request_id from public_key_requests where email = ${email} and verified_at is not null order by created_at desc limit 1`)[0]!;
  return row.license_request_id;
}

/** The newest approve link mailed to `to` about a request. */
function linkFor(a: TestApp, to: string, requestId: string): string {
  const mail = a.mailer
    .to(to)
    .filter((m) => m.text.includes(requestId))
    .at(-1);
  const m = mail && /\/approve\?token=([A-Za-z0-9_-]{43})/.exec(mail.text);
  if (!m) throw new Error(`no approve link to ${to} for ${requestId}`);
  return m[1]!;
}

async function requestRow(id: string) {
  return (await t.db<{ status: string; decided_by: string | null; decided_by_label: string; decision_note: string; resulting_license_id: string | null }[]>`
    select status, decided_by, decided_by_label, decision_note, resulting_license_id from license_requests where id = ${id}`)[0]!;
}

async function usedAt(token: string): Promise<Date | null> {
  return (await t.db<{ used_at: Date | null }[]>`select used_at from request_approval_links where token_hash = ${new Bun.CryptoHasher('sha256').update(token).digest()}`)[0]!.used_at;
}

describeDb('the one-click approval link', () => {
  test('the default path: confirm, the approvers\' mail, preview, approve; the key is mailed (and works), never answered', async () => {
    const email = addr('default');
    const requestId = await formRequest(app, email, 'Approved Ada');
    expect(app.mailer.to(email)[0]!.text).toContain('once an administrator approves it');
    expect(app.mailer.to(email).at(-1)!.subject).toBe('Your request for a kudurru personal key is waiting for approval');
    expect(await t.db`select 1 from licenses l join customer_members m on m.customer_id = l.customer_id where m.email = ${email}`).toHaveLength(0);

    const token = linkFor(app, 'admin@example.org', requestId);
    const preview = await callJson(app, 'approvalPreview', { body: { token } });
    expect(preview.status).toBe(200);
    expect(preview.body).toEqual({
      request: {
        id: requestId,
        name: 'Approved Ada',
        email,
        use: 'DNS filtering for my home',
        source: 'web',
        createdAt: preview.body.request.createdAt,
        reviewReason: 'auto_approve_off',
        attestation: { at: preview.body.request.attestation.at, statement: ATTESTATION_STATEMENT },
      },
      renews: null,
      terms: { plan: 'personal', nodes: 3, features: null, expiresOn: addDays(todayUtc(), 365) },
      status: 'open',
      decidedBy: null,
      expired: false,
      sentTo: 'admin@example.org',
    });
    // Opening the link (and the preview) changes nothing.
    expect(await usedAt(token)).toBeNull();
    expect((await requestRow(requestId)).status).toBe('open');

    app.mailer.clear();
    const res = await call(app, 'approvalDecide', { body: { token, decision: 'approve' } });
    const text = await res.text();
    expect(res.status).toBe(200);
    expect(text).not.toContain('eyJ');
    const result = JSON.parse(text) as { status: string; licenseId: string; outcome: string };
    expect(result).toEqual({ status: 'approved', licenseId: result.licenseId, outcome: 'issued' });

    const lic = (await t.db<{ plan: string; nodes: number; source: string; created_by_label: string }[]>`select plan, nodes, source, created_by_label from licenses where id = ${result.licenseId}`)[0]!;
    expect(lic).toEqual({ plan: 'personal', nodes: 3, source: 'web', created_by_label: 'approval link (admin@example.org)' });
    const key = (await t.db<{ token: string; issued_by: string | null; issued_by_label: string }[]>`select token, issued_by, issued_by_label from license_keys where license_id = ${result.licenseId}`)[0]!;
    expect(key.issued_by).toBeNull();
    const claims = (await verify(key.token, signing.keyring)).claims;
    expect(claims).toMatchObject({ jti: result.licenseId, plan: 'personal', nodes: 3, sub: 'Approved Ada' });

    const mail = app.mailer.to(email);
    expect(mail).toHaveLength(1);
    expect(mail[0]!.subject).toBe('Your kudurru license key');
    expect(mail[0]!.text).toContain('has been approved');
    expect(mail[0]!.text).toContain(key.token);
    expect(mail[0]!.text).toContain('/invites/');

    expect(await requestRow(requestId)).toMatchObject({ status: 'approved', decided_by: null, decided_by_label: 'approval link (admin@example.org)', resulting_license_id: result.licenseId });
    expect(await usedAt(token)).not.toBeNull();
    const events = await t.db<{ action: string; actor_label: string; details: Record<string, unknown>; ip: string | null }[]>`
      select action, actor_label, details, host(ip) as ip from audit_events where (target_id = ${requestId} and action = 'request.approved') or (target_id = ${result.licenseId} and action = 'license.issued')`;
    const approved = events.find((e) => e.action === 'request.approved')!;
    expect(approved).toMatchObject({ actor_label: 'approval link (admin@example.org)', details: { via: 'approval_link', sentTo: 'admin@example.org', outcome: 'issued' }, ip: '127.0.0.1' });
    expect(events.find((e) => e.action === 'license.issued')!.details).toMatchObject({ requestId, via: 'approval_link' });

    // Used: it still previews, and says so; it cannot decide again.
    const after = await callJson(app, 'approvalPreview', { body: { token } });
    expect(after.body).toMatchObject({ status: 'approved', expired: true, decidedBy: 'approval link (admin@example.org)' });
    const again = await callJson(app, 'approvalDecide', { body: { token, decision: 'approve' } });
    expect(again.status).toBe(400);
    expect(again.error?.key).toBe('invalid_token');
    expect(again.error?.message).toContain('used already');
  });

  test('the actor is the enabled license admin whose verified email the link went to', async () => {
    const rita = await createUser(t.db, { roles: ['license_admin'], name: 'Rita Reviewer', email: `rita-${process.pid}@approvals.example` });
    // Someone else with the address but no admin role does not count.
    await createUser(t.db, { roles: ['customer'], email: `other-${process.pid}@approvals.example` });
    const ritaApp = makeTestApp({ handlers, db: t.db, signing, config: config({ mail: { adminNotify: [rita.email!, `other-${process.pid}@approvals.example`] } }) });
    try {
      const requestId = await formRequest(ritaApp, addr('actor'));
      const res = await callJson(ritaApp, 'approvalDecide', { body: { token: linkFor(ritaApp, rita.email!, requestId), decision: 'approve' } });
      expect(res.status).toBe(200);
      expect(await requestRow(requestId)).toMatchObject({ decided_by: rita.id, decided_by_label: `Rita Reviewer (${rita.email})` });
      expect((await t.db<{ issued_by: string }[]>`select issued_by from license_keys where license_id = ${res.body.licenseId!}`)[0]!.issued_by).toBe(rita.id);
      // The other recipient's link (no admin behind it) now finds the request decided.
      const other = await callJson(ritaApp, 'approvalDecide', { body: { token: linkFor(ritaApp, `other-${process.pid}@approvals.example`, requestId), decision: 'approve' } });
      expect(other.status).toBe(409);
      expect(other.error?.key).toBe('conflict');
      const preview = await callJson(ritaApp, 'approvalPreview', { body: { token: linkFor(ritaApp, `other-${process.pid}@approvals.example`, requestId) } });
      expect(preview.body).toMatchObject({ status: 'approved', expired: false, decidedBy: `Rita Reviewer (${rita.email})` });
    } finally {
      await ritaApp.close();
    }
  });

  test('reject: a note of 1-1000 characters, mailed to the requester; the link is used', async () => {
    const email = addr('reject');
    const requestId = await formRequest(app, email, 'Rejected Rob');
    const token = linkFor(app, 'admin@example.org', requestId);
    for (const note of [undefined, '   ', 'x'.repeat(1001)]) {
      const res = await callJson(app, 'approvalDecide', { body: { token, decision: 'reject', ...(note === undefined ? {} : { note }) } });
      expect(res.status).toBe(400);
      expect(res.error?.fields).toEqual(['note']);
    }
    expect((await callJson(app, 'approvalDecide', { body: { token, decision: 'maybe' } })).error?.fields).toEqual(['decision']);
    expect(await usedAt(token)).toBeNull();

    app.mailer.clear();
    const res = await callJson(app, 'approvalDecide', { body: { token, decision: 'reject', note: 'This looks like a company network.' } });
    expect(res.status).toBe(200);
    expect(res.body).toEqual({ status: 'rejected' });
    expect(await requestRow(requestId)).toMatchObject({ status: 'rejected', decision_note: 'This looks like a company network.', resulting_license_id: null });
    const mail = app.mailer.to(email)[0]!;
    expect(mail.text).toContain('This looks like a company network.');
    expect(mail.text).toContain('Hello Rejected Rob,');
    expect(await usedAt(token)).not.toBeNull();
    const event = (await t.db<{ details: Record<string, unknown> }[]>`select details from audit_events where action = 'request.rejected' and target_id = ${requestId}`)[0]!;
    expect(event.details).toMatchObject({ via: 'approval_link', sentTo: 'admin@example.org' });
  });

  test('a request decided in the portal makes its links useless', async () => {
    const admin = await signInAs(t.db, await createUser(t.db, { roles: ['license_admin'] }));
    const requestId = await formRequest(app, addr('portal'));
    const token = linkFor(app, 'admin@example.org', requestId);
    expect((await callJson(app, 'requestReject', { as: admin, params: { id: requestId }, body: { note: 'Decided in the portal.' } })).status).toBe(200);
    const res = await callJson(app, 'approvalDecide', { body: { token, decision: 'approve' } });
    expect(res.status).toBe(409);
    expect(res.error?.message).toContain('already decided');
    expect(await usedAt(token)).toBeNull();
    expect((await callJson(app, 'approvalPreview', { body: { token } })).body).toMatchObject({ status: 'rejected', expired: false });
  });

  test('an expired link still previews but cannot decide; unknown and malformed tokens are 400 invalid_token', async () => {
    const requestId = await formRequest(app, addr('expired'));
    const token = linkFor(app, 'admin@example.org', requestId);
    await t.db`update request_approval_links set expires_at = now() - interval '1 second' where request_id = ${requestId}`;
    expect((await callJson(app, 'approvalPreview', { body: { token } })).body).toMatchObject({ status: 'open', expired: true });
    const res = await callJson(app, 'approvalDecide', { body: { token, decision: 'approve' } });
    expect(res.status).toBe(400);
    expect(res.error?.key).toBe('invalid_token');
    expect(res.error?.message).toContain('expired');
    expect((await requestRow(requestId)).status).toBe('open');

    const unknown = 'A'.repeat(43);
    for (const bad of [unknown, 'nope']) {
      expect((await callJson(app, 'approvalPreview', { body: { token: bad } })).error?.key).toBe('invalid_token');
      expect((await callJson(app, 'approvalDecide', { body: { token: bad, decision: 'approve' } })).error?.key).toBe('invalid_token');
    }
    expect((await callJson(app, 'approvalPreview', { body: {} })).error?.fields).toEqual(['token']);
    // GET never decides (mail scanners fetch links): the routes are POST only.
    expect((await app.fetch(`/api/v1/public/approvals/decide?token=${token}`)).status).toBe(405);
  });

  test('a link only decides while its address still approves (in ADMIN_NOTIFY_EMAIL, or an enabled admin when it is unset)', async () => {
    // ADMIN_NOTIFY_EMAIL changed: the old address's links stop working.
    const requestId = await formRequest(app, addr('revoked'));
    const token = linkFor(app, 'admin@example.org', requestId);
    const moved = makeTestApp({ handlers, db: t.db, signing, config: config({ mail: { adminNotify: ['new-approver@example.org'] } }) });
    try {
      expect((await callJson(moved, 'approvalPreview', { body: { token } })).body).toMatchObject({ status: 'open', expired: true });
      const res = await callJson(moved, 'approvalDecide', { body: { token, decision: 'approve' } });
      expect(res.status).toBe(400);
      expect(res.error?.message).toContain('no longer approves');
    } finally {
      await moved.close();
    }
    expect(await usedAt(token)).toBeNull();

    // ADMIN_NOTIFY_EMAIL unset: the links go to the admins; a disabled admin's link stops working.
    const fallback = makeTestApp({ handlers, db: t.db, signing, config: config({ mail: { adminNotify: [] } }) });
    try {
      const leaving = await createUser(t.db, { roles: ['license_admin'], email: `leaving-${process.pid}@approvals.example` });
      const second = await formRequest(fallback, addr('fallback'));
      const link = linkFor(fallback, leaving.email!, second);
      await t.db`update users set disabled_at = now() where id = ${leaving.id}`;
      const res = await callJson(fallback, 'approvalDecide', { body: { token: link, decision: 'approve' } });
      expect(res.status).toBe(400);
      expect(res.error?.key).toBe('invalid_token');
      expect((await requestRow(second)).status).toBe('open');
    } finally {
      await fallback.close();
    }
  });

  test('a role from the IdP makes an approver within 30 days of the last sign-in; then the mail stops and the links stop working', async () => {
    const fallback = makeTestApp({ handlers, db: t.db, signing, config: config({ mail: { adminNotify: [] } }) });
    const idp = await createUser(t.db, { roles: ['customer'], idpRoles: ['license_admin'], name: 'Ida IdP', email: `ida-${process.pid}@approvals.example` });
    try {
      const requestId = await formRequest(fallback, addr('idp'));
      const token = linkFor(fallback, idp.email!, requestId);
      expect((await callJson(fallback, 'approvalPreview', { body: { token } })).body).toMatchObject({ status: 'open', expired: false });
      // Removed from the admin group at the IdP: idp_roles keep the role until a sign-in that never comes.
      await t.db`update users set last_login_at = now() - interval '31 days' where id = ${idp.id}`;
      expect((await callJson(fallback, 'approvalPreview', { body: { token } })).body).toMatchObject({ status: 'open', expired: true });
      const res = await callJson(fallback, 'approvalDecide', { body: { token, decision: 'reject', note: 'Not from me.' } });
      expect(res.status).toBe(400);
      expect(res.error?.key).toBe('invalid_token');
      expect(res.error?.message).toContain('no longer approves');
      expect((await requestRow(requestId)).status).toBe('open');
      fallback.mailer.clear();
      const next = await formRequest(fallback, addr('idp-next'));
      expect(fallback.mailer.to(idp.email!)).toHaveLength(0);
      expect(await t.db`select 1 from request_approval_links where request_id = ${next} and sent_to = ${idp.email!}`).toHaveLength(0);
    } finally {
      await fallback.close();
    }
    // Named in ADMIN_NOTIFY_EMAIL, the address still approves, but the decision credits no admin.
    const named = makeTestApp({ handlers, db: t.db, signing, config: config({ mail: { adminNotify: [idp.email!] } }) });
    try {
      const requestId = await formRequest(named, addr('idp-named'));
      expect((await callJson(named, 'approvalDecide', { body: { token: linkFor(named, idp.email!, requestId), decision: 'approve' } })).status).toBe(200);
      expect(await requestRow(requestId)).toMatchObject({ decided_by: null, decided_by_label: `approval link (${idp.email})` });
    } finally {
      await named.close();
    }
  });

  test('approving checks the review rules again: an address blocked, or a license withdrawn, since the mail → 409; the link can still reject', async () => {
    const blocked = addr('blocked-later');
    const first = await formRequest(app, blocked);
    const token = linkFor(app, 'admin@example.org', first);
    await t.db`insert into blocked_emails (email, reason) values (${blocked}, 'test')`;
    const res = await callJson(app, 'approvalDecide', { body: { token, decision: 'approve' } });
    expect(res.status).toBe(409);
    expect(res.error?.key).toBe('conflict');
    expect(res.error?.message).toContain('The address is on the blocklist.');
    expect(res.error?.hint).toBe('http://localhost:8790/requests');
    expect(await usedAt(token)).toBeNull();
    expect((await requestRow(first)).status).toBe('open');
    expect((await callJson(app, 'approvalDecide', { body: { token, decision: 'reject', note: 'Blocked since.' } })).body).toEqual({ status: 'rejected' });

    // A renewal waits with a link; the member row is removed and the
    // license withdrawn (no member address left, so the request stays open).
    const email = addr('withdrawn-later');
    const issued = await callJson(app, 'approvalDecide', { body: { token: linkFor(app, 'admin@example.org', await formRequest(app, email)), decision: 'approve' } });
    const id = issued.body.licenseId!;
    const soon = new Date(`${addDays(todayUtc(), 10)}T23:59:59Z`);
    await t.db`update licenses set expires_at = ${soon} where id = ${id}`;
    await t.db`update license_keys set expires_at = ${soon} where license_id = ${id}`;
    const second = await formRequest(app, email);
    const link = linkFor(app, 'admin@example.org', second);
    await t.db`delete from customer_members where email = ${email}`;
    await t.db`update licenses set status = 'withdrawn', withdrawn_at = now() where id = ${id}`;
    const late = await callJson(app, 'approvalDecide', { body: { token: link, decision: 'approve' } });
    expect(late.status).toBe(409);
    expect(late.error?.message).toContain('A license of this person was withdrawn before.');
    expect(await usedAt(link)).toBeNull();
    expect(await requestRow(second)).toMatchObject({ status: 'open', resulting_license_id: null });
    // No second person customer, no new key.
    expect(await t.db`select 1 from licenses l join customers c on c.id = l.customer_id where c.contact_email = ${email}`).toHaveLength(1);
    expect(await t.db`select 1 from customers where contact_email = ${email}`).toHaveLength(1);
  });

  test('approvals through links to a shared address count against ISSUE_MAX_PER_ACTOR_PER_DAY by its label', async () => {
    const shared = `shared-${process.pid}@approvals.example`;
    const quota = makeTestApp({ handlers, db: t.db, signing, config: config({ mail: { adminNotify: [shared] }, issuing: { maxPerActorPerDay: 1 } }) });
    try {
      const first = await formRequest(quota, addr('quota'));
      const second = await formRequest(quota, addr('quota'));
      expect((await callJson(quota, 'approvalDecide', { body: { token: linkFor(quota, shared, first), decision: 'approve' } })).status).toBe(200);
      const token = linkFor(quota, shared, second);
      const res = await callJson(quota, 'approvalDecide', { body: { token, decision: 'approve' } });
      expect(res.status).toBe(429);
      expect(res.error?.key).toBe('issuing_quota');
      expect(res.error?.message).toContain('approval links sent to this address');
      expect(await usedAt(token)).toBeNull();
      expect((await requestRow(second)).status).toBe('open');
      // Rejecting signs nothing, so the quota does not stop it.
      expect((await callJson(quota, 'approvalDecide', { body: { token, decision: 'reject', note: 'Later.' } })).body).toEqual({ status: 'rejected' });
    } finally {
      await quota.close();
    }
  });

  test('a blocked address gets no approve link: a signed-in admin decides', async () => {
    const email = addr('blocked');
    await t.db`insert into blocked_emails (email, reason) values (${email}, 'test')`;
    const requestId = await formRequest(app, email);
    expect(await t.db`select 1 from request_approval_links where request_id = ${requestId}`).toHaveLength(0);
    const mail = app.mailer.to('admin@example.org').filter((m) => m.text.includes(requestId)).at(-1)!;
    expect(mail.text).not.toContain('/approve?token=');
    expect(mail.text).toContain('no approve link');
  });

  test('a renewal through the link renews the existing key', async () => {
    const email = addr('renewal');
    const first = await formRequest(app, email, 'Renewing Rae');
    const issued = await callJson(app, 'approvalDecide', { body: { token: linkFor(app, 'admin@example.org', first), decision: 'approve' } });
    const id = issued.body.licenseId!;
    const soon = addDays(todayUtc(), 10);
    // A personal license for fewer nodes than a new key gets: the renewal keeps them, and the mail and the page say so.
    await t.db`update licenses set expires_at = ${new Date(`${soon}T23:59:59Z`)}, nodes = 1 where id = ${id}`;
    await t.db`update license_keys set expires_at = ${new Date(`${soon}T23:59:59Z`)} where license_id = ${id}`;

    const second = await formRequest(app, email, 'Renewing Rae');
    const admin = app.mailer.to('admin@example.org').filter((m) => m.text.includes(second)).at(-1)!;
    expect(admin.subject).toBe(`Approve the renewal of ${id} for Renewing Rae?`);
    expect(admin.text).toContain('Nodes:      1');
    expect(admin.text).toContain(`Expires:    ${addDays(soon, 365)} (UTC) if approved by ${soon}`);
    const token = linkFor(app, 'admin@example.org', second);
    const preview = await callJson(app, 'approvalPreview', { body: { token } });
    expect(preview.body.renews).toEqual({ licenseId: id, expiresOn: soon });
    expect(preview.body.terms).toEqual({ plan: 'personal', nodes: 1, features: null, expiresOn: addDays(soon, 365) });
    app.mailer.clear();
    const res = await callJson(app, 'approvalDecide', { body: { token, decision: 'approve' } });
    expect(res.body).toEqual({ status: 'approved', licenseId: id, outcome: 'renewed' });
    expect((await t.db<{ reason: string }[]>`select reason from license_keys where license_id = ${id} order by issued_at`).map((k) => k.reason)).toEqual(['issue', 'renew']);
    expect((await t.db<{ nodes: number }[]>`select nodes from licenses where id = ${id}`)[0]!.nodes).toBe(1);
    const renewed = (await t.db<{ token: string }[]>`select token from license_keys where license_id = ${id} and superseded_at is null`)[0]!;
    expect((await verify(renewed.token, signing.keyring)).claims).toMatchObject({ nodes: 1 });
    expect(app.mailer.to(email)[0]!.subject).toBe('Your renewed kudurru license key');
    // Decided: the preview no longer offers a renewal.
    expect((await callJson(app, 'approvalPreview', { body: { token } })).body).toMatchObject({ status: 'approved', renews: null });
  });

  test('when the key cannot be signed the decision rolls back and the link stays usable', async () => {
    const requestId = await formRequest(app, addr('nosigner'));
    const token = linkFor(app, 'admin@example.org', requestId);
    const noKey = makeTestApp({ handlers, db: t.db, signing: signingUnavailable('no signing key is configured'), config: config() });
    try {
      const res = await callJson(noKey, 'approvalDecide', { body: { token, decision: 'approve' } });
      expect(res.status).toBe(503);
      expect(res.error?.key).toBe('signing_unavailable');
    } finally {
      await noKey.close();
    }
    expect(await usedAt(token)).toBeNull();
    expect((await requestRow(requestId)).status).toBe('open');
    expect((await callJson(app, 'approvalDecide', { body: { token, decision: 'approve' } })).body.status).toBe('approved');
  });

  test('both routes are rate-limited per address (30 a minute by default)', async () => {
    expect(DEFAULT_RATE_LIMITS.approvalPreview).toEqual({ max: 30, windowMs: 60_000 });
    expect(DEFAULT_RATE_LIMITS.approvalDecide).toEqual({ max: 30, windowMs: 60_000 });
    const log = new MemoryLogger();
    const cfg = config();
    const limited = makeTestApp({ handlers, db: t.db, signing, config: cfg, log, pipeline: createPipeline(cfg, log, { rateLimits: { approvalPreview: { max: 2, windowMs: 60_000 }, approvalDecide: { max: 2, windowMs: 60_000 } } }) });
    try {
      for (let i = 0; i < 2; i++) expect((await callJson(limited, 'approvalPreview', { body: { token: 'A'.repeat(43) } })).status).toBe(400);
      const res = await callJson(limited, 'approvalPreview', { body: { token: 'A'.repeat(43) } });
      expect(res.status).toBe(429);
      expect(res.error?.key).toBe('rate_limited');
      expect(res.error?.message).toContain('Too many requests');
      expect(Number(res.res.headers.get('retry-after'))).toBeGreaterThan(0);
      expect((await callJson(limited, 'approvalDecide', { body: { token: 'A'.repeat(43), decision: 'approve' } })).status).toBe(400);
    } finally {
      await limited.close();
    }
  });
});
