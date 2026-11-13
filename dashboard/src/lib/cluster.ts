// Enrolled SQLite cluster membership in the dashboard (docs/deploy/sqlite-cluster.md):
// the checks of the activation, invitation and promotion wizards, what a
// failed command means for the next decision, the commands and the Compose
// snippet for a new member, and the documented operator action for each
// managed root-anchor state (docs/deploy/sqlite-join.md).
//
// None of the three commands is ever sent twice by the dashboard: the server
// never refreshes a refused review and a lost answer does not prove that
// nothing happened (D345, D347, D354).
import { ApiError } from '../api/client';
import type { RootAnchorStatus } from '../api/types';
import { hostPortError } from './upstreams';

const DOCS = 'https://github.com/robertherbaugh/kudurru/blob/main/docs/deploy';

/** Operator documentation the cluster panels link to. */
export const CLUSTER_DOCS = {
  cluster: `${DOCS}/sqlite-cluster.md`,
  activate: `${DOCS}/sqlite-cluster.md#1-activate-the-first-primary`,
  join: `${DOCS}/sqlite-join.md`,
  joinRecover: `${DOCS}/sqlite-join.md#recover-an-incomplete-join`,
  promote: `${DOCS}/sqlite-cluster.md`,
  fenced: `${DOCS}/sqlite-join.md#reconnecting-an-old-primary`,
  conflict: `${DOCS}/sqlite-join.md#recover-from-conflicting-promotions`,
  anchorsMembers: `${DOCS}/sqlite-join.md#managed-root-anchors-on-fenced-and-conflicted-members`,
  anchorsReturn: `${DOCS}/sqlite-join.md#a-member-returns-with-different-trust-anchor-state`,
  anchorsRecognize: `${DOCS}/sqlite-join.md#recognize-a-member-that-refuses-the-primarys-data`,
  anchorsEnds: `${DOCS}/sqlite-join.md#what-ends-the-state`,
  anchorsReplace: `${DOCS}/sqlite-join.md#replace-a-member-that-cannot-follow`,
  anchorsAbandon: `${DOCS}/sqlite-join.md#keep-a-fenced-members-data-when-its-primary-is-lost-for-good`,
  anchorsServfail: `${DOCS}/sqlite-join.md#other-states-in-which-a-managed-member-answers-servfail`,
  anchorsQuarantine: `${DOCS}/runbooks.md#recover-a-quarantined-managed-root-anchor`,
} as const;

/** A member name: up to 63 lowercase letters, digits and hyphens (the node's node.name). */
export function memberNameError(v: string): string | null {
  if (!v) return 'A member name is required';
  if (v.length > 63) return 'At most 63 characters';
  return /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/.test(v) ? null : 'Lowercase letters, digits and hyphens, not at the start or end';
}

/** A member's internal address: host:port that the other members reach (TLS, port 8082 by default). */
export function memberAddressError(v: string): string | null {
  if (!v) return 'An internal address is required';
  if (v.length > 320) return 'At most 320 characters';
  // IPv6 hosts need brackets ([2001:db8::12]:8082); everything else has one colon.
  const bracketed = v.startsWith('[') && /\]:\d+$/.test(v);
  const plain = !v.startsWith('[') && v.indexOf(':') > 0 && v.indexOf(':') === v.lastIndexOf(':');
  if (!bracketed && !plain) return 'host:port, for example 10.0.0.12:8082';
  return hostPortError(v);
}

/** An audit reason (required by every cluster command, at most 512 characters). */
export function reasonError(v: string): string | null {
  if (!v.trim()) return 'A reason for the audit log is required';
  return v.length > 512 ? 'At most 512 characters' : null;
}

/**
 * What a failed cluster command means for the next decision:
 *
 * - `refused`: the server refused the request before it could commit
 *   anything (invalid input, a missing permission, this node's role); the
 *   operator may correct the input and decide again.
 * - `stale`: the reviewed state no longer matches (409); the review must be
 *   read again in a new wizard. Nothing is refreshed automatically.
 * - `uncertain`: the answer was lost or says that the outcome is unknown
 *   (completion_unknown, committed_output_unavailable, a network failure, a
 *   timeout, any other server error). The command may have committed; the
 *   wizard never sends it again.
 */
