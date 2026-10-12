// The short dialogs of the Allowlist and Denylist: add entries (type with
// help per type, one or many values with per-type validation and
// examples, everyone or some client groups, a comment), edit one entry
// (type, value, scope, comment, enabled) and confirm a delete. Writes go
// to POST /allowlist|/denylist and PATCH …/{id}; the answer's skipped
// policies are reported, since their lists cannot be attached there.
import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { List, ListItem } from '@astryxdesign/core/List';
import { Text } from '@astryxdesign/core/Text';
import { VStack } from '@astryxdesign/core/VStack';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useState } from 'react';
import { qk, useApi } from '../../api/context';
import type { DomainAction, DomainEntry, DomainEntryPatch, DomainEntryType, DomainWriteResult } from '../../api/types';
import { ACTION_LABEL, ACTION_OPTIONS, firstValueError, normaliseValue, skippedText, splitValues, TYPE_OPTIONS, TYPE_PLACEHOLDER, valueError } from '../../lib/domains';
import { ObjectRefPicker } from '../fields/ObjectRefPicker';
import { SelectField, type SelectOption } from '../fields/SelectField';
import { SwitchField, TextAreaField, TextField } from '../fields/TextField';
import { useNotify } from '../Feedback';
import { FormDialog } from '../FormDialog';

type Scope = 'everyone' | 'groups';

const SCOPE_OPTIONS: SelectOption[] = [
  { value: 'everyone', label: 'Everyone', help: 'Every client of the policies of the namespace.' },
  { value: 'groups', label: 'Some client groups', help: 'Only the clients of the chosen groups (through the policies that cover them).' },
];

/** Refreshes everything an entry write can change: the entry pages, the lists behind them and the policies they were attached to. */
export function useInvalidateDomains(): () => void {
  const qc = useQueryClient();
  return () => {
    void qc.invalidateQueries({ queryKey: qk.domains() });
    void qc.invalidateQueries({ queryKey: qk.objects('blocklists') });
    void qc.invalidateQueries({ queryKey: qk.objects('policies') });
  };
}

/** The toast text of a write: what happened, the policies the list was attached to and the ones it could not be. */
export function writeSummary(head: string, r: Pick<DomainWriteResult, 'attached' | 'skipped'> | undefined, action: DomainAction): string {
  const parts = [head];
  if (r?.attached?.length) parts.push(`Now used by ${r.attached.join(', ')}.`);
  if (r?.skipped?.length) parts.push(skippedText(r.skipped, action));
  return parts.join(' ');
}

function ScopeFields({ scope, groups, onScope, onGroups, showError, isDisabled }: { scope: Scope; groups: string[]; onScope: (s: Scope) => void; onGroups: (g: string[]) => void; showError: boolean; isDisabled?: boolean }) {
  return (
    <>
      <SelectField label="Applies to" value={scope} options={SCOPE_OPTIONS} onChange={(v) => onScope(v as Scope)} isDisabled={isDisabled} />
      {scope === 'groups' ? (
        <ObjectRefPicker
          kind="clientgroups"
          label="Client groups"
          multiple
          value={groups}
          onChange={onGroups}
          isDisabled={isDisabled}
          description="A group needs a policy that covers it; the entry joins that policy's lists."
          error={showError && !groups.length ? 'Choose at least one group' : undefined}
        />
      ) : null}
    </>
  );
}

export interface AddEntryDialogProps {
  open: boolean;
  onClose: () => void;
  /** Fixed on the focused pages; the combined page and the Lists page let the user choose. */
  action?: DomainAction;
  defaultAction?: DomainAction;
  defaultType?: DomainEntryType;
  namespace?: string;
}

