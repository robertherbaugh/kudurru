// The portal's mails (design spec §6): plain text, British spelling, the
// product named "kudurru license portal". One function per mail returns the
// MailMessage; callers send it after their transaction commits (or, for the
// confirmation link, before answering). Key mails carry the key in the body
// and as a license.key attachment (ASCII, trailing newline, no BOM), the
// install hints of catalog KEY_SOURCES with the restart note for the flag
// and the environment variable, the non-commercial notice on personal keys,
// KEY_IS_NOT_A_LICENCE and, for keys from the kudurru.io form, an invite
// link to the portal.

import type { ReviewReason } from '../../shared/api';
import {
  ALL_FEATURES_LABEL,
  CONFIRM_TTL_HOURS,
  featureLabel,
  INVITE_TTL_DAYS,
  KEY_IS_NOT_A_LICENCE,
  KEY_SOURCES,
  NONCOMMERCIAL_LICENSE_URL,
  OFFLINE_NOTE,
  planLabel,
} from '../../shared/catalog';
import type { MailAttachment, MailMessage } from './types';

/** The product name every mail signs with. */
export const PRODUCT_NAME = 'kudurru license portal';

/** Text for a header line: control characters (CR, LF, …) become spaces. */
function oneLine(s: string): string {
  let out = '';
  for (const ch of s) {
    const c = ch.charCodeAt(0);
    out += c <= 0x1f || (c >= 0x7f && c <= 0x9f) ? ' ' : ch;
  }
  return out.trim().slice(0, 200);
}

function signature(publicUrl: string): string {
  return `${PRODUCT_NAME}\n${publicUrl}`;
}

function paragraphs(...parts: (string | null | false | undefined)[]): string {
  return parts.filter((p): p is string => typeof p === 'string' && p !== '').join('\n\n') + '\n';
}

/** The license.key attachment: the token and a newline, ASCII, no BOM. */
export function licenseKeyAttachment(token: string): MailAttachment {
  if (!/^[\x21-\x7e]+$/.test(token)) throw new Error('a license key token is printable ASCII');
  return { filename: 'license.key', content: token + '\n', contentType: 'text/plain; charset=us-ascii' };
}

// --- the public form ------------------------------------------------------------

export interface ConfirmationMail {
  to: string;
  name: string;
  /** ${PUBLIC_URL}/verify?token=<token> */
  url: string;
  publicUrl: string;
}

/** The confirmation link of the kudurru.io form. */
export function confirmationMail(m: ConfirmationMail): MailMessage {
  return {
    to: m.to,
    subject: 'Confirm your request for a free kudurru personal key',
    text: paragraphs(
      `Hello ${m.name},`,
      'Someone, hopefully you, asked on kudurru.io for a free personal kudurru license key for this address. Confirm the request and the key is sent to this address:',
      m.url,
      `The link works for ${CONFIRM_TTL_HOURS} hours and only once. If you did not ask for a key, ignore this message: nothing happens without the confirmation.`,
      'A free personal key is for non-commercial use only.',
      signature(m.publicUrl),
    ),
  };
}

// --- keys ------------------------------------------------------------------------

/** issued: a new key; renewed: a new key with a later expiry; resent: the current key again. */
export type KeyMailKind = 'issued' | 'renewed' | 'resent';

export interface KeyMail {
  to: string;
  kind: KeyMailKind;
  license: {
    id: string;
    licensee: string;
    plan: string;
    nodes: number;
    /** null: every feature, including ones added later. */
    features: readonly string[] | null;
    /** The current key's expiry ('YYYY-MM-DD', UTC). */
    expiresOn: string;
  };
  /** The compact JWS. */
  token: string;
  /** ${PUBLIC_URL}/invites/<token> for keys from the kudurru.io form whose person has no portal access yet. */
  inviteUrl?: string | null;
  publicUrl: string;
}

const KEY_SUBJECTS: Record<KeyMailKind, string> = {
  issued: 'Your kudurru license key',
  renewed: 'Your renewed kudurru license key',
  resent: 'Your kudurru license key, sent again',
};

const KEY_OPENINGS: Record<KeyMailKind, string> = {
  issued: 'Here is your kudurru license key.',
  renewed: 'Your kudurru license key has been renewed. Here is the new key, valid until the date below. The previous key keeps working until it expires, so replace it before then.',
  resent: 'Here is your current kudurru license key again.',
};

/** The install hints (spec §6): where a node reads its key, and which sources need a restart. */
export function installHints(): string {
  return [
    'Add the key to kudurru. A node reads its key from the first of these that is set:',
    '',
    ...KEY_SOURCES.map((s) => `  ${s}`),
    '',
    'A key file (KUDURRU_LICENSE_KEY_FILE or license.key in the data directory) is read again within a minute. The --license-key flag and the KUDURRU_LICENSE_KEY variable are read only when the node starts: restart it after changing them (for example kubectl rollout restart or docker compose up -d). Then check that `kudurru license status` shows the new expiry date.',
  ].join('\n');
}

