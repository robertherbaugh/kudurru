// grant-superadmin (design spec §5 "First superadmin and break-glass"):
// the argument forms and their usage errors, a grant and a revoke by
// verified email and by issuer and subject, the printed user, the audit
// event with actor_label "cli", ended sessions, refusals for an unverified
// or unknown email and for an email two users hold, and no-ops. Also the
// built entry point: `bun server/main.ts grant-superadmin` exits 2 on a
// usage error before it reads any configuration.
import { describe, expect, test } from 'bun:test';
import { join } from 'node:path';
import { resolveSession } from '../auth/sessions';
import { grantSuperadmin, parseGrantArgs, UsageError, type CliIo } from '../cli';
import { describeDb, useTestDb } from './db';
import { createUser, signInAs, testConfig } from './fixtures';

describe('arguments', () => {
  test('the forms', () => {
    expect(parseGrantArgs(['--email', 'ana@example.org'])).toEqual({ email: 'ana@example.org', issuer: null, subject: null, revoke: false });
    expect(parseGrantArgs(['--email=ana@example.org', '--revoke'])).toEqual({ email: 'ana@example.org', issuer: null, subject: null, revoke: true });
    expect(parseGrantArgs(['--issuer', 'https://idp.example', '--subject', 'abc', '--revoke'])).toEqual({ email: null, issuer: 'https://idp.example', subject: 'abc', revoke: true });
  });

  test.each([
    [[], 'give either --email or --issuer and --subject'],
    [['--email', 'a@b.c', '--subject', 's', '--issuer', 'i'], 'give either --email or --issuer and --subject'],
    [['--issuer', 'https://idp.example'], '--issuer and --subject go together'],
    [['--email'], '--email needs a value'],
    [['--email', '--revoke'], '--email needs a value'],
    [['--email', 'a@b.c', '--email', 'd@e.f'], '--email is given twice'],
    [['--mail', 'a@b.c'], 'unknown argument "--mail"'],
    [['ana@example.org'], 'unknown argument'],
  ])('%j → %s', (argv, message) => {
    expect(() => parseGrantArgs(argv)).toThrow(UsageError);
    expect(() => parseGrantArgs(argv)).toThrow(message);
  });

  test('the entry point exits 2 on a usage error', async () => {
    const proc = Bun.spawn(['bun', join(import.meta.dir, '..', 'main.ts'), 'grant-superadmin', '--bogus'], { stderr: 'pipe', stdout: 'pipe', env: { PATH: process.env.PATH ?? '' } });
    expect(await proc.exited).toBe(2);
    expect(await new Response(proc.stderr).text()).toContain('usage: grant-superadmin');
  });
});

