import { describe, expect, it } from 'vitest';
import { compose, dnsNameToOwner, hostError, isIPv4, isIPv6, ownerError, ownerToDNSName, parse, RECORD_TYPES, tokenize, validate } from './rr';

describe('address checks', () => {
  it.each([
    ['192.0.2.1', true],
    ['255.255.255.255', true],
    ['256.0.0.1', false],
    ['1.2.3', false],
    ['01.2.3.4', false],
  ])('isIPv4(%s) = %s', (s, ok) => expect(isIPv4(s)).toBe(ok));

  it.each([
    ['2001:db8::1', true],
    ['::', true],
    ['::ffff:192.0.2.1', true],
    ['2001:db8:0:0:0:0:0:1', true],
    ['2001:db8::1::2', false],
    ['2001:db8:0:0:0:0:0:0:1', false],
    ['fe80::1%eth0', false],
    ['192.0.2.1', false],
  ])('isIPv6(%s) = %s', (s, ok) => expect(isIPv6(s)).toBe(ok));
});

describe('hostError', () => {
  it.each([
    ['example.com', null],
    ['example.com.', null],
    ['_sip._udp.example.com.', null],
    ['', 'Required'],
    ['a..b', 'Empty label (two dots in a row)'],
    [`${'a'.repeat(64)}.com`, /longer than 63/],
    ['bad_char!.com', /characters other than/],
    ['*.example.com', 'A wildcard is only allowed as the first label'],
  ])('%s → %s', (name, want) => {
    const got = hostError(name);
    if (want instanceof RegExp) expect(got).toMatch(want);
    else expect(got).toBe(want);
  });

  it('allows a leading wildcard only when asked', () => {
    expect(hostError('*.example.com', { wildcard: true })).toBeNull();
    expect(hostError('a.*.example.com', { wildcard: true })).not.toBeNull();
  });
});

