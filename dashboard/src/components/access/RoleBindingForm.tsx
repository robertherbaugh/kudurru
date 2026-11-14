// "New role binding" on the Identity tab: one POST /iam/rolebindings that
// creates a public RoleBinding once (409 when the name exists; never an
// adoption). Edit… and Delete… of an API-managed binding read it again and
// send one PUT or DELETE bound to that read's UID and resourceVersion. The scope picker offers the whole cluster, one zone, or a zone
// group: every zone labeled kudurru.io/zonegroup with the group and their
// record sets (D385). The command needs rolebindings.write at cluster scope
// and runs only on the primary of an enrolled SQLite cluster and on the
// complete SQLite runtime (other drivers answer 503; Kubernetes manages
// RoleBindings with kubectl). An unknown outcome is never sent again: the
// SQLite runtime answers an unclassified failure, including an attempted
// commit, with a bare 503 unavailable, so only the missing-runtime 503 counts
// as a refusal (roleBindingUnknown). Create, edit and delete all run through
// useIamCommand: one request at a time, aborted when the dialog closes, and a
// reviewed edit or delete is locked after a refusal. An edit keeps the stored
// labels and operator annotations (never the managed-by annotation), and a
// binding whose scope kind the form does not offer (clientgroup, reserved) is
// not edited here: the form would otherwise send it back as cluster-wide.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { HStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { useQuery } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { qk, useApi } from '../../api/context';
import type { IamRoleBinding, RoleBindingDesired, RoleBindingDocument } from '../../api/types';
import { roleBindingOutcomeUnknown } from '../../lib/cluster';
import { iamOutcomeUnknown, managerOf, operatorAnnotations } from '../../lib/iam';
import { ROLES, SCOPE_KINDS, type ScopeKind } from '../../lib/rbac';
import { zoneGroupError } from '../../lib/zones';
import { EnumSelect, type EnumOption } from '../fields/EnumSelect';
import { ObjectRefPicker } from '../fields/ObjectRefPicker';
import { TextField } from '../fields/TextField';
import { ErrorBanner, useNotify } from '../Feedback';
import { FormDialog } from '../FormDialog';
import { useIamCommand, useIamObjectReason, useReviewRead } from './iamCommand';

const SUBJECT_KINDS: EnumOption[] = [
  { value: 'User', label: 'User', help: 'A local user or a single sign-on user, by name.' },
  { value: 'Group', label: 'Group', help: 'Every member of a group (local groups or the identity provider’s groups claim).' },
  { value: 'ServiceAccount', label: 'Service account', help: 'A kudurru service account in a namespace.' },
];

const DNS_LABEL = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;
const DNS_SUBDOMAIN = /^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$/;

function labelError(v: string, what: string): string | null {
  if (!v) return `Enter a ${what}`;
  return v.length <= 63 && DNS_LABEL.test(v) ? null : 'Lower-case letters, digits and "-", at most 63';
}

function nameError(v: string): string | null {
  if (!v) return 'Enter a name';
  return v.length <= 253 && DNS_SUBDOMAIN.test(v) ? null : 'Lower-case letters, digits, "-" and ".", starting and ending with a letter or digit';
}

export interface RoleBindingDraft {
  name: string;
  namespace: string;
  role: string;
  scopeKind: ScopeKind;
  scopeName: string;
  subjectKind: string;
  subjectName: string;
  subjectNamespace: string;
}

/**
 * Unknown for a role binding command: a refusal when either the IAM rules or
 * the RoleBinding route's missing-runtime 503 says nothing ran.
 */
function roleBindingUnknown(err: unknown): boolean {
  return iamOutcomeUnknown(err) && roleBindingOutcomeUnknown(err);
}

/** Whether the form can edit a stored scope kind: an absent scope is cluster-wide. */
export function editableScope(kind: string | undefined): boolean {
  return !kind || kind === 'cluster' || kind === 'zone' || kind === 'zonegroup';
}

const UNSUPPORTED_SCOPE = 'This form edits cluster, zone and zone group scopes: use kudurru iam rolebindings replace';

/**
 * The manifest POST /iam/rolebindings (or a replace) takes for a draft. A
 * replace is a complete desired document, so it carries the reviewed
 * object's labels and operator annotations, never the managed-by annotation.
 */
