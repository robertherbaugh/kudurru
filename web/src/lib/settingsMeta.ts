// Settings metadata the Settings page renders from (C1). GET
// /settings/_schema carries enum, min/max, presets, format, ref, items,
// values, fields, group and label per key; servers from before C1 send
// none of them. KNOWN fills the gaps for the keys of internal/config/
// schema.go so the page is typed either way: metaFor() merges the server's
// metadata over it field by field (the server wins), and a key neither
// knows renders from its bare type (text, JSON for lists and maps).
import type { SettingEnumOption, SettingKey, SettingMeta, SettingShape, SettingShapeField } from '../api/types';

/** UI-only additions to a shape: what a map's keys are. */
export interface UiShape extends SettingShape {
  keyLabel?: string;
  keyPlaceholder?: string;
  items?: UiShape;
  values?: UiShape;
  fields?: (SettingShapeField & UiShape)[];
}

export type UiMeta = Omit<SettingMeta, 'items' | 'values' | 'fields'> & {
  items?: UiShape;
  values?: UiShape;
  fields?: (SettingShapeField & UiShape)[];
  keyLabel?: string;
  keyPlaceholder?: string;
};

const opt = (value: string, label: string, help?: string): SettingEnumOption => (help ? { value, label, help } : { value, label });

export const BLOCK_RESPONSES: SettingEnumOption[] = [
  opt('zeroIP', 'Zero IP (0.0.0.0 / ::)', 'A and AAAA get the unspecified address, other types NODATA; clients fail fast.'),
  opt('nxdomain', 'NXDOMAIN', 'The name "does not exist".'),
  opt('customIP', 'Custom IP', 'A and AAAA get the addresses in blocking.customIP; empty answers NODATA.'),
  opt('refused', 'REFUSED', 'Discouraged: stub resolvers fail over to the next server.'),
];

/** The C5 block-page response; offered only where the server has the block page keys. */
export const BLOCK_PAGE_RESPONSE = opt('blockPage', 'Block page', 'A and AAAA get the block page address so browsers show why; other types NODATA.');

export const STRATEGY_OPTIONS: SettingEnumOption[] = [
  opt('parallel_best', 'Parallel best', 'Race two healthy servers and take the first usable answer (default).'),
  opt('strict', 'Strict order', 'Ask the servers in order, moving on only on failure.'),
  opt('random', 'Random', 'Ask one healthy server at random.'),
  opt('rtt_weighted', 'Fastest first', 'Pick by smoothed round-trip time, weighted at random.'),
];

const TLS_FIELDS: SettingShapeField = {
  name: 'tls',
  label: 'TLS',
  type: 'object',
  fields: [
    { name: 'certFile', label: 'Certificate file', type: 'string' },
    { name: 'keyFile', label: 'Key file', type: 'string' },
  ],
};

const LISTENER = (extra: SettingShapeField[]): UiShape => ({
  type: 'object',
  fields: [{ name: 'address', label: 'Address', type: 'string', format: 'hostport' }, ...extra],
});

