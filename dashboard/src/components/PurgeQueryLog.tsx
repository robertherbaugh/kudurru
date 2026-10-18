// Purge the query log of this node (DELETE /querylog, querylog.purge):
// every row, or the rows older than an instant, behind a confirmation.
// Rollups (the long-term charts) are kept.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { SegmentedControl, SegmentedControlItem } from '@astryxdesign/core/SegmentedControl';
import { Text } from '@astryxdesign/core/Text';
import { useMutation } from '@tanstack/react-query';
import { DateTimeInput, type ISODateTimeString } from '@astryxdesign/core/DateTimeInput';
import { useState } from 'react';
import { useApi, useCan } from '../api/context';
import type { PurgeResult } from '../api/types';
import { formatExact } from '../lib/format';
import { errorText, useNotify } from './Feedback';
import { FormDialog } from './FormDialog';

export function PurgeQueryLog({ onPurged }: { onPurged: (r: PurgeResult) => void }) {
  const api = useApi();
  const can = useCan();
  const notify = useNotify();
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
      <FormDialog
        isOpen={open}
        onClose={close}
        title="Purge the query log"
        width={480}
        footer={
          <>
            <Button label="Cancel" variant="ghost" onClick={close} />
            <Button label={what === 'all' ? 'Delete every row' : 'Delete older rows'} variant="destructive" isDisabled={!ready} isLoading={m.isPending} onClick={() => m.mutate()} />
          </>
        }
      >
        <SegmentedControl label="Rows" value={what} onChange={(v) => setWhat(v === 'all' ? 'all' : 'before')}>
          <SegmentedControlItem value="before" label="Older than" />
          <SegmentedControlItem value="all" label="Every row" />
        </SegmentedControl>
        {what === 'before' ? (
          <DateTimeInput label="Delete rows before" value={(before || undefined) as ISODateTimeString | undefined} onChange={(v) => setBefore(v ?? '')} hasClear />
        ) : null}
        <Text type="supporting" color="primary" as="p">
          {what === 'all' ? 'Every logged query of this node is deleted.' : 'Queries logged before this time are deleted.'} The long-term rollups are
          kept. This cannot be undone.
        </Text>
        {m.error ? <Banner status="error" title="Nothing was deleted" description={errorText(m.error)} /> : null}
      </FormDialog>
    </>
  );
}
