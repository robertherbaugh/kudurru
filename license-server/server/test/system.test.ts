// The System page and the catalog (design spec §7 /system, §8): what the
// portal runs with, from the configuration and the signing state, keys
// signed per kid, who approves personal keys (ADMIN_NOTIFY_EMAIL, or the
// enabled admins with a verified email), no secrets anywhere in the
// answer; the catalog's plans, features, policy, signing and mail flags,
// today (UTC) and whether approval is required; both read "unavailable"
// with the reason when no key is loaded.
import { afterAll, beforeAll, expect, test } from 'bun:test';
import { FEATURES, PLANS, POLICY } from '../../shared/catalog';
import { addDays, todayUtc } from '../../shared/dates';
import { catalogHandlers } from '../api/catalog';
import { customerHandlers } from '../api/customers';
import { licenseHandlers } from '../api/licenses';
import { systemHandlers } from '../api/system';
import { mergeHandlers } from '../http/router';
import { loadSigning } from '../licensing/keyring';
import { signingUnavailable } from '../licensing/types';
import { silentLogger } from '../log';
import { MemoryMailer } from '../mail/memory';
import { call, callJson, makeTestApp, type TestApp } from './call';
import { describeDb, hasDb, useTestDb as fileDatabase } from './db';
import { createUser, GOLDEN, signInAs, testConfig, type TestSession } from './fixtures';

const handlers = mergeHandlers(systemHandlers, catalogHandlers, licenseHandlers, customerHandlers);
// One fresh database for this file (useTestDb, renamed: it is not a React hook).
const t = fileDatabase();
let app: TestApp;
let admin: TestSession;
const config = testConfig({
  mail: { smtpUrl: 'smtp://mailer:hunter2-smtp@mail.example:587' },
  oidc: { clientSecret: 'oidc-client-secret-xyz', adminClaimRules: [{ claim: 'email_verified', value: 'true' }] },
  roles: { groups: { superadmin: ['license-superadmins'], license_admin: ['license-admins'], customer: [] } },
  signing: { trustedSince: 'v0.1.0' },
  build: { version: '1.2.3', commit: 'abc123', builtAt: '2026-10-12T00:00:00Z' },
});

beforeAll(async () => {
  if (!hasDb) return;
  app = makeTestApp({ handlers, db: t.db, config, signing: await loadSigning(config, silentLogger) });
  admin = await signInAs(t.db, await createUser(t.db, { roles: ['license_admin'] }), config);
  for (let i = 0; i < 2; i++) {
    await callJson(app, 'licenseIssue', { as: admin, body: { newCustomer: { name: `Sys ${i}`, kind: 'organization' }, licensee: `Sys ${i}`, plan: 'business', nodes: 1, features: null, expiresOn: addDays(todayUtc(), 30) } });
  }
});
afterAll(async () => {
  await app?.close();
});

