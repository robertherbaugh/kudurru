// New policy, in a WizardDialog over the Policies page (?new=1) or a
// group's page: six steps (Who → What → Safe search → How → When →
// Review), each with the same typed editors the policy page uses. The
// review lists every choice with a link back to its step; Create checks
// the name again (another tab may have taken it), writes the Policy, closes
// the dialog and opens the policy's page. ?namespace= (or the namespace
// prop) creates it in another namespace; `groups` preselects client groups.
import { Banner } from '@astryxdesign/core/Banner';
import { FormLayout } from '@astryxdesign/core/FormLayout';
import { MetadataList } from '@astryxdesign/core/MetadataList';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { qk, useApi, useCan } from '../../api/context';
import type { Policy, PolicySpec } from '../../api/types';
import { errorText, useNotify } from '../Feedback';
import { TextField } from '../fields/TextField';
import { cleanPolicySpec, HowFields, howError, SafeSearchFields, ServicesFields, WhatFields, WhenFields, whenError, WhoFields, whoError } from '../PolicyFields';
import { ServiceIcon } from '../ServiceIcon';
import { useCatalogueServices } from '../ServiceGrid';
import { ReviewRow, Wizard, WizardDialog, type WizardStep } from '../Wizard';
import { COMMENT } from '../../lib/managedBy';
import { nameError } from '../../lib/objectSchemas';
import { blockResponseLabel, DEFAULT_NAMESPACE, scheduleSummary } from '../../lib/policies';
import { useNavigate, useSearchParams } from '../../lib/router';
import { usePolicies } from '../../lib/usePolicyData';

export interface PolicyWizardProps {
  /** Closes the dialog without asking (after the policy was created). */
  onClose: () => void;
  /** The namespace to create it in (default: ?namespace=, else "default"). */
  namespace?: string;
  /** Client groups to start with (bare names in the namespace). */
  groups?: string[];
}

export function PolicyWizardDialog({ isOpen, ...props }: PolicyWizardProps & { isOpen: boolean }) {
  return (
    <WizardDialog isOpen={isOpen} onClose={props.onClose} title="New policy" subtitle="Which lists and services apply to which clients, how blocks are answered and when." what="policy">
      <PolicyWizard {...props} />
    </WizardDialog>
  );
}

function PolicyWizard({ onClose, namespace: ns, groups }: PolicyWizardProps) {
  const api = useApi();
  const can = useCan();
  const qc = useQueryClient();
  const notify = useNotify();
  const navigate = useNavigate();
  const [sp] = useSearchParams();
  const pols = usePolicies();
  const namespace = ns || sp.get('namespace') || DEFAULT_NAMESPACE;
  const [name, setName] = useState('');
  const [comment, setComment] = useState('');
  const [initial] = useState<PolicySpec>(() => ({ clientGroupRefs: groups ?? [], blockListRefs: [], allowListRefs: [], blockedServices: [], priority: 100 }));
  const [spec, setSpec] = useState<PolicySpec>(initial);
  const [showErrors, setShowErrors] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const props = { spec, onChange: setSpec, namespace, showErrors };

  const nameMsg = (existing: Policy[]): string | null => {
    const e = nameError(name.trim());
    if (e) return `Name: ${e}`;
    if (existing.some((p) => p.metadata.name === name.trim() && (p.metadata.namespace ?? DEFAULT_NAMESPACE) === namespace)) return `A policy named ${name.trim()} already exists.`;
    return null;
  };

  const create = async () => {
    if (!can('policies', 'write')) {
      setError('Creating policies needs policies.write.');
      return;
    }
    const obj: Policy = {
      apiVersion: 'kudurru.io/v1alpha1',
      kind: 'Policy',
      metadata: { name: name.trim(), namespace, ...(comment.trim() ? { annotations: { [COMMENT]: comment.trim() } } : {}) },
      spec: cleanPolicySpec(spec),
    };
    setBusy(true);
    setError(null);
    try {
      // A whole-object PUT would replace a policy another tab created meanwhile.
      const msg = nameMsg((await api.objects.list<Policy>('policies')).items) ?? whoError(spec);
      if (msg) {
        setError(msg);
        return;
      }
      await api.objects.put('policies', obj);
      await qc.invalidateQueries({ queryKey: qk.objects('policies') });
      notify.success(`Created policy ${obj.metadata.name}`);
      onClose();
      navigate(`/policies/${encodeURIComponent(namespace)}/${encodeURIComponent(obj.metadata.name)}`);
    } catch (err) {
      setError(errorText(err));
    } finally {
      setBusy(false);
    }
  };

  const steps: WizardStep[] = [
    {
      id: 'who',
      label: 'Who',
      description: 'Name the policy and pick the client groups it applies to.',
      content: (
        <VStack gap={5}>
          <FormLayout direction="horizontal">
            <TextField
              label="Name"
              value={name}
              onChange={setName}
              isRequired
              mono
              help='Lower-case letters, digits and "-", e.g. kids-school-nights'
              error={showErrors ? (nameError(name.trim()) ?? undefined) : undefined}
            />
            <TextField label="Comment" value={comment} onChange={setComment} isOptional />
          </FormLayout>
          <WhoFields {...props} />
        </VStack>
      ),
      validate: () => {
        setShowErrors(true);
        return nameMsg(pols.data?.items ?? []) ?? whoError(spec);
      },
    },
    {
      id: 'what',
      label: 'What',
      description: 'Block and allow lists, and whole services to block.',
      content: (
        <VStack gap={5}>
          <WhatFields {...props} />
          <ServicesFields {...props} />
        </VStack>
      ),
    },
    { id: 'safesearch', label: 'Safe search', isOptional: true, content: <SafeSearchFields {...props} /> },
    { id: 'how', label: 'How', isOptional: true, description: 'What blocked clients get back and where allowed queries go.', content: <HowFields {...props} />, validate: () => howError(spec) },
    { id: 'when', label: 'When', isOptional: true, content: <WhenFields {...props} />, validate: () => whenError(spec) },
    {
      id: 'review',
      label: 'Review',
      description: 'Check the policy before it is created.',
      content: <Review name={name.trim()} namespace={namespace} comment={comment.trim()} spec={spec} />,
    },
  ];

  return (
    <Wizard
      label="New policy"
      steps={steps}
      finishLabel="Create policy"
      onFinish={create}
      isFinishing={busy}
      isDirty={name !== '' || comment !== '' || spec !== initial}
      notice={!can('policies', 'write') && !pols.isLoading ? <Banner status="warning" title="Read-only" description="Creating policies needs policies.write." /> : null}
      error={error ? <Banner status="error" title="The server rejected the policy" description={error} /> : null}
    />
  );
}