/** Metadata for the keys of internal/config/schema.go, used where the server sends none. */
const KNOWN: Record<string, UiMeta> = {
  // Upstreams
  'upstreams.groups': {
    group: 'Upstreams',
    label: 'Upstream groups',
    keyLabel: 'Group',
    values: {
      type: 'object',
      fields: [
        { name: 'servers', label: 'Servers', type: 'string[]', items: { type: 'string', format: 'upstream' } },
        { name: 'strategy', label: 'Strategy', type: 'string', enum: STRATEGY_OPTIONS },
        { name: 'timeout', label: 'Timeout', type: 'duration', presets: ['500ms', '1s', '2s', '3s', '5s', '10s'] },
      ],
    },
  },
  'upstreams.bootstrap': { group: 'Upstreams', label: 'Bootstrap resolvers', items: { type: 'string', format: 'hostport' } },
  'upstreams.conditional': {
    group: 'Upstreams',
    label: 'Conditional forwarding',
    keyLabel: 'Domain suffix',
    keyPlaceholder: 'corp.example',
    values: { type: 'string', ref: 'upstreamGroup' },
  },
  'upstreams.ecs.mode': {
    group: 'Upstreams',
    label: 'EDNS Client Subnet',
    enum: [
      opt('strip', 'Strip', 'Never send a client subnet upstream (privacy default).'),
      opt('forward', 'Forward', "Pass the client's own ECS option."),
      opt('add', 'Forward or add', "Forward the client's option, else derive one from a public client address."),
    ],
  },
  'upstreams.ecs.ipv4Prefix': { group: 'Upstreams', label: 'ECS IPv4 prefix', min: 0, max: 32 },
  'upstreams.ecs.ipv6Prefix': { group: 'Upstreams', label: 'ECS IPv6 prefix', min: 0, max: 128 },

  // Cache
  'cache.maxEntries': { group: 'Cache', label: 'Maximum entries', min: 0 },
  'cache.minTTL': { group: 'Cache', label: 'Minimum TTL', presets: ['0s', '5s', '30s', '1m', '5m'] },
  'cache.maxTTL': { group: 'Cache', label: 'Maximum TTL', presets: ['1h', '6h', '24h', '168h'] },
  'cache.negativeTTL': { group: 'Cache', label: 'Negative answer TTL', presets: ['30s', '1m', '5m', '15m', '1h'] },
  'cache.prefetch.threshold': { group: 'Cache', label: 'Prefetch threshold', min: 0 },
  'cache.prefetch.window': { group: 'Cache', label: 'Prefetch window', presets: ['10m', '30m', '1h', '2h', '6h'] },
  'cache.serveStale.ttl': { group: 'Cache', label: 'Serve stale for', presets: ['0s', '1h', '24h', '72h', '168h'] },
  'cache.serveStale.answerTTL': { group: 'Cache', label: 'Stale answer TTL', presets: ['0s', '10s', '30s', '1m'] },
  'cache.serveStale.maxWait': { group: 'Cache', label: 'Stale answer wait', presets: ['0s', '500ms', '1800ms', '3s'] },

  // Blocking
  'blocking.enabled': { group: 'Blocking', label: 'Blocking' },
  'blocking.defaultResponse': { group: 'Blocking', label: 'Default block response', enum: BLOCK_RESPONSES },
  'blocking.blockTTL': { group: 'Blocking', label: 'Block response TTL', presets: ['10s', '30s', '1m', '5m', '10m', '1h'] },
  'blocking.allowDegraded': { group: 'Blocking', label: 'Serve before lists are loaded' },
  'blocking.customIP.v4': { group: 'Blocking', label: 'Custom IPv4 answer', format: 'ipv4' },
  'blocking.customIP.v6': { group: 'Blocking', label: 'Custom IPv6 answer', format: 'ipv6' },
  'blocking.refreshInterval': { group: 'Blocking', label: 'List refresh interval', presets: ['1h', '6h', '12h', '24h', '168h'] },
  'blocking.initStrategy': {
    group: 'Blocking',
    label: 'List loading at start',
    enum: [
      opt('fast', 'Fast', 'Serve at once from the disk cache and load the lists in the background.'),
      opt('blocking', 'Wait for lists', 'Not ready until every list is loaded.'),
      opt('failOnError', 'Wait, fail on error', 'Like waiting, and exit when a list fails to load.'),
    ],
  },
  'blocking.listsDir': { group: 'Blocking', label: 'List cache directory' },
  'blocking.allowLocalListURLs': { group: 'Blocking', label: 'Allow local list URLs' },
  'blocking.catalogue.url': { group: 'Blocking', label: 'Service catalogue URL', format: 'url' },
  'blocking.catalogue.refreshInterval': { group: 'Blocking', label: 'Catalogue refresh interval', presets: ['1h', '6h', '24h', '168h'] },

  // Block page (C5)
  'blockPage.enabled': { group: 'Block page', label: 'Block page' },
  'blockPage.ipv4': { group: 'Block page', label: 'IPv4 address clients are sent to', format: 'ipv4' },
  'blockPage.ipv6': { group: 'Block page', label: 'IPv6 address clients are sent to', format: 'ipv6' },
  'blockPage.title': { group: 'Block page', label: 'Title' },
  'blockPage.message': { group: 'Block page', label: 'Message' },
  'blockPage.contact': { group: 'Block page', label: 'Administrator contact' },
  'blockPage.showDetails': { group: 'Block page', label: 'Show why (list, rule, policy)' },
  'listeners.blockPage.http': { group: 'Block page', label: 'HTTP listener', format: 'hostport' },
  'listeners.blockPage.https': { group: 'Block page', label: 'HTTPS listener', format: 'hostport' },
  'listeners.blockPage.tls.certFile': { group: 'Block page', label: 'HTTPS certificate file' },
  'listeners.blockPage.tls.keyFile': { group: 'Block page', label: 'HTTPS key file' },

  // Query log
  'querylog.enabled': { group: 'Query log', label: 'Query log' },
  'querylog.sink': {
    group: 'Query log',
    label: 'Storage',
    enum: [opt('sqlite', 'SQLite', 'querylog.db with rollups; the dashboard needs it.'), opt('slog', 'Log lines', 'One JSON log line per query; nothing stored.')],
  },
  'querylog.privacy': {
    group: 'Query log',
    label: 'Privacy',
    enum: [
      opt('full', 'Everything', 'Client, domain and answer are stored.'),
      opt('anonymize_client', 'Anonymize clients', 'Clients are hidden as set by "Client anonymization".'),
      opt('hide_domains', 'Hide domains', 'Clients are stored, domains are not.'),
      opt('hide_domains_clients', 'Hide domains and clients', 'Only the query type, status and timing are stored.'),
      opt('nothing', 'Nothing', 'Per-minute totals only; no rows.'),
    ],
  },
  'querylog.anonymize': {
    group: 'Query log',
    label: 'Client anonymization',
    enum: [
      opt('prefix', 'Network prefix', 'IPv4 /24 and IPv6 /48.'),
      opt('pseudonym', 'Pseudonym', 'A keyed hash rotated daily: stable for a day, not reversible.'),
    ],
  },
  'querylog.retention.raw': { group: 'Query log', label: 'Keep query rows for', presets: ['24h', '168h', '720h', '2160h', '0s'] },
  'querylog.retention.maxDBBytes': { group: 'Query log', label: 'Maximum database size' },
  'querylog.exclude.domains': { group: 'Query log', label: 'Never log these names', items: { type: 'string' } },
  'querylog.exclude.clients': { group: 'Query log', label: 'Never log these clients', items: { type: 'string' } },
  'querylog.exclude.sudn': { group: 'Query log', label: 'Skip special-use names' },

  // Rate limiting
  'ratelimit.enabled': { group: 'Rate limiting', label: 'Rate limiting' },
  'ratelimit.ipv4': { group: 'Rate limiting', label: 'IPv4 limits (queries per minute)', keyLabel: 'Prefix length', keyPlaceholder: '24', values: { type: 'int', min: 0 } },
  'ratelimit.ipv6': { group: 'Rate limiting', label: 'IPv6 limits (queries per minute)', keyLabel: 'Prefix length', keyPlaceholder: '64', values: { type: 'int', min: 0 } },
  'ratelimit.slip': { group: 'Rate limiting', label: 'Slip (%)', min: 0, max: 100 },
  'ratelimit.maxBuckets': { group: 'Rate limiting', label: 'Tracked prefixes', min: 1 },
  'ratelimit.exempt': { group: 'Rate limiting', label: 'Exempt clients', items: { type: 'string', format: 'cidr' } },

  // Security
  'auth.localLogin': {
    group: 'Security',
    label: 'Password login',
    enum: [
      opt('enabled', 'Enabled', 'Every local user may sign in with a password.'),
      opt('admins-only', 'Admins only', 'Only built-in admins may sign in with a password.'),
      opt('disabled', 'Disabled', 'Single sign-on only; `kudurru admin` over the admin socket still works.'),
    ],
  },
  'auth.session.idleTimeout': { group: 'Security', label: 'Session idle timeout', presets: ['15m', '30m', '1h', '4h', '12h'] },
  'auth.session.lifetime': { group: 'Security', label: 'Session lifetime', presets: ['1h', '8h', '12h', '24h', '168h'] },
  'auth.session.secure': {
    group: 'Security',
    label: 'Secure session cookie',
    enum: [
      opt('auto', 'Automatic', 'When TLS terminates here or a trusted proxy reports https.'),
      opt('always', 'Always'),
      opt('never', 'Never', 'Only for plain-HTTP test setups.'),
    ],
  },
  'auth.tokens.maxLifetime': { group: 'Security', label: 'Longest API token lifetime', presets: ['0s', '24h', '168h', '720h', '2160h', '8760h'] },
  'auth.kubernetes.mode': { group: 'Security', label: 'Kubernetes token authorization', enum: [opt('sar', 'SubjectAccessReview')] },
  'auth.adminSocket.path': { group: 'Security', label: 'Admin socket' },
  'auth.adminSocket.group': { group: 'Security', label: 'Admin socket group (gid)', min: -1 },
  'auth.bootstrap.adminPasswordFile': { group: 'Security', label: 'First-boot admin password file' },
  'auth.identityProviders': { group: 'Security', label: 'Identity providers' },
  'rbac.bindings': { group: 'Security', label: 'Role bindings' },

  // Listeners
  'listeners.dns': {
    group: 'Listeners',
    label: 'DNS (UDP/TCP)',
    items: LISTENER([{ name: 'protocols', label: 'Protocols', type: 'string[]', items: { type: 'string', enum: [opt('udp', 'UDP'), opt('tcp', 'TCP')] } }]),
  },
  'listeners.dot': { group: 'Listeners', label: 'DNS over TLS', items: LISTENER([TLS_FIELDS]) },
  'listeners.doh': { group: 'Listeners', label: 'DNS over HTTPS', items: LISTENER([{ name: 'path', label: 'Path', type: 'string' }, TLS_FIELDS]) },
  'listeners.doq': { group: 'Listeners', label: 'DNS over QUIC', items: LISTENER([TLS_FIELDS]) },
  'listeners.doh3': { group: 'Listeners', label: 'DNS over HTTP/3', items: LISTENER([{ name: 'path', label: 'Path', type: 'string' }, TLS_FIELDS]) },
  'listeners.api.address': { group: 'Listeners', label: 'API and dashboard', format: 'hostport' },
  'listeners.api.tls.auto': {
    group: 'Listeners',
    label: 'API TLS without a certificate',
    enum: [opt('selfsigned', 'Self-signed', 'Generate a certificate once.'), opt('off', 'Off', 'Plain HTTP, e.g. behind a TLS-terminating proxy.')],
  },
  'listeners.api.trustedProxies': { group: 'Listeners', label: 'Trusted reverse proxies', items: { type: 'string', format: 'cidr' } },
  'listeners.ops.address': { group: 'Listeners', label: 'Health', format: 'hostport' },
  'listeners.metrics.address': { group: 'Listeners', label: 'Metrics', format: 'hostport' },
  'listeners.internal.address': { group: 'Listeners', label: 'Node to node', format: 'hostport' },
  'listeners.proxyProtocolFrom': { group: 'Listeners', label: 'PROXY protocol peers', items: { type: 'string', format: 'cidr' } },
  'listeners.sockets': { group: 'Listeners', label: 'Sockets per listener', min: 0 },

  // Logging
  'log.level': {
    group: 'Logging',
    label: 'Log level',
    enum: [opt('debug', 'Debug'), opt('info', 'Info'), opt('warn', 'Warnings'), opt('error', 'Errors only')],
  },
  'log.format': { group: 'Logging', label: 'Log format', enum: [opt('json', 'JSON'), opt('text', 'Text')] },

  // Node and storage
  'store.driver': {
    group: 'Node and storage',
    label: 'Store',
    enum: [opt('sqlite', 'SQLite', 'state.db under the data directory.'), opt('kube', 'Kubernetes', 'CRDs through the apiserver.')],
  },
  'notify.source': { group: 'Zones and transfers', label: 'NOTIFY source address', format: 'ip' },
  tsigKeys: {
    group: 'Zones and transfers',
    label: 'TSIG keys',
    items: {
      type: 'object',
      fields: [
        { name: 'name', label: 'Name', type: 'string' },
        { name: 'algorithm', label: 'Algorithm', type: 'string', enum: [opt('hmac-sha256', 'HMAC-SHA256'), opt('hmac-sha512', 'HMAC-SHA512')] },
        { name: 'secretFile', label: 'Secret file', type: 'string' },
      ],
    },
  },
  'externalDNS.webhook.listen': { group: 'Integrations', label: 'external-dns webhook listener', format: 'hostport' },
  'externalDNS.webhook.zoneSelector': { group: 'Integrations', label: 'external-dns zone selector', keyLabel: 'Label', values: { type: 'string' } },
};

