// One row per allow or deny entry, like Pi-hole's Domains table and
// Technitium's Allowed/Blocked zones: an enable switch that saves at once,
// the domain or expression, its type, the scope (everyone or group chips
// linking to the groups), an inline-editable comment, when, by whom and
// from where it was added (manual, query log, an unblock request, migrated),
// the list it lives in and what it overrides (under the domain), and Edit /
// Delete as icon buttons, so every column fits at 1280 px with the sidebar.
// Shared by /allowlist, /denylist and /domains (DomainEntriesView).
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { HStack } from '@astryxdesign/core/HStack';
import { Link } from '@astryxdesign/core/Link';
import { StackItem } from '@astryxdesign/core/Stack';
import { Switch } from '@astryxdesign/core/Switch';
import { proportional, pixel, Table, useTableSelection, useTableSelectionState, type TableColumn, type TablePlugin } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { Token } from '@astryxdesign/core/Token';
import { VStack } from '@astryxdesign/core/VStack';
import { MessageSquarePlus, Pencil, Trash2 } from 'lucide-react';
import { useState, type Dispatch, type SetStateAction } from 'react';
import type { DomainEntry, DomainEntryType } from '../../api/types';
import { formatDateTime, formatRelative } from '../../lib/format';
import { actorName, ORIGIN_LABEL, refPath, shortRef, TYPE_LABEL } from '../../lib/domains';
import { TextField } from '../fields/TextField';
import { RegexImpactDialog } from './RegexImpactDialog';

const TYPE_COLOR: Record<DomainEntryType, 'default' | 'blue' | 'purple'> = { exact: 'default', wildcard: 'blue', regex: 'purple' };

/** Why the row's controls are disabled ("" when they are not). */
export function lockReason(e: DomainEntry, canWrite: boolean): string {
  if (e.readOnly) return e.readOnlyReason || 'Managed outside the dashboard';
  if (!canWrite) return 'Needs blocklists.write';
  return '';
}

/** Data attributes on each body row (tests and e2e find rows by them). */
const rowAttrs: TablePlugin<DomainEntry> = {
  transformBodyRow: (props, item) => ({
    ...props,
    htmlProps: { ...props.htmlProps, ...({ 'data-testid': `entry-${item.value}`, 'data-enabled': String(item.enabled), 'data-action': item.action } as Record<string, string>) },
  }),
};

export interface DomainEntriesTableProps {
  entries: DomainEntry[];
  /** The combined view shows the action of each row. */
  showAction?: boolean;
  canWrite: boolean;
  selected: Set<string>;
  setSelected: Dispatch<SetStateAction<Set<string>>>;
  /** Ids whose switch is saving. */
  pending: ReadonlySet<string>;
  onToggle: (e: DomainEntry, enabled: boolean) => void;
  onComment: (e: DomainEntry, comment: string) => Promise<unknown>;
  onEdit: (e: DomainEntry) => void;
  onDelete: (e: DomainEntry) => void;
  label: string;
  emptyText: string;
}

