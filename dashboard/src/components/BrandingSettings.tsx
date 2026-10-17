import { Banner } from '@astryxdesign/core/Banner';
import { Button } from '@astryxdesign/core/Button';
import { FileInput } from '@astryxdesign/core/FileInput';
import { HStack, VStack } from '@astryxdesign/core/Stack';
import { Text } from '@astryxdesign/core/Text';
import { Theme } from '@astryxdesign/core/theme';
import { useQueryClient } from '@tanstack/react-query';
import { useEffect, useMemo, useRef, useState } from 'react';
import { qk, useApiContext } from '../api/context';
import type { SettingKey } from '../api/types';
import { brandingKey, useBranding } from '../lib/branding';
import { brandingAccentValid, brandingNameValid, brandingPNGValid, MAX_BRAND_BYTES, readBrandingPNG } from '../lib/brandingAssets';
import { brandingTheme } from '../lib/brandingTheme';
import { getPath, lockFor } from '../lib/settings';
import { useAppearance } from '../lib/theme';
import { TextField } from './fields/TextField';
import { Panel } from './layout/Page';

const FIELDS = ['name', 'logoLight', 'logoDark', 'favicon', 'accentColor'] as const;
type BrandDraft = Record<typeof FIELDS[number], string>;
export const isBrandingSetting = (path: string) => FIELDS.some((f) => path === `branding.${f}`);
function savedDraft(settings: Record<string, unknown>): BrandDraft {
  return Object.fromEntries(FIELDS.map((f) => { const v = getPath(settings, `branding.${f}`); return [f, typeof v === 'string' ? v : '']; })) as BrandDraft;
}
function valid(d: BrandDraft) { return brandingNameValid(d.name) && brandingAccentValid(d.accentColor) && brandingPNGValid(d.logoLight) && brandingPNGValid(d.logoDark) && brandingPNGValid(d.favicon, 256); }

