import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { ApiError } from '../api/client';
import type { SettingKey } from '../api/types';
import { mockDB } from '../mocks/handlers';
import { renderWithApp, settingKey } from '../test/utils';
import SettingsPage, { SettingsList } from './SettingsPage';

const keys: SettingKey[] = [
  settingKey({ path: 'node.dataDir', type: 'string', scope: 'node', default: '/var/lib/kudurru', help: 'Data directory.', hostOnly: true, restartRequired: true }),
  settingKey({ path: 'log.level', type: 'string', scope: 'node', default: 'info', help: 'Log level.', envLocked: true }),
  settingKey({ path: 'cache.maxEntries', type: 'int', scope: 'cluster', default: 100000, help: 'Cache size.', min: 0, max: 1000000, label: 'Maximum entries', group: 'Cache' }),
  settingKey({ path: 'querylog.enabled', type: 'bool', scope: 'cluster', default: true, help: 'Log queries.' }),
];

function row(path: string): HTMLElement {
  const el = document.querySelector<HTMLElement>(`[data-configkey="${path}"]`);
  if (!el) throw new Error(`no row for ${path}`);
  return el;
}

function list(ks: SettingKey[], cluster: Record<string, unknown>, onSave = vi.fn().mockResolvedValue(undefined)) {
  renderWithApp(<SettingsList keys={ks} cluster={cluster} node={{}} canWriteCluster canWriteNode onSave={onSave} />, { user: null });
  return onSave;
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

  it('shows the human label, the path and the section of each key', () => {
    list(keys, { cache: { maxEntries: 50000 } });
    const r = row('cache.maxEntries');
    expect(r.querySelector('.k-setting-label')).toHaveTextContent('Maximum entries');
    expect(within(r).getByRole('spinbutton', { name: 'Maximum entries' })).toBeInTheDocument();
    expect(within(r).getByText('cache.maxEntries')).toHaveClass('k-mono');
    expect(screen.getByRole('heading', { name: 'Cache' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Query log' })).toBeInTheDocument();
  });

  it('checks the bounds of a number and saves it', async () => {
    const onSave = list(keys, { cache: { maxEntries: 50000 } });
    const r = row('cache.maxEntries');
    expect(r).toHaveAttribute('data-locked', 'false');
    const input = within(r).getByRole('spinbutton');
    expect(input).toHaveValue(50000);
    fireEvent.change(input, { target: { value: '2000000' } });
    expect(within(r).getByText('Between 0 and 1000000')).toBeInTheDocument();
    fireEvent.click(within(r).getByRole('button', { name: 'Save' }));
    expect(await within(r).findByRole('alert')).toHaveTextContent('Between 0 and 1000000');
    expect(onSave).not.toHaveBeenCalled();
    fireEvent.change(input, { target: { value: '250000' } });
    fireEvent.click(within(r).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(onSave).toHaveBeenCalledWith(keys[2], 250000));
  });

  it('renders a server enum as a select with the help of the chosen option', async () => {
    const k = settingKey({
      path: 'blocking.defaultResponse',
      type: 'string',
      scope: 'cluster',
      default: 'zeroIP',
      enum: [
        { value: 'zeroIP', label: 'Zero IP', help: 'Unspecified address.' },
        { value: 'nxdomain', label: 'NXDOMAIN', help: 'The name does not exist.' },
      ],
      label: 'Default block response',
      group: 'Blocking',
    });
    const onSave = list([k], { blocking: { defaultResponse: 'zeroIP' } });
    const r = row('blocking.defaultResponse');
    const sel = within(r).getByRole('combobox', { name: 'Default block response' });
    expect(within(sel).getAllByRole('option').map((o) => o.textContent)).toEqual(['Zero IP', 'NXDOMAIN']);
    expect(within(r).getByText('Unspecified address.')).toBeInTheDocument();
    fireEvent.change(sel, { target: { value: 'nxdomain' } });
    expect(within(r).getByText('The name does not exist.')).toBeInTheDocument();
    fireEvent.click(within(r).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(onSave).toHaveBeenCalledWith(k, 'nxdomain'));
  });

  it('falls back to the bare type when the server sends no metadata', () => {
    const ks = [
      settingKey({ path: 'blocking.defaultResponse', type: 'string', scope: 'cluster', default: 'zeroIP' }),
      settingKey({ path: 'querylog.retention.raw', type: 'duration', scope: 'cluster', default: '720h' }),
      settingKey({ path: 'future.table', type: 'list', scope: 'cluster', default: [] }),
    ];
    list(ks, { blocking: { defaultResponse: 'nxdomain' }, querylog: { retention: { raw: '168h0m0s' } }, future: { table: [{ a: 1 }] } });
    expect(within(row('blocking.defaultResponse')).getByRole('textbox', { name: 'Default response' })).toHaveValue('nxdomain');
    expect(within(row('querylog.retention.raw')).getByRole('combobox', { name: 'Retention raw' })).toHaveValue('168h');
    expect(within(row('future.table')).getByRole<HTMLTextAreaElement>('textbox', { name: 'Table (JSON)' }).value).toContain('"a": 1');
    // Sections come from the path without a group.
    expect(screen.getByRole('heading', { name: 'Blocking' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Other' })).toBeInTheDocument();
  });

  it('names the failing parts of a rejected value (error.fields)', async () => {
    const k = settingKey({
      path: 'dns.forwarders',
      type: 'map',
      scope: 'cluster',
      default: {},
      values: { type: 'object', fields: [{ name: 'strategy', type: 'string' }] },
    });
    const err = new Error('settings: upstreams.groups.corp.strategy: "fastest" is not allowed', {
      cause: new ApiError(400, 'invalid', 'x', undefined, undefined, ['dns.forwarders.corp.strategy']),
    });
    const onSave = vi.fn().mockRejectedValue(err);
    renderWithApp(<SettingsList keys={[k]} cluster={{ dns: { forwarders: { corp: { strategy: 'strict' } } } }} node={{}} canWriteCluster canWriteNode onSave={onSave} />, { user: null });
    const r = row('dns.forwarders');
    fireEvent.change(within(r).getByRole('textbox', { name: 'strategy' }), { target: { value: 'fastest' } });
    fireEvent.click(within(r).getByRole('button', { name: 'Save' }));
    expect(await within(r).findByRole('alert')).toHaveTextContent('(at corp › strategy)');
  });

  it('edits string lists row by row with inline format errors', async () => {
    const k = settingKey({ path: 'ratelimit.exempt', type: 'string[]', scope: 'cluster', default: [], label: 'Exempt clients', items: { type: 'string', format: 'cidr' } });
    const onSave = list([k], { ratelimit: { exempt: ['10.0.0.0/8'] } });
    const r = row('ratelimit.exempt');
    fireEvent.click(within(r).getByRole('button', { name: 'Add' }));
    fireEvent.change(within(r).getByRole('textbox', { name: 'Exempt clients 2' }), { target: { value: '300.1.1.1' } });
    expect(within(r).getAllByText('Not an IP address or CIDR prefix').length).toBeGreaterThan(0);
    fireEvent.click(within(r).getByRole('button', { name: 'Save' }));
    expect(await within(r).findByRole('alert')).toHaveTextContent('300.1.1.1');
    fireEvent.change(within(r).getByRole('textbox', { name: 'Exempt clients 2' }), { target: { value: '192.168.1.0/24' } });
    fireEvent.click(within(r).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(onSave).toHaveBeenCalledWith(k, ['10.0.0.0/8', '192.168.1.0/24']));
  });

  it('offers reset to default for changed values and an Edit as JSON escape hatch', async () => {
    const k = settingKey({
      path: 'ratelimit.ipv4',
      type: 'map',
      scope: 'cluster',
      default: { '24': 6000, '32': 600 },
      keys: { type: 'int', min: 0, max: 32 },
      values: { type: 'int', min: 0 },
    });
    const onSave = list([k], { ratelimit: { ipv4: { '24': 9000, '32': 600 } } });
    const r = row('ratelimit.ipv4');
    expect(r).toHaveAttribute('data-changed', 'true');
    expect(within(r).getByText('changed')).toBeInTheDocument();
    expect(within(r).getAllByTestId('map-row')).toHaveLength(2);
    fireEvent.click(within(r).getByRole('button', { name: 'Edit as JSON' }));
    expect(within(r).getByRole<HTMLTextAreaElement>('textbox', { name: /JSON/ }).value).toContain('9000');
    fireEvent.click(within(r).getByRole('button', { name: 'Edit as form' }));
    fireEvent.click(within(r).getByRole('button', { name: 'Reset to default' }));
    await waitFor(() => expect(onSave).toHaveBeenCalledWith(k, null));
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
    fireEvent.change(within(r).getByRole('spinbutton'), { target: { value: '123456' } });
    fireEvent.click(within(r).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect((mockDB().settings.cache as Record<string, unknown>).maxEntries).toBe(123456));
  });

  it('shows the real values from the settings envelope, booleans included', async () => {
    renderWithApp(<SettingsPage />, { route: '/settings' });
    const summary = await waitFor(() => within(row('upstreams.groups')).getByTestId('groups-summary'));
    expect(within(summary).getByRole('link', { name: 'default' })).toHaveAttribute('href', '/upstreams/default');
    expect(within(summary).getByRole('link', { name: 'Edit on the Upstreams page' })).toHaveAttribute('href', '/upstreams');
    expect(within(row('blocking.enabled')).getByRole('switch')).toBeChecked();
    expect(within(row('querylog.enabled')).getByRole('switch')).toBeChecked();
    expect(within(row('cache.maxEntries')).getByRole('spinbutton')).toHaveValue(100000);
    expect(within(row('blocking.blockTTL')).getByRole('combobox', { name: 'Block response TTL' })).toHaveValue('1m');
  });

  it('saves an enum choice and shows the new value', async () => {
    renderWithApp(<SettingsPage />, { route: '/settings' });
    const sel = await waitFor(() => within(row('querylog.privacy')).getByRole('combobox', { name: 'Query log privacy' }));
    fireEvent.change(sel, { target: { value: 'hide_domains' } });
    fireEvent.click(within(row('querylog.privacy')).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect((mockDB().settings.querylog as Record<string, unknown>).privacy).toBe('hide_domains'));
    await waitFor(() => expect(within(row('querylog.privacy')).getByRole('combobox', { name: 'Query log privacy' })).toHaveValue('hide_domains'));
    expect(within(row('querylog.privacy')).getByText('changed')).toBeInTheDocument();
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

  it('deletes map rows removed in the editor with null in the merge patch', async () => {
    renderWithApp(<SettingsPage />, { route: '/settings?view=expert&q=conditional' });
    const r = await waitFor(() => row('upstreams.conditional'));
    await waitFor(() => expect(within(r).getByRole('textbox', { name: 'Domain suffix' })).toHaveValue('corp.example'));
    fireEvent.click(within(r).getByRole('button', { name: 'Remove corp.example' }));
    fireEvent.click(within(r).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect((mockDB().settings.upstreams as Record<string, unknown>).conditional).toEqual({}));
  });

  it('resets a key to its default with null', async () => {
    renderWithApp(<SettingsPage />, { route: '/settings' });
    const r = await waitFor(() => row('blocking.blockTTL'));
    expect(r).toHaveAttribute('data-changed', 'true');
    fireEvent.click(within(r).getByRole('button', { name: 'Reset to default' }));
    await waitFor(() => expect((mockDB().settings.blocking as Record<string, unknown>).blockTTL).toBe('10m'));
  });

  it('finds keys by their label', async () => {
    renderWithApp(<SettingsPage />, { route: '/settings?view=expert&q=bootstrap resolvers' });
    await waitFor(() => row('upstreams.bootstrap'));
    expect(document.querySelectorAll('[data-configkey]')).toHaveLength(1);
  });

  it('shows the block page section with the HTTPS caveat and a sandboxed preview', async () => {
    renderWithApp(<SettingsPage />, { route: '/settings' });
    const section = await screen.findByRole('region', { name: 'Block page' });
    expect(within(within(section).getByTestId('blockpage-intro')).getByText(/certificate warning/)).toBeInTheDocument();
    expect(within(section).getByRole('link', { name: 'Block page deployment notes' })).toHaveAttribute('href', expect.stringContaining('docs/deploy/blocking-page.md'));
    expect(within(section).getByText('blockPage.enabled')).toBeInTheDocument();
    expect(row('listeners.blockPage.http')).toHaveAttribute('data-locked', 'true');
    fireEvent.click(within(section).getByRole('button', { name: 'Preview' }));
    const dialog = await screen.findByRole('dialog');
    const frame = await within(dialog).findByTitle('Block page for ads.example.com');
    expect(frame).toHaveAttribute('sandbox', '');
    expect(frame.getAttribute('src')).toContain('/blockpage/preview?domain=ads.example.com');
    fireEvent.change(within(dialog).getByRole('textbox', { name: /^Blocked domain/ }), { target: { value: 'tracker.example.net' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Show' }));
    expect(await within(dialog).findByTitle('Block page for tracker.example.net')).toBeInTheDocument();
    expect(within(dialog).getByRole('link', { name: 'Open in a new tab' }).getAttribute('href')).toContain('/blockpage/preview?domain=tracker.example.net');
    // The server metadata offers the blockPage response.
    expect(within(row('blocking.defaultResponse')).getByRole('option', { name: 'Block page' })).toBeInTheDocument();
  });

  it('shows the server refusing the block page response without the block page inline', async () => {
    renderWithApp(<SettingsPage />, { route: '/settings' });
    const r = await waitFor(() => row('blocking.defaultResponse'));
    fireEvent.change(within(r).getByRole('combobox', { name: 'Default block response' }), { target: { value: 'blockPage' } });
    fireEvent.click(within(r).getByRole('button', { name: 'Save' }));
    expect(await within(r).findByRole('alert')).toHaveTextContent('blocking.defaultResponse blockPage needs blockPage.enabled: true');
    expect((mockDB().settings.blocking as Record<string, unknown>).defaultResponse).toBe('zeroIP');
  });

  it('shows only the basic keys and appearance in Basic view', async () => {
    renderWithApp(<SettingsPage />, { route: '/settings' });
    await screen.findByText('cache.maxEntries');
    expect(screen.getByRole('heading', { name: 'Appearance' })).toBeInTheDocument();
    expect(document.querySelector('[data-configkey="listeners.api.address"]')).toBeNull();
  });
});
