// Client addresses (design spec §3 TRUSTED_PROXIES, §13 CLIENT_IP_HEADER,
// §6 rate limits). With CLIENT_IP_HEADER the address is that header's,
// set by the only proxy in front of the server (Cloudflare Containers). The
// socket peer is the client unless it is a trusted proxy; then the client
// is the rightmost X-Forwarded-For entry that is not itself a trusted
// proxy (entries to its left are whatever the client chose to send, so
// they are never believed). X-Forwarded-Proto is ignored: PUBLIC_URL
// decides the scheme. Rate limits count IPv4 addresses one by one and IPv6
// addresses by their /64, the smallest block a home or cloud customer gets.
//
// The parsers are strict (no leading zeros in IPv4, no zone ids) so a
// forwarded header cannot smuggle odd spellings past the trusted list, and
// every address this module returns is in canonical form (dotted IPv4,
// RFC 5952 IPv6), valid as a Postgres inet value.

import type { Config } from '../config';
import type { Logger } from '../log';

/** An address as bytes: 4 for IPv4, 16 for IPv6. IPv4-mapped IPv6 (::ffff:a.b.c.d) is parsed as IPv4. */
export interface IpAddress {
  readonly version: 4 | 6;
  readonly bytes: Uint8Array;
}

/** A network: an address with its prefix length (host bits cleared). */
export interface Cidr {
  readonly version: 4 | 6;
  readonly bytes: Uint8Array;
  readonly prefix: number;
}

function parseIpv4(s: string): Uint8Array | null {
  const parts = s.split('.');
  if (parts.length !== 4) return null;
  const out = new Uint8Array(4);
  for (let i = 0; i < 4; i++) {
    const p = parts[i]!;
    if (!/^(0|[1-9]\d{0,2})$/.test(p)) return null;
    const n = Number(p);
    if (n > 255) return null;
    out[i] = n;
  }
  return out;
}

function parseIpv6(s: string): Uint8Array | null {
  if (s.length < 2 || s.length > 45 || s.includes('%')) return null;
  let head = s;
  let tail = '';
  const gap = s.indexOf('::');
  if (gap >= 0) {
    if (s.indexOf('::', gap + 1) >= 0) return null;
    head = s.slice(0, gap);
    tail = s.slice(gap + 2);
  }
  const words: number[] = [];
  const parseGroups = (part: string, allowV4: boolean): number[] | null => {
    if (part === '') return [];
    const groups = part.split(':');
    const out: number[] = [];
    for (let i = 0; i < groups.length; i++) {
      const g = groups[i]!;
      if (allowV4 && i === groups.length - 1 && g.includes('.')) {
        const v4 = parseIpv4(g);
        if (!v4) return null;
        out.push((v4[0]! << 8) | v4[1]!, (v4[2]! << 8) | v4[3]!);
        continue;
      }
      if (!/^[0-9a-fA-F]{1,4}$/.test(g)) return null;
      out.push(parseInt(g, 16));
    }
    return out;
  };
  const a = parseGroups(head, gap < 0);
  const b = gap >= 0 ? parseGroups(tail, true) : [];
  if (!a || !b) return null;
  if (gap >= 0) {
    if (a.length + b.length > 7) return null;
    words.push(...a, ...new Array<number>(8 - a.length - b.length).fill(0), ...b);
  } else {
    if (a.length !== 8) return null;
    words.push(...a);
  }
  const out = new Uint8Array(16);
  for (let i = 0; i < 8; i++) {
    out[i * 2] = words[i]! >> 8;
    out[i * 2 + 1] = words[i]! & 0xff;
  }
  return out;
}

function isV4Mapped(b: Uint8Array): boolean {
  for (let i = 0; i < 10; i++) if (b[i] !== 0) return false;
  return b[10] === 0xff && b[11] === 0xff;
}

/** Parses an IP address literal (no brackets, port or zone); IPv4-mapped IPv6 becomes IPv4. Null when it is not one. */
export function parseIp(s: string): IpAddress | null {
  const text = s.trim();
  if (text.includes(':')) {
    const b = parseIpv6(text);
    if (!b) return null;
    if (isV4Mapped(b)) return { version: 4, bytes: b.slice(12) };
    return { version: 6, bytes: b };
  }
  const b = parseIpv4(text);
  return b ? { version: 4, bytes: b } : null;
}

