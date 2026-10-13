// Shared by GitHub Actions and a local `bun run deploy`. Secret values
// enter Docker through its environment and Wrangler through a temporary
// secrets file, never through build arguments or the committed config.
import { spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { parseConfigFileTextToJson } from 'typescript';
import { SERVER_ENV } from '../src/env';

const project = resolve(import.meta.dir, '..');
const root = resolve(project, '../..');
const wrangler = resolve(project, 'node_modules/wrangler/bin/wrangler.js');
const runtimeSecrets = ['DATABASE_URL', 'OIDC_CLIENT_SECRET', 'LICENSE_SIGNING_KEY', 'SMTP_URL'] as const;
const requiredVariables = ['PUBLIC_URL', 'OIDC_ISSUER', 'OIDC_CLIENT_ID', 'LICENSE_SIGNING_KID'] as const;
export const runtimeVariables = SERVER_ENV.filter((name) => ![...runtimeSecrets, 'DATABASE_MIGRATE_URL'].some((secret) => secret === name));

type Environment = Readonly<Record<string, string | undefined>>;
interface WorkerConfig {
  main: string;
  vars?: Record<string, string>;
  routes?: { pattern: string; custom_domain?: boolean }[];
  containers: { class_name: string; image: string }[];
  [name: string]: unknown;
}

/** Resolve deploy inputs without ever putting secret values in the Worker config. */
export function prepareDeployment(template: WorkerConfig, env: Environment, commit: string, attempt: string) {
  for (const name of ['CLOUDFLARE_ACCOUNT_ID', 'CLOUDFLARE_API_TOKEN', ...runtimeSecrets]) {
    if (!env[name]?.trim()) throw new Error(`${name} is required`);
  }
  const cloudflare = {
    CLOUDFLARE_ACCOUNT_ID: env.CLOUDFLARE_ACCOUNT_ID!.trim(),
    CLOUDFLARE_API_TOKEN: env.CLOUDFLARE_API_TOKEN!.trim(),
  };
  if (!/^[a-f0-9]{32}$/.test(cloudflare.CLOUDFLARE_ACCOUNT_ID)) throw new Error('CLOUDFLARE_ACCOUNT_ID must be a 32-character hex account ID');
  if (!/^[a-f0-9]{40}$/.test(commit)) throw new Error('The deployment requires a full Git commit SHA');
  if (!/^[a-zA-Z0-9_.-]+$/.test(attempt)) throw new Error('Invalid deployment attempt');
  let variables: Record<string, string>;
  try {
    const parsed: unknown = JSON.parse(env.LICENSE_PORTAL_VARIABLES ?? '{}');
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed) || Object.values(parsed).some((value) => typeof value !== 'string')) throw new Error();
    variables = parsed as Record<string, string>;
  } catch {
    throw new Error('LICENSE_PORTAL_VARIABLES must be a JSON object of string values');
  }
  const vars: Record<string, string> = {};
  for (const name of runtimeVariables) {
    const value = (env[name] ?? variables[name])?.trim();
    if (value) vars[name] = value;
  }
  for (const name of requiredVariables) {
    if (!vars[name]) throw new Error(`${name} is required`);
  }
  // CI migrates before deployment; the serving role may not own the schema.
  vars.MIGRATE_ON_START ??= 'false';
  if (vars.MIGRATE_ON_START !== 'false') throw new Error('MIGRATE_ON_START must be false; deployment runs migrations separately');
  if (!template.routes?.some((route) => route.custom_domain && `https://${route.pattern}` === vars.PUBLIC_URL)) {
    throw new Error('Enable a custom domain route matching PUBLIC_URL in wrangler.jsonc');
  }
  if (Object.values(vars).some((value) => value.includes('example.org'))) {
    throw new Error('Replace the example.org placeholders in deployment variables before deploying');
  }
  if (runtimeSecrets.some((name) => name in (template.vars ?? {})) || 'DATABASE_MIGRATE_URL' in (template.vars ?? {})) {
    throw new Error('Store credentials as secrets, not vars in wrangler.jsonc');
  }

  const image = `kudurru-license-server:sha-${commit}-${attempt}`;
  const config: WorkerConfig & { vars: Record<string, string> & { PUBLIC_URL: string } } = {
    ...template,
    main: resolve(project, template.main),
    vars: vars as Record<string, string> & { PUBLIC_URL: string },
    containers: template.containers.map((container) => ({
      ...container,
      image: `registry.cloudflare.com/${cloudflare.CLOUDFLARE_ACCOUNT_ID}/${image}`,
    })),
  };
  const secrets = Object.fromEntries(runtimeSecrets.map((name) => [name, env[name]!.trim()]));
  const migrationUrl = env.DATABASE_MIGRATE_URL?.trim() || secrets.DATABASE_URL!;
  return { config, secrets, cloudflare, migrationUrl, image };
}

/** Tooling inherits host/Docker settings, but no portal or deployment credentials. */
function commandEnv(env: Environment): Record<string, string | undefined> {
  const clean = { ...env };
  for (const name of [...runtimeSecrets, 'DATABASE_MIGRATE_URL', 'CLOUDFLARE_API_TOKEN', 'CLOUDFLARE_ACCOUNT_ID', 'LICENSE_PORTAL_VARIABLES']) {
    delete clean[name];
  }
  return clean;
}