export function roleBindingManifest(d: RoleBindingDraft, review?: RoleBindingDocument): RoleBindingDesired {
  const scoped = d.scopeKind !== 'cluster';
  const labels = review?.metadata.labels;
  const annotations = review ? operatorAnnotations(review.metadata) : undefined;
  return {
    apiVersion: 'kudurru.io/v1alpha1',
    kind: 'RoleBinding',
    metadata: { name: d.name.trim(), namespace: d.namespace.trim(), ...(labels && Object.keys(labels).length ? { labels } : {}), ...(annotations ? { annotations } : {}) },
    spec: {
      role: d.role,
      scope: scoped ? { kind: d.scopeKind, name: d.scopeName.trim() } : { kind: 'cluster' },
      subjects: [{ kind: d.subjectKind, name: d.subjectName.trim(), ...(d.subjectKind === 'ServiceAccount' ? { namespace: d.subjectNamespace.trim() } : {}) }],
    },
  };
}

/** The first problem of a draft by field, empty when it can be sent. */
export function draftErrors(d: RoleBindingDraft): Partial<Record<keyof RoleBindingDraft, string>> {
  const out: Partial<Record<keyof RoleBindingDraft, string>> = {};
  const set = (k: keyof RoleBindingDraft, e: string | null) => {
    if (e) out[k] = e;
  };
  set('name', nameError(d.name.trim()));
  set('namespace', labelError(d.namespace.trim(), 'namespace'));
  set('role', d.role ? null : 'Pick a role');
  if (d.scopeKind === 'zone') set('scopeName', d.scopeName ? null : 'Pick a zone');
  if (d.scopeKind === 'zonegroup') set('scopeName', zoneGroupError(d.scopeName.trim()));
  set('subjectName', d.subjectName.trim() ? null : 'Enter a name');
  if (d.subjectKind === 'ServiceAccount') set('subjectNamespace', labelError(d.subjectNamespace.trim(), 'namespace'));
  return out;
}

const EMPTY: RoleBindingDraft = { name: '', namespace: 'default', role: 'zone-admin', scopeKind: 'zonegroup', scopeName: '', subjectKind: 'User', subjectName: '', subjectNamespace: 'default' };

/** "New role binding", disabled with the reason when the session or node cannot create one. */
export function NewRoleBindingButton() {
  const reason = useIamObjectReason()('rolebindings', 'write');
  const [open, setOpen] = useState(false);
  return (
    <>
      <Button label="New role binding" variant="secondary" size="sm" isDisabled={!!reason} tooltip={reason} onClick={() => setOpen(true)} />
      {open ? <RoleBindingDialog onClose={() => setOpen(false)} /> : null}
    </>
  );
}

/** The draft of a stored binding with one subject (the form edits one) and a scope editableScope accepts. */
export function draftOfBinding(doc: RoleBindingDocument): RoleBindingDraft {
  const subject = doc.spec.subjects?.[0];
  const kind = doc.spec.scope?.kind;
  return {
    name: doc.metadata.name ?? '',
    namespace: doc.metadata.namespace ?? '',
    role: doc.spec.role,
    scopeKind: (kind === 'zone' || kind === 'zonegroup' ? kind : 'cluster'),
    scopeName: doc.spec.scope?.name ?? '',
    subjectKind: subject?.kind ?? 'User',
    subjectName: subject?.name ?? '',
    subjectNamespace: subject?.namespace ?? 'default',
  };
}

/** Edit… and Delete… of one RoleBinding object row (D294): each reads the binding again and sends one command bound to that read. */
export function RoleBindingActions({ binding }: { binding: IamRoleBinding }) {
  const reason = useIamObjectReason();
  const [action, setAction] = useState<'edit' | 'delete' | null>(null);
  if (binding.source !== 'object' || !binding.namespace) return null;
  const row = binding.managedBy && binding.managedBy !== 'api' ? `Managed by ${binding.managedBy}: change it there` : undefined;
  const subjects = binding.subjects?.length ?? 0;
  const edit = row ?? (subjects > 1 ? `It has ${subjects} subjects and this form edits one: use kudurru iam rolebindings replace` : undefined) ?? (editableScope(binding.scope?.kind) ? undefined : UNSUPPORTED_SCOPE) ?? reason('rolebindings', 'write');
  const del = row ?? reason('rolebindings', 'delete');
  const label = `${binding.namespace}/${binding.name}`;
  return (
    <HStack gap={1} wrap="wrap">
      <Button label={`Edit ${label}`} size="sm" variant="ghost" isDisabled={!!edit} tooltip={edit} onClick={() => setAction('edit')}>
        Edit…
      </Button>
      <Button label={`Delete ${label}`} size="sm" variant="ghost" isDisabled={!!del} tooltip={del} onClick={() => setAction('delete')}>
        Delete…
      </Button>
      {action ? <ReviewedBindingDialog namespace={binding.namespace} name={binding.name} action={action} onClose={() => setAction(null)} /> : null}
    </HStack>
  );
}

