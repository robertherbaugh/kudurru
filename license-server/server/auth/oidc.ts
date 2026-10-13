// OIDC sign-in as a confidential client, backend for frontend (design spec
// §2 "OIDC", §6 "Sign-in"), with openid-client 6.8.8. The browser never sees
// a token: the server runs authorization code + PKCE S256 + state + nonce
// and keeps only the verified claims.
//
// Checks beyond openid-client's defaults (research oidc-auth.md §10):
// - enableNonRepudiationChecks: the ID token signature is verified against
//   the IdP's JWKS (openid-client skips it by default);
// - the alg is pinned (OIDC_ID_TOKEN_ALG) or taken from discovery with
//   none and HS* removed (a list with nothing left refuses discovery);
// - azp, when present, must be our client id (openid-client checks it only
//   with several audiences); sub must be a non-empty string;
// - a distributed groups claim (Entra ID overage) is refused;
// - plain http only for a loopback issuer while PUBLIC_URL is http too
//   (allowInsecureRequests is all or nothing, so this is the only gate);
// - the token request's redirect_uri comes from PUBLIC_URL, never from the
//   request's Host (behind a proxy that is the internal address).
//
// Discovery is lazy and shared: the first sign-in runs it, a failure is
// retried at most every DISCOVERY_RETRY_MS, and meanwhile sign-in answers
// 503 idp_unavailable; the server never fails to start because the IdP is
// down. Sign-ins in flight live in oidc_logins (state, nonce, verifier,
// next, the SHA-256 of the browser's binding cookie), at most
// MAX_PENDING_LOGINS at once, consumed by a single DELETE … RETURNING.
// Causes are logged, never shown: the callback redirects to
// /login?error=<LoginErrorKey> (loginErrors.ts).

import * as oidc from 'openid-client';
// Named: discovery() recognises it by identity in `execute` (it also allows http for the discovery request itself).
import { allowInsecureRequests } from 'openid-client';
import { ROUTES } from '../../shared/routes';
import { ASYMMETRIC_ID_TOKEN_ALGS, isLoopbackHost, type Config } from '../config';
import { maybeOne, toInt, type Queryable } from '../db/sql';
import { HttpError } from '../http/errors';
import type { Logger } from '../log';
import type { IdClaims } from './identity';
import { hasDistributedGroups } from './identity';
import type { LoginErrorKey } from './loginErrors';

/** How long a sign-in may take from the redirect to the IdP to the callback. */
export const LOGIN_TTL_MS = 10 * 60_000;

/** Unexpired sign-ins in flight beyond which the start answers 503 login_busy. */
export const MAX_PENDING_LOGINS = 10_000;

/** The shortest pause between two discovery attempts after a failure. */
export const DISCOVERY_RETRY_MS = 10_000;

/** Seconds openid-client waits for the IdP (discovery, token, JWKS). */
export const IDP_TIMEOUT_SECONDS = 10;

/** A sign-in that ends at /login?error=<key>; `reason` (and the cause) go to the log only. */
export class LoginError extends Error {
  override readonly name = 'LoginError';
  constructor(
    readonly key: LoginErrorKey,
    readonly reason: string,
    opts: { cause?: unknown } = {},
  ) {
    super(`${key}: ${reason}`, opts.cause === undefined ? undefined : { cause: opts.cause });
  }
}

/** The redirect URI registered at the IdP: PUBLIC_URL + /api/v1/auth/callback. */
export function redirectUri(config: Pick<Config, 'publicUrl'>): string {
  return config.publicUrl + ROUTES.authCallback.path;
}

/** Whether plain http may be used with the IdP: a loopback issuer while PUBLIC_URL is http (config.ts refuses anything else at startup). */
export function insecureRequestsAllowed(config: Pick<Config, 'oidc' | 'secureCookies'>): boolean {
  let issuer: URL;
  try {
    issuer = new URL(config.oidc.issuer);
  } catch {
    return false;
  }
  return issuer.protocol === 'http:' && isLoopbackHost(issuer.hostname) && !config.secureCookies;
}

/** What an error from openid-client may tell the log: name, message and codes, never bodies or claims (they may hold tokens). */
export function describeError(e: unknown): Record<string, unknown> {
  if (!(e instanceof Error)) return { error: String(e).slice(0, 200) };
  const out: Record<string, unknown> = { name: e.name, message: e.message.slice(0, 300) };
  const code = (e as { code?: unknown }).code;
  if (typeof code === 'string') out.code = code;
  const error = (e as { error?: unknown }).error;
  if (typeof error === 'string') out.error = error.slice(0, 100);
  const desc = (e as { error_description?: unknown }).error_description;
  if (typeof desc === 'string') out.errorDescription = desc.slice(0, 300);
  const status = (e as { status?: unknown }).status;
  if (typeof status === 'number') out.status = status;
  // openid-client wraps the specific failure ("JWT signature verification failed") one level down; only its message is kept.
  if (e.cause instanceof Error) out.detail = e.cause.message.slice(0, 300);
  return out;
}

