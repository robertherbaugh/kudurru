// A local external-dns webhook peer for the integrations spec: a plain
// HTTP server on 127.0.0.1 (the webhook provider accepts http only on a
// loopback address) speaking the external-dns webhook protocol that a
// records-only webhook Integration publishes to (D380): GET / answers the
// domain filter, GET /records the stored rows, POST /adjustendpoints echoes,
// POST /records applies a plan. It keeps every row in memory, including
// the kudurru registry TXT rows, and requires the bearer token the node
// presents from its credential file.
import http from 'node:http';

const MEDIA = 'application/external.dns.webhook+json;version=1';

export interface PeerEndpoint {
  dnsName: string;
  targets?: string[];
  recordType: string;
  setIdentifier?: string;
  recordTTL?: number;
  labels?: Record<string, string>;
}

interface Changes {
  create?: PeerEndpoint[];
  updateOld?: PeerEndpoint[];
  updateNew?: PeerEndpoint[];
  delete?: PeerEndpoint[];
  Create?: PeerEndpoint[];
  UpdateOld?: PeerEndpoint[];
  UpdateNew?: PeerEndpoint[];
  Delete?: PeerEndpoint[];
}

export interface ExternalDNSPeer {
  /** http://127.0.0.1:<port> */
  url: string;
  /** name/type -> targets of every row. */
  rows: () => Record<string, string[]>;
  /** Plans applied by POST /records. */
  posts: () => number;
  /** Removes every row behind the instance's back. */
  wipe: () => void;
  /** While set, POST /records answers 503 and changes nothing (holds a Delete cleanup). */
  refuseWrites: (on: boolean) => void;
  close: () => Promise<void>;
}

const key = (e: PeerEndpoint) => `${e.dnsName}/${e.recordType}/${e.setIdentifier ?? ''}`;

export async function startExternalDNSPeer(token: string, domains: string[]): Promise<ExternalDNSPeer> {
  const rows = new Map<string, PeerEndpoint>();
  let posts = 0;
  let refusing = false;
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
    const chunks: Buffer[] = [];
    req.on('data', (c: Buffer) => chunks.push(c));
    req.on('end', () => {
      const text = Buffer.concat(chunks).toString('utf8');
      if (req.method === 'GET' && path === '/') return reply(200, { include: domains });
      if (req.method === 'GET' && path === '/records') return reply(200, [...rows.values()]);
      if (req.method === 'POST' && path === '/adjustendpoints') return reply(200, text ? (JSON.parse(text) as unknown) : []);
      if (req.method === 'POST' && path === '/records') {
        if (refusing) return reply(503);
        let ch: Changes;
        try {
          ch = JSON.parse(text) as Changes;
        } catch {
          return reply(400);
        }
        posts++;
        for (const e of [...(ch.delete ?? []), ...(ch.Delete ?? []), ...(ch.updateOld ?? []), ...(ch.UpdateOld ?? [])]) rows.delete(key(e));
        for (const e of [...(ch.create ?? []), ...(ch.Create ?? []), ...(ch.updateNew ?? []), ...(ch.UpdateNew ?? [])]) rows.set(key(e), e);
        res.writeHead(204);
        res.end();
        return;
      }
      reply(404);
    });
  });
  await new Promise<void>((resolve, reject) => {
    srv.once('error', reject);
    srv.listen(0, '127.0.0.1', () => resolve());
  });
  const addr = srv.address();
  if (!addr || typeof addr === 'string') throw new Error('external-dns peer has no TCP address');
  return {
    url: `http://127.0.0.1:${addr.port}`,
    rows: () => Object.fromEntries([...rows.values()].map((e) => [`${e.dnsName}/${e.recordType}`, [...(e.targets ?? [])]])),
    posts: () => posts,
    wipe: () => rows.clear(),
    refuseWrites: (on: boolean) => {
      refusing = on;
    },
    close: () => new Promise<void>((resolve) => srv.close(() => resolve())),
  };
}
