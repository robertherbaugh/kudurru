import { expect, test } from 'bun:test';
import { existsSync, mkdtempSync, readFileSync, readdirSync, rmSync, statSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { deploy, prepareDeployment, runtimeVariables } from '../scripts/deploy';

const template = {
  name: 'kudurru-license-portal',
  main: 'src/index.ts',
  routes: [{ pattern: 'licenses.kudurru.io', custom_domain: true }],
  containers: [{ class_name: 'LicensePortal', image: 'placeholder', instance_type: 'basic' }],
};
const env = {
  CLOUDFLARE_ACCOUNT_ID: 'a'.repeat(32),
  CLOUDFLARE_API_TOKEN: 'cloudflare-token',
  DATABASE_URL: 'postgres://postgres:bootstrap-password@db:5432/licenses?sslmode=verify-full',
  PUBLIC_URL: 'https://licenses.kudurru.io',
  OIDC_ISSUER: 'https://idp.kudurru.io',
  OIDC_CLIENT_ID: 'kudurru-license-portal',
  OIDC_CLIENT_SECRET: 'oidc-secret',
  LICENSE_SIGNING_KEY: 'signing-seed',
  LICENSE_SIGNING_KID: 'kudurru-2026-1',
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
  expect(() => prepareDeployment(template, { ...env, OIDC_ISSUER: 'https://idp.example.org' }, commit, '123.1'))
    .toThrow('Replace the example.org placeholders');
  expect(() => prepareDeployment({ ...template, vars: { DATABASE_URL: env.DATABASE_URL } }, env, commit, '123.1'))
    .toThrow('Store credentials as secrets');
});

test('required public settings must come from deployment variables, even with old template values', () => {
  const old = { ...template, vars: { ...env } };
  for (const name of ['PUBLIC_URL', 'OIDC_ISSUER', 'OIDC_CLIENT_ID', 'LICENSE_SIGNING_KID']) {
    for (const value of [undefined, '', ' \n']) {
      expect(() => prepareDeployment(old, { ...env, [name]: value }, commit, '123.1'))
        .toThrow(`${name} is required`);
    }
  }
});

test('deployment variables replace template OIDC settings without modifying it or becoming secrets', () => {
  const old = { ...template, vars: { OIDC_ISSUER: 'https://idp.example.org', OIDC_CLIENT_ID: 'old-client' } };
  const result = prepareDeployment(old, { ...env, ADMIN_NOTIFY_EMAIL: ' approvals@kudurru.io\n' }, commit, '123.1');
  expect(result.config.vars.OIDC_ISSUER).toBe(env.OIDC_ISSUER);
  expect(result.config.vars.OIDC_CLIENT_ID).toBe(env.OIDC_CLIENT_ID);
  expect(result.config.vars.ADMIN_NOTIFY_EMAIL).toBe('approvals@kudurru.io');
  expect(old.vars.OIDC_ISSUER).toBe('https://idp.example.org');
  for (const name of ['OIDC_ISSUER', 'OIDC_CLIENT_ID', 'ADMIN_NOTIFY_EMAIL']) {
    expect(result.secrets).not.toHaveProperty(name);
  }
  expect(prepareDeployment(template, { ...env, ADMIN_NOTIFY_EMAIL: ' \n' }, commit, '123.1').config.vars)
    .not.toHaveProperty('ADMIN_NOTIFY_EMAIL');
  expect(() => prepareDeployment(template, { ...env, ADMIN_NOTIFY_EMAIL: 'approvers@example.org' }, commit, '123.1'))
    .toThrow('Replace the example.org placeholders');
});

