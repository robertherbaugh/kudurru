// Client addresses (design spec §3 TRUSTED_PROXIES, §6 rate limits): strict
// address and CIDR parsing (IPv4, IPv6, IPv4-mapped), canonical text, the
// rate-limit block (IPv4 address, IPv6 /64), and the client IP: the peer
// unless it is a trusted proxy, then the rightmost X-Forwarded-For entry
// that is not one, never a client-chosen entry to its left.
import { describe, expect, test } from 'bun:test';
import { cidrContains, clientIpResolver, formatCidr, ipBucket, normalizeIp, parseCidr, parseIp } from '../http/ip';
import { MemoryLogger } from '../log';

describe('addresses', () => {
  test.each([
    ['192.0.2.1', '192.0.2.1'],
    ['0.0.0.0', '0.0.0.0'],
    ['255.255.255.255', '255.255.255.255'],
    ['2001:db8::1', '2001:db8::1'],
    ['2001:DB8:0:0:0:0:0:1', '2001:db8::1'],
    ['2001:db8:0:0:1:0:0:1', '2001:db8::1:0:0:1'],
    ['::', '::'],
    ['::1', '::1'],
    ['fe80::', 'fe80::'],
    ['1:0:2:3:4:5:6:7', '1:0:2:3:4:5:6:7'],
    ['::ffff:192.0.2.1', '192.0.2.1'],
    ['::ffff:c000:201', '192.0.2.1'],
    ['64:ff9b::192.0.2.1', '64:ff9b::c000:201'],
    [' 192.0.2.1 ', '192.0.2.1'],
  ])('%j → %j', (input, want) => expect(normalizeIp(input)).toBe(want));

  test.each(['', '1.2.3', '1.2.3.4.5', '01.2.3.4', '256.1.1.1', '1.2.3.-1', 'a.b.c.d', '1:2:3:4:5:6:7:8:9', '1::2::3', ':1', '1:', 'fe80::1%eth0', '12345::', 'g::1', '1.2.3.4::', 'localhost', '[::1]'])(
    'refuses %j',
    (input) => expect(parseIp(input)).toBeNull(),
  );

  test('CIDRs', () => {
    expect(formatCidr(parseCidr('10.1.2.3/8'))).toBe('10.0.0.0/8');
    expect(formatCidr(parseCidr('192.0.2.7'))).toBe('192.0.2.7/32');
    expect(formatCidr(parseCidr('2001:db8:aaaa::1/32'))).toBe('2001:db8::/32');
    expect(formatCidr(parseCidr('::1'))).toBe('::1/128');
    expect(formatCidr(parseCidr('::ffff:10.0.0.0/104'))).toBe('10.0.0.0/8');
    expect(formatCidr(parseCidr('0.0.0.0/0'))).toBe('0.0.0.0/0');
    for (const bad of ['10.0.0.0/33', '10.0.0.0/', '10.0.0.0/08', '2001:db8::/129', 'nope/8', '10.0.0.0/8/8', '*', '10.0.0.0/-1']) expect(() => parseCidr(bad)).toThrow(RangeError);

    const net = parseCidr('10.0.0.0/8');
    expect(cidrContains(net, parseIp('10.255.0.1')!)).toBe(true);
    expect(cidrContains(net, parseIp('11.0.0.1')!)).toBe(false);
    expect(cidrContains(net, parseIp('::ffff:10.0.0.1')!)).toBe(true);
    expect(cidrContains(net, parseIp('::a00:1')!)).toBe(false);
    expect(cidrContains(parseCidr('172.16.0.0/12'), parseIp('172.31.255.255')!)).toBe(true);
    expect(cidrContains(parseCidr('172.16.0.0/12'), parseIp('172.32.0.0')!)).toBe(false);
    expect(cidrContains(parseCidr('fd00::/8'), parseIp('fd12:3456::1')!)).toBe(true);
    expect(cidrContains(parseCidr('fd00::/8'), parseIp('fe00::1')!)).toBe(false);
    expect(cidrContains(parseCidr('0.0.0.0/0'), parseIp('203.0.113.9')!)).toBe(true);
    expect(cidrContains(parseCidr('::/0'), parseIp('203.0.113.9')!)).toBe(false);
  });

  test('rate-limit blocks: IPv4 per address, IPv6 per /64', () => {
    expect(ipBucket('192.0.2.7')).toBe('192.0.2.7/32');
    expect(ipBucket('::ffff:192.0.2.7')).toBe('192.0.2.7/32');
    expect(ipBucket('2001:db8:1:2:3:4:5:6')).toBe('2001:db8:1:2::/64');
    expect(ipBucket('2001:db8:1:2:ffff::1')).toBe('2001:db8:1:2::/64');
    expect(ipBucket('2001:db8:1:3::1')).toBe('2001:db8:1:3::/64');
    expect(ipBucket(null)).toBeNull();
    expect(ipBucket('nonsense')).toBeNull();
  });
});

