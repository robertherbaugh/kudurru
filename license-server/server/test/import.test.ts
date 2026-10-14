// Importing keys signed before the portal existed (design spec §6 "Import"):
// verified against the trusted keyring (signature and Validate shape, as
// the nodes do, whitespace removed), an existing jti refused, the license
// created from the claims with one key (reason import, the token as given
// minus whitespace), nbf and unknown features kept, values the portal
// cannot hold refused, and renewing an import re-runs the issue policy.
import { afterAll, beforeAll, expect, test } from 'bun:test';
import { customerHandlers } from '../api/customers';
import { licenseHandlers } from '../api/licenses';
import { mergeHandlers } from '../http/router';
import { base64url, encodeHeader, importSigningKey } from '../licensing/jws';
import { loadSigning } from '../licensing/keyring';
import type { SigningState } from '../licensing/types';
import { silentLogger } from '../log';
import { callJson, makeTestApp, type TestApp } from './call';
import { describeDb, hasDb, useTestDb as fileDatabase } from './db';
import { createCustomer, createUser, GOLDEN, GOLDEN_VECTORS, signInAs, testConfig, type TestSession } from './fixtures';

// One fresh database for this file (useTestDb, renamed: it is not a React hook).
const t = fileDatabase();
let app: TestApp;
let signing: SigningState;
let admin: TestSession;

beforeAll(async () => {
  if (!hasDb) return;
  signing = await loadSigning(testConfig(), silentLogger);
  app = makeTestApp({ handlers: mergeHandlers(licenseHandlers, customerHandlers), db: t.db, signing });
  admin = await signInAs(t.db, await createUser(t.db, { roles: ['license_admin'] }));
});
afterAll(async () => {
  await app?.close();
});

/** A key signed by the golden (trusted) key with this payload text. */
async function signed(payload: string, kid = GOLDEN.kid): Promise<string> {
  const key = await importSigningKey(new Uint8Array(Buffer.from(GOLDEN.seed, 'base64')));
  const input = `${base64url(encodeHeader(kid))}.${base64url(payload)}`;
  return `${input}.${base64url(new Uint8Array(await crypto.subtle.sign('Ed25519', key, new TextEncoder().encode(input))))}`;
}

const future = Math.floor(Date.now() / 1000) + 200 * 86_400;

