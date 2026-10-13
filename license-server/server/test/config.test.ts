// The configuration parser (design spec §3): the defaults of the table,
// *_FILE variants for the secrets (and only for them: the signing key's file
// form is its own variable), durations, comma lists and the errors that
// name the variable; then the startup refusals of §0.6, §2 and §3 (sslmode,
// non-Postgres URLs, the env signing key outside dev mode, key and kid
// together, admin groups without claim rules under DEFAULT_ROLE=customer,
// http issuers, PUBLIC_URL beyond an origin, TRUSTED_PROXIES CIDRs,
// PUBLIC_FORM_ORIGINS exact origins), none of which echoes a secret.
import { describe, expect, test } from 'bun:test';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { CONFIG_VARS, ConfigError, isLoopbackHost, loadConfig, parseDuration, redactedConfig, type Env } from '../config';

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
    expect(loadConfig({ ...required, PUBLIC_URL: 'http://localhost:8791', OIDC_ISSUER: 'http://localhost:5556/dex' }).oidc.issuer).toBe('http://localhost:5556/dex');
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
    const c = loadConfig({ ...required, SMTP_URL: 'smtp://u:p@mail:587', LICENSE_SERVER_DEV: 'true', LICENSE_SIGNING_KEY: 'c2VlZA==', LICENSE_SIGNING_KID: 'dev-1' });
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

