// Postgres through Bun.sql (design spec §2, §9). One pool per process
// (connect), transactions through withTx, and the small helpers every
// query module uses. The verified traps (research bun-platform.md §1):
// - always type a query: tx<UserRow[]>`select …` (the default row type is any);
// - arrays go through db.array(xs, 'TEXT') (textArray), never a plain JS array;
// - jsonb takes the object itself, never JSON.stringify(x);
// - a `date` column wants a 'YYYY-MM-DD' string, not a Date;
// - an empty IN list is a syntax error: use = any(${textArray(db, xs)});
// - queries are lazy: an unawaited query never runs (in tests,
//   expect(query).rejects spins forever: wrap it, test/db.ts settle());
// - never use the outer pool inside withTx (deadlocks at max 1);
// - int8/count(*)/bigserial come back as strings (toInt);
// - the SQLSTATE of a SQL.PostgresError is in errno, not code.

import { SQL, type SQLArrayParameter, type TransactionSQL } from 'bun';
import { readFileSync } from 'node:fs';
import type { DatabaseTls } from '../config';

/** The connection pool. */
export type Db = SQL;

/** A transaction (sql.begin's callback argument). Every service function takes one. */
export type Tx = TransactionSQL;

/** Something that runs queries: the pool or a transaction. */
export type Queryable = Db | Tx;

export interface ConnectOptions {
  url: string;
  /** Pool size (DATABASE_POOL_MAX); default 10. */
  max?: number;
  /** verify-full: TLS with the chain and the host name checked; disable: plaintext. */
  tls: DatabaseTls;
  /** CA bundle for verify-full (DATABASE_CA_FILE); default Bun's bundled roots. */
  caFile?: string | null;
  /** Seconds before an idle connection closes; default 30. */
  idleTimeout?: number;
  /** Seconds to wait for a connection; default 10. */
  connectionTimeout?: number;
}

/**
 * Opens a pool. Bun's own sslmode=require/prefer and `tls: true` encrypt
 * without checking the certificate, so an sslmode= parameter is refused here
 * and verify-full is spelled out as TLS options. Connections open lazily.
 */
export function connect(opts: ConnectOptions): Db {
  let url: URL;
  try {
    url = new URL(opts.url);
  } catch {
    throw new Error('the database URL is not a URL');
  }
  // sslmode=verify-full (what PlanetScale and others put in their URLs) and
  // sslrootcert=system mean what DATABASE_TLS=verify-full does; they are
  // taken out so Bun does not apply its own weaker reading. Anything else
  // is refused (config.ts says the same at startup).
  const sslmode = url.searchParams.get('sslmode');
  if (sslmode !== null && sslmode !== 'verify-full') {
    throw new Error(`the database URL has sslmode=${sslmode}: remove it or use verify-full (Bun's sslmode=require does not verify the certificate)`);
  }
  if (sslmode === 'verify-full' && opts.tls === 'disable') throw new Error('the database URL asks for sslmode=verify-full but DATABASE_TLS is disable');
  const rootcert = url.searchParams.get('sslrootcert');
  if (rootcert !== null && rootcert !== 'system') throw new Error('the database URL has an sslrootcert= other than system: set DATABASE_CA_FILE instead');
  url.searchParams.delete('sslmode');
  url.searchParams.delete('sslrootcert');
  const cleanUrl = url.toString();
  const common = {
    max: opts.max ?? 10,
    idleTimeout: opts.idleTimeout ?? 30,
    connectionTimeout: opts.connectionTimeout ?? 10,
  };
  if (opts.tls === 'disable') return new SQL(cleanUrl, { ...common, tls: false });
  const ca = opts.caFile ? readFileSync(opts.caFile, 'utf8') : undefined;
  return new SQL(cleanUrl, {
    ...common,
    tls: { rejectUnauthorized: true, serverName: url.hostname, ...(ca ? { ca } : {}) },
  });
}

/** Runs `fn` in one transaction: committed when it resolves, rolled back when it throws. */
export async function withTx<T>(db: Db, fn: (tx: Tx) => Promise<T>): Promise<T> {
  return await db.begin((tx) => fn(tx));
}

/** Whether `e` is a Postgres error, optionally with this SQLSTATE (e.errno). */
export function isPgError(e: unknown, errno?: string): e is SQL.PostgresError {
  return e instanceof SQL.PostgresError && (errno === undefined || e.errno === errno);
}

/** The SQLSTATEs the code reacts to. */
export const PG = {
  uniqueViolation: '23505',
  foreignKeyViolation: '23503',
  checkViolation: '23514',
  notNullViolation: '23502',
  invalidTextRepresentation: '22P02',
  serializationFailure: '40001',
  undefinedTable: '42P01',
} as const;

/** A text[] parameter (`= any(${textArray(tx, ids)})`, insert of a text[] column). Never pass a JS array directly. */
export function textArray(q: Queryable, values: readonly string[]): SQLArrayParameter {
  return q.array([...values], 'TEXT');
}

/** An int8/count(*)/bigserial value (a string from Bun.sql) as a number. */
export function toInt(v: string | number | bigint | null | undefined): number {
  if (v === null || v === undefined) return 0;
  return typeof v === 'number' ? v : Number(v);
}

/** The only row, or throws (a query that must hit exactly one row). */
export function one<T>(rows: readonly T[], what = 'row'): T {
  if (rows.length !== 1) throw new Error(`expected one ${what}, got ${rows.length}`);
  return rows[0]!;
}

/** The first row or null. */
export function maybeOne<T>(rows: readonly T[]): T | null {
  return rows[0] ?? null;
}

/** An RFC 3339 instant for a timestamptz column value. */
export function iso(d: Date): string {
  return d.toISOString();
}

/** iso for nullable columns. */
export function isoOrNull(d: Date | null | undefined): string | null {
  return d ? d.toISOString() : null;
}
