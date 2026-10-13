// Who signed in (design spec §5, §6 "Sign-in" step 2): verified ID token
// claims → a users row (keyed by issuer and subject, never by email) and
// the roles the IdP grants.
// - email is kept only when email_verified is true or "true" (Cognito
//   sends a string); otherwise it is stored as null;
// - name: name, else preferred_username, else the verified email;
// - groups come only from OIDC_GROUPS_CLAIM (a string or an array of
//   strings); there is no implicit `roles` claim;
// - ROLE_SUPERADMIN_GROUPS and ROLE_LICENSE_ADMIN_GROUPS count only when
//   every OIDC_ADMIN_CLAIM_RULES entry matches (exact, by JSON text), so an
//   outsider who names a group like ours at a public IdP gets nothing;
// - a sign-in that maps to no role gets DEFAULT_ROLE (customer, or none).
// idp_roles are rewritten at every sign-in; granted_roles (the portal's
// own grants) are never touched here.

import { ROLES, type Role } from '../../shared/permissions';
import type { ClaimRule, Config } from '../config';
import { newId } from '../db/ids';
import { one, textArray, type Queryable } from '../db/sql';
import { goTrimSpace, hasControlChars, isEmailSyntax, normalizeEmail } from '../http/body';

/** A verified ID token's claims (openid-client's tokens.claims()). */
export type IdClaims = Readonly<Record<string, unknown>>;

/** The longest name kept (users.name), in UTF-16 code units. */
export const MAX_NAME_LENGTH = 200;

/** The most groups read from one token. */
export const MAX_GROUPS = 1000;

/** email_verified as kudurru reads it: true or "true"; anything else (or nothing) is false. */
export function emailVerifiedClaim(v: unknown): boolean {
  return v === true || v === 'true';
}

/** A scalar claim compared with a required value by its JSON text: strings exactly, booleans as true/false, numbers as written. */
export function claimEquals(v: unknown, want: string): boolean {
  if (typeof v === 'string') return v === want;
  if (typeof v === 'boolean') return String(v) === want;
  if (typeof v === 'number' && Number.isFinite(v)) return JSON.stringify(v) === want;
  return false;
}

/** Whether every OIDC_ADMIN_CLAIM_RULES entry matches (vacuously true without rules). */
export function adminRulesMatch(claims: IdClaims, rules: readonly ClaimRule[]): boolean {
  return rules.every((r) => claimEquals(claims[r.claim], r.value));
}

/** The groups of OIDC_GROUPS_CLAIM: a string or an array (non-string and empty entries dropped). */
export function groupsOf(claims: IdClaims, claimName: string): string[] {
  const v = claims[claimName];
  if (typeof v === 'string') return v === '' ? [] : [v];
  if (!Array.isArray(v)) return [];
  const out: string[] = [];
  for (const g of v as unknown[]) {
    if (typeof g === 'string' && g !== '') out.push(g);
    if (out.length >= MAX_GROUPS) break;
  }
  return out;
}

/** Whether the groups claim is distributed (Entra ID group overage: `_claim_names.<claim>`), which the portal refuses to sign in. */
export function hasDistributedGroups(claims: IdClaims, claimName: string): boolean {
  const names = claims._claim_names;
  return names !== null && typeof names === 'object' && !Array.isArray(names) && Object.prototype.hasOwnProperty.call(names, claimName);
}

/** A display string from a claim: control characters dropped, Go-trimmed, at most MAX_NAME_LENGTH code units (never half a surrogate pair). */
export function cleanName(v: unknown): string {
  if (typeof v !== 'string') return '';
  let s = '';
  for (const ch of v) {
    if (hasControlChars(ch)) continue;
    const c = ch.codePointAt(0)!;
    if (c >= 0xd800 && c <= 0xdfff) continue;
    s += ch;
  }
  s = goTrimSpace(s);
  if (s.length <= MAX_NAME_LENGTH) return s;
  let out = '';
  for (const ch of s) {
    if (out.length + ch.length > MAX_NAME_LENGTH) break;
    out += ch;
  }
  return goTrimSpace(out);
}

/** The person as the portal records them. */
export interface Identity {
  issuer: string;
  subject: string;
  /** Normalized (trimmed, lower case) and only when verified; null otherwise. */
  email: string | null;
  emailVerified: boolean;
  name: string;
  groups: string[];
  /** auth_time of the token, when it is a sane number; else null. */
  authTime: Date | null;
}

