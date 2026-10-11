// Realistic fixtures for MSW (development with `bun run dev:mock` and the
// tests): 3 zones with 50 records, 6 block/allow lists including the
// starter list, 4 client groups, 3 policies, 24 h of time series, 500
// query-log rows and 2 nodes. Everything is generated from a fixed seed
// relative to a fixed clock (`now`), so tests see the same data each run.
import type {
  AnyObject,
  AuditEntry,
  BlockList,
  ClientGroup,
  DNSRecordSet,
  DNSZone,
  DiagnosisMessage,
  Endpoint,
  NodeInfo,
  Policy,
  QueryRow,
  SettingKey,
  TimeseriesPoint,
} from '../api/types';
import schemaJSON from './data/settings-schema.json';
import { pick, pickSkewed, rng } from './rand';

const NS = 'default';
const API = 'kudurru.io/v1alpha1' as const;
const MANAGED = 'kudurru.io/managed-by';

export interface MockUser {
  username: string;
  password: string;
  name: string;
  roles: string[];
}

export interface MockDB {
  now: number;
  zones: DNSZone[];
  recordSets: DNSRecordSet[];
  lists: BlockList[];
  groups: ClientGroup[];
  policies: Policy[];
  queries: QueryRow[];
  series: TimeseriesPoint[];
  nodes: NodeInfo[];
  messages: DiagnosisMessage[];
  audit: AuditEntry[];
  settings: Record<string, unknown>;
  nodeConfig: Record<string, unknown>;
  schema: SettingKey[];
  blocking: { blocking: boolean; until?: number };
  tokens: { id: string; name: string; createdAt: string; expiresAt?: string; lastUsedAt?: string }[];
  users: MockUser[];
  session: { user: MockUser } | null;
  nextQueryID: number;
  rv: number;
}

export const CLIENTS: { ip: string; name: string; group: string }[] = [
  { ip: '192.168.1.10', name: 'robert-laptop', group: 'default' },
  { ip: '192.168.1.11', name: 'robert-phone', group: 'default' },
  { ip: '192.168.1.20', name: 'living-room-tv', group: 'iot' },
  { ip: '192.168.1.21', name: 'thermostat', group: 'iot' },
  { ip: '192.168.1.22', name: 'doorbell', group: 'iot' },
  { ip: '192.168.1.30', name: 'kids-tablet', group: 'kids' },
  { ip: '192.168.1.31', name: 'kids-laptop', group: 'kids' },
  { ip: '192.168.1.40', name: 'nas', group: 'servers' },
  { ip: '192.168.1.41', name: 'homeassistant', group: 'servers' },
  { ip: '192.168.1.50', name: 'guest-phone', group: 'default' },
  { ip: '10.0.0.5', name: 'k8s-node-a', group: 'servers' },
  { ip: '2001:db8::1:10', name: 'robert-laptop-v6', group: 'default' },
];

