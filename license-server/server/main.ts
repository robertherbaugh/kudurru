// The license portal's entry point (design spec §1, §2), bundled to
// dist/server.js:
//
//   bun dist/server.js                    serve (migrates first when MIGRATE_ON_START)
//   bun dist/server.js migrate            apply pending migrations and exit
//   bun dist/server.js grant-superadmin   (--email <verified email> | --issuer <iss> --subject <sub>) [--revoke]
//
// Startup refuses (exit 1 with one clear line) on a bad configuration, a
// database migrated by a newer release, pending migrations it may not apply,
// a signing key it must refuse (licensing/keyring.ts), or a route without a
// handler; anything else that fails before serving (an unreachable
// database) is one JSON error line and exit 1, not a stack trace. Serving
// runs the real pipeline (http/pipeline.ts), the UI build (http/static.ts)
// and the background cleanup (jobs.ts). SIGTERM and SIGINT drain the server
// (≤ 20 s), stop the cleanup, close the pool and exit 0.

import { GRANT_USAGE, grantSuperadmin, parseGrantArgs, UsageError, type GrantArgs } from './cli';
import { ConfigError, loadConfig, redactedConfig, type Config } from './config';
import { migrate, migrationStatus, UnknownMigrationError } from './db/migrate';
import { connect, type Db } from './db/sql';
import { createPipeline } from './http/pipeline';
import { assertHandlers, startServer } from './http/router';
import { loadStaticUi, noUiFallback } from './http/static';
import { startJobs } from './jobs';
import { loadSigning } from './licensing/keyring';
import { trustedKids } from './licensing/types';
import { jsonLogger, type Logger } from './log';
import { smtpMailer } from './mail/smtp';
import { handlers } from './api';

function fatal(log: Logger | null, msg: string, fields: Record<string, unknown> = {}): never {
  if (log) log.error(msg, fields);
  else console.error(`license portal: ${msg}`);
  process.exit(1);
}

function configOrExit(): Config {
  try {
    return loadConfig(process.env);
  } catch (e) {
    if (e instanceof ConfigError) fatal(null, e.message);
    throw e;
  }
}

function dbFor(config: Config, url: string, max: number): Db {
  return connect({ url, max, tls: config.database.tls, caFile: config.database.caFile });
}

async function runMigrations(config: Config, log: Logger): Promise<void> {
  const db = dbFor(config, config.database.migrateUrl ?? config.database.url, 1);
  try {
    await migrate(db, { log });
  } catch (e) {
    if (e instanceof UnknownMigrationError) fatal(log, e.message);
    throw e;
  } finally {
    await db.close();
  }
}

/** Refuses a database this build cannot run against (newer, or not migrated yet). */
async function checkSchema(db: Db, log: Logger): Promise<void> {
  const status = await migrationStatus(db);
  if (status.unknown.length > 0) fatal(log, new UnknownMigrationError(status.unknown).message);
  if (status.pending.length > 0) fatal(log, `the database lacks migration ${status.pending.join(', ')}: run \`bun dist/server.js migrate\` or set MIGRATE_ON_START=true`);
}

async function serve(): Promise<void> {
  const config = configOrExit();
  const log = jsonLogger({ level: config.logLevel });
  log.info('starting', { config: redactedConfig(config), build: config.build });
  if (config.database.migrateOnStart) await runMigrations(config, log);

  const db = dbFor(config, config.database.url, config.database.poolMax);
  await checkSchema(db, log);

  let signing: Awaited<ReturnType<typeof loadSigning>>;
  try {
    signing = await loadSigning(config, log);
  } catch (e) {
    fatal(log, `signing key refused: ${(e as Error).message}`);
  }
  log.info('trusted signing keys', { kids: trustedKids(signing), signer: signing.signer?.kid ?? null });
  if (!signing.signer) {
    if (config.signing.required) fatal(log, `REQUIRE_SIGNING_KEY is set but signing is unavailable: ${signing.unavailableReason}`);
    log.error('signing unavailable; issuing answers 503', { reason: signing.unavailableReason });
  }
  const mailer = smtpMailer(config, log);
  if (!mailer.configured) log.warn('mail is not configured (SMTP_URL is unset): nothing is mailed and the public form answers 503');

  try {
    assertHandlers(handlers);
  } catch (e) {
    fatal(log, (e as Error).message);
  }
  const ui = loadStaticUi(log);
  const server = startServer(
    { db, config, log, signing, mailer },
    handlers,
    { hostname: config.host, port: config.port, extraRoutes: ui?.routes, fallback: ui?.fallback ?? noUiFallback },
    createPipeline(config, log),
  );
  const jobs = startJobs({ db, log });
  log.info('listening', { url: `http://${config.host}:${server.port}`, publicUrl: config.publicUrl });

  let stopping = false;
  const stop = (signal: string): void => {
    if (stopping) return;
    stopping = true;
    log.info('stopping', { signal });
    const drained = Promise.all([server.stop(), jobs.stop()]);
    const timeout = new Promise<void>((resolve) => setTimeout(resolve, 20_000).unref());
    void Promise.race([drained, timeout])
      .then(() => db.close({ timeout: 5 }))
      .finally(() => process.exit(0));
  };
  process.on('SIGTERM', () => stop('SIGTERM'));
  process.on('SIGINT', () => stop('SIGINT'));
}

async function migrateCommand(): Promise<void> {
  const config = configOrExit();
  const log = jsonLogger({ level: config.logLevel });
  await runMigrations(config, log);
  process.exit(0);
}

async function grantSuperadminCommand(argv: readonly string[]): Promise<void> {
  let args: GrantArgs;
  try {
    args = parseGrantArgs(argv);
  } catch (e) {
    if (!(e instanceof UsageError)) throw e;
    console.error(`grant-superadmin: ${e.message}\n${GRANT_USAGE}`);
    process.exit(2);
  }
  const config = configOrExit();
  const log = jsonLogger({ level: config.logLevel });
  const db = dbFor(config, config.database.url, 1);
  let code: number;
  try {
    await checkSchema(db, log);
    code = await grantSuperadmin(db, args, { out: (l) => void process.stdout.write(l + '\n'), err: (l) => console.error(l) });
  } finally {
    await db.close();
  }
  process.exit(code);
}

/** What a startup failure may tell the log: name, message and code, no stack (an unreachable database is an operator error, not a crash). */
function describeStartupError(e: unknown): Record<string, unknown> {
  if (!(e instanceof Error)) return { message: String(e).slice(0, 300) };
  const out: Record<string, unknown> = { name: e.name, message: e.message.slice(0, 300) };
  const code = (e as { code?: unknown }).code;
  if (typeof code === 'string') out.code = code;
  return out;
}

const command = process.argv[2] ?? 'serve';
try {
  switch (command) {
    case 'serve':
      await serve();
      break;
    case 'migrate':
      await migrateCommand();
      break;
    case 'grant-superadmin':
      await grantSuperadminCommand(process.argv.slice(3));
      break;
    default:
      console.error(`usage: server.js [serve | migrate | grant-superadmin ...]\n  ${GRANT_USAGE}`);
      process.exit(2);
  }
} catch (e) {
  // One JSON line instead of Bun's stack trace (the database is down, the network refuses, ...).
  fatal(jsonLogger(), `${command} failed`, { error: describeStartupError(e) });
}
