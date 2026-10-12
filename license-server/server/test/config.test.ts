// The configuration parser (design spec §3): the defaults of the table,
// *_FILE variants for the secrets (and only for them: the signing key's file
// form is its own variable), durations, comma lists and the errors that
// name the variable. Stream A adds the startup refusals of §2/§3.
import { describe, expect, test } from 'bun:test';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { CONFIG_VARS, ConfigError, loadConfig, parseDuration, redactedConfig } from '../config';

const required = {
  PUBLIC_URL: 'https://licenses.example.org/',
  DATABASE_URL: 'postgres://app:pw@db:5432/licenses',
  OIDC_ISSUER: 'https://idp.example.org/',
  OIDC_CLIENT_ID: 'license-portal',
  OIDC_CLIENT_SECRET: 'shh',
};

describe('loadConfig', () => {
  test('the defaults of spec §3', () => {
    const c = loadConfig(required);
    expect(c.publicUrl).toBe('https://licenses.example.org');
    expect(c.publicOrigin).toBe('https://licenses.example.org');
    expect(c.secureCookies).toBe(true);
    expect([c.host, c.port, c.devMode, c.logLevel]).toEqual(['0.0.0.0', 8790, false, 'info']);
    expect(c.database).toMatchObject({ tls: 'verify-full', tlsExplicit: false, poolMax: 10, migrateOnStart: true, migrateUrl: null });
    expect(c.oidc).toMatchObject({ issuer: 'https://idp.example.org/', scopes: ['openid', 'profile', 'email'], displayName: 'Sign in', groupsClaim: 'groups', adminClaimRules: [] });
    expect(c.roles).toEqual({ groups: { superadmin: [], license_admin: [], customer: [] }, defaultRole: 'customer', memberAutoLink: false });
    expect(c.session).toEqual({ idleTimeoutMs: 30 * 60_000, lifetimeMs: 12 * 3_600_000 });
    expect(c.mail).toEqual({ smtpUrl: null, from: 'kudurru license portal <licenses@kudurru.io>', replyTo: 'hello@kudurru.io', adminNotify: null });
    expect(c.publicForm).toEqual({ enabled: true, origins: ['https://kudurru.io'], limitPerIpPerHour: 5, limitPerEmailPerDay: 3 });
    expect(c.personal).toEqual({ autoApprove: true, autoApproveMaxPerDay: 50 });
    expect(c.issuing).toEqual({ maxPerActorPerDay: 50 });
    expect(c.signing).toMatchObject({ keyFile: null, keyEnv: null, kid: null, required: false });
    expect(c.signing.trustedKeysDir.endsWith(join('internal', 'license', 'keys'))).toBe(true);
    expect(c.build).toEqual({ version: 'dev', commit: 'dev', builtAt: null });
  });

  test('the OIDC issuer is kept exactly (trailing slash included); the build stamp from the image', () => {
    expect(loadConfig({ ...required, OIDC_ISSUER: ' https://sts.windows.net/tenant/ ' }).oidc.issuer).toBe('https://sts.windows.net/tenant/');
    expect(loadConfig({ ...required, OIDC_ISSUER: 'http://localhost:5556/dex' }).oidc.issuer).toBe('http://localhost:5556/dex');
    const built = loadConfig({ ...required, LICENSE_SERVER_VERSION: '0.1.0', LICENSE_SERVER_COMMIT: 'abc1234', LICENSE_SERVER_BUILT_AT: '2026-10-12T00:00:00Z' });
    expect(built.build).toEqual({ version: '0.1.0', commit: 'abc1234', builtAt: '2026-10-12T00:00:00Z' });
    expect(loadConfig({ ...required, LICENSE_SERVER_COMMIT: '', LICENSE_SERVER_BUILT_AT: '' }).build).toEqual({ version: 'dev', commit: 'dev', builtAt: null });
  });

  test('secrets from files; the signing key file is not the env form', () => {
    const dir = mkdtempSync(join(tmpdir(), 'kls-config-'));
    writeFileSync(join(dir, 'db'), 'postgres://app:from-file@db/licenses\n');
    writeFileSync(join(dir, 'seed'), 'c2VlZA==\n');
    const c = loadConfig({ ...required, DATABASE_URL: '', DATABASE_URL_FILE: join(dir, 'db'), LICENSE_SIGNING_KEY_FILE: join(dir, 'seed'), LICENSE_SIGNING_KID: 'kudurru-2026-1' });
    expect(c.database.url).toBe('postgres://app:from-file@db/licenses');
    expect(c.signing).toMatchObject({ keyFile: join(dir, 'seed'), keyEnv: null, kid: 'kudurru-2026-1' });
    expect(() => loadConfig({ ...required, DATABASE_URL_FILE: join(dir, 'db') })).toThrow('DATABASE_URL and DATABASE_URL_FILE are both set');
    expect(() => loadConfig({ ...required, OIDC_CLIENT_SECRET: '', OIDC_CLIENT_SECRET_FILE: join(dir, 'missing') })).toThrow(ConfigError);
  });

  test('lists, switches and errors that name the variable', () => {
    const c = loadConfig({
      ...required,
      PUBLIC_URL: 'http://localhost:8790',
      PUBLIC_FORM_ORIGINS: '-',
      ROLE_SUPERADMIN_GROUPS: 'admins, ops ,',
      OIDC_ADMIN_CLAIM_RULES: 'hd=example.org,acr=mfa',
      TRUSTED_PROXIES: '10.0.0.0/8, 192.168.0.0/16',
      SESSION_IDLE_TIMEOUT: '1h30m',
    });
    expect(c.secureCookies).toBe(false);
    expect(c.publicForm).toMatchObject({ enabled: false, origins: [] });
    expect(c.roles.groups.superadmin).toEqual(['admins', 'ops']);
    expect(c.oidc.adminClaimRules).toEqual([
      { claim: 'hd', value: 'example.org' },
      { claim: 'acr', value: 'mfa' },
    ]);
    expect(c.trustedProxies).toEqual(['10.0.0.0/8', '192.168.0.0/16']);
    expect(c.session.idleTimeoutMs).toBe(90 * 60_000);
    expect(() => loadConfig({ ...required, PUBLIC_URL: '' })).toThrow('PUBLIC_URL is required');
    expect(() => loadConfig({ ...required, PORT: 'eighty' })).toThrow('PORT must be a whole number');
    expect(() => loadConfig({ ...required, MIGRATE_ON_START: 'maybe' })).toThrow('MIGRATE_ON_START must be true or false');
    expect(() => loadConfig({ ...required, SESSION_LIFETIME: '10m' })).toThrow('SESSION_IDLE_TIMEOUT must not be longer than SESSION_LIFETIME');
  });

  test('redactedConfig hides every secret', () => {
    const c = loadConfig({ ...required, SMTP_URL: 'smtp://u:p@mail:587', LICENSE_SERVER_DEV: 'true', LICENSE_SIGNING_KEY: 'c2VlZA==' });
    const text = JSON.stringify(redactedConfig(c));
    for (const secret of ['pw@db', 'shh', 'u:p@mail', 'c2VlZA==']) expect(text).not.toContain(secret);
    // Every secret of the table is covered.
    expect(CONFIG_VARS.filter((v) => 'secret' in v).map((v) => v.name)).toEqual(['DATABASE_URL', 'DATABASE_MIGRATE_URL', 'OIDC_CLIENT_SECRET', 'LICENSE_SIGNING_KEY', 'SMTP_URL']);
  });
});

describe('parseDuration', () => {
  test.each([
    ['30m', 1_800_000],
    ['12h', 43_200_000],
    ['1h30m', 5_400_000],
    ['90s', 90_000],
    ['500ms', 500],
    ['7d', 604_800_000],
    ['1.5h', 5_400_000],
  ] as const)('%s', (s, ms) => expect(parseDuration(s)).toBe(ms));
  test.each(['', '30', '-5m', '5 m', 'm', '1y'])('refuses %j', (s) => expect(() => parseDuration(s)).toThrow(RangeError));
});
