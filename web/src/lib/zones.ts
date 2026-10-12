// Typed choices for DNSZone fields (api/v1alpha1/dnszone_types.go): the
// Technitium-style access modes with what each means for a client, the
// SOA timer defaults and presets, and the zone-name checks the wizard runs.
import type { EnumOption } from '../components/fields/EnumSelect';
import type { AccessMode } from '../api/types';
import { nameError } from './objectSchemas';
import { hostError } from './rr';

export const QUERY_ACCESS: (EnumOption & { value: AccessMode })[] = [
  { value: 'allow', label: 'Everyone', help: 'Any client may query the zone (the default).' },
  { value: 'allowOnlyPrivate', label: 'Private networks only', help: 'RFC 1918 and RFC 4193 addresses, loopback and link-local clients.' },
  { value: 'useNetworkACL', label: 'Clients in the ACL', help: 'The ACL below decides, first match wins; a client it does not match is refused.' },
  { value: 'allowOnlyZoneNameServers', label: 'Zone name servers only', help: "Only the addresses of the zone's own NS records." },
  { value: 'zoneNameServersAndACL', label: 'Name servers and the ACL', help: 'The zone name servers, plus the clients the ACL allows.' },
  { value: 'deny', label: 'Nobody', help: 'Every query is refused; the zone stays defined.' },
];

/** Modes that read the ACL. */
export function usesACL(mode: string): boolean {
  return mode === 'useNetworkACL' || mode === 'zoneNameServersAndACL';
}

/** The CRD defaults of SOASpec and DNSZoneSpec.defaultTTL, in seconds. */
export const SOA_DEFAULTS = { refresh: 3600, retry: 900, expire: 1209600, minimum: 300, defaultTTL: 300 } as const;

export const SOA_PRESETS = {
  refresh: [900, 1800, 3600, 7200, 14400, 86400],
  retry: [300, 600, 900, 1800, 3600],
  expire: [604800, 1209600, 2419200],
  minimum: [60, 300, 900, 3600],
  defaultTTL: [60, 300, 900, 3600, 86400],
} as const;

/** An ACL entry: an address or CIDR, optionally negated with a leading "!". */
export function aclEntryError(s: string, cidr: (v: string) => string | null): string | null {
  return cidr(s.startsWith('!') ? s.slice(1) : s);
}

/** The object name of a zone apex: lower case, no trailing dot ("Home.Arpa." → "home.arpa"). */
export function zoneObjectName(apex: string): string {
  return apex.trim().replace(/\.$/, '').toLowerCase();
}

/** Why an apex cannot become a zone, or null. */
export function apexError(apex: string, existing: string[]): string | null {
  const he = hostError(apex);
  if (he) return he;
  const name = zoneObjectName(apex);
  if (nameError(name)) return 'Use letters, digits, "-" and "." (the object is named after the zone)';
  if (name === 'new') return '"new" is reserved (/zones/new opens the wizard); pick another name';
  if (existing.includes(name)) return `A zone ${name} exists`;
  return null;
}

/** "hostmaster@example.com" → "hostmaster.example.com." (an SOA RNAME); names stay as they are. */
export function rnameOf(v: string): string {
  const t = v.trim();
  const at = t.indexOf('@');
  if (at < 0) return t;
  return `${t.slice(0, at).replace(/\./g, '\\.')}.${t.slice(at + 1).replace(/\.$/, '')}.`;
}
