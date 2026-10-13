// Test databases. Every test file that touches Postgres gets its own fresh
// database: useTestDb() creates it in beforeAll from a template database
// that holds the migrated schema (created once per migration set, named
// after a hash of the migrations, under an advisory lock so parallel test
// processes do not race) and drops it WITH (FORCE) in afterAll.
//
// TEST_DATABASE_URL names a Postgres role that may create databases (e.g.
// postgres://postgres@127.0.0.1:55433/postgres). Without it database tests
// skip with a warning; REQUIRE_DB=1 (CI) makes that a failure instead.
//
//   const t = useTestDb();
//   describeDb('licenses', () => { test('…', async () => { await t.db`select 1`; }); });

import { afterAll, beforeAll, describe } from 'bun:test';
import { createHash, randomBytes } from 'node:crypto';
import { migrate } from '../db/migrate';
import { connect, type Db } from '../db/sql';
import { MIGRATIONS } from '../migrations';

export const TEST_DATABASE_URL = process.env.TEST_DATABASE_URL ?? '';

/** Whether database tests run. */
export const hasDb = TEST_DATABASE_URL !== '';

if (!hasDb && process.env.REQUIRE_DB === '1') {
  throw new Error('REQUIRE_DB=1 but TEST_DATABASE_URL is not set: point it at a Postgres role that may create databases, e.g. postgres://postgres@127.0.0.1:5432/postgres');
}

/** The advisory lock key around template creation and CREATE DATABASE … TEMPLATE. */
const TEMPLATE_LOCK = 7404;

let warned = false;

/** describe() for tests that need Postgres: skipped, with one warning per process, without TEST_DATABASE_URL. */
export function describeDb(label: string, fn: () => void): void {
  if (!hasDb && !warned) {
    warned = true;
    console.warn('TEST_DATABASE_URL is not set: the database tests are skipped (set it, or REQUIRE_DB=1 to fail instead).');
  }
  describe.skipIf(!hasDb)(label, fn);
}

/**
 * A real promise for a lazy Bun.sql query. Bun's expect(query).rejects
 * inspects the promise without calling then(), so a query that was never
 * started spins forever: write `await expect(settle(db`…`)).rejects…`.
 */
export async function settle(q: PromiseLike<unknown>): Promise<void> {
  await q;
}

/** The URL of another database on the same server. */
export function databaseUrl(name: string): string {
  const url = new URL(TEST_DATABASE_URL);
  url.pathname = `/${name}`;
  return url.toString();
}

/** The template's name: changes whenever a migration does. */
function templateName(): string {
  const hash = createHash('sha256');
  for (const m of MIGRATIONS) hash.update(`${m.version}\0${m.name}\0${m.sql}\0`);
  return `kls_tpl_${hash.digest('hex').slice(0, 16)}`;
}

function quoteIdent(name: string): string {
  if (!/^[a-z0-9_]+$/.test(name)) throw new Error(`unexpected database name ${name}`);
  return `"${name}"`;
}

export interface TestDatabase {
  readonly name: string;
  readonly url: string;
  readonly db: Db;
}

/** Creates a migrated database for one test file (see useTestDb). */
export async function createTestDatabase(): Promise<TestDatabase> {
  const admin = connect({ url: TEST_DATABASE_URL, tls: 'disable', max: 1 });
  const name = `kls_test_${process.pid}_${randomBytes(4).toString('hex')}`;
  try {
    const conn = await admin.reserve();
    try {
      await conn`select pg_advisory_lock(${TEMPLATE_LOCK})`;
      try {
        const template = templateName();
        const exists = await conn<{ one: number }[]>`select 1 as one from pg_database where datname = ${template}`;
        if (exists.length === 0) {
          const stale = await conn<{ datname: string }[]>`select datname from pg_database where datname like 'kls\\_tpl\\_%' and datname <> ${template}`;
          for (const s of stale) await conn.unsafe(`drop database if exists ${quoteIdent(s.datname)} with (force)`);
          const building = `${template}_building`;
          await conn.unsafe(`drop database if exists ${quoteIdent(building)} with (force)`);
          await conn.unsafe(`create database ${quoteIdent(building)}`);
          const tdb = connect({ url: databaseUrl(building), tls: 'disable', max: 1 });
          try {
            await migrate(tdb);
          } finally {
            await tdb.close();
          }
          await conn.unsafe(`alter database ${quoteIdent(building)} rename to ${quoteIdent(template)}`);
        }
        await conn.unsafe(`create database ${quoteIdent(name)} template ${quoteIdent(template)}`);
      } finally {
        await conn`select pg_advisory_unlock(${TEMPLATE_LOCK})`;
      }
    } finally {
      conn.release();
    }
  } finally {
    await admin.close();
  }
  const url = databaseUrl(name);
  return { name, url, db: connect({ url, tls: 'disable', max: 4 }) };
}

/** Closes the pool and drops the database. */
export async function dropTestDatabase(t: TestDatabase): Promise<void> {
  await t.db.close({ timeout: 1 });
  const admin = connect({ url: TEST_DATABASE_URL, tls: 'disable', max: 1 });
  try {
    await admin.unsafe(`drop database if exists ${quoteIdent(t.name)} with (force)`);
  } finally {
    await admin.close();
  }
}

/**
 * A fresh migrated database for this test file: created in beforeAll,
 * dropped in afterAll. Reading `db` without TEST_DATABASE_URL throws (use
 * describeDb so those tests skip).
 */
export function useTestDb(): TestDatabase {
  let current: TestDatabase | null = null;
  beforeAll(async () => {
    if (hasDb) current = await createTestDatabase();
  });
  afterAll(async () => {
    if (current) await dropTestDatabase(current);
    current = null;
  });
  const get = (): TestDatabase => {
    if (!current) throw new Error(hasDb ? 'the test database is not ready (use it inside tests, not at import)' : 'TEST_DATABASE_URL is not set');
    return current;
  };
  return {
    get name() {
      return get().name;
    },
    get url() {
      return get().url;
    },
    get db() {
      return get().db;
    },
  };
}