/** The verified claims of a completed sign-in. */
export interface VerifiedLogin {
  claims: IdClaims;
  issuer: string;
  subject: string;
}

export interface OidcClientOptions {
  /** The clock of the discovery retry; default Date.now (setSystemTime moves it). */
  now?: () => number;
  /** openid-client's fetch (tests); default the global fetch. */
  fetch?: oidc.CustomFetch;
}

/** The IdP as the portal uses it: one per configuration (oidcClientFor). */
export class OidcClient {
  private configuration: oidc.Configuration | null = null;
  private inflight: Promise<oidc.Configuration> | null = null;
  private lastFailure = -Infinity;
  private readonly now: () => number;

  constructor(
    private readonly config: Config,
    private readonly opts: OidcClientOptions = {},
  ) {
    this.now = opts.now ?? Date.now;
  }

  /** The discovered configuration; 503 idp_unavailable while discovery fails (retried at most every DISCOVERY_RETRY_MS). */
  async discovered(log: Logger): Promise<oidc.Configuration> {
    if (this.configuration) return this.configuration;
    if (this.inflight) return this.inflight;
    if (this.now() - this.lastFailure < DISCOVERY_RETRY_MS) throw idpUnavailable();
    const attempt = this.discover().then(
      (c) => {
        this.configuration = c;
        log.info('oidc discovery done', { issuer: this.config.oidc.issuer });
        return c;
      },
      (e: unknown) => {
        this.lastFailure = this.now();
        log.error('oidc discovery failed; sign-in answers 503 until it works', { issuer: this.config.oidc.issuer, err: describeError(e) });
        throw idpUnavailable();
      },
    );
    this.inflight = attempt;
    try {
      return await attempt;
    } finally {
      this.inflight = null;
    }
  }

  private async discover(): Promise<oidc.Configuration> {
    const c = this.config.oidc;
    const insecure = insecureRequestsAllowed(this.config);
    if (new URL(c.issuer).protocol === 'http:' && !insecure) throw new Error('an http issuer is allowed only on loopback while PUBLIC_URL is http');
    const execute = insecure ? [allowInsecureRequests] : [];
    const options: oidc.DiscoveryRequestOptions = { execute, timeout: IDP_TIMEOUT_SECONDS };
    if (this.opts.fetch) options[oidc.customFetch] = this.opts.fetch;
    const found = await oidc.discovery(new URL(c.issuer), c.clientId, c.clientSecret, undefined, options);
    // serverMetadata() adds supportsPKCE (non-enumerable); copying the enumerable entries leaves plain JSON for the Configuration below.
    const server = Object.fromEntries(Object.entries(found.serverMetadata())) as oidc.ServerMetadata;

    // The ID token algorithms: the pin, else discovery's list without none and HS*.
    const offered = server.id_token_signing_alg_values_supported;
    let algs: string[] | undefined;
    if (Array.isArray(offered)) {
      algs = offered.filter((a) => ASYMMETRIC_ID_TOKEN_ALGS.includes(a));
      if (algs.length === 0 && c.idTokenAlg === null) throw new Error(`the IdP offers no asymmetric ID token algorithm (${offered.join(', ')}); set OIDC_ID_TOKEN_ALG`);
    }
    // client_secret_post when offered (oauth4webapi's Basic form-encodes the client id, which some IdPs mishandle), else Basic (the default).
    const methods = server.token_endpoint_auth_methods_supported;
    const auth = Array.isArray(methods) && methods.includes('client_secret_post') ? oidc.ClientSecretPost(c.clientSecret) : oidc.ClientSecretBasic(c.clientSecret);
    const metadata: Partial<oidc.ClientMetadata> = { client_secret: c.clientSecret };
    if (c.idTokenAlg !== null) metadata.id_token_signed_response_alg = c.idTokenAlg;
    const configuration = new oidc.Configuration({ ...server, id_token_signing_alg_values_supported: algs }, c.clientId, metadata, auth);
    if (insecure) allowInsecureRequests(configuration);
    oidc.enableNonRepudiationChecks(configuration);
    configuration.timeout = IDP_TIMEOUT_SECONDS;
    if (this.opts.fetch) configuration[oidc.customFetch] = this.opts.fetch;
    return configuration;
  }

  /** The IdP's authorization URL for one sign-in (code, PKCE S256, state, nonce). */
  async authorizationUrl(log: Logger, p: { state: string; nonce: string; codeVerifier: string }): Promise<URL> {
    const configuration = await this.discovered(log);
    const challenge = await oidc.calculatePKCECodeChallenge(p.codeVerifier);
    return oidc.buildAuthorizationUrl(configuration, {
      redirect_uri: redirectUri(this.config),
      scope: this.config.oidc.scopes.join(' '),
      response_type: 'code',
      code_challenge: challenge,
      code_challenge_method: 'S256',
      state: p.state,
      nonce: p.nonce,
    });
  }

