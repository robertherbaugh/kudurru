// New group, in a WizardDialog over the Groups page (?new=1): a client
// group in four steps (Name → Members → Policies → Review). Members are
// typed matchers, with the clients seen in the last 24 hours offered as
// one-click additions; the optional Policies step adds the new group to
// existing policies (their clientGroupRefs), so the group does something
// the moment it exists. Create closes the dialog and opens the group's page.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { CheckboxList, CheckboxListItem } from '@astryxdesign/core/CheckboxList';
import { FormLayout } from '@astryxdesign/core/FormLayout';
import { Heading } from '@astryxdesign/core/Heading';
import { List, ListItem } from '@astryxdesign/core/List';
import { MetadataList } from '@astryxdesign/core/MetadataList';
import { VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { qk, useApi, useCan, useScopeParams } from '../../api/context';
import type { ClientGroup, ClientMatcher, Policy } from '../../api/types';
import { errorText, useNotify } from '../Feedback';
import { FieldShell } from '../fields/FieldHelp';
import { TextField } from '../fields/TextField';
import { cleanMatchers, describeMatcher, matcherFor, MatchersField, matchersError, matcherValue } from '../fields/MatchersField';
import { ReviewRow, Wizard, WizardDialog, type WizardStep } from '../Wizard';
import { formatExact } from '../../lib/format';
import { COMMENT, objectOwnership } from '../../lib/managedBy';
import { nameError } from '../../lib/objectSchemas';
import { DEFAULT_NAMESPACE, sortByPriority } from '../../lib/policies';
import { useNavigate, useSearchParams } from '../../lib/router';
import { useGroups, usePolicies } from '../../lib/usePolicyData';

export interface GroupWizardProps {
  /** Closes the dialog without asking (after the group was created). */
  onClose: () => void;
}

export function GroupWizardDialog({ isOpen, onClose }: GroupWizardProps & { isOpen: boolean }) {
  return (
    <WizardDialog isOpen={isOpen} onClose={onClose} title="New group" subtitle="A set of clients that policies can target." what="group">
      <GroupWizard onClose={onClose} />
    </WizardDialog>
  );
}

function GroupWizard({ onClose }: GroupWizardProps) {
  const api = useApi();
  const can = useCan();
  const scope = useScopeParams();
  const qc = useQueryClient();
  const notify = useNotify();
  const navigate = useNavigate();
  const [sp] = useSearchParams();
  const namespace = sp.get('namespace') || DEFAULT_NAMESPACE;
  const groups = useGroups();
  const pols = usePolicies();
  const clients = useQuery({ queryKey: qk.clients(scope), queryFn: () => api.clients.list(scope, { window: '24h' }), enabled: can('clients', 'read') });
  const [name, setName] = useState('');
  const [comment, setComment] = useState('');
  const [matchers, setMatchers] = useState<ClientMatcher[]>([]);
  const [attach, setAttach] = useState<string[]>([]);
  const [showErrors, setShowErrors] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const policies = sortByPriority((pols.data?.items ?? []).filter((p) => (p.metadata.namespace ?? DEFAULT_NAMESPACE) === namespace));
  const writable = policies.filter((p) => objectOwnership(p).editable);
  const taken = new Set(matchers.map(matcherValue));

  const nameMsg = (existing: ClientGroup[]): string | null => {
    const e = nameError(name.trim());
    if (e) return `Name: ${e}`;
    if (existing.some((g) => g.metadata.name === name.trim() && (g.metadata.namespace ?? DEFAULT_NAMESPACE) === namespace)) return `A group named ${name.trim()} already exists.`;
    return null;
  };

  const create = async () => {
    const obj: ClientGroup = {
      apiVersion: 'kudurru.io/v1alpha1',
      kind: 'ClientGroup',
      metadata: { name: name.trim(), namespace, ...(comment.trim() ? { annotations: { [COMMENT]: comment.trim() } } : {}) },
      spec: { clients: cleanMatchers(matchers) },
    };
    setBusy(true);
    setError(null);
    try {
      const msg = nameMsg((await api.objects.list<ClientGroup>('clientgroups')).items) ?? matchersError(matchers);
      if (msg) {
        setError(msg);
        return;
      }
      await api.objects.put('clientgroups', obj);
      const failed: string[] = [];
      for (const pn of attach) {
        const p = policies.find((x) => x.metadata.name === pn);
        if (!p) continue;
        try {
          await api.objects.put<Policy>('policies', { ...p, spec: { ...p.spec, clientGroupRefs: [...(p.spec.clientGroupRefs ?? []), obj.metadata.name] } });
        } catch (err) {
          failed.push(`${pn}: ${errorText(err)}`);
        }
      }
      await qc.invalidateQueries({ queryKey: qk.objects('clientgroups') });
      await qc.invalidateQueries({ queryKey: qk.objects('policies') });
      notify.success(`Created group ${obj.metadata.name}`);
      for (const f of failed) notify.error(new Error(f), 'Could not add the group to a policy');
      onClose();
      navigate(`/groups/${encodeURIComponent(namespace)}/${encodeURIComponent(obj.metadata.name)}`);
    } catch (err) {
      setError(errorText(err));
    } finally {
      setBusy(false);
    }
  };

  const seen = (clients.data?.items ?? []).filter((c) => !taken.has(c.key));

  const steps: WizardStep[] = [
    {
      id: 'name',
      label: 'Name',
      content: (
        <FormLayout direction="horizontal">
          <TextField
            label="Name"
            value={name}
            onChange={setName}
            isRequired
            mono
            help='Lower-case letters, digits and "-", e.g. kids'
            error={showErrors ? (nameError(name.trim()) ?? undefined) : undefined}
          />
          <TextField label="Comment" value={comment} onChange={setComment} isOptional />
        </FormLayout>
      ),
      validate: () => {
        setShowErrors(true);
        return nameMsg(groups.data?.items ?? []);
      },
    },
    {
      id: 'members',
      label: 'Members',
      description: 'The devices of the group: by address, range, client ID or MAC address.',
      content: (
        <VStack gap={5}>
          <MatchersField value={matchers} onChange={setMatchers} showErrors={showErrors} />
          {seen.length ? (
            <List
              density="compact"
              hasDividers
              header={
                <VStack gap={0.5}>
                  <Heading level={4}>Seen in the last 24 hours</Heading>
                  <Text type="supporting">Add a client by the address or client ID it queried with.</Text>
                </VStack>
              }
            >
              {seen.map((c) => (
                <ListItem
                  key={c.key}
                  label={c.name ?? c.key}
                  description={`${c.name ? `${c.key} · ` : ''}${formatExact(c.count)} queries${c.group ? ` · now in ${c.group.replace(/^default\//, '')}` : ''}`}
                  endContent={
                    <Button label={`Add ${c.name ?? c.key}`} size="sm" variant="secondary" onClick={() => setMatchers([...matchers, matcherFor(c, '')])}>
                      Add
                    </Button>
                  }
                />
              ))}
            </List>
          ) : null}
        </VStack>
      ),
      validate: () => matchersError(matchers),
    },
    {
      id: 'policies',
      label: 'Policies',
      isOptional: true,
      description: 'Add the group to existing policies. Without one its members get the default policy.',
      content: (
        <FieldShell help={writable.length ? 'Or create a policy for the group afterwards from its page.' : 'No policy can be edited here; create one for the group afterwards from its page.'} describes="group">
          {() => (
            <CheckboxList label="Apply these policies" value={attach} onChange={setAttach}>
              {writable.map((p) => (
                <CheckboxListItem
                  key={p.metadata.name}
                  value={p.metadata.name}
                  label={p.metadata.name}
                  description={`priority ${p.spec.priority ?? 0} · groups ${(p.spec.clientGroupRefs ?? []).join(', ') || 'none'}`}
                />
              ))}
            </CheckboxList>
          )}
        </FieldShell>
      ),
    },
    {
      id: 'review',
      label: 'Review',
      content: <Review name={name.trim()} comment={comment.trim()} matchers={matchers} attach={attach} />,
    },
  ];

  return (
    <Wizard
      label="New group"
      steps={steps}
      finishLabel="Create group"
      onFinish={create}
      isFinishing={busy}
      isDirty={name !== '' || comment !== '' || matchers.length > 0 || attach.length > 0}
      notice={!can('clientgroups', 'write') && !groups.isLoading ? <Banner status="warning" title="Read-only" description="Creating groups needs clientgroups.write." /> : null}
      error={error ? <Banner status="error" title="The server rejected the group" description={error} /> : null}
    />
  );
}

function Review({ name, comment, matchers, attach }: { name: string; comment: string; matchers: ClientMatcher[]; attach: string[] }) {
  return (
    <MetadataList data-testid="group-review">
      <ReviewRow label="Name" step="name" what="name">
        <Text type="code">{name}</Text>
        {comment ? <Text color="secondary"> · {comment}</Text> : null}
      </ReviewRow>
      <ReviewRow label="Members" step="members" what="members">
        {matchers.length ? cleanMatchers(matchers).map(describeMatcher).join(', ') : <Text color="secondary">none yet</Text>}
      </ReviewRow>
      <ReviewRow label="Policies" step="policies" what="policies">
        {attach.length ? attach.join(', ') : <Text color="secondary">the default policy</Text>}
      </ReviewRow>
    </MetadataList>
  );
}