export function DomainEntriesTable({ entries, showAction, canWrite, selected, setSelected, pending, onToggle, onComment, onEdit, onDelete, label, emptyText }: DomainEntriesTableProps) {
  const { selectionConfig } = useTableSelectionState({
    data: entries,
    idKey: 'id',
    selectedKeys: selected,
    setSelectedKeys: setSelected,
    getIsItemEnabled: (e) => !lockReason(e, canWrite),
  });
  const selection = useTableSelection({ ...selectionConfig, getRowLabel: (e: DomainEntry) => e.value });

  const columns: TableColumn<DomainEntry>[] = [
    {
      key: 'value',
      header: 'Domain',
      width: proportional(2.5, { minWidth: 190 }),
      renderCell: (e) => <DomainCell e={e} />,
    },
    ...(showAction
      ? [
          {
            key: 'action',
            header: 'Action',
            width: pixel(88),
            renderCell: (e: DomainEntry) => <Badge variant={e.action === 'allow' ? 'success' : 'error'} label={e.action} />,
          },
        ]
      : []),
    {
      key: 'type',
      header: 'Type',
      width: pixel(112),
      renderCell: (e) =>
        e.kind === 'exception' ? (
          <VStack gap={0.5} align="start">
            <Token size="sm" color="orange" label="exception" description="list exception: an entry switched off in one list" />
            <Token size="sm" color={TYPE_COLOR[e.type]} label={TYPE_LABEL[e.type]} />
          </VStack>
        ) : (
          <Token size="sm" color={TYPE_COLOR[e.type]} label={TYPE_LABEL[e.type]} />
        ),
    },
    {
      key: 'scope',
      header: 'Applies to',
      width: proportional(1.5, { minWidth: 120 }),
      renderCell: (e) => <ScopeCell e={e} />,
    },
    {
      key: 'comment',
      header: 'Comment',
      width: proportional(1.75, { minWidth: 130 }),
      renderCell: (e) => <CommentCell e={e} locked={e.kind === 'exception' || !!lockReason(e, canWrite)} onSave={(c) => onComment(e, c)} />,
    },
    {
      key: 'created',
      header: 'Added',
      width: proportional(1.5, { minWidth: 120 }),
      renderCell: (e) => <AddedCell e={e} />,
    },
    {
      key: 'enabled',
      header: 'On',
      width: pixel(64),
      renderCell: (e) => {
        const why = lockReason(e, canWrite);
        return (
          <Switch
            label={e.kind === 'exception' ? `${e.value} switched off in ${shortRef(e.lists[0]?.list ?? '', e.namespace)}` : `${e.value} enabled`}
            isLabelHidden
            size="sm"
            value={e.enabled}
            isLoading={pending.has(e.id)}
            isDisabled={!!why || pending.has(e.id)}
            disabledMessage={why || undefined}
            onChange={(on) => onToggle(e, on)}
          />
        );
      },
    },
    {
      key: 'actions',
      header: 'Actions',
      width: pixel(96),
      renderCell: (e) => {
        const why = lockReason(e, canWrite);
        return (
          <HStack gap={0.5}>
            {e.kind === 'exception' ? null : (
              <Button label="Edit" size="sm" variant="ghost" isIconOnly icon={<Pencil size={16} aria-hidden="true" />} isDisabled={!!why} tooltip={why || `Edit ${e.value}`} onClick={() => onEdit(e)} />
            )}
            <Button
              label="Delete"
              size="sm"
              variant="ghost"
              isIconOnly
              icon={<Trash2 size={16} aria-hidden="true" />}
              isDisabled={!!why}
              tooltip={why || (e.kind === 'exception' ? `Switch ${e.value} back on in ${shortRef(e.lists[0]?.list ?? '', e.namespace)}` : `Delete ${e.value}`)}
              onClick={() => onDelete(e)}
            />
          </HStack>
        );
      },
    },
  ];

  return (
    <Table<DomainEntry>
      aria-label={label}
      density="compact"
      verticalAlign="top"
      idKey="id"
      data={entries}
      columns={columns}
      plugins={{ selection, rowAttrs }}
      emptyState={<Text type="supporting">{emptyText}</Text>}
    />
  );
}

/** The value, what the entry overrides and why it is read-only. */
function DomainCell({ e }: { e: DomainEntry }) {
  const [inspect, setInspect] = useState(false);
  return (
    <VStack gap={0.5}>
      <Text type="code" wordBreak="break-all" hasStrikethrough={!e.enabled}>
        {e.value}
      </Text>
      {e.type === 'regex' && e.kind !== 'exception' ? (
        <>
          <Button label="Test a domain" size="sm" variant="ghost" onClick={() => setInspect(true)} />
          {inspect ? <RegexImpactDialog entry={e} onClose={() => setInspect(false)} /> : null}
        </>
      ) : null}
      {e.kind === 'exception' ? (
        e.stillMatchedBy.length ? (
          <Text type="supporting" size="sm">
            {e.action === 'allow' ? 'Still blocked by ' : 'Still allowed by '}
            {e.stillMatchedBy.map((o, i) => (
              <Text key={o.list} type="inherit">
                {i ? ', ' : ''}
                <Link href={refPath('lists', o.list)} color="inherit" hasUnderline>
                  {shortRef(o.list, e.namespace)}
                </Link>
              </Text>
            ))}
          </Text>
        ) : null
      ) : (
        <Text type="supporting" size="sm">
          In{' '}
          {e.lists.map((l, i) => (
            <Text key={`${l.list}/${l.source}`} type="inherit">
              {i ? ', ' : ''}
              <Link href={refPath('lists', l.list)} color="inherit" hasUnderline>
                {shortRef(l.list, e.namespace)}
              </Link>
            </Text>
          ))}
        </Text>
      )}
      {e.kind !== 'exception' && e.overrides.length ? (
        <Text type="supporting" size="sm">
          {e.action === 'allow' ? 'Overrides block list ' : 'Overridden by allow list '}
          {e.overrides.map((o, i) => (
            <Text key={o.list} type="inherit">
              {i ? ', ' : ''}
              <Link href={refPath('lists', o.list)} color="inherit" hasUnderline>
                {shortRef(o.list, e.namespace)}
              </Link>
            </Text>
          ))}
        </Text>
      ) : null}
      {e.readOnly ? (
        <Text type="supporting" size="sm">
          Read-only: {e.readOnlyReason || 'managed outside the dashboard'}
        </Text>
      ) : null}
    </VStack>
  );
}

