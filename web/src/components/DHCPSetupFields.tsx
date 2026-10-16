import { HStack } from '@astryxdesign/core/Stack';
import { useQuery } from '@tanstack/react-query';
import type { DHCPSetupScope } from '../api/types';
import { qk, useApi } from '../api/context';
import { ErrorBanner } from './Feedback';
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
export const setupScopeError = (s: DHCPSetupScope) => topologyError(s) ?? networkError(s) ?? dnsError(s) ?? reservationError(s);

type Props = { value: DHCPSetupScope; onChange: (scope: DHCPSetupScope) => void; disabled?: boolean; existing?: boolean };
export function DHCPTopologyFields({ value, onChange, disabled, existing }: Props) {
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
      <TextField label="Server address" value={value.serverIP} onChange={(v) => change('serverIP', v)} isDisabled={disabled} placeholder="192.0.2.2"
        help={selected?.addresses.length ? `Assigned addresses: ${selected.addresses.join(', ')}.` : 'An IPv4 address already assigned to the selected interface.'} />
      <TextField label="Client subnet" value={value.subnet} onChange={(v) => change('subnet', v)} isDisabled={disabled} placeholder="192.0.2.0/24" help="A network prefix. Scopes cannot overlap." />
    </HStack>
    <SwitchField label="Relay-only scope" value={value.relayOnly} onChange={(v) => change('relayOnly', v)} isDisabled={disabled} help="Receive requests from trusted relays for a remote subnet." />
    <ShapeField label="Trusted relays" value={value.trustedRelays} onChange={(v) => change('trustedRelays', v as DHCPSetupScope['trustedRelays'])} isDisabled={disabled}
      shape={{ type: 'list', items: { type: 'object', fields: [{ name: 'sourceIP', label: 'Relay source address', type: 'string', format: 'ipv4' }, { name: 'gatewayIP', label: 'Relay gateway address', type: 'string', format: 'ipv4' }] } }}
      description="Authorize the exact source address and gateway address sent by each relay. Leave empty for direct clients only." />
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
