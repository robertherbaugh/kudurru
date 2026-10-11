// Settings (§10 principle 10): Basic shows the handful of keys most people
// change plus appearance; Expert lists every key of GET /settings/_schema
// in sections by the C1 `group` (lib/settingsMeta.ts), with search over
// labels, paths and help. Every key gets a typed control from its shape
// (SettingRow). Cluster keys are patched with
// PATCH /settings, node keys with PATCH /node/config (JSON merge patch);
// host-only and environment-locked keys are read-only with the reason.
// Both answer the envelope {scope, node, settings, changed, restartRequired,
// persisted}; the values are under `settings`.
import { Banner } from '@astryxdesign/core/Banner';
import { SegmentedControl, SegmentedControlItem } from '@astryxdesign/core/SegmentedControl';
import { TextInput } from '@astryxdesign/core/TextInput';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useMemo, useState } from 'react';
import { ApiError } from '../api/client';
import { qk, useApi, useCan } from '../api/context';
import type { SettingKey, SettingsBody } from '../api/types';
import { AppearanceControls } from '../components/AppearanceButton';
import { ErrorBanner, Loading, useNotify } from '../components/Feedback';
import { BlockPageIntro } from '../components/BlockPagePreview';
import { SettingRow } from '../components/SettingRow';
import { Link, useSearchParams } from '../lib/router';
import { BASIC_KEYS, getPath, mergePatchFor } from '../lib/settings';
import { compareGroups, groupOf, labelOf, metaFor } from '../lib/settingsMeta';
import { groupNames, mapPatch, strategyLabelOf, upstreamsOf } from '../lib/upstreams';

export interface SettingsListProps {
  keys: SettingKey[];
  /** The `settings` documents of GET /settings and GET /node/config. */
  cluster: Record<string, unknown> | undefined;
  node: Record<string, unknown> | undefined;
  canWriteCluster: boolean;
  canWriteNode: boolean;
  onSave: (k: SettingKey, value: unknown) => Promise<void>;
  grouped?: boolean;
}

/** The value of a key: from its scope's settings document, else the schema's effective value. */
export function valueOf(k: SettingKey, cluster: Record<string, unknown> | undefined, node: Record<string, unknown> | undefined): unknown {
  const v = getPath(k.scope === 'node' ? node : cluster, k.path);
  return v === undefined ? k.value : v;
}

/** The warnings a PATCH answer calls for: not persisted, or a restart needed. */
export interface SaveNotice {
  persisted: boolean;
  restartRequired: string[];
}

export function noticeFor(body: SettingsBody): SaveNotice | null {
  const restart = body.restartRequired ?? [];
  if (body.persisted !== false && restart.length === 0) return null;
  return { persisted: body.persisted !== false, restartRequired: restart };
}

/** upstreams.groups is edited on the Upstreams pages; the row shows what is configured. */
function GroupsSummary({ value }: { value: unknown }) {
  const cfg = upstreamsOf({ settings: { upstreams: { groups: value } } } as unknown as SettingsBody);
  const names = groupNames(cfg);
  return (
    <div className="k-stack-sm" data-testid="groups-summary">
      <ul className="k-link-list k-stack-sm">
        {names.map((n) => (
          <li key={n}>
            <Link href={`/upstreams/${encodeURIComponent(n)}`} className="k-mono">
              {n}
            </Link>{' '}
            <span className="k-muted k-small">
              {cfg.groups[n]?.servers.length ?? 0} server{cfg.groups[n]?.servers.length === 1 ? '' : 's'}, {strategyLabelOf(cfg.groups[n]?.strategy ?? '')}
            </span>
          </li>
        ))}
      </ul>
      <Link href="/upstreams" className="k-small">
        Edit on the Upstreams page
      </Link>
    </div>
  );
}

