// A local kudurru webhook inventory peer for the inventory spec: plain HTTP
// on 127.0.0.1 (the webhook provider accepts http only on a loopback
// address) speaking the kudurru integration protocol with the "clients"
// capability (ee/integration/webhook, D380): GET / answers the declaration,
// GET /clients and GET /networks the complete arrays the node polls. Every
// request must carry the bearer token the node reads from its credential
// file; the token is never echoed.
import http from 'node:http';

const MEDIA = 'application/kudurru.integration+json;version=1';

export interface PeerClient {
  id: string;
  name: string;
  hostname: string;
  ips: string[];
  macs: string[];
  tags: string[];
}

export interface InventoryPeer {
  url: string;
  /** GET /clients served so far. */
  polls: () => number;
  close: () => Promise<void>;
}

export const PEER_NETWORK = { name: 'lan', vlan: 10, cidr: '192.0.2.0/24', domain: 'example.test' };

export async function startInventoryPeer(token: string, clients: PeerClient[]): Promise<InventoryPeer> {
  let polls = 0;
  const srv = http.createServer((req, res) => {
    const reply = (status: number, body?: unknown) => {
      res.writeHead(status, { 'Content-Type': MEDIA, 'Cache-Control': 'no-store' });
      res.end(body === undefined ? undefined : JSON.stringify(body));
    };
    if (req.headers.authorization !== `Bearer ${token}`) {
      res.writeHead(401, { 'WWW-Authenticate': 'Bearer' });
      res.end();
      return;
    }
    const path = (req.url ?? '/').split('?')[0];
    if (req.method !== 'GET') return reply(405);
    if (path === '/') {
      return reply(200, {
        filters: [],
        capabilities: ['clients'],
        manifest: { type: 'e2e-inventory', displayName: 'E2E inventory', version: '1', ioClass: 'local_polling', capabilities: ['ClientSource'], configSchema: { type: 'object' } },
      });
    }
    if (path === '/clients') {
      polls++;
      // lastSeen is the remote observation time; the node keeps it for at most three minutes.
      const lastSeen = new Date(Math.floor(Date.now() / 1000) * 1000).toISOString();
      return reply(200, clients.map((c) => ({ ...c, network: PEER_NETWORK, fixedIP: '', lastSeen, attributes: {} })));
    }
    if (path === '/networks') return reply(200, [PEER_NETWORK]);
    reply(404);
  });
  await new Promise<void>((resolve, reject) => {
    srv.once('error', reject);
    srv.listen(0, '127.0.0.1', () => resolve());
  });
  const addr = srv.address();
  if (!addr || typeof addr === 'string') throw new Error('inventory peer has no TCP address');
  return {
    url: `http://127.0.0.1:${addr.port}`,
    polls: () => polls,
    close: () => new Promise<void>((resolve) => srv.close(() => resolve())),
  };
}
