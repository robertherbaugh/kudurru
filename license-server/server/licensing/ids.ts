// License ids (design spec §4): a license id is also the jti of every key
// signed for it, in hack/licensegen's format, `lic_` plus base64url of 9
// random bytes (16 characters, e.g. lic_ejB_fnIEG90C). Imported keys keep
// the jti they were signed with, which can be any non-empty string, so the
// checks here are the database's (licenses.id: non-empty, at most 200
// characters), not the portal's own format.

import { randomBytes } from 'node:crypto';
import { ID_PREFIX } from '../../shared/api';

/** The longest jti the licenses table stores (licenses.id check). */
export const MAX_JTI_LENGTH = 200;

/** A new license id: lic_ + base64url(9 random bytes). */
export function newLicenseId(): string {
  return ID_PREFIX.license + randomBytes(9).toString('base64url');
}

/** Whether `s` has the shape of a license id the portal issues (lic_ + 12 base64url characters). */
export function isPortalLicenseId(s: unknown): s is string {
  return typeof s === 'string' && /^lic_[A-Za-z0-9_-]{12}$/.test(s);
}

/** Whether `s` can name a license in a path or a body: any jti the licenses table can hold. */
export function isLicenseId(s: unknown): s is string {
  return typeof s === 'string' && s.length > 0 && s.length <= MAX_JTI_LENGTH;
}
