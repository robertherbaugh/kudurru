// The audit log API and its guarantees (design spec §4, §8 GET /audit, §10
// "audit append-only"): newest first, filters (action, target, actor),
// keyset paging with `next`/`before`, only for audit.read; the table
// refuses UPDATE, DELETE and TRUNCATE; and after real actions no detail
// holds a key token.
import { afterAll, beforeAll, expect, test } from 'bun:test';
import { AUDIT_TARGET_TYPES } from '../../shared/api';
import { addDays, todayUtc } from '../../shared/dates';
import { auditHandlers } from '../api/audit';
import { customerHandlers } from '../api/customers';
import { licenseHandlers } from '../api/licenses';
import { mergeHandlers } from '../http/router';
import { loadSigning } from '../licensing/keyring';
import { containsToken, silentLogger } from '../log';
import { call, callJson, makeTestApp, type TestApp } from './call';
import { describeDb, hasDb, settle, useTestDb as fileDatabase } from './db';
import { createUser, signInAs, testConfig, type TestSession } from './fixtures';

// One fresh database for this file (useTestDb, renamed: it is not a React hook).
const t = fileDatabase();
let app: TestApp;
let admin: TestSession;
let adminId: string;
let licenseId = '';

beforeAll(async () => {
  if (!hasDb) return;
  app = makeTestApp({ handlers: mergeHandlers(auditHandlers, licenseHandlers, customerHandlers), db: t.db, signing: await loadSigning(testConfig(), silentLogger) });
  const u = await createUser(t.db, { roles: ['license_admin'] });
  adminId = u.id;
  admin = await signInAs(t.db, u);
  const issued = await callJson(app, 'licenseIssue', {
    as: admin,
    body: { newCustomer: { name: 'Audit Co', kind: 'organization', contactEmail: 'audit@co.example' }, licensee: 'Audit Co', plan: 'business', nodes: 3, features: null, expiresOn: addDays(todayUtc(), 60) },
  });
  licenseId = issued.body.id;
  await callJson(app, 'licenseRenew', { as: admin, params: { id: licenseId }, body: { expiresOn: addDays(todayUtc(), 120) } });
  await call(app, 'licenseKeyDownload', { as: admin, params: { id: licenseId, keyId: issued.body.currentKey.id } });
  await callJson(app, 'licenseEmail', { as: admin, params: { id: licenseId }, body: {} });
});
afterAll(async () => {
  await app?.close();
});

describeDb('audit API', () => {
  test('newest first, with actor, target, details, ip and request id', async () => {
    const res = await callJson(app, 'auditList', { as: admin, query: { targetType: 'license', targetId: licenseId } });
    expect(res.status).toBe(200);
    expect(res.body.items.map((e) => e.action)).toEqual(['license.key_emailed', 'license.key_downloaded', 'license.renewed', 'license.issued']);
    const e = res.body.items[3]!;
    expect(e).toMatchObject({ actor: { id: adminId }, targetType: 'license', targetId: licenseId, ip: '127.0.0.1' });
    expect(e.requestId).toMatch(/^[0-9a-f-]{36}$/);
    expect(Number(res.body.items[0]!.id)).toBeGreaterThan(Number(e.id));
    expect(res.body.next).toBeUndefined();
  });

  test('filters by action and actor; keyset paging with next/before', async () => {
    const byAction = await callJson(app, 'auditList', { as: admin, query: { action: 'customer.created' } });
    expect(byAction.body.items.every((e) => e.action === 'customer.created')).toBe(true);
    const byActor = await callJson(app, 'auditList', { as: admin, query: { actorId: adminId } });
    expect(byActor.body.items.length).toBeGreaterThanOrEqual(5);
    const first = await callJson(app, 'auditList', { as: admin, query: { actorId: adminId, limit: 2 } });
    expect(first.body.items).toHaveLength(2);
    expect(first.body.next).toBe(first.body.items[1]!.id);
    const second = await callJson(app, 'auditList', { as: admin, query: { actorId: adminId, limit: 2, before: first.body.next! } });
    expect(Number(second.body.items[0]!.id)).toBeLessThan(Number(first.body.items[1]!.id));
    const all = [...first.body.items, ...second.body.items].map((e) => e.id);
    expect(new Set(all).size).toBe(4);
    expect((await callJson(app, 'auditList', { as: admin, query: { before: 'x' } })).status).toBe(400);
    expect((await callJson(app, 'auditList', { as: admin, query: { limit: 500 } })).status).toBe(400);
    expect((await callJson(app, 'auditList', { as: admin, query: { targetType: 'nope' } })).status).toBe(400);
  });

  test('every target type the portal audits can be filtered by (business requests and Order Forms included)', async () => {
    for (const targetType of AUDIT_TARGET_TYPES) {
      const res = await callJson(app, 'auditList', { as: admin, query: { targetType, targetId: 'none-such' } });
      expect(`${targetType} ${res.status}`).toBe(`${targetType} 200`);
      expect(res.body.items).toEqual([]);
    }
    await t.db`insert into audit_events (actor_label, action, target_type, target_id, details) values ('test', 'order_form.created', 'order_form', 'ord_audit_filter', '{}')`;
    const res = await callJson(app, 'auditList', { as: admin, query: { targetType: 'order_form', targetId: 'ord_audit_filter' } });
    expect(res.body.items.map((e) => [e.action, e.targetType, e.targetId])).toEqual([['order_form.created', 'order_form', 'ord_audit_filter']]);
  });

  test('only for audit.read', async () => {
    const customer = await signInAs(t.db, await createUser(t.db));
    const res = await callJson(app, 'auditList', { as: customer });
    expect(res.status).toBe(403);
    expect(res.error?.hint).toEqual({ missing: 'audit.read' });
  });

  test('append-only: UPDATE, DELETE and TRUNCATE are refused', async () => {
    await expect(settle(t.db`update audit_events set actor_label = 'x'`)).rejects.toThrow('append-only');
    await expect(settle(t.db`delete from audit_events`)).rejects.toThrow('append-only');
    await expect(settle(t.db`truncate audit_events`)).rejects.toThrow('append-only');
  });

  test('no audit detail holds a key token', async () => {
    const rows = await t.db<{ details: unknown }[]>`select details from audit_events`;
    expect(rows.length).toBeGreaterThan(4);
    for (const r of rows) expect(containsToken(JSON.stringify(r.details))).toBe(false);
  });
});
