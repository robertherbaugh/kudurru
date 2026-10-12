// Test fixtures: a complete Config with test values (testConfig), the
// golden signing key of golden/golden.json (public, test-only, kid
// server-test-1, never trusted by a kudurru release), and rows to start
// from: users with roles, customers with members, and signed-in sessions.

import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import type { CustomerKind } from '../../shared/api';
import type { Role } from '../../shared/permissions';
import { createSession, sessionCookieName } from '../auth/sessions';
import type { Config } from '../config';
import { newId } from '../db/ids';
import { textArray, type Queryable } from '../db/sql';
import golden from './golden/golden.json' with { type: 'json' };
import { TEST_DATABASE_URL } from './db';

/** The golden test key: the seed licensegen would write, its kid and its .pub content. */
export const GOLDEN = {
  kid: golden.kid,
  /** Std base64 of the 32-byte seed (the LICENSE_SIGNING_KEY format). */
  seed: golden.seed,
  /** Std base64 of the public key (the <kid>.pub format). */
  publicKey: golden.publicKey,
} as const;

/** The golden vectors (name, claims, key) Go's license.Sign produced. */
export const GOLDEN_VECTORS = golden.vectors;

let trustedDir: string | null = null;

/** A temporary trusted keys directory holding only server-test-1.pub (created once per process). */
export function goldenTrustedKeysDir(): string {
  if (!trustedDir) {
    trustedDir = mkdtempSync(join(tmpdir(), 'kls-trusted-'));
    writeFileSync(join(trustedDir, `${GOLDEN.kid}.pub`), `${GOLDEN.publicKey}\n`);
  }
  return trustedDir;
}

/** A partial Config: nested objects may be partial too. */
export type ConfigOverrides = { [K in keyof Config]?: Config[K] extends readonly unknown[] ? Config[K] : Config[K] extends object ? Partial<Config[K]> : Config[K] };

/**
 * A complete Config for tests: http://localhost (plain kls_session cookies),
 * dev mode with the golden key as LICENSE_SIGNING_KEY, no SMTP, no admin
 * group mappings, the spec's defaults elsewhere. Nested sections are merged
 * one level deep: testConfig({ personal: { autoApprove: false } }).
 */
export function testConfig(overrides: ConfigOverrides = {}): Config {
  const base: Config = {
    publicUrl: 'http://localhost:8790',
    publicOrigin: 'http://localhost:8790',
    secureCookies: false,
    host: '127.0.0.1',
    port: 0,
    devMode: true,
    logLevel: 'debug',
    database: {
      url: TEST_DATABASE_URL || 'postgres://postgres@127.0.0.1:5432/postgres',
      migrateUrl: null,
      tls: 'disable',
      tlsExplicit: true,
      caFile: null,
      poolMax: 4,
      migrateOnStart: false,
    },
    oidc: {
      issuer: 'http://127.0.0.1:5556/dex',
      clientId: 'license-portal',
      clientSecret: 'license-portal-test-secret',
      scopes: ['openid', 'profile', 'email', 'groups'],
      displayName: 'Sign in',
      groupsClaim: 'groups',
      idTokenAlg: null,
      adminClaimRules: [],
    },
    roles: {
      groups: { superadmin: [], license_admin: [], customer: [] },
      defaultRole: 'customer',
      memberAutoLink: false,
    },
    signing: {
      keyFile: null,
      keyEnv: GOLDEN.seed,
      kid: GOLDEN.kid,
      trustedKeysDir: goldenTrustedKeysDir(),
      trustedSince: null,
      required: false,
    },
    session: { idleTimeoutMs: 30 * 60_000, lifetimeMs: 12 * 3_600_000 },
    trustedProxies: [],
    mail: {
      smtpUrl: null,
      from: 'kudurru license portal <licenses@kudurru.io>',
      replyTo: 'hello@kudurru.io',
      adminNotify: 'admin@example.org',
    },
    publicForm: { enabled: true, origins: ['https://kudurru.io'], limitPerIpPerHour: 5, limitPerEmailPerDay: 3 },
    personal: { autoApprove: true, autoApproveMaxPerDay: 50 },
    issuing: { maxPerActorPerDay: 50 },
    build: { version: 'test', commit: 'test', builtAt: null },
  };
  const out: Record<string, unknown> = { ...base };
  for (const [k, v] of Object.entries(overrides)) {
    const cur = (base as unknown as Record<string, unknown>)[k];
    out[k] = v !== null && typeof v === 'object' && !Array.isArray(v) && cur !== null && typeof cur === 'object' ? { ...cur, ...v } : v;
  }
  return out as unknown as Config;
}

