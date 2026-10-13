// Members and invites (design spec §6 "Members and invites", §10
// "invites"): adding a member returns the invite link once and mails it;
// preview names the customer without changing anything; any signed-in
// person redeems a link once, within 14 days; a linked row is never
// re-linked or re-invited; removal ends the access at once; tokens are
// stored hashed and never audited; MEMBER_AUTO_LINK links at once.
import { afterAll, beforeAll, expect, test } from 'bun:test';
import { customerHandlers } from '../api/customers';
import { meHandlers } from '../api/me';
import { mergeHandlers } from '../http/router';
import { loadSigning } from '../licensing/keyring';
import type { SigningState } from '../licensing/types';
import { silentLogger } from '../log';
import { call, callJson, makeTestApp, type TestApp } from './call';
import { describeDb, hasDb, useTestDb as fileDatabase } from './db';
import { createCustomer, createUser, signInAs, testConfig, type TestSession } from './fixtures';

const handlers = mergeHandlers(customerHandlers, meHandlers);
// One fresh database for this file (useTestDb, renamed: it is not a React hook).
const t = fileDatabase();
let app: TestApp;
let signing: SigningState;
let admin: TestSession;

beforeAll(async () => {
  if (!hasDb) return;
  signing = await loadSigning(testConfig(), silentLogger);
  app = makeTestApp({ handlers, db: t.db, signing });
  admin = await signInAs(t.db, await createUser(t.db, { roles: ['license_admin'] }));
});
afterAll(async () => {
  await app?.close();
});

function tokenOf(url: string): string {
  return url.slice(url.lastIndexOf('/') + 1);
}

