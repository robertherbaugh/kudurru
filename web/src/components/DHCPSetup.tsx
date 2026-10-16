import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Heading } from '@astryxdesign/core/Heading';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { Section } from '@astryxdesign/core/Section';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { ApiError } from '../api/client';
import { qk, useApi, useCan } from '../api/context';
import type { DHCPSetup, DHCPSetupRequest, DHCPSetupScope } from '../api/types';
import { useNavigate } from '../lib/router';
import { ErrorBanner, useNotify } from './Feedback';
import { FormDialog } from './FormDialog';
import { DHCPAdoptionWizard } from './DHCPAdoptionWizard';
import { Wizard, WizardDialog, type WizardStep } from './Wizard';
import { DHCPDNSFields, DHCPNetworkFields, DHCPReservationFields, DHCPTopologyFields, dnsError, networkError, newDHCPScope, setupScopeError, topologyError } from './DHCPSetupFields';
import { cleanList } from './fields/StringListField';
import { SwitchField } from './fields/TextField';

export function useDHCPSetup() {
  const api = useApi();
  const can = useCan();
  return useQuery({ queryKey: qk.dhcp('setup'), queryFn: api.dhcp.setup, enabled: can('dhcp', 'read'), refetchInterval: 15_000 });
}
export function useSaveDHCPSetup() {
  const api = useApi();
  const qc = useQueryClient();
  const notify = useNotify();
  return useMutation({ mutationFn: ({ base, ...request }: DHCPSetupRequest & { base: DHCPSetup }) => api.dhcp.saveSetup(request, base.etag),
    onSuccess: (next) => { qc.setQueryData(qk.dhcp('setup'), next); notify.success('DHCP setup saved for the next restart'); },
    onError: (error) => { if (error instanceof ApiError && error.status === 412) void qc.invalidateQueries({ queryKey: qk.dhcp('setup') }); },
  });
}
export const setupRequest = (base: DHCPSetup): DHCPSetupRequest & { base: DHCPSetup } => ({ base, enabled: base.enabled, scopes: base.scopes, scopeRevisions: base.scopeRevisions });
export const setupChanged = (a: DHCPSetup, b: DHCPSetup) => a.etag !== b.etag || JSON.stringify(a.scopeRevisions) !== JSON.stringify(b.scopeRevisions);

export function DHCPSetupNotices({ setup }: { setup: DHCPSetup }) {
  return <>
    {!setup.available ? <Banner status="warning" title="Dashboard setup is unavailable" description={setup.unavailableReason} /> : null}
    {setup.environmentLocked ? <Banner status="info" title="DHCP is controlled by environment variables" description="Remove DHCP environment overrides and restart this node before managing setup in the dashboard." /> : null}
    {setup.runtimeError ? <Banner status="error" title="DHCP could not start" description={setup.runtimeError} /> : null}
    {setup.restartRequired ? <Banner status="warning" title="Restart required" description={`Saved setup will ${setup.enabled ? 'enable DHCP with these scopes' : 'disable DHCP'} after this node restarts. The running service has not changed. Live leases and settings revisions are checked again at startup.`} /> : null}
  </>;
}

