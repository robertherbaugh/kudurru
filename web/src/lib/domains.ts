// The Allowlist and Denylist (Pi-hole's Domains page, Technitium's Allowed
// and Blocked zones): entry types with their help and per-type validation
// mirroring the server's, origins, scopes and the page paths. Shared by
// the three pages (/allowlist, /denylist, /domains), the add and edit
// dialogs and the query-log actions.
import type { DomainAction, DomainEntry, DomainEntryType, DomainOrigin } from '../api/types';
import type { SelectOption } from '../components/fields/SelectField';

export const ACTION_LABEL: Record<DomainAction, string> = { allow: 'Allowlist', deny: 'Denylist' };

/** The focused page of an action. */
export function actionPath(action: DomainAction): string {
  return action === 'allow' ? '/allowlist' : '/denylist';
}

export const ACTION_OPTIONS: (SelectOption & { value: DomainAction })[] = [
  { value: 'allow', label: 'Allow', help: 'Never blocked, whatever the block lists of the policy say (allow wins).' },
  { value: 'deny', label: 'Deny', help: 'Blocked for the clients of the policies the entry applies to.' },
];

export const TYPE_OPTIONS: (SelectOption & { value: DomainEntryType })[] = [
  { value: 'exact', label: 'Exact domain', help: 'Only this name, e.g. ads.example.com (not its subdomains).' },
  { value: 'wildcard', label: 'Wildcard', help: '*.example.com matches example.com and every subdomain of it (also accepts ||example.com^ or example.com).' },
  { value: 'regex', label: 'Regular expression', help: 'An RE2 expression matched against the whole name, e.g. ^ads?[0-9]*\\. (with or without /…/).' },
];

export const TYPE_LABEL: Record<DomainEntryType, string> = { exact: 'exact', wildcard: 'wildcard', regex: 'regex' };

export const TYPE_PLACEHOLDER: Record<DomainEntryType, string> = {
  exact: 'ads.example.com\ntracker.example.org',
  wildcard: '*.example.com\n*.doubleclick.net',
  regex: '^ads?[0-9]*\\.\n(^|\\.)telemetry\\.',
};

export const ENABLED_OPTIONS: SelectOption[] = [
  { value: 'true', label: 'Enabled only' },
  { value: 'false', label: 'Disabled only' },
];

export const ORIGIN_LABEL: Record<DomainOrigin, string> = {
  manual: 'manual',
  querylog: 'query log',
  unblock: 'unblock request',
  api: 'API',
  migrated: 'migrated',
};

// internal/api dnsName: RFC 1123-ish labels, underscores allowed.
const LABEL = '[a-z0-9_]([a-z0-9_-]{0,61}[a-z0-9_])?';
const DNS_NAME = new RegExp(`^(${LABEL}\\.)*${LABEL}$`);

function dnsNameError(n: string, raw: string, example: string): string | null {
  if (n.length > 253 || !DNS_NAME.test(n)) return `"${raw}" is not a domain name (example: ${example})`;
  return null;
}

/**
 * The value as the server stores it: exact names lower-case without the
 * trailing dot, wildcards as "*.x" (from "*.x", "||x^" or "x"), regex
 * without the slashes.
 */
export function normaliseValue(type: DomainEntryType, raw: string): string {
  const v = raw.trim();
  if (type === 'regex') return v.length > 2 && v.startsWith('/') && v.endsWith('/') ? v.slice(1, -1) : v;
  let n = v.toLowerCase().replace(/\.$/, '');
  if (type === 'wildcard') {
    if (n.startsWith('||')) n = n.slice(2).replace(/\^$/, '');
    else if (n.startsWith('*.')) n = n.slice(2);
    return `*.${n}`;
  }
  return n;
}

