// Pieces of an unblock request shared by the Requests table and the
// request page. Everything the requester typed (name, contact, reason) is
// rendered as React text, never as HTML.
import { Badge } from '@astryxdesign/core/Badge';
import type { UnblockBlock, UnblockRequest, UnblockState } from '../../api/types';
import { formatDateTime, formatRelative } from '../../lib/format';
import { clientHref, objectHref, shortRef } from '../../lib/policies';
import { Link } from '../../lib/router';
import { blockKindText, blockSource, stateTone } from '../../lib/unblock';

export function StateBadge({ state }: { state: UnblockState }) {
  return (
    <span data-testid="request-state" data-state={state}>
      <Badge variant={stateTone(state)} label={state} />
    </span>
  );
}

/** A timestamp as "3 minutes ago" with the exact time as its title. */
export function When({ ts }: { ts: string | undefined }) {
  if (!ts) return <span className="k-muted">–</span>;
  return (
    <time dateTime={ts} title={formatDateTime(ts)}>
      {formatRelative(ts)}
    </time>
  );
}

/** The client as querylog.privacy allowed recording it. */
export function ClientText({ r }: { r: Pick<UnblockRequest, 'client' | 'clientPrivacy'> }) {
  if (r.clientPrivacy === 'hidden' || !r.client) return <span className="k-muted">hidden by query-log privacy</span>;
  if (r.clientPrivacy === 'prefix') {
    return (
      <span>
        <span className="k-mono">{r.client}</span> <span className="k-muted k-small">network address, anonymised by query-log privacy</span>
      </span>
    );
  }
  return (
    <Link href={clientHref(r.client)} className="k-mono">
      {r.client}
    </Link>
  );
}

/** What the requester typed about themselves: "name (contact)". */
export function requesterText(r: Pick<UnblockRequest, 'requesterName' | 'contact'>): string {
  if (r.requesterName && r.contact) return `${r.requesterName} (${r.contact})`;
  return r.requesterName ?? r.contact ?? '';
}

export function RefLinks({ kind, refs, empty }: { kind: 'groups' | 'policies' | 'lists'; refs: string[]; empty: string }) {
  if (!refs.length) return <span className="k-muted">{empty}</span>;
  return (
    <span>
      {refs.map((g, i) => (
        <span key={g}>
          {i ? ', ' : ''}
          <Link href={objectHref(kind, g)}>{shortRef(g)}</Link>
        </span>
      ))}
    </span>
  );
}

/** The requester column: typed name and contact, client, groups. */
export function Requester({ r }: { r: UnblockRequest }) {
  const typed = requesterText(r);
  return (
    <span className="k-stack-sm">
      {typed ? <span className="k-break">{typed}</span> : null}
      <span className="k-small">
        <ClientText r={r} />
      </span>
      {r.groups.length ? (
        <span className="k-small">
          <RefLinks kind="groups" refs={r.groups} empty="" />
        </span>
      ) : null}
    </span>
  );
}

/** The deciding list (a link for a BlockList; services and engines as text). */
function BlockSource({ list }: { list: string | undefined }) {
  const src = blockSource(list);
  if (!src) return null;
  if (src.kind === 'list') return <Link href={objectHref('lists', src.ref)}>{shortRef(src.ref)}</Link>;
  return <span>{src.kind === 'service' ? `service ${src.id}` : `safe search ${src.id}`}</span>;
}

/** Why the name was blocked when the request was made. */
export function WhyBlocked({ block, detailed }: { block: UnblockBlock | undefined; detailed?: boolean }) {
  if (!block) return <span className="k-muted">not recorded</span>;
  return (
    <span className="k-stack-sm">
      <span>
        {blockKindText(block)}
        {block.list ? (
          <>
            : <BlockSource list={block.list} />
          </>
        ) : null}
      </span>
      {block.policy ? (
        <span className="k-small">
          policy <Link href={objectHref('policies', block.policy)}>{shortRef(block.policy)}</Link>
        </span>
      ) : null}
      {detailed && block.rule ? <span className="k-mono k-small k-break">{block.rule}</span> : null}
    </span>
  );
}