describeDb('import', () => {
  test('a licensegen key: the license from its claims, one key (reason import), the token stored clean', async () => {
    const c = await createCustomer(t.db);
    const vector = GOLDEN_VECTORS[1]!;
    const wrapped = vector.key.match(/.{1,60}/g)!.join('\r\n') + '\n';
    const res = await callJson(app, 'licenseImport', { as: admin, body: { token: wrapped, customerId: c.id } });
    expect(res.status).toBe(200);
    expect(res.body).toMatchObject({ id: vector.claims.jti, licensee: 'Example GmbH', plan: 'business', nodes: 25, features: ['oidc', 'edge', 'audit'], source: 'import', expiresOn: '2027-10-13' });
    expect(res.body.keys).toHaveLength(1);
    expect(res.body.keys[0]).toMatchObject({ reason: 'import', kid: GOLDEN.kid, issuedAt: new Date(vector.claims.iat * 1000).toISOString() });
    expect(res.body.currentKey.token).toBe(vector.key);
    const audit = (await t.db<{ details: Record<string, unknown> }[]>`select details from audit_events where action = 'license.imported' and target_id = ${vector.claims.jti}`)[0]!;
    expect(audit.details).toMatchObject({ customerId: c.id, kid: GOLDEN.kid, plan: 'business' });
    expect(JSON.stringify(audit.details)).not.toContain(vector.key.slice(-30));

    const again = await callJson(app, 'licenseImport', { as: admin, body: { token: vector.key, customerId: c.id } });
    expect(again.status).toBe(409);
    expect(again.error?.key).toBe('conflict');
  });

  test('keeps an nbf and unknown features as signed; renewing such an import re-runs the policy', async () => {
    const c = await createCustomer(t.db);
    const token = await signed(`{"jti":"lic_imp_unknownf","iss":"kudurru","sub":"Old Co","plan":"business","nodes":4,"features":["teleport","oidc"],"iat":1700000000,"nbf":1700000000,"exp":${future}}`);
    const res = await callJson(app, 'licenseImport', { as: admin, body: { token, customerId: c.id } });
    expect(res.status).toBe(200);
    expect(res.body.features).toEqual(['teleport', 'oidc']);
    const audit = (await t.db<{ details: Record<string, unknown> }[]>`select details from audit_events where action = 'license.imported' and target_id = 'lic_imp_unknownf'`)[0]!;
    expect(audit.details.nbf).toBe(1700000000);
    const renew = await callJson(app, 'licenseRenew', { as: admin, params: { id: 'lic_imp_unknownf' }, body: { expiresOn: new Date((future + 30 * 86_400) * 1000).toISOString().slice(0, 10) } });
    expect(renew.status).toBe(400);
    expect(renew.error?.fields).toEqual(['features']);
    // Change picks known features and signs a portal key.
    const change = await callJson(app, 'licenseChange', { as: admin, params: { id: 'lic_imp_unknownf' }, body: { features: ['oidc'] } });
    expect(change.status).toBe(200);
    expect(change.body.keys.map((k) => k.reason)).toEqual(['change', 'import']);
  });

  test('an expired key is still a record and imports (state expired)', async () => {
    const c = await createCustomer(t.db);
    const res = await callJson(app, 'licenseImport', { as: admin, body: { token: GOLDEN_VECTORS[0]!.key, customerId: c.id } });
    // GOLDEN_VECTORS[0] has the jti lic_AAAAAAAAAAAA shared by other vectors: import it once.
    expect(res.status).toBe(200);
    expect(['active', 'expiring', 'grace', 'expired']).toContain(res.body.state);
    const old = await signed(`{"jti":"lic_imp_expired1","iss":"kudurru","sub":"Old","plan":"trial","nodes":1,"iat":1600000000,"exp":1600086400}`);
    const expired = await callJson(app, 'licenseImport', { as: admin, body: { token: old, customerId: c.id } });
    expect(expired.status).toBe(200);
    expect(expired.body.state).toBe('expired');
  });

  test.each([
    ['a tampered signature', () => Promise.resolve(GOLDEN_VECTORS[2]!.key.slice(0, -4) + 'AAAA'), 'does not verify'],
    ['a kid that is not trusted', async () => signed(`{"jti":"lic_x","iss":"kudurru","sub":"s","plan":"business","nodes":1,"iat":0,"exp":${future}}`, 'kudurru-2099-9'), 'LICENSE_TRUSTED_KEYS_DIR'],
    ['claims Validate refuses (exp not after iat)', async () => signed('{"jti":"lic_x","iss":"kudurru","sub":"s","plan":"business","nodes":1,"iat":5,"exp":5}'), 'exp is not after iat'],
    ['an unknown plan', async () => signed(`{"jti":"lic_x","iss":"kudurru","sub":"s","plan":"pro","nodes":1,"iat":0,"exp":${future}}`), 'plan'],
    ['more nodes than the portal stores', async () => signed(`{"jti":"lic_x","iss":"kudurru","sub":"s","plan":"business","nodes":100001,"iat":0,"exp":${future}}`), '100000'],
    ['an expiry after 9999', async () => signed('{"jti":"lic_x","iss":"kudurru","sub":"s","plan":"business","nodes":1,"iat":0,"exp":253402300800}'), '9999'],
    ['not a key at all', () => Promise.resolve('hello'), 'does not verify'],
  ])('400 invalid (fields token): %s', async (_name, make, message) => {
    const c = await createCustomer(t.db);
    const res = await callJson(app, 'licenseImport', { as: admin, body: { token: await make(), customerId: c.id } });
    expect(res.status).toBe(400);
    expect(res.error?.fields).toEqual(['token']);
    expect(res.error?.message).toContain(message);
  });

  test('an unknown customer is 404', async () => {
    const token = await signed(`{"jti":"lic_imp_nocust","iss":"kudurru","sub":"s","plan":"business","nodes":1,"iat":0,"exp":${future}}`);
    expect((await callJson(app, 'licenseImport', { as: admin, body: { token, customerId: 'cus_nobodyhere' } })).status).toBe(404);
  });
});
