import { HStack } from '@astryxdesign/core/Stack';
import { useQuery } from '@tanstack/react-query';
import type { DHCPSetupScope } from '../api/types';
import { qk, useApi } from '../api/context';
import { ErrorBanner } from './Feedback';
import { DHCPAdvancedFields, advancedOptionsError } from './DHCPAdvancedFields';
import { DurationField, durationError } from './fields/DurationField';
import { SelectField } from './fields/SelectField';
import { ShapeField } from './fields/ShapeField';
import { StringListField, cleanList } from './fields/StringListField';
import { SwitchField, TextField } from './fields/TextField';
import { formatError } from './fields/formats';
import { parseDuration } from '../lib/format';

export const newDHCPScope = (): DHCPSetupScope => ({ name: '', interface: '', serverIP: '', subnet: '', relayOnly: false, trustedRelays: [],
  rangeStart: '', rangeEnd: '', router: '', dnsServers: [], domainName: '', leaseDuration: '12h', registerDNS: false, dnsTTL: '30s', reservations: [] });
const ipError = (v: string) => !v.trim() ? 'An IPv4 address is required' : formatError('ipv4', v);
export function topologyError(scope: DHCPSetupScope): string | undefined {
  if (!/^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(scope.name)) return 'Use a scope name with 1–63 lowercase letters, digits or hyphens, starting and ending with a letter or digit.';
  if (!scope.interface) return 'Choose a host interface.';
  if (ipError(scope.serverIP)) return 'Enter an IPv4 server address.';
  if (!scope.subnet.includes('/') || scope.subnet.includes(':') || formatError('cidr', scope.subnet)) return 'Enter an IPv4 subnet, for example 192.0.2.0/24.';
  if (scope.relayOnly && !scope.trustedRelays.length) return 'Add at least one trusted relay for a relay-only scope.';
  if (scope.trustedRelays.some((r) => ipError(r.sourceIP) || ipError(r.gatewayIP))) return 'Each relay needs an IPv4 source and gateway address.';
  if (scope.trustedRelays.some((r) => (r.linkSelection && ipError(r.linkSelection)) || (r.serverIdentifier && ipError(r.serverIdentifier)) || (r.sourcePort && (!Number.isInteger(r.sourcePort) || r.sourcePort < 1 || r.sourcePort > 65535)))) return 'Relay extensions need valid IPv4 addresses and a source port from 1 to 65535.';
  if ((scope.relayOnly || scope.trustedRelays.length > 0) && scope.conflictDetection === 'arp') return 'Use ICMP conflict detection for scopes with trusted relays.';
  if (scope.probeTimeout && (durationError(scope.probeTimeout, { min: 0.05 }) || parseDuration(scope.probeTimeout) > 0.5 || !Number.isInteger(parseDuration(scope.probeTimeout) * 1000))) return 'Use a probe timeout between 50 and 500 milliseconds, in whole milliseconds.';
}
export function networkError(scope: DHCPSetupScope): string | undefined {
  if ([scope.rangeStart, scope.rangeEnd, scope.router].some((ip) => ipError(ip))) return 'Enter the first address, last address and router as IPv4 addresses.';
  if (!cleanList(scope.dnsServers).length || cleanList(scope.dnsServers).some((ip) => ipError(ip))) return 'Add at least one IPv4 DNS server.';
  const duration = parseDuration(scope.leaseDuration);
  if (durationError(scope.leaseDuration, { min: 60 }) || duration > 30 * 86400 || !Number.isInteger(duration)) return 'Use a lease duration from one minute to 30 days, in whole seconds.';
}
export function dnsError(scope: DHCPSetupScope): string | undefined {
  if (scope.domainName && formatError('domain', scope.domainName)) return 'Enter a valid domain name.';
  if (scope.registerDNS && !scope.domainName.trim()) return 'A domain is required for DNS registration.';
  const ttl = parseDuration(scope.dnsTTL);
  if (durationError(scope.dnsTTL, { min: 1 }) || ttl > 3600 || !Number.isInteger(ttl)) return 'Use a record TTL from one second to one hour, in whole seconds.';
}
export function reservationError(scope: DHCPSetupScope): string | undefined {
  if (scope.reservations.some((r) => !/^(?:[\da-f]{2}:){5}[\da-f]{2}$/i.test(r.mac) || ipError(r.ip))) return 'Each reservation needs an Ethernet MAC and an IPv4 address.';
}
export const setupScopeError = (s: DHCPSetupScope) => topologyError(s) ?? networkError(s) ?? dnsError(s) ?? reservationError(s) ?? advancedOptionsError(s);