export function AddEntryDialog({ open, onClose, action: fixed, defaultAction = 'deny', defaultType = 'exact', namespace = 'default' }: AddEntryDialogProps) {
  const api = useApi();
  const notify = useNotify();
  const invalidate = useInvalidateDomains();
  const [action, setAction] = useState<DomainAction>(fixed ?? defaultAction);
  const [type, setType] = useState<DomainEntryType>(defaultType);
  const [text, setText] = useState('');
  const [scope, setScope] = useState<Scope>('everyone');
  const [groups, setGroups] = useState<string[]>([]);
  const [comment, setComment] = useState('');
  const [tried, setTried] = useState(false);
  const [rejected, setRejected] = useState<{ item: string; error: string }[]>([]);
  const act = fixed ?? action;
  const values = splitValues(type, text);
  const bad = firstValueError(type, values);
  const scopeOK = scope === 'everyone' || groups.length > 0;
  const stored = type === 'wildcard' && values.length && !bad ? values.map((v) => normaliseValue(type, v)) : [];

  const reset = () => {
    setText('');
    setComment('');
    setTried(false);
    setRejected([]);
  };
  const close = () => {
    reset();
    onClose();
  };

  const m = useMutation({
    mutationFn: () =>
      api.domains.create(act, {
        values,
        type,
        namespace,
        ...(scope === 'groups' ? { groups } : {}),
        ...(comment.trim() ? { comment: comment.trim() } : {}),
        origin: 'manual',
      }),
    onSuccess: (r) => {
      const ok = r.processed?.success.length ?? r.items.length;
      const errors = r.processed?.errors ?? [];
      if (ok) notify.success(writeSummary(`Added ${ok} to the ${ACTION_LABEL[act]}.`, r, act));
      invalidate();
      if (errors.length) {
        // Keep the rejected values in the box so they can be fixed.
        setRejected(errors.map((e) => ({ item: e.item, error: e.error ?? 'rejected' })));
        setText(errors.map((e) => e.item).join('\n'));
      } else {
        close();
      }
    },
    onError: (err) => notify.error(err, `Could not add to the ${ACTION_LABEL[act]}`),
  });

  const submit = () => {
    setTried(true);
    if (values.length && !bad && scopeOK) m.mutate();
  };

  return (
    <FormDialog
      isOpen={open}
      onClose={close}
      title={fixed ? `Add to the ${ACTION_LABEL[fixed]}` : 'Add domains'}
      width={560}
      onSubmit={submit}
      noValidate
      footer={
        <>
          <Button label="Cancel" variant="ghost" onClick={close} />
          <Button type="submit" label={values.length > 1 ? `Add ${values.length}` : 'Add'} variant="primary" isDisabled={!values.length || !!bad} isLoading={m.isPending} />
        </>
      }
    >
      <VStack gap={4}>
        {!fixed ? <SelectField label="Action" value={action} options={ACTION_OPTIONS} onChange={(v) => setAction(v as DomainAction)} /> : null}
        <SelectField
          label="Type"
          value={type}
          options={TYPE_OPTIONS}
          onChange={(v) => {
            setType(v as DomainEntryType);
            setRejected([]);
          }}
        />
        <TextAreaField
          label={type === 'regex' ? 'Expressions' : 'Domains'}
          help={
            stored.length
              ? `Saved as ${stored.slice(0, 3).join(', ')}${stored.length > 3 ? ` and ${stored.length - 3} more` : ''}.`
              : type === 'regex'
                ? 'One expression per line.'
                : 'One per line, or separated by spaces or commas.'
          }
          placeholder={TYPE_PLACEHOLDER[type]}
          rows={4}
          hasSpellCheck={false}
          mono
          value={text}
          onChange={(v) => {
            setText(v);
            setRejected([]);
          }}
          error={bad ? `${bad.value}: ${bad.error}` : tried && !values.length ? 'Enter at least one value' : undefined}
        />
        <ScopeFields scope={scope} groups={groups} onScope={setScope} onGroups={setGroups} showError={tried} />
        <TextField label="Comment" isOptional value={comment} onChange={(v) => setComment(v.slice(0, 500))} placeholder="Why this entry exists" />
        {rejected.length ? (
          <Banner status="warning" title={`${rejected.length} not added`} collapsible={false}>
            <List density="compact">
              {rejected.slice(0, 10).map((e) => (
                <ListItem key={e.item} label={e.item} description={e.error} />
              ))}
            </List>
          </Banner>
        ) : null}
      </VStack>
    </FormDialog>
  );
}