export function DHCPSetupControls({ setup }: { setup: DHCPSetup }) {
  const can = useCan();
  const save = useSaveDHCPSetup();
  const [adopting, setAdopting] = useState<DHCPSetup | null>(null);
  const [creating, setCreating] = useState(false);
  const [completing, setCompleting] = useState(false);
  const incompleteFile = !setup.enabled && setup.scopes.some((scope) => setupScopeError(scope));
  const writable = setup.available && !setup.environmentLocked && can('dhcp', 'write');
  return <VStack gap={4}>
    <DHCPSetupNotices setup={setup} />
    <HStack gap={4} hAlign="between" wrap="wrap" vAlign="center">
      <SwitchField label="Enable DHCP after restart" value={setup.enabled} onChange={(enabled) => save.mutate({ ...setupRequest(setup), enabled })}
        isDisabled={!writable || setup.managedBy !== 'api' || save.isPending || (!setup.enabled && !setup.scopes.length)}
        help={setup.scopes.length ? 'Enable and disable changes take effect when this node restarts.' : 'Create a scope before enabling DHCP.'} />
      <Button label="New scope" variant="primary" onClick={() => setCreating(true)} isDisabled={!writable || setup.managedBy !== 'api'}
        tooltip={!can('dhcp', 'write') ? 'Requires dhcp.write' : !setup.available ? setup.unavailableReason : setup.environmentLocked ? 'Controlled by environment variables' : setup.managedBy === 'file' ? 'Adopt setup to manage it in the dashboard' : undefined} />
    </HStack>
    {setup.managedBy === 'file' ? <Banner status="info" title="Managed by the configuration file" description="Adopt this node’s current DHCP setup to edit it here. The host YAML stays unchanged; saved dashboard setup takes precedence after restart."
      endContent={<Button label={incompleteFile ? "Complete and adopt setup" : "Manage in dashboard"} isDisabled={!writable} onClick={() => incompleteFile ? setCompleting(true) : setAdopting(setup)} />} /> : null}
    <Text type="supporting">The host must provide the interfaces, IPv4 addresses and DHCP network privileges. Stop any competing DHCP server before activating these scopes.</Text>
    {save.error ? <ErrorBanner error={save.error} /> : null}
    <FormDialog isOpen={!!adopting} onClose={() => setAdopting(null)} title="Manage DHCP in the dashboard"
      footer={<><Button label="Cancel" onClick={() => setAdopting(null)} /><Button label="Adopt setup" variant="primary" isLoading={save.isPending}
        isDisabled={!adopting || setupChanged(setup, adopting)} onClick={() => { if (adopting) save.mutate({ ...setupRequest(adopting), adopt: true }, { onSuccess: () => setAdopting(null) }); }} /></>}>
      <Text>Use the current effective scopes, network options and reservations as the dashboard setup. Existing leases stay in their databases. Adoption takes effect after a restart.</Text>
      {adopting && setupChanged(setup, adopting) ? <Text>The setup changed. Close and reopen this confirmation to review the latest values.</Text> : null}
      {save.error ? <ErrorBanner error={save.error} /> : null}
    </FormDialog>
    <WizardDialog isOpen={completing} onClose={() => setCompleting(false)} title="Complete and adopt DHCP setup" what="dashboard setup">
      <DHCPAdoptionWizard setup={setup} close={() => setCompleting(false)} />
    </WizardDialog>
    <WizardDialog isOpen={creating} onClose={() => setCreating(false)} title="New DHCP scope" what="DHCP scope">
      <DHCPScopeWizard setup={setup} close={() => setCreating(false)} />
    </WizardDialog>
  </VStack>;
}

function DHCPScopeWizard({ setup, close }: { setup: DHCPSetup; close: () => void }) {
  const navigate = useNavigate();
  const [base, setBase] = useState(setup);
  const [draft, setDraft] = useState(newDHCPScope);
  const save = useSaveDHCPSetup();
  const stale = setupChanged(setup, base);
  const fields = { value: draft, onChange: setDraft };
  const steps: WizardStep[] = [
    { id: 'network', label: 'Subnet', validate: () => topologyError(draft) ?? (base.scopes.some((s) => s.name === draft.name) ? 'That scope name already exists.' : undefined), content: <DHCPTopologyFields {...fields} /> },
    { id: 'options', label: 'Addresses', validate: () => networkError(draft), content: <DHCPNetworkFields {...fields} /> },
    { id: 'dns', label: 'DNS', validate: () => dnsError(draft), content: <DHCPDNSFields {...fields} /> },
    { id: 'review', label: 'Review', validate: () => stale ? 'The saved setup changed. Reload the setup before creating this scope.' : setupScopeError(draft), content: <>
      <MetadataList>
        <MetadataListItem label="Scope">{draft.name}</MetadataListItem>
        <MetadataListItem label="Interface">{draft.interface} · {draft.serverIP}</MetadataListItem>
        <MetadataListItem label="Subnet">{draft.subnet} · {draft.relayOnly ? 'Via relay' : 'Direct clients'}</MetadataListItem>
        <MetadataListItem label="Address pool">{draft.rangeStart} – {draft.rangeEnd}</MetadataListItem>
        <MetadataListItem label="Router">{draft.router}</MetadataListItem>
        <MetadataListItem label="DNS">{cleanList(draft.dnsServers).join(', ')} · {draft.domainName || 'No domain'}</MetadataListItem>
      </MetadataList>
      <Text>Save this scope, then add any reservations on its detail page. Restart the node to activate saved setup. {base.enabled ? 'DHCP is already enabled in saved setup.' : 'DHCP stays disabled until you enable it.'}</Text>
    </> },
  ];
  return <Wizard label="New DHCP scope" steps={steps} finishLabel="Save scope" isFinishing={save.isPending} isDirty={draft.name !== '' || draft.interface !== ''}
    onFinish={() => { if (!stale) save.mutate({ ...setupRequest(base), scopes: [...base.scopes, { ...draft, dnsServers: cleanList(draft.dnsServers) }] }, { onSuccess: () => { close(); navigate(`/dhcp/${encodeURIComponent(draft.name)}`); } }); }}
    notice={stale ? <Banner status="warning" title="The saved setup changed" description="Your new scope draft is retained. Reload the setup before continuing."
      endContent={<Button label="Reload setup" onClick={() => { setBase(setup); save.reset(); }} />} /> : undefined}
    error={save.error ? <ErrorBanner error={save.error} /> : undefined} />;
}

