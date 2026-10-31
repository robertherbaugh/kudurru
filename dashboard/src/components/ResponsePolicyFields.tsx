import { Button } from '@astryxdesign/core/Button';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import type { PolicySpec } from '../api/types';
import { hostError, isIPv4, isIPv6, parse, validate } from '../lib/rr';
import { EnumSelect } from './fields/EnumSelect';
import { formatError } from './fields/formats';
import { StringListField } from './fields/StringListField';
import { TextField } from './fields/TextField';
import type { PolicyFieldsProps } from './PolicyFields';

type Rule = NonNullable<PolicySpec['rewrites']>[number];
const TYPES = ['A', 'AAAA', 'CNAME', 'PTR', 'TXT', 'MX', 'SRV', 'HTTPS', 'SVCB', 'CAA', 'NS', 'NAPTR'].map((value) => ({ value, label: value }));
const DNS64 = [
  { value: 'inherit', label: 'Inherit', help: 'Use the next policy or the global DNS64 prefix.' },
  { value: 'off', label: 'Off', help: 'Disable DNS64 for these clients, even when global settings enable it.' },
  { value: 'custom', label: 'Use a prefix', help: 'Synthesize IPv6 answers for these clients using their NAT64 router prefix.' },
];
const AAAA = [
  { value: 'inherit', label: 'Inherit', help: 'Use the next policy or the global IPv6 filtering setting.' },
  { value: 'on', label: 'Filter IPv6', help: 'Remove IPv6 addresses and hints from forwarded responses. This also prevents DNS64 synthesis.' },
  { value: 'off', label: 'Keep IPv6', help: 'Keep IPv6 responses even when the global setting filters them.' },
];

function prefixError(value: string): string | null {
  if (!value) return null;
  const [addr = '', bits] = value.split('/');
  return isIPv6(addr) && ['32', '40', '48', '56', '64', '96'].includes(bits ?? '') && !formatError('cidr', value)
    ? null : 'Use an IPv6 /32, /40, /48, /56, /64 or /96 network, for example 64:ff9b::/96.';
}

function targetError(type: string, target: string): string | null {
  if (!target.trim()) return 'A target is required.';
  if (/[\r\n\0]/.test(target)) return 'Use one target per row.';
  if (!type) return isIPv4(target) || isIPv6(target) ? null : 'Use an IPv4 or IPv6 address, or choose a record type.';
  const parsed = parse(type, target);
  if (parsed.type === 'raw') return `Enter valid ${type} record data.`;
  return Object.values(validate(parsed.type, parsed.values))[0] ?? null;
}

export function responsePolicyError(spec: PolicySpec): string | null {
  const error = prefixError(spec.response?.dns64Prefix ?? '');
  if (error) return error;
  if ((spec.rewrites?.length ?? 0) > 128) return 'Use at most 128 rewrites.';
  for (const [i, rule] of (spec.rewrites ?? []).entries()) {
    const name = rule.name.replace(/^\*\./, '');
    const nameError = hostError(name);
    if (nameError) return `Rewrite ${i + 1}: ${nameError}`;
    const type = rule.cname ? 'CNAME' : rule.type ?? '';
    const targets = rule.cname ? [rule.cname] : rule.targets ?? [];
    if (!targets.length || targets.length > 16 || (type === 'CNAME' && targets.length !== 1)) return `Rewrite ${i + 1}: use 1 to 16 targets, or one CNAME target.`;
    for (const target of targets) {
      const problem = targetError(type, target);
      if (problem) return `Rewrite ${i + 1}: ${problem}`;
    }
  }
  return null;
}

export function responsePolicySummary(spec: PolicySpec): string {
  const prefix = spec.response?.dns64Prefix;
  const filter = spec.response?.filterAAAA;
  return `${spec.rewrites?.length ?? 0} rewrite${spec.rewrites?.length === 1 ? '' : 's'} · DNS64 ${prefix === undefined ? 'inherited' : prefix || 'off'} · IPv6 ${filter === undefined ? 'inherited' : filter ? 'filtered' : 'kept'}`;
}

