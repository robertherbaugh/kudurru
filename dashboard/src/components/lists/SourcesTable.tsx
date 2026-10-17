// The sources of one BlockList, one row each like Pi-hole's Adlists: an
// enable switch (spec.sources[].enabled, ux-plan C2), the type, the URL or
// the number of inline entries, format, subdomains, the per-source status
// ("N of M entries enabled" once single entries are switched off, last
// fetch, error) and the entries browser, edit and delete actions. Every
// kind of source opens the entries browser. A disabled source's cells are
// drawn in the secondary colour.
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { FieldStatus } from '@astryxdesign/core/FieldStatus';
import { Icon } from '@astryxdesign/core/Icon';
import { Link } from '@astryxdesign/core/Link';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Switch } from '@astryxdesign/core/Switch';
import { pixel, proportional, Table, type TableColumn, type TablePlugin } from '@astryxdesign/core/Table';
import { Text } from '@astryxdesign/core/Text';
import { Tooltip } from '@astryxdesign/core/Tooltip';
import { VisuallyHidden } from '@astryxdesign/core/VisuallyHidden';
import { Database, FileText, Link as LinkIcon, ListChecks, Pencil, Trash2 } from 'lucide-react';
import type { BlockList, ListSource, SourceStatus } from '../../api/types';
import { formatExact, formatRelative } from '../../lib/format';
import { enabledEntriesText, formatLabel, sourceEnabled, sourceKind, sourceName, sourceStatus, type SourceKind } from '../../lib/lists';

const KIND: Record<SourceKind, { label: string; Icon: typeof LinkIcon }> = {
  url: { label: 'URL', Icon: LinkIcon },
  inline: { label: 'Inline entries', Icon: FileText },
  configMap: { label: 'ConfigMap', Icon: Database },
  entries: { label: 'Allow or deny entries', Icon: ListChecks },
};

/** One source as a table row (a type literal, as the Table's Record constraint needs). */
type Row = { key: string; index: number; name: string; src: ListSource; kind: SourceKind; st: SourceStatus | undefined; on: boolean };

/** Data attributes on each body row (tests and e2e find sources by them). */
const rowAttrs: TablePlugin<Row> = {
  transformBodyRow: (props, item) => ({
    ...props,
    htmlProps: { ...props.htmlProps, ...({ 'data-testid': `source-${item.name}`, 'data-enabled': String(item.on) } as Record<string, string>) },
  }),
};

export interface SourcesTableProps {
  list: BlockList;
  /** Writes allowed (API-owned list and blocklists.write). */
  editable: boolean;
  /** Index of the source whose switch is saving. */
  pending?: number | null;
  onToggle: (index: number, enabled: boolean) => void;
  onEdit: (index: number) => void;
  onDelete: (index: number) => void;
  onBrowse: (index: number) => void;
}