type Props = { value: DHCPSetupScope; onChange: (scope: DHCPSetupScope) => void; disabled?: boolean; existing?: boolean; sharedLocked?: boolean };
export function DHCPTopologyFields({ value, onChange, disabled, existing, sharedLocked }: Props) {
  const api = useApi();
  const inventory = useQuery({ queryKey: qk.dhcp('interfaces'), queryFn: api.dhcp.interfaces });
  const change = <K extends keyof DHCPSetupScope>(k: K, v: DHCPSetupScope[K]) => onChange({ ...value, [k]: v });
  const selected = inventory.data?.items.find((i) => i.name === value.interface);
  return <>
    {inventory.error ? <ErrorBanner error={inventory.error} /> : null}
    <TextField label="Scope name" value={value.name} onChange={(v) => change('name', v)} isDisabled={disabled || existing} placeholder="lan" help="A stable name for this subnet and its lease history." />
    <SelectField label="Host interface" value={value.interface} options={(inventory.data?.items ?? []).map((i) => ({ value: i.name, label: i.name,
      detail: `${i.addresses.join(', ') || 'No IPv4 address'} · ${i.up ? 'up' : 'down'}`, disabled: i.loopback }))}
      onChange={(v) => change('interface', v)} isDisabled={disabled} isLoading={inventory.isPending}
      help="The interface must already exist on this node. Direct clients need an active Ethernet interface with broadcast support." />
    <HStack gap={4} wrap="wrap">
      <TextField label="Server address" value={value.serverIP} onChange={(v) => change('serverIP', v)} isDisabled={disabled || sharedLocked} placeholder="192.0.2.2"
        help={selected?.addresses.length ? `Assigned addresses: ${selected.addresses.join(', ')}.` : 'An IPv4 address already assigned to the selected interface.'} />
      <TextField label="Client subnet" value={value.subnet} onChange={(v) => change('subnet', v)} isDisabled={disabled || sharedLocked} placeholder="192.0.2.0/24" help="A network prefix. Scopes cannot overlap." />
    </HStack>
    <SwitchField label="Relay-only scope" value={value.relayOnly} onChange={(v) => change('relayOnly', v)} isDisabled={disabled || sharedLocked} help="Receive requests from trusted relays for a remote subnet." />
    <ShapeField label="Trusted relays" value={value.trustedRelays} onChange={(v) => change('trustedRelays', (v as DHCPSetupScope['trustedRelays']).map((relay) => ({ ...relay, sourcePort: Number(relay.sourcePort || 0) })))} isDisabled={disabled || sharedLocked}
      shape={{ type: 'list', items: { type: 'object', fields: [{ name: 'sourceIP', label: 'Relay source address', type: 'string', format: 'ipv4' }, { name: 'gatewayIP', label: 'Relay gateway address', type: 'string', format: 'ipv4' },
        { name: 'sourcePort', label: 'Relay source port', type: 'int', min: 0, max: 65535, help: '0 uses port 67. A different port explicitly authorizes RFC 8357 replies to that port.' },
        { name: 'linkSelection', label: 'Relay link selection', type: 'string', format: 'ipv4', help: 'Optional exact RFC 3527 client-link address.' },
        { name: 'serverIdentifier', label: 'Relay server identifier', type: 'string', format: 'ipv4', help: 'Optional exact RFC 5107 server identifier override.' }] } }}
      description="Authorize the exact source, gateway and optional extensions of each relay. Configure relays to send to all failover members." />
    <SelectField label="Address conflict detection" value={value.conflictDetection || 'auto'} onChange={(v) => change('conflictDetection', v)} isDisabled={disabled || sharedLocked}
      options={[{ value: 'auto', label: 'Automatic', help: 'ARP on direct links; ICMP when trusted relays are configured.' }, { value: 'arp', label: 'ARP on the local link' }, { value: 'icmp', label: 'ICMP echo', help: 'For routed or relayed networks. Targets and firewalls must allow echo responses.' }, { value: 'disabled', label: 'Disabled', help: 'Provide independent conflict checks before assigning addresses.' }]} />
    <DurationField label="Conflict probe timeout" value={value.probeTimeout || '200ms'} onChange={(v) => change('probeTimeout', v)} isDisabled={disabled || sharedLocked || value.conflictDetection === 'disabled'}
      presets={[{ value: '50ms', label: '50 milliseconds' }, { value: '100ms', label: '100 milliseconds' }, { value: '200ms', label: '200 milliseconds' }, { value: '500ms', label: '500 milliseconds' }]}
      description="50 to 500 whole milliseconds. Conflicts are durably quarantined before trying another address." />
    <ShapeField label="Failover server addresses" value={value.failoverServers ?? []} onChange={(v) => change('failoverServers', v as DHCPSetupScope['failoverServers'])} isDisabled={disabled || sharedLocked}
      shape={{ type: 'list', items: { type: 'object', fields: [{ name: 'nodeID', label: 'Server node ID', type: 'string' }, { name: 'address', label: 'Server IPv4 address', type: 'string', format: 'ipv4' }] } }}
      description="For automatic failover, list all three voter IDs and their server addresses. These addresses are excluded from allocation." />
  </>;
}
export function DHCPNetworkFields({ value, onChange, disabled }: Props) {
  const change = <K extends keyof DHCPSetupScope>(k: K, v: DHCPSetupScope[K]) => onChange({ ...value, [k]: v });
  return <>
    <HStack gap={4} wrap="wrap">
      {(['rangeStart', 'rangeEnd', 'router'] as const).map((key) => <TextField key={key} label={{ rangeStart: 'First address', rangeEnd: 'Last address', router: 'Router' }[key]}
        value={value[key]} onChange={(v) => change(key, v)} isDisabled={disabled} placeholder="192.0.2.100" />)}
    </HStack>
    <StringListField label="DNS servers" value={value.dnsServers} onChange={(v) => change('dnsServers', v)} addLabel="Add DNS server" isDisabled={disabled}
      placeholder="192.0.2.2" itemError={ipError} description="IPv4 DNS servers advertised to clients. Each address must be reachable from their subnet." />
    <DurationField label="Lease duration" value={value.leaseDuration} onChange={(v) => change('leaseDuration', v)} isDisabled={disabled} description="One minute to 30 days." />
    <DHCPAdvancedFields value={value} onChange={onChange} disabled={disabled} />
  </>;
}
export function DHCPDNSFields({ value, onChange, disabled }: Props) {
  return <>
    <TextField label="Domain name" value={value.domainName} onChange={(domainName) => onChange({ ...value, domainName })} isDisabled={disabled} placeholder="lan.home.arpa"
      help="Advertised to clients. Use a dedicated domain for registered lease names; specific configured zones take precedence." />
    <SwitchField label="Register lease names in DNS" value={value.registerDNS} onChange={(registerDNS) => onChange({ ...value, registerDNS })} isDisabled={disabled} help="Serve A and PTR records for active leases." />
    <DurationField label="Lease record TTL" value={value.dnsTTL} onChange={(dnsTTL) => onChange({ ...value, dnsTTL })} isDisabled={disabled} description="One second to one hour, limited by the remaining lease lifetime." />
  </>;
}
export function DHCPReservationFields({ value, onChange, disabled }: Props) {
  return <ShapeField label="Reservations" value={value.reservations} onChange={(v) => onChange({ ...value, reservations: v as DHCPSetupScope['reservations'] })} isDisabled={disabled}
    shape={{ type: 'list', items: { type: 'object', fields: [{ name: 'mac', label: 'MAC address', type: 'string', help: 'For example 02:00:00:00:00:10.' },
      { name: 'ip', label: 'Reserved address', type: 'string', format: 'ipv4' }, { name: 'hostname', label: 'Hostname', type: 'string', help: 'Optional client name.' }] } }}
    description="Reserve an address in this subnet for a specific client. Existing live leases are checked before changes apply." />;
}
