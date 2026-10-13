// A fake OpenID Provider for the sign-in tests (design spec §10 "auth"), a
// Bun port of the shape of kudurru's Go mock OP (ee/oidc/mockop_test.go):
// discovery, a JWKS (RS256 or EdDSA), an authorize endpoint that redirects
// straight back with a code (the "user" is whoever the test set), and a
// token endpoint that enforces client authentication (client_secret_basic
// or client_secret_post), the exact redirect_uri, PKCE S256 and single-use
// codes. Tamper hooks change the next ID tokens: claims (aud, azp, nonce,
// email_verified, a distributed groups claim, …), the header, a broken
// signature or a signature by a key outside the JWKS.
//
//   const op = await FakeOp.start({ redirectUri: 'http://localhost:8790/api/v1/auth/callback' });
//   op.user = { sub: 'alice', email: 'alice@example.org', email_verified: true };
//   const callback = await op.authorize(loginResponse.headers.get('location')!);
//   op.close();

import type { Server } from 'bun';
import { createHash, randomBytes, type webcrypto } from 'node:crypto';

export type FakeOpAlg = 'RS256' | 'EdDSA';

/** The claims of the person the next authorize signs in (sub required; anything else goes into the ID token as is). */
export interface FakeUser {
  sub: string;
  [claim: string]: unknown;
}

/** Changes to the ID tokens the token endpoint signs (until reset()). */
export interface Tamper {
  /** Edit the claims before signing (delete, replace, add). */
  claims?: (claims: Record<string, unknown>) => void;
  /** Extra or replaced JWS header fields (e.g. { alg: 'HS256' }). */
  header?: Record<string, unknown>;
  /** Flip a bit of the signature. */
  badSignature?: boolean;
  /** Sign with a key that is not in the JWKS (same kid). */
  foreignKey?: boolean;
}

export interface FakeOpOptions {
  /** The one registered redirect URI (exact match). */
  redirectUri: string;
  alg?: FakeOpAlg;
  clientId?: string;
  clientSecret?: string;
  /** The issuer's path; default /dex (as Dex). */
  issuerPath?: string;
  /** id_token_signing_alg_values_supported; default [alg]; null leaves it out. */
  advertisedAlgs?: string[] | null;
  /** token_endpoint_auth_methods_supported; default both secret methods. */
  authMethods?: string[];
  /**
   * The issuer the OP claims to be instead of its own http://127.0.0.1
   * address (an https one, for tests of an https PUBLIC_URL). The client
   * reaches it through `op.fetch`, which maps it back; authorize() too.
   */
  publicIssuer?: string;
}

interface CodeGrant {
  clientId: string;
  redirectUri: string;
  challenge: string;
  nonce: string;
  scope: string;
  user: FakeUser;
}

interface SigningKey {
  kid: string;
  alg: FakeOpAlg;
  privateKey: CryptoKey;
  publicJwk: webcrypto.JsonWebKey & { kid: string; alg: string; use: string };
}

const b64u = (b: Uint8Array | ArrayBuffer | string): string =>
  (typeof b === 'string' ? Buffer.from(b, 'utf8') : Buffer.from(b instanceof ArrayBuffer ? new Uint8Array(b) : b)).toString('base64url');

async function makeKey(alg: FakeOpAlg, kid: string): Promise<SigningKey> {
  const pair =
    alg === 'RS256'
      ? await crypto.subtle.generateKey({ name: 'RSASSA-PKCS1-v1_5', modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: 'SHA-256' }, true, ['sign', 'verify'])
      : ((await crypto.subtle.generateKey({ name: 'Ed25519' }, true, ['sign', 'verify'])) as unknown as CryptoKeyPair);
  const jwk = await crypto.subtle.exportKey('jwk', pair.publicKey);
  return { kid, alg, privateKey: pair.privateKey, publicJwk: { ...jwk, kid, alg, use: 'sig', key_ops: undefined, ext: undefined } };
}

async function signWith(key: SigningKey, signingInput: string): Promise<Uint8Array> {
  const algorithm = key.alg === 'RS256' ? { name: 'RSASSA-PKCS1-v1_5' } : { name: 'Ed25519' };
  return new Uint8Array(await crypto.subtle.sign(algorithm, key.privateKey, new TextEncoder().encode(signingInput)));
}

function form(status: number, body: Record<string, unknown>): Response {
  return Response.json(body, { status, headers: { 'Cache-Control': 'no-store' } });
}

