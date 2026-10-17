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
import { DHCPFailoverEditor } from './DHCPFailover';
import { Wizard, WizardDialog, type WizardStep } from './Wizard';
import { DHCPDNSFields, DHCPNetworkFields, DHCPReservationFields, DHCPTopologyFields, dnsError, isKube, kubeScope, managedText, networkError, newDHCPScope, setupScopeError, topologyError } from './DHCPSetupFields';
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
    onSuccess: (next) => { qc.setQueryData(qk.dhcp('setup'), next); notify.success(isKube(next) ? 'DHCP setup saved; changes apply right away' : 'DHCP setup saved for the next restart'); },
    onError: (error) => { if (error instanceof ApiError && error.status === 412) void qc.invalidateQueries({ queryKey: qk.dhcp('setup') }); },
  });
}
export const setupRequest = (base: DHCPSetup): DHCPSetupRequest & { base: DHCPSetup } => ({ base, enabled: base.enabled, scopes: base.scopes, scopeRevisions: base.scopeRevisions, failover: base.failover });
export const setupChanged = (a: DHCPSetup, b: DHCPSetup) => a.etag !== b.etag || JSON.stringify(a.scopeRevisions) !== JSON.stringify(b.scopeRevisions);

export function DHCPSetupNotices({ setup }: { setup: DHCPSetup }) {
  const kube = isKube(setup);
  return <>
    {!setup.available ? <Banner status="warning" title="Dashboard setup is unavailable" description={setup.unavailableReason} /> : null}
    {!kube && setup.environmentLocked ? <Banner status="info" title="DHCP is controlled by environment variables" description="Remove DHCP environment overrides and restart this node before managing setup in the dashboard." /> : null}
    {setup.runtimeError ? <Banner status={kube ? 'warning' : 'error'} title={kube ? 'DHCP reports an error' : 'DHCP could not start'} description={setup.runtimeError} /> : null}
    {!kube && setup.restartRequired ? <Banner status="warning" title="Restart required" description={`Saved setup will ${setup.enabled ? 'enable DHCP with these scopes' : 'disable DHCP'} after this node restarts. The running service has not changed. Live leases and settings revisions are checked again at startup.`} /> : null}
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
  const kube = isKube(setup);
  return <VStack gap={4}>
    <DHCPSetupNotices setup={setup} />
    <HStack gap={4} hAlign="between" wrap="wrap" vAlign="center">
      {kube ? <SwitchField label="DHCP enabled" value={setup.enabled} isDisabled help="Set dhcp.enabled in the Helm values." />
        : <SwitchField label="Enable DHCP after restart" value={setup.enabled} onChange={(enabled) => save.mutate({ ...setupRequest(setup), enabled })}
          isDisabled={!writable || setup.managedBy !== 'api' || save.isPending || (!setup.enabled && !setup.scopes.length)}
          help={setup.scopes.length ? 'Enable and disable changes take effect when this node restarts.' : 'Create a scope before enabling DHCP.'} />}
      <Button label="New scope" variant="primary" onClick={() => setCreating(true)} isDisabled={!writable || setup.managedBy !== 'api' || setup.failoverInitialized}
        tooltip={!can('dhcp', 'write') ? 'Requires dhcp.write' : setup.failoverInitialized ? 'Scope topology is fixed for this established failover cluster' : !setup.available ? setup.unavailableReason : setup.environmentLocked ? 'Controlled by environment variables' : setup.managedBy === 'file' ? 'Adopt setup to manage it in the dashboard' : undefined} />
    </HStack>
    {!kube && setup.managedBy === 'file' ? <Banner status="info" title="Managed by the configuration file" description="Adopt this node’s current DHCP setup to edit it here. The host YAML stays unchanged; saved dashboard setup takes precedence after restart."
      endContent={<Button label={incompleteFile ? "Complete and adopt setup" : "Manage in dashboard"} isDisabled={!writable} onClick={() => incompleteFile ? setCompleting(true) : setAdopting(setup)} />} /> : null}
    <Text type="supporting">{kube ? `Clients reach kudurru through trusted relays at the LoadBalancer address${setup.serverAddress ? ` ${setup.serverAddress}` : ''}. Stop any competing DHCP server for these subnets. Saved changes apply right away.`
      : 'The host must provide the interfaces, IPv4 addresses and DHCP network privileges. Stop any competing DHCP server before activating these scopes.'}</Text>
    {kube ? null : <DHCPFailoverEditor setup={setup} />}
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
  const [draft, setDraft] = useState(() => kubeScope(setup, newDHCPScope()));
  const save = useSaveDHCPSetup();
  const stale = setupChanged(setup, base);
  const kube = isKube(setup);
  const fields = { value: draft, onChange: setDraft };
  const steps: WizardStep[] = [
    { id: 'network', label: 'Subnet', validate: () => topologyError(kubeScope(base, draft), kube) ?? (base.scopes.some((s) => s.name === draft.name) ? 'That scope name already exists.' : undefined), content: <DHCPTopologyFields {...fields} kube={kube} serverAddress={base.serverAddress} /> },
    { id: 'options', label: 'Addresses', validate: () => networkError(draft), content: <DHCPNetworkFields {...fields} /> },
    { id: 'dns', label: 'DNS', validate: () => dnsError(draft), content: <DHCPDNSFields {...fields} /> },
    { id: 'review', label: 'Review', validate: () => stale ? 'The saved setup changed. Reload the setup before creating this scope.' : setupScopeError(kubeScope(base, draft), kube), content: <>
      <MetadataList>
        <MetadataListItem label="Scope">{draft.name}</MetadataListItem>
        {kube ? <MetadataListItem label="Relays send to">{base.serverAddress}</MetadataListItem> : <MetadataListItem label="Interface">{draft.interface} · {draft.serverIP}</MetadataListItem>}
        <MetadataListItem label="Subnet">{draft.subnet} · {draft.relayOnly ? 'Via relay' : 'Direct clients'}</MetadataListItem>
        <MetadataListItem label="Address pool">{draft.rangeStart} – {draft.rangeEnd}</MetadataListItem>
        <MetadataListItem label="Router">{draft.router}</MetadataListItem>
        <MetadataListItem label="DNS">{cleanList(draft.dnsServers).join(', ')} · {draft.domainName || 'No domain'}</MetadataListItem>
      </MetadataList>
      <Text>{kube ? 'Save this scope, then add any reservations on its detail page. Saved changes apply right away.'
        : `Save this scope, then add any reservations on its detail page. Restart the node to activate saved setup. ${base.enabled ? 'DHCP is already enabled in saved setup.' : 'DHCP stays disabled until you enable it.'}`}</Text>
    </> },
  ];
  return <Wizard label="New DHCP scope" steps={steps} finishLabel="Save scope" isFinishing={save.isPending} isDirty={draft.name !== '' || draft.interface !== ''}
    onFinish={() => { if (!stale) save.mutate({ ...setupRequest(base), scopes: [...base.scopes, { ...kubeScope(base, draft), dnsServers: cleanList(draft.dnsServers) }] }, { onSuccess: () => { close(); navigate(`/dhcp/${encodeURIComponent(draft.name)}`); } }); }}
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
  const kube = isKube(setup);
  // A scope owned elsewhere (e.g. Git through Argo CD) is read-only here.
  const owner = setup.scopeOwners?.[scope.name];
  const writable = setup.available && setup.managedBy === 'api' && !owner && !setup.environmentLocked && can('dhcp', 'write');
  return <Section padding={0} variant="transparent"><VStack gap={4}>
    <HStack gap={2} wrap="wrap" hAlign="between" vAlign="center"><Heading level={2}>{kube ? 'Scope setup' : 'Setup for next restart'}</Heading><HStack gap={2}>
      <Button label={editing ? 'Hide setup editor' : 'Edit setup'} onClick={() => setEditing(!editing)} />
      {owner ? null : <Button label="Remove scope" isDisabled={!writable || setup.failoverInitialized} tooltip={setup.failoverInitialized ? 'Scope topology is fixed for this established failover cluster' : undefined} onClick={() => setRemoving(setup)} />}
    </HStack></HStack>
    <Text type="supporting">{kube ? `${scope.subnet} · ${setup.pendingScopes.includes(scope.name) ? 'Applying saved changes' : running ? 'Scope is running' : 'Scope is not running'}.`
      : `${scope.interface} · ${scope.subnet} · ${setup.pendingScopes.includes(scope.name) ? 'Saved changes pending' : running ? 'Scope is running' : 'Scope is not running'}.`}</Text>
    {owner ? <Banner status="info" title={managedText(owner)} description="This scope is read-only in the dashboard." /> : null}
    {editing ? <SavedScopeEditor setup={setup} scope={scope} writable={writable} /> : null}
    <FormDialog isOpen={!!removing} onClose={() => setRemoving(null)} title={`Remove scope ${scope.name}`}
      footer={<><Button label="Cancel" onClick={() => setRemoving(null)} /><Button label="Remove scope" variant="destructive" isLoading={save.isPending}
        isDisabled={!removing || setupChanged(setup, removing)} onClick={() => { if (removing) { const scopes = removing.scopes.filter((s) => s.name !== scope.name); save.mutate({ ...setupRequest(removing), scopes, enabled: kube || (scopes.length > 0 && removing.enabled) }, { onSuccess: () => navigate('/dhcp') }); } }} /></>}>
      <Text>{kube ? 'Remove this scope? DHCP stops answering for its subnet right away.'
        : 'Remove this scope from saved setup after restart. Its lease database is retained. Live leases, offers or quarantined addresses can block the next DHCP startup. Removing the last scope also stages disabling DHCP.'}</Text>
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
  const kube = isKube(setup);
  const owner = setup.scopeOwners?.[scope.name];
  const fields = { value: draft, onChange: setDraft, disabled: !writable || save.isPending };
  const sharedFields = { ...fields, disabled: fields.disabled || setup.failoverInitialized };
  return <VStack as="form" gap={4} aria-label="Saved scope setup" onSubmit={(e) => { e.preventDefault(); const error = setupScopeError(kubeScope(base, draft), kube); setError(error); if (writable && !save.isPending && !error && !stale) save.mutate({ ...setupRequest(base), scopes: base.scopes.map((s) => s.name === scope.name ? { ...kubeScope(base, draft), dnsServers: cleanList(draft.dnsServers) } : s) }, { onSuccess: (next) => { setBase(next); setDraft(next.scopes.find((s) => s.name === scope.name) ?? draft); } }); }}>
    <Text type="supporting">{kube ? 'Save the subnet, relays, options and reservations together. Changes apply right away; existing leases must remain valid for the new setup.'
      : 'Save topology, options and reservations together for the next restart. Existing leases must remain valid for the new setup.'}</Text>
    {stale ? <Banner status="warning" title="The saved setup changed" description="Your draft is retained. Reload saved setup to use the latest revisions."
      endContent={<Button label="Reload saved setup" onClick={() => { setBase(setup); setDraft(scope); setError(undefined); save.reset(); }} />} /> : null}
    {setup.failoverInitialized ? <Banner status="info" title="Shared failover settings are replicated live" description="Only this node’s interface can change here. Use the running network options and reservations editors to change shared settings through the quorum." /> : null}
    <DHCPTopologyFields {...fields} existing sharedLocked={setup.failoverInitialized} kube={kube} serverAddress={setup.serverAddress} />
    <Heading level={3}>{kube ? 'Network options' : 'Network options after restart'}</Heading><DHCPNetworkFields {...sharedFields} />
    <Heading level={3}>{kube ? 'DNS' : 'DNS after restart'}</Heading><DHCPDNSFields {...sharedFields} />
    <DHCPReservationFields {...sharedFields} />
    {error ? <Banner status="error" title="Check the scope setup" description={error} /> : null}
    {save.error ? <ErrorBanner error={save.error} /> : null}
    <HStack gap={2}><Button label={kube ? 'Save scope' : 'Save setup for restart'} type="submit" variant="primary" isDisabled={!writable || stale} isLoading={save.isPending}
      tooltip={owner ? managedText(owner) : undefined} /></HStack>
  </VStack>;
}