/** The canonical text of an address: dotted IPv4, or RFC 5952 IPv6 (lower case, the longest run of zero groups as ::). */
export function formatIp(ip: IpAddress): string {
  const b = ip.bytes;
  if (ip.version === 4) return `${b[0]}.${b[1]}.${b[2]}.${b[3]}`;
  const words: number[] = [];
  for (let i = 0; i < 8; i++) words.push((b[i * 2]! << 8) | b[i * 2 + 1]!);
  let bestStart = -1;
  let bestLen = 0;
  for (let i = 0; i < 8; ) {
    if (words[i] !== 0) {
      i++;
      continue;
    }
    let j = i;
    while (j < 8 && words[j] === 0) j++;
    if (j - i > bestLen) {
      bestStart = i;
      bestLen = j - i;
    }
    i = j;
  }
  const hex = (ws: number[]) => ws.map((w) => w.toString(16)).join(':');
  if (bestLen < 2) return hex(words);
  return `${hex(words.slice(0, bestStart))}::${hex(words.slice(bestStart + bestLen))}`;
}

/** The canonical text of an address literal, or null when `s` is not one. */
export function normalizeIp(s: string): string | null {
  const ip = parseIp(s);
  return ip ? formatIp(ip) : null;
}

function maskBytes(bytes: Uint8Array, prefix: number): Uint8Array {
  const out = bytes.slice();
  for (let i = 0; i < out.length; i++) {
    const bits = Math.max(0, Math.min(8, prefix - i * 8));
    out[i] = out[i]! & (bits === 0 ? 0 : (0xff << (8 - bits)) & 0xff);
  }
  return out;
}

/**
 * Parses a CIDR ('10.0.0.0/8', '2001:db8::/32') or a single address (a
 * /32 or /128). Host bits are cleared, as Go's net.ParseCIDR does. Throws a
 * RangeError naming the value on anything else.
 */
export function parseCidr(s: string): Cidr {
  const text = s.trim();
  const slash = text.indexOf('/');
  const addr = parseIp(slash < 0 ? text : text.slice(0, slash));
  if (!addr) throw new RangeError(`not an IP address or CIDR: ${JSON.stringify(s)}`);
  const max = addr.version === 4 ? 32 : 128;
  let prefix = max;
  if (slash >= 0) {
    const p = text.slice(slash + 1);
    // An IPv4-mapped IPv6 network ('::ffff:10.0.0.0/104') is the IPv4 network /8.
    const mapped = addr.version === 4 && text.slice(0, slash).includes(':');
    if (!/^(0|[1-9]\d{0,2})$/.test(p)) throw new RangeError(`not a CIDR prefix length: ${JSON.stringify(s)}`);
    prefix = Number(p) - (mapped ? 96 : 0);
    if (prefix < 0 || prefix > max) throw new RangeError(`the prefix length of ${JSON.stringify(s)} is out of range`);
  }
  return { version: addr.version, prefix, bytes: maskBytes(addr.bytes, prefix) };
}

/** Whether the network contains the address (an IPv4 network never contains an IPv6 address and vice versa). */
export function cidrContains(net: Cidr, ip: IpAddress): boolean {
  if (net.version !== ip.version) return false;
  const masked = maskBytes(ip.bytes, net.prefix);
  for (let i = 0; i < masked.length; i++) if (masked[i] !== net.bytes[i]) return false;
  return true;
}

/** The text form of a network ('10.0.0.0/8', '2001:db8::/32'). */
export function formatCidr(net: Cidr): string {
  return `${formatIp({ version: net.version, bytes: net.bytes })}/${net.prefix}`;
}

/** TRUSTED_PROXIES parsed; throws on the first invalid entry (config.ts refuses to start). */
export function parseTrustedProxies(entries: readonly string[]): Cidr[] {
  return entries.map(parseCidr);
}

