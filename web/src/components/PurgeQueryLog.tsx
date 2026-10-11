// Purge the query log of this node (DELETE /querylog, querylog.purge):
// every row, or the rows older than an instant, behind a confirmation.
// Rollups (the long-term charts) are kept.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Dialog, DialogHeader } from '@astryxdesign/core/Dialog';
import { Layout, LayoutContent, LayoutFooter } from '@astryxdesign/core/Layout';
import { SegmentedControl, SegmentedControlItem } from '@astryxdesign/core/SegmentedControl';
import { useMutation } from '@tanstack/react-query';
import { useId, useState } from 'react';
import { useApi, useCan } from '../api/context';
import type { PurgeResult } from '../api/types';
import { formatExact } from '../lib/format';
import { errorText, useNotify } from './Feedback';

export function PurgeQueryLog({ onPurged }: { onPurged: (r: PurgeResult) => void }) {
  const api = useApi();
  const can = useCan();
  const notify = useNotify();
  const beforeId = useId();
  const [open, setOpen] = useState(false);
  const [what, setWhat] = useState<'all' | 'before'>('before');
  const [before, setBefore] = useState('');
  const beforeISO = before ? new Date(before).toISOString() : '';
  const m = useMutation({
    mutationFn: () => api.queries.purge(what === 'before' ? beforeISO : undefined),
    onSuccess: (r) => {
      notify.success(`Deleted ${formatExact(r?.deleted ?? 0)} query-log rows${r?.node ? ` on ${r.node}` : ''}`);
      onPurged(r);
      setOpen(false);
    },
  });
  const allowed = can('querylog', 'purge');
  const close = () => {
    m.reset();
    setOpen(false);
  };
  const ready = what === 'all' || !!beforeISO;
  return (
    <>
      <Button label="Purge…" variant="ghost" isDisabled={!allowed} tooltip={allowed ? 'Delete query-log rows' : 'Needs querylog.purge'} onClick={() => setOpen(true)} />
      <Dialog isOpen={open} onOpenChange={(o) => !o && close()} purpose="form" width={480} maxHeight="90dvh">
        <Layout
          header={<DialogHeader title="Purge the query log" onOpenChange={(o) => !o && close()} />}
          content={
            <LayoutContent>
              <div className="k-stack-lg">
                <SegmentedControl label="Rows" value={what} onChange={(v) => setWhat(v === 'all' ? 'all' : 'before')}>
                  <SegmentedControlItem value="before" label="Older than" />
                  <SegmentedControlItem value="all" label="Every row" />
                </SegmentedControl>
                {what === 'before' ? (
                  <label className="k-label" htmlFor={beforeId}>
                    Delete rows before
                    <input id={beforeId} className="k-native-input" type="datetime-local" value={before} onChange={(e) => setBefore(e.target.value)} />
                  </label>
                ) : null}
                <p className="k-small">
                  {what === 'all' ? 'Every logged query of this node is deleted.' : 'Queries logged before this time are deleted.'} The long-term rollups are
                  kept. This cannot be undone.
                </p>
                {m.error ? <Banner status="error" title="Nothing was deleted" description={errorText(m.error)} /> : null}
              </div>
            </LayoutContent>
          }
          footer={
            <LayoutFooter hasDivider>
              <div className="k-row">
                <span className="k-spacer" />
                <Button label="Cancel" variant="ghost" onClick={close} />
                <Button label={what === 'all' ? 'Delete every row' : 'Delete older rows'} variant="destructive" isDisabled={!ready} isLoading={m.isPending} onClick={() => m.mutate()} />
              </div>
            </LayoutFooter>
          }
        />
      </Dialog>
    </>
  );
}