const PERMITTED_DOMAINS = [
  'www.google.com', 'connectivitycheck.gstatic.com', 'api.github.com', 'github.com', 'objects.githubusercontent.com',
  'time.apple.com', 'gateway.icloud.com', 'www.wikipedia.org', 'en.wikipedia.org', 'proxy.golang.org',
  'registry.npmjs.org', 'www.youtube.com', 'i.ytimg.com', 'netflix.com', 'nflxvideo.net', 'pool.ntp.org',
  'discord.com', 'cdn.discordapp.com', 'slack.com', 'zoom.us', 'outlook.office365.com', 'login.microsoftonline.com',
  'api.spotify.com', 'open.spotify.com', 'news.ycombinator.com', 'www.reddit.com', 'cloudflare-dns.com',
  'updates.home-assistant.io', 'firmware.ring.com', 'ota.nest.com', 'store.steampowered.com', 'raw.githubusercontent.com',
];
const BLOCKED_DOMAINS: { domain: string; list: string; rule: string; status: string }[] = [
  { domain: 'doubleclick.net', list: 'default/stevenblack', rule: '0.0.0.0 doubleclick.net', status: 'blocked_list' },
  { domain: 'ads.google.com', list: 'default/starter', rule: 'ads.google.com', status: 'blocked_list' },
  { domain: 'googleadservices.com', list: 'default/stevenblack', rule: '0.0.0.0 googleadservices.com', status: 'blocked_list' },
  { domain: 'app-measurement.com', list: 'default/oisd-small', rule: '||app-measurement.com^', status: 'blocked_list' },
  { domain: 'graph.facebook.com', list: 'default/oisd-small', rule: '||graph.facebook.com^', status: 'blocked_list' },
  { domain: 'telemetry.tv-vendor.example', list: 'default/regex-trackers', rule: '/^telemetry\\./', status: 'blocked_regex' },
  { domain: 'metrics.roku.com', list: 'default/regex-trackers', rule: '/^metrics\\./', status: 'blocked_regex' },
  { domain: 'tracking.example-shop.com', list: 'default/ui-deny', rule: 'tracking.example-shop.com', status: 'blocked_exact' },
  { domain: 'cdn.tracker-cname.example', list: 'default/stevenblack', rule: '0.0.0.0 tracker-cname.example', status: 'blocked_cname' },
  { domain: 'stats.g.doubleclick.net', list: 'default/stevenblack', rule: '0.0.0.0 stats.g.doubleclick.net', status: 'blocked_list' },
  { domain: 'tiktokv.com', list: 'default/starter', rule: 'tiktokv.com', status: 'blocked_list' },
];
const LOCAL_NAMES = ['nas.home.arpa', 'homeassistant.home.arpa', 'printer.home.arpa', 'grafana.example.internal', 'git.example.internal'];
const UPSTREAMS = ['tls://1.1.1.1:853', 'tls://9.9.9.9:853', 'https://dns.google/dns-query'];
const QTYPES = ['A', 'A', 'A', 'AAAA', 'AAAA', 'HTTPS', 'PTR', 'TXT', 'SRV', 'MX'];
const PROTOS = ['udp', 'udp', 'udp', 'tcp', 'dot', 'doh'];

/** The fixed "now" every fixture is relative to: 2026-10-10 18:00 UTC. */
export const FIXED_NOW = Date.UTC(2026, 9, 10, 18, 0, 0);

function meta(name: string, managedBy: string, extra: Partial<AnyObject['metadata']> = {}, namespaced = true) {
  return {
    name,
    ...(namespaced ? { namespace: NS } : {}),
    uid: `uid-${name}`,
    resourceVersion: '1',
    generation: 1,
    creationTimestamp: '2026-09-01T12:00:00Z',
    annotations: { [MANAGED]: managedBy, ...(extra.annotations ?? {}) },
    ...(extra.labels ? { labels: extra.labels } : {}),
  };
}

function ready(reason = 'AsExpected', message = '') {
  return [{ type: 'Ready', status: 'True' as const, reason, message, lastTransitionTime: '2026-10-10T06:00:00Z' }];
}

function buildZones(): DNSZone[] {
  return [
    {
      apiVersion: API,
      kind: 'DNSZone',
      metadata: meta('root', 'bootstrap', {}, false),
      spec: { zoneName: '.', type: 'forward', forward: { upstreamGroup: 'default' } },
      status: { serial: 0, recordCount: 0, conditions: ready() },
    },
    {
      apiVersion: API,
      kind: 'DNSZone',
      metadata: meta('home-arpa', 'file:objects.d/zones.yaml', {}, false),
      spec: {
        zoneName: 'home.arpa.',
        type: 'primary',
        soa: { mname: 'ns1.home.arpa.', rname: 'hostmaster.home.arpa.', refresh: 3600, retry: 600, expire: 604800, minimum: 300 },
        nameservers: ['ns1.home.arpa.'],
        defaultTTL: 300,
        policy: { zoneTransfer: 'useNetworkACL', transferACL: ['192.168.1.0/24'], notify: 'specified', notifyTargets: ['192.168.1.2:53'] },
      },
      status: { serial: 2026101007, recordCount: 0, contentHash: 'sha256:7f1c', conditions: ready() },
    },
    {
      apiVersion: API,
      kind: 'DNSZone',
      metadata: meta('example-internal', 'api', {}, false),
      spec: {
        zoneName: 'example.internal.',
        type: 'primary',
        nameservers: ['ns1.example.internal.', 'ns2.example.internal.'],
        defaultTTL: 600,
        policy: { zoneTransfer: 'deny', notify: 'zoneNameServers' },
      },
      status: { serial: 2026100912, recordCount: 0, contentHash: 'sha256:a03e', conditions: ready() },
    },
  ];
}

