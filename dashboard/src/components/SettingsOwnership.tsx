import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { CodeBlock } from '@astryxdesign/core/CodeBlock';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { qk, useApi, useCan } from '../api/context';
import type { SettingsBody } from '../api/types';
import { errorText } from './Feedback';
import { FormDialog } from './FormDialog';
import { CheckboxField, TextAreaField } from './fields/TextField';
import { Panel } from './layout/Page';

/** Explicit ownership and a reviewed, two-step handoff back to GitOps. */
export function SettingsOwnership({ body }: { body: SettingsBody }) {
  const api = useApi();
  const qc = useQueryClient();
  const can = useCan();
  const section = body.sections?.upstreams;
  const [adopting, setAdopting] = useState<string>();
  const [exporting, setExporting] = useState(false);
  const [release, setRelease] = useState(false);
  const [preview, setPreview] = useState<{ yaml: string; revision: string }>();
  const [proposing, setProposing] = useState(false);
  const [proposed, setProposed] = useState('');
  const [applied, setApplied] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  if (!section) return null;
  const editable = can('settings', 'write') && section.switchable && !section.envLocked;
  const update = (next: SettingsBody) => {
    qc.setQueryData(qk.settings, next);
    void qc.invalidateQueries({ queryKey: qk.schema });
    setAdopting(undefined); setExporting(false); setPreview(undefined);
  };
  const transition = async (action: 'adopt' | 'release') => {
    const revision = action === 'release' ? preview?.revision : adopting;
    if (!revision) return;
    setBusy(true); setError(undefined);
    try { update(await api.settings.ownership('upstreams', action, revision)); }
    catch (err) { setError(errorText(err)); }
    finally { setBusy(false); }
  };
  const generate = async () => {
    setBusy(true); setError(undefined); setApplied(false);
    try {
      let value: Record<string, unknown> | undefined;
      if (proposing && !release) {
        const parsed: unknown = JSON.parse(proposed);
        if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) throw new Error('Proposed upstreams must be a JSON object.');
        value = parsed as Record<string, unknown>;
      }
      setPreview(await api.settings.yaml('upstreams', value));
    } catch (err) { setError(errorText(err)); }
    finally { setBusy(false); }
  };
  const openExport = (handoff: boolean) => {
    setRelease(handoff); setExporting(true); setPreview(undefined); setProposing(false); setApplied(false); setError(undefined);
    setProposed(JSON.stringify(body.settings.upstreams, null, 2));
  };
  return (
    <Panel title="Management">
      <HStack gap={2} vAlign="center" wrap="wrap">
        <Text weight="semibold">{section.owner === 'ui' ? 'Managed in the UI' : 'Managed by GitOps'}</Text>
        {section.owner === 'gitops' ? <Button label="Adopt into UI management" size="sm" isDisabled={!editable} onClick={() => { setAdopting(section.revision); setError(undefined); }} /> :
          <Button label="Return to GitOps" size="sm" isDisabled={!editable} onClick={() => openExport(true)} />}
        <Button label="Generate YAML" size="sm" variant="ghost" onClick={() => openExport(false)} />
      </HStack>
      {section.envLocked ? <Text color="secondary">Upstream environment overrides must be removed on every node before changing ownership.</Text> : null}
      {body.conflicts?.includes('upstreams') ? <Banner status="warning" title="GitOps also declares upstreams" description="UI settings remain authoritative. Remove the upstreams section from your Helm values while it is managed here." /> : null}
      {exporting ? (
        <VStack gap={3} align="stretch">
          <Text as="p">{release ? 'Generate the current Helm values, commit and apply them, and wait for the settings volume to update on every replica. Then refresh the preview before releasing ownership. The server checks its mounted routing settings before the handoff.' : 'Generate Helm values for the current upstreams or proposed changes. Generating YAML does not save settings.'}</Text>
          {!release ? <CheckboxField label="Edit proposed upstreams as JSON" value={proposing} onChange={(v) => { setProposing(v); setPreview(undefined); }} /> : null}
          {proposing && !release ? <TextAreaField label="Proposed upstreams (JSON)" mono rows={8} value={proposed} onChange={(v) => { setProposed(v); setPreview(undefined); }} /> : null}
          <HStack gap={2} wrap="wrap">
            <Button label={preview ? 'Refresh YAML preview' : 'Generate preview'} size="sm" isLoading={busy} onClick={() => void generate()} />
            <Button label="Cancel" size="sm" variant="ghost" onClick={() => { setExporting(false); setError(undefined); }} />
          </HStack>
          {preview ? <CodeBlock code={preview.yaml} language="yaml" title="Helm values" width="100%" maxHeight="24rem" /> : null}
          {preview && release ? <>
            <CheckboxField label="I committed and applied these Helm values" value={applied} onChange={setApplied} />
            <HStack gap={2}><Button label="Release to GitOps" variant="primary" isDisabled={!applied || !editable} isLoading={busy} onClick={() => void transition('release')} /></HStack>
          </> : null}
          {error ? <Banner status="error" title="Management change could not be completed" description={error} /> : null}
        </VStack>
      ) : null}
      <FormDialog isOpen={adopting !== undefined} onClose={() => setAdopting(undefined)} title="Adopt upstreams into UI management?" footer={<>
        <Button label="Cancel" onClick={() => setAdopting(undefined)} />
        <Button label="Adopt upstreams" variant="primary" isLoading={busy} onClick={() => void transition('adopt')} />
      </>}>
        <Text as="p">Copy all current upstream groups, servers and routing rules into UI management. Future Helm changes to upstreams will be ignored until you return ownership to GitOps.</Text>
        {error ? <Banner status="error" title="Upstreams could not be adopted" description={error} /> : null}
      </FormDialog>
    </Panel>
  );
}
