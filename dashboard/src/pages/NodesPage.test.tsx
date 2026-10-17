import { fireEvent, screen, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { describe, expect, it } from 'vitest';
import { server } from '../test/setup';
import { API, renderWithApp } from '../test/utils';
import MessagesPage, { messageKeys, objectLink, parseObjectRef } from './MessagesPage';
import NodesPage from './NodesPage';

describe('NodesPage', () => {
  it('shows degraded supervisors and the owner of each VIP', async () => {
    server.use(http.get(`${API}/edge`, () => HttpResponse.json({
      edge: { enabled: true, state: 'degraded', message: 'some supervisors are not ready' },
      vip: { enabled: true, state: 'running' }, backends: [], node: 'kudurru-0',
      supervisors: [{ node: 'worker-a', edge: { enabled: true, state: 'unavailable' }, vip: { enabled: true, state: 'running' } }],
      vips: [{ name: 'dns-a', node: 'worker-a', address: '192.0.2.53/24', state: 'MASTER', priority: 200 },
        { name: 'dns-b', node: 'worker-a', address: '192.0.2.54/24', state: 'BACKUP', priority: 180 }],
    })));
    renderWithApp(<NodesPage />, { route: '/nodes' });
    expect(await screen.findByText('degraded')).toBeInTheDocument();
    expect(screen.getByText('some supervisors are not ready')).toBeInTheDocument();
    const supervisors = screen.getByRole('list', { name: 'Edge supervisors' });
    expect(within(supervisors).getByText('dnsdist: unavailable; keepalived: running')).toBeInTheDocument();
    const vips = screen.getByRole('list', { name: 'VIP states' });
    expect(within(vips).getByText('dns-a on worker-a')).toBeInTheDocument();
    expect(within(vips).getByText('MASTER')).toBeInTheDocument();
    expect(within(vips).getByText('BACKUP')).toBeInTheDocument();
  });

  it('keeps a failed peer visible with unknown readiness and its status error', async () => {
    server.use(http.get(`${API}/nodes`, () => HttpResponse.json({ items: [
      { name: 'node-a', self: true, leader: true, ready: true, state: 'live', version: 'v0.1.0' },
      { name: 'node-b', self: false, leader: false, error: 'peer status: connection refused' },
    ] })));
    renderWithApp(<NodesPage />, { route: '/nodes' });
    const table = await screen.findByRole('table', { name: 'Nodes' });
    const peer = within(table).getByText('node-b').closest('tr')!;
    expect(within(peer).getAllByText('readiness unknown').length).toBeGreaterThan(0);
    expect(within(peer).getByText('peer status: connection refused')).toBeInTheDocument();
    expect(within(peer).queryByText('not ready')).toBeNull();
  });

  it('renders /nodes, /cluster and /edge as the server sends them', async () => {
    renderWithApp(<NodesPage />, { route: '/nodes' });
    const table = await screen.findByRole('table', { name: 'Nodes' });
    const self = within(table).getByText('kudurru-0').closest('tr')!;
    expect(within(self).getByText('this node')).toBeInTheDocument();
    expect(within(self).getByText('Live')).toBeInTheDocument();
    expect(within(self).getByText('leader')).toBeInTheDocument();
    // A peer without `ready` is unknown, not "not ready".
    const peer = within(table).getByText('kudurru-1').closest('tr')!;
    expect(within(peer).getAllByText('readiness unknown').length).toBeGreaterThan(0);
    expect(within(peer).queryByText('not ready')).toBeNull();
    expect(await screen.findByText('store sqlite')).toBeInTheDocument();
    expect(screen.getByText('dc493171-2dd3-4282-b0ab-c1a0a31ea26f')).toBeInTheDocument();
    expect(await screen.findByText('MASTER')).toBeInTheDocument();
    expect(screen.getByText('running')).toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/undefined|NaN/);
  });

  it('shows the health components of this node', async () => {
    renderWithApp(<NodesPage />, { route: '/nodes' });
    const table = await screen.findByRole('table', { name: 'Nodes' });
    const self = within(table).getByText('kudurru-0').closest('tr')!;
    fireEvent.click(within(self).getByRole('button', { name: 'Health' }));
    const health = await screen.findByRole('table', { name: 'Health components of kudurru-0' });
    for (const c of ['blocking', 'blocklists', 'dns', 'objects', 'zonecache']) expect(within(health).getByText(c)).toBeInTheDocument();
  });
});

describe('MessagesPage', () => {
  it('parses object references and links them', () => {
    expect(parseObjectRef('Policy/default/x')).toEqual({ kind: 'Policy', namespace: 'default', name: 'x' });
    expect(parseObjectRef('DNSZone/home-arpa')).toEqual({ kind: 'DNSZone', name: 'home-arpa' });
    expect(parseObjectRef('zones')).toBeNull();
    expect(objectLink({ kind: 'BlockList', namespace: 'default', name: 'ui-deny' })).toBe('/lists?q=ui-deny');
  });

  it('gives identical messages distinct keys', () => {
    const m = { severity: 'info' as const, source: 'policy', reason: 'R', message: 'same', node: 'n' };
    const keys = messageKeys([m, m, { ...m, message: 'other' }]);
    expect(new Set(keys).size).toBe(3);
  });

  it('renders the messages with their object links and sources', async () => {
    renderWithApp(<MessagesPage />, { route: '/messages' });
    const link = await screen.findByRole('link', { name: 'Policy default/iot-strict' });
    expect(link).toHaveAttribute('href', '/policies?q=iot-strict');
    expect(screen.getByRole('link', { name: 'BlockList default/ui-deny' })).toHaveAttribute('href', '/lists?q=ui-deny');
    expect(screen.getAllByText('blocklists').length).toBeGreaterThan(0);
  });
});
