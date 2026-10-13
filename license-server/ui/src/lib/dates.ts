// How the portal writes dates (design spec §0.9, §7): every key date is a
// UTC calendar day labeled "(UTC)", so what the portal says matches what
// `kudurru license status` prints on a node, wherever the reader sits.
// utcDay is the one helper every page uses for a day; utcDateTime is for
// the moments of the audit trail and sign-ins. Nothing here reads the
// browser's time zone.
import { isExpiresOn } from '@shared/dates';
import type { Instant } from '@shared/api';

/** The UTC suffix every date carries. */
export const UTC = '(UTC)';

/** The 'YYYY-MM-DD' UTC day of an expiresOn ('YYYY-MM-DD') or an instant (RFC 3339), or null. */
export function utcDayOf(value: string | null | undefined): string | null {
  if (!value) return null;
  if (isExpiresOn(value)) return value;
  const t = Date.parse(value);
  return Number.isNaN(t) ? null : new Date(t).toISOString().slice(0, 10);
}

/** 'YYYY-MM-DD (UTC)' for a key expiry (expiresOn) or any instant; '–' when unknown. */
export function utcDay(value: string | null | undefined): string {
  const d = utcDayOf(value);
  return d ? `${d} ${UTC}` : '–';
}

/** 'YYYY-MM-DD HH:MM (UTC)' for an instant (audit events, sign-ins); '–' when unknown. */
export function utcDateTime(value: Instant | null | undefined): string {
  if (!value) return '–';
  const t = Date.parse(value);
  if (Number.isNaN(t)) return '–';
  const iso = new Date(t).toISOString();
  return `${iso.slice(0, 10)} ${iso.slice(11, 16)} ${UTC}`;
}
