import { expect, test } from 'bun:test';
import { prepareDeployment } from '../scripts/deploy';

const template = {
  name: 'kudurru-license-portal',
  main: 'src/index.ts',
  routes: [{ pattern: 'licenses.kudurru.io', custom_domain: true }],
  vars: { PUBLIC_URL: 'https://licenses.kudurru.io', OIDC_ISSUER: 'https://idp.kudurru.io' },
  containers: [{ class_name: 'LicensePortal', image: 'placeholder', instance_type: 'basic' }],
};
const env = {
  CLOUDFLARE_ACCOUNT_ID: 'a'.repeat(32),
  CLOUDFLARE_API_TOKEN: 'cloudflare-token',
  DATABASE_URL: 'postgres://postgres:bootstrap-password@db:5432/licenses?sslmode=verify-full',
  OIDC_CLIENT_SECRET: 'oidc-secret',
  LICENSE_SIGNING_KEY: 'signing-seed',
  SMTP_URL: 'smtps://api_token:mail-token@smtp.mx.cloudflare.net:465',
};
const commit = 'b'.repeat(40);

test('one database credential serves and migrates, with runtime secrets outside the generated config', () => {
  const result = prepareDeployment(template, env, commit, '123.1');
  expect(result.migrationUrl).toBe(env.DATABASE_URL);
  expect(result.secrets.DATABASE_URL).toBe(env.DATABASE_URL);
  expect(result.config.vars.MIGRATE_ON_START).toBe('false');
  expect(result.config.containers[0]!.image).toBe(`registry.cloudflare.com/${env.CLOUDFLARE_ACCOUNT_ID}/kudurru-license-server:sha-${commit}-123.1`);
  expect(result.config.routes).toEqual(template.routes);
  expect(template.containers[0]!.image).toBe('placeholder');
  for (const secret of [env.CLOUDFLARE_API_TOKEN, ...Object.values(result.secrets)]) {
    expect(JSON.stringify(result.config)).not.toContain(secret);
  }
});

test('an optional migration credential never reaches the serving container', () => {
  const owner = 'postgres://owner:owner-password@db/licenses';
  const result = prepareDeployment(template, { ...env, DATABASE_MIGRATE_URL: owner }, commit, '123.1');
  expect(result.migrationUrl).toBe(owner);
  expect(result.secrets).not.toHaveProperty('DATABASE_MIGRATE_URL');
  expect(JSON.stringify(result.config)).not.toContain(owner);
});

test('a retry gets a distinct image reference to trigger a container rollout', () => {
  expect(prepareDeployment(template, env, commit, '123.1').image)
    .not.toBe(prepareDeployment(template, env, commit, '123.2').image);
});

test('missing secrets and unfinished configuration stop before any deployment command', () => {
  expect(() => prepareDeployment(template, { ...env, LICENSE_SIGNING_KEY: '' }, commit, '123.1'))
    .toThrow('LICENSE_SIGNING_KEY is required');
  expect(() => prepareDeployment({ ...template, routes: [] }, env, commit, '123.1'))
    .toThrow('Enable a custom domain route');
  expect(() => prepareDeployment({ ...template, vars: { ...template.vars, OIDC_ISSUER: 'https://idp.example.org' } }, env, commit, '123.1'))
    .toThrow('Replace the example.org placeholders');
  expect(() => prepareDeployment({ ...template, vars: { ...template.vars, DATABASE_URL: env.DATABASE_URL } }, env, commit, '123.1'))
    .toThrow('Store credentials as secrets');
});