/** The generated settings list (exported for tests). */
export function SettingsList({ keys, cluster, node, canWriteCluster, canWriteNode, onSave, grouped = true }: SettingsListProps) {
  const sections = useMemo(() => {
    const m = new Map<string, SettingKey[]>();
    for (const k of keys) {
      const s = grouped ? groupOf(k, metaFor(k)) : '';
      m.set(s, [...(m.get(s) ?? []), k]);
    }
    return [...m].sort(([a], [b]) => compareGroups(a, b));
  }, [keys, grouped]);
  const slug = (s: string) => s.toLowerCase().replace(/[^a-z0-9]+/g, '-');
  return (
    <div className="k-stack-lg">
      {sections.map(([section, list]) => (
        <section key={section || 'all'} className="k-panel" aria-labelledby={section ? `sec-${slug(section)}` : undefined} data-section={section}>
          {section ? (
            <h2 id={`sec-${slug(section)}`} className="k-h3">
              {section}
            </h2>
          ) : null}
          {section === 'Block page' ? <BlockPageIntro /> : null}
          {list.map((k) => (
            <SettingRow
              key={k.path}
              k={k}
              meta={metaFor(k)}
              value={valueOf(k, cluster, node)}
              canWrite={k.scope === 'node' ? canWriteNode : canWriteCluster}
              onSave={onSave}
              summary={k.path === 'upstreams.groups' ? <GroupsSummary value={valueOf(k, cluster, node)} /> : undefined}
              extra={
                k.path.startsWith('upstreams.') && k.path !== 'upstreams.groups' ? (
                  <Link href="/upstreams" className="k-small">
                    Also on the Upstreams page
                  </Link>
                ) : undefined
              }
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
    return s
      ? all.filter((k) => {
          const m = metaFor(k);
          return [k.path, k.help, labelOf(k, m), groupOf(k, m)].some((t) => t.toLowerCase().includes(s));
        })
      : all;
  }, [schema.data, view, search]);

  const [notice, setNotice] = useState<SaveNotice | null>(null);

  const onSave = async (k: SettingKey, value: unknown) => {
    // A merge patch merges maps: keys removed in the editor must be sent as null.
    const before = valueOf(k, cluster.data?.settings, node.data?.settings);
    // null resets the key to its default (internal/api patchSettings).
    const v =
      k.type === 'map' && value && typeof value === 'object' && before && typeof before === 'object'
        ? mapPatch(before as Record<string, unknown>, value as Record<string, unknown>)
        : value;
    const patch = mergePatchFor(k.path, v);
    try {
      const body = k.scope === 'node' ? await api.settings.patchNodeConfig(patch) : await api.settings.patch(patch);
      qc.setQueryData(k.scope === 'node' ? qk.nodeConfig : qk.settings, body);
      void qc.invalidateQueries({ queryKey: qk.schema });
      setNotice(noticeFor(body));
      notify.success(`${k.path} ${value === null ? 'reset to its default' : 'saved'}${k.restartRequired || body.restartRequired?.length ? '; takes effect after a restart' : ''}`);
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
      {notice && !notice.persisted ? (
        <Banner
          status="warning"
          title="Kept in memory only"
          description="This server has no settings file, so the change is lost on restart. Set KUDURRU_SETTINGS_FILE (the packages and the container image do) to persist changes made here."
          isDismissable
          onDismiss={() => setNotice(null)}
        />
      ) : null}
      {notice?.restartRequired.length ? (
        <Banner
          status="info"
          title="Restart required"
          description={`${notice.restartRequired.join(', ')} take${notice.restartRequired.length === 1 ? 's' : ''} effect after kudurru restarts.`}
          isDismissable
          onDismiss={() => setNotice(null)}
        />
      ) : null}
      {schema.error ? <ErrorBanner error={schema.error} /> : null}
      {cluster.error ? <ErrorBanner error={cluster.error} /> : null}
      {schema.isLoading || cluster.isLoading ? <Loading /> : null}
      {schema.data && cluster.data ? (
        <SettingsList
          keys={keys}
          cluster={cluster.data.settings}
          node={node.data?.settings}
          canWriteCluster={can('settings', 'write')}
          canWriteNode={can('node', 'config')}
          onSave={onSave}
          grouped
        />
      ) : null}
    </div>
  );
}