export function ResponsePolicyFields({ spec, onChange, isDisabled }: PolicyFieldsProps) {
  const response = spec.response ?? {};
  const rules = spec.rewrites ?? [];
  const setRule = (i: number, rule: Rule) => onChange({ ...spec, rewrites: rules.map((r, n) => n === i ? rule : r) });
  return (
    <VStack gap={4}>
      <EnumSelect label="DNS64 for these clients" value={response.dns64Prefix === undefined ? 'inherit' : response.dns64Prefix === '' ? 'off' : 'custom'} options={DNS64} isDisabled={isDisabled}
        onChange={(v) => onChange({ ...spec, response: { ...response, dns64Prefix: v === 'inherit' ? undefined : v === 'off' ? '' : '64:ff9b::/96' } })} />
      {response.dns64Prefix ? <TextField label="Client DNS64 prefix" value={response.dns64Prefix} placeholder="64:ff9b::/96" isDisabled={isDisabled} error={prefixError(response.dns64Prefix) ?? undefined}
        help="Use a canonical network with no host bits. Clients requesting DNSSEC proofs keep the original denial."
        onChange={(v) => onChange({ ...spec, response: { ...response, dns64Prefix: v } })} /> : null}
      <EnumSelect label="IPv6 answers for these clients" value={response.filterAAAA === undefined ? 'inherit' : response.filterAAAA ? 'on' : 'off'} options={AAAA} isDisabled={isDisabled}
        onChange={(v) => onChange({ ...spec, response: { ...response, filterAAAA: v === 'inherit' ? undefined : v === 'on' } })} />
      <Text type="supporting">Rewrites replace forwarded answers for these clients and remain active while ad blocking is paused. Owned zones take precedence. CNAME rules return an alias for the client to follow.</Text>
      {rules.map((rule, i) => {
        const type = rule.cname ? 'CNAME' : rule.type ?? '';
        return (
          <VStack key={i} gap={2} role="group" aria-label={`Rewrite ${i + 1}`}>
            <HStack gap={2} vAlign="center">
              <Text weight="semibold">Rewrite {i + 1}</Text>
              <Button label={`Remove rewrite ${i + 1}`} variant="ghost" size="sm" isDisabled={isDisabled} onClick={() => onChange({ ...spec, rewrites: rules.filter((_, n) => n !== i) })} />
            </HStack>
            <TextField label={`Rewrite ${i + 1} name`} value={rule.name} placeholder="host.example or *.example" isDisabled={isDisabled} help="Exact name, or *.suffix for descendants only. The most specific rule wins within this policy."
              onChange={(name) => setRule(i, { ...rule, name })} error={rule.name ? hostError(rule.name.replace(/^\*\./, '')) ?? undefined : undefined} />
            <EnumSelect label={`Rewrite ${i + 1} type`} value={type} emptyLabel="Infer address families" options={TYPES} isDisabled={isDisabled}
              onChange={(type) => setRule(i, { name: rule.name, type, targets: rule.cname ? [rule.cname] : rule.targets })} />
            <StringListField label={`Rewrite ${i + 1} targets`} value={rule.cname ? [rule.cname] : rule.targets ?? []} isDisabled={isDisabled} allowPaste={false}
              description={type === 'CNAME' ? 'One canonical target name.' : 'One address or DNS presentation-format value per row. Quote TXT values, for example "hello".'}
              itemError={(value) => targetError(type, value)} onChange={(targets) => setRule(i, { name: rule.name, type, targets })} />
          </VStack>
        );
      })}
      <Button label="Add rewrite" variant="secondary" isDisabled={isDisabled || rules.length >= 128} onClick={() => onChange({ ...spec, rewrites: [...rules, { name: '', type: 'A', targets: [''] }] })} />
    </VStack>
  );
}
