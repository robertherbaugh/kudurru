// The wire contract of the license portal (design spec §8): every request
// body, every response DTO, the list shapes and the error envelope. These
// are the ONLY wire types: the server builds its responses from them and
// the UI's API client reads them, so the two never drift. RouteTypes at the
// bottom ties each route of shared/routes.ts to its query, body and
// response.
//
// Wire conventions:
// - ids are prefixed strings (ID_PREFIX); a license id is also the key's jti;
// - instants are RFC 3339 UTC strings ('2026-10-12T21:04:05.123Z', Instant);
// - a key's expiry is `expiresOn`, the last valid UTC day ('YYYY-MM-DD',
//   shared/dates.ts), never an instant;
// - lists are { items, total } (offset paging), { items, next? } (keyset
//   paging) or { items } (short, unpaged);
// - absent optional fields are omitted; fields that can be empty are null.
//
// Customer-facing DTOs (MyLicense, MyLicenseDetail, MyRequest) never carry
// notes, agreement references, withdraw reasons or who did what (§9).

import type { Feature, FeatureInfo, LicenseState, Plan, PlanInfo, Policy } from './catalog';
import type { ExpiresOn } from './dates';
import type { Permission, Role } from './permissions';
import type { RouteId } from './routes';

export type { ExpiresOn } from './dates';

// --- primitives ------------------------------------------------------------

/** An instant: RFC 3339 in UTC (Date.prototype.toISOString). */
export type Instant = string;

/** Any JSON value (audit details, attestation extras). */
export type JsonValue = string | number | boolean | null | JsonValue[] | { [key: string]: JsonValue };

/** A JSON object. */
export type JsonObject = { [key: string]: JsonValue };

/** The prefixes of the portal's ids. Ids are the prefix plus base64url of 9 random bytes (licensegen's license id format). */
export const ID_PREFIX = {
  user: 'usr_',
  customer: 'cus_',
  /** Licenses issued by the portal; imported keys keep their own jti. */
  license: 'lic_',
  key: 'key_',
  request: 'req_',
  publicRequest: 'pkr_',
} as const;

export type IdKind = keyof typeof ID_PREFIX;

/** Default and largest `limit` of the paged lists. */
export const DEFAULT_PAGE_LIMIT = 50;
export const MAX_PAGE_LIMIT = 200;

/** A list paged by offset (customers, licenses). */
export interface Page<T> {
  items: T[];
  /** Matching rows across all pages. */
  total: number;
}

/** A list paged by a cursor (audit). Pass `next` as `before` to get the following page; absent on the last page. */
export interface KeysetPage<T> {
  items: T[];
  next?: string;
}

/** A short list that is never paged. */
export interface Items<T> {
  items: T[];
}

/** Who did something, as recorded at the time (the label survives renames and deleted users). */
export interface ActorRef {
  /** The user id (usr_…), or null for the public form, the CLI and the system. */
  id: string | null;
  /** "Name (email)", "cli", "system" or the public requester's email. */
  label: string;
}

/** A user as other records show them. */
export interface UserRef {
  id: string;
  name: string;
  email: string | null;
}

/** A customer as other records show them. */
export interface CustomerRef {
  id: string;
  name: string;
  kind: CustomerKind;
}

// --- errors -----------------------------------------------------------------

/** Every error key the API answers with (spec §8). */
export type ErrorKey =
  | 'invalid'
  | 'unauthenticated'
  | 'forbidden'
  | 'csrf'
  | 'not_found'
  | 'method_not_allowed'
  | 'conflict'
  | 'license_withdrawn'
  | 'unsupported_media_type'
  | 'payload_too_large'
  | 'rate_limited'
  | 'issuing_quota'
  | 'signing_unavailable'
  | 'unavailable'
  | 'idp_unavailable'
  | 'login_busy'
  | 'invalid_token'
  | 'email_unverified'
  | 'forbidden_origin'
  | 'no_role'
  | 'internal';