export type CommandOutcome = 'refused' | 'stale' | 'uncertain';

const REFUSED_BEFORE_HANDLER = new Set(['primary_required', 'cluster_fenced', 'cluster_conflict', 'generation_pending', 'forbidden', 'csrf']);

export function commandOutcome(err: unknown): CommandOutcome {
  if (err instanceof ApiError) {
    if (err.status === 409) return 'stale';
    if (err.status === 400 || err.status === 401 || err.status === 403 || err.status === 413 || err.status === 415 || err.status === 429) return 'refused';
    if (err.status === 503 && REFUSED_BEFORE_HANDLER.has(err.key)) return 'refused';
  }
  return 'uncertain';
}

/** The sentence a wizard shows after a stale or uncertain answer (it then sends nothing more). */
export function lockedText(outcome: Exclude<CommandOutcome, 'refused'>, what: string): string {
  if (outcome === 'stale') {
    return `The state you reviewed changed, so the server refused the ${what}. Close this wizard and open it again to review the current state; the dashboard does not send the ${what} again.`;
  }
  return `The dashboard cannot tell whether the ${what} happened. It does not send it again: close this wizard, check the enrollment on this page and decide from what it shows.`;
}

/**
 * The sentence after a 409 to an invitation. The primary answers 409 to more
 * than a changed source: also to a member name or internal address that an
 * earlier member used or reserved, and to an expiry more than ten minutes
 * ahead (or already past) on the primary's own clock. The answer does not
 * say which, so the wizard names all three and still sends nothing more.
 */
export function invitationConflictText(): string {
  return "The primary did not issue it. Either its state changed since your review, the member name or internal address is already used or reserved by an earlier member, or the expiry is more than ten minutes ahead or already past on the primary's clock (check this computer's clock against the primary's). Close this wizard and open it again with a fresh review and, if needed, another name or address; the dashboard does not send this invitation again.";
}

/** The offline join command for the new member (binary or package install). */
export function joinCommand(name: string, address: string, invitationFile: string): string {
  return [
    'kudurru cluster join --offline \\',
    '  --data-dir /var/lib/kudurru \\',
    `  --invitation-file ${invitationFile} \\`,
    `  --confirm-name ${name} \\`,
    `  --confirm-address ${address}`,
  ].join('\n');
}

/** The image tag of a release version ("v0.2.0" → "0.2.0"); undefined for a development build. */
export function imageTag(version: string | undefined): string | undefined {
  const m = /^v?(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)$/.exec(version ?? '');
  return m?.[1];
}

/**
 * The Compose changes for the new member, starting from deploy/compose/single:
 * its node name, a data directory that the join creates (it refuses one that
 * exists, and the volume's mount point does), and the published internal
 * listener. The secret encryption_key file must hold the primary's key.
 */
export function composeSnippet(name: string, address: string, version: string | undefined): string {
  const tag = imageTag(version);
  const port = address.slice(address.lastIndexOf(':') + 1);
  return [
    `# docker-compose.yml on ${name}: deploy/compose/single with these changes.`,
    '# secrets/encryption_key must be a copy of the primary\'s encryption key.',
    'services:',
    '  kudurru:',
    tag ? `    image: ghcr.io/robertherbaugh/kudurru:${tag}   # the primary's release` : '    # image: the same release as the primary',
    '    environment:',
    `      KUDURRU_NODE__NAME: ${name}`,
    '      KUDURRU_NODE__DATADIR: /var/lib/kudurru/member',
    '    ports:',
    `      - "${port}:8082"   # internal listener: the members reach ${address}`,
  ].join('\n');
}

/** The commands that join and start the Compose member (the invitation on standard input). */
export function composeCommands(name: string, address: string, invitationFile: string): string {
  return [
    `docker compose run -T --rm --no-deps kudurru cluster join --offline \\`,
    '  --data-dir /var/lib/kudurru/member --invitation-file /dev/stdin \\',
    `  --confirm-name ${name} --confirm-address ${address} < ${invitationFile}`,
    'docker compose up -d',
  ].join('\n');
}

