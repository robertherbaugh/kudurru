// The migration runner (design spec §2): applies the pending migrations of
// server/migrations in order, each in its own transaction (tx.unsafe runs a
// whole file over the simple protocol, so plpgsql bodies work), and records
// them in schema_migrations. Session advisory lock 7403 on one reserved
// connection keeps two instances starting at once from racing. A database
// that holds a version this build does not know is refused: it was migrated
// by a newer release, and running old code against it is unsafe.
//
// `bun dist/server.js migrate` and MIGRATE_ON_START call migrate() with the
// owner role (DATABASE_MIGRATE_URL, else DATABASE_URL); /readyz and startup
// call migrationStatus() with the runtime role.

import { MIGRATIONS, type Migration } from '../migrations';
import type { Logger } from '../log';
import { isPgError, PG, type Db, type Queryable } from './sql';

/** The pg_advisory_lock key of the runner. */
export const MIGRATION_LOCK = 7403;

/** schema_migrations holds versions this build does not know. */
export class UnknownMigrationError extends Error {
  override readonly name = 'UnknownMigrationError';
  constructor(readonly versions: number[]) {
    super(
      `the database has migration ${versions.join(', ')}, which this build does not know: it was migrated by a newer release. Run that release or restore a backup.`,
    );
  }
}

export interface MigrationStatus {
  /** Versions recorded in schema_migrations that this build knows. */
  applied: number[];
  /** Versions of this build not applied yet. */
  pending: number[];
  /** Versions recorded that this build does not know (refuse to run). */
  unknown: number[];
}

export interface MigrateOptions {
  log?: Logger;
  /** Default: MIGRATIONS (tests pass their own lists). */
  migrations?: readonly Migration[];
}

const CREATE_TABLE = `create table if not exists schema_migrations (
  version int primary key,
  name text not null,
  applied_at timestamptz not null default now()
)`;

/** Throws on a list that is not strictly increasing or whose names do not start with the zero-padded version. */
function checkList(migrations: readonly Migration[]): void {
  let last = 0;
  for (const m of migrations) {
    if (!Number.isInteger(m.version) || m.version <= last) throw new Error(`migration ${m.name}: versions must increase (after ${last})`);
    if (!m.name.startsWith(String(m.version).padStart(4, '0') + '_')) throw new Error(`migration ${m.name}: the name must start with ${String(m.version).padStart(4, '0')}_`);
    last = m.version;
  }
}

function statusOf(recorded: number[], migrations: readonly Migration[]): MigrationStatus {
  const known = new Set(migrations.map((m) => m.version));
  const done = new Set(recorded);
  return {
    applied: recorded.filter((v) => known.has(v)),
    pending: migrations.map((m) => m.version).filter((v) => !done.has(v)),
    unknown: recorded.filter((v) => !known.has(v)),
  };
}

/** What schema_migrations says compared with this build (all pending when the table does not exist yet). */
export async function migrationStatus(db: Queryable, migrations: readonly Migration[] = MIGRATIONS): Promise<MigrationStatus> {
  let recorded: number[];
  try {
    const rows = await db<{ version: number }[]>`select version from schema_migrations order by version`;
    recorded = rows.map((r) => r.version);
  } catch (e) {
    if (!isPgError(e, PG.undefinedTable)) throw e;
    recorded = [];
  }
  return statusOf(recorded, migrations);
}

/**
 * Applies every pending migration and returns the versions it applied
 * (empty when the database is current). Throws UnknownMigrationError
 * before changing anything when the database is newer than this build;
 * a failing migration rolls back its own file and stops the run.
 */
export async function migrate(db: Db, opts: MigrateOptions = {}): Promise<number[]> {
  const migrations = opts.migrations ?? MIGRATIONS;
  checkList(migrations);
  const applied: number[] = [];
  const conn = await db.reserve();
  try {
    await conn`select pg_advisory_lock(${MIGRATION_LOCK})`;
    try {
      await conn.unsafe(CREATE_TABLE);
      const rows = await conn<{ version: number }[]>`select version from schema_migrations order by version`;
      const status = statusOf(
        rows.map((r) => r.version),
        migrations,
      );
      if (status.unknown.length > 0) throw new UnknownMigrationError(status.unknown);
      for (const m of migrations) {
        if (!status.pending.includes(m.version)) continue;
        opts.log?.info('applying migration', { version: m.version, name: m.name });
        await conn.begin(async (tx) => {
          await tx.unsafe(m.sql);
          await tx`insert into schema_migrations (version, name) values (${m.version}, ${m.name})`;
        });
        applied.push(m.version);
      }
      if (applied.length > 0) opts.log?.info('migrations applied', { applied });
    } finally {
      await conn`select pg_advisory_unlock(${MIGRATION_LOCK})`;
    }
  } finally {
    conn.release();
  }
  return applied;
}