/** The HTTP status of each error key. */
export const ERROR_STATUS: Readonly<Record<ErrorKey, number>> = Object.freeze({
  invalid: 400,
  invalid_token: 400,
  email_unverified: 400,
  unauthenticated: 401,
  forbidden: 403,
  csrf: 403,
  forbidden_origin: 403,
  no_role: 403,
  not_found: 404,
  method_not_allowed: 405,
  conflict: 409,
  license_withdrawn: 409,
  payload_too_large: 413,
  unsupported_media_type: 415,
  rate_limited: 429,
  issuing_quota: 429,
  internal: 500,
  signing_unavailable: 503,
  unavailable: 503,
  idp_unavailable: 503,
  login_busy: 503,
});

/** The hint of a 403 forbidden: the permission the route needs. */
export interface MissingPermissionHint {
  missing: Permission;
}

/**
 * A hint next to the message: the request id of a 500 (quote it to the
 * operator, it is in the logs), the missing permission of a 403, or a short
 * string with a next step.
 */
export type ErrorHint = string | MissingPermissionHint;

/** Every error response: `{"error":{"key","message","hint"?,"fields"?}}`. */
export interface ApiErrorBody {
  error: {
    key: ErrorKey;
    /** One sentence for people; never internal detail. */
    message: string;
    hint?: ErrorHint;
    /** The body fields a 400 invalid failed on, as dotted paths ("newCustomer.name"); an array element reports its array. */
    fields?: string[];
  };
}

/** Whether a parsed response body is the error envelope (the UI's client uses it before trusting `error.key`). */
export function isApiErrorBody(v: unknown): v is ApiErrorBody {
  if (v === null || typeof v !== 'object' || !('error' in v)) return false;
  const e = v.error;
  return e !== null && typeof e === 'object' && typeof (e as { key?: unknown }).key === 'string' && typeof (e as { message?: unknown }).message === 'string';
}

// --- shared vocabulary -----------------------------------------------------

export type CustomerKind = 'person' | 'organization';

/** How a license came to be. */
export type LicenseSource = 'admin' | 'web' | 'portal' | 'import';

/** Why a key was signed (license_keys.reason). */
export type KeyReason = 'issue' | 'renew' | 'change' | 'import';

/** invited: has an invite link and no user yet; linked: belongs to a signed-in user. */
export type MemberStatus = 'invited' | 'linked';

export type RequestStatus = 'open' | 'approved' | 'rejected' | 'canceled';

/** Why a personal request waits for a person instead of getting a key at once. */
export type ReviewReason = 'auto_approve_off' | 'daily_cap' | 'withdrawn_history' | 'blocked';

/** Where a personal request came from. */
export type RequestSource = 'web' | 'portal';

/**
 * What a personal key request led to (services/personal.ts):
 * issued: a new key was mailed; renewed: the existing personal key was
 * renewed and mailed; resent: the current key was mailed again;
 * pending_review: a person decides (the requester is told by mail).
 */
export type PersonalOutcome = 'issued' | 'renewed' | 'resent' | 'pending_review';

/** Where a personal attestation was given. */
export type AttestationSource = 'kudurru.io form' | 'portal' | 'admin';

/**
 * The record of the non-commercial certification behind a personal key
 * (licenses.attestation, license_requests.attestation; the admin view).
 * kudurru.io form and portal: license, statement (ATTESTATION_STATEMENT),
 * at, ip, userAgent, email. admin (a personal key issued by an admin): by
 * and at only.
 */
export interface Attestation {
  source: AttestationSource;
  at: Instant;
  /** ATTESTATION_LICENSE. */
  license?: string;
  /** ATTESTATION_STATEMENT, word for word as shown. */
  statement?: string;
  ip?: string | null;
  userAgent?: string | null;
  email?: string;
  /** The admin's user id (source admin). */
  by?: string;
}

/** What a customer sees of their own attestation. */
export interface AttestationSummary {
  source: AttestationSource;
  at: Instant;
  statement: string | null;
  license: string | null;
}

// --- auth and health -------------------------------------------------------

/** GET /healthz: the process is up. */
export interface Healthz {
  ok: true;
}

