// Delete a zone with its records (DELETE /zones/{zone}, D148). Opening the
// dialog runs the same request with ?dryRun=true, so it shows exactly what
// goes ("N records in M record sets") or why the server refuses (managed by
// a file, an integration or Kubernetes, with every blocking object; the
// root zone). A zone that still has records needs its name typed to
// confirm.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { List, ListItem } from '@astryxdesign/core/List';
import { Text } from '@astryxdesign/core/Text';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { ApiError } from '../api/client';
import { qk, useApi, useSession } from '../api/context';
import { formatExact } from '../lib/format';
import { errorText, Loading, useNotify } from './Feedback';
import { TextField } from './fields/TextField';
import { FormDialog } from './FormDialog';

export interface ZoneDeleteDialogProps {
  /** The DNSZone object name (the {zone} of the API). */
  zone: string;
  /** The apex, for the title and the typed confirmation. */
  zoneName: string;
  isOpen: boolean;
  onClose: () => void;
  onDeleted?: () => void;
}

const bare = (s: string) => s.trim().replace(/\.$/, '').toLowerCase();

export function ZoneDeleteDialog({ zone, zoneName, isOpen, onClose, onDeleted }: ZoneDeleteDialogProps) {
  const api = useApi();
  const qc = useQueryClient();
  const notify = useNotify();
  const [typed, setTyped] = useState('');
  // The dry run is a DELETE: it needs the session's CSRF token first.
  const session = useSession();
  const preview = useQuery({
    queryKey: ['zones', zone, 'delete-preview'],
    queryFn: () => api.zones.delete(zone, { dryRun: true }),
    enabled: isOpen && !!session.data,
    retry: false,
    gcTime: 0,
  });
  const del = useMutation({
    mutationFn: () => api.zones.delete(zone),
    onSuccess: (r) => {
      notify.success(r.recordSetCount ? `Deleted zone ${zoneName} and ${formatExact(r.recordSetCount)} record sets` : `Deleted zone ${zoneName}`);
      // Drop everything under ['zones', zone] (details, records, this
      // preview) before anything refetches it, then leave the page, then
      // refresh only the zone list: a prefix invalidation would refetch the
      // deleted zone's queries (404) and run the dry-run DELETE again.
      void qc.cancelQueries({ queryKey: qk.zone(zone) });
      qc.removeQueries({ queryKey: qk.zone(zone) });
      setTyped('');
      onDeleted?.();
      onClose();
      void qc.invalidateQueries({ queryKey: qk.zones, exact: true });
    },
  });
  const close = () => {
    setTyped('');
    del.reset();
    onClose();
  };
  const p = preview.data;
  const refused = preview.error instanceof ApiError && preview.error.status === 409 ? preview.error : undefined;
  const needsName = !!p && p.recordSetCount > 0;
  const confirmed = !needsName || bare(typed) === bare(zoneName);

  return (
    <FormDialog
      isOpen={isOpen}
      onClose={close}
      title={`Delete zone ${zoneName}`}
      width={520}
      footer={
        <>
          <Button label="Cancel" variant="ghost" onClick={close} />
          <Button
            label="Delete zone"
            variant="destructive"
            isDisabled={!p || !confirmed || !!refused}
            isLoading={del.isPending}
            onClick={() => del.mutate()}
          />
        </>
      }
    >
      {preview.isLoading ? <Loading label="Checking what the zone holds" /> : null}
      {refused ? (
        <>
          <Banner
            status="warning"
            title="This zone cannot be deleted here"
            description={refused.key === 'default_zone' ? `${refused.message}. ${typeof refused.hint === 'string' ? refused.hint : ''}` : errorText(refused)}
          />
          {refused.objects.length ? (
            <List aria-label="Objects that block the delete">
              {refused.objects.map((o) => (
                <ListItem
                  key={`${o.kind}/${o.namespace ?? ''}/${o.name}`}
                  label={`${o.kind} ${o.namespace ? `${o.namespace}/` : ''}${o.name}`}
                  description={o.managedBy ? `managed by ${o.managedBy}` : undefined}
                />
              ))}
            </List>
          ) : null}
        </>
      ) : null}
      {preview.error && !refused ? <Banner status="error" title="The zone could not be checked" description={errorText(preview.error)} /> : null}
      {p ? (
        <>
          <Text as="p">
            {p.recordSetCount
              ? `The ${p.type} zone ${zoneName} and its ${formatExact(p.endpointCount)} records in ${formatExact(p.recordSetCount)} record sets are deleted. Names in the zone then answer from the upstreams.`
              : `The ${p.type} zone ${zoneName} is deleted. It has no records.`}{' '}
            This cannot be undone; export the zone first to keep a copy.
          </Text>
          {needsName ? (
            <TextField
              label="Type the zone name to confirm"
              mono
              placeholder={bare(zoneName)}
              value={typed}
              onChange={setTyped}
              error={typed && !confirmed ? 'Does not match the zone name' : undefined}
            />
          ) : null}
        </>
      ) : null}
      {del.error ? <Banner status="error" title="Nothing was deleted" description={errorText(del.error)} /> : null}
    </FormDialog>
  );
}
