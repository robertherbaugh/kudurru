import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { PROVIDERS, serversFor, transportsOf } from '../lib/upstreamPresets';
import { groupFieldErrors, withMode } from '../components/UpstreamGroup';
import { bootstrapError, conditionalKeyError, groupPatch, mapPatch, serverError, serverLabel, upstreamsOf, type UpstreamGroupConfig } from '../lib/upstreams';
import { mockDB } from '../mocks/handlers';
import { findSelector, getSelector, pick } from '../test/controls';
import { renderWithApp } from '../test/utils';
import UpstreamGroupPage from './UpstreamGroupPage';
import UpstreamsPage from './UpstreamsPage';
import { UpstreamWizardDialog } from '../components/wizards/UpstreamWizard';

type SavedGroup = { mode?: string; servers?: string[] | null; strategy?: string; timeout?: string };

function groups(): Record<string, SavedGroup> {
  return (mockDB().settings.upstreams as { groups: Record<string, SavedGroup> }).groups;
}

function recursion(): Record<string, unknown> {
  return (mockDB().settings.upstreams as { recursion: Record<string, unknown> }).recursion;
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

  it('reads the mode and the recursion options, forward and the defaults when missing', () => {
    const cfg = upstreamsOf({ settings: { upstreams: { groups: { a: { servers: ['1.1.1.1'] }, b: { mode: 'recursive', servers: null } } } } } as never);
    expect(cfg.groups.a?.mode).toBe('forward');
    expect(cfg.groups.b).toEqual({ mode: 'recursive', servers: [], strategy: '', timeout: '' });
    expect(cfg.recursion).toEqual({ dnssec: true, qnameMinimization: true, ipv6: 'auto', trustAnchors: [], rootHints: [] });
  });

  it('validates and saves a group by mode, never keeping the other mode\'s fields', () => {
    const fwd: UpstreamGroupConfig = { mode: 'forward', servers: ['1.1.1.1', ' '], strategy: 'strict', timeout: '2s' };
    expect(groupFieldErrors({ ...fwd, servers: [] }).servers).toBe('A group needs at least one server');
    expect(groupFieldErrors({ ...fwd, mode: 'recursive', servers: [] })).toEqual({});
    expect(groupFieldErrors({ ...fwd, mode: 'recursive' }).servers).toContain('has no servers');
    expect(groupPatch(fwd)).toEqual({ mode: null, servers: ['1.1.1.1'], strategy: 'strict', timeout: '2s' });
    expect(groupPatch({ ...fwd, mode: 'recursive', timeout: '4s' })).toEqual({ mode: 'recursive', servers: null, strategy: null, timeout: '4s' });
    // A timeout at the old mode's default follows the mode; a chosen one stays.
    expect(withMode(fwd, 'recursive').timeout).toBe('4s');
    expect(withMode(withMode(fwd, 'recursive'), 'forward').timeout).toBe('2s');
    expect(withMode({ ...fwd, timeout: '3s' }, 'recursive').timeout).toBe('3s');
  });
});

describe('provider presets', () => {
  it('turns a preset into valid, named server entries', () => {
    const quad9 = PROVIDERS.find((p) => p.id === 'quad9')!;
    const v = quad9.variants[0]!;
    expect(transportsOf(v)).toEqual(['doh', 'dot', 'plain']);
    expect(serversFor(quad9, v, 'dot')).toEqual(['quad9-dot=tls://dns.quad9.net']);
    expect(serversFor(quad9, v, 'plain', { ipv6: true })).toEqual(['quad9-1=9.9.9.9:53', 'quad9-2=149.112.112.112:53', 'quad9-3=[2620:fe::fe]:53', 'quad9-4=[2620:fe::9]:53']);
    const next = PROVIDERS.find((p) => p.id === 'nextdns')!;
    expect(serversFor(next, next.variants[0]!, 'doq', { id: 'abc123' })).toEqual(['nextdns-doq=quic://abc123.dns.nextdns.io']);
    // Mullvad is encrypted only.
    expect(transportsOf(PROVIDERS.find((p) => p.id === 'mullvad')!.variants[0]!)).toEqual(['doh', 'dot']);
    for (const p of PROVIDERS)
      for (const variant of p.variants)
        for (const t of transportsOf(variant)) for (const e of serversFor(p, variant, t, { id: 'abc123', ipv6: true })) expect([e, serverError(e)]).toEqual([e, null]);
  });
});

