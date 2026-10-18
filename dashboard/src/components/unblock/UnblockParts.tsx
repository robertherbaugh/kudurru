// Pieces of an unblock request shared by the Requests table and the
// request page. Everything the requester typed (name, contact, reason) is
// rendered as React text, never as HTML.
import { Badge } from '@astryxdesign/core/Badge';
import { Link } from '@astryxdesign/core/Link';
import { VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { Tooltip } from '@astryxdesign/core/Tooltip';
import type { ReactNode } from 'react';
import type { UnblockBlock, UnblockRequest, UnblockState } from '../../api/types';
import { formatDateTime, formatRelative } from '../../lib/format';
import { clientHref, objectHref, shortRef } from '../../lib/policies';
import { blockKindText, blockSource, stateTone } from '../../lib/unblock';

export function StateBadge({ state }: { state: UnblockState }) {
  return <Badge variant={stateTone(state)} label={state} data-testid="request-state" {...({ 'data-state': state } as object)} />;
}

/** A timestamp as "3 minutes ago" with the exact time in a tooltip. */
export function When({ ts }: { ts: string | undefined }) {
  if (!ts) return <Text type="inherit" color="secondary">–</Text>;
  return (
    <Tooltip content={formatDateTime(ts)} hasHoverIndication={false}>
      <Text type="inherit">
        <time dateTime={ts}>{formatRelative(ts)}</time>
      </Text>
    </Tooltip>
  );
}

/** Free text someone typed (a reason, a note): line breaks kept, long words broken. */
export function Typed({ children }: { children: ReactNode }) {
  return (
    <Text type="inherit" style={{ whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>
      {children}
    </Text>
  );
}

/** The client as querylog.privacy allowed recording it. */
export function ClientText({ r }: { r: Pick<UnblockRequest, 'client' | 'clientPrivacy'> }) {
  if (r.clientPrivacy === 'hidden' || !r.client) return <Text type="inherit" color="secondary">hidden by query-log privacy</Text>;
  if (r.clientPrivacy === 'prefix') {
    return (
      <Text type="inherit">
        <Text type="code">{r.client}</Text> <Text type="supporting">network address, anonymised by query-log privacy</Text>
      </Text>
    );
  }
  return (
    <Link type="inherit" href={clientHref(r.client)} hasUnderline>
      <Text type="code" color="inherit">
        {r.client}
      </Text>
    </Link>
  );
}

/** A name as the unblock requests record it: lower case, no trailing dot (query-log names may have one). */
export function domainKey(name: string): string {
  return name.replace(/\.$/, '').toLowerCase();
}

/** What the requester typed about themselves: "name (contact)". */
export function requesterText(r: Pick<UnblockRequest, 'requesterName' | 'contact'>): string {
  if (r.requesterName && r.contact) return `${r.requesterName} (${r.contact})`;
  return r.requesterName ?? r.contact ?? '';
}

export function RefLinks({ kind, refs, empty }: { kind: 'groups' | 'policies' | 'lists'; refs: string[]; empty: string }) {
  if (!refs.length) return <Text type="inherit" color="secondary">{empty}</Text>;
  return (
    <Text type="inherit">
      {refs.map((g, i) => (
        <Text type="inherit" key={g}>
          {i ? ', ' : ''}
          <Link type="inherit" href={objectHref(kind, g)} color="inherit" hasUnderline>
            {shortRef(g)}
          </Link>
        </Text>
      ))}
    </Text>
  );
}

/** The requester column: typed name and contact, client, groups. */
export function Requester({ r }: { r: UnblockRequest }) {
  const typed = requesterText(r);
  return (
    <VStack gap={1.5}>
      {typed ? <Typed>{typed}</Typed> : null}
      <Text type="supporting" color="primary">
        <ClientText r={r} />
      </Text>
      {r.groups.length ? (
        <Text type="supporting" color="primary">
          <RefLinks kind="groups" refs={r.groups} empty="" />
        </Text>
      ) : null}
    </VStack>
  );
}

/** The deciding list (a link for a BlockList; services and engines as text). */
function BlockSource({ list }: { list: string | undefined }) {
  const src = blockSource(list);
  if (!src) return null;
  if (src.kind === 'list') {
    return (
      <Link type="inherit" href={objectHref('lists', src.ref)} color="inherit" hasUnderline>
        {shortRef(src.ref)}
      </Link>
    );
  }
  return <>{src.kind === 'service' ? `service ${src.id}` : `safe search ${src.id}`}</>;
}

/** Why the name was blocked when the request was made. */
export function WhyBlocked({ block, detailed }: { block: UnblockBlock | undefined; detailed?: boolean }) {
  if (!block) return <Text type="inherit" color="secondary">not recorded</Text>;
  return (
    <VStack gap={1.5}>
      <Text type="inherit">
        {blockKindText(block)}
        {block.list ? (
          <>
            : <BlockSource list={block.list} />
          </>
        ) : null}
      </Text>
      {block.policy ? (
        <Text type="supporting" color="primary">
          policy{' '}
          <Link type="inherit" href={objectHref('policies', block.policy)} color="inherit" hasUnderline>
            {shortRef(block.policy)}
          </Link>
        </Text>
      ) : null}
      {detailed && block.rule ? (
        <Text type="code" maxLines={3} wordBreak="break-all">
          {block.rule}
        </Text>
      ) : null}
    </VStack>
  );
}
