// The licensed features as the dashboard describes them, and what the
// license banner says. The server (internal/license) decides; this file only
// words it. docs/architecture/licensing.md.
import type { LicenseInfo } from '../api/types';

/** The edition's contact for free personal keys and commercial licences. */
export const LICENSE_CONTACT = 'hello@kudurru.io';

/** The page that explains the license and how to get a key. */
export const LICENSE_PAGE_URL = 'https://kudurru.io/license';

/** Where the licensing rules for people are written down (docs, from LICENSING.md). */
export const LICENSING_URL = 'https://docs.kudurru.io/project/licensing/';

/** The license text itself, on the project's website (also in the docs). */
export const LICENSE_TEXT_URL = 'https://kudurru.io/license/text/';

export interface FeatureInfo {
  /** The license.Feature value in GET /license `features`. */
  key: string;
  label: string;
  /** What the key unlocks, in one sentence. */
  unlocks: string;
}

/** Every licensed feature (internal/license Features), in the order the page lists them. */
export const FEATURES: FeatureInfo[] = [
  { key: 'rbac', label: 'Roles and access control', unlocks: 'Role bindings, roles scoped to zones and Kubernetes RBAC. Without it the built-in administrator is the only role.' },
  { key: 'oidc', label: 'Single sign-on', unlocks: 'Sign-in and API tokens through OpenID Connect identity providers.' },
  { key: 'audit', label: 'Audit trail', unlocks: 'The stored audit events, the Audit tab and the export. Audit log lines are always written.' },
  { key: 'longterm', label: 'Long-term statistics', unlocks: 'Statistics for periods older than 24 hours. They are collected either way.' },
  { key: 'requests', label: 'Unblock requests', unlocks: 'The request form on the block page and the Requests page. The block page itself is free.' },
  { key: 'edge', label: 'High-availability edge', unlocks: 'The dnsdist and keepalived supervisors and the floating address.' },
  { key: 'integrations', label: 'Integrations', unlocks: 'The external-dns webhook and other integrations.' },
];

export function featureLabel(key: string): string {
  return FEATURES.find((f) => f.key === key)?.label ?? key;
}

/** Plan names as people read them. */
export function planLabel(plan: string | undefined): string {
  switch (plan) {
    case 'personal':
      return 'Personal (free, non-commercial)';
    case 'business':
      return 'Business';
    case 'enterprise':
      return 'Enterprise';
    case 'trial':
      return 'Trial';
    default:
      return plan ?? '';
  }
}

/** How a state reads in a badge or a sentence. */
export function stateLabel(state: string): string {
  switch (state) {
    case 'community':
      return 'No key';
    case 'active':
      return 'Active';
    case 'grace':
      return 'Expired, in grace period';
    case 'expired':
      return 'Expired';
    case 'invalid':
      return 'Invalid key';
    default:
      return state;
  }
}

/** The tone of a state: only states that need action are loud. */
export function stateTone(state: string): 'neutral' | 'success' | 'warning' | 'error' {
  switch (state) {
    case 'active':
      return 'success';
    case 'grace':
      return 'warning';
    case 'expired':
    case 'invalid':
      return 'error';
    default:
      return 'neutral';
  }
}

/** Whole days from now until `iso`; negative when it has passed. */
export function daysUntil(iso: string | undefined, now = Date.now()): number | undefined {
  if (!iso) return undefined;
  const t = Date.parse(iso);
  return Number.isNaN(t) ? undefined : Math.floor((t - now) / 86_400_000);
}

/** The key expires within this many days: the banner starts to say so. */
export const EXPIRY_WARNING_DAYS = 30;

export interface LicenseNotice {
  status: 'info' | 'warning' | 'error';
  title: string;
  description: string;
}

/**
 * What the banner above every page says about the license, or null when
 * there is nothing to act on. A node without a key is not nagged (it is a
 * supported way to run); a key that is invalid, expired, in grace, about to
 * expire, or a cluster over its limit is.
 */
export function licenseNotice(l: LicenseInfo | undefined, now = Date.now()): LicenseNotice | null {
  if (!l) return null;
  if (l.overLimit) {
    return {
      status: 'warning',
      title: 'More nodes than the license allows',
      description: `The cluster has ${l.nodesInUse} nodes and the license allows ${l.nodes}. Configuration changes through the API are refused until it is back under the limit. DNS is not affected.`,
    };
  }
  switch (l.state) {
    case 'invalid':
      return {
        status: 'error',
        title: 'The license key is invalid',
        description: `${l.error ?? 'It could not be verified.'} This node runs with the free limits until a valid key is in place. DNS is not affected.`,
      };
    case 'expired':
      return {
        status: 'error',
        title: 'The license key has expired',
        description: 'This node runs with the free limits until the key is renewed. DNS is not affected.',
      };
    case 'grace': {
      const left = daysUntil(l.graceEndsAt, now);
      return {
        status: 'warning',
        title: 'The license key has expired',
        description: `It keeps working for ${left !== undefined && left >= 0 ? `${left} more ${left === 1 ? 'day' : 'days'}` : 'a short grace period'}. Renew it to keep the licensed features.`,
      };
    }
    case 'active': {
      const left = daysUntil(l.expiresAt, now);
      if (left !== undefined && left <= EXPIRY_WARNING_DAYS) {
        return {
          status: 'info',
          title: 'The license key expires soon',
          description: `It expires in ${Math.max(left, 0)} ${left === 1 ? 'day' : 'days'}. Renew it to keep the licensed features.`,
        };
      }
      return null;
    }
    default:
      return null;
  }
}

/** A sentence for a feature that needs a key; `what` names the thing. */
export function lockedSentence(what: string): string {
  return `${what} needs a license key. A free personal key covers up to 3 nodes for non-commercial use; businesses need an agreement.`;
}
