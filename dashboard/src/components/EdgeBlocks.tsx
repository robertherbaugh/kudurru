// Operator edge blocks (D388): every dnsdist edge drops queries from a
// client prefix until an absolute expiry. The Clients page and a client's
// page offer "Block at edge" for an address; the Clients page lists the
// active blocks with Revoke. Create and revoke are single commands that need
// edgeblocks.write and run only on the primary (an enrolled follower answers
// 503 primary_required, so useCan refuses them there); an answer that leaves
// the outcome unknown is never sent again: the dialog locks and the list is
// read again.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Code } from '@astryxdesign/core/Code';
import { pixel, proportional, Table, type TableColumn } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { Tooltip } from '@astryxdesign/core/Tooltip';
import { VisuallyHidden } from '@astryxdesign/core/VisuallyHidden';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useRef, useState } from 'react';
import { useApi, useCan, useCannotReason } from '../api/context';
import type { EdgeBlock, EdgeBlockList } from '../api/types';
import { outcomeUnknown } from '../lib/cluster';
import { formatDateTime, formatRelative, parseDuration } from '../lib/format';
import { isIPv4, isIPv6 } from '../lib/rr';
import { ErrorBanner, errorText, useNotify } from './Feedback';
import { DurationField, durationPresets, secondsLabel } from './fields/DurationField';
import { TextField } from './fields/TextField';
import { FormDialog } from './FormDialog';
import { Panel } from './layout/Page';

export const edgeBlocksKey = ['edge', 'blocks'] as const;

/** The bounds GET /edge/blocks reports, with the server's defaults until it answers. */
interface Limits {
  minSeconds: number;
  maxSeconds: number;
  minIPv4Bits: number;
  minIPv6Bits: number;
}

const DEFAULT_LIMITS: Limits = { minSeconds: 60, maxSeconds: 604_800, minIPv4Bits: 16, minIPv6Bits: 32 };

const BLOCK_DURATIONS = durationPresets(['15m', '1h', '6h', '24h', '168h']);

/** Why a prefix cannot be blocked, or null: an address or a CIDR prefix no wider than the bounds. */
export function edgePrefixError(v: string, limits: Limits = DEFAULT_LIMITS): string | null {
  const t = v.trim();
  if (!t) return 'Enter an address or prefix';
  const [addr = '', len, extra] = t.split('/');
  if (extra !== undefined) return 'One "/" at most';
  const v4 = isIPv4(addr);
  if (!v4 && !isIPv6(addr)) return 'Not an IP address or CIDR prefix';
  if (len === undefined) return null;
  const max = v4 ? 32 : 128;
  const min = v4 ? limits.minIPv4Bits : limits.minIPv6Bits;
  if (!/^\d{1,3}$/.test(len) || Number(len) > max) return `The prefix length must be 0-${max}`;
  return Number(len) < min ? `${v4 ? 'IPv4' : 'IPv6'} prefixes must be /${min} or longer` : null;
}

/** Why a duration is outside the block bounds, or null. */
export function edgeDurationError(v: string, limits: Limits = DEFAULT_LIMITS): string | null {
  const s = parseDuration(v.trim());
  if (!v.trim() || Number.isNaN(s)) return 'A duration such as 30m, 2h or 3d (72h)';
  if (s < limits.minSeconds || s > limits.maxSeconds) return `Between ${secondsLabel(limits.minSeconds)} and ${secondsLabel(limits.maxSeconds)}`;
  return null;
}

function useEdgeBlocks(enabled = true) {
  const api = useApi();
  return useQuery<EdgeBlockList>({ queryKey: edgeBlocksKey, queryFn: () => api.cluster.edgeBlocks(), enabled, refetchInterval: 30_000 });
}

