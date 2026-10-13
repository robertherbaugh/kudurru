// Configuration (design spec §3): every environment variable, its default
// and the typed Config the server runs with. Secret URLs and the client
// secret also come from a file: FOO_FILE names a file whose trimmed content
// is FOO (Docker and Kubernetes secrets). The signing key's file form is its
// own variable, LICENSE_SIGNING_KEY_FILE. CONFIG_VARS is the one table of variables: loadConfig
// reads it, the README documents it and the System page shows the
// non-secret part.
//
// loadConfig parses and checks types and required values; validateConfig
// then refuses the settings the spec (§0.6, §2, §3) says the server must not
// start with, each with one sentence naming the variable and the fix:
// an sslmode= in a database URL other than verify-full (Bun's
// sslmode=require does not verify the certificate), a non-Postgres database
// URL, a signing key without its kid (or a kid without a key),
// DEFAULT_ROLE=customer with admin group mappings but no
// OIDC_ADMIN_CLAIM_RULES, an http OIDC issuer unless it is a loopback
// literal and PUBLIC_URL is http too, a PUBLIC_URL that is more than an
// origin, TRUSTED_PROXIES entries that are not CIDRs, a CLIENT_IP_HEADER that
// is not a header name and PUBLIC_FORM_ORIGINS entries that are not exact
// origins. No message ever repeats a secret's value.
//
// The signing key: LICENSE_SIGNING_KEY_FILE where secrets are files (Docker,
// Kubernetes); LICENSE_SIGNING_KEY where the platform hands secrets over as
// environment variables only (Cloudflare Containers, spec §13).