function ep(dnsName: string, recordType: string, targets: string[], recordTTL?: number, extra: Partial<Endpoint> = {}): Endpoint {
  return { dnsName, recordType, targets, ...(recordTTL ? { recordTTL } : {}), ...extra };
}

function buildRecordSets(): DNSRecordSet[] {
  const home: Endpoint[] = [
    ep('home.arpa', 'NS', ['ns1.home.arpa.']),
    ep('ns1.home.arpa', 'A', ['192.168.1.2']),
    ep('router.home.arpa', 'A', ['192.168.1.1']),
    ep('nas.home.arpa', 'A', ['192.168.1.40'], 3600, { kudurru: { comment: 'Synology, rack shelf 2' } }),
    ep('nas.home.arpa', 'AAAA', ['2001:db8::40']),
    ep('homeassistant.home.arpa', 'A', ['192.168.1.41']),
    ep('ha.home.arpa', 'CNAME', ['homeassistant.home.arpa.']),
    ep('printer.home.arpa', 'A', ['192.168.1.60']),
    ep('tv.home.arpa', 'A', ['192.168.1.20']),
    ep('thermostat.home.arpa', 'A', ['192.168.1.21']),
    ep('doorbell.home.arpa', 'A', ['192.168.1.22'], 300, { kudurru: { disabled: true, comment: 'replaced, keep for a week' } }),
    ep('laptop.home.arpa', 'A', ['192.168.1.10']),
    ep('phone.home.arpa', 'A', ['192.168.1.11']),
    ep('_ipp._tcp.home.arpa', 'SRV', ['0 0 631 printer.home.arpa.']),
    ep('home.arpa', 'TXT', ['v=spf1 -all']),
    ep('10.1.168.192.in-addr.arpa', 'PTR', ['laptop.home.arpa.']),
    ep('k8s.home.arpa', 'A', ['10.0.0.5', '10.0.0.6', '10.0.0.7']),
    ep('*.apps.home.arpa', 'A', ['10.0.0.100']),
    ep('grafana.home.arpa', 'CNAME', ['k8s.home.arpa.']),
    ep('mqtt.home.arpa', 'A', ['192.168.1.41']),
  ];
  const internal: Endpoint[] = [
    ep('example.internal', 'NS', ['ns1.example.internal.', 'ns2.example.internal.']),
    ep('ns1.example.internal', 'A', ['10.10.0.2']),
    ep('ns2.example.internal', 'A', ['10.10.0.3']),
    ep('example.internal', 'MX', ['10 mail.example.internal.', '20 mail2.example.internal.']),
    ep('mail.example.internal', 'A', ['10.10.0.25']),
    ep('mail2.example.internal', 'A', ['10.10.0.26']),
    ep('example.internal', 'TXT', ['v=spf1 mx -all']),
    ep('_dmarc.example.internal', 'TXT', ['v=DMARC1; p=reject; rua=mailto:dmarc@example.internal']),
    ep('example.internal', 'CAA', ['0 issue "letsencrypt.org"']),
    ep('www.example.internal', 'A', ['10.10.0.80']),
    ep('www.example.internal', 'AAAA', ['fd00:10::80']),
    ep('www.example.internal', 'HTTPS', ['1 . alpn=h2,h3']),
    ep('git.example.internal', 'A', ['10.10.0.81']),
    ep('grafana.example.internal', 'CNAME', ['www.example.internal.']),
    ep('_sip._udp.example.internal', 'SRV', ['10 60 5060 sip.example.internal.']),
    ep('sip.example.internal', 'A', ['10.10.0.90']),
    ep('example.internal', 'NAPTR', ['100 10 "S" "SIP+D2U" "" _sip._udp.example.internal.']),
    ep('_443._tcp.www.example.internal', 'TLSA', ['3 1 1 0c72ac70b745ac19998811b131d662c9ac69dbdbe7cb23e5b514b56664c5d3d6']),
    ep('bastion.example.internal', 'SSHFP', ['4 2 123456789abcdef67890123456789abcdef67890123456789abcdef123456789']),
    ep('_dns.example.internal', 'SVCB', ['1 ns1.example.internal. alpn=dot port=853']),
    ep('api.example.internal', 'A', ['10.10.0.100'], 60, { setIdentifier: 'blue' }),
    ep('api.example.internal', 'A', ['10.10.0.101'], 60, { setIdentifier: 'green' }),
    ep('vpn.example.internal', 'A', ['10.10.0.5']),
    ep('ldap.example.internal', 'A', ['10.10.0.10']),
    ep('old.example.internal', 'TYPE65280', ['\\# 4 0a0a0001'], 300, { kudurru: { comment: 'private RR type, raw rdata' } }),
    ep('metrics.example.internal', 'CNAME', ['grafana.example.internal.']),
    ep('ci.example.internal', 'A', ['10.10.0.82']),
    ep('ext.example.internal', 'A', ['10.10.0.200'], 120),
    ep('svc.example.internal', 'A', ['10.10.0.201'], 120),
    ep('cache.example.internal', 'A', ['10.10.0.202'], 120),
  ];
  // 20 + 30 = 50 endpoints across five record sets with three managers.
  return [
    { apiVersion: API, kind: 'DNSRecordSet', metadata: meta('home-core', 'file:objects.d/zones.yaml'), spec: { zoneRef: { name: 'home-arpa' }, endpoints: home.slice(0, 14) } },
    { apiVersion: API, kind: 'DNSRecordSet', metadata: meta('api-home-arpa', 'api'), spec: { zoneRef: { name: 'home-arpa' }, endpoints: home.slice(14) } },
    { apiVersion: API, kind: 'DNSRecordSet', metadata: meta('api-example-internal', 'api'), spec: { zoneRef: { name: 'example-internal' }, endpoints: internal.slice(0, 27) } },
    {
      apiVersion: API,
      kind: 'DNSRecordSet',
      metadata: meta('external-dns-ingress', 'integration:external-dns'),
      spec: { zoneRef: { name: 'example-internal' }, endpoints: internal.slice(27) },
    },
  ];
}