function ReviewedBindingDialog({ namespace, name, action, onClose }: { namespace: string; name: string; action: 'edit' | 'delete'; onClose: () => void }) {
  const api = useApi();
  const review = useReviewRead((signal) => api.iam.roleBinding(namespace, name, signal));
  const title = action === 'edit' ? `Edit role binding ${namespace}/${name}` : `Delete role binding ${namespace}/${name}?`;
  const doc = review.data;
  const multi = action === 'edit' && (doc?.spec.subjects?.length ?? 0) > 1;
  const scopeOff = action === 'edit' && !!doc && !editableScope(doc.spec.scope?.kind);
  if (!doc || managerOf(doc.metadata) !== 'api' || multi || scopeOff) {
    return (
      <FormDialog isOpen onClose={onClose} title={title} width={520} footer={<Button label="Close" variant="ghost" onClick={onClose} />}>
        {review.error ? (
          <ErrorBanner error={review.error} title="The role binding could not be read" />
        ) : !doc ? (
          <Text>Reading the role binding…</Text>
        ) : managerOf(doc.metadata) !== 'api' ? (
          <Banner status="info" title="Managed elsewhere" description={`This binding belongs to ${managerOf(doc.metadata) || 'no API manager'}. Change it where it is declared.`} />
        ) : multi ? (
          <Banner status="info" title="More than one subject" description="This form edits a binding with one subject. Use kudurru iam rolebindings replace for this one." />
        ) : (
          <Banner status="info" title="Scope this form does not edit" description={`Its scope kind is ${doc.spec.scope?.kind ?? ''}. ${UNSUPPORTED_SCOPE}. Nothing was changed.`} />
        )}
      </FormDialog>
    );
  }
  return action === 'edit' ? <RoleBindingDialog review={doc} onClose={onClose} /> : <DeleteBindingDialog doc={doc} title={title} onClose={onClose} />;
}

function DeleteBindingDialog({ doc, title, onClose }: { doc: RoleBindingDocument; title: string; onClose: () => void }) {
  const api = useApi();
  const notify = useNotify();
  const cmd = useIamCommand('role binding', { isUnknown: roleBindingUnknown, floor: false });
  const submit = async () => {
    const r = await cmd.run((signal) => api.iam.deleteRoleBinding(doc.metadata.namespace ?? '', doc.metadata.name ?? '', { uid: doc.metadata.uid ?? '', resourceVersion: doc.metadata.resourceVersion ?? '' }, signal));
    if (r.ok) {
      notify.success(`Deleted role binding ${doc.metadata.namespace}/${doc.metadata.name}`);
      onClose();
    }
  };
  return (
    <FormDialog
      isOpen
      onClose={onClose}
      title={title}
      width={480}
      onSubmit={() => void submit()}
      footer={
        <>
          <Button label={cmd.locked ? 'Close' : 'Cancel'} variant="ghost" onClick={onClose} />
          <Button type="submit" label="Delete role binding" variant="destructive" isLoading={cmd.pending} isDisabled={cmd.locked} />
        </>
      }
    >
      <Text as="p">
        Role {doc.spec.role} for {(doc.spec.subjects ?? []).map((s) => `${s.kind} ${s.namespace ? `${s.namespace}/` : ''}${s.name}`).join(', ')}. The subjects lose what it granted at once, on every node; their sessions and tokens stay but no longer carry the role.
      </Text>
      {cmd.outcome}
    </FormDialog>
  );
}