describe('startup refusals', () => {
  /** The ConfigError message of loading `env` over the required set. */
  const refusal = (env: Env): string => {
    try {
      loadConfig({ ...required, ...env });
    } catch (e) {
      expect(e).toBeInstanceOf(ConfigError);
      return (e as Error).message;
    }
    throw new Error(`accepted: ${JSON.stringify(env)}`);
  };
  const accepts = (env: Env) => expect(() => loadConfig({ ...required, ...env })).not.toThrow();

  test('database URLs: sslmode refused, postgres only, no password echoed', () => {
    for (const mode of ['require', 'prefer', 'verify-full', 'disable']) {
      const msg = refusal({ DATABASE_URL: `postgres://app:hunter2@db/licenses?sslmode=${mode}` });
      expect(msg).toContain('DATABASE_URL has an sslmode= parameter');
      expect(msg).toContain('DATABASE_TLS');
      expect(msg).not.toContain('hunter2');
    }
    expect(refusal({ DATABASE_MIGRATE_URL: 'postgresql://owner:hunter2@db/licenses?sslmode=require' })).toContain('DATABASE_MIGRATE_URL has an sslmode= parameter');
    expect(refusal({ DATABASE_URL: 'sqlite://./app.db' })).toBe('DATABASE_URL must be a postgres:// URL');
    expect(refusal({ DATABASE_URL: 'hunter2 is not a url' })).not.toContain('hunter2');
    accepts({ DATABASE_URL: 'postgresql://app:pw@db:5432/licenses?application_name=portal' });
  });

  test('DATABASE_TLS and DATABASE_CA_FILE', () => {
    expect(refusal({ DATABASE_TLS: 'require' })).toContain('DATABASE_TLS must be verify-full or disable');
    const c = loadConfig({ ...required, DATABASE_TLS: 'disable' });
    expect(c.database).toMatchObject({ tls: 'disable', tlsExplicit: true });
    expect(refusal({ DATABASE_CA_FILE: '/nonexistent/ca.pem' })).toBe('DATABASE_CA_FILE: cannot read /nonexistent/ca.pem');
    const dir = mkdtempSync(join(tmpdir(), 'kls-ca-'));
    writeFileSync(join(dir, 'ca.pem'), '-----BEGIN CERTIFICATE-----\n');
    accepts({ DATABASE_CA_FILE: join(dir, 'ca.pem') });
    expect(refusal({ DATABASE_CA_FILE: join(dir, 'ca.pem'), DATABASE_TLS: 'disable' })).toContain('drop one');
  });

  test('the signing key: env form only in dev mode, key and kid together, kid a plain name', () => {
    const msg = refusal({ LICENSE_SIGNING_KEY: 'c2VjcmV0LXNlZWQ=', LICENSE_SIGNING_KID: 'kudurru-2026-1' });
    expect(msg).toContain('LICENSE_SIGNING_KEY');
    expect(msg).toContain('LICENSE_SERVER_DEV=true');
    expect(msg).not.toContain('c2VjcmV0LXNlZWQ=');
    accepts({ LICENSE_SERVER_DEV: 'true', LICENSE_SIGNING_KEY: 'c2VjcmV0LXNlZWQ=', LICENSE_SIGNING_KID: 'e2e-test' });
    expect(refusal({ LICENSE_SIGNING_KEY_FILE: '/run/secrets/signing.key' })).toBe('a signing key is configured without LICENSE_SIGNING_KID');
    expect(refusal({ LICENSE_SIGNING_KID: 'kudurru-2026-1' })).toContain('LICENSE_SIGNING_KID is set but no signing key is');
    expect(refusal({ LICENSE_SERVER_DEV: 'true', LICENSE_SIGNING_KEY: 'x', LICENSE_SIGNING_KEY_FILE: '/k', LICENSE_SIGNING_KID: 'k' })).toContain('both set');
    for (const kid of ['../keys/evil', 'a/b', '.hidden', 'a b', 'x'.repeat(129)]) {
      expect(refusal({ LICENSE_SIGNING_KEY_FILE: '/k', LICENSE_SIGNING_KID: kid })).toContain('LICENSE_SIGNING_KID must be a plain file name');
    }
    accepts({ LICENSE_SIGNING_KEY_FILE: '/run/secrets/signing.key', LICENSE_SIGNING_KID: 'kudurru-2026-1' });
  });

  test('admin group mappings need OIDC_ADMIN_CLAIM_RULES while DEFAULT_ROLE=customer', () => {
    for (const env of [{ ROLE_SUPERADMIN_GROUPS: 'license-superadmins' }, { ROLE_LICENSE_ADMIN_GROUPS: 'license-admins' }]) {
      const msg = refusal(env);
      expect(msg).toContain('OIDC_ADMIN_CLAIM_RULES');
      expect(msg).toContain('DEFAULT_ROLE=customer');
      accepts({ ...env, OIDC_ADMIN_CLAIM_RULES: 'hd=example.org' });
      accepts({ ...env, DEFAULT_ROLE: 'none' });
    }
    accepts({ ROLE_CUSTOMER_GROUPS: 'customers' });
    expect(refusal({ OIDC_ADMIN_CLAIM_RULES: 'hd=' })).toContain('is not claim=value');
    expect(refusal({ OIDC_ADMIN_CLAIM_RULES: '=x' })).toContain('is not claim=value');
    expect(refusal({ OIDC_ADMIN_CLAIM_RULES: 'hd' })).toContain('is not claim=value');
    expect(loadConfig({ ...required, OIDC_ADMIN_CLAIM_RULES: 'tid = abc-123 , email_verified=true' }).oidc.adminClaimRules).toEqual([
      { claim: 'tid', value: 'abc-123' },
      { claim: 'email_verified', value: 'true' },
    ]);
  });

  test('the issuer: https, or http on a loopback literal while PUBLIC_URL is http too', () => {
    const local = { PUBLIC_URL: 'http://localhost:8791' };
    for (const issuer of ['http://localhost:5556/dex', 'http://127.0.0.1:5556/dex', 'http://127.1.2.3/dex', 'http://[::1]:5556/dex']) accepts({ ...local, OIDC_ISSUER: issuer });
    for (const issuer of ['http://dex.localhost:5556/dex', 'http://dex:5556/dex', 'http://10.0.0.5/dex', 'http://localhost.example.org/dex']) {
      expect(refusal({ ...local, OIDC_ISSUER: issuer })).toContain('OIDC_ISSUER must be https');
    }
    expect(refusal({ OIDC_ISSUER: 'http://localhost:5556/dex' })).toBe('OIDC_ISSUER is http while PUBLIC_URL is https: use an https issuer');
    expect(refusal({ OIDC_ISSUER: 'ftp://idp.example.org' })).toBe('OIDC_ISSUER must be an https URL');
    expect(refusal({ OIDC_ISSUER: 'https://idp.example.org/?tenant=x' })).toContain('query');
    expect(refusal({ OIDC_ISSUER: 'idp.example.org' })).toContain('OIDC_ISSUER is not a URL');
    expect(refusal({ OIDC_SCOPES: 'profile email' })).toBe('OIDC_SCOPES must include openid');
    expect(refusal({ OIDC_ID_TOKEN_ALG: 'HS256' })).toContain('OIDC_ID_TOKEN_ALG must be one of');
    expect(refusal({ OIDC_ID_TOKEN_ALG: 'none' })).toContain('none and HS* are refused');
    expect(loadConfig({ ...required, OIDC_ID_TOKEN_ALG: 'EdDSA' }).oidc.idTokenAlg).toBe('EdDSA');
    expect(isLoopbackHost('127.0.0.256')).toBe(false);
    expect(isLoopbackHost('LOCALHOST')).toBe(true);
  });

  test('PUBLIC_URL is a bare origin', () => {
    expect(loadConfig({ ...required, PUBLIC_URL: 'https://Licenses.Example.org:443/' }).publicUrl).toBe('https://licenses.example.org');
    expect(loadConfig({ ...required, PUBLIC_URL: 'http://localhost:5174' }).publicUrl).toBe('http://localhost:5174');
    for (const url of ['https://licenses.example.org/portal', 'https://licenses.example.org//', 'https://licenses.example.org/?x=1', 'https://licenses.example.org/#x', 'https://licenses.example.org?']) {
      expect(refusal({ PUBLIC_URL: url })).toMatch(/PUBLIC_URL must (be the bare origin|not have a query or fragment)/);
    }
    expect(refusal({ PUBLIC_URL: 'https://user:pw@licenses.example.org' })).toBe('PUBLIC_URL must not contain a user name or password');
    expect(refusal({ PUBLIC_URL: 'ftp://licenses.example.org' })).toContain('https://');
    expect(refusal({ PUBLIC_URL: 'licenses.example.org' })).toContain('PUBLIC_URL is not a URL');
  });

  test('TRUSTED_PROXIES are CIDRs or addresses', () => {
    expect(loadConfig({ ...required, TRUSTED_PROXIES: '10.0.0.0/8, fd00::/8, 192.0.2.1' }).trustedProxies).toEqual(['10.0.0.0/8', 'fd00::/8', '192.0.2.1']);
    for (const bad of ['10.0.0.0/33', 'proxy.internal', '10.0.0.0/8 192.168.0.0/16', '*']) expect(refusal({ TRUSTED_PROXIES: bad })).toContain('TRUSTED_PROXIES');
  });

  test('PUBLIC_FORM_ORIGINS: exact origins or -', () => {
    expect(loadConfig({ ...required, PUBLIC_FORM_ORIGINS: 'https://kudurru.io, https://WWW.kudurru.io:443/, http://localhost:8792' }).publicForm.origins).toEqual([
      'https://kudurru.io',
      'https://www.kudurru.io',
      'http://localhost:8792',
    ]);
    expect(loadConfig({ ...required, PUBLIC_FORM_ORIGINS: ' - ' }).publicForm).toMatchObject({ enabled: false, origins: [] });
    for (const bad of ['*', 'https://*.kudurru.io', 'https://kudurru.io/license', 'kudurru.io', 'null', 'https://kudurru.io?x', 'https://kudurru.io, -', ',']) {
      expect(refusal({ PUBLIC_FORM_ORIGINS: bad })).toContain('PUBLIC_FORM_ORIGINS');
    }
  });

  test('durations and sessions', () => {
    expect(refusal({ SESSION_IDLE_TIMEOUT: '30' })).toContain('SESSION_IDLE_TIMEOUT: not a duration');
    expect(refusal({ SESSION_IDLE_TIMEOUT: '30s' })).toBe('SESSION_IDLE_TIMEOUT must be at least 1m');
    expect(refusal({ SESSION_LIFETIME: '0s', SESSION_IDLE_TIMEOUT: '1m' })).toContain('SESSION_LIFETIME must be positive');
    expect(loadConfig({ ...required, SESSION_IDLE_TIMEOUT: '15m', SESSION_LIFETIME: '8h' }).session).toEqual({ idleTimeoutMs: 900_000, lifetimeMs: 28_800_000 });
  });

  test('SMTP_URL: smtp or smtps, never echoed', () => {
    accepts({ SMTP_URL: 'smtp://user:pass@mail.example.org:587' });
    accepts({ SMTP_URL: 'smtps://user:pass@mail.example.org:465' });
    for (const bad of ['http://user:hunter2@mail.example.org', 'not a url hunter2', 'smtp://']) {
      const msg = refusal({ SMTP_URL: bad });
      expect(msg).toContain('SMTP_URL must be');
      expect(msg).not.toContain('hunter2');
    }
  });

  test('the dev and compose settings load', () => {
    accepts({
      PUBLIC_URL: 'http://localhost:5174',
      LICENSE_SERVER_DEV: 'true',
      DATABASE_URL: 'postgres://postgres:postgres@localhost:55432/license_portal',
      DATABASE_TLS: 'disable',
      OIDC_ISSUER: 'http://localhost:5556/dex',
      OIDC_SCOPES: 'openid profile email groups',
      ROLE_SUPERADMIN_GROUPS: 'license-superadmins',
      ROLE_LICENSE_ADMIN_GROUPS: 'license-admins',
      OIDC_ADMIN_CLAIM_RULES: 'email_verified=true',
      LICENSE_SIGNING_KEY_FILE: 'dev/secrets/signing.key',
      LICENSE_SIGNING_KID: 'dev-1',
      LICENSE_TRUSTED_KEYS_DIR: 'dev/trusted-keys',
      SMTP_URL: 'smtp://localhost:1025',
      PUBLIC_FORM_ORIGINS: 'http://localhost:8792',
    });
  });
});
