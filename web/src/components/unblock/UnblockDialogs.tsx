// The two decisions on an unblock request, short single-purpose dialogs:
// Approve (scope, match, duration, note) and Deny (note). Mount them only
// while a request is chosen (keyed by it), so every opening starts from a
// fresh draft.
import { Button } from '@astryxdesign/core/Button';
import { TextArea } from '@astryxdesign/core/TextArea';
import { useState } from 'react';
import { useApproveUnblock, useDenyUnblock } from '../../api/hooks';
import type { UnblockDecisionResult, UnblockMatch, UnblockRequest, UnblockScope } from '../../api/types';
import { formatDateTime } from '../../lib/format';
import { shortRef } from '../../lib/policies';
import {
  APPROVAL_DURATIONS,
  NOTE_MAX,
  allowListName,
  approvalBody,
  approvalDraft,
  approvalDurationError,
  matchOptions,
  scopeOptions,
  type ApprovalDraft,
} from '../../lib/unblock';
import { useNotify } from '../Feedback';
import { DurationField } from '../fields/DurationField';
import { EnumSelect } from '../fields/EnumSelect';
import { FormDialog } from '../FormDialog';

/** The success toast of an approval: where the entry went, and the policies left alone. */
export function approvalMessage(res: UnblockDecisionResult): string {
  const r = res.request;
  const allow = r.allow;
  const list = allow ? shortRef(allow.list) : 'ui-allow';
  const who = allow?.scope === 'group' && allow.group ? `for ${shortRef(allow.group)}` : 'for everyone';
  const until = allow?.expiresAt ? ` until ${formatDateTime(allow.expiresAt)}` : '';
  const parts = [`Allowed ${r.domain} ${who}${until} (list ${list})`];
  if (res.attached.length) parts.push(`${list} attached to ${res.attached.map(shortRef).join(', ')}`);
  if (res.skipped.length) parts.push(`not attached to ${res.skipped.map(shortRef).join(', ')}: managed elsewhere, add the list there`);
  return `${parts.join('. ')}.`;
}

function NoteField({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  return (
    <TextArea
      label="Note"
      isOptional
      description={`Shown to the user on the block page (at most ${NOTE_MAX} characters).`}
      value={value}
      onChange={(v) => onChange(v.slice(0, NOTE_MAX))}
      maxLength={NOTE_MAX}
      rows={3}
    />
  );
}

export function ApproveDialog({ request: r, onClose, onDone }: { request: UnblockRequest; onClose: () => void; onDone?: (res: UnblockDecisionResult) => void }) {
  const [d, setD] = useState<ApprovalDraft>(() => approvalDraft(r));
  const approve = useApproveUnblock();
  const notify = useNotify();
  const set = (patch: Partial<ApprovalDraft>) => setD((prev) => ({ ...prev, ...patch }));
  const durErr = approvalDurationError(d.duration);
  const submit = () => {
    if (durErr) return;
    approve.mutate(
      { namespace: r.namespace, name: r.name, body: approvalBody(d) },
      {
        onSuccess: (res) => {
          notify.success(approvalMessage(res));
          onDone?.(res);
          onClose();
        },
        onError: (err) => notify.error(err, `Could not approve ${r.domain}`),
      },
    );
  };
  const groupOptions = r.groups.map((g) => ({ value: g, label: shortRef(g) }));
  return (
    <FormDialog
      isOpen
      onClose={onClose}
      title={`Allow ${r.domain}?`}
      subtitle="Adds an allow entry; every node recompiles its lists and forgets the cached answer."
      onSubmit={submit}
      footer={
        <>
          <Button label="Cancel" variant="ghost" onClick={onClose} />
          <Button type="submit" label="Approve" variant="primary" isDisabled={!!durErr} isLoading={approve.isPending} />
        </>
      }
    >
      <EnumSelect label="Scope" value={d.scope} options={scopeOptions(r, d.group || undefined)} onChange={(v) => set({ scope: v as UnblockScope })} />
      {d.scope === 'group' && r.groups.length > 1 ? (
        <EnumSelect
          label="Group"
          value={d.group}
          options={groupOptions}
          description="The requester is in several client groups; the entry goes to this group's list."
          onChange={(v) => set({ group: v })}
        />
      ) : null}
      <EnumSelect label="Match" value={d.match} options={matchOptions(r.domain)} onChange={(v) => set({ match: v as UnblockMatch })} />
      <DurationField
        label="Duration"
        value={d.duration}
        onChange={(v) => set({ duration: v })}
        presets={APPROVAL_DURATIONS}
        emptyLabel="Permanently"
        description={
          d.duration
            ? `The entry leaves ${allowListName(d.scope, d.group)} when the approval ends.`
            : `The entry stays on ${allowListName(d.scope, d.group)} until you deny the request or remove it on the Domains page.`
        }
        error={durErr ?? undefined}
      />
      <NoteField value={d.note} onChange={(note) => set({ note })} />
    </FormDialog>
  );
}

export function DenyDialog({ request: r, onClose, onDone }: { request: UnblockRequest; onClose: () => void; onDone?: (res: UnblockDecisionResult) => void }) {
  const [note, setNote] = useState('');
  const deny = useDenyUnblock();
  const notify = useNotify();
  const revoking = r.state === 'Approved';
  const submit = () =>
    deny.mutate(
      { namespace: r.namespace, name: r.name, body: note.trim() ? { note: note.trim() } : {} },
      {
        onSuccess: (res) => {
          notify.success(revoking ? `Revoked the approval of ${r.domain}` : `Denied ${r.domain}`);
          onDone?.(res);
          onClose();
        },
        onError: (err) => notify.error(err, `Could not deny ${r.domain}`),
      },
    );
  return (
    <FormDialog
      isOpen
      onClose={onClose}
      title={revoking ? `Revoke the approval of ${r.domain}?` : `Deny ${r.domain}?`}
      width={480}
      onSubmit={submit}
      footer={
        <>
          <Button label="Cancel" variant="ghost" onClick={onClose} />
          <Button type="submit" label="Deny" variant="destructive" isLoading={deny.isPending} />
        </>
      }
    >
      {revoking && r.allow ? (
        <p className="k-small">
          The approval is in force.{' '}
          {r.allow.added ? (
            <>
              <span className="k-mono">{r.allow.entry}</span> leaves {shortRef(r.allow.list)} and the name is blocked again.
            </>
          ) : (
            <>
              <span className="k-mono">{r.allow.entry}</span> was on {shortRef(r.allow.list)} before the approval, so it stays there.
            </>
          )}
        </p>
      ) : (
        <p className="k-small">The name stays blocked. The user sees the decision on the block page.</p>
      )}
      <NoteField value={note} onChange={setNote} />
    </FormDialog>
  );
}
