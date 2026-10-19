// The add/edit dialog for one DHCP reservation, shared by a scope's page
// and the lease list (which reserves a lease's client).
import { Button } from '@astryxdesign/core/Button';
import { Text } from '@astryxdesign/core/Text';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { ApiError } from '../api/client';
import { qk, useApi } from '../api/context';
import type { DHCPConfig, DHCPReservation } from '../api/types';
import { ErrorBanner } from './Feedback';
import { FormDialog } from './FormDialog';
import { TextField } from './fields/TextField';
import { formatError } from './fields/formats';

export const ipError = (v: string) => v.trim() ? formatError('ipv4', v) : 'An IPv4 address is required';
export const hostnameError = (v: string) => !v || (v.length <= 253 && v.replace(/\.$/, '').split('.').every((p) => /^[a-z\d](?:[a-z\d-]{0,61}[a-z\d])?$/i.test(p))) ? undefined : 'Use a hostname such as printer or printer.home.arpa';
function macError(v: string) {
  return /^(?:[\da-f]{2}:){5}[\da-f]{2}$/i.test(v) && (parseInt(v.slice(0, 2), 16) & 1) === 0 && v !== '00:00:00:00:00:00'
    ? undefined : 'Use a unicast Ethernet MAC, for example 02:00:00:00:00:10';
}

export function useRefreshOnConflict(scope: string) {
  const qc = useQueryClient();
  return (error: Error) => {
    if (error instanceof ApiError && error.status === 412) {
      void qc.invalidateQueries({ queryKey: qk.dhcp(scope, 'config') });
    }
  };
}

// reservation edits an existing reservation; initial prefills a new one
// for a known client (its MAC is then fixed too).
export function ReservationDialog({ config, reservation, initial, close, saved }: {
  config: DHCPConfig; reservation?: DHCPReservation; initial?: DHCPReservation; close: () => void; saved: (c: DHCPConfig) => void;
}) {
  const api = useApi();
  const onConflict = useRefreshOnConflict(config.scope);
  const [etag] = useState(config.etag);
  const [draft, setDraft] = useState<DHCPReservation>(reservation ?? initial ?? { mac: '', ip: '', hostname: '' });
  const [submitted, setSubmitted] = useState(false);
  const errors = { mac: macError(draft.mac), ip: ipError(draft.ip) ?? undefined, hostname: hostnameError(draft.hostname) };
  const mutation = useMutation({ onError: onConflict, mutationFn: () => api.dhcp.reserve(config.scope, { ...draft, mac: draft.mac.toLowerCase() }, etag), onSuccess: (next) => { saved(next); close(); } });
  return <FormDialog isOpen onClose={close} title={reservation ? 'Edit reservation' : 'Add reservation'}
    onSubmit={() => { setSubmitted(true); if (!Object.values(errors).some(Boolean)) mutation.mutate(); }}
    footer={<><Button label="Cancel" onClick={close} /><Button label="Save reservation" type="submit" variant="primary" isLoading={mutation.isPending} isDisabled={etag !== config.etag} /></>}>
    {mutation.error ? <ErrorBanner error={mutation.error} /> : null}
    {etag !== config.etag ? <Text>This scope changed. Close and reopen this dialog to use the saved values.</Text> : null}
    <TextField label="MAC address" value={draft.mac} onChange={(mac) => setDraft({ ...draft, mac })} isDisabled={!!(reservation ?? initial)} mono placeholder="02:00:00:00:00:10" error={submitted ? errors.mac : undefined} />
    <TextField label="Reserved address" value={draft.ip} onChange={(ip) => setDraft({ ...draft, ip })} mono placeholder="192.0.2.10" error={submitted ? errors.ip : undefined}
      help="Must be inside this subnet and not held by another client. A client leasing a different address moves to this one when it next renews or reconnects." />
    <TextField label="Hostname" value={draft.hostname} onChange={(hostname) => setDraft({ ...draft, hostname })} placeholder="printer" error={submitted ? errors.hostname : undefined} help="Optional. Takes precedence over the client's hostname." />
  </FormDialog>;
}
