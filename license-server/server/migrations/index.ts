// The migrations this build knows, in order (design spec §2). Listed by hand
// and imported as text so `bun build` bundles them into dist/server.js; the
// image carries no .sql files. server/test/migrate.test.ts checks the list
// against the directory. A migration is never edited after release: add
// 0002_<name>.sql and a line here.

import init from './0001_init.sql' with { type: 'text' };

export interface Migration {
  /** The number in the file name; schema_migrations.version. */
  version: number;
  /** The file name without .sql. */
  name: string;
  sql: string;
}

export const MIGRATIONS: readonly Migration[] = [{ version: 1, name: '0001_init', sql: init }];