  /**
   * Exchanges the code of a callback and checks the ID token. `search` is
   * the callback's query string; the URL the IdP redirected to is rebuilt
   * from PUBLIC_URL. Throws LoginError (failed, or idp_unavailable while
   * discovery fails).
   */
  async exchange(log: Logger, search: string, p: { state: string; nonce: string; codeVerifier: string }): Promise<VerifiedLogin> {
    let configuration: oidc.Configuration;
    try {
      configuration = await this.discovered(log);
    } catch (e) {
      throw new LoginError('idp_unavailable', 'discovery failed', { cause: e });
    }
    const currentUrl = new URL(redirectUri(this.config) + search);
    let claims: IdClaims | undefined;
    try {
      const tokens = await oidc.authorizationCodeGrant(configuration, currentUrl, {
        pkceCodeVerifier: p.codeVerifier,
        expectedState: p.state,
        expectedNonce: p.nonce,
        idTokenExpected: true,
      });
      claims = tokens.claims();
    } catch (e) {
      throw new LoginError('failed', 'code exchange or ID token check failed', { cause: e });
    }
    if (!claims) throw new LoginError('failed', 'no ID token');
    const clientId = this.config.oidc.clientId;
    if (claims.azp !== undefined && claims.azp !== clientId) throw new LoginError('failed', 'the ID token was issued to another client (azp)');
    if (typeof claims.sub !== 'string' || claims.sub === '') throw new LoginError('failed', 'the ID token has no subject');
    if (typeof claims.iss !== 'string' || claims.iss === '') throw new LoginError('failed', 'the ID token has no issuer');
    if (hasDistributedGroups(claims, this.config.oidc.groupsClaim)) {
      throw new LoginError('failed', 'the IdP sent the groups as a distributed claim (group overage); configure it to send only the groups assigned to this application');
    }
    return { claims, issuer: claims.iss, subject: claims.sub };
  }
}

function idpUnavailable(): HttpError {
  return new HttpError(503, 'idp_unavailable', 'The sign-in service cannot be reached right now. Try again in a minute.');
}

const clients = new WeakMap<Config, OidcClient>();

/** The OidcClient of a configuration (one per Config object, so per server). */
export function oidcClientFor(config: Config): OidcClient {
  let c = clients.get(config);
  if (!c) {
    c = new OidcClient(config);
    clients.set(config, c);
  }
  return c;
}

/** Replaces the OidcClient of a configuration (tests: a custom fetch or clock). */
export function setOidcClient(config: Config, client: OidcClient): void {
  clients.set(config, client);
}

// --- sign-ins in flight (oidc_logins) ---------------------------------------

/** One oidc_logins row. */
export interface PendingLogin {
  state: string;
  bindingHash: Buffer;
  nonce: string;
  codeVerifier: string;
  next: string;
  expiresAt: Date;
}

/** Unexpired sign-ins in flight, counted up to MAX_PENDING_LOGINS + 1. */
export async function countPendingLogins(q: Queryable, now: Date): Promise<number> {
  const rows = await q<{ n: string }[]>`
    select count(*) as n from (select 1 from oidc_logins where expires_at > ${now} limit ${MAX_PENDING_LOGINS + 1}) pending`;
  return toInt(rows[0]?.n);
}

/** Stores a sign-in in flight. */
export async function insertPendingLogin(q: Queryable, login: PendingLogin, now: Date): Promise<void> {
  await q`
    insert into oidc_logins (state, binding_hash, nonce, code_verifier, next, created_at, expires_at)
    values (${login.state}, ${login.bindingHash}, ${login.nonce}, ${login.codeVerifier}, ${login.next}, ${now}, ${login.expiresAt})`;
}

interface LoginRow {
  state: string;
  binding_hash: Buffer;
  nonce: string;
  code_verifier: string;
  next: string;
  expires_at: Date;
}

/** Takes the sign-in of `state` out of oidc_logins (single use, expired or not); null when there is none. */
export async function consumePendingLogin(q: Queryable, state: string): Promise<PendingLogin | null> {
  const row = maybeOne(
    await q<LoginRow[]>`delete from oidc_logins where state = ${state} returning state, binding_hash, nonce, code_verifier, next, expires_at`,
  );
  if (!row) return null;
  return { state: row.state, bindingHash: row.binding_hash, nonce: row.nonce, codeVerifier: row.code_verifier, next: row.next, expiresAt: row.expires_at };
}

/** Deletes the expired sign-ins (the background job); returns how many went. */
export async function deleteExpiredLogins(q: Queryable, now: Date): Promise<number> {
  const rows = await q<{ n: string }[]>`with d as (delete from oidc_logins where expires_at <= ${now} returning 1) select count(*) as n from d`;
  return toInt(rows[0]?.n);
}