import { accessSync, constants, readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import type { Role } from '../shared/permissions';
import { parseCidr } from './http/ip';
import { isLogLevel, type LogLevel } from './log';

export type DatabaseTls = 'verify-full' | 'disable';

/** DEFAULT_ROLE: what a sign-in that maps to no role gets. */
export type DefaultRole = 'customer' | 'none';

/** One OIDC_ADMIN_CLAIM_RULES entry: the claim must equal the value exactly (JSON-text compare). */
export interface ClaimRule {
  claim: string;
  value: string;
}

export interface Config {
  /** PUBLIC_URL without a trailing slash, e.g. https://licenses.kudurru.io. */
  publicUrl: string;
  /** The origin of publicUrl (CSRF Origin check). */
  publicOrigin: string;
  /** publicUrl is https: Secure cookies with the __Host- prefix and HSTS. */
  secureCookies: boolean;
  host: string;
  port: number;
  /** LICENSE_SERVER_DEV: env-form signing key, test kids, the dev banner. */
  devMode: boolean;
  logLevel: LogLevel;
  database: {
    /** Runtime role (DML). Secret. */
    url: string;
    /** Owner role for migrations; null: url. Secret. */
    migrateUrl: string | null;
    tls: DatabaseTls;
    /** DATABASE_TLS was given explicitly (disable is allowed only then). */
    tlsExplicit: boolean;
    caFile: string | null;
    poolMax: number;
    migrateOnStart: boolean;
  };
  oidc: {
    issuer: string;
    clientId: string;
    /** Secret. */
    clientSecret: string;
    scopes: string[];
    displayName: string;
    groupsClaim: string;
    idTokenAlg: string | null;
    adminClaimRules: ClaimRule[];
  };
  roles: {
    /** IdP groups per role (ROLE_SUPERADMIN_GROUPS, ROLE_LICENSE_ADMIN_GROUPS, ROLE_CUSTOMER_GROUPS). */
    groups: Record<Role, string[]>;
    defaultRole: DefaultRole;
    /** MEMBER_AUTO_LINK: link member rows by IdP-verified email at sign-in. */
    memberAutoLink: boolean;
  };
  signing: {
    /** LICENSE_SIGNING_KEY_FILE: std base64 of the 32-byte seed (licensegen format). */
    keyFile: string | null;
    /** LICENSE_SIGNING_KEY: the seed itself, for platforms that hand secrets over as environment variables only. Secret: never logged, never shown. */
    keyEnv: string | null;
    kid: string | null;
    /** Directory of <kid>.pub files, as the kudurru releases embed them. */
    trustedKeysDir: string;
    /** LICENSE_SIGNING_TRUSTED_SINCE, e.g. v0.1.0. */
    trustedSince: string | null;
    /** REQUIRE_SIGNING_KEY: no usable key is fatal. */
    required: boolean;
  };
  session: {
    idleTimeoutMs: number;
    lifetimeMs: number;
  };
  /** TRUSTED_PROXIES as given (CIDRs); server/http/ip.ts parses them. */
  trustedProxies: string[];
  /**
   * CLIENT_IP_HEADER: a request header that holds the client address, set by
   * the only proxy in front of the server (the Cloudflare Worker sets
   * X-Client-IP from CF-Connecting-IP). Believed on every request, so only
   * for a server no client can reach without that proxy. Lower-case.
   */
  clientIpHeader: string | null;
  mail: {
    /** SMTP_URL; null: mail is not configured. Secret. */
    smtpUrl: string | null;
    from: string;
    replyTo: string;
    adminNotify: string | null;
  };
  publicForm: {
    /** False when PUBLIC_FORM_ORIGINS is "-": the public routes answer 404. */
    enabled: boolean;
    /** Exact origins allowed to POST the form. */
    origins: string[];
    limitPerIpPerHour: number;
    limitPerEmailPerDay: number;
  };
  personal: {
    autoApprove: boolean;
    autoApproveMaxPerDay: number;
  };
  issuing: {
    maxPerActorPerDay: number;
  };
  build: {
    version: string;
    commit: string;
    builtAt: string | null;
  };
}

/** One environment variable. */
export interface ConfigVar {
  readonly name: string;
  /** The default as the operator writes it; null: unset. */
  readonly default: string | null;
  readonly required?: true;
  /** Never logged or shown. */
  readonly secret?: true;
  /** Also read from the file NAME_FILE names (Docker and Kubernetes secrets). */
  readonly file?: true;
  readonly description: string;
}

/** The repository's trusted keys (internal/license/keys), the default outside the image. */
export const REPO_TRUSTED_KEYS_DIR = fileURLToPath(new URL('../../internal/license/keys', import.meta.url));

/** Every variable of spec §3, with its default. */
export const CONFIG_VARS = [
  { name: 'PUBLIC_URL', default: null, required: true, description: 'External URL of the portal: the bare origin (scheme, host, port; no path). Cookie Secure, __Host- and HSTS follow its scheme' },
  { name: 'HOST', default: '0.0.0.0', description: 'Listen address' },
  { name: 'PORT', default: '8790', description: 'Listen port' },
  { name: 'LICENSE_SERVER_DEV', default: 'false', description: 'Dev mode: test signing keys allowed, a development banner in the UI' },
  { name: 'DATABASE_URL', default: null, file: true, required: true, secret: true, description: 'Postgres URL of the runtime role (DML)' },
  { name: 'DATABASE_MIGRATE_URL', default: null, file: true, secret: true, description: 'Postgres URL of the owner role for migrations' },
  { name: 'DATABASE_TLS', default: 'verify-full', description: 'verify-full or disable' },
  { name: 'DATABASE_CA_FILE', default: null, description: "CA bundle for verify-full (else Bun's bundled roots)" },
  { name: 'DATABASE_POOL_MAX', default: '10', description: 'Connection pool size' },
  { name: 'MIGRATE_ON_START', default: 'true', description: 'Run pending migrations at startup' },
  { name: 'OIDC_ISSUER', default: null, required: true, description: 'OIDC issuer URL, exactly as the IdP writes it; https (http only for a loopback issuer while PUBLIC_URL is http)' },
  { name: 'OIDC_CLIENT_ID', default: null, required: true, description: 'OIDC client id' },
  { name: 'OIDC_CLIENT_SECRET', default: null, file: true, required: true, secret: true, description: 'OIDC client secret' },
  { name: 'OIDC_SCOPES', default: 'openid profile email', description: 'Space-separated scopes; add groups for IdPs that support it' },
  { name: 'OIDC_DISPLAY_NAME', default: 'Sign in', description: 'Sign-in button label' },
  { name: 'OIDC_GROUPS_CLAIM', default: 'groups', description: 'The only claim read for groups (string or array)' },
  { name: 'OIDC_ID_TOKEN_ALG', default: null, description: 'Pin the ID token alg' },
  { name: 'OIDC_ADMIN_CLAIM_RULES', default: null, description: 'claim=value comma list; all must match before admin group mappings are honored' },
  { name: 'ROLE_SUPERADMIN_GROUPS', default: '', description: 'IdP groups mapped to superadmin (comma list)' },
  { name: 'ROLE_LICENSE_ADMIN_GROUPS', default: '', description: 'IdP groups mapped to license_admin (comma list)' },
  { name: 'ROLE_CUSTOMER_GROUPS', default: '', description: 'IdP groups mapped to customer (comma list)' },
  { name: 'DEFAULT_ROLE', default: 'customer', description: 'customer or none: the role of a sign-in that maps to nothing' },
  { name: 'MEMBER_AUTO_LINK', default: 'false', description: 'Also link member rows by IdP-verified email at sign-in' },
  { name: 'LICENSE_SIGNING_KEY_FILE', default: null, description: 'File with the signing seed (licensegen format)' },
  { name: 'LICENSE_SIGNING_KID', default: null, description: 'Key id of the signing key (a plain file name: <kid>.pub in the trusted keys directory)' },
  // Not read from LICENSE_SIGNING_KEY_FILE: that is the file form above, the production one.
  { name: 'LICENSE_SIGNING_KEY', default: null, secret: true, description: 'The signing seed itself, for platforms that pass secrets as environment variables only (Cloudflare Containers); elsewhere prefer LICENSE_SIGNING_KEY_FILE' },
  { name: 'LICENSE_TRUSTED_KEYS_DIR', default: REPO_TRUSTED_KEYS_DIR, description: 'Directory of <kid>.pub files kudurru releases embed (image: /app/trusted-keys)' },
  { name: 'LICENSE_SIGNING_TRUSTED_SINCE', default: null, description: 'First kudurru release that trusts the signing kid, e.g. v0.1.0' },
  { name: 'REQUIRE_SIGNING_KEY', default: 'false', description: 'Refuse to start without a usable signing key' },
  { name: 'SESSION_IDLE_TIMEOUT', default: '30m', description: 'Session idle timeout' },
  { name: 'SESSION_LIFETIME', default: '12h', description: 'Absolute session lifetime' },
  { name: 'TRUSTED_PROXIES', default: '', description: 'CIDRs whose X-Forwarded-For is believed (comma list)' },
  { name: 'CLIENT_IP_HEADER', default: null, description: 'A header the only proxy in front of the server sets to the client address (X-Client-IP behind the Cloudflare Worker)' },
  { name: 'SMTP_URL', default: null, file: true, secret: true, description: 'smtp://user:pass@host:587 (STARTTLS) or smtps://…:465' },
  { name: 'MAIL_FROM', default: 'kudurru license portal <licenses@kudurru.io>', description: 'From header of every mail' },
  { name: 'MAIL_REPLY_TO', default: 'hello@kudurru.io', description: 'Reply-To header of every mail' },
  { name: 'ADMIN_NOTIFY_EMAIL', default: null, description: 'Gets a mail for each request that waits for review' },
  { name: 'PUBLIC_FORM_ORIGINS', default: 'https://kudurru.io', description: 'Exact origins of the public form (comma list); - disables the public routes' },
  { name: 'PUBLIC_LIMIT_PER_IP_PER_HOUR', default: '5', description: 'Public form requests per IPv4 address or IPv6 /64 per hour' },
  { name: 'PUBLIC_LIMIT_PER_EMAIL_PER_DAY', default: '3', description: 'Public form requests per email per day (silent 202 beyond)' },
  { name: 'PERSONAL_AUTO_APPROVE', default: 'true', description: 'A confirmed personal request gets a key without review' },
  { name: 'PERSONAL_AUTO_APPROVE_MAX_PER_DAY', default: '50', description: 'Beyond this many automatic personal keys a day, requests wait for review' },
  { name: 'ISSUE_MAX_PER_ACTOR_PER_DAY', default: '50', description: 'Admin signatures per actor per 24 h (429 issuing_quota)' },
  { name: 'LOG_LEVEL', default: 'info', description: 'debug, info, warn or error' },
] as const satisfies readonly ConfigVar[];

export type ConfigVarName = (typeof CONFIG_VARS)[number]['name'];

/** The environment as loadConfig reads it. */
export type Env = Readonly<Record<string, string | undefined>>;

/** A configuration the server refuses to start with; the message names the variable and the fix. */
export class ConfigError extends Error {
  override readonly name = 'ConfigError';
}

const UNITS: Record<string, number> = { ms: 1, s: 1000, m: 60_000, h: 3_600_000, d: 86_400_000 };

/**
 * Parses a Go-style duration ('30m', '12h', '1h30m', '90s', '500ms', '7d')
 * into milliseconds. Throws a RangeError on anything else, including a
 * bare number and a negative value.
 */
export function parseDuration(s: string): number {
  const text = s.trim();
  if (!/^(\d+(\.\d+)?(ms|s|m|h|d))+$/.test(text)) throw new RangeError(`not a duration (use 30m, 12h, 1h30m): ${JSON.stringify(s)}`);
  let total = 0;
  for (const m of text.matchAll(/(\d+(?:\.\d+)?)(ms|s|m|h|d)/g)) total += Number(m[1]) * (UNITS[m[2]!] ?? 0);
  return Math.round(total);
}

/** Splits a comma list, trimming entries and dropping empty ones. */
export function commaList(s: string | null | undefined): string[] {
  return (s ?? '')
    .split(',')
    .map((x) => x.trim())
    .filter((x) => x !== '');
}

/** Reads NAME, or for `file` variables the trimmed content of the file NAME_FILE names (not both). */
function raw(env: Env, v: ConfigVar): string | null {
  const direct = env[v.name];
  const file = v.file ? env[`${v.name}_FILE`] : undefined;
  if (file !== undefined && file !== '') {
    if (direct !== undefined && direct !== '') throw new ConfigError(`${v.name} and ${v.name}_FILE are both set; set one`);
    try {
      return readFileSync(file, 'utf8').trim();
    } catch (e) {
      throw new ConfigError(`${v.name}_FILE: cannot read ${file}: ${(e as Error).message}`);
    }
  }
  if (direct !== undefined && direct !== '') return direct.trim();
  return v.default;
}

function bool(name: string, s: string): boolean {
  const v = s.toLowerCase();
  if (v === 'true' || v === '1' || v === 'yes') return true;
  if (v === 'false' || v === '0' || v === 'no') return false;
  throw new ConfigError(`${name} must be true or false, got ${JSON.stringify(s)}`);
}

function int(name: string, s: string, min: number, max: number): number {
  const n = Number(s);
  if (!Number.isInteger(n) || n < min || n > max) throw new ConfigError(`${name} must be a whole number from ${min} to ${max}, got ${JSON.stringify(s)}`);
  return n;
}

function duration(name: string, s: string): number {
  try {
    return parseDuration(s);
  } catch (e) {
    throw new ConfigError(`${name}: ${(e as Error).message}`);
  }
}

function claimRules(s: string | null): ClaimRule[] {
  return commaList(s).map((pair) => {
    const i = pair.indexOf('=');
    const claim = i > 0 ? pair.slice(0, i).trim() : '';
    const value = i > 0 ? pair.slice(i + 1).trim() : '';
    if (claim === '' || value === '') throw new ConfigError(`OIDC_ADMIN_CLAIM_RULES: ${JSON.stringify(pair)} is not claim=value (both non-empty), e.g. hd=example.org`);
    return { claim, value };
  });
}

/**
 * PUBLIC_URL as the origin it must be: http or https, a host, no path
 * (one trailing slash is fine), query, fragment or user info. The portal
 * is served at the root: the redirect URI is PUBLIC_URL +
 * /api/v1/auth/callback and the cookies are __Host- (Path=/).
 */
function parsePublicUrl(raw: string): URL {
  let url: URL;
  try {
    url = new URL(raw);
  } catch {
    throw new ConfigError(`PUBLIC_URL is not a URL: ${JSON.stringify(raw)}`);
  }
  if (url.protocol !== 'https:' && url.protocol !== 'http:') throw new ConfigError('PUBLIC_URL must start with https:// (or http:// for local development)');
  if (url.username || url.password) throw new ConfigError('PUBLIC_URL must not contain a user name or password');
  if (url.search || url.hash || raw.includes('?') || raw.includes('#')) throw new ConfigError('PUBLIC_URL must not have a query or fragment');
  if (url.pathname !== '/') {
    throw new ConfigError(`PUBLIC_URL must be the bare origin (scheme, host and port, e.g. https://licenses.kudurru.io), not ${JSON.stringify(raw)}: the portal is served at the root`);
  }
  return url;
}

/** PUBLIC_FORM_ORIGINS entries as exact origins ('https://kudurru.io'; host lower-cased, default port dropped). */
function parseOrigins(raw: string): string[] {
  const entries = commaList(raw);
  if (entries.length === 0) throw new ConfigError('PUBLIC_FORM_ORIGINS is empty: list the origins of the kudurru.io form, or set it to - to turn the public routes off');
  return entries.map((entry) => {
    if (entry === '-') throw new ConfigError('PUBLIC_FORM_ORIGINS: - turns the public routes off and cannot be combined with origins');
    let url: URL;
    try {
      url = new URL(entry);
    } catch {
      throw new ConfigError(`PUBLIC_FORM_ORIGINS: ${JSON.stringify(entry)} is not an origin such as https://kudurru.io`);
    }
    const bare = (url.protocol === 'https:' || url.protocol === 'http:') && !url.username && !url.password && url.pathname === '/' && !url.search && !url.hash && !/[?#*]/.test(entry);
    if (!bare) throw new ConfigError(`PUBLIC_FORM_ORIGINS: ${JSON.stringify(entry)} is not an exact origin (scheme://host[:port], no path or wildcard)`);
    return url.origin;
  });
}

/** Whether a host name is a loopback literal: localhost, 127.0.0.0/8 or ::1 (no *.localhost, no DNS name). */
export function isLoopbackHost(hostname: string): boolean {
  const h = hostname.toLowerCase();
  if (h === 'localhost' || h === '[::1]' || h === '::1') return true;
  const m = /^127\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(h);
  return m !== null && m.slice(1).every((x) => Number(x) <= 255);
}

/** The ID token algorithms the portal accepts (asymmetric only: never none or HS*). */
export const ASYMMETRIC_ID_TOKEN_ALGS: readonly string[] = ['RS256', 'RS384', 'RS512', 'PS256', 'PS384', 'PS512', 'ES256', 'ES384', 'ES512', 'EdDSA', 'Ed25519'];

/** A kid names the <kid>.pub file in LICENSE_TRUSTED_KEYS_DIR: a plain file name, never a path. */
const KID = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/;

/** Why a database URL is refused (message without the URL, which holds the password), or null. */
function databaseUrlProblem(name: string, url: string): string | null {
  let u: URL;
  try {
    u = new URL(url);
  } catch {
    return `${name} is not a URL (postgres://user:password@host:5432/database)`;
  }
  if (u.protocol !== 'postgres:' && u.protocol !== 'postgresql:') return `${name} must be a postgres:// URL`;
  const sslmode = u.searchParams.get('sslmode');
  if (sslmode !== null && sslmode !== 'verify-full') {
    return `${name} has sslmode=${sslmode}: remove it (DATABASE_TLS=verify-full, the default, verifies the certificate) or use sslmode=verify-full; Bun's sslmode=require encrypts without verifying the certificate`;
  }
  const rootcert = u.searchParams.get('sslrootcert');
  if (rootcert !== null && rootcert !== 'system') return `${name} has sslrootcert=${rootcert}: set DATABASE_CA_FILE to a CA bundle instead (sslrootcert=system, the public roots, is the default)`;
  return null;
}

/** What `server.js migrate` needs: the database settings and the log level, nothing about sign-in or the public URL. */
export type DatabaseConfig = Pick<Config, 'database' | 'logLevel'>;

/**
 * The database part of the configuration, for one-off migrations from CI
 * or a workstation (spec §14): only DATABASE_* and LOG_LEVEL are read, so
 * no sign-in settings or public URL are needed. DATABASE_MIGRATE_URL alone
 * is enough (it is the URL migrations use).
 */
export function loadDatabaseConfig(env: Env): DatabaseConfig {
  const kept: Record<string, string | undefined> = {};
  for (const [k, v] of Object.entries(env)) if (k.startsWith('DATABASE_') || k === 'LOG_LEVEL') kept[k] = v;
  const given = (name: string): boolean => (kept[name] ?? '') !== '' || (kept[`${name}_FILE`] ?? '') !== '';
  if (!given('DATABASE_URL') && given('DATABASE_MIGRATE_URL')) {
    kept.DATABASE_URL = kept.DATABASE_MIGRATE_URL;
    kept.DATABASE_URL_FILE = kept.DATABASE_MIGRATE_URL_FILE;
  }
  // loadConfig requires these; migrations never use them.
  const config = loadConfig({ ...kept, PUBLIC_URL: 'http://localhost', OIDC_ISSUER: 'http://localhost/unused', OIDC_CLIENT_ID: 'unused', OIDC_CLIENT_SECRET: 'unused' });
  return { database: config.database, logLevel: config.logLevel };
}

/**
 * Builds the Config from the environment (process.env in main). Throws a
 * ConfigError naming the variable on a missing required value or a value of
 * the wrong type.
 */
export function loadConfig(env: Env): Config {
  const get = (name: ConfigVarName): string | null => {
    const v = CONFIG_VARS.find((x) => x.name === name)!;
    return raw(env, v);
  };
  const need = (name: ConfigVarName): string => {
    const v = get(name);
    if (v === null || v === '') throw new ConfigError(`${name} is required`);
    return v;
  };
  const has = (name: ConfigVarName): boolean => (env[name] ?? '') !== '';

  const parsed = parsePublicUrl(need('PUBLIC_URL'));
  const publicUrl = parsed.origin;
  const tls = need('DATABASE_TLS');
  if (tls !== 'verify-full' && tls !== 'disable') throw new ConfigError(`DATABASE_TLS must be verify-full or disable, got ${JSON.stringify(tls)}`);
  const defaultRole = need('DEFAULT_ROLE');
  if (defaultRole !== 'customer' && defaultRole !== 'none') throw new ConfigError(`DEFAULT_ROLE must be customer or none, got ${JSON.stringify(defaultRole)}`);
  const logLevel = need('LOG_LEVEL');
  if (!isLogLevel(logLevel)) throw new ConfigError(`LOG_LEVEL must be debug, info, warn or error, got ${JSON.stringify(logLevel)}`);
  const origins = need('PUBLIC_FORM_ORIGINS').trim();

  const config: Config = {
    publicUrl,
    publicOrigin: parsed.origin,
    secureCookies: parsed.protocol === 'https:',
    host: need('HOST'),
    port: int('PORT', need('PORT'), 1, 65535),
    devMode: bool('LICENSE_SERVER_DEV', need('LICENSE_SERVER_DEV')),
    logLevel,
    database: {
      url: need('DATABASE_URL'),
      migrateUrl: get('DATABASE_MIGRATE_URL'),
      tls,
      tlsExplicit: has('DATABASE_TLS'),
      caFile: get('DATABASE_CA_FILE'),
      poolMax: int('DATABASE_POOL_MAX', need('DATABASE_POOL_MAX'), 1, 1000),
      migrateOnStart: bool('MIGRATE_ON_START', need('MIGRATE_ON_START')),
    },
    oidc: {
      // Exactly as given (raw() trims whitespace only): the issuer must equal
      // the discovered `issuer` character for character, trailing slash
      // included (e.g. https://sts.windows.net/<tenant>/).
      issuer: need('OIDC_ISSUER'),
      clientId: need('OIDC_CLIENT_ID'),
      clientSecret: need('OIDC_CLIENT_SECRET'),
      scopes: need('OIDC_SCOPES').split(/\s+/).filter((s) => s !== ''),
      displayName: need('OIDC_DISPLAY_NAME'),
      groupsClaim: need('OIDC_GROUPS_CLAIM'),
      idTokenAlg: get('OIDC_ID_TOKEN_ALG'),
      adminClaimRules: claimRules(get('OIDC_ADMIN_CLAIM_RULES')),
    },
    roles: {
      groups: {
        superadmin: commaList(get('ROLE_SUPERADMIN_GROUPS')),
        license_admin: commaList(get('ROLE_LICENSE_ADMIN_GROUPS')),
        customer: commaList(get('ROLE_CUSTOMER_GROUPS')),
      },
      defaultRole,
      memberAutoLink: bool('MEMBER_AUTO_LINK', need('MEMBER_AUTO_LINK')),
    },
    signing: {
      keyFile: get('LICENSE_SIGNING_KEY_FILE'),
      keyEnv: get('LICENSE_SIGNING_KEY'),
      kid: get('LICENSE_SIGNING_KID'),
      trustedKeysDir: need('LICENSE_TRUSTED_KEYS_DIR'),
      trustedSince: get('LICENSE_SIGNING_TRUSTED_SINCE'),
      required: bool('REQUIRE_SIGNING_KEY', need('REQUIRE_SIGNING_KEY')),
    },
    session: {
      idleTimeoutMs: duration('SESSION_IDLE_TIMEOUT', need('SESSION_IDLE_TIMEOUT')),
      lifetimeMs: duration('SESSION_LIFETIME', need('SESSION_LIFETIME')),
    },
    trustedProxies: commaList(get('TRUSTED_PROXIES')),
    clientIpHeader: get('CLIENT_IP_HEADER')?.toLowerCase() ?? null,
    mail: {
      smtpUrl: get('SMTP_URL'),
      from: need('MAIL_FROM'),
      replyTo: need('MAIL_REPLY_TO'),
      adminNotify: get('ADMIN_NOTIFY_EMAIL'),
    },
    publicForm: {
      enabled: origins !== '-',
      origins: origins === '-' ? [] : parseOrigins(origins),
      limitPerIpPerHour: int('PUBLIC_LIMIT_PER_IP_PER_HOUR', need('PUBLIC_LIMIT_PER_IP_PER_HOUR'), 1, 1_000_000),
      limitPerEmailPerDay: int('PUBLIC_LIMIT_PER_EMAIL_PER_DAY', need('PUBLIC_LIMIT_PER_EMAIL_PER_DAY'), 1, 1_000_000),
    },
    personal: {
      autoApprove: bool('PERSONAL_AUTO_APPROVE', need('PERSONAL_AUTO_APPROVE')),
      autoApproveMaxPerDay: int('PERSONAL_AUTO_APPROVE_MAX_PER_DAY', need('PERSONAL_AUTO_APPROVE_MAX_PER_DAY'), 0, 1_000_000),
    },
    issuing: {
      maxPerActorPerDay: int('ISSUE_MAX_PER_ACTOR_PER_DAY', need('ISSUE_MAX_PER_ACTOR_PER_DAY'), 1, 1_000_000),
    },
    build: buildInfo(env),
  };
  validateConfig(config);
  return config;
}

/**
 * The build stamp (spec §12): the Dockerfile turns its build args VERSION,
 * COMMIT and BUILT_AT into LICENSE_SERVER_VERSION, LICENSE_SERVER_COMMIT and
 * LICENSE_SERVER_BUILT_AT. Not operator settings, so not in CONFIG_VARS.
 * Unset or empty: version and commit "dev", builtAt null.
 */
export function buildInfo(env: Env): Config['build'] {
  const value = (name: string): string | null => {
    const v = env[name]?.trim();
    return v ? v : null;
  };
  return {
    version: value('LICENSE_SERVER_VERSION') ?? 'dev',
    commit: value('LICENSE_SERVER_COMMIT') ?? 'dev',
    builtAt: value('LICENSE_SERVER_BUILT_AT'),
  };
}

/**
 * The startup refusals of spec §0.6, §2 and §3 (loadConfig calls it).
 * Throws a ConfigError naming the variable and the fix; never echoes a
 * secret. The only I/O is checking that DATABASE_CA_FILE is readable.
 */
export function validateConfig(config: Config): void {
  // Sessions.
  if (config.session.idleTimeoutMs < 60_000) throw new ConfigError('SESSION_IDLE_TIMEOUT must be at least 1m');
  if (config.session.lifetimeMs <= 0) throw new ConfigError('SESSION_LIFETIME must be positive');
  if (config.session.idleTimeoutMs > config.session.lifetimeMs) throw new ConfigError('SESSION_IDLE_TIMEOUT must not be longer than SESSION_LIFETIME');

  // Database.
  for (const [name, url] of [['DATABASE_URL', config.database.url], ['DATABASE_MIGRATE_URL', config.database.migrateUrl]] as const) {
    const problem = url === null ? null : databaseUrlProblem(name, url);
    if (problem) throw new ConfigError(problem);
  }
  if (config.database.tls === 'disable' && !config.database.tlsExplicit) throw new ConfigError('DATABASE_TLS=disable must be set explicitly');
  if (config.database.caFile !== null) {
    if (config.database.tls === 'disable') throw new ConfigError('DATABASE_CA_FILE is set but DATABASE_TLS is disable: drop one');
    try {
      accessSync(config.database.caFile, constants.R_OK);
    } catch {
      throw new ConfigError(`DATABASE_CA_FILE: cannot read ${config.database.caFile}`);
    }
  }

  // Sign-in.
  let issuer: URL;
  try {
    issuer = new URL(config.oidc.issuer);
  } catch {
    throw new ConfigError(`OIDC_ISSUER is not a URL: ${JSON.stringify(config.oidc.issuer)}`);
  }
  if (issuer.search || issuer.hash || issuer.username || issuer.password) throw new ConfigError('OIDC_ISSUER must not have a query, fragment or user info');
  if (issuer.protocol === 'http:') {
    if (!isLoopbackHost(issuer.hostname)) throw new ConfigError('OIDC_ISSUER must be https (http is allowed only for localhost, 127.0.0.0/8 or ::1 during local development)');
    if (config.secureCookies) throw new ConfigError('OIDC_ISSUER is http while PUBLIC_URL is https: use an https issuer');
  } else if (issuer.protocol !== 'https:') {
    throw new ConfigError('OIDC_ISSUER must be an https URL');
  }
  if (!config.oidc.scopes.includes('openid')) throw new ConfigError('OIDC_SCOPES must include openid');
  if (config.oidc.idTokenAlg !== null && !ASYMMETRIC_ID_TOKEN_ALGS.includes(config.oidc.idTokenAlg)) {
    throw new ConfigError(`OIDC_ID_TOKEN_ALG must be one of ${ASYMMETRIC_ID_TOKEN_ALGS.join(', ')} (none and HS* are refused)`);
  }
  const adminGroups = config.roles.groups.superadmin.length + config.roles.groups.license_admin.length;
  if (config.roles.defaultRole === 'customer' && adminGroups > 0 && config.oidc.adminClaimRules.length === 0) {
    throw new ConfigError(
      'ROLE_SUPERADMIN_GROUPS or ROLE_LICENSE_ADMIN_GROUPS is set while DEFAULT_ROLE=customer lets anyone the IdP knows sign in: ' +
        'set OIDC_ADMIN_CLAIM_RULES (e.g. hd=yourdomain.example or tid=<tenant id>) so only your own accounts can hold an admin group, ' +
        'or set DEFAULT_ROLE=none, or grant admin roles in the portal instead',
    );
  }

  // Signing (the key itself is checked by licensing/keyring.ts).
  const s = config.signing;
  if (s.keyEnv !== null && s.keyFile !== null) throw new ConfigError('LICENSE_SIGNING_KEY and LICENSE_SIGNING_KEY_FILE are both set; set one');
  if ((s.keyEnv !== null || s.keyFile !== null) && s.kid === null) throw new ConfigError('a signing key is configured without LICENSE_SIGNING_KID');
  if (s.kid !== null && s.keyEnv === null && s.keyFile === null) throw new ConfigError('LICENSE_SIGNING_KID is set but no signing key is (set LICENSE_SIGNING_KEY_FILE or LICENSE_SIGNING_KEY)');
  if (s.kid !== null && !KID.test(s.kid)) throw new ConfigError(`LICENSE_SIGNING_KID must be a plain file name such as kudurru-2026-1, got ${JSON.stringify(s.kid)}`);

  // The network.
  for (const entry of config.trustedProxies) {
    try {
      parseCidr(entry);
    } catch {
      throw new ConfigError(`TRUSTED_PROXIES: ${JSON.stringify(entry)} is not an IP address or CIDR (e.g. 10.0.0.0/8, fd00::/8)`);
    }
  }

  if (config.clientIpHeader !== null) {
    if (!/^[a-z0-9][a-z0-9-]*$/.test(config.clientIpHeader)) throw new ConfigError(`CLIENT_IP_HEADER must be a header name such as X-Client-IP, got ${JSON.stringify(config.clientIpHeader)}`);
    if (['cookie', 'authorization', 'host', 'origin', 'x-kudurru-csrf'].includes(config.clientIpHeader)) throw new ConfigError(`CLIENT_IP_HEADER cannot be ${config.clientIpHeader}`);
  }

  // Mail.
  if (config.mail.smtpUrl !== null) {
    let smtp: URL | null;
    try {
      smtp = new URL(config.mail.smtpUrl);
    } catch {
      smtp = null;
    }
    if (!smtp || (smtp.protocol !== 'smtp:' && smtp.protocol !== 'smtps:') || !smtp.hostname) {
      throw new ConfigError('SMTP_URL must be smtp://user:password@host:587 (STARTTLS) or smtps://user:password@host:465');
    }
  }
}

/** The configuration with every secret replaced, for logging the startup settings. */
export function redactedConfig(config: Config): Config {
  const hide = (v: string | null): string | null => (v === null ? null : '[redacted]');
  return {
    ...config,
    database: { ...config.database, url: '[redacted]', migrateUrl: hide(config.database.migrateUrl) },
    oidc: { ...config.oidc, clientSecret: '[redacted]' },
    signing: { ...config.signing, keyEnv: hide(config.signing.keyEnv) },
    mail: { ...config.mail, smtpUrl: hide(config.mail.smtpUrl) },
  };
}