function Review({ name, namespace, comment, spec }: { name: string; namespace: string; comment: string; spec: PolicySpec }) {
  const services = useCatalogueServices();
  const byId = new Map((services.data?.items ?? []).map((s) => [s.id, s]));
  const none = <Text color="secondary">none</Text>;
  const names = (xs: string[] | undefined) => (xs?.length ? xs.join(', ') : none);
  return (
    <MetadataList data-testid="policy-review">
      <ReviewRow label="Name" step="who" what="name">
        <Text type="code">{name}</Text>
        {namespace !== DEFAULT_NAMESPACE ? <Text color="secondary"> in {namespace}</Text> : null}
        {comment ? <Text color="secondary"> · {comment}</Text> : null}
      </ReviewRow>
      <ReviewRow label="Groups" step="who" what="groups">
        {names(spec.clientGroupRefs)} <Text color="secondary">· priority {spec.priority ?? 0}</Text>
      </ReviewRow>
      <ReviewRow label="Block lists" step="what" what="block lists">
        {spec.allowOnly ? <Text weight="semibold">Allow only: everything else is blocked</Text> : names(spec.blockListRefs)}
      </ReviewRow>
      <ReviewRow label="Allow lists" step="what" what="allow lists">
        {names(spec.allowListRefs)}
      </ReviewRow>
      <ReviewRow label="Blocked services" step="what" what="blocked services">
        {spec.blockedServices?.length ? (
          <HStack gap={3} wrap="wrap" role="list" aria-label="Blocked services">
            {spec.blockedServices.map((id) => (
              <HStack key={id} gap={1} vAlign="center" role="listitem">
                <ServiceIcon id={id} name={byId.get(id)?.name ?? id} icon={byId.get(id)?.icon} iconOnDark={byId.get(id)?.iconOnDark} size={20} />
                <Text>{byId.get(id)?.name ?? id}</Text>
              </HStack>
            ))}
          </HStack>
        ) : (
          none
        )}
      </ReviewRow>
      <ReviewRow label="Safe search" step="safesearch" what="safe search">
        {spec.safeSearch ? `On (${spec.safeSearchMode === 'moderate' ? 'moderate' : 'strict'})` : 'Off'}
      </ReviewRow>
      <ReviewRow label="Block response" step="how" what="block response">
        {blockResponseLabel(spec.blockMode)}
        {spec.blockTTL ? ` · TTL ${spec.blockTTL}` : ''}
        {spec.upstreamGroup ? ` · upstream group ${spec.upstreamGroup}` : ''}
        {spec.queryLog === false ? ' · not logged' : ''}
      </ReviewRow>
      <ReviewRow label="When" step="when" what="schedule">
        {scheduleSummary(spec.schedule)}
      </ReviewRow>
    </MetadataList>
  );
}
