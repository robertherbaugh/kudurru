// What a kudurru license key can say, and how people read it: the plans and
// features of internal/license (license.go) and the labels of the dashboard
// (dashboard/src/lib/license.ts). The server validates against these lists and the
// UI words them; server/test/catalog.test.ts fails when they drift from the
// Go code.

/** A plan the nodes know. The list is closed: a plan a node does not know makes the key invalid there. */
export type Plan = 'personal' | 'business' | 'enterprise' | 'trial';

/** A licensed feature. The list is open on the node: unknown names are ignored. */
export type Feature = 'oidc' | 'edge' | 'integrations' | 'rbac' | 'audit' | 'longterm' | 'requests';

/** The iss claim of every key. */
export const ISSUER = 'kudurru';

/** Days an expired key keeps working on a node (license.Grace). */
export const GRACE_DAYS = 30;

/** license.Grace in seconds: a node accepts a key while now < exp + GRACE_SECONDS. */
export const GRACE_SECONDS = GRACE_DAYS * 86_400;

/** The node warns about expiry this many days ahead (web EXPIRY_WARNING_DAYS); the server's `expiring` state uses the same window. */
export const EXPIRY_WARNING_DAYS = 30;

// Policy. The portal's own rules for what it signs (the nodes know none of
// them; a key is checked for its shape and its dates only). They change by
// release, next to the public copy that promises them (website, LICENSING.md),
// never by configuration.

/** A personal key covers at most this many nodes. */
export const PERSONAL_MAX_NODES = 3;
/** The term of a personal key, and of every personal renewal, in days. */
export const PERSONAL_TERM_DAYS = 365;
/** A personal key can be renewed from this many days before it expires (and in grace or after). */
export const RENEW_WINDOW_DAYS = 30;
/** A trial key expires at most this many days after the day it is issued. */
export const TRIAL_MAX_DAYS = 60;
/** The term the issue wizard proposes for a business key, in days. */
export const BUSINESS_DEFAULT_TERM_DAYS = 365;
/** No key expires more than this many days after the day it is signed (ten years). */
export const MAX_TERM_DAYS = 3653;
/** The largest node limit a key may carry (licenses.nodes check). */
export const MAX_NODES = 100_000;
/** A confirmation link of the public form works this many hours. */
export const CONFIRM_TTL_HOURS = 24;
/** An invite link to a customer's licenses works this many days. */
export const INVITE_TTL_DAYS = 14;
/** A one-click approval link in an admin's mail works this many days (and once). */
export const APPROVAL_LINK_TTL_DAYS = 7;

/** Every policy constant in one object (GET /api/v1/catalog, the System page). */
export interface Policy {
  personalMaxNodes: number;
  personalTermDays: number;
  renewWindowDays: number;
  trialMaxDays: number;
  businessDefaultTermDays: number;
  maxTermDays: number;
  maxNodes: number;
  graceDays: number;
  expiryWarningDays: number;
  confirmTtlHours: number;
  inviteTtlDays: number;
  approvalLinkTtlDays: number;
}

export const POLICY: Readonly<Policy> = Object.freeze({
  personalMaxNodes: PERSONAL_MAX_NODES,
  personalTermDays: PERSONAL_TERM_DAYS,
  renewWindowDays: RENEW_WINDOW_DAYS,
  trialMaxDays: TRIAL_MAX_DAYS,
  businessDefaultTermDays: BUSINESS_DEFAULT_TERM_DAYS,
  maxTermDays: MAX_TERM_DAYS,
  maxNodes: MAX_NODES,
  graceDays: GRACE_DAYS,
  expiryWarningDays: EXPIRY_WARNING_DAYS,
  confirmTtlHours: CONFIRM_TTL_HOURS,
  inviteTtlDays: INVITE_TTL_DAYS,
  approvalLinkTtlDays: APPROVAL_LINK_TTL_DAYS,
});

/** The license a personal key is certified for (the attestation's `license`, a license identifier). */
export const ATTESTATION_LICENSE = 'LicenseRef-Kudurru-Community';