function buildLists(now: number): BlockList[] {
  const iso = (msAgo: number) => new Date(now - msAgo).toISOString();
  return [
    {
      apiVersion: API,
      kind: 'BlockList',
      metadata: meta('starter', 'bootstrap', { annotations: { 'kudurru.io/comment': 'Embedded starter list (Steven Black, MIT), 2,852 names' } }),
      spec: { mode: 'block', sources: [{ name: 'embedded', inline: ['ads.google.com', 'tiktokv.com', '…'], format: 'domains' }] },
      status: { entries: 2852, ruleCount: 2852, invalid: 0, lastFetched: iso(3_600_000 * 20), sources: [{ name: 'embedded', entries: 2852, digest: 'sha256:9a1b', lastFetched: iso(3_600_000 * 20) }], conditions: ready() },
    },
    {
      apiVersion: API,
      kind: 'BlockList',
      metadata: meta('stevenblack', 'api', { annotations: { 'kudurru.io/comment': 'Unified hosts' } }),
      spec: { mode: 'block', refreshInterval: '24h', sources: [{ name: 'unified', url: 'https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts', format: 'hosts' }] },
      status: {
        entries: 81234,
        ruleCount: 81234,
        invalid: 3,
        lastFetched: iso(3_600_000 * 4),
        sources: [{ name: 'unified', entries: 81234, invalid: 3, etag: '"5f2c"', digest: 'sha256:51c0', lastFetched: iso(3_600_000 * 4) }],
        conditions: ready(),
      },
    },
    {
      apiVersion: API,
      kind: 'BlockList',
      metadata: meta('oisd-small', 'kubernetes'),
      spec: { mode: 'block', refreshInterval: '12h', sources: [{ name: 'oisd', url: 'https://small.oisd.nl/', format: 'adblock' }] },
      status: {
        entries: 52011,
        ruleCount: 52011,
        invalid: 0,
        lastFetched: iso(3_600_000 * 30),
        sources: [{ name: 'oisd', entries: 52011, digest: 'sha256:0d9e', lastFetched: iso(3_600_000 * 30), error: 'GET https://small.oisd.nl/: context deadline exceeded (serving the cached body)' }],
        conditions: [{ type: 'Ready', status: 'True', reason: 'Degraded', message: 'serving the cached body; last fetch failed', lastTransitionTime: iso(3_600_000 * 6) }],
      },
    },
    {
      apiVersion: API,
      kind: 'BlockList',
      metadata: meta('regex-trackers', 'file:objects.d/lists.yaml'),
      spec: { mode: 'block', sources: [{ name: 'patterns', inline: ['/^telemetry\\./', '/^metrics\\./', '/^(ads?|track)[0-9]*\\./'], format: 'regex' }] },
      status: { entries: 3, ruleCount: 3, invalid: 0, sources: [{ name: 'patterns', entries: 3 }], conditions: ready() },
    },
    {
      apiVersion: API,
      kind: 'BlockList',
      metadata: meta('ui-allow', 'api', { annotations: { 'kudurru.io/comment': 'Allowed from the query log' } }),
      spec: { mode: 'allow', sources: [{ name: 'inline', inline: ['s.youtube.com', 'clickserve.dartsearch.net'], format: 'domains' }] },
      status: { entries: 2, ruleCount: 2, sources: [{ name: 'inline', entries: 2 }], conditions: ready() },
    },
    {
      apiVersion: API,
      kind: 'BlockList',
      metadata: meta('ui-deny', 'api', { annotations: { 'kudurru.io/enabled': 'true' } }),
      spec: { mode: 'block', sources: [{ name: 'inline', inline: ['tracking.example-shop.com'], format: 'domains' }] },
      status: {
        entries: 1,
        ruleCount: 1,
        sources: [{ name: 'inline', entries: 1 }],
        conditions: [{ type: 'Ready', status: 'False', reason: 'Invalid', message: 'line 2: "bad..domain" is not a domain name', lastTransitionTime: iso(600_000) }],
        invalid: 1,
      },
    },
  ];
}

