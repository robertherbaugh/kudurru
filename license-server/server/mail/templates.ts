// The portal's mails (design spec §6): branded HTML with a complete plain
// text alternative, American English, the
// product named "kudurru license portal". One function per mail returns the
// MailMessage; callers send it after their transaction commits (or, for the
// confirmation link, before answering). Key mails carry the key in the body
// and as a license.key attachment (ASCII, trailing newline, no BOM), the
// install hints of catalog KEY_SOURCES with the restart note for the flag
// and the environment variable, the non-commercial notice on personal keys,
// KEY_IS_NOT_A_LICENSE and, for keys from the kudurru.io form, an invite
// link to the portal. A personal request that waits for an admin mails the
// requester ("waiting for approval") and each approver: the details, the
// terms approval signs and, unless the request needs a signed-in admin
// (blocked, withdrawn history), a one-click approve link of their own. A
// business request from the form mails the requester ("we have your
// request") and each approver (the details and a link to the Requests page).

import type { BusinessEdition, RequestSource, ReviewReason } from '../../shared/api';
import {
  ALL_FEATURES_LABEL,
  APPROVAL_LINK_TTL_DAYS,
  BUSINESS_CONTACT,
  BUSINESS_EDITIONS,
  CONFIRM_TTL_HOURS,
  featureLabel,
  INVITE_TTL_DAYS,
  KEY_IS_NOT_A_LICENSE,
  KEY_SOURCES,
  NONCOMMERCIAL_LICENSE_URL,
  OFFLINE_NOTE,
  planLabel,
} from '../../shared/catalog';
import type { MailAttachment, MailMessage } from './types';
import { brandedMail, PRODUCT_NAME } from './html';

export { PRODUCT_NAME } from './html';

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

/** An instant as people read it in a mail: 2026-10-13 09:41 UTC. */
function utcMinute(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : `${d.toISOString().slice(0, 16).replace('T', ' ')} UTC`;
}

/** Where a request came from, as a mail names it. */
function sourceLabel(source: RequestSource): string {
  return source === 'web' ? 'the kudurru.io form' : `the ${PRODUCT_NAME}`;
}

/** The license.key attachment: the token and a newline, ASCII, no BOM. */
export function licenseKeyAttachment(token: string): MailAttachment {
  if (!/^[\x21-\x7e]+$/.test(token)) throw new Error('a license key token is printable ASCII');
  return { filename: 'license.key', content: token + '\n', contentType: 'text/plain; charset=us-ascii' };
}

// --- the public form ------------------------------------------------------------

export interface ConfirmationMail {
  to: string;
  /** ${PUBLIC_URL}/verify?token=<token> */
  url: string;
  publicUrl: string;
  /** Personal keys wait for an admin's approval (PERSONAL_AUTO_APPROVE=false). */
  approvalRequired: boolean;
}

