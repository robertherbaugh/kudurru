// Shared rules of the IAM write dialogs on the Access page's Identity tab
// (D294, D396): how an answer is classified, what a draft may hold, and the
// limits an operator meets.
import { ApiError } from '../api/client';
import { outcomeUnknown } from './cluster';

/**
 * 503 keys the IAM commands give before anything ran: the follower and
 * cluster refusals, a static host (read_only), the Kubernetes driver
 * (user_management_unavailable, iam_management_unavailable) and the
 * RoleBinding route without the SQLite runtime (not_implemented).
 */
const IAM_REFUSED_503 = new Set(['primary_required', 'cluster_fenced', 'cluster_conflict', 'generation_pending', 'forbidden', 'csrf', 'read_only', 'not_implemented', 'user_management_unavailable', 'iam_management_unavailable', 'factor_management_unavailable']);

/**
 * Whether the answer to one IAM command leaves its outcome unknown. The
 * SQLite runtime answers every error it does not classify, including a
 * commit it attempted, with a bare 503 unavailable (servingHTTPAuthError in
 * cmd/kudurru); the security-record limit answers the same, without a
 * change. So a bare 503 unavailable, a 500, a 502/504 or no answer is
 * unknown: the dialog never sends the command again and asks for a fresh
 * read. A 4xx (409 changed, 404 gone, 403 refused, 400 invalid) is a refusal.
 */
export function iamOutcomeUnknown(err: unknown): boolean {
  if (err instanceof ApiError && err.status === 503) return !IAM_REFUSED_503.has(err.key);
  return outcomeUnknown(err);
}

/** The sentence a dialog shows for a refusal it can explain better than the server's message. */
export function iamRefusalText(err: unknown, what: string): string | undefined {
  if (!(err instanceof ApiError)) return undefined;
  if (err.status === 409 && err.key === 'managed_by') return `The ${what} belongs to another manager (kudurru.yaml, objects.d or a migration). Change it where it is declared.`;
  if (err.status === 409 && err.key === 'disabled') return `The ${what} is disabled. Enable it before issuing a token.`;
  if (err.status === 409) return `The ${what} changed or already exists since you reviewed it. Close this dialog, read the list again and decide again; nothing was sent twice.`;
  if (err.status === 404) return `The ${what} no longer exists. Close this dialog and read the list again.`;
  if (err.status === 403 && err.key !== 'csrf') return `The server refused the change: ${err.message}. The built-in admin, accounts of the configuration file and accounts with bootstrap authority are not changed here.`;
  if (err.key === 'user_management_unavailable' || err.key === 'iam_management_unavailable') return `This server does not change ${what}s through the API (the Kubernetes driver): use kubectl or your GitOps tool.${typeof err.hint === 'string' ? ` ${err.hint}` : ''}`;
  return undefined;
}

/** The unknown-outcome sentence, with the security-record limit that answers the same way. */
export function iamUnknownText(what: string): string {
  return `The dashboard cannot tell whether the ${what} committed and does not send it again. Close this dialog and read the list again before deciding. If the cluster has reached its limit of revocation records, the server answers the same way and nothing changed.`;
}

/**
 * The permanent revocation records (security floors) of D396: an account
 * delete, the first disable, group change or password reset of an account, and
 * a change or delete of an identity provider keep one, against the 4,096-row
 * security section shared with sessions, TOTP records, external identities and
 * browser attempts (servingauth.MaxRows). Nothing prunes them.
 */
export const FLOOR_LIMIT_TEXT =
  'kudurru keeps a small permanent revocation record for each account or provider whose sessions and tokens a change ends, and for each delete. With sessions and other sign-in records they share a limit of about 4,000 per cluster, and nothing removes them; at the limit, deletes, disables and new sign-ins answer 503 and change nothing. Prefer disabling a long-lived account to creating and deleting one per job.';

const DNS_LABEL = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;
const DNS_SUBDOMAIN = /^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$/;

/** A local account or service account name: a DNS subdomain. */
export function subdomainError(v: string): string | null {
  if (!v) return 'Enter a name';
  return v.length <= 253 && DNS_SUBDOMAIN.test(v) ? null : 'Lower-case letters, digits, "-" and ".", starting and ending with a letter or digit';
}

/** A namespace or identity provider id: a DNS label. */
export function labelError(v: string, what = 'name'): string | null {
  if (!v) return `Enter a ${what}`;
  return v.length <= 63 && DNS_LABEL.test(v) ? null : 'Lower-case letters, digits and "-", at most 63';
}

/** A local group (validLocalGroup in cmd/kudurru): no colon, no control character, no surrounding space. */
export function localGroupError(g: string): string | null {
  if (!g) return 'Enter a group';
  // eslint-disable-next-line no-control-regex
  if (g.length > 253 || g.includes(':') || /[\u0000-\u001f\u007f]/.test(g) || g.trim() !== g) return 'At most 253 characters, no colon, no leading or trailing space';
  return null;
}

/** The groups of a draft: errors per row and duplicates. */
export function groupsError(groups: string[]): string | null {
  if (groups.length > 64) return 'At most 64 groups';
  for (let i = 0; i < groups.length; i++) {
    const g = groups[i] as string;
    const e = localGroupError(g);
    if (e) return `${g || `Line ${i + 1}`}: ${e}`;
    if (groups.indexOf(g) !== i) return `${g} is listed twice`;
  }
  return null;
}

export const MIN_PASSWORD_LEN = 8;

/** A new password (8 characters or more, at most 1024 bytes) and its confirmation. */
export function passwordErrors(password: string, confirm: string): { password?: string; confirm?: string } {
  const out: { password?: string; confirm?: string } = {};
  if ([...password].length < MIN_PASSWORD_LEN) out.password = `At least ${MIN_PASSWORD_LEN} characters`;
  else if (new TextEncoder().encode(password).length > 1024) out.password = 'At most 1024 bytes';
  if (confirm !== password) out.confirm = 'Does not match the password';
  return out;
}

/** The server-owned manager annotation; a desired document must never carry it. */
export const MANAGED_BY = 'kudurru.io/managed-by';

/** The manager of a stored object ('' when none is recorded). */
export function managerOf(meta: { annotations?: Record<string, string> } | undefined): string {
  return meta?.annotations?.[MANAGED_BY] ?? '';
}

/** Annotations of a stored object without the server-owned manager, for a replace. */
export function operatorAnnotations(meta: { annotations?: Record<string, string> } | undefined): Record<string, string> | undefined {
  const a = { ...(meta?.annotations ?? {}) };
  delete a[MANAGED_BY];
  return Object.keys(a).length ? a : undefined;
}