/**
 * The certification a person ticks before a personal key is issued, word
 * for word as the kudurru.io form (website) and the portal show it. Stored in
 * every personal attestation (licenses.attestation, license_requests.attestation).
 */
export const ATTESTATION_STATEMENT =
  'I agree and certify this key will only be used for eligible personal, qualifying nonprofit or community-development use in accordance with the license (Kudurru Community License)';

/** Where the Kudurru Community License is published (website LICENSE_TEXT_PATH). */
export const NONCOMMERCIAL_LICENSE_URL = 'https://kudurru.io/license/text/';

/**
 * Where people write about a business license. The kudurru.io form sends
 * business requests to the portal (Requests, Business); this address stays
 * for the signed-in page's button and the form's fallback when the portal is
 * unavailable.
 */
export const BUSINESS_CONTACT = 'hello@kudurru.io';
export const BUSINESS_MAILTO = 'mailto:hello@kudurru.io?subject=Business%20License';

/** The editions the kudurru.io form offers (website COMMERCIAL_PLANS), by key. */
export interface BusinessEditionInfo {
  label: string;
  /** The plan a license for this edition is issued with. */
  plan: Extract<Plan, 'business' | 'enterprise'>;
}

/** Commercial Core is issued as plan business, Enterprise as plan enterprise. */
export const BUSINESS_EDITIONS = {
  core: { label: 'Commercial Core', plan: 'business' },
  enterprise: { label: 'Enterprise', plan: 'enterprise' },
} as const satisfies Record<string, BusinessEditionInfo>;

/** An edition of the kudurru.io form (BusinessRequest.edition). */
export type BusinessEdition = keyof typeof BUSINESS_EDITIONS;

/** The most nodes a business request may ask for (the kudurru.io form's limit). */
export const BUSINESS_REQUEST_MAX_NODES = 10_000;

/** The page that explains the license and how to get a key. */
export const LICENSE_PAGE_URL = 'https://kudurru.io/license';

/** Where the licensing rules for people are written down. */
export const LICENSING_URL = 'https://github.com/robertherbaugh/kudurru/blob/main/LICENSING.md';

export interface PlanInfo {
  key: Plan;
  label: string;
  /** One sentence for pickers. */
  description: string;
}

/** The plans, in the order pickers list them. Portal labels; plan identifiers match the dashboard. */
export const PLANS: readonly PlanInfo[] = [
  { key: 'personal', label: 'Personal / nonprofit (free)', description: 'Free key for eligible personal, homelab, nonprofit or community-development use under the Community License. Up to 3 nodes, every feature.' },
  { key: 'business', label: 'Business', description: 'A business key under a commercial agreement: nodes and features as agreed.' },
  { key: 'enterprise', label: 'Enterprise', description: 'Enterprise features authorized by an accepted commercial agreement and Order Form.' },
  { key: 'trial', label: 'Trial', description: 'A short evaluation key. Nodes keep it working for 30 days after it expires, like any key.' },
];

export interface FeatureInfo {
  key: Feature;
  label: string;
  /** What the key unlocks, in one sentence (verbatim from the dashboard). */
  unlocks: string;
}

/** Every licensed feature, in the order the dashboard lists them (not Go's Features() order). */
export const FEATURES: readonly FeatureInfo[] = [
  { key: 'rbac', label: 'Roles and access control', unlocks: 'Role bindings, roles scoped to zones and Kubernetes RBAC. Without it the built-in administrator is the only role.' },
  { key: 'oidc', label: 'Single sign-on', unlocks: 'Sign-in and API tokens through OpenID Connect identity providers.' },
  { key: 'audit', label: 'Audit trail', unlocks: 'The stored audit events, the Audit tab and the export. Audit log lines are always written.' },
  { key: 'longterm', label: 'Long-term statistics', unlocks: 'Statistics for periods older than 24 hours. They are collected either way.' },
  { key: 'requests', label: 'Unblock requests', unlocks: 'The request form on the block page and the Requests page. The block page itself is free.' },
  { key: 'edge', label: 'High-availability edge', unlocks: 'The dnsdist and keepalived supervisors and the floating address.' },
  { key: 'integrations', label: 'Integrations', unlocks: 'The external-dns webhook and other integrations.' },
];