/** GET /readyz: 200 when everything is ready, 503 otherwise. */
export interface Readyz {
  ok: boolean;
  db: 'ok' | 'error';
  /** pending: this build has migrations the database lacks; unknown_version: the database is newer than this build. */
  migrations: 'ok' | 'pending' | 'unknown_version' | 'error';
  /** 'ready', or 'unavailable: <reason>' (no key configured; the server still runs). */
  signing: 'ready' | `unavailable: ${string}`;
}

/** GET /api/v1/auth/config: what the login page needs before anyone signs in. */
export interface AuthConfig {
  /** OIDC_DISPLAY_NAME, the sign-in button's label. */
  displayName: string;
  devMode: boolean;
}

/** GET /api/v1/auth/login query. */
export interface LoginQuery {
  /** Where to go after signing in; SafeNext falls back to "/". */
  next?: string;
}

/** GET /api/v1/auth/me: the signed-in person. */
export interface Me {
  user: {
    id: string;
    name: string;
    /** Kept only when the IdP says it is verified; null otherwise. */
    email: string | null;
    emailVerified: boolean;
  };
  /** Effective roles (IdP ∪ granted), in ROLES order. */
  roles: Role[];
  /** The permissions those roles grant, in PERMISSIONS order. */
  permissions: Permission[];
  /** Echo it as X-Kudurru-CSRF on every unsafe request. */
  csrf: string;
  devMode: boolean;
}

/** GET /api/v1/catalog: the lists and rules the UI words and validates with. */
export interface Catalog {
  plans: PlanInfo[];
  features: FeatureInfo[];
  policy: Policy;
  signing: {
    /** False: issuing answers 503 signing_unavailable. */
    available: boolean;
    kid?: string;
    /** LICENSE_SIGNING_TRUSTED_SINCE: "Works with kudurru <trustedSince> or later". */
    trustedSince?: string;
    /** The signer is a test key no kudurru release trusts (dev mode banner). */
    testKey: boolean;
  };
  mail: {
    /** False: nothing is mailed; the UI offers copy and download instead. */
    configured: boolean;
  };
  /** Today as the server sees it (UTC); date presets count from it. */
  today: ExpiresOn;
}

// --- the customer's own view (self.*) ---------------------------------------

/** One of the signed-in person's licenses (they are a member of its customer). */
export interface MyLicense {
  id: string;
  customer: { id: string; name: string };
  licensee: string;
  plan: Plan;
  nodes: number;
  /** null: every feature, including ones added later. Keys imported from licensegen may name features this build does not know. */
  features: string[] | null;
  expiresOn: ExpiresOn;
  state: LicenseState;
  /** The last day a key of this license still works on nodes (grace included), UTC. */
  keepsWorkingUntil: ExpiresOn;
  /** Set when the person can renew it now (personal plan, active, within the renew window, in grace or expired): the expiry the renewal gives. */
  renewal: { expiresOn: ExpiresOn } | null;
}

/** GET /api/v1/me/licenses/:id. */
export interface MyLicenseDetail extends MyLicense {
  /** The current key; null when the license is withdrawn. */
  key: {
    kid: string;
    token: string;
    issuedAt: Instant;
    expiresOn: ExpiresOn;
  } | null;
  /** "Works with kudurru <version> or later" when the signing kid's first trusting release is known. */
  worksWithKudurru: string | null;
  /** Everyone with access to this customer's licenses (read-only). */
  people: { email: string; status: MemberStatus }[];
  /** Set when withdrawn: the date, the message for the customer and how long installed keys keep working. */
  withdrawn: {
    at: Instant;
    keepsWorkingUntil: ExpiresOn;
    customerMessage: string;
  } | null;
  attestation: AttestationSummary | null;
}

/** One of the signed-in person's personal key requests. */
export interface MyRequest {
  id: string;
  status: RequestStatus;
  name: string;
  use: string;
  createdAt: Instant;
  decidedAt: Instant | null;
  /** The note sent with a rejection, or why it was canceled. */
  decisionNote: string;
  /** The license it led to, if any. */
  licenseId: string | null;
}

