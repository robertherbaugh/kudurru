// The sources of one BlockList, one row each like Pi-hole's Adlists: an
// enable switch (spec.sources[].enabled, ux-plan C2), the type, the URL or
// the number of inline entries, format, subdomains, the per-source status
// (entries, last fetch, error) and edit / delete actions.
import { Badge } from '@astryxdesign/core/Badge';
import { Button } from '@astryxdesign/core/Button';
import { Switch } from '@astryxdesign/core/Switch';
import { Database, FileText, Link as LinkIcon } from 'lucide-react';
import type { BlockList } from '../../api/types';
import { formatExact, formatRelative } from '../../lib/format';
import { formatLabel, sourceEnabled, sourceKind, sourceName, sourceStatus, type SourceKind } from '../../lib/lists';

const KIND: Record<SourceKind, { label: string; Icon: typeof LinkIcon }> = {
  url: { label: 'URL', Icon: LinkIcon },
  inline: { label: 'Inline entries', Icon: FileText },
  configMap: { label: 'ConfigMap', Icon: Database },
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
  return (
    <div className="k-table-wrap">
      <table className="k-table" aria-label="Sources">
        <thead>
          <tr>
            <th scope="col">Enabled</th>
            <th scope="col">Type</th>
            <th scope="col">Name</th>
            <th scope="col">Source</th>
            <th scope="col">Format</th>
            <th scope="col">Subdomains</th>
            <th scope="col" className="num">
              Entries
            </th>
            <th scope="col">Last fetched</th>
            <th scope="col">Error</th>
            <th scope="col">
              <span className="k-visually-hidden">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {sources.map((src, i) => {
            const name = sourceName(i, src);
            const kind = sourceKind(src);
            const { label, Icon } = KIND[kind];
            const st = sourceStatus(list, i);
            const on = sourceEnabled(src);
            return (
              <tr key={`${i}-${name}`} data-testid={`source-${name}`} data-enabled={on} className={on ? undefined : 'k-row-off'}>
                <td>
                  <Switch
                    label={`Source ${name} enabled`}
                    isLabelHidden
                    size="sm"
                    value={on}
                    isLoading={pending === i}
                    isDisabled={!editable || busy}
                    onChange={(v) => onToggle(i, v)}
                  />
                </td>
                <td>
                  <span className="k-source-kind" title={label}>
                    <Icon size={16} aria-hidden="true" />
                    <span className="k-visually-hidden">{label}</span>
                  </span>
                </td>
                <td className="k-mono k-nowrap">{name}</td>
                <td className="k-break">
                  {kind === 'url' ? (
                    <a href={src.url} target="_blank" rel="noopener noreferrer" className="k-mono k-small">
                      {src.url}
                    </a>
                  ) : kind === 'inline' ? (
                    <button type="button" className="k-linklike" onClick={() => onBrowse(i)} aria-label={`Browse the ${formatExact(src.inline?.length ?? 0)} inline entries of ${name}`}>
                      {formatExact(src.inline?.length ?? 0)} inline entries
                    </button>
                  ) : (
                    <span className="k-mono k-small">
                      ConfigMap {src.configMapRef?.namespace ? `${src.configMapRef.namespace}/` : ''}
                      {src.configMapRef?.name}
                    </span>
                  )}
                </td>
                <td className="k-small">{formatLabel(src.format)}</td>
                <td>{src.subdomains ? <Badge variant="info" label="yes" /> : <span className="k-muted k-small">no</span>}</td>
                <td className="num" title={on ? undefined : 'Not counted while the source is disabled'}>
                  {on ? formatExact(st?.entries) : <span className="k-muted">{st?.entries ? `${formatExact(st.entries)} (off)` : 'off'}</span>}
                  {on && st?.invalid ? <div className="k-muted k-small">{formatExact(st.invalid)} invalid</div> : null}
                </td>
                <td className="k-small k-nowrap">
                  {!on ? (
                    <span className="k-muted">disabled</span>
                  ) : kind === 'inline' ? (
                    <span className="k-muted" title="Inline entries are compiled from the object; there is nothing to fetch">
                      compiled
                    </span>
                  ) : st?.lastFetched ? (
                    <span title={st.lastFetched}>{formatRelative(st.lastFetched)}</span>
                  ) : (
                    <span className="k-muted">not yet</span>
                  )}
                </td>
                <td className="k-small k-break">{on && st?.error ? <span className="k-error-text">{st.error}</span> : <span className="k-muted">–</span>}</td>
                <td className="k-nowrap k-row-actions">
                  {kind === 'inline' ? <Button label="Entries" size="sm" variant="ghost" onClick={() => onBrowse(i)} /> : null}
                  {editable ? (
                    <>
                      <Button label="Edit" size="sm" variant="ghost" onClick={() => onEdit(i)} />
                      <Button
                        label="Delete"
                        size="sm"
                        variant="ghost"
                        isDisabled={sources.length === 1}
                        tooltip={sources.length === 1 ? 'A list needs at least one source; delete the list instead' : undefined}
                        onClick={() => onDelete(i)}
                      />
                    </>
                  ) : null}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