describeDb('system and catalog', () => {
  test('GET /system: signing, sign-in, mail, public form, policy and build; no secrets', async () => {
    const res = await call(app, 'system', { as: admin });
    expect(res.status).toBe(200);
    const text = await res.text();
    const body = JSON.parse(text) as Record<string, unknown>;
    expect(body).toMatchObject({
      devMode: true,
      signing: { available: true, kid: GOLDEN.kid, trusted: true, trustedSince: 'v0.1.0', testKey: true, unavailableReason: null, trustedKids: [GOLDEN.kid], keysByKid: [{ kid: GOLDEN.kid, count: 2 }] },
      signIn: { issuer: config.oidc.issuer, clientId: 'license-portal', groupsClaim: 'groups', roleMappings: { superadmin: ['license-superadmins'], license_admin: ['license-admins'], customer: [] }, adminClaimRules: [{ claim: 'email_verified', value: 'true' }], defaultRole: 'customer', memberAutoLink: false },
      sessions: { idleTimeoutSeconds: 1800, lifetimeSeconds: 43200 },
      mail: { configured: true, from: config.mail.from, replyTo: 'hello@kudurru.io', adminNotify: 'admin@example.org' },
      publicForm: { enabled: true, origins: ['https://kudurru.io'], limitPerIpPerHour: 5, limitPerEmailPerDay: 3, personalAutoApprove: false, personalAutoApproveMaxPerDay: 50 },
      issuing: { maxPerActorPerDay: 50 },
      approval: { required: true, recipients: ['admin@example.org'], fromAdmins: false },
      policy: { ...POLICY },
      build: { version: '1.2.3', commit: 'abc123', builtAt: '2026-10-12T00:00:00Z' },
    });
    for (const secret of ['hunter2-smtp', 'oidc-client-secret-xyz', GOLDEN.seed, config.database.url]) expect(text).not.toContain(secret);
  });

  test('GET /catalog: lists, policy, signing, mail and today', async () => {
    const customer = await signInAs(t.db, await createUser(t.db), config);
    const res = await callJson(app, 'catalog', { as: customer });
    expect(res.status).toBe(200);
    expect(res.body).toEqual({
      plans: [...PLANS],
      features: [...FEATURES],
      policy: { ...POLICY },
      signing: { available: true, kid: GOLDEN.kid, trustedSince: 'v0.1.0', testKey: true },
      mail: { configured: true },
      today: todayUtc(),
      approvalRequired: true,
    });
  });

  test('approval: without ADMIN_NOTIFY_EMAIL the approvers are the enabled admins with a verified email; automatic approval says so', async () => {
    await createUser(t.db, { roles: ['superadmin'], email: 'sys-super@example.org' });
    await createUser(t.db, { roles: ['customer'], idpRoles: ['license_admin'], email: 'sys-idp-admin@example.org' });
    await createUser(t.db, { roles: ['license_admin'], email: null });
    await createUser(t.db, { roles: ['license_admin'], email: 'sys-disabled@example.org', disabled: true });
    await createUser(t.db, { roles: ['customer'], email: 'sys-customer@example.org' });
    const fallback = testConfig({ ...config, mail: { ...config.mail, adminNotify: [] }, personal: { autoApprove: true } });
    const other = makeTestApp({ handlers, db: t.db, config: fallback, signing: await loadSigning(fallback, silentLogger) });
    try {
      const sys = await callJson(other, 'system', { as: admin });
      expect(sys.body.mail.adminNotify).toBeNull();
      expect(sys.body.approval.required).toBe(false);
      expect(sys.body.approval.fromAdmins).toBe(true);
      expect(sys.body.approval.recipients).toContain('sys-super@example.org');
      expect(sys.body.approval.recipients).toContain('sys-idp-admin@example.org');
      for (const no of ['sys-disabled@example.org', 'sys-customer@example.org']) expect(sys.body.approval.recipients).not.toContain(no);
      expect((await callJson(other, 'catalog', { as: admin })).body.approvalRequired).toBe(false);
    } finally {
      await other.close();
    }
  });

  test('without a signer and mail: unavailable with the reason', async () => {
    const off = makeTestApp({ handlers, db: t.db, config, signing: signingUnavailable('no signing key is configured', new Map([[GOLDEN.kid, GOLDEN.publicKey]])), mailer: new MemoryMailer(false) });
    try {
      const sys = await callJson(off, 'system', { as: admin });
      expect(sys.body.signing).toMatchObject({ available: false, kid: null, trusted: false, testKey: false, unavailableReason: 'no signing key is configured', trustedKids: [GOLDEN.kid] });
      expect(sys.body.mail.configured).toBe(false);
      const cat = await callJson(off, 'catalog', { as: admin });
      expect(cat.body.signing).toEqual({ available: false, testKey: false });
      expect(cat.body.mail).toEqual({ configured: false });
    } finally {
      await off.close();
    }
  });

  test('a customer cannot read /system', async () => {
    const customer = await signInAs(t.db, await createUser(t.db), config);
    const res = await callJson(app, 'system', { as: customer });
    expect(res.status).toBe(403);
    expect(res.error?.hint).toEqual({ missing: 'system.read' });
  });
});
