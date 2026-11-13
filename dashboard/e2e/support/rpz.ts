// A stub RPZ primary for the end-to-end run: DNS over TCP on 127.0.0.1 that
// answers SOA, AXFR and IXFR (as a full transfer) for one policy zone, so a
// BlockList RPZ source transfers without leaving the host. `fail()` makes
// every later query answer SERVFAIL, which lets the node's copy expire on
// the zone's own SOA timers (refresh and retry 1 s, expire `expire` s).
import net from 'node:net';

const TYPE = { NS: 2, CNAME: 5, SOA: 6, IXFR: 251, AXFR: 252 } as const;

function encodeName(name: string): Buffer {
  const parts = name.replace(/\.$/, '').split('.').filter(Boolean);
  return Buffer.concat([...parts.map((p) => Buffer.concat([Buffer.from([p.length]), Buffer.from(p, 'ascii')])), Buffer.from([0])]);
}

function rr(name: string, type: number, rdata: Buffer, ttl = 30): Buffer {
  const fixed = Buffer.alloc(10);
  fixed.writeUInt16BE(type, 0);
  fixed.writeUInt16BE(1, 2); // IN
  fixed.writeUInt32BE(ttl, 4);
  fixed.writeUInt16BE(rdata.length, 8);
  return Buffer.concat([encodeName(name), fixed, rdata]);
}

function soa(zone: string, serial: number, expire: number): Buffer {
  const timers = Buffer.alloc(20);
  timers.writeUInt32BE(serial, 0);
  timers.writeUInt32BE(1, 4); // refresh
  timers.writeUInt32BE(1, 8); // retry
  timers.writeUInt32BE(expire, 12);
  timers.writeUInt32BE(5, 16); // minimum
  return rr(zone, TYPE.SOA, Buffer.concat([encodeName(`ns.${zone}`), encodeName(`hostmaster.${zone}`), timers]));
}

/** The question of a query: name (lower case, absolute), type and the offset after it. */
function question(msg: Buffer): { name: string; qtype: number; end: number } | null {
  if (msg.length < 12 || msg.readUInt16BE(4) !== 1) return null;
  const labels: string[] = [];
  let off = 12;
  for (;;) {
    const len = msg[off];
    if (len === undefined || len > 63) return null;
    off++;
    if (len === 0) break;
    labels.push(msg.subarray(off, off + len).toString('ascii').toLowerCase());
    off += len;
  }
  if (off + 4 > msg.length) return null;
  return { name: `${labels.join('.')}.`, qtype: msg.readUInt16BE(off), end: off + 4 };
}

export interface RPZPrimary {
  /** 127.0.0.1:<port>, as a BlockList rpz primary. */
  address: string;
  /** Transfers served (AXFR or IXFR). */
  transfers: () => number;
  /** From now on every query answers SERVFAIL. */
  fail: () => void;
  close: () => Promise<void>;
}

/**
 * Serves zone with serial: one QNAME rule per name in `blocked` (CNAME .,
 * NXDOMAIN) and one NSDNAME trigger, which kudurru ignores and counts as
 * resolver-ns-provenance.
 */
export async function startRPZPrimary(zone: string, opts: { serial: number; expire: number; blocked: string[] }): Promise<RPZPrimary> {
  const origin = zone.toLowerCase().replace(/\.?$/, '.');
  let failing = false;
  let transfers = 0;
  const records = [
    rr(origin, TYPE.NS, encodeName(`ns.${origin}`)),
    ...opts.blocked.map((name) => rr(`${name.replace(/\.$/, '')}.${origin}`, TYPE.CNAME, encodeName('.'))),
    rr(`ns.example.rpz-nsdname.${origin}`, TYPE.CNAME, encodeName('.')),
  ];
  const answer = (msg: Buffer): Buffer => {
    const q = question(msg);
    const header = Buffer.alloc(12);
    header.writeUInt16BE(msg.length >= 2 ? msg.readUInt16BE(0) : 0, 0);
    let rcode = 0;
    let answers: Buffer[] = [];
    if (!q || failing || q.name !== origin) rcode = 2;
    else if (q.qtype === TYPE.SOA) answers = [soa(origin, opts.serial, opts.expire)];
    else if (q.qtype === TYPE.AXFR || q.qtype === TYPE.IXFR) {
      transfers++;
      answers = [soa(origin, opts.serial, opts.expire), ...records, soa(origin, opts.serial, opts.expire)];
    } else rcode = 5;
    header.writeUInt16BE(0x8400 | rcode, 2); // QR, AA
    header.writeUInt16BE(q ? 1 : 0, 4);
    header.writeUInt16BE(answers.length, 6);
    return Buffer.concat([header, q ? msg.subarray(12, q.end) : Buffer.alloc(0), ...answers]);
  };
  const sockets = new Set<net.Socket>();
  const srv = net.createServer((sock) => {
    sockets.add(sock);
    sock.on('close', () => sockets.delete(sock));
    sock.on('error', () => sock.destroy());
    let buf = Buffer.alloc(0);
    sock.on('data', (chunk) => {
      buf = Buffer.concat([buf, chunk]);
      while (buf.length >= 2) {
        const len = buf.readUInt16BE(0);
        if (buf.length < 2 + len) break;
        const out = answer(buf.subarray(2, 2 + len));
        buf = buf.subarray(2 + len);
        const prefix = Buffer.alloc(2);
        prefix.writeUInt16BE(out.length, 0);
        sock.write(Buffer.concat([prefix, out]));
      }
    });
  });
  await new Promise<void>((resolve, reject) => {
    srv.once('error', reject);
    srv.listen(0, '127.0.0.1', () => resolve());
  });
  const a = srv.address();
  const port = typeof a === 'object' && a ? a.port : 0;
  return {
    address: `127.0.0.1:${port}`,
    transfers: () => transfers,
    fail: () => {
      failing = true;
    },
    close: () =>
      new Promise<void>((resolve) => {
        for (const s of sockets) s.destroy();
        srv.close(() => resolve());
      }),
  };
}