/** POST /api/v1/me/requests: a personal key for the signed-in person (needs an IdP-verified email). */
export interface PersonalRequestBody {
  /** The licensee: 1-200 UTF-16 code units, not blank, no control characters. */
  name: string;
  /** What kudurru runs for: 1-1000 UTF-16 code units, not blank. */
  use: string;
  /** The certification (ATTESTATION_STATEMENT) was ticked: must be exactly true. */
  noncommercial: true;
}

export interface PersonalRequestResult {
  outcome: PersonalOutcome;
  /** Set for pending_review. */
  requestId?: string;
  /** Set for issued, renewed and resent. */
  licenseId?: string;
}

/** POST /api/v1/me/invites/preview and POST /api/v1/me/invites/redeem. */
export interface RedeemInviteBody {
  /** The token of the invite link (/invites/<token>). */
  token: string;
}

/**
 * POST /api/v1/me/invites/preview: what the invite page shows before the
 * person accepts ("Accept access to the licenses of <customer>?"). Changes
 * nothing. 400 invalid_token for an unknown or already redeemed token; an
 * expired one still names the customer, so the page can say whom to ask
 * for a new link (redeem then answers 400 invalid_token).
 */
export interface InvitePreview {
  customer: { id: string; name: string };
  /** The address the invite was sent to (any signed-in person may redeem it). */
  email: string;
  expiresAt: Instant;
  expired: boolean;
}

export interface RedeemInviteResult {
  customer: { id: string; name: string };
}

// --- public form (FROZEN: www is coded to it) -------------------------------

/**
 * POST /api/v1/public/key-requests from kudurru.io (CORS, ≤ 8 KiB). Unknown
 * fields are ignored. 202 PublicKeyRequestAccepted for every valid input;
 * 400 invalid with `fields`; 429 rate_limited with Retry-After; 503
 * unavailable.
 */
export interface PublicKeyRequestBody {
  /** 1-200 UTF-16 code units, not blank under Go's unicode.IsSpace, no Cc characters, no lone surrogates. */
  name: string;
  /** ≤ 254 characters, one @, a non-empty local part, a domain with a dot, no spaces. */
  email: string;
  /** 1-1000 UTF-16 code units, not blank. */
  use: string;
  /** Exactly true (else 400 with fields ["noncommercial"]). */
  noncommercial: true;
  /** The honeypot: non-empty → 202 and nothing happens. */
  website?: string;
}

export interface PublicKeyRequestAccepted {
  status: 'sent';
}

/** POST /api/v1/public/key-requests/verify (the /verify page; no session, no CORS). */
export interface VerifyBody {
  /** The token of the confirmation link (/verify?token=…). */
  token: string;
}

/** Never contains the key: it goes by mail. 400 invalid_token for an unknown, used or expired token. */
export interface VerifyResult {
  outcome: PersonalOutcome;
}

// --- licenses (admin) -------------------------------------------------------

/** A signed key, without its token (the history of a license). */
export interface KeyRecord {
  id: string;
  kid: string;
  reason: KeyReason;
  issuedAt: Instant;
  expiresOn: ExpiresOn;
  issuedBy: ActorRef;
  /** True for the one key of the license that is not superseded. */
  current: boolean;
  supersededAt: Instant | null;
  /** The key that replaced it. */
  supersededBy: string | null;
}

/** A license in lists. */
export interface LicenseSummary {
  id: string;
  customer: CustomerRef;
  licensee: string;
  plan: Plan;
  nodes: number;
  /** null: every feature, including ones added later. */
  features: string[] | null;
  /** The current key's expiry. */
  expiresOn: ExpiresOn;
  state: LicenseState;
  /** The last day any key of this license still works on nodes: the latest key expiry + 30 days. */
  keepsWorkingUntil: ExpiresOn;
  source: LicenseSource;
  createdAt: Instant;
  updatedAt: Instant;
}

