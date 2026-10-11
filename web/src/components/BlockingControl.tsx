// Disable blocking with a countdown (§10 principle 7): 10 s / 30 s / 5 min
// / indefinite, or a longer duration picked from presets (custom allowed). POST /dns/blocking toggles the node-local switch
// today and the cluster-wide Publisher once replication lands; the UI
// only shows what the server reports back.
import { Button } from '@astryxdesign/core/Button';
import { StatusDot } from '@astryxdesign/core/StatusDot';
import { useMemo, useState } from 'react';
import { useCan } from '../api/context';
import { useBlocking, useSetBlocking } from '../api/hooks';
import type { BlockingState } from '../api/types';
import { formatCountdown, parseDuration } from '../lib/format';
import { Link } from '../lib/router';
import { useCountdown } from '../lib/useCountdown';
import { useNotify } from './Feedback';
import { DurationField, durationPresets } from './fields/DurationField';
import { FormDialog } from './FormDialog';

/** The preset durations offered when disabling, in seconds (0 = indefinite). */
export const BLOCKING_PRESETS: { label: string; seconds: number }[] = [
  { label: '10 seconds', seconds: 10 },
  { label: '30 seconds', seconds: 30 },
  { label: '5 minutes', seconds: 300 },
  { label: 'Indefinitely', seconds: 0 },
];

/** Longer pauses offered in the "Other duration" select. */
export const BLOCKING_DURATIONS = durationPresets(['15m', '30m', '1h', '2h', '6h', '12h', '24h']);

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
  const [custom, setCustom] = useState('15m');
  const [customError, setCustomError] = useState<string | undefined>();
  const until = useMemo(() => resumeAt(state, fetchedAt), [state, fetchedAt]);
  const left = useCountdown(until, onExpire);

  if (!state) {
    return <span className="k-muted k-small">Blocking state unknown</span>;
  }
  if (state.configured === false) {
    // blocking.enabled is false: the switch would have no effect.
    return (
      <div className={compact ? 'k-stack' : 'k-row'} data-testid="blocking-panel">
        <span className="k-row">
          <StatusDot variant="neutral" label="off in settings" />
          <span>Blocking is turned off in the settings</span>
        </span>
        <Link href="/settings" className="k-small">
          Turn on blocking.enabled
        </Link>
      </div>
    );
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
      <FormDialog
        isOpen={open}
        onClose={() => setOpen(false)}
        title="Disable blocking"
        width={420}
        footer={<Button label="Cancel" variant="ghost" onClick={() => setOpen(false)} />}
      >
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
          <DurationField
            label="Other duration"
            description="Custom takes minutes or a duration such as 90s or 1h30m"
            value={custom}
            onChange={(v) => setCustom(v)}
            presets={BLOCKING_DURATIONS}
            error={customError}
          />
          <Button type="submit" label="Disable" variant="primary" />
        </form>
      </FormDialog>
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
