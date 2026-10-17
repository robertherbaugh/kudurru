// New upstream group, in a WizardDialog over the Upstreams page (?new=1)
// or a missing group's page: mode (forward or recursive) → provider (a
// public resolver preset or Custom) → servers (the preset's filtering level
// and transport fill the list; Custom or "Edit the list" edits it row by
// row) → name, strategy and timeout → review → PATCH /settings
// {upstreams:{groups:{name: …}}}, then the dialog closes and the group's
// page opens. A recursive group has no provider, servers or strategy, so it
// goes from the mode straight to its name and timeout. Presets live in
// lib/upstreamPresets.ts with the provider documentation they were checked
// against.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { MetadataList, MetadataListItem } from '@astryxdesign/core/MetadataList';
import { Link } from '@astryxdesign/core/Link';
import { List, ListItem } from '@astryxdesign/core/List';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useMemo, useState } from 'react';
import { useCan } from '../../api/context';
import { errorText } from '../Feedback';
import { DialogForm } from '../FormDialog';
import { ChoiceCards } from '../fields/ChoiceCards';
import { EnumSelect } from '../fields/EnumSelect';
import { SwitchField, TextField } from '../fields/TextField';
import { ProviderIcon } from '../ProviderIcon';
import { GroupFields, groupFieldErrors, ServersField, strategyLabel, useUpstreams, withMode } from '../UpstreamGroup';
import { Wizard, WizardDialog, type WizardStep } from '../Wizard';
import { useNavigate } from '../../lib/router';
import { groupNameFor, PROVIDERS, serversFor, TRANSPORTS, transportsOf, type Provider, type Transport } from '../../lib/upstreamPresets';
import { groupNameError, groupNames, groupPatch, splitServerName, UPSTREAM_MODES, type UpstreamGroupConfig, type UpstreamMode } from '../../lib/upstreams';

const CUSTOM = 'custom';

/** The name the wizard suggests for a recursive group. */
const RECURSIVE_NAME = 'resolver';

/** Why a new group name cannot be used, or null. */
export function newGroupNameError(name: string, existing: string[]): string | null {
  const e = groupNameError(name);
  if (e) return e;
  if (name === 'new') return '"new" is reserved (/upstreams/new opens this wizard); pick another name';
  return existing.includes(name) ? 'A group with this name exists' : null;
}

export interface UpstreamWizardProps {
  /** Closes the dialog without asking (after the group was created). */
  onClose: () => void;
}

export function UpstreamWizardDialog({ isOpen, onClose }: UpstreamWizardProps & { isOpen: boolean }) {
  return (
    <WizardDialog
      isOpen={isOpen}
      onClose={onClose}
      title="New upstream group"
      subtitle="How a policy, a forward zone or a conditional rule resolves names: through servers you pick, or recursively from the root servers."
      what="upstream group"
    >
      <UpstreamWizard onClose={onClose} />
    </WizardDialog>
  );
}

