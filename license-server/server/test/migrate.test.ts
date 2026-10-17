// The migration runner and the schema of 0001_init.sql and later (design
// spec §2, §4): migrations apply cleanly and only once (also when two
// instances start together), a database migrated by a newer release is
// refused, a broken migration leaves nothing behind, the static list
// matches the directory, audit_events is append-only, and the constraints
// the invariants rest on hold (approval links included).
import { afterAll, beforeAll, expect, test } from 'bun:test';
import { readdirSync } from 'node:fs';
import { join } from 'node:path';
import { migrate, migrationStatus, UnknownMigrationError } from '../db/migrate';
import { connect, type Db } from '../db/sql';
import { MIGRATIONS } from '../migrations';

/** Every version this build knows. */
const ALL = MIGRATIONS.map((m) => m.version);
import { TEST_DATABASE_URL, databaseUrl, describeDb, hasDb, settle as run } from './db';

// No database needed: the bundle must carry every .sql file, in order.
test('MIGRATIONS lists every file of server/migrations, in order', () => {
  const files = readdirSync(join(import.meta.dir, '..', 'migrations'))
    .filter((f) => f.endsWith('.sql'))
    .sort();
  expect(MIGRATIONS.map((m) => `${m.name}.sql`)).toEqual(files);
  MIGRATIONS.forEach((m, i) => {
    expect(m.version).toBe(i + 1);
    expect(m.sql.length).toBeGreaterThan(0);
  });
});