export interface TestUser {
  id: string;
  name: string;
  email: string | null;
  issuer: string;
  subject: string;
  idpRoles: Role[];
  grantedRoles: Role[];
}

export interface CreateUserOptions {
  /** Granted roles (users.granted_roles); default ['customer']. */
  roles?: Role[];
  /** IdP roles (users.idp_roles); default []. */
  idpRoles?: Role[];
  /** Default: a unique @example.org address; null for a user without a verified email. */
  email?: string | null;
  name?: string;
  disabled?: boolean;
  issuer?: string;
  subject?: string;
}

let counter = 0;

function unique(): string {
  counter += 1;
  return `${process.pid}-${counter}-${Math.random().toString(36).slice(2, 8)}`;
}

/** Inserts a user. */
export async function createUser(db: Queryable, opts: CreateUserOptions = {}): Promise<TestUser> {
  const u = unique();
  const user: TestUser = {
    id: newId('user'),
    name: opts.name ?? `Test User ${u}`,
    email: opts.email === undefined ? `user-${u}@example.org` : opts.email,
    issuer: opts.issuer ?? 'http://127.0.0.1:5556/dex',
    subject: opts.subject ?? `sub-${u}`,
    idpRoles: opts.idpRoles ?? [],
    grantedRoles: opts.roles ?? ['customer'],
  };
  await db`
    insert into users (id, issuer, subject, email, email_verified, name, idp_roles, granted_roles, disabled_at, last_login_at)
    values (${user.id}, ${user.issuer}, ${user.subject}, ${user.email}, ${user.email !== null}, ${user.name},
            ${textArray(db, user.idpRoles)}, ${textArray(db, user.grantedRoles)}, ${opts.disabled ? new Date() : null}, ${new Date()})`;
  return user;
}

export interface TestCustomer {
  id: string;
  name: string;
  kind: CustomerKind;
  contactEmail: string | null;
}

export interface CreateCustomerOptions {
  name?: string;
  kind?: CustomerKind;
  contactEmail?: string | null;
  /** Member rows: an email (invited, no token) or an email linked to a user. */
  members?: (string | { email: string; userId: string })[];
}

/** Inserts a customer and its member rows. */
export async function createCustomer(db: Queryable, opts: CreateCustomerOptions = {}): Promise<TestCustomer> {
  const customer: TestCustomer = {
    id: newId('customer'),
    name: opts.name ?? `Customer ${unique()}`,
    kind: opts.kind ?? 'organization',
    contactEmail: opts.contactEmail === undefined ? null : opts.contactEmail,
  };
  await db`
    insert into customers (id, name, kind, contact_email, created_by_label)
    values (${customer.id}, ${customer.name}, ${customer.kind}, ${customer.contactEmail}, ${'test'})`;
  for (const m of opts.members ?? []) {
    const email = (typeof m === 'string' ? m : m.email).toLowerCase();
    const userId = typeof m === 'string' ? null : m.userId;
    await db`
      insert into customer_members (customer_id, email, user_id, invited_by_label, linked_at)
      values (${customer.id}, ${email}, ${userId}, ${'test'}, ${userId ? new Date() : null})`;
  }
  return customer;
}

/** What a test sends to act as a signed-in user (call.ts `as`). */
export interface TestSession {
  /** The Cookie header value: name=secret. */
  cookie: string;
  /** The X-Kudurru-CSRF value. */
  csrf: string;
  cookieValue: string;
}

/** Creates a session for the user, as a completed sign-in would. */
export async function signInAs(db: Queryable, user: Pick<TestUser, 'id'>, config: Config = testConfig(), now: Date = new Date()): Promise<TestSession> {
  const s = await createSession(db, { userId: user.id, authTime: now, ip: '127.0.0.1', userAgent: 'bun test', config, now });
  return { cookie: `${sessionCookieName(config)}=${s.cookieValue}`, csrf: s.csrf, cookieValue: s.cookieValue };
}
