import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { bootstrapError, conditionalKeyError, mapPatch, serverError, serverLabel } from '../lib/upstreams';
import { mockDB } from '../mocks/handlers';
import { renderWithApp } from '../test/utils';
import UpstreamsPage from './UpstreamsPage';

function groups(): Record<string, { servers: string[]; strategy: string; timeout: string }> {
  return (mockDB().settings.upstreams as { groups: Record<string, { servers: string[]; strategy: string; timeout: string }> }).groups;
}

describe('upstream helpers', () => {
  it.each([
    '1.1.1.1',
    '1.1.1.1:53',
    '[2606:4700::1111]:53',
    '2606:4700::1111',
    'dns.google',
    'udp://9.9.9.9:53',
    'tcp://9.9.9.9',
    'tls://dns.quad9.net',
    'tls://1.1.1.1:853',
    'https://dns.google/dns-query',
    'https://user:pw@doh.example/dns-query?x=1',
    'quic://dns.adguard-dns.com',
    'h3://dns.google/dns-query',
    'sdns://AgcAAAAAAAAABzEuMC4wLjEAEmRucy5jbG91ZGZsYXJlLmNvbQovZG5zLXF1ZXJ5',
    'cloudflare=tls://1.1.1.1',
  ])('accepts %s', (s) => expect(serverError(s)).toBeNull());

  it.each([
    ['', 'A server address is required'],
    ['ftp://x', 'Unknown scheme'],
    ['1.1.1.1:99999', 'The port must be 1-65535'],
    ['256.1.1.1', 'Not an IPv4 address'],
    ['tls://1.1.1.1/path', 'tls:// takes host[:port] only'],
    ['sdns://x', 'Not a DNS stamp'],
    ['=1.1.1.1', 'display name'],
    ['dns.google/dns-query', 'Use a scheme'],
  ])('rejects %s', (s, msg) => expect(serverError(s)).toContain(msg));

  it('labels servers like the query log and checks bootstrap and conditional keys', () => {
    expect(serverLabel('quad9=tls://9.9.9.9')).toBe('quad9');
    expect(serverLabel('https://dns.google/dns-query?a=b')).toBe('https://dns.google/dns-query?a=b');
    expect(bootstrapError('1.1.1.1:53')).toBeNull();
    expect(bootstrapError('dns.google')).toContain('An IP address');
    expect(conditionalKeyError('corp.example')).toBeNull();
    expect(conditionalKeyError('[/corp.example/10.in-addr.arpa/]')).toBeNull();
    expect(conditionalKeyError('bad..name')).not.toBeNull();
    expect(mapPatch({ a: 1, b: 2 }, { b: 3, c: 4 })).toEqual({ a: null, b: 3, c: 4 });
  });
});

describe('UpstreamsPage', () => {
  it('shows every group with per-server health, RTT and traffic', async () => {
    renderWithApp(<UpstreamsPage />, { route: '/upstreams' });
    const def = await screen.findByTestId('group-default');
    expect(within(def).getByText('Parallel best')).toBeInTheDocument();
    const cf = within(def).getByText('cloudflare').closest('tr')!;
    expect(within(cf).getByText('tls://1.1.1.1:853')).toBeInTheDocument();
    expect(await within(cf).findByText('healthy')).toBeInTheDocument();
    expect(within(cf).getByText('8.4 ms')).toBeInTheDocument();
    const filtered = screen.getByTestId('group-filtered');
    expect(await within(filtered).findByText(/^backoff/)).toBeInTheDocument();
    expect(within(filtered).getByText('Strict order')).toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/undefined|NaN/);
  });

  it('creates a group from the structured form after validating the servers', async () => {
    renderWithApp(<UpstreamsPage />, { route: '/upstreams' });
    await screen.findByTestId('group-default');
    const newGroup = screen.getByRole('button', { name: 'New group' });
    await waitFor(() => expect(newGroup).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(newGroup);
    const dialog = await screen.findByRole('dialog');
    fireEvent.change(within(dialog).getByRole('textbox', { name: /^Name/ }), { target: { value: 'corp' } });
    fireEvent.change(within(dialog).getByRole('textbox', { name: /^Servers/ }), { target: { value: '10.0.0.53\nftp://nope' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Create group' }));
    expect(await within(dialog).findByText(/Line 2 \(ftp:\/\/nope\): Unknown scheme/)).toBeInTheDocument();
    fireEvent.change(within(dialog).getByRole('textbox', { name: /^Servers/ }), { target: { value: '10.0.0.53\ncorp-dot=tls://dns.corp.example' } });
    fireEvent.change(within(dialog).getByRole('combobox', { name: /^Strategy/ }), { target: { value: 'strict' } });
    fireEvent.change(within(dialog).getByRole('textbox', { name: /^Timeout/ }), { target: { value: '500ms' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Create group' }));
    await waitFor(() => expect(groups().corp).toEqual({ servers: ['10.0.0.53', 'corp-dot=tls://dns.corp.example'], strategy: 'strict', timeout: '500ms' }));
    expect(await screen.findByTestId('group-corp')).toBeInTheDocument();
  });

  it('deletes a group with a null in the merge patch', async () => {
    mockDB().settings = { ...mockDB().settings, upstreams: { ...(mockDB().settings.upstreams as object), conditional: {} } };
    renderWithApp(<UpstreamsPage />, { route: '/upstreams' });
    const filtered = await screen.findByTestId('group-filtered');
    const del = within(filtered).getByRole('button', { name: 'Delete' });
    await waitFor(() => expect(del).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(del);
    const dialog = await screen.findByRole('dialog');
    // The iot-strict policy still routes to "filtered".
    expect(await within(dialog).findByText(/policy iot-strict/)).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole('button', { name: 'Delete' }));
    await waitFor(() => expect(groups().filtered).toBeUndefined());
    expect(groups().default).toBeDefined();
  });

  it('turns read-only with the explanation when the settings are managed elsewhere', async () => {
    mockDB().settingsManagedBy = 'Kubernetes (the kudurru-settings ConfigMap)';
    renderWithApp(<UpstreamsPage />, { route: '/upstreams' });
    const def = await screen.findByTestId('group-default');
    const edit = within(def).getByRole('button', { name: 'Edit' });
    await waitFor(() => expect(edit).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(edit);
    const dialog = await screen.findByRole('dialog');
    fireEvent.click(within(dialog).getByRole('button', { name: 'Save' }));
    expect(await within(dialog).findByText(/managed by Kubernetes/)).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    expect(await screen.findByText('Read-only here')).toBeInTheDocument();
    const after = within(screen.getByTestId('group-default')).getByRole('button', { name: 'Edit' });
    await waitFor(() => expect(after.getAttribute('aria-disabled') === 'true' || after.hasAttribute('disabled')).toBe(true));
  });
});
