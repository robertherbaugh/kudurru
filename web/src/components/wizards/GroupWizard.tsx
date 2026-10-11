// New group, in a WizardDialog over the Groups page (?new=1): a client
// group in four steps (Name → Members → Policies → Review). Members are
// typed matchers, with the clients seen in the last 24 hours offered as
// one-click additions; the optional Policies step adds the new group to
// existing policies (their clientGroupRefs), so the group does something
// the moment it exists. Create closes the dialog and opens the group's page.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { qk, useApi, useCan, useScopeParams } from '../../api/context';
import type { ClientGroup, ClientMatcher, Policy } from '../../api/types';
import { errorText, useNotify } from '../Feedback';
import { cleanMatchers, describeMatcher, matcherFor, MatchersField, matchersError, matcherValue } from '../fields/MatchersField';
import { Wizard, WizardDialog, useWizardNav, type WizardStep } from '../Wizard';
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
  const clients = useQuery({ queryKey: qk.clients(scope), queryFn: () => api.clients.list({ window: '24h' }), enabled: can('clients', 'read') });
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
        <div className="k-form-grid">
          <VStack gap={1}>
            <TextInput label="Name" value={name} onChange={setName} isRequired status={showErrors && nameError(name.trim()) ? { type: 'error', message: nameError(name.trim()) ?? '' } : undefined} />
            <Text type="supporting" display="block">
              Lower-case letters, digits and "-", e.g. kids
            </Text>
          </VStack>
          <TextInput label="Comment" value={comment} onChange={setComment} isOptional />
        </div>
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
        <div className="k-stack-lg">
          <MatchersField value={matchers} onChange={setMatchers} showErrors={showErrors} />
          {seen.length ? (
            <fieldset className="k-fieldset">
              <legend>Seen in the last 24 hours</legend>
              <p className="k-muted k-small">Add a client by the address or client ID it queried with.</p>
              <ul className="k-checklist">
                {seen.map((c) => (
                  <li key={c.key} className="k-row">
                    <Button label={`Add ${c.name ?? c.key}`} size="sm" variant="secondary" onClick={() => setMatchers([...matchers, matcherFor(c, '')])}>
                      Add
                    </Button>
                    <span>{c.name ?? c.key}</span>
                    {c.name ? <span className="k-muted k-small k-mono">{c.key}</span> : null}
                    <span className="k-muted k-small">
                      {formatExact(c.count)} queries{c.group ? ` · now in ${c.group.replace(/^default\//, '')}` : ''}
                    </span>
                  </li>
                ))}
              </ul>
            </fieldset>
          ) : null}
        </div>
      ),
      validate: () => matchersError(matchers),
    },
    {
      id: 'policies',
      label: 'Policies',
      isOptional: true,
      description: 'Add the group to existing policies. Without one its members get the default policy.',
      content: (
        <fieldset className="k-fieldset">
          <legend>Apply these policies</legend>
          {writable.length === 0 ? <p className="k-muted k-small">No policy can be edited here.</p> : null}
          <ul className="k-checklist">
            {writable.map((p) => (
              <li key={p.metadata.name}>
                <label>
                  <input
                    type="checkbox"
                    checked={attach.includes(p.metadata.name)}
                    onChange={(e) => setAttach(e.target.checked ? [...attach, p.metadata.name] : attach.filter((x) => x !== p.metadata.name))}
                  />{' '}
                  <span className="k-mono">{p.metadata.name}</span>
                  <span className="k-muted k-small">
                    {' '}
                    · priority {p.spec.priority ?? 0} · groups {(p.spec.clientGroupRefs ?? []).join(', ') || 'none'}
                  </span>
                </label>
              </li>
            ))}
          </ul>
          <p className="k-muted k-small">Or create a policy for the group afterwards from its page.</p>
        </fieldset>
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
  const nav = useWizardNav();
  const edit = (step: string, what: string) => (
    <Button label={`Edit ${what}`} size="sm" variant="ghost" onClick={() => nav.goTo(step)}>
      Edit
    </Button>
  );
  return (
    <dl className="k-review" data-testid="group-review">
      <dt>Name</dt>
      <dd>
        <span className="k-mono">{name}</span>
        {comment ? <span className="k-muted"> · {comment}</span> : null}
      </dd>
      <dd className="k-review-edit">{edit('name', 'name')}</dd>
      <dt>Members</dt>
      <dd>{matchers.length ? cleanMatchers(matchers).map(describeMatcher).join(', ') : <span className="k-muted">none yet</span>}</dd>
      <dd className="k-review-edit">{edit('members', 'members')}</dd>
      <dt>Policies</dt>
      <dd>{attach.length ? attach.join(', ') : <span className="k-muted">the default policy</span>}</dd>
      <dd className="k-review-edit">{edit('policies', 'policies')}</dd>
    </dl>
  );
}
