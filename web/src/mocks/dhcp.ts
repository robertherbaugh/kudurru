import { http, HttpResponse } from 'msw';
import type { DHCPConfig, DHCPLease, DHCPPoolOptions, DHCPReservation, DHCPSetup, DHCPSetupRequest } from '../api/types';

export interface DHCPMock { setup?: DHCPSetup; enabled: boolean; config: DHCPConfig; leases: DHCPLease[] }
function fixture(): DHCPMock {
  return {
    enabled: true,
    config: {
      took_ms: 1, node: 'stone-1', scope: 'default', management: 'api', revision: '1', etag: '"stone-1-default-1"', envLocked: [],
      topology: { interface: 'eth0', serverIP: '192.0.2.2', subnet: '192.0.2.0/24', relayOnly: false, trustedRelays: [] },
      options: { rangeStart: '192.0.2.100', rangeEnd: '192.0.2.199', router: '192.0.2.1', dnsServers: ['192.0.2.2'], domainName: 'lan.home.arpa', leaseDuration: '12h', registerDNS: true, dnsTTL: '30s' },
      reservations: [{ mac: '02:00:00:00:00:10', ip: '192.0.2.10', hostname: 'printer' }],
    },
    leases: [
      { address: '192.0.2.10', mac: '02:00:00:00:00:10', clientID: '02:00:00:00:00:10', hostname: 'printer', state: 'bound', expiresAt: new Date(Date.now() + 86400_000).toISOString() },
      { address: '192.0.2.100', mac: '02:00:00:00:00:20', clientID: '02:00:00:00:00:20', hostname: 'laptop', state: 'bound', expiresAt: new Date(Date.now() + 3600_000).toISOString() },
    ],
  };
}
let state = fixture();
export const mockDHCP = () => state;
export const resetDHCP = () => { state = fixture(); };

export function mockDHCPSetup(): DHCPSetup {
  return state.setup ?? { node: state.config.node, took_ms: 1, enabled: state.enabled, managedBy: state.config.management,
    available: true, unavailableReason: '', runtimeError: '', environmentLocked: state.config.envLocked.length > 0, restartRequired: false,
    revision: '1', etag: '"setup-1"', scopeRevisions: { default: state.config.revision }, pendingScopes: [],
    scopes: [{ name: state.config.scope, ...state.config.topology, ...state.config.options, reservations: state.config.reservations }] };
}

type Guard = (r: Request, resource: string, verb: string) => Response | null;
const fail = (status: number, key: string, message: string) => HttpResponse.json({ error: { key, message }, took_ms: 1 }, { status });
function revise() {
  state.config.revision = String(Number(state.config.revision) + 1);
  state.config.etag = `"stone-1-default-${state.config.revision}"`;
}
function change(request: Request) {
  if (state.config.management !== 'api') return fail(409, 'managed_by', 'Scope is managed by the configuration file');
  if (request.headers.get('If-Match') !== state.config.etag) return fail(412, 'precondition_failed', 'The DHCP scope changed; reload its saved values');
  return null;
}