function UpstreamWizard({ onClose }: UpstreamWizardProps) {
  const can = useCan();
  const navigate = useNavigate();
  const u = useUpstreams();
  const existing = groupNames(u.cfg);
  const [choice, setChoice] = useState('');
  const [variantId, setVariantId] = useState('');
  const [transport, setTransport] = useState<Transport>('doh');
  const [ipv6, setIPv6] = useState(false);
  const [profile, setProfile] = useState('');
  const [editing, setEditing] = useState(false);
  const [custom, setCustom] = useState<string[]>([]);
  const [name, setName] = useState('');
  const [nameTouched, setNameTouched] = useState(false);
  const [group, setGroup] = useState<UpstreamGroupConfig>({ mode: 'forward', servers: [], strategy: 'parallel_best', timeout: '2s' });
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  const provider: Provider | undefined = PROVIDERS.find((p) => p.id === choice);
  const variant = provider?.variants.find((v) => v.id === variantId) ?? provider?.variants[0];
  const transports = variant ? transportsOf(variant) : [];
  const t = transports.includes(transport) ? transport : (transports[0] ?? 'doh');
  const presetServers = useMemo(
    () => (provider && variant ? serversFor(provider, variant, t, { ipv6, id: profile.trim() }) : []),
    [provider, variant, t, ipv6, profile],
  );
  const recursive = group.mode === 'recursive';
  const servers = recursive ? [] : !provider || editing ? custom : presetServers;
  const suggested = recursive ? (existing.includes(RECURSIVE_NAME) ? '' : RECURSIVE_NAME) : provider && variant ? groupNameFor(provider, variant) : '';
  const groupName = nameTouched ? name : name || suggested;

  const pickProvider = (id: string) => {
    setChoice(id);
    const p = PROVIDERS.find((x) => x.id === id);
    setVariantId(p?.variants[0]?.id ?? '');
    setEditing(false);
    if (!nameTouched) setName('');
    if (id === CUSTOM) setCustom([]);
  };

  const create = async () => {
    const cfg = groupPatch({ ...group, servers });
    setBusy(true);
    setError(undefined);
    try {
      await u.save({ groups: { [groupName]: cfg } }, `Group ${groupName}`);
      onClose();
      navigate(`/upstreams/${encodeURIComponent(groupName)}`);
    } catch (e) {
      setError(errorText(e));
    } finally {
      setBusy(false);
    }
  };

  const choices = [
    ...PROVIDERS.map((p) => ({ value: p.id, label: p.name, description: p.description, icon: <ProviderIcon name={p.name} ids={p.icons} /> })),
    { value: CUSTOM, label: 'Custom', description: 'Enter the servers yourself: a router, a Pi-hole, a corporate resolver or any DoT/DoH/DoQ URL.', icon: <ProviderIcon name="Custom" ids={[]} /> },
  ];

  const modeStep: WizardStep = {
    id: 'mode',
    label: 'Mode',
    description: 'Forward to resolvers you trust, or let kudurru resolve names itself.',
    content: (
      <ChoiceCards
        label="Mode"
        isLabelHidden
        value={group.mode}
        choices={UPSTREAM_MODES.map((m) => ({ value: m.value, label: m.label, description: m.help }))}
        onChange={(m) => setGroup(withMode(group, m as UpstreamMode))}
      />
    ),
  };

  const forwardSteps: WizardStep[] = [
    {
      id: 'provider',
      label: 'Provider',
      description: 'A public resolver fills in its documented addresses; Custom takes any server.',
      validate: () => (choice ? null : 'Pick a provider or Custom.'),
      content: <ChoiceCards label="Provider" isLabelHidden value={choice} choices={choices} onChange={pickProvider} />,
    },
    {
      id: 'servers',
      label: 'Servers',
      description: provider ? `${provider.name}: choose the filtering and how kudurru talks to it.` : 'One entry per server.',
      validate: () => {
        if (provider?.needsID && !provider.needsID.pattern.test(profile.trim())) return `${provider.needsID.label}: ${provider.needsID.help}`;
        return groupFieldErrors({ ...group, servers }).servers ?? null;
      },
      content: (
        <>
          {provider && variant ? (
            <>
              {provider.variants.length > 1 ? (
                <EnumSelect label="Filtering" value={variant.id} options={provider.variants.map((v) => ({ value: v.id, label: v.label, help: v.help }))} onChange={setVariantId} />
              ) : (
                <Text>
                  <Text weight="semibold">{variant.label}</Text> <Text color="secondary">{variant.help}</Text>
                </Text>
              )}
              {provider.needsID ? (
                <TextField
                  label={provider.needsID.label}
                  mono
                  placeholder={provider.needsID.placeholder}
                  value={profile}
                  onChange={setProfile}
                  isRequired
                  help={provider.needsID.help}
                  error={profile.trim() && !provider.needsID.pattern.test(profile.trim()) ? 'Six letters or digits' : undefined}
                />
              ) : null}
              <EnumSelect
                label="Transport"
                value={t}
                options={TRANSPORTS.filter((x) => transports.includes(x.value)).map((x) => ({ value: x.value, label: x.label, help: x.help }))}
                onChange={(v) => setTransport(v as Transport)}
              />
              {t === 'plain' && variant.ipv6?.length ? <SwitchField label="Also use the IPv6 addresses" help="Only when this network has IPv6" value={ipv6} onChange={setIPv6} /> : null}
              {t !== 'plain' ? (
                <Text type="supporting">
                  Host names are looked up with the bootstrap resolvers ({u.cfg.bootstrap.join(', ') || 'the system resolver'}), set on the Upstreams page.
                </Text>
              ) : null}
              {editing ? (
                <ServersField value={custom} onChange={setCustom} />
              ) : (
                <VStack gap={1}>
                  <List
                    density="compact"
                    header={
                      <Text type="label" weight="semibold">
                        Servers
                      </Text>
                    }
                    data-testid="preset-servers"
                  >
                    {presetServers.map((s) => {
                      const { name: n, addr } = splitServerName(s);
                      return <ListItem key={s} label={n} description={<Text type="code">{addr}</Text>} />;
                    })}
                  </List>
                  <HStack gap={2}>
                    <Button
                      label="Edit the list"
                      size="sm"
                      variant="ghost"
                      onClick={() => {
                        setCustom(presetServers);
                        setEditing(true);
                      }}
                    />
                  </HStack>
                </VStack>
              )}
              <Text type="supporting">
                Addresses as documented at{' '}
                <Link href={provider.homepage} isExternalLink>
                  {provider.homepage}
                </Link>
                .
              </Text>
            </>
          ) : (
            <ServersField value={custom} onChange={setCustom} />
          )}
        </>
      ),
    },
  ];

  const steps: WizardStep[] = [
    modeStep,
    ...(recursive ? [] : forwardSteps),
    {
      id: 'group',
      label: 'Group',
      description: recursive
        ? 'How policies, zones and conditional rules refer to it, and how long a lookup may take.'
        : 'How policies, zones and conditional rules refer to it, and how its servers are asked.',
      validate: () => newGroupNameError(groupName, existing) ?? groupFieldErrors({ ...group, servers }).timeout ?? null,
      content: (
        <>
          <TextField
            label="Name"
            mono
            value={groupName}
            onChange={(v) => {
              setNameTouched(true);
              setName(v);
            }}
            placeholder="corp"
            isRequired
            help="Letters, digits, - _ and ."
            error={groupName ? (newGroupNameError(groupName, existing) ?? undefined) : undefined}
          />
          <GroupFields value={{ ...group, servers }} onChange={(g) => setGroup({ ...g, servers: group.servers })} serversHidden modeHidden />
        </>
      ),
    },
    {
      id: 'review',
      label: 'Review',
      content: (
        <MetadataList data-testid="upstream-review">
          <MetadataListItem label="Name">
            <Text type="code">{groupName}</Text>
          </MetadataListItem>
          <MetadataListItem label="Mode">{recursive ? `Recursive, from the root servers${u.cfg.recursion.dnssec ? ' with DNSSEC validation' : ''}` : 'Forward'}</MetadataListItem>
          {recursive ? null : (
            <MetadataListItem label="Provider">
              {provider ? `${provider.name}${variant && provider.variants.length > 1 ? `, ${variant.label}` : ''}, ${TRANSPORTS.find((x) => x.value === t)?.label}` : 'Custom'}
            </MetadataListItem>
          )}
          {recursive ? null : (
            <MetadataListItem label="Servers">
              <VStack gap={1}>
                {servers.map((s) => (
                  <Text key={s} type="code">
                    {s}
                  </Text>
                ))}
              </VStack>
            </MetadataListItem>
          )}
          {recursive ? null : <MetadataListItem label="Strategy">{strategyLabel(group.strategy)}</MetadataListItem>}
          <MetadataListItem label="Timeout">{recursive ? `${group.timeout} per lookup` : group.timeout}</MetadataListItem>
        </MetadataList>
      ),
    },
  ];

  if (!can('settings', 'write') || u.readOnlyReason) {
    return (
      <DialogForm footer={<Button label="Close" variant="secondary" onClick={onClose} />}>
        <Banner status="info" title="Read-only" description={u.readOnlyReason ?? 'Creating upstream groups needs the settings.write permission.'} />
      </DialogForm>
    );
  }

  return (
    <Wizard
      label="New upstream group"
      steps={steps}
      finishLabel="Create group"
      isFinishing={busy}
      onFinish={create}
      isDirty={group.mode !== 'forward' || choice !== '' || name !== '' || profile !== '' || custom.length > 0}
      error={error ? <Banner status="error" title="The group could not be created" description={error} /> : null}
    />
  );
}
