// New upstream group, in a WizardDialog over the Upstreams page (?new=1)
// or a missing group's page: provider (a public resolver preset or Custom)
// → servers (the preset's filtering level and transport fill the list;
// Custom or "Edit the list" edits it row by row) → name, strategy and
// timeout → review → PATCH /settings {upstreams:{groups:{name: …}}}, then
// the dialog closes and the group's page opens. Presets live in
// lib/upstreamPresets.ts with the provider documentation they were checked
// against.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { Switch } from '@astryxdesign/core/Switch';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useMemo, useState } from 'react';
import { useCan } from '../../api/context';
import { errorText } from '../Feedback';
import { DialogForm } from '../FormDialog';
import { ChoiceCards } from '../fields/ChoiceCards';
import { EnumSelect } from '../fields/EnumSelect';
import { ProviderIcon } from '../ProviderIcon';
import { GroupFields, groupFieldErrors, ServersField, strategyLabel, useUpstreams } from '../UpstreamGroup';
import { Wizard, WizardDialog, type WizardStep } from '../Wizard';
import { useNavigate } from '../../lib/router';
import { groupNameFor, PROVIDERS, serversFor, TRANSPORTS, transportsOf, type Provider, type Transport } from '../../lib/upstreamPresets';
import { groupNameError, groupNames, splitServerName, type UpstreamGroupConfig } from '../../lib/upstreams';

const CUSTOM = 'custom';

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
    <WizardDialog isOpen={isOpen} onClose={onClose} title="New upstream group" subtitle="The resolvers a policy, a forward zone or a conditional rule forwards to." what="upstream group">
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
  const [group, setGroup] = useState<UpstreamGroupConfig>({ servers: [], strategy: 'parallel_best', timeout: '2s' });
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
  const servers = !provider || editing ? custom : presetServers;
  const suggested = provider && variant ? groupNameFor(provider, variant) : '';
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
    const cfg = { ...group, servers: servers.map((s) => s.trim()).filter(Boolean) };
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

  const steps: WizardStep[] = [
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
                <p className="k-small">
                  <strong>{variant.label}</strong> <span className="k-muted">{variant.help}</span>
                </p>
              )}
              {provider.needsID ? (
                <VStack gap={1}>
                  <TextInput
                    label={provider.needsID.label}
                    placeholder={provider.needsID.placeholder}
                    value={profile}
                    onChange={setProfile}
                    isRequired
                    status={profile.trim() && !provider.needsID.pattern.test(profile.trim()) ? { type: 'error', message: 'Six letters or digits' } : undefined}
                  />
                  <Text type="supporting" display="block">
                    {provider.needsID.help}
                  </Text>
                </VStack>
              ) : null}
              <EnumSelect
                label="Transport"
                value={t}
                options={TRANSPORTS.filter((x) => transports.includes(x.value)).map((x) => ({ value: x.value, label: x.label, help: x.help }))}
                onChange={(v) => setTransport(v as Transport)}
              />
              {t === 'plain' && variant.ipv6?.length ? <Switch label="Also use the IPv6 addresses" description="Only when this network has IPv6" value={ipv6} onChange={setIPv6} /> : null}
              {t !== 'plain' ? (
                <p className="k-muted k-small">
                  Host names are looked up with the bootstrap resolvers ({u.cfg.bootstrap.join(', ') || 'the system resolver'}), set on the Upstreams page.
                </p>
              ) : null}
              {editing ? (
                <ServersField value={custom} onChange={setCustom} />
              ) : (
                <div className="k-stack-sm">
                  <span className="k-label">Servers</span>
                  <ul className="k-link-list k-stack-sm" data-testid="preset-servers">
                    {presetServers.map((s) => {
                      const { name: n, addr } = splitServerName(s);
                      return (
                        <li key={s}>
                          <strong>{n}</strong> <span className="k-mono k-small">{addr}</span>
                        </li>
                      );
                    })}
                  </ul>
                  <span>
                    <Button
                      label="Edit the list"
                      size="sm"
                      variant="ghost"
                      onClick={() => {
                        setCustom(presetServers);
                        setEditing(true);
                      }}
                    />
                  </span>
                </div>
              )}
              <p className="k-muted k-small">
                Addresses as documented at <a href={provider.homepage} target="_blank" rel="noopener noreferrer">{provider.homepage}</a>.
              </p>
            </>
          ) : (
            <ServersField value={custom} onChange={setCustom} />
          )}
        </>
      ),
    },
    {
      id: 'group',
      label: 'Group',
      description: 'How policies, zones and conditional rules refer to it, and how its servers are asked.',
      validate: () => newGroupNameError(groupName, existing) ?? groupFieldErrors({ ...group, servers }).timeout ?? null,
      content: (
        <>
          <VStack gap={1}>
            <TextInput
              label="Name"
              value={groupName}
              onChange={(v) => {
                setNameTouched(true);
                setName(v);
              }}
              placeholder="corp"
              isRequired
              status={groupName && newGroupNameError(groupName, existing) ? { type: 'error', message: newGroupNameError(groupName, existing) ?? '' } : undefined}
            />
            <Text type="supporting" display="block">
              Letters, digits, - _ and .
            </Text>
          </VStack>
          <GroupFields value={{ ...group, servers }} onChange={(g) => setGroup({ ...g, servers: group.servers })} serversHidden />
        </>
      ),
    },
    {
      id: 'review',
      label: 'Review',
      content: (
        <dl className="k-review" data-testid="upstream-review">
          <dt>Name</dt>
          <dd className="k-mono">{groupName}</dd>
          <dt>Provider</dt>
          <dd>{provider ? `${provider.name}${variant && provider.variants.length > 1 ? `, ${variant.label}` : ''}, ${TRANSPORTS.find((x) => x.value === t)?.label}` : 'Custom'}</dd>
          <dt>Servers</dt>
          <dd className="k-mono k-small">
            {servers.map((s) => (
              <div key={s}>{s}</div>
            ))}
          </dd>
          <dt>Strategy</dt>
          <dd>{strategyLabel(group.strategy)}</dd>
          <dt>Timeout</dt>
          <dd>{group.timeout}</dd>
        </dl>
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
      isDirty={choice !== '' || name !== '' || profile !== '' || custom.length > 0}
      error={error ? <Banner status="error" title="The group could not be created" description={error} /> : null}
    />
  );
}