/** Words for the reason of GET /cluster/promotion (internal/cluster/replica). */
export function promotionReasonText(reason: string): string {
  switch (reason) {
    case 'eligible':
      return 'This follower can be promoted: its primary has been unreachable for two pull intervals and it serves its installed data.';
    case 'awaiting_missed_pulls':
      return 'The primary still answers, or has not been unreachable for two pull intervals yet. Promote only when the primary is lost.';
    case 'not_follower':
      return 'Only a follower can be promoted; this node is not one.';
    case 'no_installed_image':
      return 'This follower has not installed data of its current term yet.';
    case 'fenced':
      return 'This member is fenced or holds a conflict; it cannot be promoted.';
    case 'serving_generation_pending':
      return 'This node does not serve its installed data yet; read the review again in a moment.';
    case 'request_unavailable':
      return 'The review could not be completed; read it again.';
    default:
      return reason;
  }
}

/** A managed root-anchor state that needs an operator, with the documented action. */
export interface AnchorAdvice {
  id: string;
  severity: 'error' | 'warning' | 'info';
  title: string;
  action: string;
  href: string;
}

/** Words and actions for a pull refusal (RootAnchorPull.refusal, D369). */
function refusalAdvice(refusal: string, tags: number[]): AnchorAdvice {
  const keys = tags.length ? ` (key tag ${tags.join(', ')})` : '';
  switch (refusal) {
    case 'carried_revocation':
      return {
        id: 'refusal', severity: 'warning', title: `This member refuses the primary's trust-anchor state: it knows a root key to be revoked that the primary still trusts${keys}`,
        action: "It ends on its own at the first pull after the primary's next root-anchor check records the revocation; make sure the primary reaches the root servers over TCP 53 and has a synchronized clock.",
        href: CLUSTER_DOCS.anchorsEnds,
      };
    case 'recovery_history':
      return {
        id: 'refusal', severity: 'warning', title: "This member refuses the primary's trust-anchor state: the two disagree about `anchors recover` history",
        action: 'This database cannot follow the current primary. Replace the member under a new name and internal address.',
        href: CLUSTER_DOCS.anchorsReplace,
      };
    case 'dormant_owner':
      return {
        id: 'refusal', severity: 'warning', title: "This member refuses the primary's trust-anchor state: the primary never started managed trust anchors",
        action: 'Start managed trust anchors on the primary from the same seeds (anchorMode: managed with the same trustAnchors), or replace the member.',
        href: CLUSTER_DOCS.anchorsEnds,
      };
    default:
      return {
        id: 'refusal', severity: 'warning', title: `This member refuses the primary's trust-anchor state (${refusal})`,
        action: "It keeps answering from its own last-good data and accepts no change. Compare both sides' anchor state, then wait for the state to end or replace the member.",
        href: CLUSTER_DOCS.anchorsReturn,
      };
  }
}

