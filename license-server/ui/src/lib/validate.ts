// The checks the portal's forms run before they send anything, mirroring
// the server's rules (spec §6: the licensee follows the public form's name
// rules; emails need one @, a local part and a dotted domain). The server
// repeats every check and its 400 `fields` name what it refused; these
// only spare a round trip and put the message next to the field.
import { MAX_NODES, PERSONAL_MAX_NODES, type Plan } from '@shared/catalog';
import { numberError } from '../ui/fields/NumberField';

/** The longest licensee, customer name or request name (UTF-16 code units, as the server counts). */
export const NAME_MAX = 200;

// Cc characters (C0, DEL, C1): the server refuses them in names.
// eslint-disable-next-line no-control-regex
const CONTROL = /[\u0000-\u001f\u007f-\u009f]/;

/** A licensee or customer name: 1-200 characters, not blank, no control characters. */
export function nameError(v: string, what = 'a name'): string | null {
  if (!v.trim()) return `Enter ${what}`;
  if (v.length > NAME_MAX) return `At most ${NAME_MAX} characters`;
  if (CONTROL.test(v)) return 'No control characters';
  return null;
}

/** An email address as the server accepts it (≤ 254, one @, a local part, a domain with a dot, no spaces). */
export function emailError(v: string, opts: { optional?: boolean } = {}): string | null {
  const t = v.trim();
  if (!t) return opts.optional ? null : 'Enter an email address';
  if (t.length > 254 || /\s/.test(t)) return 'Not an email address';
  const at = t.indexOf('@');
  if (at <= 0 || at !== t.lastIndexOf('@')) return 'Not an email address';
  const domain = t.slice(at + 1);
  if (!domain.includes('.') || domain.startsWith('.') || domain.endsWith('.')) return 'Not an email address';
  return null;
}

/** The node limit a plan allows. */
export function maxNodesFor(plan: Plan): number {
  return plan === 'personal' ? PERSONAL_MAX_NODES : MAX_NODES;
}

/** The nodes field of a key: a whole number from 1 to the plan's limit. */
export function nodesError(v: string, plan: Plan): string | null {
  return numberError(v, { min: 1, max: maxNodesFor(plan) });
}
