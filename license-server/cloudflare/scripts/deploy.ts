// Shared by GitHub Actions and a local `bun run deploy`. Secret values
// enter Docker through its environment and Wrangler through a temporary
// secrets file, never through build arguments or the committed config.
import { spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { parseConfigFileTextToJson } from 'typescript';

const project = resolve(import.meta.dir, '..');
const root = resolve(project, '../..');
const wrangler = resolve(project, 'node_modules/wrangler/bin/wrangler.js');
const runtimeSecrets = ['DATABASE_URL', 'OIDC_CLIENT_SECRET', 'LICENSE_SIGNING_KEY', 'SMTP_URL'] as const;

type Environment = Readonly<Record<string, string | undefined>>;
interface WorkerConfig {
  main: string;
  vars: Record<string, string> & { PUBLIC_URL: string };
  routes?: { pattern: string; custom_domain?: boolean }[];
  containers: { class_name: string; image: string }[];
  [name: string]: unknown;
}

/** Resolve deploy inputs without ever putting secret values in the Worker config. */
export function prepareDeployment(template: WorkerConfig, env: Environment, commit: string, attempt: string) {
  for (const name of ['CLOUDFLARE_ACCOUNT_ID', 'CLOUDFLARE_API_TOKEN', ...runtimeSecrets]) {
    if (!env[name]?.trim()) throw new Error(`${name} is required`);
  }
  if (!/^[a-f0-9]{32}$/.test(env.CLOUDFLARE_ACCOUNT_ID!)) throw new Error('CLOUDFLARE_ACCOUNT_ID must be a 32-character hex account ID');
  if (!/^[a-f0-9]{40}$/.test(commit)) throw new Error('The deployment requires a full Git commit SHA');
  if (!/^[a-zA-Z0-9_.-]+$/.test(attempt)) throw new Error('Invalid deployment attempt');
  if (!template.routes?.some((route) => route.custom_domain && `https://${route.pattern}` === template.vars.PUBLIC_URL)) {
    throw new Error('Enable a custom domain route matching PUBLIC_URL in wrangler.jsonc');
  }
  if (Object.values(template.vars).some((value) => value.includes('example.org'))) {
    throw new Error('Replace the example.org placeholders in wrangler.jsonc before deploying');
  }
  if (runtimeSecrets.some((name) => name in template.vars) || 'DATABASE_MIGRATE_URL' in template.vars) {
    throw new Error('Store credentials as secrets, not vars in wrangler.jsonc');
  }

  const image = `kudurru-license-server:sha-${commit}-${attempt}`;
  const config: WorkerConfig = {
    ...template,
    main: resolve(project, template.main),
    vars: { ...template.vars, MIGRATE_ON_START: 'false' },
    containers: template.containers.map((container) => ({
      ...container,
      image: `registry.cloudflare.com/${env.CLOUDFLARE_ACCOUNT_ID}/${image}`,
    })),
  };
  const secrets = Object.fromEntries(runtimeSecrets.map((name) => [name, env[name]!]));
  const migrationUrl = env.DATABASE_MIGRATE_URL?.trim() ? env.DATABASE_MIGRATE_URL : env.DATABASE_URL!;
  return { config, secrets, migrationUrl, image };
}

function run(args: string[], cwd: string, env: Environment = process.env): void {
  const result = spawnSync(args[0]!, args.slice(1), { cwd, env, stdio: 'inherit' });
  if (result.error || result.status !== 0) throw new Error(`${args[0]} ${args[1] ?? ''} failed`);
}

function git(...args: string[]): string {
  const result = spawnSync('git', args, { cwd: root, encoding: 'utf8' });
  if (result.status !== 0) throw new Error('Could not read deployment metadata from Git');
  return result.stdout.trim();
}

async function deploy(): Promise<void> {
  const parsed = parseConfigFileTextToJson('wrangler.jsonc', readFileSync(resolve(project, 'wrangler.jsonc'), 'utf8'));
  if (parsed.error) throw new Error('Could not parse wrangler.jsonc');
  const commit = git('rev-parse', 'HEAD');
  const attempt = process.env.GITHUB_RUN_ID
    ? `${process.env.GITHUB_RUN_ID}.${process.env.GITHUB_RUN_ATTEMPT ?? '1'}`
    : String(Date.now());
  const { config, secrets, migrationUrl, image } = prepareDeployment(parsed.config as WorkerConfig, process.env, commit, attempt);
  const created = git('log', '-1', '--format=%cI');
  const state = resolve(project, '.wrangler');
  mkdirSync(state, { recursive: true });
  const temp = mkdtempSync(resolve(state, 'deploy-'));
  const configFile = resolve(temp, 'wrangler.json');
  const secretsFile = resolve(temp, 'secrets.json');
  try {
    writeFileSync(configFile, JSON.stringify(config));
    writeFileSync(secretsFile, JSON.stringify(secrets), { mode: 0o600 });
    console.info('Building the license portal image');
    run(['docker', 'build', '--platform', 'linux/amd64', '-f', 'license-server/Dockerfile',
      '--build-arg', `VERSION=sha-${commit.slice(0, 12)}`, '--build-arg', `COMMIT=${commit}`,
      '--build-arg', `BUILT_AT=${created}`, '-t', image, '.'], root);
    console.info('Pushing the image to Cloudflare');
    run(['node', wrangler, 'containers', 'push', image], project);
    console.info('Applying pending database migrations');
    run(['docker', 'run', '--rm', '-e', 'DATABASE_MIGRATE_URL', '-e', 'DATABASE_TLS=verify-full', image, 'migrate'], root,
      { ...process.env, DATABASE_MIGRATE_URL: migrationUrl });
    console.info('Deploying the Worker and container with runtime secrets');
    run(['node', wrangler, 'deploy', '--config', configFile, '--secrets-file', secretsFile], project);
  } finally {
    rmSync(temp, { recursive: true, force: true });
  }

  // Provisioning and the container rollout continue after Wrangler exits.
  // Check the database, schema and signer through the public Worker route.
  console.info('Waiting for the portal database, schema and signing key to be ready');
  const readyUrl = new URL('/readyz', config.vars.PUBLIC_URL).href;
  for (let i = 0; i < 72; i++) {
    try {
      const response = await fetch(readyUrl, { signal: AbortSignal.timeout(10_000) });
      if (response.ok) {
        const status = await response.json() as { ok?: boolean; db?: string; migrations?: string; signing?: string };
        if (status.ok && status.db === 'ok' && status.migrations === 'ok' && status.signing === 'ready') {
          console.info(`Portal ready: ${config.vars.PUBLIC_URL}`);
          return;
        }
      } else {
        await response.body?.cancel();
      }
    } catch {
      // A new custom domain or container may not answer during provisioning.
    }
    await Bun.sleep(10_000);
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
