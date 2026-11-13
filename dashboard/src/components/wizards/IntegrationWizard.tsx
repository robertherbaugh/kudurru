// New integration, in a WizardDialog over the Integrations page (?new=1):
// Provider → Settings → Credentials → Review. Everything comes from GET
// /integrations/manifests: the providers of this build (a type this node
// refuses is listed but cannot be chosen), each one's configuration schema
// without the fields this node refuses, its credential keys and the
// reference forms this node resolves. Create is one create-only POST
// /objects/integrations (D400); it needs integrations.write, integrations.secrets
// when it binds a credential, and a license that covers integrations.
import { Banner } from '@astryxdesign/core/Banner';
import { CodeBlock } from '@astryxdesign/core/CodeBlock';
import { FormLayout } from '@astryxdesign/core/FormLayout';
import { MetadataList } from '@astryxdesign/core/MetadataList';
import { VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useQueryClient } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { useApi, useCan, useCannotReason } from '../../api/context';
import type { IntegrationManifests } from '../../api/types';
import {
  buildIntegration,
  CAPABILITY_LABELS,
  configError,
  credentialsError,
  describeRef,
  integrationHref,
  newDraft,
  publishes,
  unsupportedReason,
  type IntegrationDraft,
} from '../../lib/integrations';
import { nameError } from '../../lib/objectSchemas';
import { useNavigate } from '../../lib/router';
import { toYAML } from '../../lib/yaml';
import { errorText, useNotify } from '../Feedback';
import { ChoiceCards } from '../fields/ChoiceCards';
import { TextField } from '../fields/TextField';
import { CredentialFields, SettingsFields } from '../integrations/IntegrationFields';
import { ReviewRow, Wizard, WizardDialog, type WizardStep } from '../Wizard';

import { INTEGRATIONS_KEY } from '../../lib/useIntegrations';

export interface IntegrationWizardProps {
  manifests: IntegrationManifests;
  onClose: () => void;
}

export function IntegrationWizardDialog({ isOpen, ...rest }: IntegrationWizardProps & { isOpen: boolean }) {
  return (
    <WizardDialog isOpen={isOpen} onClose={rest.onClose} title="New integration" subtitle="Connect a provider: publish records, read client inventory or offer actions." what="integration">
      <IntegrationWizard {...rest} />
    </WizardDialog>
  );
}

