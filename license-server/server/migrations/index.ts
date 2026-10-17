// The migrations this build knows, in order (design spec §2). Listed by hand
// and imported as text so `bun build` bundles them into dist/server.js; the
// image carries no .sql files. server/test/migrate.test.ts checks the list
// against the directory. A migration is never edited after release: add
// 000N_<name>.sql and a line here.

import init from './0001_init.sql' with { type: 'text' };
import runtimeGrants from './0002_runtime_grants.sql' with { type: 'text' };
import approvalLinks from './0003_request_approval_links.sql' with { type: 'text' };
import userDelete from './0004_user_delete.sql' with { type: 'text' };
import businessRequests from './0005_business_requests.sql' with { type: 'text' };

export interface Migration {
  /** The number in the file name; schema_migrations.version. */
  version: number;
  /** The file name without .sql. */
  name: string;
  sql: string;
}

export const MIGRATIONS: readonly Migration[] = [
  { version: 1, name: '0001_init', sql: init },
  { version: 2, name: '0002_runtime_grants', sql: runtimeGrants },
  { version: 3, name: '0003_request_approval_links', sql: approvalLinks },
  { version: 4, name: '0004_user_delete', sql: userDelete },
  { version: 5, name: '0005_business_requests', sql: businessRequests },
];
