// The controls of one Integration, built from its provider's manifest
// (lib/integrations): the provider block's fields from the configuration
// schema (ShapeField), the shared fields (resync, deletion policy,
// suspend) and one reference per credential key the provider reads. A
// credential is a file path or an environment name of a form this node
// resolves; there is no control for a credential value.
import { Banner } from '@astryxdesign/core/Banner';
import { Grid } from '@astryxdesign/core/Grid';
import { VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import type { IntegrationManifest, IntegrationManifests } from '../../api/types';
import {
  CREDENTIAL_FORM_LABELS,
  credentialRefError,
  DELETION_POLICY_OPTIONS,
  humanize,
  isDestinationField,
  notOfferedFields,
  providerShape,
  type CredentialDraft,
  type CredentialForm,
  type IntegrationDraft,
} from '../../lib/integrations';
import { DurationField } from '../fields/DurationField';
import { EnumSelect } from '../fields/EnumSelect';
import { FieldGroup } from '../fields/FieldGroup';
import { ShapeField } from '../fields/ShapeField';
import { SwitchField, TextField } from '../fields/TextField';

export interface SettingsFieldsProps {
  manifest: IntegrationManifest;
  draft: IntegrationDraft;
  onChange: (d: IntegrationDraft) => void;
  isDisabled?: boolean | undefined;
  /** Set when the destination fields may not change (bound credentials, no integrations.secrets). */
  destinationLockReason?: string | undefined;
}

/** The provider block and the shared fields of an instance. */
export function SettingsFields({ manifest, draft, onChange, isDisabled, destinationLockReason }: SettingsFieldsProps) {
  const shape = providerShape(manifest);
  const notOffered = notOfferedFields(manifest);
  const config = (draft.config && typeof draft.config === 'object' ? draft.config : {}) as Record<string, unknown>;
  return (
    <VStack gap={4}>
      <FieldGroup label={`${manifest.displayName} settings`} data-testid="provider-fields">
        {shape.fields?.length ? (
          <Grid columns={{ minWidth: 240, repeat: 'fit' }} gap={3} align="start">
            {(shape.fields ?? []).map((f) => {
              const locked = !!destinationLockReason && isDestinationField(manifest, f.name);
              return (
                <ShapeField
                  key={f.name}
                  label={`${f.label ?? f.name}${f.required ? '' : ' (optional)'}`}
                  description={locked ? destinationLockReason : f.help}
                  shape={f}
                  value={config[f.name]}
                  onChange={(v) => onChange({ ...draft, config: { ...config, [f.name]: v } })}
                  isDisabled={isDisabled || locked}
                />
              );
            })}
          </Grid>
        ) : (
          <Text type="supporting">This provider has no settings of its own.</Text>
        )}
        {notOffered.length ? (
          <VStack gap={1} data-testid="refused-fields">
            {notOffered.map((g) => (
              <Text key={`${g.effect} ${g.reason} ${g.message}`} type="supporting" display="block">
                {g.effect === 'refused' ? 'Not offered on this node (refused)' : 'Not offered on this node (no effect here)'}: {g.fields.join(', ')}. {g.message}
                {g.reason ? ` (${g.reason})` : ''}
              </Text>
            ))}
          </VStack>
        ) : null}
      </FieldGroup>
      <Grid columns={{ minWidth: 220, repeat: 'fit' }} gap={3} align="start">
        <DurationField
          label="Resync interval"
          value={draft.resync}
          onChange={(v) => onChange({ ...draft, resync: v })}
          emptyLabel="Provider default"
          description="How often the host runs the instance again on its own."
          isDisabled={isDisabled}
        />
        <EnumSelect
          label="When deleted"
          value={draft.deletionPolicy}
          options={DELETION_POLICY_OPTIONS}
          onChange={(v) => onChange({ ...draft, deletionPolicy: v === 'Delete' ? 'Delete' : 'Retain' })}
          isDisabled={isDisabled}
        />
        <SwitchField
          label="Suspended"
          help="A suspended instance makes no provider request."
          value={draft.suspend}
          onChange={(v: boolean) => onChange({ ...draft, suspend: v })}
          isDisabled={isDisabled}
        />
      </Grid>
    </VStack>
  );
}

export interface CredentialFieldsProps {
  manifest: IntegrationManifest;
  manifests: IntegrationManifests;
  draft: IntegrationDraft;
  onChange: (d: IntegrationDraft) => void;
  isDisabled?: boolean | undefined;
  /** Why the references cannot be changed (no integrations.secrets). */
  disabledReason?: string | undefined;
  showErrors?: boolean;
}

/** One reference per credential key of the provider: references only, never values. */
export function CredentialFields({ manifest, manifests, draft, onChange, isDisabled, disabledReason, showErrors }: CredentialFieldsProps) {
  const forms = manifests.credentialSources.filter((f): f is CredentialForm => f === 'file' || f === 'env');
  const set = (name: string, cd: CredentialDraft) => onChange({ ...draft, credentials: { ...draft.credentials, [name]: cd } });
  const disabled = isDisabled || !!disabledReason;
  return (
    <VStack gap={3} data-testid="credential-fields">
      <Text type="supporting">
        A credential is named by reference: a file on the node or an environment variable that the node configuration grants
        (credentials.allowedFiles, credentials.allowedEnvironment). Its value is never entered here, stored in the Integration or shown.
      </Text>
      {disabledReason ? <Banner status="info" title="References are read-only" description={disabledReason} /> : null}
      {!forms.length ? (
        <Banner
          status="warning"
          title="This node resolves no credential references"
          description="Grant the files or variable names an Integration may use under credentials.allowedFiles or credentials.allowedEnvironment in the node configuration, then restart the node."
        />
      ) : null}
      {manifest.credentials.length === 0 ? <Text type="supporting">This provider reads no credentials.</Text> : null}
      {manifest.credentials.map((c) => {
        const cd = draft.credentials[c.name] ?? { source: '', value: '' };
        const label = humanize(c.name);
        if (cd.secretRef) {
          return (
            <FieldGroup key={c.name} label={label}>
              <Text>
                Secret {cd.secretRef.name}, key {cd.secretRef.key}
              </Text>
              <Text type="supporting">Secret references are kept as they are; neither host resolves them for an Integration. Mount the Secret as a file instead.</Text>
            </FieldGroup>
          );
        }
        const grants = cd.source === 'file' ? manifests.allowedFiles : cd.source === 'env' ? manifests.allowedEnvironment : undefined;
        const err = showErrors ? (credentialRefError(cd) ?? (c.required && !cd.source ? 'Required' : undefined)) : undefined;
        const options = [
          ...forms.map((f) => ({ value: f, label: CREDENTIAL_FORM_LABELS[f] })),
          // A stored form this node no longer resolves stays visible.
          ...(cd.source && !forms.includes(cd.source) ? [{ value: cd.source, label: `${CREDENTIAL_FORM_LABELS[cd.source]} (not resolved here)` }] : []),
        ];
        return (
          <FieldGroup key={c.name} label={`${label}${c.required ? '' : ' (optional)'}`} data-testid={`credential-${c.name}`}>
            <Grid columns={{ minWidth: 220, repeat: 'fit' }} gap={3} align="start">
              <EnumSelect
                label={`${label} reference`}
                value={cd.source}
                options={options}
                emptyLabel="Not set"
                onChange={(v) => set(c.name, { source: v === 'file' || v === 'env' ? v : '', value: '' })}
                isDisabled={disabled}
                description={c.verify ? `Check proves it with ${c.verify}.` : undefined}
              />
              {cd.source && grants?.length ? (
                <EnumSelect
                  label={cd.source === 'file' ? `${label} file` : `${label} variable`}
                  value={cd.value}
                  options={grants.map((g) => ({ value: g, label: g }))}
                  onChange={(v) => set(c.name, { ...cd, value: v })}
                  isDisabled={disabled}
                  error={err ?? undefined}
                />
              ) : cd.source ? (
                <TextField
                  label={cd.source === 'file' ? `${label} file` : `${label} variable`}
                  value={cd.value}
                  onChange={(v: string) => set(c.name, { ...cd, value: v })}
                  mono
                  placeholder={cd.source === 'file' ? '/run/secrets/token' : 'PROVIDER_TOKEN'}
                  help={cd.source === 'file' ? 'An absolute path the node configuration grants.' : 'A variable name the node configuration grants.'}
                  isDisabled={disabled}
                  error={err ?? undefined}
                />
              ) : null}
            </Grid>
          </FieldGroup>
        );
      })}
    </VStack>
  );
}