function IntegrationWizard({ manifests, onClose }: IntegrationWizardProps) {
  const api = useApi();
  const can = useCan();
  const cannot = useCannotReason();
  const qc = useQueryClient();
  const notify = useNotify();
  const navigate = useNavigate();
  const fixedNamespace = manifests.namespace || '';
  const first = manifests.items.find((m) => m.supported);
  const [type, setType] = useState(first?.type ?? '');
  const [drafts, setDrafts] = useState<Record<string, IntegrationDraft>>({});
  const [showErrors, setShowErrors] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [touched, setTouched] = useState(false);

  const manifest = manifests.items.find((m) => m.type === type);
  const draft = useMemo(() => (manifest ? (drafts[manifest.type] ?? newDraft(manifest, fixedNamespace || 'default')) : undefined), [drafts, manifest, fixedNamespace]);
  const update = (d: IntegrationDraft) => {
    setTouched(true);
    setDrafts((all) => ({ ...all, [d.type]: d }));
  };
  const canSecrets = can('integrations', 'secrets');
  const writeReason = cannot('integrations', 'write');
  const doc = manifest && draft ? buildIntegration(manifest, draft) : undefined;
  const bindsCredentials = !!doc?.spec.credentials && Object.keys(doc.spec.credentials).length > 0;

  const nameMsg = (): string | null => {
    if (!draft) return 'Choose a provider';
    const e = nameError(draft.name.trim());
    if (e) return `Name: ${e}`;
    if (!fixedNamespace) {
      const ne = nameError(draft.namespace.trim());
      if (ne) return `Namespace: ${ne}`;
    }
    return null;
  };

  const create = async () => {
    if (!manifest || !draft || !doc) return;
    setBusy(true);
    setError(null);
    try {
      const created = await api.integrations.create(doc);
      await qc.invalidateQueries({ queryKey: INTEGRATIONS_KEY });
      notify.success(`Created integration ${created.metadata.name}`);
      onClose();
      navigate(integrationHref(created.metadata.namespace ?? draft.namespace, created.metadata.name));
    } catch (err) {
      setError(errorText(err));
    } finally {
      setBusy(false);
    }
  };

  const blocked = !manifests.licensed
    ? 'A new Integration needs a license that covers integrations; existing ones can still be changed or removed.'
    : writeReason
      ? `Creating an integration: ${writeReason}.`
      : bindsCredentials && !canSecrets
        ? 'Binding a credential needs integrations.secrets besides integrations.write.'
        : undefined;

  const steps: WizardStep[] = [
    {
      id: 'provider',
      label: 'Provider',
      content: (
        <ChoiceCards
          label="Provider"
          value={type}
          onChange={(v) => {
            setTouched(true);
            setType(v);
          }}
          choices={manifests.items.map((m) => ({
            value: m.type,
            label: m.displayName,
            description: unsupportedReason(m) ?? m.capabilities.map((c) => CAPABILITY_LABELS[c] ?? c).join(' · '),
            isDisabled: !m.supported,
          }))}
          help={manifests.driver === 'kube' ? `On Kubernetes only Integrations in ${manifests.namespace || 'the release namespace'} run.` : undefined}
        />
      ),
      validate: () => (!manifest ? 'Choose a provider' : unsupportedReason(manifest) ?? null),
    },
    {
      id: 'settings',
      label: 'Settings',
      content:
        manifest && draft ? (
          <VStack gap={4}>
            <FormLayout direction="horizontal">
              <TextField
                label="Name"
                value={draft.name}
                onChange={(v: string) => update({ ...draft, name: v })}
                isRequired
                mono
                help='Lower-case letters, digits and "-", e.g. public-dns'
                error={showErrors ? (nameError(draft.name.trim()) ?? undefined) : undefined}
              />
              <TextField
                label="Namespace"
                value={fixedNamespace || draft.namespace}
                onChange={(v: string) => update({ ...draft, namespace: v })}
                mono
                isDisabled={!!fixedNamespace}
                help={fixedNamespace ? 'The only namespace whose Integrations run on this node.' : undefined}
              />
            </FormLayout>
            <SettingsFields manifest={manifest} draft={draft} onChange={update} />
          </VStack>
        ) : null,
      validate: () => {
        setShowErrors(true);
        return nameMsg() ?? (manifest && draft ? configError(manifest, draft) : null);
      },
    },
    {
      id: 'credentials',
      label: 'Credentials',
      content:
        manifest && draft ? (
          <CredentialFields
            manifest={manifest}
            manifests={manifests}
            draft={draft}
            onChange={update}
            showErrors={showErrors}
            disabledReason={!canSecrets ? 'Binding a credential needs integrations.secrets.' : undefined}
          />
        ) : null,
      validate: () => (manifest && draft ? credentialsError(manifest, draft, manifests.credentialSources) : null),
    },
    {
      id: 'review',
      label: 'Review',
      content:
        manifest && draft && doc ? (
          <VStack gap={3}>
            <MetadataList data-testid="integration-review">
              <ReviewRow label="Provider" step="provider" what="provider">
                {manifest.displayName}
              </ReviewRow>
              <ReviewRow label="Name" step="settings" what="name">
                <Text type="code">
                  {doc.metadata.namespace}/{doc.metadata.name}
                </Text>
              </ReviewRow>
              <ReviewRow label="Credentials" step="credentials" what="credentials">
                {manifest.credentials.length ? manifest.credentials.map((c) => `${c.name}: ${describeRef(doc.spec.credentials?.[c.name])}`).join('; ') : 'none'}
              </ReviewRow>
              <ReviewRow label="When deleted" step="settings" what="deletion policy">
                {draft.deletionPolicy}
              </ReviewRow>
              <ReviewRow label="Sync" step="settings" what="settings">
                {publishes(manifest, doc.spec)
                  ? 'Publishes records: every sync is a reviewed Preview, then Publish this plan by its digest.'
                  : 'Observes the provider: a sync publishes no records.'}
              </ReviewRow>
            </MetadataList>
            <CodeBlock code={toYAML(doc)} language="yaml" title="Integration" width="100%" maxHeight="20rem" />
          </VStack>
        ) : null,
    },
  ];

  return (
    <Wizard
      label="New integration"
      steps={steps}
      finishLabel="Create integration"
      onFinish={create}
      isFinishing={busy}
      isFinishDisabled={!!blocked}
      revalidateOnFinish
      isDirty={touched}
      notice={blocked ? <Banner status="warning" title="Cannot create" description={blocked} /> : null}
      error={error ? <Banner status="error" title="The server rejected the integration" description={error} /> : null}
    />
  );
}
