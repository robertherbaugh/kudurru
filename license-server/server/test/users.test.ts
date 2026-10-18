// Users, roles and sessions (design spec §5, §10): the list and detail,
// granted roles and disable/enable with the guards (no removing your own
// superadmin grant, no disabling yourself, the last enabled superadmin
// keeps it), the target's sessions ended on every change but never the
// acting session, revoke-sessions, delete (sessions and memberships go,
// labels stay, not yourself, not the last enabled superadmin), and the
// audit events.
import { afterAll, beforeAll, expect, test } from 'bun:test';
import { userHandlers } from '../api/users';
import { textArray } from '../db/sql';
import { mergeHandlers } from '../http/router';
import { silentLogger } from '../log';
import { MemoryMailer } from '../mail/memory';
import { deleteUser } from '../services/users';
import { inTransaction } from '../services/types';
import { signingUnavailable } from '../licensing/types';
import { call, callJson, makeTestApp, type TestApp } from './call';
import { describeDb, hasDb, useTestDb as fileDatabase } from './db';
import { createCustomer, createUser, signInAs, testConfig, type TestSession, type TestUser } from './fixtures';

// One fresh database for this file (useTestDb, renamed: it is not a React hook).
const t = fileDatabase();
let app: TestApp;
let root: TestUser;
let rootSession: TestSession;

beforeAll(async () => {
  if (!hasDb) return;
  app = makeTestApp({ handlers: mergeHandlers(userHandlers), db: t.db });
  root = await createUser(t.db, { roles: ['superadmin'], name: 'Root One', email: 'root@users.example' });
  rootSession = await signInAs(t.db, root);
});
afterAll(async () => {
  await app?.close();
});

async function sessionsOf(userId: string): Promise<number> {
  return Number((await t.db<{ n: string }[]>`select count(*) as n from sessions where user_id = ${userId}`)[0]!.n);
}