describeDb('migrations against Postgres', () => {
  // A fresh, unmigrated database per test file (useTestDb would hand out a migrated one).
  const name = `kls_migrate_${process.pid}_${Math.random().toString(36).slice(2, 8)}`;
  let admin: Db;
  let db: Db;

  beforeAll(async () => {
    admin = connect({ url: TEST_DATABASE_URL, tls: 'disable', max: 1 });
    await admin.unsafe(`create database "${name}"`);
    db = connect({ url: databaseUrl(name), tls: 'disable', max: 4 });
  });

  afterAll(async () => {
    if (!hasDb) return;
    await db.close({ timeout: 1 });
    await admin.unsafe(`drop database if exists "${name}" with (force)`);
    await admin.close();
  });

  test('two instances starting together apply each migration once', async () => {
    expect(await migrationStatus(db)).toEqual({ applied: [], pending: ALL, unknown: [] });
    const [a, b] = await Promise.all([migrate(db), migrate(db)]);
    expect([...a, ...b].sort((x, y) => x - y)).toEqual(ALL);
    expect(await migrationStatus(db)).toEqual({ applied: ALL, pending: [], unknown: [] });
  });

  test('a second run is a no-op', async () => {
    expect(await migrate(db)).toEqual([]);
    const rows = await db<{ version: number; name: string }[]>`select version, name from schema_migrations order by version`;
    expect(rows).toEqual(MIGRATIONS.map((m) => ({ version: m.version, name: m.name })));
  });

  test('every table of spec §4 exists', async () => {
    const rows = await db<{ table_name: string }[]>`select table_name from information_schema.tables where table_schema = 'public' order by 1`;
    expect(rows.map((r) => r.table_name)).toEqual([
      'audit_events',
      'blocked_emails',
      'business_requests',
      'customer_members',
      'customers',
      'license_keys',
      'license_requests',
      'licenses',
      'oidc_logins',
      'public_key_requests',
      'request_approval_links',
      'schema_migrations',
      'sessions',
      'users',
    ]);
  });

  // A database migrated by a newer release: refuse before touching it.
  test('an unknown version is refused', async () => {
    await db`insert into schema_migrations (version, name) values (999, '0999_future')`;
    try {
      expect((await migrationStatus(db)).unknown).toEqual([999]);
      const err = await migrate(db).catch((e: unknown) => e);
      expect(err).toBeInstanceOf(UnknownMigrationError);
      expect((err as UnknownMigrationError).versions).toEqual([999]);
    } finally {
      await db`delete from schema_migrations where version = 999`;
    }
  });

  test('a failing migration rolls back its whole file', async () => {
    const next = MIGRATIONS.length + 1;
    const broken = [...MIGRATIONS, { version: next, name: `${String(next).padStart(4, '0')}_broken`, sql: 'create table half_done (a int); select no_such_column from users;' }];
    await expect(migrate(db, { migrations: broken })).rejects.toMatchObject({ errno: '42703' });
    const tables = await db<{ n: string }[]>`select count(*) as n from information_schema.tables where table_name = 'half_done'`;
    expect(tables[0]!.n).toBe('0');
    expect((await migrationStatus(db)).applied).toEqual(ALL);
  });

  test('audit_events refuses UPDATE, DELETE and TRUNCATE', async () => {
    await db`insert into audit_events (actor_label, action, target_type, target_id) values ('test', 'customer.created', 'customer', 'cus_x')`;
    await expect(run(db`update audit_events set action = 'x'`)).rejects.toMatchObject({ errno: '42501' });
    await expect(run(db`delete from audit_events`)).rejects.toMatchObject({ errno: '42501' });
    await expect(run(db`truncate audit_events`)).rejects.toMatchObject({ errno: '42501' });
    const rows = await db<{ action: string }[]>`select action from audit_events`;
    expect(rows).toEqual([{ action: 'customer.created' }]);
  });

  test('the schema keeps the invariants', async () => {
    await db`insert into customers (id, name, kind) values ('cus_inv', 'Invariant Ltd', 'organization')`;
    const license = (id: string, features: Bun.SQLArrayParameter | null) =>
      db`insert into licenses (id, customer_id, licensee, plan, nodes, features, expires_at, source)
         values (${id}, 'cus_inv', 'Invariant Ltd', 'business', 5, ${features}, now() + interval '1 year', 'admin')`;
    await license('lic_inv', null);
    // features is null (all) or a non-empty list, never [].
    await expect(run(license('lic_empty', db.array([], 'TEXT')))).rejects.toMatchObject({ errno: '23514' });
    const key = (id: string) =>
      db`insert into license_keys (id, license_id, kid, token, claims, issued_at, expires_at, reason)
         values (${id}, 'lic_inv', 'server-test-1', 'x.y.z', ${{ jti: 'lic_inv' }}, now(), now() + interval '1 year', 'issue')`;
    await key('key_one');
    // One current (unsuperseded) key per license.
    await expect(run(key('key_two'))).rejects.toMatchObject({ errno: '23505', constraint: 'license_keys_current_uq' });
    // Emails are stored lower-cased.
    await expect(run(db`insert into blocked_emails (email) values ('Someone@Example.org')`)).rejects.toMatchObject({ errno: '23514' });
    // At most one open request per email.
    const request = (id: string) =>
      db`insert into license_requests (id, source, requester_email, name, use_description, attestation, review_reason)
         values (${id}, 'web', 'p@example.org', 'P', 'homelab', ${{ source: 'kudurru.io form' }}, 'daily_cap')`;
    await request('req_one');
    await expect(run(request('req_two'))).rejects.toMatchObject({ errno: '23505', constraint: 'license_requests_open_email_uq' });
    // An approval link is stored with its address lower-cased and goes with its request.
    const link = (to: string) =>
      db`insert into request_approval_links (token_hash, request_id, sent_to, expires_at) values (${Buffer.from(to)}, 'req_one', ${to}, now() + interval '7 days')`;
    await expect(run(link('Admin@Example.org'))).rejects.toMatchObject({ errno: '23514' });
    await link('admin@example.org');
    await db`delete from license_requests where id = 'req_one'`;
    expect(await db`select 1 from request_approval_links`).toHaveLength(0);
    // Roles are the three known ones.
    await expect(run(db`insert into users (id, issuer, subject, granted_roles) values ('usr_r', 'i', 's', ${db.array(['root'], 'TEXT')})`)).rejects.toMatchObject({ errno: '23514' });
  });
});
