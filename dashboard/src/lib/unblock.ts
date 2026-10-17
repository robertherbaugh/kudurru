// Unblock requests (the block page's "ask to allow"): the decision options
// with their explanations (internal/unblock/decide.go), the approve body,
// and the words for states and block reasons. Pure, so tests and pages
// share them.
import type { EnumOption } from '../components/fields/EnumSelect';
import type { DurationPreset } from '../components/fields/DurationField';
import type { UnblockApproval, UnblockBlock, UnblockMatch, UnblockRequest, UnblockScope, UnblockState } from '../api/types';
import { parseDuration } from './format';
import { shortRef, splitRef } from './policies';

export const UNBLOCK_STATES: UnblockState[] = ['Pending', 'Approved', 'Denied', 'Expired'];

/** The state filter of the Requests page; "all" sends no state. */
export const STATE_FILTER: EnumOption[] = [...UNBLOCK_STATES.map((s) => ({ value: s, label: s })), { value: 'all', label: 'All' }];

export type BadgeTone = 'warning' | 'success' | 'error' | 'neutral';

export function stateTone(s: UnblockState): BadgeTone {
  switch (s) {
    case 'Pending':
      return 'warning';
    case 'Approved':
      return 'success';
    case 'Denied':
      return 'error';
    default:
      return 'neutral';
  }
}

/** The allow list of an approval: ui-allow, or ui-allow-<group> (unblock.GroupListPrefix). */
export function allowListName(scope: UnblockScope, group?: string): string {
  return scope === 'group' && group ? `ui-allow-${splitRef(group).name}` : 'ui-allow';
}

/** The scope choice; group is disabled when the request names no client group. */
export function scopeOptions(r: Pick<UnblockRequest, 'groups'>, group?: string): EnumOption[] {
  const g = group ?? r.groups[0];
  return [
    {
      value: 'everyone',
      label: 'Everyone',
      help: 'Adds the name to the everyone list ui-allow (the Allowlist), attached to every policy kudurru may edit, so every client may resolve it, also under strict or allow-only policies.',
    },
    {
      value: 'group',
      label: g ? `The requester's group (${shortRef(g)})` : "The requester's group (the request names none)",
      help: g
        ? `Adds it to ${allowListName('group', g)}, attached to the policies of client group ${shortRef(g)} only.`
        : 'Adds it to ui-allow-<group>, attached to the policies of that client group only.',
      disabled: r.groups.length === 0,
    },
  ];
}

export function matchOptions(domain: string): EnumOption[] {
  return [
    { value: 'exact', label: 'This name only', help: `Only ${domain}.` },
    { value: 'subdomains', label: 'This name and its subdomains', help: `${domain} and every name below it (||${domain}^).` },
  ];
}

/** Durations of a time-limited approval; the empty value (emptyLabel) is permanent. */
export const APPROVAL_DURATIONS: DurationPreset[] = [
  { value: '1h', label: '1 hour' },
  { value: '8h', label: '8 hours' },
  { value: '24h', label: '1 day' },
  { value: '168h', label: '7 days' },
];

/** The server's bound on a time-limited approval (maxApprovalDuration). */
export const MAX_APPROVAL_SECONDS = 366 * 24 * 3600;

export const NOTE_MAX = 500;

export interface ApprovalDraft {
  scope: UnblockScope;
  match: UnblockMatch;
  /** "ns/name", one of the request's groups. */
  group: string;
  /** Go duration; empty is permanent. */
  duration: string;
  note: string;
}

export function approvalDraft(r: Pick<UnblockRequest, 'groups'>): ApprovalDraft {
  return { scope: 'everyone', match: 'exact', group: r.groups[0] ?? '', duration: '', note: '' };
}

/** A user-facing error for the approval's duration, or null. */
export function approvalDurationError(v: string): string | null {
  const t = v.trim();
  if (!t) return null;
  const s = parseDuration(t);
  if (Number.isNaN(s)) return 'A duration such as 1h, 8h or 36h';
  if (s <= 0) return 'Longer than 0; choose Permanently for no end';
  if (s > MAX_APPROVAL_SECONDS) return 'At most 366 days (8784h)';
  return null;
}

/** POST …/approve: no duration is permanent; group only for a group approval. */
export function approvalBody(d: ApprovalDraft): UnblockApproval {
  const body: UnblockApproval = { scope: d.scope, match: d.match };
  if (d.scope === 'group' && d.group) body.group = d.group;
  if (d.duration.trim()) body.duration = d.duration.trim();
  if (d.note.trim()) body.note = d.note.trim();
  return body;
}

/** What the "list" of a block names: a BlockList ref, a blocked service or a safe-search engine. */
export function blockSource(list: string | undefined): { kind: 'list'; ref: string } | { kind: 'service' | 'safesearch'; id: string } | null {
  if (!list) return null;
  if (list.startsWith('service:')) return { kind: 'service', id: list.slice('service:'.length) };
  if (list.startsWith('safesearch:')) return { kind: 'safesearch', id: list.slice('safesearch:'.length) };
  return { kind: 'list', ref: list };
}

/** A short sentence for the block kind. */
export function blockKindText(b: UnblockBlock): string {
  switch (b.kind) {
    case 'list':
      return 'On a block list';
    case 'service':
      return 'A blocked service';
    case 'safesearch':
      return 'Safe search';
    case 'policy':
      return 'Not on the allow lists of an allow-only policy';
    case 'cname':
      return 'Its CNAME target is blocked';
    default:
      return b.kind;
  }
}

/** The path of a request's detail page. */
export function requestHref(r: Pick<UnblockRequest, 'namespace' | 'name'>): string {
  return `/requests/${encodeURIComponent(r.namespace)}/${encodeURIComponent(r.name)}`;
}

/** Whether an approval can be given (not while one is in force). */
export function canApprove(r: Pick<UnblockRequest, 'state'>): boolean {
  return r.state !== 'Approved';
}

/** Whether a denial makes sense: a pending request, or revoking an approval. */
export function canDeny(r: Pick<UnblockRequest, 'state'>): boolean {
  return r.state === 'Pending' || r.state === 'Approved';
}

/** "1 unblock request" / "3 unblock requests". */
export function requestsLabel(n: number): string {
  return `${n} unblock request${n === 1 ? '' : 's'}`;
}
