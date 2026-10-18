// The term rules the issue wizard and the renew dialog apply to a key's
// expiry day (spec §6 issuing policy, shared/catalog.ts policy constants):
// tomorrow at the earliest, a personal key at most one term plus the renew
// window ahead, a trial at most TRIAL_MAX_DAYS, anything else at most
// MAX_TERM_DAYS; the presets count from a base day (today when issuing,
// the later of today and the current expiry when renewing).
import { BUSINESS_DEFAULT_TERM_DAYS, MAX_TERM_DAYS, PERSONAL_TERM_DAYS, RENEW_WINDOW_DAYS, TRIAL_MAX_DAYS, type Plan } from '@shared/catalog';
import { addDays, maxExpiresOn, type ExpiresOn } from '@shared/dates';
import type { DatePreset } from '../ui/fields/DateField';

/** The earliest and latest expiry a new key of this plan may carry, from today (UTC). */
export function termBounds(plan: Plan, today: ExpiresOn): { min: ExpiresOn; max: ExpiresOn } {
  const min = addDays(today, 1);
  if (plan === 'personal') return { min, max: addDays(today, PERSONAL_TERM_DAYS + RENEW_WINDOW_DAYS) };
  if (plan === 'trial') return { min, max: addDays(today, TRIAL_MAX_DAYS) };
  return { min, max: addDays(today, MAX_TERM_DAYS) };
}

/** The expiry the issue wizard proposes for a plan. */
export function defaultExpiry(plan: Plan, today: ExpiresOn): ExpiresOn {
  if (plan === 'personal') return addDays(today, PERSONAL_TERM_DAYS);
  if (plan === 'trial') return addDays(today, 30);
  return addDays(today, BUSINESS_DEFAULT_TERM_DAYS);
}

/** Term presets for a plan, counted from `base`, kept within `max`. */
export function termPresets(plan: Plan, base: ExpiresOn, max?: ExpiresOn): DatePreset[] {
  const presets: DatePreset[] =
    plan === 'personal'
      ? [{ label: '1 year', value: addDays(base, PERSONAL_TERM_DAYS) }]
      : plan === 'trial'
        ? [14, 30, TRIAL_MAX_DAYS].map((d) => ({ label: `${d} days`, value: addDays(base, d) }))
        : [1, 2, 3].map((y) => ({ label: y === 1 ? '1 year' : `${y} years`, value: addDays(base, 365 * y) }));
  return max ? presets.filter((p) => p.value <= max) : presets;
}

/** Where renewal presets count from (spec §0.9): the later of today and the current expiry. */
export function renewalBase(today: ExpiresOn, current: ExpiresOn): ExpiresOn {
  return maxExpiresOn(today, current);
}