export function SourcesTable({ list, editable, pending, onToggle, onEdit, onDelete, onBrowse }: SourcesTableProps) {
  const sources = list.spec.sources;
  const busy = pending !== null && pending !== undefined;
  const rows: Row[] = sources.map((src, i) => {
    const name = sourceName(i, src);
    return { key: `${i}-${name}`, index: i, name, src, kind: sourceKind(src), st: sourceStatus(list, i), on: sourceEnabled(src) };
  });
  /** A cell's text: secondary while the source is off. */
  const tone = (r: Row) => (r.on ? 'primary' : 'secondary');

  const columns: TableColumn<Row>[] = [
    {
      // The type as an icon before the name (a column of its own does not fit at 1280 px).
      key: 'name',
      header: 'Name',
      width: proportional(1.5, { minWidth: 104 }),
      renderCell: (r) => {
        const { label, Icon: KindIcon } = KIND[r.kind];
        return (
          <HStack gap={1.5} vAlign="center" wrap="nowrap">
            <Tooltip content={label} hasHoverIndication={false}>
              <Text color="secondary">
                <Icon icon={KindIcon} size="sm" />
                <VisuallyHidden>{label}</VisuallyHidden>
              </Text>
            </Tooltip>
            <Text type="code" size="sm" color={tone(r)} textWrap="nowrap">
              {r.name}
            </Text>
          </HStack>
        );
      },
    },
    {
      key: 'source',
      header: 'Source',
      width: proportional(2.5, { minWidth: 150 }),
      renderCell: (r) =>
        r.kind === 'url' ? (
          <Link href={r.src.url} target="_blank" hasUnderline color={r.on ? 'primary' : 'secondary'} size="sm">
            <Text type="code" size="sm" color="inherit" wordBreak="break-all" maxLines={3}>
              {r.src.url}
            </Text>
          </Link>
        ) : r.kind === 'inline' ? (
          <Link label={`Browse the ${formatExact(r.src.inline?.length ?? 0)} inline entries of ${r.name}`} onClick={() => onBrowse(r.index)}>
            {formatExact(r.src.inline?.length ?? 0)} inline entries
          </Link>
        ) : r.kind === 'entries' ? (
          <Link label={`Browse the ${formatExact(r.src.entries?.length ?? 0)} entries of ${r.name}`} onClick={() => onBrowse(r.index)}>
            {formatExact(r.src.entries?.length ?? 0)} {list.spec.mode === 'allow' ? 'allow' : 'deny'} entries
          </Link>
        ) : (
          <Text type="code" size="sm" color={tone(r)} wordBreak="break-all">
            ConfigMap {r.src.configMapRef?.namespace ? `${r.src.configMapRef.namespace}/` : ''}
            {r.src.configMapRef?.name}
          </Text>
        ),
    },
    {
      key: 'format',
      header: 'Format',
      width: proportional(1, { minWidth: 72 }),
      renderCell: (r) => (
        <Text size="sm" color={tone(r)}>
          {formatLabel(r.src.format)}
        </Text>
      ),
    },
    {
      key: 'subdomains',
      header: 'Subdomains',
      width: pixel(108),
      renderCell: (r) => (r.src.subdomains ? <Badge variant="info" label="yes" /> : <Text type="supporting">no</Text>),
    },
    {
      key: 'entries',
      header: 'Entries',
      width: pixel(92),
      align: 'end',
      renderCell: (r) =>
        r.on ? (
          <VStack gap={0}>
            <Text>{enabledEntriesText(r.st)}</Text>
            {r.st?.invalid ? <Text type="supporting">{formatExact(r.st.invalid)} invalid</Text> : null}
          </VStack>
        ) : (
          <Tooltip content="Not counted while the source is disabled" hasHoverIndication={false}>
            <Text color="secondary">{r.st?.entries ? `${formatExact(r.st.entries)} (off)` : 'off'}</Text>
          </Tooltip>
        ),
    },
    {
      key: 'fetched',
      header: 'Last fetched',
      width: pixel(108),
      renderCell: (r) =>
        !r.on ? (
          <Text type="supporting">disabled</Text>
        ) : r.kind === 'inline' || r.kind === 'entries' ? (
          <Tooltip content="Inline entries are compiled from the object; there is nothing to fetch" hasHoverIndication={false}>
            <Text type="supporting">compiled</Text>
          </Tooltip>
        ) : r.st?.lastFetched ? (
          <Text size="sm" textWrap="nowrap">
            <time dateTime={r.st.lastFetched} title={r.st.lastFetched}>
              {formatRelative(r.st.lastFetched)}
            </time>
          </Text>
        ) : (
          <Text type="supporting">not yet</Text>
        ),
    },
    {
      key: 'error',
      header: 'Error',
      width: proportional(1, { minWidth: 56 }),
      renderCell: (r) =>
        r.on && r.st?.error ? (
          <FieldStatus type="error" variant="detached" message={r.st.error} />
        ) : (
          <Text type="supporting">–</Text>
        ),
    },
    {
      key: 'enabled',
      header: 'Enabled',
      width: pixel(76),
      renderCell: (r) => (
        <Switch label={`Source ${r.name} enabled`} isLabelHidden size="sm" value={r.on} isLoading={pending === r.index} isDisabled={!editable || busy} onChange={(v) => onToggle(r.index, v)} />
      ),
    },
    {
      key: 'actions',
      header: <VisuallyHidden>Actions</VisuallyHidden>,
      width: pixel(editable ? 156 : 92),
      renderCell: (r) => (
        <HStack gap={0.5} wrap="nowrap">
          <Button label="Entries" size="sm" variant="ghost" tooltip={`Browse and switch off single entries of ${r.name}`} onClick={() => onBrowse(r.index)} />
          {editable ? (
            <>
              {r.kind !== 'entries' ? <Button label="Edit" size="sm" variant="ghost" isIconOnly icon={<Pencil size={16} aria-hidden="true" />} tooltip={`Edit ${r.name}`} onClick={() => onEdit(r.index)} /> : null}
              <Button
                label="Delete"
                size="sm"
                variant="ghost"
                isIconOnly
                icon={<Trash2 size={16} aria-hidden="true" />}
                isDisabled={sources.length === 1}
                tooltip={sources.length === 1 ? 'A list needs at least one source; delete the list instead' : `Delete ${r.name}`}
                onClick={() => onDelete(r.index)}
              />
            </>
          ) : null}
        </HStack>
      ),
    },
  ];

  return <Table<Row> aria-label="Sources" density="compact" hasHover idKey="key" data={rows} columns={columns} plugins={{ rowAttrs }} />;
}
