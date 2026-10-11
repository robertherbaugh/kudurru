// Disable blocking with a countdown (§10 principle 7): 10 s / 30 s / 5 min
// / custom / indefinite. POST /dns/blocking toggles the node-local switch
// today and the cluster-wide Publisher once replication lands; the UI
// only shows what the server reports back.
import { Button } from '@astryxdesign/core/Button';
import { Dialog } from '@astryxdesign/core/Dialog';
import { StatusDot } from '@astryxdesign/core/StatusDot';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useId, useMemo, useState } from 'react';
import { useCan } from '../api/context';
import { useBlocking, useSetBlocking } from '../api/hooks';
import type { BlockingState } from '../api/types';
import { formatCountdown, parseDuration } from '../lib/format';
import { useCountdown } from '../lib/useCountdown';
import { useNotify } from './Feedback';

/** The preset durations offered when disabling, in seconds (0 = indefinite). */
export const BLOCKING_PRESETS: { label: string; seconds: number }[] = [
  { label: '10 seconds', seconds: 10 },
  { label: '30 seconds', seconds: 30 },
  { label: '5 minutes', seconds: 300 },
  { label: 'Indefinitely', seconds: 0 },
];

/** The instant blocking re-enables, from `until` or from `timer` at fetch time. */
export function resumeAt(state: BlockingState | undefined, fetchedAt: number): string | undefined {
  if (!state || state.blocking) return undefined;
  if (state.until) return state.until;
  if (state.timer && state.timer > 0) return new Date(fetchedAt + state.timer * 1000).toISOString();
  return undefined;
}

export interface BlockingPanelProps {
  state: BlockingState | undefined;
  /** When the state was fetched (ms since epoch), to anchor `timer`. */
  fetchedAt: number;
  canToggle: boolean;
  busy?: boolean;
  /** Disable for `seconds` (0 = indefinitely) or enable (blocking=true). */
  onSet: (v: { blocking: boolean; timer?: number }) => void;
  /** Called when a countdown reaches zero, to refetch the real state. */
  onExpire?: () => void;
  compact?: boolean;
}

/** Presentational blocking switch; BlockingControl wires it to the API. */
export function BlockingPanel({ state, fetchedAt, canToggle, busy, onSet, onExpire, compact }: BlockingPanelProps) {
  const [open, setOpen] = useState(false);
  const [custom, setCustom] = useState('');
  const [customError, setCustomError] = useState<string | undefined>();
  const until = useMemo(() => resumeAt(state, fetchedAt), [state, fetchedAt]);
  const left = useCountdown(until, onExpire);
  const titleId = useId();

  if (!state) {
    return <span className="k-muted k-small">Blocking state unknown</span>;
  }

  const disable = (seconds: number) => {
    onSet(seconds > 0 ? { blocking: false, timer: seconds } : { blocking: false });
    setOpen(false);
  };
  const submitCustom = () => {
    const raw = custom.trim();
    // A bare number means minutes; anything else is a Go duration.
    const seconds = /^\d+(\.\d+)?$/.test(raw) ? Number(raw) * 60 : parseDuration(raw);
    if (!Number.isFinite(seconds) || seconds <= 0) {
      setCustomError('Minutes (e.g. 15) or a duration such as 90s, 20m or 1h30m');
      return;
    }
    setCustomError(undefined);
    disable(Math.round(seconds));
  };

  const status = state.blocking
    ? 'Blocking enabled'
    : left !== undefined
      ? `Blocking disabled, re-enables in ${formatCountdown(left)}`
      : 'Blocking disabled indefinitely';

  return (
    <div className={compact ? 'k-stack' : 'k-row'} data-testid="blocking-panel">
      <span className="k-row" aria-live="polite">
        <StatusDot variant={state.blocking ? 'success' : 'warning'} label={state.blocking ? 'enabled' : 'disabled'} />
        <span>{status}</span>
      </span>
      {state.blocking ? (
        <Button
          label="Disable blocking"
          size="sm"
          variant="secondary"
          isDisabled={!canToggle}
          isLoading={busy}
          tooltip={canToggle ? undefined : 'Needs the blocking.toggle permission'}
          onClick={() => setOpen(true)}
        />
      ) : (
        <Button
          label="Enable blocking"
          size="sm"
          variant="primary"
          isDisabled={!canToggle}
          isLoading={busy}
          onClick={() => onSet({ blocking: true })}
        />
      )}
      <Dialog isOpen={open} onOpenChange={setOpen} purpose="form" width={420} aria-labelledby={titleId}>
        <div className="k-stack-lg">
          <h2 id={titleId} className="k-dialog-title">
            Disable blocking
          </h2>
          <p className="k-muted k-small">
            Queries are answered without block lists until blocking re-enables. Allow lists, zones and policies keep working.
          </p>
          <div className="k-row" role="group" aria-label="Disable for">
            {BLOCKING_PRESETS.map((p) => (
              <Button key={p.label} label={p.label} variant="secondary" onClick={() => disable(p.seconds)} />
            ))}
          </div>
          <form
            className="k-row-end"
            onSubmit={(e) => {
              e.preventDefault();
              submitCustom();
            }}
          >
            <TextInput
              label="Custom duration"
              description="Minutes, or a duration such as 90s or 1h30m"
              value={custom}
              onChange={(v) => setCustom(v)}
              status={customError ? { type: 'error', message: customError } : undefined}
              placeholder="15"
            />
            <Button type="submit" label="Disable" variant="primary" />
          </form>
          <div className="k-row">
            <span className="k-spacer" />
            <Button label="Cancel" variant="ghost" onClick={() => setOpen(false)} />
          </div>
        </div>
      </Dialog>
    </div>
  );
}

/** The blocking switch wired to GET/POST /dns/blocking. */
export function BlockingControl({ compact }: { compact?: boolean }) {
  const q = useBlocking();
  const set = useSetBlocking();
  const can = useCan();
  const notify = useNotify();
  return (
    <BlockingPanel
      state={q.data}
      fetchedAt={q.dataUpdatedAt}
      canToggle={can('blocking', 'toggle')}
      busy={set.isPending}
      compact={compact}
      onExpire={() => void q.refetch()}
      onSet={(v) =>
        set.mutate(v, {
          onSuccess: (s) => notify.success(s.blocking ? 'Blocking enabled' : 'Blocking disabled'),
          onError: (err) => notify.error(err, 'Could not change blocking'),
        })
      }
    />
  );
}
