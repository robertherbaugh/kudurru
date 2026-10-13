// The least-privilege database roles (design spec §14; README "Database
// roles"; deploy/postgres-roles.sql; migrations 0002 and 0003): a database
// owned by license_portal_owner, migrated by it, and every route of the
// portal run through the real handlers connected as license_portal_app,
// the only role a production container holds (automatic approval and the
// default, approval required, with its one-click link). A grant the code
// needs and the migrations lack shows up here as a 500 with "permission
// denied" in the log; the second test proves what the runtime role must
// never do. The commented grant block of deploy/postgres-roles.sql must
// stay identical to the migrations' grants.
import { afterAll, beforeAll, describe, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { addDays, todayUtc } from '../../shared/dates';
import { handlers } from '../api';
import { migrate } from '../db/migrate';
import { connect, type Db } from '../db/sql';
import { cleanup } from '../jobs';
import { loadSigning } from '../licensing/keyring';
import type { SigningState } from '../licensing/types';
import { MemoryLogger, silentLogger } from '../log';
import { MIGRATIONS } from '../migrations';
import { callJson, makeTestApp, type TestApp } from './call';
import { databaseUrl, describeDb, hasDb, settle, TEST_DATABASE_URL } from './db';
import { createUser, GOLDEN_VECTORS, signInAs, testConfig } from './fixtures';

const OWNER = 'license_portal_owner';
const APP = 'license_portal_app';
const PASSWORD = 'kls-roles-test';

function grantLines(sql: string): string[] {
  return sql
    .split('\n')
    .map((l) => l.replace(/^\s*--\s?/, '').trim())
    .filter((l) => /^grant .* to license_portal_app;$/.test(l) && !/ on (schema|database) /.test(l));
}

describe('the grants', () => {
  test('deploy/postgres-roles.sql repeats the grants of the migrations (0002, 0003), grant for grant', () => {
    const migrations = MIGRATIONS.map((m) => m.sql).join('\n');
    const setup = readFileSync(join(import.meta.dir, '..', '..', 'deploy', 'postgres-roles.sql'), 'utf8');
    expect(grantLines(migrations).length).toBeGreaterThan(0);
    expect(grantLines(migrations)).toContain('grant select, insert, update, delete on request_approval_links to license_portal_app;');
    expect(grantLines(setup)).toEqual(grantLines(migrations));
  });
});

function roleUrl(role: string, db: string): string {
  const url = new URL(databaseUrl(db));
  url.username = role;
  url.password = PASSWORD;
  return url.toString();
}

const dbName = `kls_roles_${process.pid}_${Math.random().toString(36).slice(2, 8)}`;
let admin: Db;
let appDb: Db;
let signing: SigningState;
let app: TestApp;
const log = new MemoryLogger();

beforeAll(async () => {
  if (!hasDb) return;
  admin = connect({ url: TEST_DATABASE_URL, tls: 'disable', max: 1 });
  // Roles are global to the server: create them once, with a password for
  // servers that ask for one (the CI service does), as postgres-roles.sql does.
  for (const role of [OWNER, APP]) {
    await admin.unsafe(`do $$ begin
      if not exists (select 1 from pg_roles where rolname = '${role}') then create role ${role} login password '${PASSWORD}'; end if;
    end $$`);
  }
  await admin.unsafe(`create database "${dbName}" owner ${OWNER}`);
  await admin.unsafe(`grant connect on database "${dbName}" to ${APP}`);
  const owner = connect({ url: roleUrl(OWNER, dbName), tls: 'disable', max: 1 });
  try {
    await migrate(owner, { log: silentLogger });
  } finally {
    await owner.close();
  }
  appDb = connect({ url: roleUrl(APP, dbName), tls: 'disable', max: 4 });
  signing = await loadSigning(testConfig(), silentLogger);
  app = makeTestApp({ handlers, db: appDb, signing, log, config: testConfig({ personal: { autoApprove: true } }) });
});

afterAll(async () => {
  if (!hasDb) return;
  await app?.close();
  await appDb?.close();
  await admin.unsafe(`drop database if exists "${dbName}" with (force)`);
  await admin.close();
});

const inDays = (n: number) => addDays(todayUtc(), n);

describeDb('as license_portal_app', () => {
  test('every route the portal serves works with the runtime grants alone', async () => {
    const superadmin = await createUser(appDb, { roles: ['superadmin'] });
    const admin = await createUser(appDb, { roles: ['license_admin'] });
    const customer = await createUser(appDb, { roles: ['customer'], email: `customer-${process.pid}@roles.example` });
    const su = await signInAs(appDb, superadmin);
    const as = await signInAs(appDb, admin);
    const cu = await signInAs(appDb, customer);
    const ok = (status: number) => expect(status).toBeLessThan(500);

    // Issue, read, renew, change, email, download.
    const issued = await callJson(app, 'licenseIssue', {
      as,
      body: { newCustomer: { name: 'Roles GmbH', kind: 'organization', contactEmail: 'ops@roles.example' }, licensee: 'Roles GmbH', plan: 'business', nodes: 5, features: null, expiresOn: inDays(365) },
    });
    expect(issued.status).toBe(200);
    const id = issued.body.id;
    ok((await callJson(app, 'licensesList', { as })).status);
    const detail = await callJson(app, 'licenseGet', { as, params: { id } });
    expect(detail.status).toBe(200);
    expect((await callJson(app, 'licenseRenew', { as, params: { id }, body: { expiresOn: inDays(400) } })).status).toBe(200);
    expect((await callJson(app, 'licenseChange', { as, params: { id }, body: { nodes: 7 } })).status).toBe(200);
    expect((await callJson(app, 'licenseEmail', { as, params: { id } })).status).toBe(200);
    const keyId = (await callJson(app, 'licenseGet', { as, params: { id } })).body.keys[0]!.id;
    expect((await app.fetch(`/api/v1/licenses/${id}/keys/${keyId}/download`, { headers: { cookie: as.cookie } })).status).toBe(200);

    // Customers and members.
    const customerId = detail.body.customer.id;
    ok((await callJson(app, 'customersList', { as })).status);
    expect((await callJson(app, 'customerUpdate', { as, params: { id: customerId }, body: { notes: 'least privilege' } })).status).toBe(200);
    expect((await callJson(app, 'memberAdd', { as, params: { id: customerId }, body: { email: 'member@roles.example' } })).status).toBe(200);
    expect((await callJson(app, 'memberInvite', { as, params: { id: customerId, email: 'member@roles.example' } })).status).toBe(200);
    expect((await callJson(app, 'memberRemove', { as, params: { id: customerId, email: 'member@roles.example' } })).status).toBe(204);

    // The customer's own pages and the personal flow.
    const personal = await callJson(app, 'myRequestCreate', { as: cu, body: { name: 'Roles Customer', use: 'homelab', noncommercial: true } });
    expect(personal.status).toBe(200);
    expect(personal.body.outcome).toBe('issued');
    const mine = personal.body.licenseId!;
    ok((await callJson(app, 'myLicenses', { as: cu })).status);
    expect((await callJson(app, 'myLicense', { as: cu, params: { id: mine } })).status).toBe(200);
    expect((await app.fetch(`/api/v1/me/licenses/${mine}/key`, { headers: { cookie: cu.cookie } })).status).toBe(200);
    ok((await callJson(app, 'myRequests', { as: cu })).status);

    // The public form and its confirmation.
    app.mailer.clear();
    const formEmail = `form-${process.pid}@roles.example`;
    expect((await callJson(app, 'publicKeyRequest', { body: { name: 'Form Person', email: formEmail, use: 'homelab', noncommercial: true, website: '' } })).status).toBe(202);
    const token = /\/verify\?token=([A-Za-z0-9_-]{43})/.exec(app.mailer.to(formEmail)[0]!.text)![1]!;
    expect((await callJson(app, 'publicKeyRequestVerify', { body: { token } })).body.outcome).toBe('issued');

    // Approval required (the default): the form, the approvers' link, its preview and decision.
    const approving = makeTestApp({ handlers, db: appDb, signing, log, config: testConfig() });
    try {
      const waitEmail = `wait-${process.pid}@roles.example`;
      expect((await callJson(approving, 'publicKeyRequest', { body: { name: 'Waiting Person', email: waitEmail, use: 'homelab', noncommercial: true, website: '' } })).status).toBe(202);
      const confirm = /\/verify\?token=([A-Za-z0-9_-]{43})/.exec(approving.mailer.to(waitEmail)[0]!.text)![1]!;
      expect((await callJson(approving, 'publicKeyRequestVerify', { body: { token: confirm } })).body.outcome).toBe('pending_review');
      const link = /\/approve\?token=([A-Za-z0-9_-]{43})/.exec(approving.mailer.to('admin@example.org')[0]!.text)![1]!;
      expect((await callJson(approving, 'approvalPreview', { body: { token: link } })).status).toBe(200);
      expect((await callJson(approving, 'approvalDecide', { body: { token: link, decision: 'approve' } })).body.status).toBe('approved');
      expect((await callJson(approving, 'catalog', { as: cu })).body.approvalRequired).toBe(true);
    } finally {
      await approving.close();
    }

    // Withdraw with blocking, a request that waits for review, approve and reject, the block list.
    expect((await callJson(app, 'licenseWithdraw', { as, params: { id: mine }, body: { reason: 'test', blockPersonal: true } })).status).toBe(200);
    const pending = await callJson(app, 'myRequestCreate', { as: cu, body: { name: 'Roles Customer', use: 'homelab', noncommercial: true } });
    expect(pending.body.outcome).toBe('pending_review');
    const requests = await callJson(app, 'requestsList', { as });
    expect(requests.status).toBe(200);
    expect((await callJson(app, 'requestReject', { as, params: { id: pending.body.requestId! }, body: { note: 'no' } })).status).toBe(200);
    ok((await callJson(app, 'blockedEmailsList', { as })).status);
    expect((await callJson(app, 'blockedEmailRemove', { as: su, params: { email: customer.email! } })).status).toBe(204);
    const again = await callJson(app, 'myRequestCreate', { as: cu, body: { name: 'Roles Customer', use: 'homelab', noncommercial: true } });
    expect(again.body.outcome).toBe('pending_review');
    const approved = await callJson(app, 'requestApprove', { as, params: { id: again.body.requestId! }, body: {} });
    expect(approved.status).toBe(200);

    // Import a key signed by hand.
    expect((await callJson(app, 'licenseImport', { as, body: { token: GOLDEN_VECTORS[0]!.key, customerId } })).status).toBe(200);

    // Users, sessions, audit, system.
    ok((await callJson(app, 'usersList', { as: su })).status);
    ok((await callJson(app, 'userGet', { as: su, params: { id: admin.id } })).status);
    expect((await callJson(app, 'userUpdate', { as: su, params: { id: customer.id }, body: { grantedRoles: ['customer', 'license_admin'] } })).status).toBe(200);
    expect((await callJson(app, 'userRevokeSessions', { as: su, params: { id: customer.id } })).status).toBe(200);
    ok((await callJson(app, 'auditList', { as: su })).status);
    expect((await callJson(app, 'system', { as: su })).status).toBe(200);
    expect((await callJson(app, 'authMe', { as: su })).status).toBe(200);
    expect((await callJson(app, 'authLogout', { as: su })).status).toBe(204);

    // Sign-in state and the cleanup job (a year on, so it deletes and anonymizes).
    await appDb`insert into oidc_logins (state, binding_hash, nonce, code_verifier, next, created_at, expires_at)
                values ('roles-state', ${Buffer.alloc(32)}, 'n', 'v', '/', now(), now() + interval '10 minutes')`;
    const swept = await cleanup(appDb, new Date(Date.now() + 400 * 86_400_000));
    expect(swept.logins).toBeGreaterThanOrEqual(1);
    expect(swept.approvalLinks).toBeGreaterThanOrEqual(1);

    expect(log.entries.filter((e) => JSON.stringify(e).includes('permission denied'))).toEqual([]);
  });

  test('what the runtime role can never do', async () => {
    const denied = async (q: PromiseLike<unknown>) => {
      const err = await settle(q).then(
        () => null,
        (e: unknown) => e as { errno?: string; message?: string },
      );
      expect(err).not.toBeNull();
      // 42501 insufficient_privilege, or the append-only trigger (P0001) where the privilege exists.
      expect(['42501', 'P0001']).toContain(err!.errno ?? '');
    };
    await denied(appDb`create table intruder (i int)`);
    await denied(appDb`truncate audit_events`);
    await denied(appDb`update audit_events set action = 'rewritten'`);
    await denied(appDb`delete from audit_events`);
    await denied(appDb`alter table audit_events disable trigger all`);
    await denied(appDb`delete from licenses`);
    await denied(appDb`delete from license_keys`);
    await denied(appDb`delete from users`);
    await denied(appDb`insert into schema_migrations (version, name) values (999, 'intruder')`);
    await denied(appDb`truncate sessions`);
    await denied(appDb`drop table blocked_emails`);
  });
});