export function EditEntryDialog({ entry, onClose }: { entry: DomainEntry; onClose: () => void }) {
  const api = useApi();
  const notify = useNotify();
  const invalidate = useInvalidateDomains();
  const initialScope: Scope = entry.everyone ? 'everyone' : 'groups';
  const [type, setType] = useState<DomainEntryType>(entry.type);
  const [value, setValue] = useState(entry.value);
  const [scope, setScope] = useState<Scope>(initialScope);
  const [groups, setGroups] = useState<string[]>(entry.groups);
  const [comment, setComment] = useState(entry.comment ?? '');
  const [enabled, setEnabled] = useState(entry.enabled);
  const [tried, setTried] = useState(false);
  const err = valueError(type, value);
  const scopeOK = scope === 'everyone' || groups.length > 0;

  const patch = (): DomainEntryPatch => {
    const p: DomainEntryPatch = {};
    const v = normaliseValue(type, value);
    if (type !== entry.type) p.type = type;
    if (v !== entry.value || type !== entry.type) p.value = v;
    if (scope !== initialScope || (scope === 'groups' && [...groups].sort().join() !== [...entry.groups].sort().join())) {
      p.scope = scope;
      if (scope === 'groups') p.groups = groups;
    }
    if (comment.trim() !== (entry.comment ?? '')) p.comment = comment.trim();
    if (enabled !== entry.enabled) p.enabled = enabled;
    return p;
  };
  const changes = patch();
  const dirty = Object.keys(changes).length > 0;

  const m = useMutation({
    mutationFn: () => api.domains.patch(entry.action, entry.id, changes),
    onSuccess: (r) => {
      notify.success(writeSummary(`Saved ${r.items[0]?.value ?? entry.value}.`, r, entry.action));
      invalidate();
      onClose();
    },
    onError: (e) => notify.error(e, `Could not save ${entry.value}`),
  });

  return (
    <FormDialog
      isOpen
      onClose={onClose}
      title={`Edit ${entry.value}`}
      subtitle={`${ACTION_LABEL[entry.action]} entry`}
      width={540}
      noValidate
      onSubmit={() => {
        setTried(true);
        if (dirty && !err && scopeOK) m.mutate();
      }}
      footer={
        <>
          <Button label="Cancel" variant="ghost" onClick={onClose} />
          <Button type="submit" label="Save" variant="primary" isDisabled={!dirty || !!err} isLoading={m.isPending} />
        </>
      }
    >
      <VStack gap={4}>
        <SelectField label="Type" value={type} options={TYPE_OPTIONS} onChange={(v) => setType(v as DomainEntryType)} />
        <TextField label={type === 'regex' ? 'Expression' : 'Domain'} mono value={value} onChange={setValue} placeholder={TYPE_PLACEHOLDER[type].split('\n')[0]} error={err ?? undefined} />
        <ScopeFields scope={scope} groups={groups} onScope={setScope} onGroups={setGroups} showError={tried} />
        <TextField label="Comment" isOptional value={comment} onChange={(v) => setComment(v.slice(0, 500))} />
        <SwitchField label="Enabled" help="A disabled entry stays on its list but does not apply." value={enabled} onChange={setEnabled} />
        {entry.origin === 'unblock' && entry.request ? <Text type="supporting">Added by approving the unblock request {entry.request}.</Text> : null}
      </VStack>
    </FormDialog>
  );
}

export function DeleteEntriesDialog({ entries, busy, onConfirm, onClose }: { entries: DomainEntry[]; busy: boolean; onConfirm: () => void; onClose: () => void }) {
  const one = entries.length === 1 ? entries[0] : undefined;
  return (
    <FormDialog
      isOpen
      onClose={onClose}
      title={one ? `Delete ${one.value}?` : `Delete ${entries.length} entries?`}
      width={460}
      footer={
        <>
          <Button label="Cancel" variant="ghost" onClick={onClose} />
          <Button label="Delete" variant="destructive" isLoading={busy} onClick={onConfirm} />
        </>
      }
    >
      <VStack gap={2}>
        {!one ? (
          <List density="compact">
            {entries.slice(0, 12).map((e) => (
              <ListItem key={e.id} label={e.value} description={e.action === 'allow' ? 'allow' : 'deny'} />
            ))}
          </List>
        ) : null}
        {entries.length > 12 ? <Text type="supporting">and {entries.length - 12} more</Text> : null}
        <Text type="supporting">They leave every list they are on, for everyone and every group. To keep one for later, switch it off instead.</Text>
      </VStack>
    </FormDialog>
  );
}
