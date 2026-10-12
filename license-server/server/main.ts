// The license portal's entry point (design spec §1, §2), bundled to
// dist/server.js:
//
//   bun dist/server.js                    serve (migrates first when MIGRATE_ON_START)
//   bun dist/server.js migrate            apply pending migrations and exit
//   bun dist/server.js grant-superadmin   (--email <verified email> | --issuer <iss> --subject <sub>) [--revoke]
//
// Startup refuses (exit 1 with one clear line) on a bad configuration, a
// database migrated by a newer release, pending migrations it may not apply,
// or a route without a handler. SIGTERM and SIGINT drain the server (≤ 20 s),
// close the pool and exit 0.

import { ConfigError, loadConfig, redactedConfig, type Config } from './config';
import { migrate, migrationStatus, UnknownMigrationError } from './db/migrate';
import { connect, type Db } from './db/sql';
import { assertHandlers, startServer } from './http/router';
import { signingUnavailable, trustedKids, type SigningState } from './licensing/types';
import { jsonLogger, type Logger } from './log';
import { unconfiguredMailer, type Mailer } from './mail/types';
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

async function serve(): Promise<void> {
  const config = configOrExit();
  const log = jsonLogger({ level: config.logLevel });
  log.info('starting', { config: redactedConfig(config) });
  if (config.database.migrateOnStart) await runMigrations(config, log);

  const db = dbFor(config, config.database.url, config.database.poolMax);
  const status = await migrationStatus(db);
  if (status.unknown.length > 0) fatal(log, new UnknownMigrationError(status.unknown).message);
  if (status.pending.length > 0) fatal(log, `the database lacks migration ${status.pending.join(', ')}: run \`bun dist/server.js migrate\` or set MIGRATE_ON_START=true`);

  // TODO(stream A wires, stream B provides): `const signing = await
  // loadSigning(config, log)` from licensing/keyring.ts. It reads the
  // trusted keyring, imports the signer with the refusals of spec §0.6
  // (throwing a clear error: fatal here) and returns signingUnavailable(
  // 'no signing key is configured', keyring) when no key is set.
  const signing: SigningState = signingUnavailable('no signing key is configured');
  log.info('trusted signing keys', { kids: trustedKids(signing) });
  if (!signing.signer) {
    if (config.signing.required) fatal(log, `REQUIRE_SIGNING_KEY is set but signing is unavailable: ${signing.unavailableReason}`);
    log.error('signing unavailable; issuing answers 503', { reason: signing.unavailableReason });
  }
  // TODO(stream A wires, stream B provides): `smtpMailer(config)` from
  // mail/smtp.ts when SMTP_URL is set.
  const mailer: Mailer = unconfiguredMailer;

  try {
    assertHandlers(handlers);
  } catch (e) {
    fatal(log, (e as Error).message);
  }
  // TODO(stream A): the pipeline (headers, CORS, CSRF, rate limits, client
  // IP), the static UI routes and SPA fallback (http/static.ts) and the
  // background cleanup timer.
  const server = startServer({ db, config, log, signing, mailer }, handlers, { hostname: config.host, port: config.port });
  log.info('listening', { url: `http://${config.host}:${server.port}`, publicUrl: config.publicUrl });

  let stopping = false;
  const stop = (signal: string): void => {
    if (stopping) return;
    stopping = true;
    log.info('stopping', { signal });
    const drained = server.stop();
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

function grantSuperadmin(): never {
  // TODO(stream A): find the user by --email (verified) or --issuer/--subject,
  // print it, write granted_roles (--revoke removes), audit with actor_label "cli".
  fatal(null, 'grant-superadmin is not implemented yet');
}

const command = process.argv[2] ?? 'serve';
switch (command) {
  case 'serve':
    await serve();
    break;
  case 'migrate':
    await migrateCommand();
    break;
  case 'grant-superadmin':
    grantSuperadmin();
    break;
  default:
    console.error('usage: server.js [serve | migrate | grant-superadmin (--email <email> | --issuer <iss> --subject <sub>) [--revoke]]');
    process.exit(2);
}