function buildGroups(): ClientGroup[] {
  return [
    { apiVersion: API, kind: 'ClientGroup', metadata: meta('default', 'bootstrap', { annotations: { 'kudurru.io/comment': 'Every client' } }), spec: { clients: [{ cidr: '0.0.0.0/0' }, { cidr: '::/0' }] }, status: { members: 12, conditions: ready() } },
    {
      apiVersion: API,
      kind: 'ClientGroup',
      metadata: meta('kids', 'api'),
      spec: { clients: [{ ip: '192.168.1.30', name: 'kids-tablet' }, { ip: '192.168.1.31', name: 'kids-laptop', comment: 'school laptop' }, { mac: 'aa:bb:cc:dd:ee:01', name: 'switch' }] },
      status: { members: 2, conditions: ready() },
    },
    {
      apiVersion: API,
      kind: 'ClientGroup',
      metadata: meta('iot', 'file:objects.d/groups.yaml'),
      spec: { clients: [{ cidr: '192.168.1.20/30' }, { nameGlob: '*.iot.home.arpa' }] },
      status: { members: 3, conditions: ready() },
    },
    {
      apiVersion: API,
      kind: 'ClientGroup',
      metadata: meta('servers', 'api', { annotations: { 'kudurru.io/enabled': 'false', 'kudurru.io/comment': 'paused during the NAS migration' } }),
      spec: { clients: [{ ip: '192.168.1.40' }, { ip: '192.168.1.41' }, { cidr: '10.0.0.0/24' }] },
      status: { members: 3, conditions: ready() },
    },
  ];
}