function RoleBindingDialog({ onClose, review }: { onClose: () => void; review?: RoleBindingDocument }) {
  const api = useApi();
  const notify = useNotify();
  const [initial] = useState<RoleBindingDraft>(() => (review ? draftOfBinding(review) : EMPTY));
  const [d, setD] = useState<RoleBindingDraft>(initial);
  const [touched, setTouched] = useState(false);
  // A create may be sent again after a refusal once the draft changed; a
  // reviewed edit is bound to its read and locked after one.
  const cmd = useIamCommand('role binding', { retryAfterRefusal: !review, isUnknown: roleBindingUnknown, floor: false });
  const zones = useQuery({ queryKey: qk.zones, queryFn: () => api.zones.list() });
  const groups = useMemo(() => [...new Set((zones.data?.items ?? []).map((z) => z.zoneGroup).filter((g): g is string => !!g))].sort(), [zones.data]);
  const errors = draftErrors(d);
  const changed = !review || JSON.stringify(d) !== JSON.stringify(initial);
  const shown = (k: keyof RoleBindingDraft) => (touched ? errors[k] : undefined);
  const update = (patch: Partial<RoleBindingDraft>) => setD((prev) => ({ ...prev, ...patch }));
  const submit = async () => {
    setTouched(true);
    if (Object.keys(errors).length || !changed) return;
    const binding = roleBindingManifest(d, review);
    const r = await cmd.run((signal) =>
      review
        ? api.iam.replaceRoleBinding(d.namespace.trim(), d.name.trim(), { uid: review.metadata.uid ?? '', resourceVersion: review.metadata.resourceVersion ?? '', binding }, signal)
        : api.iam.createRoleBinding(binding, signal),
    );
    if (r.ok) {
      notify.success(`${review ? 'Updated' : 'Created'} role binding ${r.value.metadata.namespace ?? d.namespace}/${r.value.metadata.name ?? d.name}`);
      onClose();
    }
  };
  const groupHelp = groups.length ? `Zone groups in use: ${groups.join(', ')}. A group with no zones yet grants nothing until a zone carries its label.` : 'No zone carries a kudurru.io/zonegroup label yet; the binding grants nothing until one does.';
  return (
    <FormDialog
      isOpen
      onClose={onClose}
      title={review ? `Edit role binding ${review.metadata.namespace}/${review.metadata.name}` : 'New role binding'}
      width={540}
      onSubmit={() => void submit()}
      footer={
        <>
          <Button label={cmd.locked ? 'Close' : 'Cancel'} variant="ghost" onClick={onClose} />
          <Button type="submit" label={review ? 'Save changes' : 'Create'} variant="primary" isLoading={cmd.pending} isDisabled={cmd.locked || !changed} />
        </>
      }
    >
      <TextField label="Name" value={d.name} onChange={(v) => update({ name: v })} mono isRequired isDisabled={!!review} placeholder="prod-zone-admins" error={shown('name')} />
      <TextField label="Namespace" value={d.namespace} onChange={(v) => update({ namespace: v })} mono isRequired isDisabled={!!review} error={shown('namespace')} />
      <EnumSelect label="Role" value={d.role} options={ROLES} onChange={(v) => update({ role: v })} error={shown('role')} />
      <EnumSelect label="Scope" value={d.scopeKind} options={SCOPE_KINDS} onChange={(v) => update({ scopeKind: v as ScopeKind, scopeName: '' })} />
      {d.scopeKind === 'zone' ? <ObjectRefPicker kind="dnszones" label="Zone" value={d.scopeName ? [d.scopeName] : []} onChange={(v) => update({ scopeName: v[0] ?? '' })} {...(shown('scopeName') ? { error: shown('scopeName') } : {})} /> : null}
      {d.scopeKind === 'zonegroup' ? <TextField label="Zone group" value={d.scopeName} onChange={(v) => update({ scopeName: v })} mono isRequired placeholder="prod" help={groupHelp} error={shown('scopeName')} /> : null}
      <EnumSelect label="Subject" value={d.subjectKind} options={SUBJECT_KINDS} onChange={(v) => update({ subjectKind: v })} />
      <TextField label={d.subjectKind === 'Group' ? 'Group name' : d.subjectKind === 'ServiceAccount' ? 'Service account name' : 'User name'} value={d.subjectName} onChange={(v) => update({ subjectName: v })} mono isRequired error={shown('subjectName')} />
      {d.subjectKind === 'ServiceAccount' ? <TextField label="Service account namespace" value={d.subjectNamespace} onChange={(v) => update({ subjectNamespace: v })} mono isRequired error={shown('subjectNamespace')} /> : null}
      {d.scopeKind === 'zonegroup' ? (
        <Text type="supporting" as="p">
          Moving a zone into or out of a group (its label) needs zones.write at cluster scope, so a zone admin of the group cannot hand zones to another group.
        </Text>
      ) : null}
      {cmd.outcome}
    </FormDialog>
  );
}