/** Sections for keys without a group, by the path's first segment. */
const SECTION_GROUPS: Record<string, string> = {
  upstreams: 'Upstreams',
  cache: 'Cache',
  blocking: 'Blocking',
  blockPage: 'Block page',
  querylog: 'Query log',
  ratelimit: 'Rate limiting',
  auth: 'Security',
  rbac: 'Security',
  listeners: 'Listeners',
  log: 'Logging',
  node: 'Node and storage',
  store: 'Node and storage',
  objects: 'Node and storage',
  objectsDir: 'Node and storage',
  settingsFile: 'Node and storage',
  notify: 'Zones and transfers',
  tsigKeys: 'Zones and transfers',
  externalDNS: 'Integrations',
};

/** The order sections appear in; others follow alphabetically. */
export const GROUP_ORDER = [
  'DNS',
  'Upstreams',
  'Blocking',
  'Block page',
  'Cache',
  'Query log',
  'Rate limiting',
  'Security',
  'Zones and transfers',
  'Listeners',
  'Logging',
  'Integrations',
  'Node and storage',
];

/** "maxDBBytes" → "max DB bytes", "customIP" → "custom IP". */
function words(seg: string): string {
  return seg
    .replace(/([a-z0-9])([A-Z])/g, '$1 $2')
    .replace(/([A-Z]+)([A-Z][a-z])/g, '$1 $2')
    .split(' ')
    .map((w) => (/^[A-Z0-9]+$/.test(w) && w.length > 1 ? w : w.toLowerCase()))
    .join(' ');
}