/** GET /api/v1/licenses/:id and the answer of every license action. */
export interface LicenseDetail extends LicenseSummary {
  /** The current key, the only token the API returns for a license. */
  currentKey: {
    id: string;
    kid: string;
    token: string;
    issuedAt: Instant;
    expiresOn: ExpiresOn;
  };
  /** Every key ever signed for it, newest first, metadata only. */
  keys: KeyRecord[];
  attestation: Attestation | null;
  agreementRef: string;
  notes: string;
  createdBy: ActorRef;
  /** "Works with kudurru <version> or later" for the current key's kid, when known. */
  worksWithKudurru: string | null;
  withdrawn: {
    at: Instant;
    by: ActorRef;
    /** Internal: never shown to the customer. */
    reason: string;
    /** Shown to the customer on their license page. */
    customerMessage: string;
    keepsWorkingUntil: ExpiresOn;
  } | null;
}

/** GET /api/v1/licenses query. */
export interface LicenseListQuery {
  /** Matches the license id, the licensee and the customer name. */
  q?: string;
  state?: LicenseState;
  plan?: Plan;
  customerId?: string;
  /** Licenses whose current key expires within this many days (30, 60, 90 in the UI), expired ones excluded. */
  expiringWithinDays?: number;
  limit?: number;
  offset?: number;
}

/** A customer created together with a license (the issue wizard's "new customer"). */
export interface NewCustomerBody {
  name: string;
  kind: CustomerKind;
  contactEmail?: string | null;
}

interface IssueLicenseFields {
  /** Printed on the key (sub): the rules of PublicKeyRequestBody.name. */
  licensee: string;
  plan: Plan;
  nodes: number;
  /** null: every feature, including ones added later; otherwise a non-empty list of known features. Never []. */
  features: Feature[] | null;
  /** Tomorrow at the earliest; within the plan's term limits (Policy). */
  expiresOn: ExpiresOn;
  agreementRef?: string;
  notes?: string;
  /** Mail the key to the customer's contact email after signing. */
  emailCustomer?: boolean;
}

/** POST /api/v1/licenses: for an existing customer or a new one, never both. */
export type IssueLicenseBody =
  | (IssueLicenseFields & { customerId: string; newCustomer?: undefined })
  | (IssueLicenseFields & { newCustomer: NewCustomerBody; customerId?: undefined });

/** POST /api/v1/licenses/:id/renew: the same claims with a later expiry. */
export interface RenewBody {
  /** After the current expiresOn. */
  expiresOn: ExpiresOn;
  emailCustomer?: boolean;
}

/** POST /api/v1/licenses/:id/change: signs a new key with the changed claims (the issue policy applies to the result). */
export interface ChangeBody {
  licensee?: string;
  plan?: Plan;
  nodes?: number;
  features?: Feature[] | null;
  expiresOn?: ExpiresOn;
  /** Moves the license to another customer (audited as license.moved). */
  customerId?: string;
  emailCustomer?: boolean;
}

/** POST /api/v1/licenses/:id/withdraw. Final. */
export interface WithdrawBody {
  /** Internal reason, 1-1000 characters. */
  reason: string;
  /** Shown to the customer on their license page. */
  customerMessage?: string;
  /** Also block the customer's member emails from the personal flow. */
  blockPersonal?: boolean;
}

/** POST /api/v1/licenses/import: a key signed before the portal existed (licensegen). */
export interface ImportBody {
  /** The compact JWS; whitespace is ignored as on the nodes. */
  token: string;
  customerId: string;
}

/** POST /api/v1/licenses/:id/email: mails the current key. */
export interface EmailKeyBody {
  /** Default: the customer's contact email. */
  to?: string;
}

export interface EmailKeyResult {
  sentTo: string;
}

// --- customers and members --------------------------------------------------

/** A customer in lists. */
export interface Customer {
  id: string;
  name: string;
  kind: CustomerKind;
  contactEmail: string | null;
  licenseCount: number;
  memberCount: number;
  createdAt: Instant;
  updatedAt: Instant;
}

/** One person with access to a customer's licenses. */
export interface Member {
  email: string;
  status: MemberStatus;
  /** The linked user (status linked). */
  user: UserRef | null;
  invitedAt: Instant;
  /** Who added or last re-invited them. */
  invitedBy: string;
  linkedAt: Instant | null;
  /** When the current invite link stops working (status invited). */
  inviteExpiresAt: Instant | null;
}

