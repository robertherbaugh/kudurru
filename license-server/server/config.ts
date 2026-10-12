// Configuration (design spec §3): every environment variable, its default
// and the typed Config the server runs with. Secret URLs and the client
// secret also come from a file: FOO_FILE names a file whose trimmed content
// is FOO (Docker and Kubernetes secrets). The signing key's file form is its
// own variable, LICENSE_SIGNING_KEY_FILE. CONFIG_VARS is the one table of variables: loadConfig
// reads it, the README documents it and the System page shows the
// non-secret part.
//
// loadConfig parses and checks types and required values. The refusals that
// need more context (an sslmode= in DATABASE_URL, DATABASE_TLS=disable only
// when set explicitly, DEFAULT_ROLE=customer with admin group mappings but no
// OIDC_ADMIN_CLAIM_RULES, the dev-only env signing key) are stream A's: see
// the TODO in validateConfig.

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import type { Role } from '../shared/permissions';
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
    /** LICENSE_SIGNING_KEY, dev mode only. Secret: never logged, never shown. */
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
  { name: 'PUBLIC_URL', default: null, required: true, description: 'External base URL, no trailing slash. Cookie Secure and __Host- follow its scheme' },
  { name: 'HOST', default: '0.0.0.0', description: 'Listen address' },
  { name: 'PORT', default: '8790', description: 'Listen port' },
  { name: 'LICENSE_SERVER_DEV', default: 'false', description: 'Dev mode: env-form signing key, test kids, dev banner in the UI' },
  { name: 'DATABASE_URL', default: null, file: true, required: true, secret: true, description: 'Postgres URL of the runtime role (DML)' },
  { name: 'DATABASE_MIGRATE_URL', default: null, file: true, secret: true, description: 'Postgres URL of the owner role for migrations' },
  { name: 'DATABASE_TLS', default: 'verify-full', description: 'verify-full or disable' },
  { name: 'DATABASE_CA_FILE', default: null, description: "CA bundle for verify-full (else Bun's bundled roots)" },
  { name: 'DATABASE_POOL_MAX', default: '10', description: 'Connection pool size' },
  { name: 'MIGRATE_ON_START', default: 'true', description: 'Run pending migrations at startup' },
  { name: 'OIDC_ISSUER', default: null, required: true, description: 'OIDC issuer URL' },
  { name: 'OIDC_CLIENT_ID', default: null, required: true, description: 'OIDC client id' },
  { name: 'OIDC_CLIENT_SECRET', default: null, file: true, required: true, secret: true, description: 'OIDC client secret' },
  { name: 'OIDC_SCOPES', default: 'openid profile email', description: 'Space-separated scopes; add groups for IdPs that support it' },
  { name: 'OIDC_DISPLAY_NAME', default: 'Sign in', description: 'Sign-in button label' },
  { name: 'OIDC_GROUPS_CLAIM', default: 'groups', description: 'The only claim read for groups (string or array)' },
  { name: 'OIDC_ID_TOKEN_ALG', default: null, description: 'Pin the ID token alg' },
  { name: 'OIDC_ADMIN_CLAIM_RULES', default: null, description: 'claim=value comma list; all must match before admin group mappings are honoured' },
  { name: 'ROLE_SUPERADMIN_GROUPS', default: '', description: 'IdP groups mapped to superadmin (comma list)' },
  { name: 'ROLE_LICENSE_ADMIN_GROUPS', default: '', description: 'IdP groups mapped to license_admin (comma list)' },
  { name: 'ROLE_CUSTOMER_GROUPS', default: '', description: 'IdP groups mapped to customer (comma list)' },
  { name: 'DEFAULT_ROLE', default: 'customer', description: 'customer or none: the role of a sign-in that maps to nothing' },
  { name: 'MEMBER_AUTO_LINK', default: 'false', description: 'Also link member rows by IdP-verified email at sign-in' },
  { name: 'LICENSE_SIGNING_KEY_FILE', default: null, description: 'File with the signing seed (licensegen format)' },
  { name: 'LICENSE_SIGNING_KID', default: null, description: 'Key id of the signing key' },
  // Not read from LICENSE_SIGNING_KEY_FILE: that is the file form above, the production one.
  { name: 'LICENSE_SIGNING_KEY', default: null, secret: true, description: 'The signing seed itself, dev mode only' },
  { name: 'LICENSE_TRUSTED_KEYS_DIR', default: REPO_TRUSTED_KEYS_DIR, description: 'Directory of <kid>.pub files kudurru releases embed (image: /app/trusted-keys)' },
  { name: 'LICENSE_SIGNING_TRUSTED_SINCE', default: null, description: 'First kudurru release that trusts the signing kid, e.g. v0.1.0' },
  { name: 'REQUIRE_SIGNING_KEY', default: 'false', description: 'Refuse to start without a usable signing key' },
  { name: 'SESSION_IDLE_TIMEOUT', default: '30m', description: 'Session idle timeout' },
  { name: 'SESSION_LIFETIME', default: '12h', description: 'Absolute session lifetime' },
  { name: 'TRUSTED_PROXIES', default: '', description: 'CIDRs whose X-Forwarded-For is believed (comma list)' },
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
    if (i <= 0) throw new ConfigError(`OIDC_ADMIN_CLAIM_RULES: ${JSON.stringify(pair)} is not claim=value`);
    return { claim: pair.slice(0, i).trim(), value: pair.slice(i + 1).trim() };
  });
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

  const publicUrl = need('PUBLIC_URL').replace(/\/+$/, '');
  let parsed: URL;
  try {
    parsed = new URL(publicUrl);
  } catch {
    throw new ConfigError(`PUBLIC_URL is not a URL: ${JSON.stringify(publicUrl)}`);
  }
  if (parsed.protocol !== 'https:' && parsed.protocol !== 'http:') throw new ConfigError('PUBLIC_URL must be http or https');
  const tls = need('DATABASE_TLS');
  if (tls !== 'verify-full' && tls !== 'disable') throw new ConfigError(`DATABASE_TLS must be verify-full or disable, got ${JSON.stringify(tls)}`);
  const defaultRole = need('DEFAULT_ROLE');
  if (defaultRole !== 'customer' && defaultRole !== 'none') throw new ConfigError(`DEFAULT_ROLE must be customer or none, got ${JSON.stringify(defaultRole)}`);
  const logLevel = need('LOG_LEVEL');
  if (!isLogLevel(logLevel)) throw new ConfigError(`LOG_LEVEL must be debug, info, warn or error, got ${JSON.stringify(logLevel)}`);
  const origins = need('PUBLIC_FORM_ORIGINS');

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
    mail: {
      smtpUrl: get('SMTP_URL'),
      from: need('MAIL_FROM'),
      replyTo: need('MAIL_REPLY_TO'),
      adminNotify: get('ADMIN_NOTIFY_EMAIL'),
    },
    publicForm: {
      enabled: origins !== '-',
      origins: origins === '-' ? [] : commaList(origins).map((o) => o.replace(/\/+$/, '')),
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

/** Cross-field checks that need no I/O. */
export function validateConfig(config: Config): void {
  if (config.session.idleTimeoutMs <= 0 || config.session.lifetimeMs <= 0) throw new ConfigError('SESSION_IDLE_TIMEOUT and SESSION_LIFETIME must be positive');
  if (config.session.idleTimeoutMs > config.session.lifetimeMs) throw new ConfigError('SESSION_IDLE_TIMEOUT must not be longer than SESSION_LIFETIME');
  // TODO(stream A): the refusals of spec §2 and §3: an sslmode= query
  // parameter in DATABASE_URL / DATABASE_MIGRATE_URL; DATABASE_TLS=disable
  // only when tlsExplicit; LICENSE_SIGNING_KEY only in dev mode; a signing
  // key without LICENSE_SIGNING_KID; DEFAULT_ROLE=customer plus admin group
  // mappings without OIDC_ADMIN_CLAIM_RULES; an http OIDC issuer unless it is
  // loopback and PUBLIC_URL is http; PUBLIC_FORM_ORIGINS entries that are not
  // bare origins.
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
