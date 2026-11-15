// The portal's issue policy (design spec §0.3-0.4, §6 "Admin license
// actions"): what it agrees to sign, shared by issue, renew, change, request
// approval and the personal flow so the rules cannot drift apart. The nodes
// also enforce their own plan-specific feature rules. This is the portal's
// shared policy for terms before signing:
//
// - licensee: the public form's name rules (1-200 UTF-16 code units after
//   Go's TrimSpace, not blank, no control characters, no lone surrogates);
// - nodes 1 to MAX_NODES; personal at most PERSONAL_MAX_NODES;
// - features null (all available to the plan, including later ones) or a
//   non-empty list of known, distinct features; personal always null;
//   explicit branding requires enterprise;
// - expiresOn tomorrow at the earliest and at most MAX_TERM_DAYS ahead;
//   personal at most PERSONAL_TERM_DAYS + RENEW_WINDOW_DAYS ahead; trial at
//   most TRIAL_MAX_DAYS ahead;
// - the resulting exp more than a day after now (so iat < exp always).

import type { Feature, Plan } from '../../shared/catalog';
import {
  isFeature,
  ISSUER,
  MAX_NODES,
  MAX_TERM_DAYS,
  PERSONAL_MAX_NODES,
  PERSONAL_TERM_DAYS,
  RENEW_WINDOW_DAYS,
  TRIAL_MAX_DAYS,
} from '../../shared/catalog';
import { addDays, DAY_SECONDS, expFor, maxExpiresOn, todayUtc, unixSeconds, type ExpiresOn } from '../../shared/dates';
import { goBlank, goTrimSpace, hasControlChars, hasLoneSurrogate, v, type Validator } from '../http/body';
import { HttpError } from '../http/errors';
import type { Claims } from './types';

/** What a key says, as the portal stores it (licenses row). */
export interface KeyTerms {
  licensee: string;
  plan: Plan;
  nodes: number;
  /** null: all features available to the plan, including ones added later. */
  features: readonly string[] | null;
  expiresOn: ExpiresOn;
}

/** The longest licensee (sub) in UTF-16 code units. */
export const MAX_LICENSEE_LENGTH = 200;

/** The licensee validator of request bodies: Go-trimmed, then the public form's name rules. */
export const licenseeField: Validator<string> = v.string({ min: 1, max: MAX_LICENSEE_LENGTH, trim: true });

/** Whether `s` (already trimmed) is a licensee the portal signs. */
export function isValidLicensee(s: string): boolean {
  return s === goTrimSpace(s) && s.length >= 1 && s.length <= MAX_LICENSEE_LENGTH && !goBlank(s) && !hasControlChars(s) && !hasLoneSurrogate(s);
}

/** One failed rule: the body field and a sentence for people. */
export interface PolicyIssue {
  field: 'licensee' | 'plan' | 'nodes' | 'features' | 'expiresOn';
  message: string;
}

/** The latest expiresOn the policy allows for `plan` on the day of `now`. */
export function latestExpiresOn(plan: Plan, now: Date): ExpiresOn {
  const today = todayUtc(now);
  const days = plan === 'personal' ? PERSONAL_TERM_DAYS + RENEW_WINDOW_DAYS : plan === 'trial' ? TRIAL_MAX_DAYS : MAX_TERM_DAYS;
  return addDays(today, Math.min(days, MAX_TERM_DAYS));
}