/** GET /api/v1/customers/:id and the answer of customer writes. */
export interface CustomerDetail extends Customer {
  notes: string;
  createdBy: ActorRef;
  members: Member[];
  licenses: LicenseSummary[];
  /** Personal requests of this customer's people, newest first. */
  requests: LicenseRequest[];
}

/** GET /api/v1/customers query. */
export interface CustomerListQuery {
  /** Matches the name, the contact email and member emails. */
  q?: string;
  limit?: number;
  offset?: number;
}

/** POST /api/v1/customers. */
export interface CreateCustomerBody {
  name: string;
  kind: CustomerKind;
  contactEmail?: string | null;
  notes?: string;
}

/** PATCH /api/v1/customers/:id: only the given fields change. */
export interface UpdateCustomerBody {
  name?: string;
  kind?: CustomerKind;
  contactEmail?: string | null;
  notes?: string;
}

/** POST /api/v1/customers/:id/members. */
export interface AddMemberBody {
  email: string;
}

/** The answer of adding or re-inviting a member: the invite link is shown once so the admin can copy it. */
export interface MemberInviteResult {
  member: Member;
  inviteUrl: string;
}

// --- requests and the blocklist (admin) ---------------------------------------

/** A personal key request as admins see it. */
export interface LicenseRequest {
  id: string;
  kind: 'personal';
  source: RequestSource;
  /** The portal user who asked (source portal). */
  requestedBy: UserRef | null;
  requesterEmail: string;
  name: string;
  use: string;
  attestation: Attestation;
  reviewReason: ReviewReason | null;
  status: RequestStatus;
  decidedBy: ActorRef | null;
  decidedAt: Instant | null;
  decisionNote: string;
  resultingLicenseId: string | null;
  resultingKeyId: string | null;
  createdAt: Instant;
  /** The oldest person customer with a member row for the email, which approval would use (never an organization). */
  personCustomer: CustomerRef | null;
}

/** GET /api/v1/requests query. */
export interface RequestListQuery {
  /** Default: every status, open first. */
  status?: RequestStatus;
}

/** POST /api/v1/requests/:id/approve. Defaults: the person customer (created when missing), the request's name, today + PERSONAL_TERM_DAYS. */
export interface ApproveBody {
  /** An existing person customer (never an organization). */
  customerId?: string;
  licensee?: string;
  expiresOn?: ExpiresOn;
}

export interface ApproveResult {
  request: LicenseRequest;
  license: LicenseDetail;
}

/** POST /api/v1/requests/:id/reject: the note is mailed to the requester. */
export interface RejectBody {
  note: string;
}

export interface RejectResult {
  request: LicenseRequest;
}

/** An address whose personal requests always wait for review. */
export interface BlockedEmail {
  email: string;
  reason: string;
  blockedBy: ActorRef;
  blockedAt: Instant;
}

// --- users (admin) -----------------------------------------------------------

/** A person who signed in at least once. */
export interface User {
  id: string;
  name: string;
  email: string | null;
  emailVerified: boolean;
  issuer: string;
  subject: string;
  /** Mapped from IdP groups at each sign-in; read-only in the UI. */
  idpRoles: Role[];
  /** Granted in the portal (users.write). */
  grantedRoles: Role[];
  /** idpRoles ∪ grantedRoles; empty while disabled. */
  roles: Role[];
  disabled: boolean;
  disabledAt: Instant | null;
  createdAt: Instant;
  lastLoginAt: Instant | null;
}

/** GET /api/v1/users/:id. Recent audit comes from GET /api/v1/audit?targetType=user&targetId=. */
export interface UserDetail extends User {
  memberships: { customer: CustomerRef; email: string; linkedAt: Instant | null }[];
  /** Unexpired sessions. */
  activeSessions: number;
}

/** GET /api/v1/users query. */
export interface UserListQuery {
  /** Matches the name and the email. */
  q?: string;
}

