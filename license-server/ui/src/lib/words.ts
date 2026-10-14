// How the portal words the wire's closed sets (shared/api.ts) that the
// catalog does not already label: customer kinds, sources, key reasons,
// request states and review reasons, audit actions and targets. Plans,
// features, license states and roles are labeled in shared/catalog.ts and
// shared/permissions.ts; this file only adds what those leave out, so a
// page never prints a raw enum value.
import type { AuditAction, AuditTargetType, CustomerKind, KeyReason, LicenseSource, MemberStatus, PersonalOutcome, RequestSource, RequestStatus, ReviewReason } from '@shared/api';
import { BUSINESS_CONTACT } from '@shared/catalog';

/** The product name everywhere (design spec: never "license server"). */
export const PRODUCT_NAME = 'kudurru license portal';

/** The dev-mode banner's words (spec §7). */
export const DEV_BANNER = 'Development mode: keys are signed with a test key no kudurru release trusts';

export const KIND_LABEL: Record<CustomerKind, string> = { person: 'Person', organization: 'Organization' };

/** The kinds as SelectField options, with what each means. */
export const KIND_OPTIONS: { value: CustomerKind; label: string; help: string }[] = [
  { value: 'organization', label: KIND_LABEL.organization, help: 'A company or another organization; any number of people can be members.' },
  { value: 'person', label: KIND_LABEL.person, help: 'One person, the kind personal keys go to.' },
];

export const SOURCE_LABEL: Record<LicenseSource, string> = {
  admin: 'Issued in the portal',
  web: 'kudurru.io form',
  portal: 'Requested in the portal',
  import: 'Imported key',
};

export const KEY_REASON_LABEL: Record<KeyReason, string> = { issue: 'Issued', renew: 'Renewed', change: 'Changed', import: 'Imported' };

export const MEMBER_STATUS_LABEL: Record<MemberStatus, string> = { invited: 'Invited', linked: 'Linked' };

export const REQUEST_STATUS_LABEL: Record<RequestStatus, string> = { open: 'Open', approved: 'Approved', rejected: 'Rejected', canceled: 'Canceled' };

export const REQUEST_SOURCE_LABEL: Record<RequestSource, string> = { web: 'kudurru.io form', portal: 'Portal' };

export const REVIEW_REASON_LABEL: Record<ReviewReason, string> = {
  auto_approve_off: 'Approval required',
  daily_cap: 'Daily automatic approvals used up',
  withdrawn_history: 'An earlier license was withdrawn',
  blocked: 'The email address is blocked',
};

/** The pending_review outcome, word for word (the /verify page, the portal's request and renewal dialogs). */
export const PENDING_TITLE = 'Your request is waiting for approval';
export const PENDING_BODY = 'Your key arrives by email once it is approved.';

/**
 * What a personal key request led to, as a title and a sentence: `portal`
 * for the portal's dialogs (the person is signed in), `web` for the /verify
 * page of the kudurru.io form, whose key mail carries the invite link that
 * adds the key to a portal account (signing in alone shows nothing while
 * MEMBER_AUTO_LINK is off). pending_review is the usual answer: a license
 * admin approves every personal key unless PERSONAL_AUTO_APPROVE is on.
 */
export function outcomeWords(outcome: PersonalOutcome, email: string | null, source: RequestSource): { title: string; body: string } {
  const to = email ? ` to ${email}` : '';
  switch (outcome) {
    case 'issued':
      return {
        title: 'Your free personal key is ready',
        body:
          source === 'portal'
            ? `We mailed the key${to}. It is also on your licenses page.`
            : `We mailed the key${to}. If you are not in the license portal yet, the mail also has a link that adds the key to your account there.`,
      };
    case 'renewed':
      return { title: 'Your personal key was renewed', body: `We mailed the renewed key${to}. Install it on your nodes before the old one expires.` };
    case 'resent':
      return { title: 'You already have a personal key', body: `It is not due for renewal yet, so we mailed the current key again${to}.` };
    case 'pending_review':
      return { title: PENDING_TITLE, body: PENDING_BODY };
  }
}

export const AUDIT_ACTIONS: readonly AuditAction[] = [
  'auth.login',
  'auth.logout',
  'user.roles_changed',
  'user.disabled',
  'user.enabled',
  'user.sessions_revoked',
  'customer.created',
  'customer.updated',
  'member.added',
  'member.reinvited',
  'member.removed',
  'member.linked',
  'license.issued',
  'license.renewed',
  'license.changed',
  'license.moved',
  'license.withdrawn',
  'license.imported',
  'license.key_emailed',
  'license.key_downloaded',
  'request.created',
  'request.approved',
  'request.rejected',
  'request.canceled',
  'public_request.received',
  'public_request.confirmed',
  'blocklist.added',
  'blocklist.removed',
];

export const TARGET_LABEL: Record<AuditTargetType, string> = {
  user: 'User',
  customer: 'Customer',
  license: 'License',
  request: 'Request',
  public_request: 'Public request',
  blocked_email: 'Blocked email',
};

/** The portal page of an audit target, when it has one. */
export function targetHref(type: AuditTargetType, id: string): string | null {
  switch (type) {
    case 'user':
      return `/users/${encodeURIComponent(id)}`;
    case 'customer':
      return `/customers/${encodeURIComponent(id)}`;
    case 'license':
      return `/licenses/${encodeURIComponent(id)}`;
    default:
      return null;
  }
}

/** The mailto for business changes and renewals of one license (spec §6: subject "kudurru commercial license <License ID>"). */
export function businessMailto(licenseId: string): string {
  return `mailto:${BUSINESS_CONTACT}?subject=${encodeURIComponent(`kudurru commercial license ${licenseId}`)}`;
}