/** The fake OP; start() it, close() it in afterAll. */
export class FakeOp {
  /** The person the next authorize signs in. */
  user: FakeUser = { sub: 'fake-subject', email: 'person@example.org', email_verified: true, name: 'Fake Person' };
  /** Applied to every ID token until reset(). */
  tamper: Tamper = {};
  /** The next authorize redirects back with this error instead of a code. */
  authorizeError: string | null = null;
  /** Discovery answers 500 while true. */
  failDiscovery = false;
  /** Requests per endpoint. */
  readonly counts = { discovery: 0, jwks: 0, authorize: 0, token: 0 };
  /** The query of the last authorize request. */
  lastAuthorize: URLSearchParams | null = null;
  /** How the last token request authenticated. */
  lastClientAuth: 'basic' | 'post' | null = null;

  readonly clientId: string;
  readonly clientSecret: string;
  private readonly codes = new Map<string, CodeGrant>();
  private server: Server<undefined> | null = null;
  private key!: SigningKey;
  private foreign!: SigningKey;

  private constructor(private readonly opts: FakeOpOptions) {
    this.clientId = opts.clientId ?? 'license-portal';
    this.clientSecret = opts.clientSecret ?? 'license-portal-test-secret';
  }

  static async start(opts: FakeOpOptions): Promise<FakeOp> {
    const op = new FakeOp(opts);
    const alg = opts.alg ?? 'RS256';
    op.key = await makeKey(alg, 'fake-op-1');
    op.foreign = await makeKey(alg, 'fake-op-1');
    const path = opts.issuerPath ?? '/dex';
    op.server = Bun.serve({
      hostname: '127.0.0.1',
      port: 0,
      routes: {
        [`${path}/.well-known/openid-configuration`]: () => op.discovery(),
        [`${path}/keys`]: () => {
          op.counts.jwks++;
          return Response.json({ keys: [op.key.publicJwk] });
        },
        [`${path}/auth`]: (req: Request) => op.authorizeEndpoint(new URL(req.url)),
        [`${path}/token`]: { POST: (req: Request) => op.tokenEndpoint(req) },
      },
      fetch: () => new Response('not found', { status: 404 }),
    });
    return op;
  }

  /** The issuer it advertises: publicIssuer, else http://127.0.0.1:<port>/dex. */
  get issuer(): string {
    return this.opts.publicIssuer ?? this.localIssuer;
  }

  /** Where it really listens: http://127.0.0.1:<port>/dex. */
  get localIssuer(): string {
    if (!this.server) throw new Error('the fake OP is not running');
    return `http://127.0.0.1:${this.server.port}${this.opts.issuerPath ?? '/dex'}`;
  }

  /** A URL of the advertised issuer mapped to where the OP listens. */
  toLocal(url: string): string {
    return this.opts.publicIssuer && url.startsWith(this.opts.publicIssuer) ? this.localIssuer + url.slice(this.opts.publicIssuer.length) : url;
  }

  /** A fetch for openid-client (OidcClientOptions.fetch) that reaches publicIssuer here. */
  readonly fetch = (url: string, options: RequestInit): Promise<Response> => fetch(this.toLocal(url), options);

  /** Back to a cooperative OP (the default user stays as set). */
  reset(): void {
    this.tamper = {};
    this.authorizeError = null;
    this.failDiscovery = false;
  }

  close(): void {
    void this.server?.stop(true);
    this.server = null;
  }

  /**
   * Follows the portal's redirect to the authorize endpoint like a browser
   * and returns where the OP sends the browser back (redirect_uri with code
   * and state, or error). Throws when the OP refuses outright.
   */
  async authorize(authorizationUrl: string): Promise<URL> {
    const res = await fetch(this.toLocal(authorizationUrl), { redirect: 'manual' });
    const location = res.headers.get('location');
    if (res.status !== 302 || !location) throw new Error(`authorize answered ${res.status}: ${await res.text()}`);
    return new URL(location);
  }

  private discovery(): Response {
    this.counts.discovery++;
    if (this.failDiscovery) return new Response('down', { status: 500 });
    const issuer = this.issuer;
    const body: Record<string, unknown> = {
      issuer,
      authorization_endpoint: `${issuer}/auth`,
      token_endpoint: `${issuer}/token`,
      jwks_uri: `${issuer}/keys`,
      response_types_supported: ['code'],
      subject_types_supported: ['public'],
      code_challenge_methods_supported: ['S256'],
      scopes_supported: ['openid', 'profile', 'email', 'groups'],
      token_endpoint_auth_methods_supported: this.opts.authMethods ?? ['client_secret_basic', 'client_secret_post'],
      claims_supported: ['iss', 'sub', 'aud', 'exp', 'iat', 'email', 'email_verified', 'name', 'groups'],
    };
    const advertised = this.opts.advertisedAlgs === undefined ? [this.key.alg] : this.opts.advertisedAlgs;
    if (advertised !== null) body.id_token_signing_alg_values_supported = advertised;
    return Response.json(body);
  }