/**
 * PATCH /api/v1/users/:id. Guards: nobody removes their own superadmin
 * grant or disables themselves; the last enabled superadmin keeps it. Role
 * and disable changes end the user's sessions (except the acting one).
 */
export interface UpdateUserBody {
  grantedRoles?: Role[];
  disabled?: boolean;
}

export interface RevokeSessionsResult {
  revoked: number;
}

// --- audit -------------------------------------------------------------------

/** What an audit event records. Streams add actions here (additive changes only). */
export type AuditAction =
  | 'auth.login'
  | 'auth.logout'
  | 'user.roles_changed'
  | 'user.disabled'
  | 'user.enabled'
  | 'user.sessions_revoked'
  | 'customer.created'
  | 'customer.updated'
  | 'member.added'
  | 'member.reinvited'
  | 'member.removed'
  | 'member.linked'
  | 'license.issued'
  | 'license.renewed'
  | 'license.changed'
  | 'license.moved'
  | 'license.withdrawn'
  | 'license.imported'
  | 'license.key_emailed'
  | 'license.key_downloaded'
  | 'request.created'
  | 'request.approved'
  | 'request.rejected'
  | 'request.canceled'
  | 'public_request.received'
  | 'public_request.confirmed'
  | 'blocklist.added'
  | 'blocklist.removed';

/** The kind of record an event is about. Member events target their customer (details.email names the member). */
export type AuditTargetType = 'user' | 'customer' | 'license' | 'request' | 'public_request' | 'blocked_email';

/** One audit event. Details never contain a key token. */
export interface AuditEvent {
  /** Sequence number as a string (bigserial); the `before` cursor. */
  id: string;
  at: Instant;
  actor: ActorRef;
  action: AuditAction;
  targetType: AuditTargetType;
  targetId: string;
  details: JsonObject;
  ip: string | null;
  userAgent: string | null;
  requestId: string | null;
}

/** GET /api/v1/audit query (newest first). */
export interface AuditQuery {
  action?: AuditAction;
  targetType?: AuditTargetType;
  targetId?: string;
  actorId?: string;
  /** Events older than this event id (KeysetPage.next). */
  before?: string;
  limit?: number;
}

export type AuditPage = KeysetPage<AuditEvent>;

// --- system ------------------------------------------------------------------

/** GET /api/v1/system: the read-only System page (no secrets). */
export interface SystemInfo {
  devMode: boolean;
  signing: {
    available: boolean;
    kid: string | null;
    /** The kid's .pub is in the trusted dir and matches the key. */
    trusted: boolean;
    trustedSince: string | null;
    testKey: boolean;
    /** Why signing is unavailable. */
    unavailableReason: string | null;
    /** The kids of LICENSE_TRUSTED_KEYS_DIR. */
    trustedKids: string[];
    /** Keys signed per kid (license_keys). */
    keysByKid: { kid: string; count: number }[];
  };
  signIn: {
    issuer: string;
    clientId: string;
    scopes: string[];
    displayName: string;
    groupsClaim: string;
    idTokenAlg: string | null;
    roleMappings: Record<Role, string[]>;
    /** OIDC_ADMIN_CLAIM_RULES. */
    adminClaimRules: { claim: string; value: string }[];
    defaultRole: 'customer' | 'none';
    memberAutoLink: boolean;
  };
  sessions: { idleTimeoutSeconds: number; lifetimeSeconds: number };
  mail: { configured: boolean; from: string; replyTo: string; adminNotify: string | null };
  publicForm: {
    enabled: boolean;
    origins: string[];
    limitPerIpPerHour: number;
    limitPerEmailPerDay: number;
    personalAutoApprove: boolean;
    personalAutoApproveMaxPerDay: number;
  };
  issuing: { maxPerActorPerDay: number };
  policy: Policy;
  build: { version: string; commit: string; builtAt: string | null };
}

// --- route ↔ type table --------------------------------------------------------

/** No body or no query. */
type None = undefined;

/** One route's wire types: query parameters, JSON body and the success response (void: 204; string: a license.key download; never: a redirect). */
interface Wire<Q, B, R> {
  query: Q;
  body: B;
  response: R;
}