// Go's regexp (RE2) has no lookaround and no backreferences; JavaScript
// accepts both, so they are refused here before the server would.
const NOT_RE2 = /\(\?<?[=!]|\\[1-9]|\\k</;

/** Why a value does not fit the type (null when it does), mirroring the server's checks. */
export function valueError(type: DomainEntryType, raw: string): string | null {
  const v = raw.trim();
  if (!v) return 'Required';
  if (type === 'regex') {
    const re = normaliseValue('regex', v);
    if (!re) return 'Empty expression';
    if (NOT_RE2.test(re)) return 'RE2 has no lookahead, lookbehind or backreferences';
    try {
      new RegExp(re);
      return null;
    } catch (e) {
      return `Not a valid expression: ${(e as Error).message}`;
    }
  }
  const n = v.toLowerCase().replace(/\.$/, '');
  if (type === 'exact') {
    if (n.startsWith('*.') || n.startsWith('||')) return `"${v}" matches subdomains: use the type Wildcard`;
    return dnsNameError(n, v, 'ads.example.com');
  }
  const base = normaliseValue('wildcard', n).slice(2);
  if (base.includes('*')) return `"${v}": only a leading "*." is supported (example: *.example.com)`;
  return dnsNameError(base, v, '*.example.com');
}

/** The values of the text box: one per line; exact names and wildcards may also be separated by spaces or commas. */
export function splitValues(type: DomainEntryType, text: string): string[] {
  const lines = text.split(/\r?\n/).filter((l) => !l.trim().startsWith('#'));
  const parts = type === 'regex' ? lines : lines.flatMap((l) => l.split(/[\s,]+/));
  return parts.map((s) => s.trim()).filter(Boolean);
}

/** The first value that does not fit the type, with the reason. */
export function firstValueError(type: DomainEntryType, values: string[]): { value: string; error: string } | null {
  for (const v of values) {
    const error = valueError(type, v);
    if (error) return { value: v, error };
  }
  return null;
}

/** "ns/name" → "name" in the default namespace, else the whole ref. */
export function shortRef(ref: string, namespace = 'default'): string {
  const i = ref.indexOf('/');
  return i > 0 && ref.slice(0, i) === namespace ? ref.slice(i + 1) : ref;
}

/** /groups/ns/name, /lists/ns/name, /requests/ns/name, /policies/ns/name of a "ns/name" ref. */
export function refPath(kind: 'groups' | 'lists' | 'requests' | 'policies', ref: string): string {
  const i = ref.indexOf('/');
  const ns = i > 0 ? ref.slice(0, i) : 'default';
  const name = i > 0 ? ref.slice(i + 1) : ref;
  return `/${kind}/${encodeURIComponent(ns)}/${encodeURIComponent(name)}`;
}

/** The actor of createdBy without its kind prefix ("User:admin" → "admin"). */
export function actorName(createdBy: string | undefined): string | undefined {
  if (!createdBy) return undefined;
  const i = createdBy.indexOf(':');
  return i > 0 && i < createdBy.length - 1 ? createdBy.slice(i + 1) : createdBy;
}

/** Whether the scope of an entry reads "Everyone" or names groups. */
export function scopeText(e: Pick<DomainEntry, 'everyone' | 'groups' | 'namespace'>): string {
  if (e.everyone) return e.groups.length ? `Everyone and ${e.groups.map((g) => shortRef(g, e.namespace)).join(', ')}` : 'Everyone';
  return e.groups.map((g) => shortRef(g, e.namespace)).join(', ') || 'Everyone';
}

/** The field of a Policy that references lists of this action. */
export function refField(action: DomainAction): 'allowListRefs' | 'blockListRefs' {
  return action === 'allow' ? 'allowListRefs' : 'blockListRefs';
}

/** The action of a ui-* list name ("ui-allow", "ui-deny-kids"), else undefined. */
export function uiListAction(name: string): DomainAction | undefined {
  if (name === 'ui-allow' || name.startsWith('ui-allow-')) return 'allow';
  if (name === 'ui-deny' || name.startsWith('ui-deny-')) return 'deny';
  return undefined;
}

/** "N entries" / "1 entry". */
export function entriesWord(n: number): string {
  return n === 1 ? 'entry' : 'entries';
}

/** A sentence for the policies a write could not attach a list to. */
export function skippedText(skipped: string[], action: DomainAction): string {
  if (!skipped.length) return '';
  const names = skipped.map((p) => shortRef(p)).join(', ');
  return `Not applied for ${names}: managed elsewhere, add the list to its ${refField(action)} there.`;
}
