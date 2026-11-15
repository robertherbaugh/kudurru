// RPZ feeds (D391): how the dashboard words the feed state a node reports
// (GET /blocklists/_rpz, /blocklists/{namespace}/{name}/rpz) and checks the
// fields of an RPZ source (api/v1alpha1 RPZSource). Feed state is per node:
// every text here says "this node", never "the list".
import type { BlockList, ListSource, RPZFeed, RPZSource } from '../api/types';
import { isRPZSource, listNamespace, sourceEnabled } from './lists';
import { hostError } from './rr';
import { primaryEndpointError, tsigKeyNameError } from './zones';

function cleanList(v: string[]): string[] {
  return v.map((x) => x.trim()).filter(Boolean);
}

export interface FeedStateView {
  label: string;
  tone: 'success' | 'info' | 'warning' | 'error' | 'neutral';
  /** Whether a client whose policy selects the list gets SERVFAIL from this node. */
  servfail: boolean;
}

/** The four states of api.RPZFeed; any other word (a newer node) is shown as it is and assumed not to apply. */
export function feedStateView(f: Pick<RPZFeed, 'state' | 'lastError' | 'available'>): FeedStateView {
  switch (f.state) {
    case 'serving':
      return f.lastError ? { label: 'serving, refresh failed', tone: 'warning', servfail: false } : { label: 'serving', tone: 'success', servfail: false };
    case 'pending':
      return { label: 'pending', tone: 'warning', servfail: true };
    case 'expired':
      return { label: 'expired', tone: 'error', servfail: true };
    case 'unavailable':
      return { label: 'unavailable', tone: 'error', servfail: true };
    default:
      return { label: f.state || 'unknown', tone: 'neutral', servfail: f.available !== true };
  }
}

/** The plain consequence of a feed that is not serving on this node. */
export function servfailText(f: Pick<RPZFeed, 'state' | 'node'>): string {
  const where = f.node ? `node ${f.node}` : 'this node';
  if (f.state === 'pending') return `Until its first complete transfer, ${where} answers SERVFAIL to every client whose policy uses this list. The list is required coverage; it is never skipped.`;
  return `${where[0]?.toUpperCase() ?? ''}${where.slice(1)} answers SERVFAIL to every client whose policy uses this list until a complete transfer succeeds.`;
}

const STATE_RANK: Record<string, number> = { serving: 0, pending: 2, expired: 3, unavailable: 3 };

/** The feed that most needs attention (unavailable or expired, then pending, then a failed refresh). */
export function worstFeed(feeds: RPZFeed[]): RPZFeed | undefined {
  const rank = (f: RPZFeed) => (f.state && f.state in STATE_RANK ? (STATE_RANK[f.state] ?? 0) + (f.state === 'serving' && f.lastError ? 1 : 0) : 3);
  return feeds.reduce<RPZFeed | undefined>((worst, f) => (worst === undefined || rank(f) > rank(worst) ? f : worst), undefined);
}

/** The feeds of one list in an answer of GET /blocklists/_rpz. */
export function feedsOf(feeds: RPZFeed[] | null | undefined, l: Pick<BlockList, 'metadata'>): RPZFeed[] {
  const ns = listNamespace(l);
  return (feeds ?? []).filter((f) => f.list === l.metadata.name && (f.namespace ?? 'default') === ns);
}

/** Whether the list has an enabled RPZ feed (the node reports only enabled ones). */
export function hasEnabledFeed(l: Pick<BlockList, 'spec'>): boolean {
  return l.spec.sources.some((s) => isRPZSource(s) && sourceEnabled(s));
}

