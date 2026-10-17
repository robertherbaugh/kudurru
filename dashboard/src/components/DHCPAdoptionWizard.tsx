import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useState } from 'react';
import { ApiError } from '../api/client';
import { useCan } from '../api/context';
import type { DHCPSetup, DHCPSetupScope } from '../api/types';
import { setupChanged, setupRequest, useSaveDHCPSetup } from './DHCPSetup';
import { DHCPDNSFields, DHCPNetworkFields, DHCPReservationFields, DHCPTopologyFields, dnsError, networkError, reservationError, setupScopeError, topologyError } from './DHCPSetupFields';
import { ErrorBanner } from './Feedback';
import { Wizard, useWizardNav, type WizardStep } from './Wizard';
import { SelectField } from './fields/SelectField';
import { cleanList } from './fields/StringListField';

const firstIncomplete = (scopes: DHCPSetupScope[]) => scopes.find((scope) => setupScopeError(scope))?.name ?? scopes[0]?.name ?? '';

// The enclosing WizardDialog owns dismissal. Adoption is one complete setup
// write, so visiting another scope never persists or discards a scope's draft.
export function DHCPAdoptionWizard({ setup, close }: { setup: DHCPSetup; close: () => void }) {
  const can = useCan();
  const save = useSaveDHCPSetup();
  const [base, setBase] = useState(setup);
  const [scopes, setScopes] = useState(() => structuredClone(setup.scopes));
  const [selected, setSelected] = useState(() => firstIncomplete(setup.scopes));
  const scope = scopes.find((s) => s.name === selected);
  const changed = setupChanged(setup, base);
  const conflict = save.error instanceof ApiError && save.error.status === 412;
  const stale = changed || conflict;
  const blocked = !setup.available ? setup.unavailableReason || 'Dashboard setup is unavailable.'
    : setup.environmentLocked ? 'Remove DHCP environment overrides and restart before adopting setup.'
      : !can('dhcp', 'write') ? 'Adopting setup requires dhcp.write permission.'
        : setup.managedBy !== 'file' ? 'This setup is already managed in the dashboard.'
          : setup.enabled ? 'This completion wizard is for disabled file setup.' : undefined;
  const fields = scope ? { value: scope, onChange: (next: DHCPSetupScope) => setScopes((current) => current.map((s) => s.name === selected ? next : s)), disabled: !!blocked || save.isPending } : undefined;
  const incomplete = scopes.filter((s) => setupScopeError(s));
  const validationError = () => blocked ?? (stale ? 'Reload saved setup and review every scope before adopting.' : undefined)
    ?? (!scopes.length ? 'There are no file scopes to adopt.' : incomplete.length ? `Complete every scope before adoption: ${incomplete.map((s) => s.name).join(', ')}.` : undefined);
  const steps: WizardStep[] = [
    { id: 'choose', label: 'Choose scope', validate: () => blocked ?? (!scope ? 'Choose a scope to complete.' : undefined), content: <>
      <Text>Complete each scope before adopting this node’s setup. Switching scopes keeps all your drafts. The host YAML and running service stay unchanged.</Text>
      <SelectField label="Scope to complete" value={selected} onChange={setSelected} isDisabled={!!blocked || save.isPending}
        options={scopes.map((s) => ({ value: s.name, label: s.name, detail: setupScopeError(s) ? 'Needs completion' : 'Ready to adopt' }))} />
    </> },
    { id: 'subnet', label: 'Subnet', validate: () => blocked ?? (scope ? topologyError(scope) : 'Choose a scope.'), content: fields ? <DHCPTopologyFields {...fields} existing /> : null },
    { id: 'addresses', label: 'Addresses', validate: () => blocked ?? (scope ? networkError(scope) : 'Choose a scope.'), content: fields ? <DHCPNetworkFields {...fields} /> : null },
    { id: 'dns', label: 'DNS and reservations', validate: () => blocked ?? (scope ? dnsError(scope) ?? reservationError(scope) : 'Choose a scope.'), content: fields ? <>
      <DHCPDNSFields {...fields} /><DHCPReservationFields {...fields} />
    </> : null },
    { id: 'review', label: 'Review all scopes', validate: validationError, content: <AdoptionReview scopes={scopes} choose={setSelected} /> },
  ];
  return <Wizard label="Complete and adopt DHCP setup" steps={steps} finishLabel="Adopt setup" isFinishing={save.isPending}
    isDirty={JSON.stringify(scopes) !== JSON.stringify(base.scopes)}
    onFinish={() => {
      if (!validationError()) save.mutate({ ...setupRequest(base), adopt: true, scopes: scopes.map((s) => ({ ...s, dnsServers: cleanList(s.dnsServers) })) }, { onSuccess: close });
    }}
    error={<>
      {blocked ? <Banner status="warning" title="Setup cannot be adopted" description={blocked} /> : null}
      {stale ? <Banner status="warning" title="The saved setup changed"
        description="Your scope drafts are retained. Reloading replaces them with the latest saved values; review and complete every scope again before adopting."
        endContent={<Button label="Reload saved setup" isDisabled={!changed || save.isPending} onClick={() => {
          setBase(setup); setScopes(structuredClone(setup.scopes)); setSelected(firstIncomplete(setup.scopes)); save.reset();
        }} />} /> : null}
      {save.error ? <ErrorBanner error={save.error} /> : null}
    </>} />;
}

function AdoptionReview({ scopes, choose }: { scopes: DHCPSetupScope[]; choose: (name: string) => void }) {
  const nav = useWizardNav();
  const incomplete = scopes.filter((scope) => setupScopeError(scope));
  return <VStack gap={4}>
    <Text>Adopt all scopes together. DHCP stays disabled. Saved dashboard setup takes precedence over the host YAML after restart; lease databases are retained.</Text>
    {incomplete.length ? <Banner status="warning" title="Some scopes need completion" description={incomplete.map((s) => s.name).join(', ')} /> : null}
    <MetadataList>
      {scopes.map((scope) => {
        const error = setupScopeError(scope);
        return <MetadataListItem key={scope.name} label={scope.name}>
          <VStack gap={2}>
            <Text>{scope.interface || 'No interface'} · {scope.serverIP || 'No server address'} · {scope.subnet || 'No subnet'}</Text>
            <Text type="supporting">Pool: {scope.rangeStart || '—'} – {scope.rangeEnd || '—'}; router: {scope.router || '—'}; DNS: {cleanList(scope.dnsServers).join(', ') || '—'}; domain: {scope.domainName || 'None'}; lease: {scope.leaseDuration}; DNS registration: {scope.registerDNS ? `enabled, TTL ${scope.dnsTTL}` : 'disabled'}; {scope.reservations.length} reservations; {scope.relayOnly ? 'relay-only' : 'direct clients'}, {scope.trustedRelays.length} trusted relays.</Text>
            {error ? <Text>{error}</Text> : null}
            <HStack gap={2}><Button label={`${error ? 'Complete' : 'Edit'} ${scope.name}`} onClick={() => { choose(scope.name); nav.goTo('choose'); }} /></HStack>
          </VStack>
        </MetadataListItem>;
      })}
    </MetadataList>
  </VStack>;
}
