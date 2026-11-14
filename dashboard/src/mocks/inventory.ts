// GET /inventory of the mock server (internal/api/inventory.go, D397): the
// UniFi Integration "office" reports three devices (one never queried, two
// colliding on one address, one stale), and the DHCP scope "lan" of node
// kudurru-0 holds two leases, one superseded by the UniFi MAC. Filters,
// pages and the two 503 answers behave as the server's.
import { http, HttpResponse, type JsonBodyType } from 'msw';
import type { InventoryBody, InventoryDevice } from '../api/types';

/** The inventory source of the mock Integration default/office (GET /integrations inventorySource). */
export const OFFICE_SOURCE = 'integration-0f1c2d3e4b5a69788796a5b4c3d2e1f00f1c2d3e4b5a69788796a5b4c3d2e1f0';

export interface InventoryMock {
  /** 503: "the client inventory is not mounted on this server". */
  unmounted: boolean;
  /** 503: "the serving inventory is unavailable". */
  unavailable: boolean;
  node: string;
  /** nodeLocalLeases: the answering node is one member of a SQLite cluster (the mock cluster is). */
  nodeLocalLeases: boolean;
  devices: InventoryDevice[];
  /** Query strings of every GET, newest last. */
  requests: URLSearchParams[];
}

function fixture(now = Date.now()): InventoryMock {
  const at = (ms: number) => new Date(now + ms).toISOString();
  return {
    unmounted: false,
    unavailable: false,
    node: 'kudurru-0',
    nodeLocalLeases: true,
    requests: [],
    devices: [
      { source: 'dhcp/lan', kind: 'dhcp', id: '192.168.1.50', hostname: 'printer', macs: ['aa:bb:cc:00:00:50'], tags: [], addresses: [{ ip: '192.168.1.50', state: 'assigned' }], expiresAt: at(3_600_000), stale: false },
      {
        source: 'dhcp/lan', kind: 'dhcp', id: '192.168.1.30', hostname: 'kids-tablet', macs: ['aa:bb:cc:00:00:30'], tags: [],
        addresses: [{ ip: '192.168.1.30', state: 'superseded', ownerSource: OFFICE_SOURCE, ownerId: 'default/aa:bb:cc:dd:ee:30' }], expiresAt: at(3_600_000), stale: false,
      },
      {
        source: OFFICE_SOURCE, kind: 'inventory', id: 'default/aa:bb:cc:dd:ee:30', name: 'kids-tablet', macs: ['aa:bb:cc:dd:ee:30'], tags: ['kids'],
        addresses: [{ ip: '192.168.1.30', state: 'assigned' }], observedAt: at(-60_000), expiresAt: at(600_000), stale: false,
      },
      {
        source: OFFICE_SOURCE, kind: 'inventory', id: 'default/aa:bb:cc:dd:ee:60', name: 'thermostat', macs: ['aa:bb:cc:dd:ee:60'], tags: [],
        addresses: [{ ip: '192.168.1.60', state: 'ambiguous' }], observedAt: at(-60_000), expiresAt: at(600_000), stale: false,
      },
      {
        source: OFFICE_SOURCE, kind: 'inventory', id: 'default/aa:bb:cc:dd:ee:61', name: 'doorbell', macs: ['aa:bb:cc:dd:ee:61'], tags: [],
        addresses: [{ ip: '192.168.1.60', state: 'ambiguous' }], observedAt: at(-60_000), expiresAt: at(600_000), stale: false,
      },
      {
        source: OFFICE_SOURCE, kind: 'inventory', id: 'default/aa:bb:cc:dd:ee:70', name: 'old-laptop', macs: ['aa:bb:cc:dd:ee:70'], tags: [],
        addresses: [{ ip: '192.168.1.70', state: 'expired' }], observedAt: at(-90_000_000), expiresAt: at(-3_600_000), stale: true,
      },
    ],
  };
}

let state = fixture();
export const mockInventory = () => state;
export const resetInventory = () => {
  state = fixture();
};

type Guard = (r: Request, resource: string, verb: string) => Response | null;

const fail = (status: number, key: string, message: string) => HttpResponse.json({ error: { key, message }, took_ms: 1 } as JsonBodyType, { status });

export function inventoryHandlers(guard: Guard) {
  return [
    http.get('*/api/v1/inventory', ({ request }) => {
      const g = guard(request, 'inventory', 'read');
      if (g) return g;
      const p = new URL(request.url).searchParams;
      state.requests.push(p);
      if (state.unmounted) return fail(503, 'unavailable', 'the client inventory is not mounted on this server');
      if (state.unavailable) return fail(503, 'unavailable', 'the serving inventory is unavailable');
      const source = p.get('source') ?? '';
      const kind = p.get('kind') ?? '';
      const stale = p.get('stale') ?? '';
      const q = (p.get('q') ?? '').trim().toLowerCase();
      const limit = Number(p.get('limit') ?? 500) || 500;
      const after = p.get('continue') ?? '';
      const sorted = [...state.devices].sort((a, b) => a.source.localeCompare(b.source) || a.id.localeCompare(b.id));
      const match = sorted.filter((d) => {
        if (source && d.source !== source) return false;
        if (kind && d.kind !== kind) return false;
        if ((stale === 'true' && !d.stale) || (stale === 'false' && d.stale)) return false;
        if (!q) return true;
        return [d.id, d.name ?? '', d.hostname ?? '', ...d.macs, ...d.addresses.map((a) => a.ip)].some((f) => f.toLowerCase().includes(q));
      });
      const start = after ? match.findIndex((d) => `${d.source}\u0000${d.id}` === after) + 1 : 0;
      const items = match.slice(start, start + limit);
      const last = items.at(-1);
      const more = start + limit < match.length && last;
      const body: InventoryBody = { node: state.node, nodeLocalLeases: state.nodeLocalLeases, generation: 7, readAt: new Date().toISOString(), items, took_ms: 0.4, ...(more ? { continue: `${last.source}\u0000${last.id}` } : {}) };
      return HttpResponse.json(body);
    }),
  ];
}