/** The states of GET /cluster/anchors that need an operator, most severe first. */
export function anchorAdvice(s: RootAnchorStatus): AnchorAdvice[] {
  const out: AnchorAdvice[] = [];
  if (s.owner.quarantined) {
    out.push({ id: 'quarantined', severity: 'error', title: 'Every trusted root key is lost: this node answers no validated recursion', action: 'Recover with `kudurru anchors recover --offline` on the stopped primary.', href: CLUSTER_DOCS.anchorsQuarantine });
  }
  if (s.local?.terminal) {
    out.push({ id: 'terminal', severity: 'error', title: "This member's root-anchor output is terminal: it can never publish again", action: 'Replace the member under a new name and internal address.', href: CLUSTER_DOCS.anchorsReplace });
  } else if (s.local?.retired) {
    out.push({ id: 'retired', severity: 'error', title: 'This member saw a signed root key revocation and answers SERVFAIL', action: 'It answers again once it installs state from the primary that revokes the key. Take it out of your DNS path meanwhile.', href: CLUSTER_DOCS.anchorsServfail });
  }
  if (s.local?.denied?.some((d) => !d.revokedByOwner)) {
    out.push({ id: 'denied', severity: 'warning', title: 'This member denied a root key that its installed state still trusts', action: 'Prefer to promote another member: as the primary this one would answer SERVFAIL until its own root check records the revocation.', href: CLUSTER_DOCS.anchorsServfail });
  }
  if (s.authority.conflict) {
    out.push({ id: 'conflict', severity: 'error', title: 'This member holds an unresolved promotion conflict', action: 'Decide which member to keep and run `kudurru cluster resolution --offline` and `cluster resolve --offline` on it.', href: CLUSTER_DOCS.conflict });
  } else if (s.authority.fencedBehindTerm) {
    out.push({
      id: 'fenced', severity: 'warning', title: `This member is fenced by a newer term (epoch ${s.authority.termEpoch}) whose data it never installed (epoch ${s.authority.installedTermEpoch})`,
      action: 'It serves its own last-good data DNS-only. While the primary runs, do not resolve: check that it reaches the primary. Only when the primary and every member that followed it are lost for good, keep its data with `cluster resolve --offline --abandon-term`.',
      href: CLUSTER_DOCS.anchorsAbandon,
    });
  }
  if (s.pull?.refusal) out.push(refusalAdvice(s.pull.refusal, s.pull.refusedKeyTags ?? []));
  for (const [name, w] of [['updater', s.updater], ['supervisor', s.supervisor]] as const) {
    if (w.state === 'stopped') {
      out.push({ id: `${name}-stopped`, severity: 'warning', title: `The root-anchor ${name} stopped${w.error ? `: ${w.error}` : ''}`, action: 'Fix the cause named here (often the clock or TCP 53 to the root servers) and restart kudurru on this node.', href: CLUSTER_DOCS.anchorsMembers });
    }
  }
  if (s.anchorMode === 'managed' && s.managedTopology && s.managedTopology !== 'disabled' && s.owner.phase === 'managed' && !s.admissionCurrent) {
    out.push({ id: 'admission', severity: 'warning', title: 'Managed root-anchor admission is closed for this process', action: 'Restart kudurru on this node to publish again.', href: CLUSTER_DOCS.anchorsServfail });
  }
  if (s.owner.clockUncertain) {
    out.push({ id: 'clock', severity: 'warning', title: "The clock of this node's last root-anchor sample is uncertain", action: 'Keep member clocks synchronized; a member far behind stops its root-anchor checks until the clock is fixed and kudurru restarts.', href: CLUSTER_DOCS.anchorsMembers });
  }
  return out;
}

const REFUSED_SINGLE = new Set([...REFUSED_BEFORE_HANDLER, 'unavailable', 'not_implemented', 'user_management_unavailable', 'factor_management_unavailable']);

/**
 * Whether the answer to a single create or revoke command (an edge block, a
 * role binding) leaves its outcome unknown: no answer at all, a 500 or 502/504,
 * or a 503 that is not one of the refusals the server gives before any
 * effect. A 4xx (including 409 exists or 404 gone) is a refusal. After an
 * unknown outcome the dashboard sends nothing again: it rereads and the
 * operator decides.
 */
export function outcomeUnknown(err: unknown): boolean {
  if (!(err instanceof ApiError) || err.status === 0) return true;
  if (err.status < 500 || err.status === 501) return false;
  // A dispatched store write answers 503 unavailable with
  // {dispatched: true, outcome: unknown|committed} (cmd/kudurru kube_edge_blocks.go).
  const hint: unknown = err.hint;
  if (hint && typeof hint === 'object' && ('dispatched' in hint || 'outcome' in hint)) return true;
  return !(err.status === 503 && REFUSED_SINGLE.has(err.key));
}

/** The hint of the only 503 unavailable that POST /iam/rolebindings gives before
 * it runs anything: a runtime without the SQLite RoleBinding command
 * (internal/api/rolebindings.go). */
const ROLE_BINDING_RUNTIME_MISSING = 'requires the complete SQLite runtime';

/**
 * outcomeUnknown for POST /iam/rolebindings. On the SQLite runtime that route
 * answers every error it does not classify, including a commit that was
 * attempted and failed, with the catch-all 503 unavailable
 * (servingHTTPAuthError in cmd/kudurru), so a 503 unavailable counts as a
 * refusal only with the missing-runtime hint; any other one leaves the
 * outcome unknown and the dialog sends nothing again.
 */
export function roleBindingOutcomeUnknown(err: unknown): boolean {
  if (err instanceof ApiError && err.status === 503 && err.key === 'unavailable') {
    return err.hint !== ROLE_BINDING_RUNTIME_MISSING;
  }
  return outcomeUnknown(err);
}