function buildPolicies(): Policy[] {
  return [
    {
      apiVersion: API,
      kind: 'Policy',
      metadata: meta('default', 'bootstrap'),
      spec: { clientGroupRefs: ['default'], blockListRefs: ['starter', 'stevenblack', 'oisd-small', 'regex-trackers', 'ui-deny'], allowListRefs: ['ui-allow'], blockMode: 'zeroIP' },
      status: { conditions: ready() },
    },
    {
      apiVersion: API,
      kind: 'Policy',
      metadata: meta('kids-bedtime', 'api', { annotations: { 'kudurru.io/comment': 'Social media off on school nights' } }),
      spec: {
        clientGroupRefs: ['kids'],
        blockListRefs: ['starter', 'stevenblack', 'regex-trackers'],
        schedule: { weekdays: ['Sun', 'Mon', 'Tue', 'Wed', 'Thu'], start: '21:00', end: '07:00', tz: 'America/Chicago' },
        safeSearch: true,
        priority: 20,
        blockMode: 'nxdomain',
      },
      status: { conditions: ready() },
    },
    {
      apiVersion: API,
      kind: 'Policy',
      metadata: meta('iot-strict', 'file:objects.d/policies.yaml'),
      spec: { clientGroupRefs: ['iot'], blockListRefs: ['stevenblack', 'regex-trackers'], allowListRefs: ['ui-allow'], priority: 10, queryLog: false, upstreamGroup: 'filtered' },
      status: { conditions: [{ type: 'Ready', status: 'True', reason: 'AsExpected', message: 'tie with default at priority 10 resolved by name', lastTransitionTime: '2026-10-10T06:00:00Z' }] },
    },
  ];
}

/** 24 h at a 5 minute step with a diurnal curve. */
function buildSeries(now: number, r: () => number): TimeseriesPoint[] {
  const step = 300_000;
  const end = Math.floor(now / step) * step;
  const out: TimeseriesPoint[] = [];
  for (let t = end - 288 * step + step; t <= end; t += step) {
    const hour = new Date(t).getUTCHours() - 5; // US Central-ish household
    const diurnal = 0.35 + 0.65 * Math.max(0, Math.sin(((hour + 24) % 24 - 6) / 18 * Math.PI));
    const total = Math.round((900 + r() * 300) * diurnal + 60);
    const blocked = Math.round(total * (0.12 + r() * 0.08));
    const cached = Math.round((total - blocked) * (0.45 + r() * 0.1));
    const authoritative = Math.round((total - blocked) * 0.06);
    const other = Math.round(total * 0.005);
    const forwarded = Math.max(0, total - blocked - cached - authoritative - other);
    out.push({ ts: new Date(t).toISOString(), total, blocked, cached, forwarded, authoritative, other });
  }
  return out;
}


export function makeQueryRow(id: number, ts: number, r: () => number, node = 'kudurru-0'): QueryRow {
  const client = pickSkewed(r, CLIENTS);
  const roll = r();
  const qtype = pick(r, QTYPES);
  const base = {
    id,
    ts: new Date(ts).toISOString(),
    node,
    proto: pick(r, PROTOS),
    client: { ip: client.ip, name: client.name, group: client.group },
    qtype,
    qclass: 1,
    dnssec: 'insecure',
    flags: { RD: true, DO: r() < 0.3, TC: false, AD: false, CD: false, Cached: false, Stale: false, Prefetch: false, ECS: false, Traceparent: false },
  };
  if (roll < 0.17) {
    const b = pickSkewed(r, BLOCKED_DOMAINS);
    return {
      ...base,
      domain: b.domain,
      status: b.status,
      status_class: 'blocked',
      rcode: 'NOERROR',
      reply: { type: 'ip', time_ms: 0.08 + r() * 0.2, answer: qtype === 'AAAA' ? '::' : '0.0.0.0' },
      list: b.list,
      rule: b.rule,
      ...(b.status === 'blocked_cname' ? { cname: 'tracker-cname.example' } : {}),
    };
  }
  if (roll < 0.22) {
    const name = pick(r, LOCAL_NAMES);
    return {
      ...base,
      domain: name,
      status: 'authoritative',
      status_class: 'permitted',
      rcode: 'NOERROR',
      reply: { type: 'ip', time_ms: 0.05 + r() * 0.1, answer: '192.168.1.40' },
      zone: name.endsWith('home.arpa') ? 'home.arpa.' : 'example.internal.',
      dnssec: 'unknown',
    };
  }
  const domain = pickSkewed(r, PERMITTED_DOMAINS);
  if (roll < 0.62) {
    const stale = r() < 0.04;
    return {
      ...base,
      domain,
      status: stale ? 'cached_stale' : 'cached',
      status_class: 'cached',
      rcode: 'NOERROR',
      reply: { type: 'ip', time_ms: 0.03 + r() * 0.05, answer: `142.250.${Math.floor(r() * 255)}.${Math.floor(r() * 255)}` },
      upstream: pick(r, UPSTREAMS),
      flags: { ...base.flags, Cached: true, Stale: stale },
    };
  }
  if (roll < 0.985) {
    const cname = domain === 'www.youtube.com' ? 'youtube-ui.l.google.com' : undefined;
    return {
      ...base,
      domain,
      status: 'forwarded',
      status_class: 'permitted',
      rcode: qtype === 'MX' || qtype === 'SRV' ? 'NOERROR' : 'NOERROR',
      reply: { type: qtype === 'TXT' ? 'blob' : cname ? 'cname' : 'ip', time_ms: 8 + r() * 40, answer: cname ? `CNAME ${cname}` : '93.184.215.14' },
      upstream: pick(r, UPSTREAMS),
      dnssec: r() < 0.3 ? 'secure' : 'insecure',
      ...(cname ? { cname } : {}),
    };
  }
  const timeout = r() < 0.5;
  return {
    ...base,
    domain,
    status: timeout ? 'timeout_upstream' : 'servfail_upstream',
    status_class: 'error',
    rcode: 'SERVFAIL',
    reply: { type: 'servfail', time_ms: timeout ? 2000 : 30 + r() * 30 },
    upstream: pick(r, UPSTREAMS),
    ede: timeout ? 22 : 23,
  };
}