describe('validate / compose / parse per type', () => {
  const cases: { type: string; values: Record<string, string>; target: string; recordType?: string }[] = [
    { type: 'A', values: { address: '192.0.2.10' }, target: '192.0.2.10' },
    { type: 'AAAA', values: { address: '2001:db8::10' }, target: '2001:db8::10' },
    { type: 'CNAME', values: { target: 'www.example.com' }, target: 'www.example.com.' },
    { type: 'MX', values: { preference: '10', exchange: 'mail.example.com' }, target: '10 mail.example.com.' },
    { type: 'SRV', values: { priority: '10', weight: '5', port: '443', target: 'svc.example.com.' }, target: '10 5 443 svc.example.com.' },
    { type: 'TXT', values: { text: 'v=spf1 -all' }, target: 'v=spf1 -all' },
    { type: 'CAA', values: { flags: '0', tag: 'issue', value: 'letsencrypt.org' }, target: '0 issue "letsencrypt.org"' },
    { type: 'HTTPS', values: { priority: '1', target: '.', params: 'alpn=h2,h3' }, target: '1 . alpn=h2,h3' },
    { type: 'SVCB', values: { priority: '1', target: 'svc.example.com', params: '' }, target: '1 svc.example.com.' },
    { type: 'NS', values: { host: 'ns1.example.com' }, target: 'ns1.example.com.' },
    { type: 'PTR', values: { target: 'host.example.com.' }, target: 'host.example.com.' },
    { type: 'TLSA', values: { usage: '3', selector: '1', matching: '1', data: 'AB CD ef' }, target: '3 1 1 abcdef' },
    { type: 'SSHFP', values: { algorithm: '4', fptype: '2', fingerprint: 'DEADBEEF' }, target: '4 2 deadbeef' },
    {
      type: 'NAPTR',
      values: { order: '100', preference: '10', flags: 'S', service: 'SIP+D2U', regexp: '', replacement: '_sip._udp.example.com' },
      target: '100 10 "S" "SIP+D2U" "" _sip._udp.example.com.',
    },
    { type: 'raw', values: { rrtype: 'dname', rdata: 'target.example.com.' }, target: 'target.example.com.', recordType: 'DNAME' },
  ];

  it('covers every record type of the editor', () => {
    expect(cases.map((c) => c.type).sort()).toEqual(RECORD_TYPES.map((t) => t.type).sort());
  });

  it.each(cases)('$type is valid and composes to "$target"', ({ type, values, target, recordType }) => {
    expect(validate(type, type === 'raw' ? { ...values, rrtype: values.rrtype?.toUpperCase() ?? '' } : values)).toEqual({});
    expect(compose(type, values)).toEqual({ recordType: recordType ?? type, target });
  });

  it.each(cases.filter((c) => c.type !== 'raw'))('$type round-trips through parse', ({ type, values, target }) => {
    const back = parse(type, target);
    expect(back.type).toBe(type);
    expect(compose(type, back.values).target).toBe(target);
    void values;
  });

  it.each([
    ['A', { address: '300.1.1.1' }, { address: 'Not an IPv4 address' }],
    ['AAAA', { address: '192.0.2.1' }, { address: 'Not an IPv6 address' }],
    ['MX', { preference: '70000', exchange: 'mail.example.com' }, { preference: 'Must be between 0 and 65535' }],
    ['MX', { preference: '10' }, { exchange: 'Required' }],
    ['CAA', { flags: '0', tag: 'nope', value: 'x' }, { tag: 'One of issue, issuewild, iodef, issuemail, issuevmc' }],
    ['TLSA', { usage: '4', selector: '1', matching: '1', data: 'abc' }, { usage: 'Must be between 0 and 3', data: 'Must be an even number of hex digits' }],
    ['SSHFP', { algorithm: '0', fptype: '2', fingerprint: 'ab' }, { algorithm: 'Must be between 1 and 6' }],
    ['HTTPS', { priority: '0', target: 'svc.example.com', params: 'alpn=h2' }, { params: 'Alias mode (priority 0) takes no parameters' }],
    ['TXT', { text: 'two\nlines' }, { text: 'Must be a single line' }],
    ['raw', { rrtype: 'not a type', rdata: 'x' }, { rrtype: 'A type mnemonic (DNAME) or TYPEnnn' }],
  ])('%s %j reports %j', (type, values, errors) => {
    expect(validate(type, values)).toEqual(errors);
  });

  it('falls back to raw when a target does not parse', () => {
    expect(parse('MX', 'garbage')).toEqual({ type: 'raw', values: { rrtype: 'MX', rdata: 'garbage' } });
    expect(parse('LOC', '52 22 23.000 N 4 53 32.000 E -2.00m').type).toBe('raw');
  });

  it('tokenizes quoted strings with escapes', () => {
    expect(tokenize('0 issue "a \\"b\\" c"')).toEqual(['0', 'issue', 'a "b" c']);
  });
});

describe('owner names', () => {
  it.each([
    ['@', 'example.internal', 'example.internal'],
    ['', 'example.internal.', 'example.internal'],
    ['www', 'example.internal.', 'www.example.internal'],
    ['host.example.internal.', 'example.internal', 'host.example.internal'],
  ])('ownerToDNSName(%s, %s) = %s', (owner, zone, want) => expect(ownerToDNSName(owner, zone)).toBe(want));

  it.each([
    ['example.internal', 'example.internal.', '@'],
    ['www.example.internal', 'example.internal', 'www'],
    ['other.example.com', 'example.internal', 'other.example.com.'],
  ])('dnsNameToOwner(%s, %s) = %s', (name, zone, want) => expect(dnsNameToOwner(name, zone)).toBe(want));

  it('rejects absolute names outside the zone', () => {
    expect(ownerError('www.other.com.', 'example.internal')).toBe('Not inside example.internal.');
    expect(ownerError('*.lan', 'example.internal')).toBeNull();
  });
});