/** Words for the compiler's ignored-owner reasons (rpz.IgnoredReason); unknown ones are shown as they are. */
export const IGNORED_REASONS: Record<string, string> = {
  'apex-policy': 'Policy at the zone apex',
  'resolver-ns-provenance': 'NSDNAME or NSIP trigger (needs the resolver’s name servers)',
  'invalid-address-trigger': 'Address trigger that is not a valid prefix',
  'unknown-reserved-trigger': 'Unknown rpz- trigger',
  'nonterminal-wildcard': 'Wildcard that is not the first label',
  'dname-action': 'DNAME action',
  'invalid-cname-action': 'Invalid CNAME target',
  'unknown-reserved-action': 'Unknown rpz- action',
  'nonterminal-cname-wildcard': 'CNAME action with a wildcard that is not the first label',
};

export function ignoredReasonText(reason: string | undefined): string {
  return (reason && IGNORED_REASONS[reason]) || reason || 'unknown reason';
}

/** The fields of an RPZ source as the dialog and the wizard edit them. */
export interface RPZDraft {
  zoneName: string;
  primaries: string[];
  transferProtocol: 'tcp' | 'tls';
  tsigKeyRef: string;
}

export const EMPTY_RPZ: RPZDraft = { zoneName: '', primaries: [], transferProtocol: 'tcp', tsigKeyRef: '' };

export function rpzDraft(src: RPZSource | undefined): RPZDraft {
  if (!src) return EMPTY_RPZ;
  return { zoneName: src.zoneName ?? '', primaries: src.primaries ?? [], transferProtocol: src.transferProtocol === 'tls' ? 'tls' : 'tcp', tsigKeyRef: src.tsigKeyRef ?? '' };
}

/** The policy-zone origin: a non-root DNS name, absolute or not (the CRD pattern). */
export function rpzZoneError(zone: string): string | null {
  const z = zone.trim();
  if (!z) return 'Required: the policy zone to transfer, e.g. rpz.vendor.example.';
  if (z === '.') return 'The root zone cannot be a policy zone';
  if (z.length > 253) return 'At most 253 characters';
  if (z.includes('_')) return 'Letters, digits, "-" and "." only';
  return hostError(z, { allowRoot: false });
}

/** The first problem of an RPZ draft, or null. */
export function rpzDraftError(d: RPZDraft): string | null {
  return (
    rpzZoneError(d.zoneName) ??
    rpzPrimariesError(d.primaries) ??
    (d.tsigKeyRef.trim().length > 253 ? 'The TSIG key name has at most 253 characters' : tsigKeyNameError(d.tsigKeyRef))
  );
}

/** The first problem of the primaries list (1 to 32 endpoints of at most 300 characters), or null. */
export function rpzPrimariesError(rows: string[]): string | null {
  const primaries = cleanList(rows);
  if (!primaries.length) return 'At least one primary server is required';
  if (primaries.length > 32) return 'At most 32 primary servers';
  for (let i = 0; i < primaries.length; i++) {
    const p = primaries[i] as string;
    const e = p.length > 300 ? 'At most 300 characters' : primaryEndpointError(p);
    if (e) return `Primary ${i + 1} (${p}): ${e}`;
  }
  return null;
}

/** The rpz block of a source from a draft (tcp, the default, and an empty key are omitted). */
export function rpzSourceOf(d: RPZDraft): RPZSource {
  return {
    zoneName: d.zoneName.trim(),
    primaries: cleanList(d.primaries),
    ...(d.transferProtocol === 'tls' ? { transferProtocol: 'tls' as const } : {}),
    ...(d.tsigKeyRef.trim() ? { tsigKeyRef: d.tsigKeyRef.trim() } : {}),
  };
}

/** "rpz.vendor.example. from 192.0.2.53, ns2.vendor.example (TLS, TSIG vendor-xfr.)" */
export function rpzSummary(src: ListSource): string {
  const r = src.rpz;
  if (!r) return 'RPZ feed without transfer settings';
  const extras = [r.transferProtocol === 'tls' ? 'TLS' : '', r.tsigKeyRef ? `TSIG ${r.tsigKeyRef}` : ''].filter(Boolean);
  return `${r.zoneName} from ${(r.primaries ?? []).join(', ') || 'no primary'}${extras.length ? ` (${extras.join(', ')})` : ''}`;
}
