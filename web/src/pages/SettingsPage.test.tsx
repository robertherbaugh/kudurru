import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import type { SettingKey } from '../api/types';
import { mockDB } from '../mocks/handlers';
import { renderWithApp, settingKey } from '../test/utils';
import SettingsPage, { SettingsList } from './SettingsPage';

const keys: SettingKey[] = [
  settingKey({ path: 'node.dataDir', type: 'string', scope: 'node', default: '/var/lib/kudurru', help: 'Data directory.', hostOnly: true, restartRequired: true }),
  settingKey({ path: 'log.level', type: 'string', scope: 'node', default: 'info', help: 'Log level.', envLocked: true }),
  settingKey({ path: 'cache.maxEntries', type: 'int', scope: 'cluster', default: 100000, help: 'Cache size.' }),
  settingKey({ path: 'querylog.enabled', type: 'bool', scope: 'cluster', default: true, help: 'Log queries.' }),
];

function row(path: string): HTMLElement {
  const el = document.querySelector<HTMLElement>(`[data-configkey="${path}"]`);
  if (!el) throw new Error(`no row for ${path}`);
  return el;
}

describe('SettingsList', () => {
  it('renders host-only keys read-only with the reason and no save', () => {
    renderWithApp(
      <SettingsList keys={keys} cluster={{ cache: { maxEntries: 50000 }, querylog: { enabled: true } }} node={{ node: { dataDir: '/data' }, log: { level: 'debug' } }} canWriteCluster canWriteNode onSave={vi.fn()} />,
      { user: null },
    );
    const host = row('node.dataDir');
    expect(host).toHaveAttribute('data-locked', 'true');
    const input = within(host).getByRole('textbox');
    expect(input).toHaveAttribute('readonly');
    expect(input).toHaveValue('/data');
    expect(within(host).getByTestId('lock-node.dataDir')).toHaveTextContent(
      'Host-only: set it in kudurru.yaml or KUDURRU_NODE__DATADIR on the host; the API never accepts it.',
    );
    expect(within(host).getByText('host only')).toBeInTheDocument();
    expect(within(host).queryByRole('button', { name: 'Save' })).toBeNull();
  });

  it('names the environment variable of env-locked keys', () => {
    renderWithApp(<SettingsList keys={keys} cluster={{}} node={{ log: { level: 'debug' } }} canWriteCluster canWriteNode onSave={vi.fn()} />, { user: null });
    const env = row('log.level');
    expect(env).toHaveAttribute('data-locked', 'true');
    expect(within(env).getByTestId('lock-log.level')).toHaveTextContent('Set by the environment variable KUDURRU_LOG__LEVEL; unset it to edit here.');
  });

  it('locks writable keys when the principal lacks the permission', () => {
    renderWithApp(<SettingsList keys={keys} cluster={{ cache: { maxEntries: 50000 } }} node={{}} canWriteCluster={false} canWriteNode={false} onSave={vi.fn()} />, {
      user: null,
    });
    expect(within(row('cache.maxEntries')).getByTestId('lock-cache.maxEntries')).toHaveTextContent('Needs the settings.write permission.');
  });

  it('parses, validates and saves an editable key', async () => {
    const onSave = vi.fn().mockResolvedValue(undefined);
    renderWithApp(<SettingsList keys={keys} cluster={{ cache: { maxEntries: 50000 } }} node={{}} canWriteCluster canWriteNode onSave={onSave} />, { user: null });
    const r = row('cache.maxEntries');
    expect(r).toHaveAttribute('data-locked', 'false');
    const input = within(r).getByRole('textbox');
    expect(input).toHaveValue('50000');
    fireEvent.change(input, { target: { value: 'lots' } });
    fireEvent.click(within(r).getByRole('button', { name: 'Save' }));
    expect(await within(r).findByText('Must be a whole number')).toBeInTheDocument();
    expect(onSave).not.toHaveBeenCalled();
    fireEvent.change(input, { target: { value: '250000' } });
    fireEvent.click(within(r).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(onSave).toHaveBeenCalledWith(keys[2], 250000));
  });
});

describe('SettingsPage', () => {
  it('generates every schema key in Expert view and patches cluster settings', async () => {
    renderWithApp(<SettingsPage />, { route: '/settings?view=expert' });
    await screen.findByText('cache.maxEntries');
    const all = document.querySelectorAll('[data-configkey]');
    expect(all.length).toBe(mockDB().schema.length);
    // Host-only keys from the real schema render locked.
    expect(row('listeners.api.address')).toHaveAttribute('data-locked', 'true');
    const r = row('cache.maxEntries');
    fireEvent.change(within(r).getByRole('textbox'), { target: { value: '123456' } });
    fireEvent.click(within(r).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect((mockDB().settings.cache as Record<string, unknown>).maxEntries).toBe(123456));
  });

  it('shows the real values from the settings envelope, booleans included', async () => {
    renderWithApp(<SettingsPage />, { route: '/settings' });
    // Values live under GET /settings .settings; the switches reflect them.
    const upstreams = await waitFor(() => within(row('upstreams.groups')).getByRole('textbox'));
    expect((upstreams as HTMLTextAreaElement).value).toContain('cloudflare=tls://1.1.1.1:853');
    expect(within(row('blocking.enabled')).getByRole('switch')).toBeChecked();
    expect(within(row('querylog.enabled')).getByRole('switch')).toBeChecked();
    expect(within(row('cache.maxEntries')).getByRole('textbox')).toHaveValue('100000');
    expect(within(row('upstreams.groups')).getByRole('link', { name: 'Edit on the Upstreams page' })).toHaveAttribute('href', '/upstreams');
  });

  it('turns a switch off, keeps it off after the save and warns when nothing was persisted', async () => {
    mockDB().settingsPersisted = false;
    renderWithApp(<SettingsPage />, { route: '/settings' });
    const sw = await waitFor(() => within(row('querylog.enabled')).getByRole('switch'));
    await waitFor(() => expect(sw).toBeChecked());
    fireEvent.click(sw);
    await waitFor(() => expect((mockDB().settings.querylog as Record<string, unknown>).enabled).toBe(false));
    expect(await screen.findByText('Kept in memory only')).toBeInTheDocument();
    expect(within(row('querylog.enabled')).getByRole('switch')).not.toBeChecked();
  });

  it('deletes map keys removed in the editor with null in the merge patch', async () => {
    renderWithApp(<SettingsPage />, { route: '/settings?view=expert&q=conditional' });
    const ta = await waitFor(() => within(row('upstreams.conditional')).getByRole('textbox'));
    expect((ta as HTMLTextAreaElement).value).toContain('corp.example');
    fireEvent.change(ta, { target: { value: '{}' } });
    fireEvent.click(within(row('upstreams.conditional')).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect((mockDB().settings.upstreams as Record<string, unknown>).conditional).toEqual({}));
  });

  it('shows only the basic keys and appearance in Basic view', async () => {
    renderWithApp(<SettingsPage />, { route: '/settings' });
    await screen.findByText('cache.maxEntries');
    expect(screen.getByRole('heading', { name: 'Appearance' })).toBeInTheDocument();
    expect(document.querySelector('[data-configkey="listeners.api.address"]')).toBeNull();
  });
});