function ScopeCell({ e }: { e: DomainEntry }) {
  const own = e.lists[0];
  if (e.kind === 'exception' && own) {
    return (
      <VStack gap={0.5} align="start">
        <Text size="sm">Only list</Text>
        <Link href={refPath('lists', own.list)} size="sm">
          {shortRef(own.list, e.namespace)}
        </Link>
        <Text type="supporting" size="sm">
          {e.policies.length ? `used by ${e.policies.map((p) => shortRef(p, e.namespace)).join(', ')}` : 'used by no policy'}
        </Text>
      </VStack>
    );
  }
  return (
    <HStack gap={1} wrap="wrap">
      {e.everyone ? <Token size="sm" color="green" label="Everyone" /> : null}
      {e.groups.map((g) => (
        <Token key={g} size="sm" color="cyan" label={shortRef(g, e.namespace)} href={refPath('groups', g)} description={`Client group ${g}`} />
      ))}
    </HStack>
  );
}

function AddedCell({ e }: { e: DomainEntry }) {
  const own = e.lists[0];
  if (e.kind === 'exception' && own) {
    return (
      <VStack gap={0.5}>
        <Text size="sm">
          switched off in{' '}
          <Link href={refPath('lists', own.list)} size="sm">
            {shortRef(own.list, e.namespace)}
          </Link>
        </Text>
        <Text type="supporting" size="sm">
          source {own.source}
        </Text>
      </VStack>
    );
  }
  const who = actorName(e.createdBy);
  const origin =
    e.origin === 'unblock' && e.request ? (
      <Link href={refPath('requests', e.request)} size="sm">
        unblock request
      </Link>
    ) : (
      ORIGIN_LABEL[e.origin]
    );
  const modified = e.modified && e.modified !== e.created ? e.modified : undefined;
  return (
    <VStack gap={0.5}>
      {e.created ? (
        <Text size="sm">
          <time dateTime={e.created} title={formatDateTime(e.created)}>
            {formatRelative(e.created)}
          </time>
        </Text>
      ) : null}
      <Text type="supporting" size="sm">
        {who ? `by ${who} · ` : ''}from {origin}
      </Text>
      {modified ? (
        <Text type="supporting" size="sm">
          changed{' '}
          <time dateTime={modified} title={formatDateTime(modified)}>
            {formatRelative(modified)}
          </time>
        </Text>
      ) : null}
    </VStack>
  );
}

/** The comment, edited in place: Enter or Save stores it, Escape or Cancel drops the edit. */
function CommentCell({ e, locked, onSave }: { e: DomainEntry; locked: boolean; onSave: (c: string) => Promise<unknown> }) {
  const [draft, setDraft] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const save = async () => {
    if (draft === null) return;
    if (draft.trim() === (e.comment ?? '')) {
      setDraft(null);
      return;
    }
    setBusy(true);
    try {
      await onSave(draft.trim());
      setDraft(null);
    } catch {
      // The toast says why; the draft stays for another try.
    } finally {
      setBusy(false);
    }
  };
  if (draft !== null) {
    return (
      <VStack gap={1}>
        <TextField
          label={`Comment for ${e.value}`}
          isLabelHidden
          size="sm"
          value={draft}
          hasAutoFocus
          isDisabled={busy}
          onChange={(v) => setDraft(v.slice(0, 500))}
          onKeyDown={(k) => {
            if (k.key === 'Enter') {
              k.preventDefault();
              void save();
            } else if (k.key === 'Escape') {
              k.preventDefault();
              setDraft(null);
            }
          }}
        />
        <HStack gap={1}>
          <Button label="Save" size="sm" variant="secondary" isLoading={busy} onClick={() => void save()} />
          <Button label="Cancel" size="sm" variant="ghost" isDisabled={busy} onClick={() => setDraft(null)} />
        </HStack>
      </VStack>
    );
  }
  if (locked) return <Text size="sm" type={e.comment ? 'body' : 'supporting'}>{e.comment || '–'}</Text>;
  return (
    <HStack gap={1} align="start">
      <StackItem size="fill">
        <Text size="sm" type={e.comment ? 'body' : 'supporting'} wordBreak="break-word">
          {e.comment || '–'}
        </Text>
      </StackItem>
      <Button
        label={e.comment ? 'Edit comment' : 'Add comment'}
        size="sm"
        variant="ghost"
        isIconOnly
        icon={e.comment ? <Pencil size={14} aria-hidden="true" /> : <MessageSquarePlus size={14} aria-hidden="true" />}
        tooltip={e.comment ? 'Edit comment' : 'Add comment'}
        onClick={() => setDraft(e.comment ?? '')}
      />
    </HStack>
  );
}
