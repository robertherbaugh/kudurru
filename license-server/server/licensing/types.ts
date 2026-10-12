// The key format seams (design spec §0.2-0.6): the claims a key carries,
// the Signer that turns them into a compact JWS byte-identical to Go's
// license.Sign, and the SigningState (signer, why it is missing, the trusted
// keyring) every request and service sees. Stream B implements them (jws.ts,
// keyring.ts, claims.ts); everyone else depends only on these types. The
// private key never leaves the Signer: it is imported non-extractable and
// never logged or stored.

import type { Feature, Plan } from '../../shared/catalog';

/**
 * The signed payload of a key, exactly internal/license.Claims. The JSON
 * is written in Go's struct order with no whitespace:
 *
 *   jti, iss, sub, plan, nodes, features (omitted when absent), iat,
 *   nbf (omitted when 0; the portal never sets it), exp
 *
 * with Go's HTML-safe escaping: < > & U+2028 U+2029 are written as
 * \u003c \u003e \u0026 \u2028 \u2029. Times are whole Unix seconds; exp > iat.
 */
export interface Claims {
  /** The license id (licenses.id). */
  jti: string;
  /** Always ISSUER ("kudurru"). */
  iss: 'kudurru';
  /** The licensee printed on nodes; not blank under Go's TrimSpace. */
  sub: string;
  plan: Plan;
  /** At least 1. */
  nodes: number;
  /** Absent: every feature, including later ones. Never empty. */
  features?: readonly Feature[];
  /** Signing time, Unix seconds. */
  iat: number;
  /** Not before; the portal never sets it (a future nbf breaks a node that installs the key early). Present only on imported keys. */
  nbf?: number;
  /** expFor(expiresOn): 23:59:59 UTC of the last valid day, Unix seconds. */
  exp: number;
}

/**
 * The claims of a key someone else signed (import): checked for shape as
 * Go's Claims.Validate does, but features may name ones this build does not
 * know (nodes ignore them).
 */
export type ForeignClaims = Omit<Claims, 'features'> & { features?: readonly string[] };

/** The one signing key of the process (LICENSE_SIGNING_KEY_FILE + LICENSE_SIGNING_KID). */
export interface Signer {
  /** The key id in the JWS header; <kid>.pub is in the trusted keys dir. */
  readonly kid: string;
  /** The public key as the .pub file has it: std base64 of the 32 raw bytes. */
  readonly publicKey: string;
  /** LICENSE_SIGNING_TRUSTED_SINCE: the first kudurru release that trusts the kid. */
  readonly trustedSince?: string;
  /** A test key no release trusts (dev mode only: e2e-test, server-test-*, the golden key). */
  readonly testKey: boolean;
  /** The compact JWS for `claims`, header {"alg":"EdDSA","kid":<kid>,"typ":"JWT"}. Refuses claims that fail validation. */
  sign(claims: Claims): Promise<string>;
}

/**
 * The trusted keyring: kid → public key as the <kid>.pub file has it (std
 * base64 of the 32 raw bytes), read from LICENSE_TRUSTED_KEYS_DIR at
 * startup. Import verifies foreign keys against it; the System page lists
 * its kids.
 */
export type Keyring = ReadonlyMap<string, string>;

/**
 * Everything about signing the process decided at startup (stream B's
 * keyring.ts loads it; main.ts passes it to the router as AppDeps.signing):
 * the signer if one is usable, why not otherwise, and the keyring. It never
 * changes while the server runs. Handlers read it as ctx.signing and
 * services as ServiceCtx.signing.
 */
export interface SigningState {
  /** null: issuing answers 503 signing_unavailable and the public form 503 unavailable. */
  readonly signer: Signer | null;
  /**
   * Why signer is null, as one short phrase for /readyz ("unavailable:
   * <reason>"), the System page and the startup log, e.g. "no signing key
   * is configured". null when a signer is loaded.
   */
  readonly unavailableReason: string | null;
  readonly keyring: Keyring;
}

/** A SigningState without a signer (tests, and startup when no key is configured). */
export function signingUnavailable(reason: string, keyring: Keyring = new Map()): SigningState {
  return { signer: null, unavailableReason: reason, keyring };
}

/** A SigningState with a loaded signer; its kid must be in the keyring (startup refuses otherwise). */
export function signingReady(signer: Signer, keyring: Keyring): SigningState {
  if (keyring.get(signer.kid) !== signer.publicKey) throw new Error(`signing key ${signer.kid} is not in the trusted keyring with the same public key`);
  return { signer, unavailableReason: null, keyring };
}

/** The kids of the keyring, sorted (the System page, the startup log). */
export function trustedKids(signing: Pick<SigningState, 'keyring'>): string[] {
  return [...signing.keyring.keys()].sort();
}