describe('clientIp', () => {
  const req = (xff?: string | string[]) => {
    const h = new Headers();
    for (const v of Array.isArray(xff) ? xff : xff === undefined ? [] : [xff]) h.append('X-Forwarded-For', v);
    return new Request('http://localhost/', { headers: h });
  };

  test('no trusted proxies: the peer, whatever the header says (warned at most once a minute)', () => {
    const log = new MemoryLogger();
    let now = 0;
    const ip = clientIpResolver({ trustedProxies: [] }, { log, now: () => now });
    expect(ip(req(), '203.0.113.5')).toBe('203.0.113.5');
    expect(ip(req('198.51.100.1'), '203.0.113.5')).toBe('203.0.113.5');
    expect(ip(req('198.51.100.2'), '203.0.113.5')).toBe('203.0.113.5');
    now = 61_000;
    expect(ip(req('198.51.100.3'), '::ffff:203.0.113.5')).toBe('203.0.113.5');
    expect(log.find('X-Forwarded-For from a peer that is not in TRUSTED_PROXIES; using the peer address')).toHaveLength(2);
    expect(ip(req(), null)).toBeNull();
  });

  test('a trusted peer: the rightmost entry that is not a trusted proxy', () => {
    const ip = clientIpResolver({ trustedProxies: ['10.0.0.0/8', 'fd00::/8'] });
    const proxy = '10.0.0.2';
    expect(ip(req('203.0.113.9'), proxy)).toBe('203.0.113.9');
    // A spoofed entry on the left is ignored: the proxy appended the real client.
    expect(ip(req('1.1.1.1, 203.0.113.9'), proxy)).toBe('203.0.113.9');
    // Several proxies in a row.
    expect(ip(req('1.1.1.1, 203.0.113.9, 10.0.0.7, fd00::5'), proxy)).toBe('203.0.113.9');
    // Several header lines count as one list.
    expect(ip(req(['1.1.1.1', '203.0.113.9, 10.0.0.7']), proxy)).toBe('203.0.113.9');
    // All trusted: the leftmost.
    expect(ip(req('10.9.9.9, 10.0.0.7'), proxy)).toBe('10.9.9.9');
    // No header: the peer.
    expect(ip(req(), proxy)).toBe(proxy);
    // Ports and brackets some proxies add.
    expect(ip(req('203.0.113.9:4711'), proxy)).toBe('203.0.113.9');
    expect(ip(req('[2001:db8::7]:443'), proxy)).toBe('2001:db8::7');
    expect(ip(req('2001:DB8:0::7'), proxy)).toBe('2001:db8::7');
    // Garbage where the client should be: the peer (one shared block), never the garbage.
    expect(ip(req('1.1.1.1, unknown'), proxy)).toBe(proxy);
    expect(ip(req('1.1.1.1, , 203.0.113.9'), proxy)).toBe('203.0.113.9');
    // An untrusted peer's header is not believed.
    expect(ip(req('203.0.113.9'), '198.51.100.1')).toBe('198.51.100.1');
  });

  test('a bad TRUSTED_PROXIES entry throws (config.ts refuses to start)', () => {
    expect(() => clientIpResolver({ trustedProxies: ['10.0.0.0/8', 'proxy.internal'] })).toThrow(RangeError);
  });
});
