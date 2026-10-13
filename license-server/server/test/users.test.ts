// Users, roles and sessions (design spec §5, §10): the list and detail,
// granted roles and disable/enable with the guards (no removing your own
// superadmin grant, no disabling yourself, the last enabled superadmin
// keeps it), the target's sessions ended on every change but never the
// acting session, revoke-sessions, and the audit events.
import { afterAll, beforeAll, expect, test } from 'bun:test';
import { userHandlers } from '../api/users';
import { mergeHandlers } from '../http/router';
import { call, callJson, makeTestApp, type TestApp } from './call';
import { describeDb, hasDb, useTestDb as fileDatabase } from './db';
import { createCustomer, createUser, signInAs, type TestSession, type TestUser } from './fixtures';

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

  test('a license admin may read users but not change them', async () => {
    const reader = await signInAs(t.db, await createUser(t.db, { roles: ['license_admin'] }));
    expect((await call(app, 'usersList', { as: reader })).status).toBe(200);
    const res = await callJson(app, 'userUpdate', { as: reader, params: { id: root.id }, body: { disabled: true } });
    expect(res.status).toBe(403);
    expect(res.error?.hint).toEqual({ missing: 'users.write' });
  });
});