function run(args: string[], cwd: string, env: Environment): void {
  const result = spawnSync(args[0]!, args.slice(1), { cwd, env, stdio: 'inherit' });
  if (result.error || result.status !== 0) throw new Error(`${args[0]} ${args[1] ?? ''} failed`);
}

function git(args: string[], env: Environment): string {
  const result = spawnSync('git', args, { cwd: root, env, encoding: 'utf8' });
  if (result.status !== 0) throw new Error('Could not read deployment metadata from Git');
  return result.stdout.trim();
}

function readTemplate(): WorkerConfig {
  const parsed = parseConfigFileTextToJson('wrangler.jsonc', readFileSync(resolve(project, 'wrangler.jsonc'), 'utf8'));
  if (parsed.error) throw new Error('Could not parse wrangler.jsonc');
  return parsed.config as WorkerConfig;
}

interface DeployOptions {
  env?: Environment;
  run?: typeof run;
  git?: (...args: string[]) => string;
  fetch?: (url: string, init?: RequestInit) => Promise<Response>;
  sleep?: (ms: number) => Promise<unknown>;
  template?: WorkerConfig;
  stateDir?: string;
  log?: (message: string) => void;
}

/** Run a release; injected tools let tests exercise failures without production access. */
export async function deploy(options: DeployOptions = {}): Promise<void> {
  const env = options.env ?? process.env;
  const baseEnv = commandEnv(env);
  const execute = options.run ?? run;
  const metadata = options.git ?? ((...args: string[]) => git(args, baseEnv));
  const request = options.fetch ?? globalThis.fetch;
  const sleep = options.sleep ?? Bun.sleep;
  const log = options.log ?? console.info;
  const commit = metadata('rev-parse', 'HEAD');
  const attempt = env.GITHUB_RUN_ID
    ? `${env.GITHUB_RUN_ID}.${env.GITHUB_RUN_ATTEMPT ?? '1'}`
    : String(Date.now());
  const { config, secrets, cloudflare, migrationUrl, image } = prepareDeployment(options.template ?? readTemplate(), env, commit, attempt);
  const wranglerEnv = { ...baseEnv, ...cloudflare };
  const created = metadata('log', '-1', '--format=%cI');
  const state = options.stateDir ?? resolve(project, '.wrangler');
  mkdirSync(state, { recursive: true });
  const temp = mkdtempSync(resolve(state, 'deploy-'));
  const configFile = resolve(temp, 'wrangler.json');
  const secretsFile = resolve(temp, 'secrets.json');
  try {
    writeFileSync(configFile, JSON.stringify(config));
    writeFileSync(secretsFile, JSON.stringify(secrets), { mode: 0o600 });
    log('Building the license portal image');
    execute(['docker', 'build', '--platform', 'linux/amd64', '-f', 'license-server/Dockerfile',
      '--build-arg', `VERSION=sha-${commit.slice(0, 12)}`, '--build-arg', `COMMIT=${commit}`,
      '--build-arg', `BUILT_AT=${created}`, '-t', image, '.'], root, baseEnv);
    log('Pushing the image to Cloudflare');
    execute(['node', wrangler, 'containers', 'push', image], project, wranglerEnv);
    log('Applying pending database migrations');
    execute(['docker', 'run', '--rm', '-e', 'DATABASE_MIGRATE_URL', '-e', 'DATABASE_TLS=verify-full', image, 'migrate'], root,
      { ...baseEnv, DATABASE_MIGRATE_URL: migrationUrl });
    log('Deploying the Worker and container with runtime secrets');
    execute(['node', wrangler, 'deploy', '--config', configFile, '--secrets-file', secretsFile], project, wranglerEnv);
  } finally {
    rmSync(temp, { recursive: true, force: true });
  }

  // Provisioning and the container rollout continue after Wrangler exits.
  // Check the database, schema and signer through the public Worker route.
  log('Waiting for the portal database, schema and signing key to be ready');
  const readyUrl = new URL('/readyz', config.vars.PUBLIC_URL).href;
  for (let i = 0; i < 72; i++) {
    try {
      const response = await request(readyUrl, { signal: AbortSignal.timeout(10_000) });
      if (response.ok) {
        const status = await response.json() as { ok?: boolean; db?: string; migrations?: string; signing?: string };
        if (status.ok && status.db === 'ok' && status.migrations === 'ok' && status.signing === 'ready') {
          log(`Portal ready: ${config.vars.PUBLIC_URL}`);
          return;
        }
      } else {
        await response.body?.cancel();
      }
    } catch {
      // A new custom domain or container may not answer during provisioning.
    }
    await sleep(10_000);
  }
  throw new Error('The portal did not become ready; check Cloudflare logs. No automatic database rollback was attempted.');
}

if (import.meta.main) {
  try {
    await deploy();
  } catch (error) {
    console.error(error instanceof Error ? error.message : 'Deployment failed');
    process.exitCode = 1;
  }
}