function buildQueries(now: number, r: () => number, n = 500): QueryRow[] {
  const rows: QueryRow[] = [];
  // Newest first, like GET /queries; spread over the last 24 h with a
  // denser recent hour.
  let t = now - 2_000;
  for (let i = 0; i < n; i++) {
    const id = n - i + 100_000;
    const node = r() < 0.7 ? 'kudurru-0' : 'kudurru-1';
    rows.push(makeQueryRow(id, t, r, node));
    t -= i < 200 ? 3_000 + r() * 15_000 : 60_000 + r() * 240_000;
  }
  return rows;
}

function buildNodes(now: number): NodeInfo[] {
  return [
    {
      name: 'kudurru-0',
      version: 'v0.1.0-rc.1',
      roles: ['dns', 'api', 'controller'],
      ready: true,
      leader: true,
      vipMaster: true,
      qps: 14.2,
      snapshotAge: '2s',
      address: '192.168.1.2',
      storeDriver: 'sqlite',
      startedAt: new Date(now - 86_400_000 * 3).toISOString(),
    },
    {
      name: 'kudurru-1',
      version: 'v0.1.0-rc.1',
      roles: ['dns', 'api'],
      ready: true,
      leader: false,
      vipMaster: false,
      qps: 5.9,
      snapshotAge: '4s',
      address: '192.168.1.3',
      storeDriver: 'sqlite',
      startedAt: new Date(now - 86_400_000 * 1.2).toISOString(),
    },
  ];
}

function buildMessages(now: number): DiagnosisMessage[] {
  return [
    {
      id: 'list-unreachable/default/oisd-small',
      severity: 'warning',
      reason: 'ListUnreachable',
      message: 'BlockList default/oisd-small: last fetch failed, serving the cached body from 30 hours ago',
      object: { kind: 'BlockList', namespace: NS, name: 'oisd-small' },
      hint: 'Check egress to small.oisd.nl or raise blocking.refreshInterval.',
      since: new Date(now - 3_600_000 * 6).toISOString(),
    },
    {
      id: 'list-invalid/default/ui-deny',
      severity: 'error',
      reason: 'InvalidEntries',
      message: 'BlockList default/ui-deny has 1 invalid entry: line 2: "bad..domain" is not a domain name',
      object: { kind: 'BlockList', namespace: NS, name: 'ui-deny' },
      since: new Date(now - 600_000).toISOString(),
    },
    {
      id: 'policy-tie/default/iot-strict',
      severity: 'info',
      reason: 'PriorityTie',
      message: 'Policies default/iot-strict and default/default both match 192.168.1.20/30 at priority 10; the name decides.',
      object: { kind: 'Policy', namespace: NS, name: 'iot-strict' },
      hint: 'Give one of them a higher priority.',
      since: new Date(now - 86_400_000).toISOString(),
    },
  ];
}