function AssetPicker({ label, value, onChange, maxDimension, disabled, onReady }: { label: string; value: string; onChange: (v: string) => void; maxDimension: number; disabled: boolean; onReady: (ready: boolean) => void }) {
  const [file, setFile] = useState<File | null>(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const lifetime = useRef(0);
  useEffect(() => () => { lifetime.current++; }, []);
  const choose = async (next: File | File[] | null) => {
    const attempt = ++lifetime.current;
    const selected = Array.isArray(next) ? next[0] ?? null : next;
    setFile(selected); setError(''); setBusy(!!selected); onReady(!selected);
    if (!selected) { onChange(''); return; }
    try { const url = await readBrandingPNG(selected, maxDimension); if (attempt === lifetime.current) { onChange(url); onReady(true); } }
    catch { if (attempt === lifetime.current) setError(`Choose a static PNG, at most 64 KiB and ${maxDimension} × ${maxDimension} pixels. Animated PNGs are not supported.`); }
    finally { if (attempt === lifetime.current) setBusy(false); }
  };
  return <VStack gap={2}>
    <FileInput label={label} value={file} onChange={(f) => { void choose(f); }} accept="image/png" maxSize={MAX_BRAND_BYTES} isDisabled={disabled} isLoading={busy} description={`Static PNG only; up to 64 KiB and ${maxDimension} × ${maxDimension} pixels.`} status={error ? { type: 'error', message: error } : undefined} />
    {value ? <HStack gap={2} vAlign="center"><img className="k-brand-preview-image" src={value} alt={`${label} preview`} /><Button label={`Reset ${label.toLowerCase()}`} variant="ghost" size="sm" isDisabled={disabled} onClick={() => { ++lifetime.current; setFile(null); setBusy(false); setError(''); onReady(true); onChange(''); }} /></HStack> : null}
  </VStack>;
}
function Editor({ initial, onClose }: { initial: BrandDraft; onClose: () => void }) {
  const { api, client } = useApiContext();
  const qc = useQueryClient();
  const { theme } = useAppearance();
  const [draft, setDraft] = useState(initial);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [changed, setChanged] = useState(false);
  const [assetReady, setAssetReady] = useState<Record<string, boolean>>({});
  const [revision, setRevision] = useState(0);
  const ready = Object.values(assetReady).every(Boolean);
  const alive = useRef(true);
  useEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  const edit = (key: keyof BrandDraft, value: string) => { setChanged(true); setDraft((d) => ({ ...d, [key]: value })); };
  const previewTheme = useMemo(() => brandingTheme(theme, draft.accentColor, true), [theme, draft.accentColor]);
  const save = async () => {
    if (saving || !ready || !valid(draft)) return;
    setSaving(true); setError('');
    try {
      const result = await api.settings.patch({ branding: draft });
      if (!alive.current) return;
      qc.setQueryData(qk.settings, result);
      void qc.invalidateQueries({ queryKey: qk.schema });
      void qc.invalidateQueries({ queryKey: brandingKey(client.baseUrl) });
      onClose();
    } catch {
      if (alive.current) setError('Branding could not be confirmed as saved. Refresh settings before trying again.');
    } finally { if (alive.current) setSaving(false); }
  };
  return <VStack gap={4}>
    <TextField label="Organization name" value={draft.name} onChange={(v) => edit('name', v)} isDisabled={saving} help="Up to 80 characters, without control characters. Empty restores kudurru." error={!brandingNameValid(draft.name) ? 'Use at most 80 characters without controls.' : undefined} />
    <AssetPicker key={`logoLight-${revision}`} onReady={(v) => setAssetReady((r) => ({ ...r, logoLight: v }))} label="Light logo" value={draft.logoLight} maxDimension={1024} disabled={saving} onChange={(v) => edit('logoLight', v)} />
    <AssetPicker key={`logoDark-${revision}`} onReady={(v) => setAssetReady((r) => ({ ...r, logoDark: v }))} label="Dark logo" value={draft.logoDark} maxDimension={1024} disabled={saving} onChange={(v) => edit('logoDark', v)} />
    <AssetPicker key={`favicon-${revision}`} onReady={(v) => setAssetReady((r) => ({ ...r, favicon: v }))} label="Favicon" value={draft.favicon} maxDimension={256} disabled={saving} onChange={(v) => edit('favicon', v)} />
    <TextField label="Accent color" value={draft.accentColor} onChange={(v) => edit('accentColor', v)} isDisabled={saving} help="A six-digit hex color, such as #0064E0. Empty uses the selected theme's accent. Text contrast is derived automatically." error={!brandingAccentValid(draft.accentColor) ? 'Use #RRGGBB or leave empty.' : undefined} />
    <HStack gap={3} wrap="wrap">
      {(['light', 'dark'] as const).map((mode) => <Theme key={mode} theme={previewTheme} mode={mode}><VStack gap={2} padding={3} className="k-brand-preview" aria-label={`${mode} branding preview`}>
        <Text type="supporting">{mode === 'light' ? 'Light preview' : 'Dark preview'}</Text>
        {(mode === 'light' ? draft.logoLight || draft.logoDark : draft.logoDark || draft.logoLight) ? <img className="k-brand-preview-image" src={mode === 'light' ? draft.logoLight || draft.logoDark : draft.logoDark || draft.logoLight} alt={draft.name || 'kudurru'} /> : <Text weight="semibold">{draft.name || 'kudurru'}</Text>}
        <Button label="Preview button" variant="primary" isDisabled />
      </VStack></Theme>)}
    </HStack>
    {error ? <Banner status="error" title="Save not confirmed" description={error} /> : null}
    <HStack gap={2} wrap="wrap"><Button label="Save branding" variant="primary" isDisabled={!changed || !ready || !valid(draft)} isLoading={saving} onClick={() => { void save(); }} /><Button label="Cancel branding" variant="ghost" isDisabled={saving} onClick={onClose} /><Button label="Reset branding fields" variant="secondary" isDisabled={saving} onClick={() => { setChanged(true); setAssetReady({}); setRevision((n) => n + 1); setDraft({ name: '', logoLight: '', logoDark: '', favicon: '', accentColor: '' }); }} /></HStack>
  </VStack>;
}

export function BrandingSettings({ settings, schema, canWrite }: { settings: Record<string, unknown>; schema: SettingKey[]; canWrite: boolean }) {
  const brand = useBranding();
  const [editing, setEditing] = useState(false);
  const value = savedDraft(settings);
  const keys = schema.filter((k) => isBrandingSetting(k.path));
  const lock = keys.map((k) => lockFor(k, canWrite)).find((k) => k.locked);
  const reason = !brand.enabled ? 'Custom branding requires an active Enterprise license. Saved branding is retained when the license changes.' : keys.length !== FIELDS.length ? 'Branding settings are unavailable on this server.' : lock?.locked ? lock.reason : undefined;
  return <Panel title="Branding" headingId="sec-branding" description="Organization identity for the dashboard, sign-in page, and block page. Cluster-wide; your browser's theme, mode, and contrast remain selected." actions={!editing || reason ? <Button label="Edit branding" size="sm" variant="secondary" isDisabled={!!reason} tooltip={reason} onClick={() => setEditing(true)} /> : undefined}>
    {reason ? <Text type="supporting">{reason}</Text> : null}
    {editing && !reason ? <Editor initial={value} onClose={() => setEditing(false)} /> : <VStack gap={2}>{FIELDS.map((f) => <Text key={f} data-configkey={`branding.${f}`}>{f === 'name' ? `Organization: ${value.name || 'kudurru'}` : f === 'accentColor' ? `Accent: ${value.accentColor || 'selected theme'}` : `${f === 'logoLight' ? 'Light logo' : f === 'logoDark' ? 'Dark logo' : 'Favicon'}: ${value[f] ? 'custom PNG' : 'default'}`}</Text>)}</VStack>}
  </Panel>;
}