describeDb('grant-superadmin', () => {
  const t = useTestDb();

  function io(): CliIo & { out: (l: string) => void; lines: string[]; errors: string[] } {
    const lines: string[] = [];
    const errors: string[] = [];
    return { lines, errors, out: (l) => void lines.push(l), err: (l) => void errors.push(l) };
  }

  const rolesOf = async (id: string) => (await t.db<{ granted_roles: string[] }[]>`select granted_roles from users where id = ${id}`)[0]!.granted_roles;

  test('grant by verified email, then revoke; audited as cli; sessions end', async () => {
    const user = await createUser(t.db, { email: 'first-admin@example.org', roles: ['customer'], name: 'First Admin' });
    const config = testConfig();
    const session = await signInAs(t.db, user, config);

    const out = io();
    expect(await grantSuperadmin(t.db, parseGrantArgs(['--email', ' First-Admin@Example.org ']), out)).toBe(0);
    expect(await rolesOf(user.id)).toEqual(['customer', 'superadmin']);
    const printed = out.lines.join('\n');
    expect(printed).toContain(`user     ${user.id}`);
    expect(printed).toContain('name     First Admin');
    expect(printed).toContain('email    first-admin@example.org (verified)');
    expect(printed).toContain(`subject  ${user.subject}`);
    expect(printed).toContain('granted superadmin to');
    expect(printed).toContain('1 session(s) ended');
    expect(await resolveSession(t.db, session.cookieValue, new Date(), config)).toBeNull();

    const events = await t.db<{ actor_id: string | null; actor_label: string; details: Record<string, unknown> }[]>`
      select actor_id, actor_label, details from audit_events where action = 'user.roles_changed' and target_id = ${user.id} order by id`;
    expect(events).toHaveLength(1);
    expect(events[0]!.actor_id).toBeNull();
    expect(events[0]!.actor_label).toBe('cli');
    expect(events[0]!.details).toMatchObject({ grantedRoles: { before: ['customer'], after: ['customer', 'superadmin'] }, change: 'superadmin granted', via: 'grant-superadmin' });

    // Again: nothing to do, no event.
    const again = io();
    expect(await grantSuperadmin(t.db, parseGrantArgs(['--email', 'first-admin@example.org']), again)).toBe(0);
    expect(again.lines.at(-1)).toBe('superadmin is already granted to this user; nothing changed');

    const revoke = io();
    expect(await grantSuperadmin(t.db, parseGrantArgs(['--email', 'first-admin@example.org', '--revoke']), revoke)).toBe(0);
    expect(await rolesOf(user.id)).toEqual(['customer']);
    expect(revoke.lines.join('\n')).toContain('revoked superadmin from');
    const after = await t.db<{ details: { change: string } }[]>`select details from audit_events where action = 'user.roles_changed' and target_id = ${user.id} order by id`;
    expect(after.map((e) => e.details.change)).toEqual(['superadmin granted', 'superadmin revoked']);
    const noop = io();
    expect(await grantSuperadmin(t.db, parseGrantArgs(['--email', 'first-admin@example.org', '--revoke']), noop)).toBe(0);
    expect(noop.lines.at(-1)).toBe('superadmin is not granted to this user; nothing changed');
  });

  test('by issuer and subject (a user without a verified email, DEFAULT_ROLE=none)', async () => {
    const user = await createUser(t.db, { email: null, roles: [], issuer: 'https://idp.example.org', subject: 'opaque-sub-1' });
    const out = io();
    expect(await grantSuperadmin(t.db, parseGrantArgs(['--issuer', 'https://idp.example.org', '--subject', 'opaque-sub-1']), out)).toBe(0);
    expect(await rolesOf(user.id)).toEqual(['superadmin']);
    expect(out.lines.join('\n')).toContain('email    (none verified)');
    // Issuer and subject are exact.
    const miss = io();
    expect(await grantSuperadmin(t.db, parseGrantArgs(['--issuer', 'https://idp.example.org/', '--subject', 'opaque-sub-1']), miss)).toBe(1);
    expect(miss.errors[0]).toContain('no user has issuer');
  });

  test('refusals: unknown, unverified, and an email two users hold', async () => {
    const unknown = io();
    expect(await grantSuperadmin(t.db, parseGrantArgs(['--email', 'nobody@example.org']), unknown)).toBe(1);
    expect(unknown.errors[0]).toContain('no user holds nobody@example.org as a verified email');

    // Stored only when verified: an unverified address is null in users.email.
    await t.db`insert into users (id, issuer, subject, email, email_verified) values ('usr_unverified1', 'https://idp.example.org', 'unv-1', null, false)`;
    expect(await grantSuperadmin(t.db, parseGrantArgs(['--email', 'unverified@example.org']), io())).toBe(1);

    const a = await createUser(t.db, { email: 'shared@example.org', issuer: 'https://idp.example.org', subject: 'google-1' });
    const b = await createUser(t.db, { email: 'shared@example.org', issuer: 'https://idp.example.org', subject: 'github-1' });
    const both = io();
    expect(await grantSuperadmin(t.db, parseGrantArgs(['--email', 'shared@example.org']), both)).toBe(1);
    expect(both.errors[0]).toContain('2 users hold shared@example.org');
    expect(both.errors.join('\n')).toContain('google-1');
    expect(both.errors.join('\n')).toContain('github-1');
    expect(await rolesOf(a.id)).toEqual(['customer']);
    expect(await rolesOf(b.id)).toEqual(['customer']);
  });

  test('a disabled user is granted with a note', async () => {
    const user = await createUser(t.db, { email: 'disabled-admin@example.org', disabled: true });
    const out = io();
    expect(await grantSuperadmin(t.db, parseGrantArgs(['--email', 'disabled-admin@example.org']), out)).toBe(0);
    expect(out.lines.join('\n')).toContain('note: the user is disabled');
    expect(await rolesOf(user.id)).toEqual(['customer', 'superadmin']);
  });
});
