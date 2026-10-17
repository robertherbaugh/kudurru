import { Heading } from '@astryxdesign/core/Heading';
import { HStack } from '@astryxdesign/core/Stack';
import type { DHCPPoolOptions } from '../api/types';
import { ShapeField } from './fields/ShapeField';
import { StringListField } from './fields/StringListField';
import { TextField } from './fields/TextField';
import { formatError } from './fields/formats';

type Props<T extends DHCPPoolOptions> = { value: T; onChange: (value: T) => void; disabled?: boolean; locked?: (key: keyof DHCPPoolOptions) => boolean };
export function advancedOptionsError(v: DHCPPoolOptions): string | undefined {
  if ((v.ntpServers ?? []).some((ip) => formatError('ipv4', ip) || !ip.trim())) return 'Each NTP server needs an IPv4 address.';
  if ((v.domainSearch ?? []).some((domain) => formatError('domain', domain) || !domain.trim())) return 'Each search domain needs a valid domain name.';
  if ((v.classlessRoutes ?? []).some((r) => !r.destination?.includes('/') || r.destination.includes(':') || formatError('cidr', r.destination) || !r.router || formatError('ipv4', r.router))) return 'Each classless route needs an IPv4 destination prefix and router.';
  if (v.bootServer && formatError('ipv4', v.bootServer)) return 'The boot server needs an IPv4 address.';
  if ((v.bootProfiles ?? []).some((p) => !Number.isInteger(p.architecture) || p.architecture < 0 || p.architecture > 65535 || (p.bootServer && formatError('ipv4', p.bootServer)))) return 'Each boot profile needs an architecture from 0 to 65535 and a valid optional IPv4 server.';
  if (new Set((v.bootProfiles ?? []).map((p) => p.architecture)).size !== (v.bootProfiles ?? []).length) return 'Use each boot architecture only once.';
}

// Shared by saved setup and live options so neither path loses extended options.
export function DHCPAdvancedFields<T extends DHCPPoolOptions>({ value, onChange, disabled, locked }: Props<T>) {
  const change = <K extends keyof DHCPPoolOptions>(key: K, v: DHCPPoolOptions[K]) => onChange({ ...value, [key]: v });
  const blocked = (key: keyof DHCPPoolOptions) => disabled || locked?.(key);
  return <>
    <Heading level={3}>Additional client options</Heading>
    <StringListField label="NTP servers" value={value.ntpServers ?? []} onChange={(v) => change('ntpServers', v)} isDisabled={blocked('ntpServers')}
      placeholder="192.0.2.1" itemError={(ip) => formatError('ipv4', ip)} description="Optional IPv4 time servers advertised with option 42." />
    <StringListField label="Search domains" value={value.domainSearch ?? []} onChange={(v) => change('domainSearch', v)} isDisabled={blocked('domainSearch')}
      placeholder="home.arpa" itemError={(domain) => formatError('domain', domain)} description="Ordered DNS suffix search list advertised with option 119." />
    <ShapeField label="Classless routes" value={value.classlessRoutes ?? []} onChange={(v) => change('classlessRoutes', v as DHCPPoolOptions['classlessRoutes'])} isDisabled={blocked('classlessRoutes')}
      shape={{ type: 'list', items: { type: 'object', fields: [{ name: 'destination', label: 'Destination prefix', type: 'string', format: 'cidr', help: 'IPv4 network, for example 198.51.100.0/24.' },
        { name: 'router', label: 'Next-hop router', type: 'string', format: 'ipv4' }] } }} description="Option 121 routes. Include a 0.0.0.0/0 route if clients using these routes also need a default gateway." />
    <Heading level={3}>Network boot</Heading>
    <HStack gap={4} wrap="wrap">
      <TextField label="Boot server address" value={value.bootServer ?? ''} onChange={(v) => change('bootServer', v)} isDisabled={blocked('bootServer')} placeholder="192.0.2.5" help="Optional IPv4 next-server address." />
      <TextField label="Boot server name" value={value.bootServerName ?? ''} onChange={(v) => change('bootServerName', v)} isDisabled={blocked('bootServerName')} placeholder="boot.home.arpa" help="Optional option 66 server name." />
      <TextField label="Boot file" value={value.bootFile ?? ''} onChange={(v) => change('bootFile', v)} isDisabled={blocked('bootFile')} placeholder="ipxe.efi" help="Default boot filename or URL, option 67." />
    </HStack>
    <ShapeField label="Boot architecture profiles" value={value.bootProfiles ?? []} onChange={(v) => change('bootProfiles', (v as NonNullable<DHCPPoolOptions['bootProfiles']>).map((profile) => ({ ...profile, architecture: Number(profile.architecture) })))} isDisabled={blocked('bootProfiles')}
      shape={{ type: 'list', items: { type: 'object', fields: [{ name: 'architecture', label: 'Architecture', type: 'int', min: 0, max: 65535, help: 'Option 93 architecture, for example 0 for BIOS or 7 for EFI x64.' },
        { name: 'bootServer', label: 'Profile server address', type: 'string', format: 'ipv4' }, { name: 'bootServerName', label: 'Profile server name', type: 'string' }, { name: 'bootFile', label: 'Profile boot file', type: 'string' }] } }}
      description="Override boot settings for clients with a matching architecture. Leave an override empty to inherit its default." />
  </>;
}