describeDb('members and invites', () => {
  test('add: the invite link once (stored hashed, mailed), status invited; a duplicate is 409', async () => {
    const c = await createCustomer(t.db, { name: 'Invite Org' });
    app.mailer.clear();
    const res = await callJson(app, 'memberAdd', { as: admin, params: { id: c.id }, body: { email: ' New.Member@Example.org ' } });
    expect(res.status).toBe(200);
    expect(res.body.member).toMatchObject({ email: 'new.member@example.org', status: 'invited', user: null, linkedAt: null });
    expect(res.body.inviteUrl).toMatch(/^http:\/\/localhost:8790\/invites\/[A-Za-z0-9_-]{43}$/);
    const token = tokenOf(res.body.inviteUrl);
    const exp = new Date(res.body.member.inviteExpiresAt!).getTime();
    expect(Math.round((exp - Date.now()) / 86_400_000)).toBe(14);
    expect(JSON.stringify(await t.db`select * from customer_members where customer_id = ${c.id}`)).not.toContain(token);
    expect(JSON.stringify(await t.db`select details from audit_events where target_id = ${c.id}`)).not.toContain(token);
    const mail = app.mailer.to('new.member@example.org');
    expect(mail).toHaveLength(1);
    expect(mail[0]!.subject).toBe('Access to the kudurru licenses of Invite Org');
    expect(mail[0]!.text).toContain(res.body.inviteUrl);
    const dup = await callJson(app, 'memberAdd', { as: admin, params: { id: c.id }, body: { email: 'new.member@example.org' } });
    expect(dup.status).toBe(409);
  });

  test('preview names the customer and changes nothing; redeem links the row once; a linked row is never re-linked', async () => {
    const c = await createCustomer(t.db, { name: 'Preview Co' });
    const added = (await callJson(app, 'memberAdd', { as: admin, params: { id: c.id }, body: { email: 'invitee@example.org' } })).body;
    const token = tokenOf(added.inviteUrl);
    const someone = await createUser(t.db, { email: 'different.address@example.org' });
    const s = await signInAs(t.db, someone);
    const preview = await callJson(app, 'myInvitePreview', { as: s, body: { token } });
    expect(preview.status).toBe(200);
    expect(preview.body).toMatchObject({ customer: { id: c.id, name: 'Preview Co' }, email: 'invitee@example.org', expired: false });
    expect((await t.db<{ user_id: string | null }[]>`select user_id from customer_members where customer_id = ${c.id}`)[0]!.user_id).toBeNull();

    const redeemed = await callJson(app, 'myInviteRedeem', { as: s, body: { token } });
    expect(redeemed.status).toBe(200);
    expect(redeemed.body).toEqual({ customer: { id: c.id, name: 'Preview Co' } });
    const row = (await t.db<{ user_id: string | null; invite_token_hash: Buffer | null; linked_at: Date | null }[]>`select user_id, invite_token_hash, linked_at from customer_members where customer_id = ${c.id}`)[0]!;
    expect(row.user_id).toBe(someone.id);
    expect(row.invite_token_hash).toBeNull();
    expect(row.linked_at).not.toBeNull();
    const audit = (await t.db<{ details: Record<string, unknown> }[]>`select details from audit_events where action = 'member.linked' and target_id = ${c.id}`)[0]!;
    expect(audit.details).toMatchObject({ email: 'invitee@example.org', userId: someone.id, issuer: someone.issuer, subject: someone.subject });

    // Used: preview and redeem are invalid_token, also for another user.
    const other = await signInAs(t.db, await createUser(t.db));
    expect((await callJson(app, 'myInvitePreview', { as: other, body: { token } })).error?.key).toBe('invalid_token');
    expect((await callJson(app, 'myInviteRedeem', { as: other, body: { token } })).error?.key).toBe('invalid_token');
    // A linked member gets no new invite.
    expect((await callJson(app, 'memberInvite', { as: admin, params: { id: c.id, email: 'invitee@example.org' } })).status).toBe(409);
    const detail = await callJson(app, 'customerGet', { as: admin, params: { id: c.id } });
    expect(detail.body.members[0]).toMatchObject({ status: 'linked', user: { id: someone.id } });
  });

  test('re-invite replaces the link; an expired link names the customer but cannot be redeemed', async () => {
    const c = await createCustomer(t.db, { name: 'Expiry Co' });
    const first = (await callJson(app, 'memberAdd', { as: admin, params: { id: c.id }, body: { email: 'late@example.org' } })).body;
    const second = (await callJson(app, 'memberInvite', { as: admin, params: { id: c.id, email: 'LATE@example.org' } })).body;
    expect(second.inviteUrl).not.toBe(first.inviteUrl);
    const s = await signInAs(t.db, await createUser(t.db));
    expect((await callJson(app, 'myInvitePreview', { as: s, body: { token: tokenOf(first.inviteUrl) } })).error?.key).toBe('invalid_token');
    await t.db`update customer_members set invite_expires_at = now() - interval '1 minute' where customer_id = ${c.id}`;
    const preview = await callJson(app, 'myInvitePreview', { as: s, body: { token: tokenOf(second.inviteUrl) } });
    expect(preview.body).toMatchObject({ customer: { name: 'Expiry Co' }, expired: true });
    const redeem = await callJson(app, 'myInviteRedeem', { as: s, body: { token: tokenOf(second.inviteUrl) } });
    expect(redeem.status).toBe(400);
    expect(redeem.error?.key).toBe('invalid_token');
  });

  test('a person who already has access through another address is told so (409)', async () => {
    const user = await createUser(t.db);
    const c = await createCustomer(t.db, { members: [{ email: user.email!, userId: user.id }] });
    const added = (await callJson(app, 'memberAdd', { as: admin, params: { id: c.id }, body: { email: 'second@example.org' } })).body;
    const res = await callJson(app, 'myInviteRedeem', { as: await signInAs(t.db, user), body: { token: tokenOf(added.inviteUrl) } });
    expect(res.status).toBe(409);
  });

  test('removing a member ends the access at once', async () => {
    const user = await createUser(t.db);
    const s = await signInAs(t.db, user);
    const c = await createCustomer(t.db, { members: [{ email: user.email!, userId: user.id }] });
    await t.db`
      insert into licenses (id, customer_id, licensee, plan, nodes, expires_at, source, created_by_label)
      values (${'lic_rm' + c.id.slice(4, 14)}, ${c.id}, 'X', 'business', 1, now() + interval '30 days', 'admin', 'test')`;
    await t.db`
      insert into license_keys (id, license_id, kid, token, claims, issued_at, expires_at, issued_by_label, reason)
      values (${'key_rm' + c.id.slice(4, 14)}, ${'lic_rm' + c.id.slice(4, 14)}, 'server-test-1', 'x.y.z', ${{}}, now(), now() + interval '30 days', 'test', 'issue')`;
    expect((await callJson(app, 'myLicenses', { as: s })).body.items).toHaveLength(1);
    expect((await call(app, 'memberRemove', { as: admin, params: { id: c.id, email: user.email! } })).status).toBe(204);
    expect((await callJson(app, 'myLicenses', { as: s })).body.items).toHaveLength(0);
    expect((await call(app, 'memberRemove', { as: admin, params: { id: c.id, email: user.email! } })).status).toBe(404);
  });

  test('MEMBER_AUTO_LINK: a new member row is linked at once to the one user with that verified email', async () => {
    const auto = makeTestApp({ handlers, db: t.db, signing, config: testConfig({ roles: { memberAutoLink: true } }) });
    try {
      const user = await createUser(t.db, { email: 'auto.link@example.org' });
      const c = await createCustomer(t.db);
      const res = await callJson(auto, 'memberAdd', { as: admin, params: { id: c.id }, body: { email: 'auto.link@example.org' } });
      expect(res.body.member).toMatchObject({ status: 'linked', user: { id: user.id } });
      expect(res.body.inviteUrl).toBe('http://localhost:8790/my/licenses');
      const nobody = await callJson(auto, 'memberAdd', { as: admin, params: { id: c.id }, body: { email: 'nobody.yet@example.org' } });
      expect(nobody.body.member.status).toBe('invited');
    } finally {
      await auto.close();
    }
  });
});
