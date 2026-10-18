import { Banner } from '@astryxdesign/core/Banner';
import { VStack } from '@astryxdesign/core/Stack';
import { isIPv4, isIPv6 } from '../../lib/rr';
import { formatError } from '../fields/formats';
import { cleanList, listError, StringListField } from '../fields/StringListField';

function nameserverError(value: string): string | null {
  const name = value.trim().replace(/\.$/, '');
  if (isIPv4(name) || isIPv6(name)) return 'Enter a DNS hostname, such as dns1.example.com.';
  return formatError('domain', name);
}
export function migrationNameserverError(names: string[]): string | null {
  if (!cleanList(names).length) return 'Enter at least one kudurru nameserver DNS name.';
  if (cleanList(names).length > 16) return 'Enter at most 16 nameservers.';
  return listError(names, nameserverError);
}
export function MigrationNameservers({ value, onChange }: { value: string[]; onChange: (v: string[]) => void }) {
  return <VStack gap={3}>
    <StringListField label="Kudurru nameservers" value={value} onChange={onChange} itemError={nameserverError} addLabel="Add nameserver" placeholder="dns1.example.com" description="DNS hostnames that resolve to kudurru's serving addresses. The first name becomes the SOA primary server." />
    <Banner status="info" title="Prepare DNS names before cutover" description="Update address records, registrar/parent delegations and glue separately. This changes apex NS and SOA primary names in the imported zones; it does not shut down the source server." />
  </VStack>;
}