/** Every rule `terms` break for a key signed at `now` (empty: the portal signs it). */
export function policyIssues(terms: KeyTerms, now: Date): PolicyIssue[] {
  const issues: PolicyIssue[] = [];
  if (!isValidLicensee(terms.licensee)) issues.push({ field: 'licensee', message: 'The licensee must be 1 to 200 characters, not blank, without control characters.' });

  if (!Number.isSafeInteger(terms.nodes) || terms.nodes < 1 || terms.nodes > MAX_NODES) {
    issues.push({ field: 'nodes', message: `Nodes must be a whole number from 1 to ${MAX_NODES}.` });
  } else if (terms.plan === 'personal' && terms.nodes > PERSONAL_MAX_NODES) {
    issues.push({ field: 'nodes', message: `A personal key covers at most ${PERSONAL_MAX_NODES} nodes.` });
  }

  if (terms.features !== null) {
    if (terms.plan !== 'enterprise' && terms.features.includes('branding')) issues.push({ field: 'features', message: 'Custom branding requires the Enterprise plan.' });
    else if (terms.plan === 'personal') issues.push({ field: 'features', message: 'A personal key always grants all features available to Personal.' });
    else if (terms.features.length === 0) issues.push({ field: 'features', message: 'Choose at least one feature, or grant all features available to the plan.' });
    else if (!terms.features.every(isFeature)) {
      const unknown = terms.features.filter((f) => !isFeature(f));
      issues.push({ field: 'features', message: `This portal does not know the feature ${unknown.join(', ')}: choose from the known features.` });
    } else if (new Set(terms.features).size !== terms.features.length) issues.push({ field: 'features', message: 'A feature is listed twice.' });
  }

  const today = todayUtc(now);
  const tomorrow = addDays(today, 1);
  const latest = latestExpiresOn(terms.plan, now);
  if (terms.expiresOn < tomorrow) {
    issues.push({ field: 'expiresOn', message: 'The key must expire tomorrow (UTC) at the earliest.' });
  } else if (terms.expiresOn > latest) {
    const why =
      terms.plan === 'personal'
        ? `A personal key expires at most ${PERSONAL_TERM_DAYS + RENEW_WINDOW_DAYS} days after today`
        : terms.plan === 'trial'
          ? `A trial key expires at most ${TRIAL_MAX_DAYS} days after today`
          : `A key expires at most ${MAX_TERM_DAYS} days (ten years) after today`;
    issues.push({ field: 'expiresOn', message: `${why} (${latest} UTC).` });
  } else if (expFor(terms.expiresOn) <= unixSeconds(now) + DAY_SECONDS) {
    issues.push({ field: 'expiresOn', message: 'The key must stay valid for more than a day from now.' });
  }
  return issues;
}

/**
 * Throws 400 invalid with the failing fields unless the portal signs
 * `terms` at `now`. `prefix` names the body object the fields belong to.
 */
export function assertPolicy(terms: KeyTerms, now: Date, fieldOf: (field: PolicyIssue['field']) => string = (f) => f): void {
  const issues = policyIssues(terms, now);
  if (issues.length === 0) return;
  throw new HttpError(400, 'invalid', issues.map((i) => i.message).join(' '), { fields: [...new Set(issues.map((i) => fieldOf(i.field)))] });
}

/**
 * The claims of a key for `terms`, signed at `now` (spec §0.4: iat = now in
 * whole seconds, no nbf, exp = expFor(expiresOn), features omitted when
 * null). The current key of a license always equals this except iat and
 * kid (spec §4).
 */
export function claimsFor(licenseId: string, terms: KeyTerms, now: Date): Claims {
  const claims: Claims = {
    jti: licenseId,
    iss: ISSUER,
    sub: terms.licensee,
    plan: terms.plan,
    nodes: terms.nodes,
    iat: unixSeconds(now),
    exp: expFor(terms.expiresOn),
  };
  // Unknown names (imported keys) never reach here: policyIssues refuses
  // them and the signer refuses them again.
  if (terms.features !== null) claims.features = [...terms.features] as Feature[];
  return claims;
}

/** Whether the key with exp `exp` (Unix seconds) can be renewed by its person at `now`: within RENEW_WINDOW_DAYS of exp, in grace or expired. */
export function isRenewable(exp: number, now: Date): boolean {
  return unixSeconds(now) >= exp - RENEW_WINDOW_DAYS * DAY_SECONDS;
}

/** The expiry a personal renewal gives (spec §6): PERSONAL_TERM_DAYS after the later of today and the current expiry. */
export function personalRenewalExpiresOn(current: ExpiresOn, now: Date): ExpiresOn {
  return addDays(maxExpiresOn(todayUtc(now), current), PERSONAL_TERM_DAYS);
}

/**
 * The expiry a personal renewal gives at `now` for a key with these terms
 * and exp (Unix seconds), or null when it cannot be renewed by its person:
 * not a personal key, not within the renew window (nor in grace or
 * expired), or terms the personal policy refuses (a hand-signed import with
 * more nodes or a feature list): the portal never signs those on its own.
 */
export function personalRenewal(terms: KeyTerms, exp: number, now: Date): ExpiresOn | null {
  if (terms.plan !== 'personal' || !isRenewable(exp, now)) return null;
  const expiresOn = personalRenewalExpiresOn(terms.expiresOn, now);
  return policyIssues({ ...terms, expiresOn }, now).length === 0 ? expiresOn : null;
}

/** The expiry of a new personal key issued at `now`. */
export function personalExpiresOn(now: Date): ExpiresOn {
  return addDays(todayUtc(now), PERSONAL_TERM_DAYS);
}