/**
 * The block a rate limit counts an address in, as a CIDR Postgres reads
 * as inet ('192.0.2.7/32', '2001:db8:1:2::/64'): IPv4 addresses one by
 * one, IPv6 by the /64. Null for null or a non-address. Use it as
 * `ip <<= ${ipBucket(ip)}::inet` to count rows of the same block.
 */
export function ipBucket(ip: string | null): string | null {
  if (ip === null) return null;
  const parsed = parseIp(ip);
  if (!parsed) return null;
  return formatCidr({ version: parsed.version, prefix: parsed.version === 4 ? 32 : 64, bytes: maskBytes(parsed.bytes, parsed.version === 4 ? 32 : 64) });
}

/** One X-Forwarded-For entry without the port some proxies add ('192.0.2.1:4711', '[2001:db8::1]:443'). */
function forwardedEntry(raw: string): IpAddress | null {
  let s = raw.trim();
  if (s.startsWith('[')) {
    const end = s.indexOf(']');
    if (end < 0) return null;
    s = s.slice(1, end);
  } else if (/^\d{1,3}(\.\d{1,3}){3}:\d{1,5}$/.test(s)) {
    s = s.slice(0, s.lastIndexOf(':'));
  }
  return parseIp(s);
}

export interface ClientIpOptions {
  /** Told (at most once a minute) when X-Forwarded-For arrives from a peer that is not a trusted proxy. */
  log?: Logger;
  /** The clock of the warning's throttle; default Date.now. */
  now?: () => number;
}

/** How often the "forwarded header from an untrusted peer" warning may repeat. */
const UNTRUSTED_FORWARD_WARN_MS = 60_000;

/**
 * The Pipeline.clientIp of the portal: the peer, unless the peer is in
 * TRUSTED_PROXIES; then the rightmost X-Forwarded-For entry that is not a
 * trusted proxy (all of them trusted: the leftmost). An entry that is not
 * an address ends the walk with the peer, so a proxy that writes garbage
 * gets one shared bucket rather than a client-chosen one. Every answer is
 * canonical (normalizeIp).
 */
export function clientIpResolver(config: Pick<Config, 'trustedProxies'> & Partial<Pick<Config, 'clientIpHeader'>>, opts: ClientIpOptions = {}): (req: Request, peer: string | null) => string | null {
  const trusted = parseTrustedProxies(config.trustedProxies);
  const header = config.clientIpHeader ?? null;
  if (header !== null) {
    // CLIENT_IP_HEADER: the one proxy in front of the server (the Cloudflare
    // Worker) sets it on every request, overwriting what a client sent.
    // Without a valid address in it the peer stands, as without a proxy.
    return (req, peer) => {
      const ip = forwardedEntry(req.headers.get(header) ?? '');
      if (ip) return formatIp(ip);
      const peerIp = peer === null ? null : parseIp(peer);
      return peerIp ? formatIp(peerIp) : peer;
    };
  }
  const now = opts.now ?? Date.now;
  let lastWarn = -Infinity;
  const isTrusted = (ip: IpAddress) => trusted.some((net) => cidrContains(net, ip));

  return (req, peer) => {
    const peerIp = peer === null ? null : parseIp(peer);
    const peerText = peerIp ? formatIp(peerIp) : peer;
    const header = req.headers.get('x-forwarded-for');
    if (!peerIp || !isTrusted(peerIp)) {
      if (header !== null && opts.log && now() - lastWarn >= UNTRUSTED_FORWARD_WARN_MS) {
        lastWarn = now();
        opts.log.warn('X-Forwarded-For from a peer that is not in TRUSTED_PROXIES; using the peer address', { peer: peerText });
      }
      return peerText;
    }
    if (header === null) return peerText;
    const entries = header.split(',');
    let leftmost: IpAddress | null = null;
    for (let i = entries.length - 1; i >= 0; i--) {
      const entry = entries[i]!;
      if (entry.trim() === '') continue;
      const ip = forwardedEntry(entry);
      if (!ip) return peerText;
      if (!isTrusted(ip)) return formatIp(ip);
      leftmost = ip;
    }
    return leftmost ? formatIp(leftmost) : peerText;
  };
}
