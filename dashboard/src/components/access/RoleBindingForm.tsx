// "New role binding" on the Identity tab: one POST /iam/rolebindings that
// creates a public RoleBinding once (409 when the name exists; never an
// adoption). The scope picker offers the whole cluster, one zone, or a zone
// group: every zone labeled kudurru.io/zonegroup with the group and their
// record sets (D385). The command needs rolebindings.write at cluster scope
// and runs only on the primary of an enrolled SQLite cluster and on the
// complete SQLite runtime (other drivers answer 503; Kubernetes manages
// RoleBindings with kubectl). An unknown outcome is never sent again: the
// SQLite runtime answers an unclassified failure, including an attempted
// commit, with a bare 503 unavailable, so only the missing-runtime 503 counts
// as a refusal (roleBindingOutcomeUnknown).
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { Text } from '@astryxdesign/core/Text';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useMemo, useRef, useState } from 'react';
import { qk, useApi, useCan, useCannotReason } from '../../api/context';
import type { RoleBindingDesired } from '../../api/types';
import { roleBindingOutcomeUnknown } from '../../lib/cluster';
import { ROLES, SCOPE_KINDS, type ScopeKind } from '../../lib/rbac';
import { zoneGroupError } from '../../lib/zones';
import { EnumSelect, type EnumOption } from '../fields/EnumSelect';
import { ObjectRefPicker } from '../fields/ObjectRefPicker';
import { TextField } from '../fields/TextField';
import { ErrorBanner, errorText, useNotify } from '../Feedback';
import { FormDialog } from '../FormDialog';

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

/** The manifest POST /iam/rolebindings takes for a draft. */
export function roleBindingManifest(d: RoleBindingDraft): RoleBindingDesired {
  const scoped = d.scopeKind !== 'cluster';
  return {
    apiVersion: 'kudurru.io/v1alpha1',
    kind: 'RoleBinding',
    metadata: { name: d.name.trim(), namespace: d.namespace.trim() },
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
  const can = useCan();
  const cannot = useCannotReason();
  const [open, setOpen] = useState(false);
  const allowed = can('rolebindings', 'write');
  return (
    <>
      <Button label="New role binding" variant="secondary" size="sm" isDisabled={!allowed} tooltip={allowed ? undefined : cannot('rolebindings', 'write')} onClick={() => setOpen(true)} />
      {open ? <RoleBindingDialog onClose={() => setOpen(false)} /> : null}
    </>
  );
}

function RoleBindingDialog({ onClose }: { onClose: () => void }) {
  const api = useApi();
  const qc = useQueryClient();
  const notify = useNotify();
  const [d, setD] = useState<RoleBindingDraft>(EMPTY);
  const [touched, setTouched] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<unknown>();
  const [unknown, setUnknown] = useState(false);
  const inFlight = useRef(false);
  const zones = useQuery({ queryKey: qk.zones, queryFn: () => api.zones.list() });
  const groups = useMemo(() => [...new Set((zones.data?.items ?? []).map((z) => z.zoneGroup).filter((g): g is string => !!g))].sort(), [zones.data]);
  const errors = draftErrors(d);
  const shown = (k: keyof RoleBindingDraft) => (touched ? errors[k] : undefined);
  const update = (patch: Partial<RoleBindingDraft>) => setD((prev) => ({ ...prev, ...patch }));
  const submit = async () => {
    setTouched(true);
    if (Object.keys(errors).length || inFlight.current || unknown) return;
    inFlight.current = true;
    setPending(true);
    setError(undefined);
    try {
      const doc = await api.iam.createRoleBinding(roleBindingManifest(d));
      notify.success(`Created role binding ${doc.metadata.namespace ?? d.namespace}/${doc.metadata.name ?? d.name}`);
      onClose();
    } catch (err) {
      setError(err);
      if (roleBindingOutcomeUnknown(err)) setUnknown(true);
    } finally {
      inFlight.current = false;
      setPending(false);
      void qc.invalidateQueries({ queryKey: ['iam', 'rb'] });
    }
  };
  const groupHelp = groups.length ? `Zone groups in use: ${groups.join(', ')}. A group with no zones yet grants nothing until a zone carries its label.` : 'No zone carries a kudurru.io/zonegroup label yet; the binding grants nothing until one does.';
  return (
    <FormDialog
      isOpen
      onClose={onClose}
      title="New role binding"
      width={540}
      onSubmit={() => void submit()}
      footer={
        <>
          <Button label={unknown ? 'Close' : 'Cancel'} variant="ghost" onClick={onClose} />
          <Button type="submit" label="Create" variant="primary" isLoading={pending} isDisabled={unknown} />
        </>
      }
    >
      <TextField label="Name" value={d.name} onChange={(v) => update({ name: v })} mono isRequired placeholder="prod-zone-admins" error={shown('name')} />
      <TextField label="Namespace" value={d.namespace} onChange={(v) => update({ namespace: v })} mono isRequired error={shown('namespace')} />
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
      {error !== undefined ? (
        unknown ? (
          <Banner status="warning" title="Outcome unknown" description={`The dashboard cannot tell whether the binding was created and does not send it again. Close this dialog and check the role bindings list. The server said: ${errorText(error)}`} />
        ) : (
          <ErrorBanner error={error} />
        )
      ) : null}
    </FormDialog>
  );
}