/** The Identity of verified claims (iss and sub were checked by the caller). */
export function identityFromClaims(claims: IdClaims, groupsClaim: string, now: Date): Identity {
  const verified = emailVerifiedClaim(claims.email_verified);
  const rawEmail = typeof claims.email === 'string' ? normalizeEmail(claims.email) : '';
  const email = verified && isEmailSyntax(rawEmail) ? rawEmail : null;
  const name = cleanName(claims.name) || cleanName(claims.preferred_username) || (email ?? '');
  const at = claims.auth_time;
  // auth_time in the future (beyond a minute of skew) is not believed.
  const authTime = typeof at === 'number' && Number.isFinite(at) && at > 0 && at * 1000 <= now.getTime() + 60_000 ? new Date(Math.floor(at) * 1000) : null;
  return {
    issuer: String(claims.iss),
    subject: String(claims.sub),
    email,
    emailVerified: email !== null,
    name,
    groups: groupsOf(claims, groupsClaim),
    authTime,
  };
}

/** The IdP roles of a sign-in and whether admin groups were present but ignored (claim rules did not match). */
export interface MappedRoles {
  roles: Role[];
  adminGroupsIgnored: boolean;
}

/** Maps the groups to roles (spec §3 ROLE_*_GROUPS, OIDC_ADMIN_CLAIM_RULES, DEFAULT_ROLE), in ROLES order. */
export function mapIdpRoles(identity: Pick<Identity, 'groups'>, claims: IdClaims, config: Pick<Config, 'oidc' | 'roles'>): MappedRoles {
  const groups = new Set(identity.groups);
  const inAny = (list: readonly string[]) => list.some((g) => groups.has(g));
  const held = new Set<Role>();
  const wantsSuper = inAny(config.roles.groups.superadmin);
  const wantsAdmin = inAny(config.roles.groups.license_admin);
  const rulesOk = adminRulesMatch(claims, config.oidc.adminClaimRules);
  if (rulesOk && wantsSuper) held.add('superadmin');
  if (rulesOk && wantsAdmin) held.add('license_admin');
  if (inAny(config.roles.groups.customer)) held.add('customer');
  if (held.size === 0 && config.roles.defaultRole === 'customer') held.add('customer');
  return { roles: ROLES.filter((r) => held.has(r)), adminGroupsIgnored: !rulesOk && (wantsSuper || wantsAdmin) };
}

/** The users row after a sign-in. */
export interface SignedInUser {
  id: string;
  issuer: string;
  subject: string;
  email: string | null;
  emailVerified: boolean;
  name: string;
  idpRoles: Role[];
  grantedRoles: Role[];
  disabledAt: Date | null;
  /** The row was created by this sign-in. */
  created: boolean;
}

interface UserRow {
  id: string;
  issuer: string;
  subject: string;
  email: string | null;
  email_verified: boolean;
  name: string;
  idp_roles: string[];
  granted_roles: string[];
  disabled_at: Date | null;
  inserted: boolean;
}

const asRoles = (xs: readonly string[]): Role[] => ROLES.filter((r) => xs.includes(r));

/**
 * Creates or updates the users row of (issuer, subject): email, its
 * verification, name and idp_roles follow the token; granted_roles,
 * disabled_at and last_login_at are left alone (the caller sets
 * last_login_at once the sign-in succeeds).
 */
export async function upsertUser(q: Queryable, identity: Identity, idpRoles: readonly Role[], now: Date): Promise<SignedInUser> {
  const rows = await q<UserRow[]>`
    insert into users (id, issuer, subject, email, email_verified, name, idp_roles, created_at)
    values (${newId('user')}, ${identity.issuer}, ${identity.subject}, ${identity.email}, ${identity.emailVerified}, ${identity.name},
            ${textArray(q, idpRoles)}, ${now})
    on conflict (issuer, subject) do update
       set email = excluded.email, email_verified = excluded.email_verified, name = excluded.name, idp_roles = excluded.idp_roles
    returning id, issuer, subject, email, email_verified, name, idp_roles, granted_roles, disabled_at, (xmax::text = '0') as inserted`;
  const r = one(rows, 'users row');
  return {
    id: r.id,
    issuer: r.issuer,
    subject: r.subject,
    email: r.email,
    emailVerified: r.email_verified,
    name: r.name,
    idpRoles: asRoles(r.idp_roles),
    grantedRoles: asRoles(r.granted_roles),
    disabledAt: r.disabled_at,
    created: r.inserted,
  };
}