/** A command that is sent at most once per dialog; after an unknown outcome it stays locked. */
function useSingleCommand() {
  const qc = useQueryClient();
  const inFlight = useRef(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<unknown>();
  const [unknown, setUnknown] = useState(false);
  const run = async <T,>(send: () => Promise<T>, done: (v: T) => void) => {
    if (inFlight.current || unknown) return;
    inFlight.current = true;
    setPending(true);
    setError(undefined);
    try {
      done(await send());
    } catch (err) {
      setError(err);
      if (outcomeUnknown(err)) setUnknown(true);
    } finally {
      inFlight.current = false;
      setPending(false);
      void qc.invalidateQueries({ queryKey: edgeBlocksKey });
    }
  };
  const reset = () => {
    setError(undefined);
    setUnknown(false);
  };
  return { run, pending, error, unknown, reset };
}

function CommandError({ error, unknown, what }: { error: unknown; unknown: boolean; what: string }) {
  if (error === undefined) return null;
  if (!unknown) return <ErrorBanner error={error} />;
  return (
    <Banner
      status="warning"
      title="Outcome unknown"
      description={`The dashboard cannot tell whether the ${what} happened and does not send it again. Close this dialog and check the list of active edge blocks. The server said: ${errorText(error)}`}
    />
  );
}

export interface EdgeBlockButtonProps {
  /** The client address the dialog starts with. */
  address: string;
  /** The client's display name, for the reason and the dialog title. */
  name?: string | undefined;
  size?: 'sm' | 'md';
  variant?: 'ghost' | 'secondary';
}

/** "Block at edge" for one client address, with its dialog. */
export function EdgeBlockButton({ address, name, size = 'sm', variant = 'ghost' }: EdgeBlockButtonProps) {
  const can = useCan();
  const cannot = useCannotReason();
  const [open, setOpen] = useState(false);
  const allowed = can('edgeblocks', 'write');
  return (
    <>
      <Button
        label="Block at edge"
        aria-label={`Block ${name ?? address} at the edge`}
        size={size}
        variant={variant}
        isDisabled={!allowed}
        tooltip={allowed ? `Drop queries from ${address} at every dnsdist edge until a time you choose` : cannot('edgeblocks', 'write')}
        onClick={() => setOpen(true)}
      />
      {open ? <EdgeBlockDialog address={address} name={name} onClose={() => setOpen(false)} /> : null}
    </>
  );
}

function EdgeBlockDialog({ address, name, onClose }: { address: string; name?: string | undefined; onClose: () => void }) {
  const api = useApi();
  const notify = useNotify();
  const list = useEdgeBlocks();
  const limits: Limits = list.data ?? DEFAULT_LIMITS;
  const [prefix, setPrefix] = useState(address);
  const [duration, setDuration] = useState('1h');
  const [reason, setReason] = useState('');
  const [touched, setTouched] = useState(false);
  const cmd = useSingleCommand();
  const prefixErr = edgePrefixError(prefix, limits);
  const durationErr = edgeDurationError(duration, limits);
  const reasonErr = new TextEncoder().encode(reason).length > 256 ? 'At most 256 bytes' : null;
  const full = list.data ? list.data.items.length >= list.data.maxBlocks : false;
  const submit = () => {
    setTouched(true);
    if (prefixErr || durationErr || reasonErr || cmd.unknown) return;
    void cmd.run(
      () => api.cluster.createEdgeBlock({ prefix: prefix.trim(), seconds: parseDuration(duration.trim()), ...(reason.trim() ? { reason: reason.trim() } : {}) }),
      (b) => {
        notify.success(`Blocked ${b.prefix} at the edge until ${formatDateTime(b.until)}`);
        onClose();
      },
    );
  };
  return (
    <FormDialog
      isOpen
      onClose={onClose}
      title={`Block ${name ?? address} at the edge`}
      width={500}
      onSubmit={submit}
      footer={
        <>
          <Button label={cmd.unknown ? 'Close' : 'Cancel'} variant="ghost" onClick={onClose} />
          <Button type="submit" label="Block" variant="destructive" isLoading={cmd.pending} isDisabled={cmd.unknown || full} />
        </>
      }
    >
      <Text as="p">
        Every dnsdist edge drops this client’s queries until the block expires. Edges pull the change within seconds and enforce the expiry themselves. Queries that reach a kudurru node directly, without an edge, are not affected.
      </Text>
      {full ? <Banner status="warning" title={`${list.data?.maxBlocks ?? ''} blocks are active, the most the server keeps`} description="Revoke a block or wait for one to expire first." /> : null}
      <TextField
        label="Address or prefix"
        value={prefix}
        onChange={setPrefix}
        mono
        isRequired
        placeholder="192.0.2.10 or 192.0.2.0/24"
        help={`One address, or a prefix of /${limits.minIPv4Bits} or longer (IPv4) or /${limits.minIPv6Bits} or longer (IPv6).`}
        error={touched || prefix !== address ? (prefixErr ?? undefined) : undefined}
      />
      <DurationField label="Block for" value={duration} onChange={setDuration} presets={BLOCK_DURATIONS} description={`From ${secondsLabel(limits.minSeconds)} to ${secondsLabel(limits.maxSeconds)}.`} error={durationErr ?? undefined} />
      <TextField label="Reason" value={reason} onChange={setReason} isOptional help="Kept in the list and the audit trail; never sent to the edges." error={reasonErr ?? undefined} />
      <CommandError error={cmd.error} unknown={cmd.unknown} what="block" />
    </FormDialog>
  );
}

type BlockRow = EdgeBlock & Record<string, unknown>;

/** The active operator edge blocks with Revoke (needs edgeblocks.read to list). */
export function EdgeBlocksPanel() {
  const can = useCan();
  const readable = can('edgeblocks', 'read');
  const list = useEdgeBlocks(readable);
  const [revoking, setRevoking] = useState<EdgeBlock | null>(null);
  const writable = can('edgeblocks', 'write');
  const cannot = useCannotReason();
  if (!readable) return null;
  const columns: TableColumn<BlockRow>[] = [
    {
      key: 'prefix',
      header: 'Prefix',
      width: proportional(1.5, { minWidth: 150 }),
      renderCell: (b) => (
        <Text type="code" size="sm">
          {b.prefix}
        </Text>
      ),
    },
    {
      key: 'until',
      header: 'Until',
      width: proportional(1.2, { minWidth: 120 }),
      renderCell: (b) => (
        <Tooltip content={formatDateTime(b.until)}>
          <Text>{formatRelative(b.until)}</Text>
        </Tooltip>
      ),
    },
    { key: 'reason', header: 'Reason', width: proportional(2, { minWidth: 140 }), renderCell: (b) => (b.reason ? <Text>{b.reason}</Text> : <Text color="secondary">–</Text>) },
    {
      key: 'createdBy',
      header: 'Blocked by',
      width: proportional(1.5, { minWidth: 140 }),
      renderCell: (b) => (
        <Text size="sm">
          {b.createdBy ?? '–'}{' '}
          <Text color="secondary" type="inherit">
            {formatRelative(b.createdAt)}
          </Text>
        </Text>
      ),
    },
    {
      key: 'actions',
      header: <VisuallyHidden>Actions</VisuallyHidden>,
      width: pixel(110),
      renderCell: (b) => (
        <Button
          label="Revoke"
          aria-label={`Revoke the edge block of ${b.prefix}`}
          size="sm"
          variant="ghost"
          isDisabled={!writable}
          tooltip={writable ? undefined : cannot('edgeblocks', 'write')}
          onClick={() => setRevoking(b)}
        />
      ),
    },
  ];
  return (
    <Panel
      title="Edge blocks"
      headingId="edge-blocks-title"
      description={
        list.data ? (
          <>
            Clients every dnsdist edge drops until the block expires ({list.data.items.length} of at most {list.data.maxBlocks}). Revoking one clears dnsdist’s dynamic-block table and reinstalls the rest, so clients the edge’s rate rules had
            blocked get through until they exceed the rate again. The CLI has the same commands: <Code size="inherit">kudurru edge blocks</Code>.
          </>
        ) : undefined
      }
    >
      {list.error ? <ErrorBanner error={list.error} /> : null}
      {list.data ? (
        <Table<BlockRow>
          aria-label="Active edge blocks"
          density="compact"
          idKey="id"
          data={list.data.items}
          columns={columns}
          emptyState={<Text color="secondary">No client is blocked at the edge.</Text>}
        />
      ) : null}
      {revoking ? <RevokeDialog block={revoking} onClose={() => setRevoking(null)} /> : null}
    </Panel>
  );
}

function RevokeDialog({ block, onClose }: { block: EdgeBlock; onClose: () => void }) {
  const api = useApi();
  const notify = useNotify();
  const cmd = useSingleCommand();
  return (
    <FormDialog
      isOpen
      onClose={onClose}
      title={`Revoke the edge block of ${block.prefix}`}
      width={460}
      onSubmit={() =>
        void cmd.run(
          () => api.cluster.revokeEdgeBlock(block.id),
          () => {
            notify.success(`Revoked the edge block of ${block.prefix}`);
            onClose();
          },
        )
      }
      footer={
        <>
          <Button label={cmd.unknown ? 'Close' : 'Cancel'} variant="ghost" onClick={onClose} />
          <Button type="submit" label="Revoke" variant="primary" isLoading={cmd.pending} isDisabled={cmd.unknown} />
        </>
      }
    >
      <Text as="p">
        Edges let {block.prefix} through again within seconds. The block would otherwise expire {formatRelative(block.until)} ({formatDateTime(block.until)}).
      </Text>
      <CommandError error={cmd.error} unknown={cmd.unknown} what="revoke" />
    </FormDialog>
  );
}