export function DHCPSavedScope({ setup, scope, running }: { setup: DHCPSetup; scope: DHCPSetupScope; running: boolean }) {
  const navigate = useNavigate();
  const [editing, setEditing] = useState(!running);
  const [removing, setRemoving] = useState<DHCPSetup | null>(null);
  const can = useCan();
  const save = useSaveDHCPSetup();
  const writable = setup.available && setup.managedBy === 'api' && !setup.environmentLocked && can('dhcp', 'write');
  return <Section padding={0} variant="transparent"><VStack gap={4}>
    <HStack gap={2} wrap="wrap" hAlign="between" vAlign="center"><Heading level={2}>Setup for next restart</Heading><HStack gap={2}>
      <Button label={editing ? 'Hide setup editor' : 'Edit setup'} onClick={() => setEditing(!editing)} />
      <Button label="Remove scope" isDisabled={!writable} onClick={() => setRemoving(setup)} />
    </HStack></HStack>
    <Text type="supporting">{scope.interface} · {scope.subnet} · {setup.pendingScopes.includes(scope.name) ? 'Saved changes pending' : running ? 'Scope is running' : 'Scope is not running'}.</Text>
    {editing ? <SavedScopeEditor setup={setup} scope={scope} writable={writable} /> : null}
    <FormDialog isOpen={!!removing} onClose={() => setRemoving(null)} title={`Remove scope ${scope.name}`}
      footer={<><Button label="Cancel" onClick={() => setRemoving(null)} /><Button label="Remove scope" variant="destructive" isLoading={save.isPending}
        isDisabled={!removing || setupChanged(setup, removing)} onClick={() => { if (removing) { const scopes = removing.scopes.filter((s) => s.name !== scope.name); save.mutate({ ...setupRequest(removing), scopes, enabled: scopes.length > 0 && removing.enabled }, { onSuccess: () => navigate('/dhcp') }); } }} /></>}>
      <Text>Remove this scope from saved setup after restart. Its lease database is retained. Live leases, offers or quarantined addresses can block the next DHCP startup. Removing the last scope also stages disabling DHCP.</Text>
      {removing && setupChanged(setup, removing) ? <Text>The setup changed. Close and reopen this confirmation.</Text> : null}
      {save.error ? <ErrorBanner error={save.error} /> : null}
    </FormDialog>
  </VStack></Section>;
}
function SavedScopeEditor({ setup, scope, writable }: { setup: DHCPSetup; scope: DHCPSetupScope; writable: boolean }) {
  const [base, setBase] = useState(setup);
  const [draft, setDraft] = useState(scope);
  const [error, setError] = useState<string>();
  const save = useSaveDHCPSetup();
  const stale = setupChanged(setup, base);
  const fields = { value: draft, onChange: setDraft, disabled: !writable || save.isPending };
  return <VStack as="form" gap={4} aria-label="Saved scope setup" onSubmit={(e) => { e.preventDefault(); const error = setupScopeError(draft); setError(error); if (!error && !stale) save.mutate({ ...setupRequest(base), scopes: base.scopes.map((s) => s.name === scope.name ? { ...draft, dnsServers: cleanList(draft.dnsServers) } : s) }, { onSuccess: (next) => { setBase(next); setDraft(next.scopes.find((s) => s.name === scope.name) ?? draft); } }); }}>
    <Text type="supporting">Save topology, options and reservations together for the next restart. Existing leases must remain valid for the new setup.</Text>
    {stale ? <Banner status="warning" title="The saved setup changed" description="Your draft is retained. Reload saved setup to use the latest revisions."
      endContent={<Button label="Reload saved setup" onClick={() => { setBase(setup); setDraft(scope); setError(undefined); save.reset(); }} />} /> : null}
    <DHCPTopologyFields {...fields} existing />
    <Heading level={3}>Network options after restart</Heading><DHCPNetworkFields {...fields} />
    <Heading level={3}>DNS after restart</Heading><DHCPDNSFields {...fields} />
    <DHCPReservationFields {...fields} />
    {error ? <Banner status="error" title="Check the scope setup" description={error} /> : null}
    {save.error ? <ErrorBanner error={save.error} /> : null}
    <HStack gap={2}><Button label="Save setup for restart" type="submit" variant="primary" isDisabled={!writable || stale} isLoading={save.isPending} /></HStack>
  </VStack>;
}
