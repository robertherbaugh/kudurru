// Just enough DNS over UDP for the end-to-end run: a client that sends one
// question and decodes the answer's A records, and a stub upstream that
// answers every A question with UPSTREAM_A so recursive names resolve
// without leaving the host.
import dgram from 'node:dgram';

export const UPSTREAM_A = '192.0.2.53';

export const TYPE_A = 1;
export const TYPE_AAAA = 28;

export interface DnsAnswer {
  rcode: number;
  /** The A records of the answer section, as dotted quads. */
  a: string[];
  ancount: number;
}

function encodeName(name: string): Buffer {
  const parts = name.replace(/\.$/, '').split('.').filter(Boolean);
  const out: Buffer[] = parts.map((p) => Buffer.concat([Buffer.from([p.length]), Buffer.from(p, 'ascii')]));
  out.push(Buffer.from([0]));
  return Buffer.concat(out);
}

/** The query packet for one question with RD set. */
export function encodeQuery(id: number, name: string, qtype: number): Buffer {
  const header = Buffer.alloc(12);
  header.writeUInt16BE(id, 0);
  header.writeUInt16BE(0x0100, 2); // RD
  header.writeUInt16BE(1, 4); // QDCOUNT
  const tail = Buffer.alloc(4);
  tail.writeUInt16BE(qtype, 0);
  tail.writeUInt16BE(1, 2); // IN
  return Buffer.concat([header, encodeName(name), tail]);
}

/** Skips a (possibly compressed) name and returns the offset after it. */
function skipName(b: Buffer, off: number): number {
  for (;;) {
    const len = b[off];
    if (len === undefined) throw new Error('dns: truncated name');
    if (len === 0) return off + 1;
    if ((len & 0xc0) === 0xc0) return off + 2;
    off += len + 1;
  }
}

export function decodeAnswer(b: Buffer): DnsAnswer {
  const rcode = b.readUInt16BE(2) & 0x0f;
  const qd = b.readUInt16BE(4);
  const an = b.readUInt16BE(6);
  let off = 12;
  for (let i = 0; i < qd; i++) off = skipName(b, off) + 4;
  const a: string[] = [];
  for (let i = 0; i < an; i++) {
    off = skipName(b, off);
    const type = b.readUInt16BE(off);
    const rdlen = b.readUInt16BE(off + 8);
    off += 10;
    if (type === TYPE_A && rdlen === 4) a.push([...b.subarray(off, off + 4)].join('.'));
    off += rdlen;
  }
  return { rcode, a, ancount: an };
}

/** Sends one question to addr ("host:port") over UDP. */
export function query(addr: string, name: string, qtype = TYPE_A, timeoutMs = 3000): Promise<DnsAnswer> {
  const [host, port] = splitHostPort(addr);
  const id = Math.floor(Math.random() * 0xffff);
  return new Promise((resolve, reject) => {
    const sock = dgram.createSocket('udp4');
    const timer = setTimeout(() => {
      sock.close();
      reject(new Error(`dns: no answer for ${name} from ${addr} within ${timeoutMs} ms`));
    }, timeoutMs);
    sock.on('message', (msg) => {
      if (msg.length < 12 || msg.readUInt16BE(0) !== id) return;
      clearTimeout(timer);
      sock.close();
      try {
        resolve(decodeAnswer(msg));
      } catch (err) {
        reject(err instanceof Error ? err : new Error(String(err)));
      }
    });
    sock.on('error', (err) => {
      clearTimeout(timer);
      sock.close();
      reject(err);
    });
    sock.send(encodeQuery(id, name, qtype), port, host);
  });
}

export function splitHostPort(addr: string): [string, number] {
  const i = addr.lastIndexOf(':');
  return [addr.slice(0, i), Number(addr.slice(i + 1))];
}

/**
 * A stub upstream resolver on 127.0.0.1: every A question gets one
 * UPSTREAM_A record (TTL 60), every other type an empty NOERROR answer.
 */
export async function startUpstream(): Promise<{ address: string; close: () => Promise<void> }> {
  const sock = dgram.createSocket('udp4');
  sock.on('message', (msg, rinfo) => {
    if (msg.length < 12) return;
    let qend: number;
    try {
      qend = skipName(msg, 12) + 4;
    } catch {
      return;
    }
    const question = msg.subarray(12, qend);
    const qtype = question.readUInt16BE(question.length - 4);
    const header = Buffer.alloc(12);
    header.writeUInt16BE(msg.readUInt16BE(0), 0);
    header.writeUInt16BE(0x8180 | (msg.readUInt16BE(2) & 0x0100), 2); // QR, RA, RD copied
    header.writeUInt16BE(1, 4);
    const parts = [header, question];
    if (qtype === TYPE_A) {
      header.writeUInt16BE(1, 6);
      const rr = Buffer.alloc(16);
      rr.writeUInt16BE(0xc00c, 0); // pointer to the question name
      rr.writeUInt16BE(TYPE_A, 2);
      rr.writeUInt16BE(1, 4);
      rr.writeUInt32BE(60, 6);
      rr.writeUInt16BE(4, 10);
      UPSTREAM_A.split('.').forEach((o, i) => rr.writeUInt8(Number(o), 12 + i));
      parts.push(rr);
    }
    sock.send(Buffer.concat(parts), rinfo.port, rinfo.address);
  });
  await new Promise<void>((resolve, reject) => {
    sock.once('error', reject);
    sock.bind(0, '127.0.0.1', () => resolve());
  });
  const { port } = sock.address();
  return {
    address: `127.0.0.1:${port}`,
    close: () => new Promise((resolve) => sock.close(() => resolve())),
  };
}