/** The confirmation link of the kudurru.io form. */
export function confirmationMail(m: ConfirmationMail): MailMessage {
  return brandedMail({
    to: m.to,
    subject: 'Confirm your request for a free kudurru personal key',
    text: paragraphs(
      // A fixed greeting: the name comes from an anonymous, unconfirmed
      // form and is only used once the address is confirmed (the key mail).
      'Hello,',
      m.approvalRequired
        ? 'Someone, hopefully you, asked on kudurru.io for a free personal kudurru license key for this address. Confirm the request with this link; once an administrator approves it, the key is sent to this address:'
        : 'Someone, hopefully you, asked on kudurru.io for a free personal kudurru license key for this address. Confirm the request and the key is sent to this address:',
      m.url,
      `The link works for ${CONFIRM_TTL_HOURS} hours and only once. If you did not ask for a key, ignore this message: nothing happens without the confirmation.`,
      'A free personal/nonprofit key is for eligible community use only.',
      signature(m.publicUrl),
    ),
  }, {
    title: 'One step closer.',
    preview: 'Confirm your email to continue your request for a free personal kudurru key.',
    publicUrl: m.publicUrl,
    action: { label: 'Confirm your request', url: m.url },
  });
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
  /** The key answers a request an admin approved (issued or renewed): the mail says so. */
  approved?: boolean;
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

/** The opening of a key mail that answers an approved request. */
const APPROVED_OPENINGS: Record<Exclude<KeyMailKind, 'resent'>, string> = {
  issued: 'Your request for a free personal kudurru license key has been approved. Here is your key.',
  renewed:
    'Your request to renew your kudurru license key has been approved. Here is the new key, valid until the date below. The previous key keeps working until it expires, so replace it before then.',
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
  return brandedMail({
    to: m.to,
    subject: KEY_SUBJECTS[m.kind],
    text: paragraphs(
      `Hello ${l.licensee},`,
      m.approved && m.kind !== 'resent' ? APPROVED_OPENINGS[m.kind] : KEY_OPENINGS[m.kind],
      facts,
      'The key is attached as license.key and is also here:',
      m.token,
      installHints(),
      l.plan === 'personal' && `This is a free personal key. It is for eligible personal, qualifying nonprofit or community-development use only, under the Kudurru Community License (${NONCOMMERCIAL_LICENSE_URL}).`,
      KEY_IS_NOT_A_LICENSE,
      OFFLINE_NOTE,
      portal,
      signature(m.publicUrl),
    ),
    attachments: [licenseKeyAttachment(m.token)],
  }, {
    title: m.kind === 'renewed' ? 'Your renewed key is ready.' : m.kind === 'resent' ? 'Your key, once more.' : 'Your key is ready.',
    preview: `${planLabel(l.plan)} · ${l.nodes} nodes · valid until ${l.expiresOn} (UTC).`,
    publicUrl: m.publicUrl,
    key: m.token,
    details: [{
      text: facts,
      heading: 'Your license',
      rows: [['License ID', l.id], ['Licensee', l.licensee], ['Plan', planLabel(l.plan)], ['Nodes', String(l.nodes)], ['Features', features], ['Expires', `${l.expiresOn} (UTC)`]],
    }],
    action: { label: m.inviteUrl ? 'Access your licenses' : 'View your licenses', url: m.inviteUrl || `${m.publicUrl}/my/licenses` },
  });
}

// --- requests that wait for approval ----------------------------------------------

export interface ReviewWaitingMail {
  to: string;
  name: string;
  /** The request asks to renew a key (the portal's Renew), not for a new one. */
  renewal?: boolean;
  /** Every personal key waits for an admin (PERSONAL_AUTO_APPROVE off); else only this request does (blocked, withdrawn history, daily cap). */
  approvalRequired: boolean;
  publicUrl: string;
}

/** To the requester: the request waits for an admin's approval. */
export function reviewWaitingMail(m: ReviewWaitingMail): MailMessage {
  const who = m.approvalRequired ? 'An administrator approves every key before it is sent' : 'An administrator needs to approve this request before a key is sent';
  return brandedMail({
    to: m.to,
    subject: m.renewal ? 'Your renewal of a kudurru personal key is waiting for approval' : 'Your request for a kudurru personal key is waiting for approval',
    text: paragraphs(
      `Hello ${m.name},`,
      m.renewal
        ? `Thank you for renewing your free personal kudurru license key. ${who}; the renewed key arrives by email once your renewal is approved. Your current key keeps working until it expires. There is nothing else you need to do.`
        : `Thank you for your request for a free personal kudurru license key. ${who}; your key arrives by email once it is approved. There is nothing else you need to do.`,
      signature(m.publicUrl),
    ),
  }, {
    title: m.renewal ? 'Your renewal is in review.' : 'Your request is in review.',
    preview: 'We have your request. Your personal key arrives by email once it is approved.',
    publicUrl: m.publicUrl,
  });
}

const REVIEW_REASONS: Record<ReviewReason, string> = {
  auto_approve_off: "Every personal key needs an admin's approval (PERSONAL_AUTO_APPROVE is off).",
  daily_cap: "Today's number of automatic personal keys is reached (PERSONAL_AUTO_APPROVE_MAX_PER_DAY).",
  withdrawn_history: 'A license of this person was withdrawn before.',
  blocked: 'The address is on the blocklist.',
};

/** The sentence an admin reads for a review reason. */
export function reviewReasonText(reason: ReviewReason): string {
  return REVIEW_REASONS[reason];
}

export interface ReviewNeededMail {
  /** One approver (ADMIN_NOTIFY_EMAIL, or a license admin or superadmin). */
  to: string;
  request: {
    id: string;
    name: string;
    email: string;
    use: string;
    reason: ReviewReason;
    source: RequestSource;
    /** The non-commercial certification behind the request. */
    attestation: { statement: string | null; at: string; ip: string | null };
  };
  /** The person's active personal license that approving renews, or null for a new key. */
  renews: { licenseId: string; expiresOn: string } | null;
  /**
   * What approving signs: a personal key for this many nodes, every
   * feature, until this day (UTC) when approved by holdsUntil (null: when
   * approved today; approval later moves the expiry).
   */
  terms: { nodes: number; expiresOn: string; holdsUntil: string | null };
  /** ${PUBLIC_URL}/approve?token=<token> (this recipient's own link); null: a signed-in admin decides (blocked, withdrawn history). */
  approveUrl: string | null;
  publicUrl: string;
}

/** To one approver: a personal request waits for approval, with the details and (when allowed) a one-click approve link. */
export function reviewNeededMail(m: ReviewNeededMail): MailMessage {
  const r = m.request;
  const name = oneLine(r.name);
  const facts = [
    `Request:    ${r.id}`,
    `Name:       ${r.name}`,
    `Email:      ${r.email}`,
    `From:       ${sourceLabel(r.source)}`,
    `Why:        ${reviewReasonText(r.reason)}`,
    m.renews && `Renews:     ${m.renews.licenseId} (expires ${m.renews.expiresOn} UTC)`,
  ]
    .filter((line): line is string => typeof line === 'string')
    .join('\n');
  const certification = [
    'Their certification:',
    r.attestation.statement ? `"${r.attestation.statement}"` : '(no statement recorded)',
    `Given ${utcMinute(r.attestation.at)}${r.attestation.ip ? ` from ${r.attestation.ip}` : ''}.`,
  ].join('\n');
  const terms = [
    m.renews ? 'Approving renews the key with these terms:' : 'Approving signs this key:',
    `Plan:       ${planLabel('personal')}`,
    `Nodes:      ${m.terms.nodes}`,
    `Features:   ${ALL_FEATURES_LABEL}`,
    `Expires:    ${m.terms.expiresOn} (UTC) if approved ${m.terms.holdsUntil ? `by ${m.terms.holdsUntil}` : 'today'}`,
  ].join('\n');
  const decide = m.approveUrl
    ? [
        `Approve it and send the key with one click: the link opens a page with these details and the buttons "Approve and send the key" and "Reject". It works for ${APPROVAL_LINK_TTL_DAYS} days and once; anyone who has it can decide, so do not forward this message.`,
        m.approveUrl,
        `To change the terms first, use the portal: ${m.publicUrl}/requests`,
      ].join('\n')
    : ['This request needs a closer look (see Why above), so this mail has no approve link. A license admin decides it in the portal after signing in:', `${m.publicUrl}/requests`].join('\n');
  return brandedMail({
    to: m.to,
    subject: m.renews ? `Approve the renewal of ${m.renews.licenseId} for ${name}?` : `Approve a free personal key for ${name}?`,
    text: paragraphs(
      'Hello,',
      m.renews
        ? `${r.name} asks to renew their free personal kudurru license key. Nothing is signed until an admin approves it.`
        : `${r.name} asks for a free personal kudurru license key. Nothing is signed until an admin approves it.`,
      facts,
      `What they run kudurru for:\n${r.use}`,
      certification,
      terms,
      decide,
      signature(m.publicUrl),
    ),
  }, {
    title: m.renews ? 'A renewal needs your review.' : 'A request needs your review.',
    preview: `${name} is waiting for approval of a free personal kudurru key.`,
    publicUrl: m.publicUrl,
    details: [{
      text: facts,
      heading: 'The request',
      rows: [
        ['Request', r.id], ['Name', r.name], ['Email', r.email], ['From', sourceLabel(r.source)], ['Why', reviewReasonText(r.reason)],
        ...(m.renews ? [['Renews', `${m.renews.licenseId} (expires ${m.renews.expiresOn} UTC)`] as const] : []),
      ],
    }, {
      text: terms,
      heading: m.renews ? 'Approving renews the key with these terms' : 'Approving signs this key',
      rows: [['Plan', planLabel('personal')], ['Nodes', String(m.terms.nodes)], ['Features', ALL_FEATURES_LABEL], ['Expires', `${m.terms.expiresOn} (UTC) if approved ${m.terms.holdsUntil ? `by ${m.terms.holdsUntil}` : 'today'}`]],
    }],
    action: { label: m.approveUrl ? 'Review this request' : 'Open requests in the portal', url: m.approveUrl || `${m.publicUrl}/requests` },
  });
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
  return brandedMail({
    to: m.to,
    subject: 'Your request for a kudurru personal key',
    text: paragraphs(
      `Hello ${m.name},`,
      'We looked at your request for a free personal kudurru license key and cannot issue one.',
      m.note,
      'If you have questions, reply to this message.',
      signature(m.publicUrl),
    ),
  }, {
    title: 'An update on your request.',
    preview: 'We have reviewed your request for a personal kudurru key.',
    publicUrl: m.publicUrl,
  });
}

// --- business requests ----------------------------------------------------------

/** A business request as its mails describe it. */
export interface BusinessRequestFacts {
  id: string;
  name: string;
  email: string;
  company: string;
  edition: BusinessEdition;
  nodes: number;
  /** '' when the form left it empty. */
  details: string;
}

/**
 * The facts paragraph of a business request mail and its HTML rows. The
 * approver copy has every field and the request ID. The requester copy has
 * only the edition (a closed set) and the node count (a number): the address
 * is not confirmed, so mail to it never repeats text the submitter wrote,
 * which would let anyone send a third party their own words from the portal.
 */
function businessFacts(r: BusinessRequestFacts, audience: 'requester' | 'approver'): { text: string; rows: [string, string][] } {
  const rows: [string, string][] = [
    ...(audience === 'approver'
      ? [['Request', r.id], ['Name', r.name], ['Company', r.company], ['Email', r.email]] as [string, string][]
      : []),
    ['Edition', BUSINESS_EDITIONS[r.edition].label],
    ['Nodes', String(r.nodes)],
  ];
  return { text: rows.map(([k, v]) => `${`${k}:`.padEnd(12)}${v}`).join('\n'), rows };
}

export interface BusinessReceivedMail {
  to: string;
  request: BusinessRequestFacts;
  publicUrl: string;
}

/**
 * To the requester: we have the business request and reply by email. The
 * address is not confirmed (no link to follow), so the mail is fixed text
 * plus the edition and node count, and invites a reply, nothing more.
 */
export function businessReceivedMail(m: BusinessReceivedMail): MailMessage {
  const facts = businessFacts(m.request, 'requester');
  return brandedMail({
    to: m.to,
    subject: 'We have your kudurru business license request',
    text: paragraphs(
      // A fixed greeting, as for the personal form: the name is unconfirmed.
      'Hello,',
      'Thank you for your interest in a kudurru business license. We have your request and will reply to this address. There is nothing else you need to do.',
      facts.text,
      `If you did not send this request, ignore this message, or reply and tell us. Questions in the meantime go to ${BUSINESS_CONTACT}.`,
      signature(m.publicUrl),
    ),
  }, {
    title: 'Got it.',
    preview: 'We have your kudurru business license request and will reply by email.',
    publicUrl: m.publicUrl,
    details: [{ text: facts.text, heading: 'Your request', rows: facts.rows }],
  });
}

export interface BusinessRequestNeededMail {
  /** One approver (ADMIN_NOTIFY_EMAIL, or a license admin or superadmin). */
  to: string;
  request: BusinessRequestFacts;
  publicUrl: string;
}

/** To one approver: a new business request waits in the portal. No one-click link: an admin issues or closes it there. */
export function businessRequestNeededMail(m: BusinessRequestNeededMail): MailMessage {
  const r = m.request;
  const facts = businessFacts(r, 'approver');
  const url = `${m.publicUrl}/requests?tab=business`;
  return brandedMail({
    to: m.to,
    subject: `Business license request from ${oneLine(r.company)}`,
    text: paragraphs(
      'Hello,',
      `${r.name} of ${r.company} asks on the kudurru.io form about the ${BUSINESS_EDITIONS[r.edition].label} edition for ${r.nodes} ${r.nodes === 1 ? 'node' : 'nodes'}. They were told we reply by email; nothing is signed until a license admin issues a license.`,
      facts.text,
      r.details ? `Their details:\n${r.details}` : 'They gave no further details.',
      `Issue a license from the request, or close it with a note, on the Requests page (Business):\n${url}`,
      signature(m.publicUrl),
    ),
  }, {
    title: 'A business request came in.',
    preview: `${oneLine(r.company)} asks about the ${BUSINESS_EDITIONS[r.edition].label} edition.`,
    publicUrl: m.publicUrl,
    details: [{ text: facts.text, heading: 'The request', rows: facts.rows }],
    action: { label: 'Open business requests', url },
  });
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
  return brandedMail({
    to: m.to,
    subject: `Access to the kudurru licenses of ${oneLine(m.customerName)}`,
    text: paragraphs(
      'Hello,',
      m.expiresAt
        ? `You have been given access to the kudurru licenses of ${m.customerName} in the ${PRODUCT_NAME}. Sign in and accept the invite to see the licenses and download their keys:`
        : `You have been given access to the kudurru licenses of ${m.customerName} in the ${PRODUCT_NAME}. Sign in to see the licenses and download their keys:`,
      m.url,
      m.expiresAt && `The link works until ${m.expiresAt.toISOString().slice(0, 16).replace('T', ' ')} (UTC). Anyone who signs in with it gets access, so do not forward it.`,
      KEY_IS_NOT_A_LICENSE,
      signature(m.publicUrl),
    ),
  }, {
    title: 'Your licenses, all in one place.',
    preview: `You have been given access to the kudurru licenses of ${oneLine(m.customerName)}.`,
    publicUrl: m.publicUrl,
    action: { label: m.expiresAt ? 'Accept your invite' : 'View your licenses', url: m.url },
  });
}