export function dhcpHandlers(guard: Guard) {
  const base = '*/api/v1/node/dhcp/v4';
  const path = `${base}/scopes/:scope`;
  const check = (request: Request, scope: unknown, resource: string, verb: string) => guard(request, resource, verb)
    ?? (!state.enabled ? fail(503, 'unavailable', 'DHCP is disabled') : String(scope) !== state.config.scope ? fail(404, 'not_found', 'Unknown DHCP scope') : null);
  return [
    http.get(`${base}/setup`, ({ request }) => guard(request, 'dhcp', 'read') ?? HttpResponse.json(mockDHCPSetup())),
    http.get(`${base}/interfaces`, ({ request }) => guard(request, 'dhcp', 'read') ?? HttpResponse.json({ node: state.config.node, took_ms: 1,
      items: [{ name: 'eth0', addresses: ['192.0.2.2'], up: true, broadcast: true, loopback: false }, { name: 'eth1', addresses: ['198.51.100.2'], up: true, broadcast: true, loopback: false }] })),
    http.put(`${base}/setup`, async ({ request }) => {
      const error = guard(request, 'dhcp', 'write');
      if (error) return error;
      const current = mockDHCPSetup();
      const body = await request.json() as DHCPSetupRequest;
      if (request.headers.get('If-Match') !== current.etag || JSON.stringify(body.scopeRevisions) !== JSON.stringify(current.scopeRevisions)) return fail(412, 'precondition_failed', 'The saved DHCP setup changed; reload its saved values');
      if (current.managedBy === 'file' && !body.adopt) return fail(409, 'managed_by', 'Adopt the file setup first');
      if (current.environmentLocked) return fail(409, 'env_locked', 'Remove DHCP environment overrides');
      if (body.enabled && !body.scopes.length) return fail(400, 'invalid', 'Enabled DHCP needs a scope');
      state.setup = { ...current, enabled: body.enabled, scopes: body.scopes, managedBy: 'api', restartRequired: true,
        revision: String(Number(current.revision) + 1), etag: `"setup-${Number(current.revision) + 1}"`,
        pendingScopes: body.scopes.filter((s) => current.pendingScopes.includes(s.name) || JSON.stringify(s) !== JSON.stringify(current.scopes.find((x) => x.name === s.name))).map((s) => s.name) };
      return HttpResponse.json(state.setup);
    }),
    http.get(`${base}/status`, ({ request }) => guard(request, 'dhcp', 'read') ?? HttpResponse.json({ node: state.config.node, enabled: state.enabled, available: state.enabled,
      scopes: [{ name: state.config.scope, management: state.config.management, revision: state.config.revision, ...state.config.topology }], took_ms: 1 })),
    http.get(`${path}/config`, ({ request, params }) => check(request, params.scope, 'dhcp', 'read') ?? HttpResponse.json(state.config, { headers: { ETag: state.config.etag } })),
    http.put(`${path}/config`, async ({ request, params }) => {
      const error = check(request, params.scope, 'dhcp', 'write') ?? change(request);
      if (error) return error;
      state.config.options = await request.json() as DHCPPoolOptions;
      revise(); return HttpResponse.json(state.config);
    }),
    http.get(`${path}/leases`, ({ request, params }) => {
      const error = check(request, params.scope, 'dhcpleases', 'read');
      if (error) return error;
      const u = new URL(request.url);
      const search = u.searchParams.get('q')?.toLowerCase() ?? '';
      const wanted = u.searchParams.get('state');
      const items = state.leases.filter((l) => (!wanted || wanted === l.state) && [l.address, l.mac, l.hostname].some((v) => v.toLowerCase().includes(search)));
      return HttpResponse.json({ node: state.config.node, scope: state.config.scope, items, took_ms: 1 });
    }),
    http.put(`${path}/reservations/:mac`, async ({ request, params }) => {
      const error = check(request, params.scope, 'dhcp', 'write') ?? change(request);
      if (error) return error;
      const body = await request.json() as Omit<DHCPReservation, 'mac'>;
      const mac = String(params.mac).toLowerCase();
      if (state.leases.some((l) => l.address === body.ip && l.mac !== mac)) return fail(409, 'conflict', 'The address is held by another client');
      state.config.reservations = [...state.config.reservations.filter((r) => r.mac !== mac), { mac, ...body }];
      revise(); return HttpResponse.json(state.config);
    }),
    http.delete(`${path}/reservations/:mac`, ({ request, params }) => {
      const error = check(request, params.scope, 'dhcp', 'delete') ?? change(request);
      if (error) return error;
      state.config.reservations = state.config.reservations.filter((r) => r.mac !== String(params.mac));
      revise(); return HttpResponse.json(state.config);
    }),
    http.get(`${path}/export`, ({ request, params }) => check(request, params.scope, 'dhcpleases', 'export') ?? HttpResponse.json({ node: state.config.node, scope: state.config.scope,
      document: { version: 1, scope: state.config.scope, subnet: state.config.topology.subnet, exportedAt: new Date().toISOString(),
        leases: state.leases.map(({ clientID: _id, ...l }) => ({ ...l, identity: { kind: 'mac', value: l.mac } })) }, took_ms: 1 })),
    http.post(`${path}/import`, async ({ request, params }) => {
      const error = check(request, params.scope, 'dhcpleases', 'import');
      if (error) return error;
      const body = await request.json() as { format: string; data: string };
      if (!body.data.trim()) return fail(400, 'invalid', 'Lease data is required');
      return HttpResponse.json({ node: state.config.node, scope: state.config.scope, imported: 1, unchanged: 0, skippedExpired: 0, took_ms: 1 });
    }),
  ];
}