  private authorizeEndpoint(url: URL): Response {
    this.counts.authorize++;
    const q = url.searchParams;
    this.lastAuthorize = new URLSearchParams(q);
    if (q.get('client_id') !== this.clientId) return new Response('unknown client', { status: 400 });
    const redirectUri = q.get('redirect_uri');
    if (redirectUri !== this.opts.redirectUri) return new Response('redirect_uri is not registered', { status: 400 });
    const back = new URL(redirectUri);
    const state = q.get('state');
    if (state !== null) back.searchParams.set('state', state);
    const fail = (error: string) => {
      back.searchParams.set('error', error);
      return Response.redirect(back.href, 302);
    };
    if (this.authorizeError) {
      const error = this.authorizeError;
      this.authorizeError = null;
      return fail(error);
    }
    if (q.get('response_type') !== 'code') return fail('unsupported_response_type');
    if (q.get('code_challenge_method') !== 'S256' || !q.get('code_challenge')) return fail('invalid_request');
    if (!(q.get('scope') ?? '').split(' ').includes('openid')) return fail('invalid_scope');
    if (!state || !q.get('nonce')) return fail('invalid_request');
    const code = randomBytes(24).toString('base64url');
    this.codes.set(code, {
      clientId: this.clientId,
      redirectUri,
      challenge: q.get('code_challenge')!,
      nonce: q.get('nonce')!,
      scope: q.get('scope')!,
      user: structuredClone(this.user),
    });
    back.searchParams.set('code', code);
    return Response.redirect(back.href, 302);
  }

  private clientAuth(req: Request, body: URLSearchParams): boolean {
    const header = req.headers.get('authorization');
    if (header?.startsWith('Basic ')) {
      const decoded = Buffer.from(header.slice(6), 'base64').toString('utf8');
      const i = decoded.indexOf(':');
      const id = decodeURIComponent(decoded.slice(0, i).replace(/\+/g, ' '));
      const secret = decodeURIComponent(decoded.slice(i + 1).replace(/\+/g, ' '));
      this.lastClientAuth = 'basic';
      return id === this.clientId && secret === this.clientSecret;
    }
    this.lastClientAuth = 'post';
    return body.get('client_id') === this.clientId && body.get('client_secret') === this.clientSecret;
  }

  private async tokenEndpoint(req: Request): Promise<Response> {
    this.counts.token++;
    if (!(req.headers.get('content-type') ?? '').startsWith('application/x-www-form-urlencoded')) return form(400, { error: 'invalid_request' });
    const body = new URLSearchParams(await req.text());
    if (!this.clientAuth(req, body)) return form(401, { error: 'invalid_client' });
    if (body.get('grant_type') !== 'authorization_code') return form(400, { error: 'unsupported_grant_type' });
    const code = body.get('code') ?? '';
    const grant = this.codes.get(code);
    // Single use: a code is gone after its first exchange, whatever the outcome.
    this.codes.delete(code);
    if (!grant) return form(400, { error: 'invalid_grant', error_description: 'unknown or used code' });
    if (body.get('redirect_uri') !== grant.redirectUri) return form(400, { error: 'invalid_grant', error_description: 'redirect_uri mismatch' });
    const verifier = body.get('code_verifier') ?? '';
    const challenge = createHash('sha256').update(verifier).digest('base64url');
    if (challenge !== grant.challenge) return form(400, { error: 'invalid_grant', error_description: 'PKCE verification failed' });

    const now = Math.floor(Date.now() / 1000);
    const claims: Record<string, unknown> = {
      iss: this.issuer,
      aud: grant.clientId,
      exp: now + 3600,
      iat: now,
      auth_time: now,
      nonce: grant.nonce,
      ...grant.user,
    };
    this.tamper.claims?.(claims);
    const header = { alg: this.key.alg, kid: this.key.kid, typ: 'JWT', ...this.tamper.header };
    const input = `${b64u(JSON.stringify(header))}.${b64u(JSON.stringify(claims))}`;
    const signature = await signWith(this.tamper.foreignKey ? this.foreign : this.key, input);
    if (this.tamper.badSignature) signature[signature.length - 1] = signature[signature.length - 1]! ^ 0x01;
    return form(200, {
      access_token: randomBytes(24).toString('base64url'),
      token_type: 'Bearer',
      expires_in: 3600,
      id_token: `${input}.${b64u(signature)}`,
    });
  }
}