/**
 * The query, body and response of every route of shared/routes.ts, by
 * RouteId. The UI's client and the server's handlers type against it.
 */
export type RouteTypes = {
  healthz: Wire<None, None, Healthz>;
  readyz: Wire<None, None, Readyz>;
  authConfig: Wire<None, None, AuthConfig>;
  authLogin: Wire<LoginQuery, None, never>;
  authCallback: Wire<Record<string, string>, None, never>;
  publicKeyRequest: Wire<None, PublicKeyRequestBody, PublicKeyRequestAccepted>;
  publicKeyRequestVerify: Wire<None, VerifyBody, VerifyResult>;
  authMe: Wire<None, None, Me>;
  authLogout: Wire<None, None, void>;
  catalog: Wire<None, None, Catalog>;
  myLicenses: Wire<None, None, Items<MyLicense>>;
  myLicense: Wire<None, None, MyLicenseDetail>;
  myLicenseKey: Wire<None, None, string>;
  myRequests: Wire<None, None, Items<MyRequest>>;
  myRequestCreate: Wire<None, PersonalRequestBody, PersonalRequestResult>;
  myLicenseRenew: Wire<None, None, MyLicenseDetail>;
  myInvitePreview: Wire<None, RedeemInviteBody, InvitePreview>;
  myInviteRedeem: Wire<None, RedeemInviteBody, RedeemInviteResult>;
  customersList: Wire<CustomerListQuery, None, Page<Customer>>;
  customerCreate: Wire<None, CreateCustomerBody, CustomerDetail>;
  customerGet: Wire<None, None, CustomerDetail>;
  customerUpdate: Wire<None, UpdateCustomerBody, CustomerDetail>;
  memberAdd: Wire<None, AddMemberBody, MemberInviteResult>;
  memberInvite: Wire<None, None, MemberInviteResult>;
  memberRemove: Wire<None, None, void>;
  licensesList: Wire<LicenseListQuery, None, Page<LicenseSummary>>;
  licenseIssue: Wire<None, IssueLicenseBody, LicenseDetail>;
  licenseImport: Wire<None, ImportBody, LicenseDetail>;
  licenseGet: Wire<None, None, LicenseDetail>;
  licenseKeyDownload: Wire<None, None, string>;
  licenseRenew: Wire<None, RenewBody, LicenseDetail>;
  licenseChange: Wire<None, ChangeBody, LicenseDetail>;
  licenseWithdraw: Wire<None, WithdrawBody, LicenseDetail>;
  licenseEmail: Wire<None, EmailKeyBody, EmailKeyResult>;
  requestsList: Wire<RequestListQuery, None, Items<LicenseRequest>>;
  requestApprove: Wire<None, ApproveBody, ApproveResult>;
  requestReject: Wire<None, RejectBody, RejectResult>;
  blockedEmailsList: Wire<None, None, Items<BlockedEmail>>;
  blockedEmailRemove: Wire<None, None, void>;
  usersList: Wire<UserListQuery, None, Items<User>>;
  userGet: Wire<None, None, UserDetail>;
  userUpdate: Wire<None, UpdateUserBody, UserDetail>;
  userRevokeSessions: Wire<None, None, RevokeSessionsResult>;
  auditList: Wire<AuditQuery, None, AuditPage>;
  system: Wire<None, None, SystemInfo>;
};

/** The query parameters of a route. */
export type QueryOf<Id extends RouteId> = RouteTypes[Id]['query'];
/** The JSON body of a route (undefined: none). */
export type BodyOf<Id extends RouteId> = RouteTypes[Id]['body'];
/** The success response of a route. */
export type ResponseOf<Id extends RouteId> = RouteTypes[Id]['response'];

// Compile-time check: RouteTypes covers exactly the routes of shared/routes.ts.
type MissingRoutes = Exclude<RouteId, keyof RouteTypes>;
type ExtraRoutes = Exclude<keyof RouteTypes, RouteId>;
export const ROUTE_TYPES_COMPLETE: [MissingRoutes, ExtraRoutes] extends [never, never] ? true : never = true;