/** A key: issued, renewed or sent again. */
export function keyMail(m: KeyMail): MailMessage {
  const l = m.license;
  const features = l.features === null ? ALL_FEATURES_LABEL : l.features.map(featureLabel).join(', ');
  const facts = [
    `License ID:  ${l.id}`,
    `Licensee:    ${l.licensee}`,
    `Plan:        ${planLabel(l.plan)}`,
    `Nodes:       ${l.nodes}`,
    `Features:    ${features}`,
    `Expires:     ${l.expiresOn} (UTC)`,
  ].join('\n');
  const portal = m.inviteUrl
    ? `See your licenses and download the key again in the ${PRODUCT_NAME}. This link gives you access; it works for ${INVITE_TTL_DAYS} days:\n${m.inviteUrl}`
    : `Your licenses are in the ${PRODUCT_NAME}: ${m.publicUrl}/my/licenses`;
  return {
    to: m.to,
    subject: KEY_SUBJECTS[m.kind],
    text: paragraphs(
      `Hello ${l.licensee},`,
      KEY_OPENINGS[m.kind],
      facts,
      'The key is attached as license.key and is also here:',
      m.token,
      installHints(),
      l.plan === 'personal' && `This is a free personal key. It is for personal, non-commercial use only, under the PolyForm Noncommercial License 1.0.0 (${NONCOMMERCIAL_LICENSE_URL}).`,
      KEY_IS_NOT_A_LICENCE,
      OFFLINE_NOTE,
      portal,
      signature(m.publicUrl),
    ),
    attachments: [licenseKeyAttachment(m.token)],
  };
}

// --- requests that wait for review ------------------------------------------------

export interface ReviewWaitingMail {
  to: string;
  name: string;
  publicUrl: string;
}

/** To the requester: the request waits for a person. */
export function reviewWaitingMail(m: ReviewWaitingMail): MailMessage {
  return {
    to: m.to,
    subject: 'Your request for a kudurru personal key is waiting for review',
    text: paragraphs(
      `Hello ${m.name},`,
      'Thank you for your request for a free personal kudurru license key. A person needs to look at it before a key is issued, and you will hear from us by email. There is nothing else you need to do.',
      signature(m.publicUrl),
    ),
  };
}

const REVIEW_REASONS: Record<ReviewReason, string> = {
  auto_approve_off: 'Automatic approval of personal keys is off (PERSONAL_AUTO_APPROVE).',
  daily_cap: "Today's number of automatic personal keys is reached (PERSONAL_AUTO_APPROVE_MAX_PER_DAY).",
  withdrawn_history: 'A license of this person was withdrawn before.',
  blocked: 'The address is on the blocklist.',
};

/** The sentence an admin reads for a review reason. */
export function reviewReasonText(reason: ReviewReason): string {
  return REVIEW_REASONS[reason];
}

export interface ReviewNeededMail {
  /** ADMIN_NOTIFY_EMAIL. */
  to: string;
  request: { id: string; name: string; email: string; use: string; reason: ReviewReason; source: 'web' | 'portal' };
  publicUrl: string;
}

/** To ADMIN_NOTIFY_EMAIL: a personal request waits for review. */
export function reviewNeededMail(m: ReviewNeededMail): MailMessage {
  const r = m.request;
  return {
    to: m.to,
    subject: `Personal key request for review: ${oneLine(r.email)}`,
    text: paragraphs(
      'A request for a free personal key waits for review.',
      [
        `Request:  ${r.id}`,
        `Name:     ${r.name}`,
        `Email:    ${r.email}`,
        `From:     ${r.source === 'web' ? 'the kudurru.io form' : `the ${PRODUCT_NAME}`}`,
        `Why:      ${reviewReasonText(r.reason)}`,
      ].join('\n'),
      `What they run kudurru for:\n${r.use}`,
      `Approve or reject it: ${m.publicUrl}/requests`,
      signature(m.publicUrl),
    ),
  };
}

export interface RejectedMail {
  to: string;
  name: string;
  /** The admin's note. */
  note: string;
  publicUrl: string;
}

/** To the requester: the request was rejected, with the admin's note. */
export function rejectedMail(m: RejectedMail): MailMessage {
  return {
    to: m.to,
    subject: 'Your request for a kudurru personal key',
    text: paragraphs(
      `Hello ${m.name},`,
      'We looked at your request for a free personal kudurru license key and cannot issue one.',
      m.note,
      'If you have questions, reply to this message.',
      signature(m.publicUrl),
    ),
  };
}

// --- members -----------------------------------------------------------------------

export interface MemberInviteMail {
  to: string;
  customerName: string;
  /** ${PUBLIC_URL}/invites/<token>, or ${PUBLIC_URL}/my/licenses when the address was linked at once. */
  url: string;
  /** When the link stops working; null when there is no link to accept. */
  expiresAt: Date | null;
  publicUrl: string;
}

/** To a new member: access to a customer's licenses. */
export function memberInviteMail(m: MemberInviteMail): MailMessage {
  return {
    to: m.to,
    subject: `Access to the kudurru licenses of ${oneLine(m.customerName)}`,
    text: paragraphs(
      'Hello,',
      m.expiresAt
        ? `You have been given access to the kudurru licenses of ${m.customerName} in the ${PRODUCT_NAME}. Sign in and accept the invite to see the licenses and download their keys:`
        : `You have been given access to the kudurru licenses of ${m.customerName} in the ${PRODUCT_NAME}. Sign in to see the licenses and download their keys:`,
      m.url,
      m.expiresAt && `The link works until ${m.expiresAt.toISOString().slice(0, 16).replace('T', ' ')} (UTC). Anyone who signs in with it gets access, so do not forward it.`,
      KEY_IS_NOT_A_LICENCE,
      signature(m.publicUrl),
    ),
  };
}