test('GitHub variables supply all public settings while secrets and unrelated variables are excluded', () => {
  const variables = Object.fromEntries(runtimeVariables.map((name) => [name, ` value-for-${name} \n`]));
  Object.assign(variables, {
    PUBLIC_URL: env.PUBLIC_URL,
    OIDC_ISSUER: env.OIDC_ISSUER,
    OIDC_CLIENT_ID: env.OIDC_CLIENT_ID,
    LICENSE_SIGNING_KID: env.LICENSE_SIGNING_KID,
    MIGRATE_ON_START: 'false',
    DATABASE_URL: 'must-not-be-a-public-var',
    DATABASE_MIGRATE_URL: 'must-not-reach-the-worker',
    OIDC_CLIENT_SECRET: 'must-not-be-a-public-var',
    LICENSE_SIGNING_KEY: 'must-not-be-a-public-var',
    SMTP_URL: 'must-not-be-a-public-var',
    UNRELATED: 'ignore-this',
  });
  const { PUBLIC_URL, OIDC_ISSUER, OIDC_CLIENT_ID, LICENSE_SIGNING_KID, ...credentials } = env;
  const result = prepareDeployment(template, { ...credentials, LICENSE_PORTAL_VARIABLES: JSON.stringify(variables) }, commit, '123.1');
  for (const name of runtimeVariables) expect(result.config.vars[name]).toBe(variables[name]!.trim());
  for (const name of ['DATABASE_URL', 'DATABASE_MIGRATE_URL', 'OIDC_CLIENT_SECRET', 'LICENSE_SIGNING_KEY', 'SMTP_URL', 'UNRELATED']) {
    expect(result.config.vars).not.toHaveProperty(name);
  }
  expect(result.secrets.OIDC_CLIENT_SECRET).toBe(env.OIDC_CLIENT_SECRET);
  expect(result.secrets.DATABASE_URL).toBe(env.DATABASE_URL);
  expect(prepareDeployment(template, { ...credentials, LICENSE_PORTAL_VARIABLES: JSON.stringify(variables), OIDC_CLIENT_ID: 'local-override' }, commit, '123.1').config.vars.OIDC_CLIENT_ID)
    .toBe('local-override');
});

test('invalid GitHub variable input reports only the setting name, not its contents', () => {
  for (const input of ['invalid-sensitive-content', 'null', '[]', '{"LOG_LEVEL":1}']) {
    expect(() => prepareDeployment(template, { ...env, LICENSE_PORTAL_VARIABLES: input }, commit, '123.1'))
      .toThrow('LICENSE_PORTAL_VARIABLES must be a JSON object of string values');
  }
});

test('deployment keeps migrations out of the serving container', () => {
  expect(() => prepareDeployment(template, { ...env, MIGRATE_ON_START: 'true' }, commit, '123.1'))
    .toThrow('MIGRATE_ON_START must be false');
});

test('pasted credentials are trimmed consistently for Cloudflare and migrations', () => {
  const pasted = Object.fromEntries(Object.entries(env).map(([name, value]) => [name, ` ${value}\n`]));
  const result = prepareDeployment(template, pasted, commit, '123.1');
  expect(result.secrets).toEqual({
    DATABASE_URL: env.DATABASE_URL,
    OIDC_CLIENT_SECRET: env.OIDC_CLIENT_SECRET,
    LICENSE_SIGNING_KEY: env.LICENSE_SIGNING_KEY,
    SMTP_URL: env.SMTP_URL,
  });
  expect(result.cloudflare).toEqual({
    CLOUDFLARE_ACCOUNT_ID: env.CLOUDFLARE_ACCOUNT_ID,
    CLOUDFLARE_API_TOKEN: env.CLOUDFLARE_API_TOKEN,
  });
  expect(result.migrationUrl).toBe(env.DATABASE_URL);
  expect(result.config.vars.OIDC_ISSUER).toBe(env.OIDC_ISSUER);
  expect(result.config.vars.OIDC_CLIENT_ID).toBe(env.OIDC_CLIENT_ID);
  expect(prepareDeployment(template, { ...pasted, DATABASE_MIGRATE_URL: ' owner-url\n' }, commit, '123.1').migrationUrl)
    .toBe('owner-url');
  expect(prepareDeployment(template, { ...pasted, DATABASE_MIGRATE_URL: '\n ' }, commit, '123.1').migrationUrl)
    .toBe(env.DATABASE_URL);
});