function buildAudit(now: number): AuditEntry[] {
  const actions: [string, string, string, string][] = [
    ['admin', 'update', 'BlockList', 'stevenblack'],
    ['admin', 'create', 'DNSRecordSet', 'api-home-arpa'],
    ['robert@example.com', 'update', 'Policy', 'kids-bedtime'],
    ['system:serviceaccount:kudurru:external-dns', 'apply', 'DNSRecordSet', 'external-dns-ingress'],
    ['admin', 'blocking.disable', 'Runtime', 'blocking'],
    ['robert@example.com', 'delete', 'ClientGroup', 'guests'],
    ['viewer@example.com', 'update', 'Policy', 'default'],
  ];
  return actions.map(([actor, action, kind, name], i) => ({
    id: `a${1000 - i}`,
    ts: new Date(now - i * 3_100_000).toISOString(),
    actor,
    action,
    kind,
    ...(kind === 'Runtime' ? {} : { namespace: NS }),
    name,
    outcome: actor.startsWith('viewer') ? 'denied' : 'success',
    authzPath: actor.startsWith('system:') ? 'kubernetes-sar' : 'rolebinding/admins',
  }));
}

function buildSettings(): Record<string, unknown> {
  return {
    upstreams: {
      groups: [
        { name: 'default', strategy: 'parallel_best', upstreams: ['tls://1.1.1.1:853', 'tls://9.9.9.9:853'] },
        { name: 'filtered', strategy: 'strict', upstreams: ['https://dns.google/dns-query'] },
      ],
    },
    cache: { maxEntries: 100000, minTTL: '0s', maxTTL: '24h', prefetch: true },
    blocking: { enabled: true, defaultResponse: 'zeroIP', blockTTL: '1m', refreshInterval: '24h' },
    querylog: { enabled: true, privacy: 'full', retention: { raw: '168h', maxDBBytes: '10GiB' } },
    ratelimit: { qpm: 6000 },
  };
}

function buildNodeConfig(): Record<string, unknown> {
  return {
    node: { name: 'kudurru-0', dataDir: '/var/lib/kudurru' },
    listeners: { api: { address: '0.0.0.0:8080' }, ops: { address: '0.0.0.0:8081' }, metrics: { address: '0.0.0.0:9153' } },
    log: { level: 'info', format: 'json' },
  };
}

function withEnvLocks(schema: SettingKey[]): SettingKey[] {
  // Mirrors config_test.go: these keys come from the environment in the
  // standard compose file; log.level is env-locked here too so the UI has
  // an env-locked key that is not also host-only.
  const locked = new Set(['listeners.api.address', 'listeners.proxyProtocolFrom', 'node.name', 'log.level']);
  return schema.map((k) => (locked.has(k.path) ? { ...k, envLocked: true } : k));
}

/** A fresh database; handlers mutate it, tests call resetDB(). */
export function createDB(now = FIXED_NOW): MockDB {
  const r = rng(20261010);
  const zones = buildZones();
  const recordSets = buildRecordSets();
  for (const z of zones) {
    if (z.status) {
      z.status.recordCount = recordSets
        .filter((rs) => rs.spec.zoneRef.name === z.metadata.name)
        .reduce((n, rs) => n + rs.spec.endpoints.reduce((m, e) => m + e.targets.length, 0), 0);
    }
  }
  const queries = buildQueries(now, r);
  return {
    now,
    zones,
    recordSets,
    lists: buildLists(now),
    groups: buildGroups(),
    policies: buildPolicies(),
    queries,
    series: buildSeries(now, r),
    nodes: buildNodes(now),
    messages: buildMessages(now),
    audit: buildAudit(now),
    settings: buildSettings(),
    nodeConfig: buildNodeConfig(),
    schema: withEnvLocks((schemaJSON as { items: SettingKey[] }).items),
    blocking: { blocking: true },
    tokens: [{ id: 'tk1a2b3c', name: 'grafana', createdAt: '2026-09-20T10:00:00Z', lastUsedAt: new Date(now - 120_000).toISOString() }],
    users: [
      { username: 'admin', password: 'admin', name: 'admin', roles: ['admin'] },
      { username: 'viewer', password: 'viewer', name: 'viewer', roles: ['viewer'] },
    ],
    session: null,
    nextQueryID: 100_501,
    rv: 100,
  };
}