/** A label from the path when nothing names one: "cache.serveStale.ttl" → "Serve stale ttl". */
export function labelFromPath(path: string): string {
  const segs = path.split('.');
  const s = (segs.length > 1 ? segs.slice(1) : segs).map(words).join(' ');
  return s.charAt(0).toUpperCase() + s.slice(1);
}

/** The metadata of a key: the server's C1 fields over the built-in table. */
export function metaFor(k: SettingKey, all?: SettingKey[]): UiMeta {
  const known = KNOWN[k.path] ?? {};
  const server: UiMeta = {};
  for (const f of ['enum', 'min', 'max', 'presets', 'format', 'ref', 'items', 'values', 'fields', 'group', 'label'] as const) {
    if (k[f] !== undefined && k[f] !== null) (server as Record<string, unknown>)[f] = k[f];
  }
  const m: UiMeta = { ...known, ...server };
  // Without block page keys the server would reject the blockPage response.
  if (k.path === 'blocking.defaultResponse' && !server.enum && all?.some((x) => x.path === 'blockPage.enabled')) {
    m.enum = [...BLOCK_RESPONSES, BLOCK_PAGE_RESPONSE];
  }
  return m;
}

export function groupOf(k: SettingKey, m: UiMeta = metaFor(k)): string {
  return m.group ?? (k.path.startsWith('listeners.blockPage.') ? 'Block page' : (SECTION_GROUPS[k.path.split('.')[0] ?? ''] ?? 'Other'));
}

export function labelOf(k: SettingKey, m: UiMeta = metaFor(k)): string {
  return m.label ?? labelFromPath(k.path);
}

/** The shape a key's value is edited with. */
export function shapeOf(k: SettingKey, m: UiMeta = metaFor(k)): UiShape {
  const { group: _g, label: _l, ...shape } = m;
  return { type: k.type, ...shape };
}

/** Sorts section names by GROUP_ORDER, then alphabetically. */
export function compareGroups(a: string, b: string): number {
  const ia = GROUP_ORDER.indexOf(a);
  const ib = GROUP_ORDER.indexOf(b);
  if (ia >= 0 && ib >= 0) return ia - ib;
  if (ia >= 0) return -1;
  if (ib >= 0) return 1;
  return a.localeCompare(b);
}