function releaseFixture() {
  const stateDir = mkdtempSync(join(tmpdir(), 'license-portal-deploy-test-'));
  const calls: { args: string[]; env: Readonly<Record<string, string | undefined>> }[] = [];
  let tempDir = '';
  let secretsMode = 0;
  let uploadedSecrets: Record<string, string> = {};
  let uploadedConfig: { vars: Record<string, string>; observability?: { enabled: boolean }; containers: { observability?: { logs: { enabled: boolean } } }[] };
  const delays: number[] = [];
  const options: NonNullable<Parameters<typeof deploy>[0]> = {
    template,
    env: {
      ...env,
      DATABASE_MIGRATE_URL: 'postgres://owner:owner-password@db/licenses',
      GITHUB_RUN_ID: '123', GITHUB_RUN_ATTEMPT: '1',
      PATH: '/usr/bin', HOME: '/home/runner', DOCKER_HOST: 'unix:///var/run/docker.sock',
    },
    stateDir,
    git: (...args) => args[0] === 'rev-parse' ? commit : '2026-10-13T00:00:00Z',
    run: (args, _cwd, commandEnv) => {
      calls.push({ args, env: commandEnv });
      tempDir = join(stateDir, readdirSync(stateDir)[0]!);
      const secretsFile = join(tempDir, 'secrets.json');
      secretsMode = statSync(secretsFile).mode & 0o777;
      uploadedSecrets = JSON.parse(readFileSync(secretsFile, 'utf8'));
      uploadedConfig = JSON.parse(readFileSync(join(tempDir, 'wrangler.json'), 'utf8'));
    },
    fetch: async () => Response.json({ ok: true, db: 'ok', migrations: 'ok', signing: 'ready' }),
    sleep: async (ms) => { delays.push(ms); },
    log: () => {},
  };
  return {
    options, calls, delays,
    tempDir: () => tempDir,
    secretsMode: () => secretsMode,
    uploadedSecrets: () => uploadedSecrets,
    uploadedConfig: () => uploadedConfig,
    cleanup: () => rmSync(stateDir, { recursive: true, force: true }),
  };
}

test('the checked-in config deploys with GitHub public variables and observability enabled', async () => {
  const fixture = releaseFixture();
  delete fixture.options.template;
  const { PUBLIC_URL, OIDC_ISSUER, OIDC_CLIENT_ID, LICENSE_SIGNING_KID, ...credentials } = fixture.options.env!;
  fixture.options.env = {
    ...credentials,
    LICENSE_PORTAL_VARIABLES: JSON.stringify({ PUBLIC_URL, OIDC_ISSUER, OIDC_CLIENT_ID, LICENSE_SIGNING_KID, CLIENT_IP_HEADER: 'X-Client-IP' }),
  };
  try {
    await deploy(fixture.options);
    expect(fixture.uploadedConfig().vars.PUBLIC_URL).toBe(env.PUBLIC_URL);
    expect(fixture.uploadedConfig().vars.OIDC_ISSUER).toBe(env.OIDC_ISSUER);
    expect(fixture.uploadedConfig().vars.CLIENT_IP_HEADER).toBe('X-Client-IP');
    expect(fixture.uploadedConfig().observability?.enabled).toBe(true);
    expect(fixture.uploadedConfig().containers[0]?.observability?.logs.enabled).toBe(true);
    for (const call of fixture.calls) expect(call.env).not.toHaveProperty('LICENSE_PORTAL_VARIABLES');
  } finally {
    fixture.cleanup();
  }
});

test('migration failure prevents Worker deployment and removes the restricted secrets file', async () => {
  const fixture = releaseFixture();
  const record = fixture.options.run!;
  fixture.options.run = (args, cwd, commandEnv) => {
    record(args, cwd, commandEnv);
    if (args.at(-1) === 'migrate') throw new Error('migration failed');
  };
  fixture.options.fetch = async () => { throw new Error('readiness must not run after migration failure'); };
  try {
    await expect(deploy(fixture.options)).rejects.toThrow('migration failed');
    expect(fixture.calls.some(({ args }) => args.includes('migrate'))).toBe(true);
    expect(fixture.calls.some(({ args }) => args.includes('--secrets-file'))).toBe(false);
    expect(fixture.secretsMode()).toBe(0o600);
    expect(fixture.tempDir()).not.toBe('');
    expect(existsSync(fixture.tempDir())).toBe(false);
  } finally {
    fixture.cleanup();
  }
});

