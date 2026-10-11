// Settings (§10 principle 10): Basic shows the handful of keys most people
// change plus appearance; Expert lists every key of GET /settings/_schema
// grouped by section with search. Cluster keys are patched with
// PATCH /settings, node keys with PATCH /node/config (JSON merge patch);
// host-only and environment-locked keys are read-only with the reason.
import { SegmentedControl, SegmentedControlItem } from '@astryxdesign/core/SegmentedControl';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useMemo } from 'react';
import { ApiError } from '../api/client';
import { qk, useApi, useCan } from '../api/context';
import type { SettingKey } from '../api/types';
import { AppearanceControls } from '../components/AppearanceButton';
import { ErrorBanner, Loading, useNotify } from '../components/Feedback';
import { SettingRow } from '../components/SettingRow';
import { useSearchParams } from '../lib/router';
import { BASIC_KEYS, getPath, mergePatchFor, sectionOf } from '../lib/settings';

export interface SettingsListProps {
  keys: SettingKey[];
  cluster: Record<string, unknown> | undefined;
  node: Record<string, unknown> | undefined;
  canWriteCluster: boolean;
  canWriteNode: boolean;
  onSave: (k: SettingKey, value: unknown) => Promise<void>;
  grouped?: boolean;
}

/** The generated settings list (exported for tests). */
export function SettingsList({ keys, cluster, node, canWriteCluster, canWriteNode, onSave, grouped = true }: SettingsListProps) {
  const sections = useMemo(() => {
    const m = new Map<string, SettingKey[]>();
    for (const k of keys) {
      const s = grouped ? sectionOf(k.path) : '';
      m.set(s, [...(m.get(s) ?? []), k]);
    }
    return [...m];
  }, [keys, grouped]);
  return (
    <div className="k-stack-lg">
      {sections.map(([section, list]) => (
        <section key={section || 'all'} className="k-panel" aria-labelledby={section ? `sec-${section}` : undefined}>
          {section ? (
            <h2 id={`sec-${section}`} className="k-h3">
              {section}
            </h2>
          ) : null}
          {list.map((k) => (
            <SettingRow
              key={k.path}
              k={k}
              value={getPath(k.scope === 'node' ? node : cluster, k.path)}
              canWrite={k.scope === 'node' ? canWriteNode : canWriteCluster}
              onSave={onSave}
            />
          ))}
        </section>
      ))}
    </div>
  );
}

export default function SettingsPage() {
  const api = useApi();
  const can = useCan();
  const qc = useQueryClient();
  const notify = useNotify();
  const [sp, setParams] = useSearchParams();
  const view = sp.get('view') === 'expert' ? 'expert' : 'basic';
  const search = sp.get('q') ?? '';

  const schema = useQuery({ queryKey: qk.schema, queryFn: () => api.settings.schema(), staleTime: Infinity });
  const cluster = useQuery({ queryKey: qk.settings, queryFn: () => api.settings.get() });
  const node = useQuery({ queryKey: qk.nodeConfig, queryFn: () => api.settings.nodeConfig(), enabled: can('node', 'config') });

  const keys = useMemo(() => {
    const all = schema.data?.items ?? [];
    if (view === 'basic') return all.filter((k) => BASIC_KEYS.has(k.path));
    const s = search.trim().toLowerCase();
    return s ? all.filter((k) => k.path.toLowerCase().includes(s) || k.help.toLowerCase().includes(s)) : all;
  }, [schema.data, view, search]);

  const onSave = async (k: SettingKey, value: unknown) => {
    const patch = mergePatchFor(k.path, value);
    try {
      if (k.scope === 'node') {
        qc.setQueryData(qk.nodeConfig, await api.settings.patchNodeConfig(patch));
      } else {
        qc.setQueryData(qk.settings, await api.settings.patch(patch));
      }
      notify.success(`${k.path} saved${k.restartRequired ? '; takes effect after a restart' : ''}`);
    } catch (err) {
      const msg = err instanceof ApiError ? err.describe() : (err as Error).message;
      throw new Error(msg, { cause: err });
    }
  };

  return (
    <div className="k-page">
      <div className="k-page-header">
        <div className="k-stack">
          <h1>Settings</h1>
          <p className="k-muted k-small">
            Cluster settings apply to every node; node settings only to this one. Host-only keys (listeners, data directory, secrets on disk) are
            changed in kudurru.yaml or the environment on the host.
          </p>
        </div>
        <div className="k-actions">
          <SegmentedControl label="Detail" value={view} onChange={(v) => setParams({ view: v === 'expert' ? 'expert' : null })}>
            <SegmentedControlItem value="basic" label="Basic" />
            <SegmentedControlItem value="expert" label="Expert" />
          </SegmentedControl>
        </div>
      </div>
      {view === 'basic' ? (
        <section className="k-panel" aria-labelledby="sec-appearance">
          <h2 id="sec-appearance" className="k-h3">
            Appearance
          </h2>
          <p className="k-muted k-small">Stored in this browser only.</p>
          <AppearanceControls />
        </section>
      ) : (
        <TextInput label="Search settings" isLabelHidden placeholder="Search all settings" value={search} onChange={(v) => setParams({ q: v || null })} hasClear />
      )}
      {schema.error ? <ErrorBanner error={schema.error} /> : null}
      {cluster.error ? <ErrorBanner error={cluster.error} /> : null}
      {schema.isLoading || cluster.isLoading ? <Loading /> : null}
      {schema.data && cluster.data ? (
        <SettingsList
          keys={keys}
          cluster={cluster.data}
          node={node.data}
          canWriteCluster={can('settings', 'write')}
          canWriteNode={can('node', 'config')}
          onSave={onSave}
          grouped
        />
      ) : null}
    </div>
  );
}
