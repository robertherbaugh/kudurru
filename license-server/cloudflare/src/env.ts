// Which of the Worker's vars and secrets reach the portal's environment.
//
// The server reads its whole configuration from environment variables
// (design spec §3, server/config.ts CONFIG_VARS). A Cloudflare container
// gets environment variables only from the Durable Object that starts it,
// and no files at all, so the Durable Object copies these names from the
// Worker's env (vars in wrangler.jsonc, secrets from `wrangler secret put`)
// at every start. Names that are unset or empty are left out, so the
// server's own defaults apply to them. test/env.test.ts checks this list
// against CONFIG_VARS, so a new server setting cannot be missed here.

/** Server settings copied into the container when the Worker has them. */
export const SERVER_ENV = [
  'PUBLIC_URL',
  'LICENSE_SERVER_DEV',
  'LOG_LEVEL',
  // Postgres. DATABASE_CA_FILE is a path inside the image: it only helps
  // with an image derived from the portal's that adds the CA bundle.
  'DATABASE_URL',
  'DATABASE_MIGRATE_URL',
  'DATABASE_TLS',
  'DATABASE_CA_FILE',
  'DATABASE_POOL_MAX',
  'MIGRATE_ON_START',
  // Sign-in and roles.
  'OIDC_ISSUER',
  'OIDC_CLIENT_ID',
  'OIDC_CLIENT_SECRET',
  'OIDC_SCOPES',
  'OIDC_DISPLAY_NAME',
  'OIDC_GROUPS_CLAIM',
  'OIDC_ID_TOKEN_ALG',
  'OIDC_ADMIN_CLAIM_RULES',
  'ROLE_SUPERADMIN_GROUPS',
  'ROLE_LICENSE_ADMIN_GROUPS',
  'ROLE_CUSTOMER_GROUPS',
  'DEFAULT_ROLE',
  'MEMBER_AUTO_LINK',
  // Signing. The key arrives as LICENSE_SIGNING_KEY because Cloudflare
  // cannot mount it as a file (the server accepts the env form for this).
  'LICENSE_SIGNING_KEY',
  'LICENSE_SIGNING_KID',
  'LICENSE_SIGNING_TRUSTED_SINCE',
  'REQUIRE_SIGNING_KEY',
  // Sessions and the client address.
  'SESSION_IDLE_TIMEOUT',
  'SESSION_LIFETIME',
  'TRUSTED_PROXIES',
  'CLIENT_IP_HEADER',
  // Mail.
  'SMTP_URL',
  'MAIL_FROM',
  'MAIL_REPLY_TO',
  'ADMIN_NOTIFY_EMAIL',
  // The kudurru.io form and the issuing limits.
  'PUBLIC_FORM_ORIGINS',
  'PUBLIC_LIMIT_PER_IP_PER_HOUR',
  'PUBLIC_LIMIT_PER_EMAIL_PER_DAY',
  'PERSONAL_AUTO_APPROVE',
  'PERSONAL_AUTO_APPROVE_MAX_PER_DAY',
  'ISSUE_MAX_PER_ACTOR_PER_DAY',
] as const;

/** Server settings that are deliberately not copied, and why. */
export const NOT_FORWARDED: Readonly<Record<string, string>> = {
  HOST: 'the image listens on 0.0.0.0, which the container runtime needs',
  PORT: 'the Durable Object always connects to 8790, the image default',
  LICENSE_TRUSTED_KEYS_DIR: 'the image carries the keys it was built with in /app/trusted-keys',
  LICENSE_SIGNING_KEY_FILE: 'Cloudflare mounts no files; the key comes as LICENSE_SIGNING_KEY',
};

/** Names in SERVER_ENV that the server does not read yet (none: CLIENT_IP_HEADER landed in the server). */
export const AWAITING_SERVER: readonly string[] = [];

/** The container environment: every SERVER_ENV name the Worker's env holds as a non-empty string. */
export function serverEnv(env: object): Record<string, string> {
  const source = env as Record<string, unknown>;
  const out: Record<string, string> = {};
  for (const name of SERVER_ENV) {
    const value = source[name];
    if (typeof value === 'string' && value !== '') out[name] = value;
  }
  return out;
}

/** A SHA-256 (hex) of the container environment, to notice a change of any var or secret. */
export async function envDigest(vars: Readonly<Record<string, string>>): Promise<string> {
  const text = JSON.stringify(Object.keys(vars).sort().map((k) => [k, vars[k]]));
  const hash = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(text));
  return [...new Uint8Array(hash)].map((b) => b.toString(16).padStart(2, '0')).join('');
}