test('Worker deployment failure also removes the temporary secrets', async () => {
  const fixture = releaseFixture();
  const record = fixture.options.run!;
  fixture.options.run = (args, cwd, commandEnv) => {
    record(args, cwd, commandEnv);
    if (args.includes('--secrets-file')) throw new Error('Worker deployment failed');
  };
  try {
    await expect(deploy(fixture.options)).rejects.toThrow('Worker deployment failed');
    expect(fixture.secretsMode()).toBe(0o600);
    expect(fixture.tempDir()).not.toBe('');
    expect(existsSync(fixture.tempDir())).toBe(false);
  } finally {
    fixture.cleanup();
  }
});

test('commands receive only the credentials they need and success cleans up before readiness', async () => {
  const fixture = releaseFixture();
  fixture.options.fetch = async () => {
    expect(existsSync(fixture.tempDir())).toBe(false);
    return Response.json({ ok: true, db: 'ok', migrations: 'ok', signing: 'ready' });
  };
  try {
    await deploy(fixture.options);
    for (const call of fixture.calls) {
      const migrates = call.args.at(-1) === 'migrate';
      const usesWrangler = call.args[0] === 'node';
      for (const name of ['DATABASE_URL', 'OIDC_CLIENT_SECRET', 'LICENSE_SIGNING_KEY', 'SMTP_URL']) {
        expect(call.env).not.toHaveProperty(name);
      }
      if (migrates) expect(call.env.DATABASE_MIGRATE_URL).toBe(fixture.options.env!.DATABASE_MIGRATE_URL);
      else expect(call.env).not.toHaveProperty('DATABASE_MIGRATE_URL');
      if (usesWrangler) expect(call.env.CLOUDFLARE_API_TOKEN).toBe(env.CLOUDFLARE_API_TOKEN);
      else expect(call.env).not.toHaveProperty('CLOUDFLARE_API_TOKEN');
      expect(call.env.PATH).toBe('/usr/bin');
      expect(call.env.HOME).toBe('/home/runner');
      expect(call.env.DOCKER_HOST).toBe('unix:///var/run/docker.sock');
      for (const value of [...Object.values(env), fixture.options.env!.DATABASE_MIGRATE_URL!]) {
        // Account ID is public and may appear in the generated image reference.
        if (value !== env.CLOUDFLARE_ACCOUNT_ID) expect(call.args.join(' ')).not.toContain(value);
      }
    }
    expect(fixture.uploadedSecrets()).not.toHaveProperty('DATABASE_MIGRATE_URL');
    expect(fixture.uploadedSecrets().LICENSE_SIGNING_KEY).toBe(env.LICENSE_SIGNING_KEY);
    expect(fixture.delays).toEqual([]);
  } finally {
    fixture.cleanup();
  }
});

test('readiness retries unavailable and unsigned responses before accepting the ready portal', async () => {
  const fixture = releaseFixture();
  let requests = 0;
  fixture.options.fetch = async (url) => {
    expect(String(url)).toBe('https://licenses.kudurru.io/readyz');
    requests++;
    if (requests === 1) throw new Error('domain not provisioned');
    if (requests === 2) return new Response('starting', { status: 503 });
    return Response.json({ ok: true, db: 'ok', migrations: 'ok', signing: requests === 3 ? 'unavailable' : 'ready' });
  };
  try {
    await deploy(fixture.options);
    expect(requests).toBe(4);
    expect(fixture.delays).toEqual([10_000, 10_000, 10_000]);
  } finally {
    fixture.cleanup();
  }
});

test('readiness stops after 72 failed attempts without real network calls or sleeps', async () => {
  const fixture = releaseFixture();
  let requests = 0;
  fixture.options.fetch = async () => { requests++; return new Response(null, { status: 503 }); };
  try {
    await expect(deploy(fixture.options)).rejects.toThrow('The portal did not become ready');
    expect(requests).toBe(72);
    expect(fixture.delays).toHaveLength(72);
    expect(existsSync(fixture.tempDir())).toBe(false);
  } finally {
    fixture.cleanup();
  }
});