describe('UpstreamsPage', () => {
  it('lists the groups with strategy, health and traffic, linking to their pages', async () => {
    renderWithApp(<UpstreamsPage />, { route: '/upstreams' });
    const def = await screen.findByTestId('group-default');
    expect(within(def).getByText('Parallel best')).toBeInTheDocument();
    expect(within(def).getByRole('link', { name: 'default' })).toHaveAttribute('href', '/upstreams/default');
    expect(await within(def).findByText('2 of 2 healthy')).toBeInTheDocument();
    const filtered = screen.getByTestId('group-filtered');
    expect(within(filtered).getByText('Strict order')).toBeInTheDocument();
    expect(await within(filtered).findByText('0 of 1 healthy')).toBeInTheDocument();
    const resolver = screen.getByTestId('group-resolver');
    expect(within(resolver).getByText('Recursive')).toBeInTheDocument();
    expect(within(resolver).getByText('4s')).toBeInTheDocument();
    expect(await within(resolver).findByText('1 of 1 healthy')).toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/undefined|NaN/);
  });

  it('saves the recursion options and links to the rest in Settings', async () => {
    renderWithApp(<UpstreamsPage />, { route: '/upstreams' });
    const panel = await screen.findByTestId('recursion');
    expect(within(panel).getByRole('link', { name: 'Settings' })).toHaveAttribute('href', '/settings?view=expert&q=upstreams.recursion');
    expect(within(panel).queryByText(/No group is recursive yet/)).toBeNull();
    fireEvent.click(within(panel).getByRole('switch', { name: /DNSSEC validation/ }));
    pick(getSelector('IPv6 to name servers', panel), 'Off');
    fireEvent.click(within(panel).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(recursion()).toMatchObject({ dnssec: false, qnameMinimization: true, ipv6: 'off' }));
  });

  it('turns read-only with the explanation when the settings are managed elsewhere', async () => {
    mockDB().settingsManagedBy = 'Kubernetes (the kudurru-settings ConfigMap)';
    renderWithApp(<UpstreamsPage />, { route: '/upstreams' });
    await screen.findByTestId('group-default');
    fireEvent.click(await screen.findByRole('button', { name: 'Add resolver' }));
    fireEvent.change(screen.getByRole('textbox', { name: 'Resolvers 2' }), { target: { value: '9.9.9.9:53' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect((await screen.findAllByText(/managed by Kubernetes/)).length).toBeGreaterThan(0);
    expect(await screen.findByText('Read-only here')).toBeInTheDocument();
    const btn = screen.getByRole('button', { name: 'New group' });
    await waitFor(() => expect(btn.getAttribute('aria-disabled') === 'true' || btn.hasAttribute('disabled')).toBe(true));
  });
});

describe('UpstreamGroupPage', () => {
  it('shows the servers with health and RTT, saves typed settings and lists the users', async () => {
    renderWithApp(<UpstreamGroupPage params={{ group: 'default' }} />, { route: '/upstreams/default' });
    const cf = (await screen.findByText('cloudflare')).closest('tr')!;
    expect(within(cf).getByText('tls://1.1.1.1:853')).toBeInTheDocument();
    expect(await within(cf).findByText('healthy')).toBeInTheDocument();
    expect(within(cf).getByText('8.4 ms')).toBeInTheDocument();
    pick(getSelector('Strategy'), 'Strict order');
    expect(screen.getByText('Ask the servers in order, moving on only on failure.')).toBeInTheDocument();
    pick(getSelector('Timeout'), '5 seconds');
    fireEvent.click(screen.getByRole('button', { name: 'Add server' }));
    fireEvent.change(screen.getByRole('textbox', { name: 'Servers 3' }), { target: { value: 'ftp://nope' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText(/Line 3 \(ftp:\/\/nope\): Unknown scheme/)).toBeInTheDocument();
    fireEvent.change(screen.getByRole('textbox', { name: 'Servers 3' }), { target: { value: 'google=https://dns.google/dns-query' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() =>
      expect(groups().default).toEqual({ servers: ['cloudflare=tls://1.1.1.1:853', 'quad9=tls://9.9.9.9:853', 'google=https://dns.google/dns-query'], strategy: 'strict', timeout: '5s' }),
    );
    expect(screen.getByText(/Every query no policy/)).toBeInTheDocument();
  });

  it('turns a forwarding group recursive without keeping its servers', async () => {
    renderWithApp(<UpstreamGroupPage params={{ group: 'default' }} />, { route: '/upstreams/default' });
    await screen.findByText('cloudflare');
    pick(getSelector('Mode'), 'Recursive');
    expect(screen.getByText(/starting from the root servers, so no single provider sees every name/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Add server' })).toBeNull();
    expect(screen.queryByRole('combobox', { name: 'Strategy' })).toBeNull();
    expect(getSelector('Timeout')).toHaveTextContent('4 seconds');
    expect(screen.getByText('For the whole lookup of a name')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(groups().default).toEqual({ mode: 'recursive', timeout: '4s' }));
  });

  it('shows the recursive resolver as the server of a recursive group', async () => {
    renderWithApp(<UpstreamGroupPage params={{ group: 'resolver' }} />, { route: '/upstreams/resolver' });
    const row = (await screen.findByText('Recursive resolver')).closest('tr')!;
    expect(await within(row).findByText('healthy')).toBeInTheDocument();
    expect(within(screen.getByTestId('group-state')).getByText('Recursive')).toBeInTheDocument();
    expect(screen.getByRole('region', { name: 'Resolver' })).toBeInTheDocument();
    pick(getSelector('Mode'), 'Forward');
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('A group needs at least one server')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Add server' }));
    fireEvent.change(screen.getByRole('textbox', { name: 'Servers 1' }), { target: { value: '192.168.1.1' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(groups().resolver).toEqual({ servers: ['192.168.1.1'], strategy: 'parallel_best', timeout: '2s' }));
  });

  it('deletes a group with a null in the merge patch after naming its users', async () => {
    mockDB().settings = { ...mockDB().settings, upstreams: { ...(mockDB().settings.upstreams as object), conditional: {} } };
    renderWithApp(<UpstreamGroupPage params={{ group: 'filtered' }} />, { route: '/upstreams/filtered' });
    const used = await screen.findByTestId('used-by');
    expect(await within(used).findByRole('link', { name: 'default/iot-strict' })).toHaveAttribute('href', '/policies/default/iot-strict');
    const del = screen.getByRole('button', { name: 'Delete' });
    await waitFor(() => expect(del).not.toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(del);
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText(/policy default\/iot-strict/)).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole('button', { name: 'Delete' }));
    await waitFor(() => expect(groups().filtered).toBeUndefined());
    expect(groups().default).toBeDefined();
  });

  it('says so for a group that does not exist', async () => {
    renderWithApp(<UpstreamGroupPage params={{ group: 'nope' }} />, { route: '/upstreams/nope' });
    expect(await screen.findByText('No such upstream group')).toBeInTheDocument();
  });
});

describe('UpstreamWizardDialog', () => {
  it('creates a group from a provider preset', async () => {
    renderWithApp(<UpstreamWizardDialog isOpen onClose={() => {}} />, { route: '/upstreams?new=1' });
    expect(await screen.findByRole('radio', { name: /^Forward/ })).toBeChecked();
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    const quad9 = await screen.findByRole('radio', { name: /^Quad9/ });
    await waitFor(() => expect(quad9).toBeEnabled());
    // Icons from the C4 index where the server has one, monograms elsewhere.
    await waitFor(() => expect(document.querySelector('[data-choice="cloudflare"] img')?.getAttribute('src')).toContain('/catalogue/icons/cloudflare.svg'));
    expect(document.querySelector('[data-choice="opendns"] img')?.getAttribute('src')).toContain('/catalogue/icons/cisco.svg');
    expect(document.querySelector('[data-choice="controld"] img')?.getAttribute('src')).toContain('/catalogue/icons/control-d.svg');
    // No Quad9 logo in dashboard-icons: a monogram.
    expect(document.querySelector('[data-choice="quad9"] [data-icon]')).toHaveTextContent('Q9');
    fireEvent.click(quad9);
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    pick(await findSelector('Transport'), /^DNS over TLS/);
    expect(within(screen.getByTestId('preset-servers')).getByText('tls://dns.quad9.net')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(await screen.findByRole('textbox', { name: /^Name/ })).toHaveValue('quad9');
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(await screen.findByTestId('upstream-review')).toHaveTextContent('quad9-dot=tls://dns.quad9.net');
    fireEvent.click(screen.getByRole('button', { name: 'Create group' }));
    await waitFor(() => expect(groups().quad9).toEqual({ servers: ['quad9-dot=tls://dns.quad9.net'], strategy: 'parallel_best', timeout: '2s' }));
  });

  it('needs the NextDNS profile ID and validates custom servers and the name', async () => {
    renderWithApp(<UpstreamWizardDialog isOpen onClose={() => {}} />, { route: '/upstreams?new=1' });
    fireEvent.click(await screen.findByRole('button', { name: 'Next' }));
    fireEvent.click(await screen.findByRole('radio', { name: /^NextDNS/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    await screen.findByRole('textbox', { name: /^Profile ID/ });
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(await screen.findByText(/Profile ID: The six-character ID/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Back' }));
    fireEvent.click(await screen.findByRole('radio', { name: /^Custom/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Add server' }));
    fireEvent.change(screen.getByRole('textbox', { name: 'Servers 1' }), { target: { value: '10.0.0.53' } });
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    const name = await screen.findByRole('textbox', { name: /^Name/ });
    fireEvent.change(name, { target: { value: 'default' } });
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    expect(await screen.findByText('A group with this name exists')).toBeInTheDocument();
    fireEvent.change(name, { target: { value: 'corp' } });
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Create group' }));
    await waitFor(() => expect(groups().corp?.servers).toEqual(['10.0.0.53']));
  });

  it('creates a recursive group without a provider or servers', async () => {
    renderWithApp(<UpstreamWizardDialog isOpen onClose={() => {}} />, { route: '/upstreams?new=1' });
    fireEvent.click(await screen.findByRole('radio', { name: /^Recursive/ }));
    expect(screen.getByText(/needs outbound DNS \(UDP and TCP port 53\)/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    // Straight to the name and timeout: no provider, servers or strategy.
    const name = await screen.findByRole('textbox', { name: /^Name/ });
    expect(screen.queryByRole('radio', { name: /^Quad9/ })).toBeNull();
    expect(screen.queryByRole('combobox', { name: 'Strategy' })).toBeNull();
    expect(getSelector('Timeout')).toHaveTextContent('4 seconds');
    // The mock already has a group named resolver, so nothing is suggested.
    expect(name).toHaveValue('');
    fireEvent.change(name, { target: { value: 'private' } });
    fireEvent.click(screen.getByRole('button', { name: 'Next' }));
    const review = await screen.findByTestId('upstream-review');
    expect(review).toHaveTextContent('Recursive, from the root servers with DNSSEC validation');
    expect(review).toHaveTextContent('4s per lookup');
    expect(review).not.toHaveTextContent('Strategy');
    fireEvent.click(screen.getByRole('button', { name: 'Create group' }));
    await waitFor(() => expect(groups().private).toEqual({ mode: 'recursive', timeout: '4s' }));
  });
});
