import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { VStack } from '@astryxdesign/core/Stack';
import { useQuery } from '@tanstack/react-query';
import { useApi, useCan } from '../../api/context';
import { isIPv4, isIPv6 } from '../../lib/rr';
import { formatError } from '../fields/formats';
import { SelectField } from '../fields/SelectField';
import { cleanList, listError, StringListField } from '../fields/StringListField';

export interface NameserverSelection {
  mode: 'names' | 'addresses';
  names: string[];
  addresses: string[];
}
function nameserverError(value: string): string | null {
  const name = value.trim().replace(/\.$/, '');
  if (isIPv4(name) || isIPv6(name) || name.length > 253 || name.split('.').some((label) => !/^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/i.test(label))) return 'Enter a DNS hostname, such as dns1.example.com.';
  return formatError('domain', name);
}
function addressError(value: string): string | null {
  return formatError('ip', value.trim());
}
/** Suggest the destination shown in the browser, never the source server. */
export function initialNameservers(hostname = window.location.hostname): NameserverSelection {
  const host = hostname.toLowerCase().replace(/^\[|\]$/g, '').replace(/\.$/, '');
  const ip = isIPv4(host) || isIPv6(host);
  const usable = !ip && host !== 'localhost' && !host.endsWith('.localhost') && !nameserverError(host);
  return { mode: ip ? 'addresses' : 'names', names: usable ? [host] : [''], addresses: ip ? [host] : [''] };
}
export function nameserverRequest(value: NameserverSelection) {
  return { nameservers: value.mode === 'names' ? cleanList(value.names) : [], nameserverAddresses: value.mode === 'addresses' ? cleanList(value.addresses) : [] };
}
export function migrationNameserverError(value: NameserverSelection): string | null {
  const values = value.mode === 'names' ? value.names : value.addresses;
  if (!cleanList(values).length) return value.mode === 'names' ? 'Enter at least one kudurru nameserver DNS name.' : 'Enter at least one kudurru serving IP or VIP.';
  if (cleanList(values).length > 16) return 'Enter at most 16 nameservers.';
  return listError(values, value.mode === 'names' ? nameserverError : addressError);
}
export function MigrationNameservers({ value, onChange }: { value: NameserverSelection; onChange: (v: NameserverSelection) => void }) {
  const api = useApi();
  const can = useCan();
  const vips = useQuery({ queryKey: ['migration-vips'], queryFn: () => api.cluster.edge(), enabled: value.mode === 'addresses' && can('edge', 'read'), retry: false });
  const addresses = [...new Set((vips.data?.vips ?? []).map((v) => (v.address ?? '').split('/')[0] ?? '').filter((a) => a && !addressError(a)))].sort();
  return <VStack gap={3}>
    <SelectField label="Advertise kudurru using" value={value.mode} options={[{ value: 'names', label: 'DNS names', help: 'Starts with the hostname you used to open this dashboard. Confirm it resolves directly to kudurru on UDP and TCP port 53; dashboard proxies may use a different address.' }, { value: 'addresses', label: 'Serving IPs or VIPs', help: 'Create names within each imported zone with matching A/AAAA records. The first address supplies the SOA primary name.' }]} onChange={(mode) => onChange({ ...value, mode: mode as NameserverSelection['mode'] })} />
    {value.mode === 'names' ? <StringListField label="Kudurru nameservers" value={value.names} onChange={(names) => onChange({ ...value, names })} itemError={nameserverError} addLabel="Add nameserver" placeholder="dns1.example.com" description="DNS hostnames that resolve to kudurru's serving addresses. The first name becomes the SOA primary server." /> : <>
      <StringListField label="Kudurru serving IPs or VIPs" value={value.addresses} onChange={(ips) => onChange({ ...value, addresses: ips })} itemError={addressError} addLabel="Add address" placeholder="192.0.2.53" description="IPv4 or IPv6 addresses serving DNS on UDP and TCP port 53. Review the generated names and address records in the next step." />
      {addresses.length ? <Button label="Use detected VIPs" variant="secondary" onClick={() => onChange({ ...value, addresses })} /> : null}
      {vips.error ? <Banner status="info" title="VIP discovery unavailable" description="Enter kudurru's serving IPs or VIPs above." /> : null}
    </>}
    <Banner status="info" title="Prepare delegation before cutover" description={value.mode === 'names' ? 'Update address records, registrar/parent delegations and glue separately. This changes apex NS and SOA primary names in the imported zones; it does not shut down the source server.' : 'Kudurru creates the generated nameserver address records. Update registrar/parent delegations and glue separately, and verify the selected addresses serve DNS before retiring the source.'} />
  </VStack>;
}