/** The words for "no features list": the key grants every feature, including ones added later. */
export const ALL_FEATURES_LABEL = 'All features, including ones added later';

export function isPlan(v: unknown): v is Plan {
  return typeof v === 'string' && PLANS.some((p) => p.key === v);
}

export function isFeature(v: unknown): v is Feature {
  return typeof v === 'string' && FEATURES.some((f) => f.key === v);
}

export function planLabel(plan: string): string {
  return PLANS.find((p) => p.key === plan)?.label ?? plan;
}

export function featureLabel(feature: string): string {
  return FEATURES.find((f) => f.key === feature)?.label ?? feature;
}

/**
 * Where a license stands, as the server sees it. The first four mirror what
 * a node would report for the newest key; `withdrawn` is the server's own
 * record (a node keeps accepting the key until it expires plus grace).
 */
export type LicenseState = 'active' | 'expiring' | 'grace' | 'expired' | 'withdrawn';

export function stateLabel(state: LicenseState): string {
  switch (state) {
    case 'active':
      return 'Active';
    case 'expiring':
      return 'Expires soon';
    case 'grace':
      return 'Expired, in grace period';
    case 'expired':
      return 'Expired';
    case 'withdrawn':
      return 'Withdrawn';
  }
}

/** The tone of a state: only states that need action are loud. */
export function stateTone(state: LicenseState): 'neutral' | 'success' | 'warning' | 'error' {
  switch (state) {
    case 'active':
      return 'success';
    case 'expiring':
    case 'grace':
      return 'warning';
    case 'expired':
      return 'error';
    case 'withdrawn':
      return 'neutral';
  }
}

/** Unix seconds of an instant, as Go's time.Unix() (floor). */
function unixSeconds(t: Date | number): number {
  return typeof t === 'number' ? t : Math.floor(t.getTime() / 1000);
}

/**
 * The state of a license whose current key has the exp claim `exp` (Unix
 * seconds, or the instant as a Date), compared in whole seconds exactly as
 * a node does (internal/license/manager.go): active while now < exp, grace
 * while now < exp + GRACE_SECONDS, expired after that. `expiring` is active
 * within EXPIRY_WARNING_DAYS of exp; `withdrawn` is the portal's own record
 * (a node keeps accepting the key until exp + grace).
 */
export function licenseState(exp: Date | number, withdrawn: boolean, now: Date = new Date()): LicenseState {
  if (withdrawn) return 'withdrawn';
  const e = unixSeconds(exp);
  const n = unixSeconds(now);
  if (n < e - EXPIRY_WARNING_DAYS * 86_400) return 'active';
  if (n < e) return 'expiring';
  if (n < e + GRACE_SECONDS) return 'grace';
  return 'expired';
}

/** The first instant a key with the exp claim `exp` (Unix seconds, or a Date) no longer works on nodes: exp + GRACE_SECONDS. */
export function graceEnds(exp: Date | number): Date {
  return new Date((unixSeconds(exp) + GRACE_SECONDS) * 1000);
}

/** The words every page that hands out a key repeats (docs/guide/admin/13-license.md). */
export const KEY_IS_NOT_A_LICENSE =
  'A license key does not give you a license. The key records the license that you have. A free personal/nonprofit key is for eligible community use only.';

/** Keys are checked on the node; this service never sees a node. */
export const OFFLINE_NOTE = 'Keys are verified offline on each node. No node contacts this service.';

/** The four places a node reads its key from, first match wins (internal/license/sources.go). */
export const KEY_SOURCES: readonly string[] = [
  'kudurru serve --license-key <key>',
  'KUDURRU_LICENSE_KEY=<key>',
  'KUDURRU_LICENSE_KEY_FILE=/run/secrets/license_key',
  '<node.dataDir>/license.key',
];