describeDb('users', () => {
  test('list (q matches name and email) and detail with memberships and active sessions', async () => {
    const u = await createUser(t.db, { name: 'Listed Person', email: 'listed@users.example', idpRoles: ['license_admin'] });
    await signInAs(t.db, u);
    const c = await createCustomer(t.db, { name: 'Member Org', members: [{ email: u.email!, userId: u.id }] });
    const list = await callJson(app, 'usersList', { as: rootSession, query: { q: 'listed@' } });
    expect(list.body.items.map((x) => x.id)).toEqual([u.id]);
    const detail = await callJson(app, 'userGet', { as: rootSession, params: { id: u.id } });
    expect(detail.body).toMatchObject({ id: u.id, idpRoles: ['license_admin'], grantedRoles: ['customer'], roles: ['customer', 'license_admin'], disabled: false, activeSessions: 1 });
    expect(detail.body.memberships).toEqual([{ customer: { id: c.id, name: 'Member Org', kind: 'organization' }, email: u.email!, linkedAt: expect.any(String) as string }]);
    expect((await call(app, 'userGet', { as: rootSession, params: { id: 'usr_nobody' } })).status).toBe(404);
  });

  test('granting roles ends the target sessions (not the acting one) and is audited', async () => {
    const u = await createUser(t.db);
    await signInAs(t.db, u);
    await signInAs(t.db, u);
    const res = await callJson(app, 'userUpdate', { as: rootSession, params: { id: u.id }, body: { grantedRoles: ['license_admin', 'customer'] } });
    expect(res.status).toBe(200);
    expect(res.body.grantedRoles).toEqual(['customer', 'license_admin']);
    expect(res.body.activeSessions).toBe(0);
    expect(await sessionsOf(u.id)).toBe(0);
    expect(await sessionsOf(root.id)).toBeGreaterThan(0);
    const audit = (await t.db<{ details: Record<string, unknown> }[]>`select details from audit_events where action = 'user.roles_changed' and target_id = ${u.id}`)[0]!;
    expect(audit.details).toMatchObject({ from: ['customer'], to: ['customer', 'license_admin'], sessionsRevoked: 2 });
    // The same roles again change nothing.
    const same = await callJson(app, 'userUpdate', { as: rootSession, params: { id: u.id }, body: { grantedRoles: ['customer', 'license_admin'] } });
    expect(same.status).toBe(200);
    expect(await t.db`select 1 from audit_events where action = 'user.roles_changed' and target_id = ${u.id}`).toHaveLength(1);
  });

  test('disable and enable: sessions end, roles go while disabled', async () => {
    const u = await createUser(t.db, { roles: ['license_admin'] });
    await signInAs(t.db, u);
    const off = await callJson(app, 'userUpdate', { as: rootSession, params: { id: u.id }, body: { disabled: true } });
    expect(off.body).toMatchObject({ disabled: true, roles: [] });
    expect(await sessionsOf(u.id)).toBe(0);
    const on = await callJson(app, 'userUpdate', { as: rootSession, params: { id: u.id }, body: { disabled: false } });
    expect(on.body).toMatchObject({ disabled: false, disabledAt: null, roles: ['license_admin'] });
    const actions = (await t.db<{ action: string }[]>`select action from audit_events where target_id = ${u.id} order by id`).map((r) => r.action);
    expect(actions).toEqual(['user.disabled', 'user.enabled']);
  });

  test('guards: not your own superadmin grant, not yourself disabled, not the last enabled superadmin', async () => {
    const own = await callJson(app, 'userUpdate', { as: rootSession, params: { id: root.id }, body: { grantedRoles: ['customer'] } });
    expect(own.status).toBe(409);
    const self = await callJson(app, 'userUpdate', { as: rootSession, params: { id: root.id }, body: { disabled: true } });
    expect(self.status).toBe(409);

    // A second superadmin can demote the first only while another enabled superadmin remains.
    const second = await createUser(t.db, { roles: ['superadmin'] });
    const s2 = await signInAs(t.db, second);
    await t.db`update users set disabled_at = now() where 'superadmin' = any(granted_roles) and id not in (${root.id}, ${second.id})`;
    const demote = await callJson(app, 'userUpdate', { as: s2, params: { id: root.id }, body: { grantedRoles: ['customer'] } });
    expect(demote.status).toBe(200);
    // Now `second` is the last enabled superadmin: root (no longer superadmin) cannot act, and
    // nobody can take it from `second` through the API; an IdP superadmin role counts too.
    const idpSuper = await createUser(t.db, { roles: ['customer'], idpRoles: ['superadmin'] });
    await t.db`update users set disabled_at = now() where id = ${idpSuper.id}`;
    const last = await callJson(app, 'userUpdate', { as: s2, params: { id: second.id }, body: { grantedRoles: ['customer'] } });
    expect(last.status).toBe(409);
    await t.db`update users set disabled_at = null where id = ${idpSuper.id}`;
    const s3again = await signInAs(t.db, idpSuper);
    const nowOk = await callJson(app, 'userUpdate', { as: s3again, params: { id: second.id }, body: { disabled: true } });
    expect(nowOk.status).toBe(200);
    // Restore root for the other tests (the demotion ended its sessions).
    await t.db`update users set granted_roles = '{superadmin}' where id = ${root.id}`;
    expect(await sessionsOf(root.id)).toBe(0);
    rootSession = await signInAs(t.db, root);
  });

  test('revoke sessions: the target loses all, the acting session stays', async () => {
    const u = await createUser(t.db);
    await signInAs(t.db, u);
    const res = await callJson(app, 'userRevokeSessions', { as: rootSession, params: { id: u.id } });
    expect(res.body).toEqual({ revoked: 1 });
    expect(await sessionsOf(u.id)).toBe(0);
    await signInAs(t.db, root);
    const mine = await callJson(app, 'userRevokeSessions', { as: rootSession, params: { id: root.id } });
    expect(mine.body.revoked).toBeGreaterThanOrEqual(1);
    expect(await sessionsOf(root.id)).toBe(1);
    expect((await callJson(app, 'usersList', { as: rootSession })).status).toBe(200);
  });

  test('delete: the user, their sessions and memberships go; labels stay; audited with who it was', async () => {
    const u = await createUser(t.db, { name: 'Gone Person', email: 'gone@users.example', roles: ['license_admin'] });
    await signInAs(t.db, u);
    await signInAs(t.db, u);
    const member = await createCustomer(t.db, { name: 'Gone Org', members: [{ email: u.email!, userId: u.id }, 'other@users.example'] });
    const made = await createCustomer(t.db, { name: 'Made By Gone' });
    await t.db`update customers set created_by = ${u.id}, created_by_label = 'Gone Person' where id = ${made.id}`;

    const res = await call(app, 'userDelete', { as: rootSession, params: { id: u.id } });
    expect(res.status).toBe(204);
    expect((await call(app, 'userGet', { as: rootSession, params: { id: u.id } })).status).toBe(404);
    expect(await sessionsOf(u.id)).toBe(0);
    const members = await t.db<{ email: string }[]>`select email from customer_members where customer_id = ${member.id} order by email`;
    expect(members.map((m) => m.email)).toEqual(['other@users.example']);
    expect(await t.db`select created_by, created_by_label from customers where id = ${made.id}`).toEqual([{ created_by: null, created_by_label: 'Gone Person' }]);
    const audit = (await t.db<{ actor_id: string; details: Record<string, unknown> }[]>`select actor_id, details from audit_events where action = 'user.deleted' and target_id = ${u.id}`)[0]!;
    expect(audit.actor_id).toBe(root.id);
    expect(audit.details).toEqual({
      name: 'Gone Person',
      email: 'gone@users.example',
      issuer: u.issuer,
      subject: u.subject,
      roles: ['license_admin'],
      memberships: [{ customerId: member.id, email: 'gone@users.example' }],
      sessionsEnded: 2,
    });
    // Gone is gone.
    expect((await call(app, 'userDelete', { as: rootSession, params: { id: u.id } })).status).toBe(404);
  });

  test('delete guards: not yourself; not the last enabled superadmin (two superadmins deleting each other at once)', async () => {
    const self = await callJson(app, 'userDelete', { as: rootSession, params: { id: root.id } });
    expect(self.status).toBe(409);
    expect(self.error?.message).toContain('cannot delete yourself');

    const a = await createUser(t.db, { roles: ['superadmin'] });
    const b = await createUser(t.db, { roles: ['superadmin'] });
    // Only a and b are enabled superadmins; each one's DELETE of the other passed its permission check.
    const others = await t.db<{ id: string }[]>`
      update users set disabled_at = now()
       where disabled_at is null and ('superadmin' = any(granted_roles) or 'superadmin' = any(idp_roles)) and id not in (${a.id}, ${b.id})
      returning id`;
    const deps = { db: t.db, config: testConfig(), log: silentLogger, signing: signingUnavailable('not needed'), mailer: new MemoryMailer() };
    const actorOf = (x: TestUser) => ({ kind: 'user' as const, userId: x.id, label: x.name });
    try {
      await inTransaction(deps, actorOf(a), (s) => deleteUser(s, b.id, { userId: a.id, sessionHash: null }));
      const second = inTransaction(deps, actorOf(b), (s) => deleteUser(s, a.id, { userId: b.id, sessionHash: null }));
      expect(second).rejects.toMatchObject({ status: 409 });
      await second.catch(() => undefined);
      expect(await t.db`select 1 from users where id = ${a.id}`).toHaveLength(1);
    } finally {
      await t.db`update users set disabled_at = null where id = any(${textArray(t.db, others.map((o) => o.id))})`;
    }
  });

  test('a license admin may read users but not change them', async () => {
    const reader = await signInAs(t.db, await createUser(t.db, { roles: ['license_admin'] }));
    expect((await call(app, 'usersList', { as: reader })).status).toBe(200);
    const res = await callJson(app, 'userUpdate', { as: reader, params: { id: root.id }, body: { disabled: true } });
    expect(res.status).toBe(403);
    expect(res.error?.hint).toEqual({ missing: 'users.write' });
    const del = await callJson(app, 'userDelete', { as: reader, params: { id: root.id } });
    expect(del.status).toBe(403);
    expect(del.error?.hint).toEqual({ missing: 'users.write' });
  });
});
