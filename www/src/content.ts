// Everything the pages say. Claims here follow CHANGELOG.md (0.1.0), the
// README and bench/baseline.txt; when a feature moves between releases,
// change it here and nowhere else.

export const SITE_URL = "https://kudurru.io";
export const DOCS_URL = "https://docs.kudurru.io";
export const REPO_URL = "https://github.com/robertherbaugh/kudurru";
export const IMAGE = "ghcr.io/robertherbaugh/kudurru";
export const CHARTS = "oci://ghcr.io/robertherbaugh/charts";

// The license page (/license/): follows LICENSING.md and internal/license.
export const LICENSE_CONTACT = "hello@kudurru.io";
// The license server that issues free personal keys: its origin, no path. The
// form posts to /api/v1/public/key-requests below it. Override at build time
// with WWW_LICENSE_API; the CSP (src/headers.ts) allows exactly this origin,
// and the server's PUBLIC_FORM_ORIGINS must list https://kudurru.io.
export const DEFAULT_LICENSE_API = "https://licenses.kudurru.io";
// A business request is not sent to the server: it opens this subject in the
// visitor's mail program with every field filled in.
export const BUSINESS_SUBJECT = "Business License";
export const LICENSE_FREE_NODES = 3;
export const LICENSE_FREE_POLICIES = 2;

export interface LicenseRow {
  what: string;
  community: string;
  personal: string;
  business: string;
}

export const LICENSE_ROWS: LicenseRow[] = [
  { what: "DNS answering, blocking, zones, the block page, the dashboard, short-term statistics", community: "Yes", personal: "Yes", business: "Yes" },
  { what: "Nodes in one cluster", community: "1", personal: `Up to ${LICENSE_FREE_NODES}`, business: "As agreed" },
  { what: "Policies", community: String(LICENSE_FREE_POLICIES), personal: "Unlimited", business: "Unlimited" },
  { what: "Roles beyond the built-in administrator, zone-scoped roles, Kubernetes RBAC", community: "No", personal: "Yes", business: "Yes" },
  { what: "OIDC single sign-on", community: "No", personal: "Yes", business: "Yes" },
  { what: "The audit trail and its export", community: "No", personal: "Yes", business: "Yes" },
  { what: "Long-term statistics (anything older than 24 hours)", community: "No", personal: "Yes", business: "Yes" },
  { what: "Unblock requests from the block page", community: "No", personal: "Yes", business: "Yes" },
  { what: "The dnsdist and keepalived edge", community: "No", personal: "Yes", business: "Yes" },
  { what: "Integrations, such as the external-dns webhook", community: "No", personal: "Yes", business: "Yes" },
  { what: "Use by a business", community: "No", personal: "No", business: "Yes" },
];

export const TAGLINE = "authority over every zone.";
export const DESCRIPTION =
  "kudurru (koo-DOO-roo) is a Kubernetes-native DNS server that also runs as one container: authoritative zones, a forwarding resolver over every encrypted transport, blocking with client groups, a dashboard that explains every answer, OIDC and RBAC.";

export function docs(path = "/"): string {
  return DOCS_URL + path;
}

export interface NavLink {
  label: string;
  href: string;
}

export const NAV: NavLink[] = [
  { label: "Pipeline", href: "#pipeline" },
  { label: "Features", href: "#features" },
  { label: "Dashboard", href: "#dashboard" },
  { label: "Deploy", href: "#deploy" },
  { label: "Roadmap", href: "#roadmap" },
];

// The README's lineage table: what each reference project contributed.
export interface Lineage {
  idea: string;
  from: { name: string; href: string }[];
  meaning: string;
}

export const LINEAGE: Lineage[] = [
  {
    idea: "Replicated, API-first, one binary with roles",
    from: [{ name: "Gravity", href: "https://github.com/BeryJu/gravity" }],
    meaning: "One kudurru binary runs every role. The store replicates configuration; nobody copies it by hand.",
  },
  {
    idea: "The extreme feature set",
    from: [{ name: "Technitium", href: "https://technitium.com/dns/" }],
    meaning: "Typed zones with real SOA serials, AXFR and IXFR, NOTIFY, TSIG, every encrypted transport, per-zone access policies.",
  },
  {
    idea: "The dashboard everyone loves",
    from: [{ name: "Pi-hole", href: "https://pi-hole.net/" }],
    meaning: "Stat tiles, 24-hour charts, drill-down everywhere, a query log that says why, and multi-node views Pi-hole never had.",
  },
  {
    idea: "Powerful blocking",
    from: [
      { name: "blocky", href: "https://github.com/0xERR0R/blocky" },
      { name: "AdGuard Home", href: "https://github.com/AdguardTeam/AdGuardHome" },
    ],
    meaning: "hosts, domain, adblock and regex lists; client groups; per-group policies and schedules; zero-allocation matchers behind a CI gate.",
  },
  {
    idea: "Load balancing at the edge",
    from: [{ name: "dnsdist", href: "https://dnsdist.org" }],
    meaning: "A kudurru supervisor renders, checks and runs dnsdist in front of every node. Client IPs survive through authenticated PROXYv2.",
  },
  {
    idea: "Virtual IPs on bare metal",
    from: [{ name: "keepalived", href: "https://www.keepalived.org" }],
    meaning: "A kudurru supervisor renders and runs keepalived (VRRPv3, authenticated, nopreempt), so the VIP follows dnsdist health.",
  },
];

export const TOGETHER: { title: string; body: string; release?: string }[] = [
  {
    title: "CRDs as the source of truth",
    body: "In a cluster, zones, records, lists, groups and policies are Kubernetes objects. In Docker the same YAML goes into SQLite.",
  },
  {
    title: "OIDC login with real RBAC",
    body: "viewer, auditor, operator, zone-admin and admin, scoped per zone. A login with no role binding is refused, never silently admin. Needs a license key (free for personal use).",
  },
  {
    title: "A simple integration model",
    body: "Write records to Cloudflare for split horizon, pull client inventory from UniFi.",
    release: "v0.2",
  },
];

// The v0.1 pipeline. pipeline.Order also reserves rewrite, dnssec and
// postprocess; those stages arrive with later releases.
export type StageId =
  | "ingress"
  | "identify"
  | "control"
  | "authoritative"
  | "blocking"
  | "cache"
  | "upstream"
  | "log";

export const STAGES: { id: StageId; does: string }[] = [
  { id: "ingress", does: "Decodes the message, applies PROXYv2 and per-prefix rate limits" },
  { id: "identify", does: "Maps the source address or DoT/DoH client ID to a client and its groups" },
  { id: "control", does: "Refuses malformed queries, answers CHAOS and health probes" },
  { id: "authoritative", does: "Answers names in local zones from an in-memory tree" },
  { id: "blocking", does: "Evaluates the client's merged policies: lists, services, safe search" },
  { id: "cache", does: "Sharded TTL cache with prefetch, serve-stale and in-flight coalescing" },
  { id: "upstream", does: "Forwards to the client's upstream group over DoT, DoH, DoQ or Do53" },
  { id: "log", does: "Records the answer and the reason for it in the query log" },
];

export type Status = "blocked" | "rewritten" | "authoritative" | "cached" | "forwarded" | "truncated";

// How the query log names each status.
export const STATUS_LABEL: Record<Status, string> = {
  blocked: "blocked",
  rewritten: "safe search",
  authoritative: "authoritative",
  cached: "cached",
  forwarded: "forwarded",
  truncated: "truncated",
};

export interface Step {
  stage: StageId;
  note: string;
}

export interface Scenario {
  id: string;
  label: string;
  client: string;
  address: string;
  proto: "udp" | "tcp" | "dot" | "doh" | "doq";
  qtype: string;
  qname: string;
  status: Status;
  // The stage that produced the answer.
  decidedBy: StageId;
  steps: Step[];
  answer: string[];
  micros: number;
}

// Sample traffic. Names under .example and the 192.0.2.0/24,
// 198.51.100.0/24 and 203.0.113.0/24 addresses are documentation
// placeholders (RFC 2606, RFC 5737).
export const SCENARIOS: Scenario[] = [
  {
    id: "blocked",
    label: "An ad domain from a kid's tablet",
    client: "kids-tablet",
    address: "192.168.1.42",
    proto: "udp",
    qtype: "A",
    qname: "doubleclick.net",
    status: "blocked",
    decidedBy: "blocking",
    steps: [
      { stage: "ingress", note: "41 bytes over UDP from 192.168.1.42, under the prefix rate limit" },
      { stage: "identify", note: "192.168.1.42 is client kids-tablet, member of group kids" },
      { stage: "control", note: "well-formed, one question, no CHAOS class" },
      { stage: "authoritative", note: "no local zone covers doubleclick.net" },
      { stage: "blocking", note: "policy kids matches list default-ads, rule ||doubleclick.net^, mode zeroIP" },
      { stage: "log", note: "status blocked list, EDE 15 (Blocked), list default-ads" },
    ],
    answer: ["doubleclick.net.  600  IN  A  0.0.0.0"],
    micros: 109,
  },
  {
    id: "authoritative",
    label: "A name in your own zone",
    client: "robert-laptop",
    address: "192.168.1.10",
    proto: "dot",
    qtype: "A",
    qname: "nas.home.example.com",
    status: "authoritative",
    decidedBy: "authoritative",
    steps: [
      { stage: "ingress", note: "DoT session from 192.168.1.10, TLS 1.3" },
      { stage: "identify", note: "192.168.1.10 is client robert-laptop, group default" },
      { stage: "control", note: "well-formed, one question" },
      { stage: "authoritative", note: "zone home.example.com (primary, serial 2026101203), record set apps/nas" },
      { stage: "log", note: "status authoritative, AA set, zone home.example.com" },
    ],
    answer: ["nas.home.example.com.  300  IN  A  10.0.0.20"],
    micros: 38,
  },
  {
    id: "cached",
    label: "A popular name, already cached",
    client: "living-room-tv",
    address: "192.168.1.31",
    proto: "doh",
    qtype: "A",
    qname: "media.streaming.example",
    status: "cached",
    decidedBy: "cache",
    steps: [
      { stage: "ingress", note: "DoH POST over HTTP/2 from 192.168.1.31" },
      { stage: "identify", note: "192.168.1.31 is client living-room-tv, group default" },
      { stage: "control", note: "well-formed, one question" },
      { stage: "authoritative", note: "no local zone covers streaming.example" },
      { stage: "blocking", note: "no list, service or rule of policy default matches" },
      { stage: "cache", note: "hit in shard 11, 41 s of 60 s TTL left, prefetch scheduled at 6 s" },
      { stage: "log", note: "status cached, upstream none" },
    ],
    answer: ["media.streaming.example.  41  IN  A  198.51.100.7"],
    micros: 52,
  },
  {
    id: "forwarded",
    label: "A new name, sent upstream",
    client: "robert-phone",
    address: "192.168.1.12",
    proto: "doq",
    qtype: "AAAA",
    qname: "api.weather.example",
    status: "forwarded",
    decidedBy: "upstream",
    steps: [
      { stage: "ingress", note: "DoQ stream from 192.168.1.12" },
      { stage: "identify", note: "192.168.1.12 is client robert-phone, group default" },
      { stage: "control", note: "well-formed, one question" },
      { stage: "authoritative", note: "no local zone covers weather.example" },
      { stage: "blocking", note: "no list, service or rule of policy default matches" },
      { stage: "cache", note: "miss; one in-flight request for the name, so no duplicate goes upstream" },
      { stage: "upstream", note: "group default, parallel_best: quad9 over DoT in 12.4 ms won, cloudflare over DoH in 15.1 ms" },
      { stage: "log", note: "status forwarded, upstream quad9, stored in the cache for 300 s" },
    ],
    answer: ["api.weather.example.  300  IN  AAAA  2001:db8:4::17"],
    micros: 12_610,
  },
  {
    id: "safesearch",
    label: "Video search with safe search on",
    client: "kids-tablet",
    address: "192.168.1.42",
    proto: "doh",
    qtype: "A",
    qname: "www.youtube.com",
    status: "rewritten",
    decidedBy: "blocking",
    steps: [
      { stage: "ingress", note: "DoH GET over HTTP/2 from 192.168.1.42" },
      { stage: "identify", note: "192.168.1.42 is client kids-tablet, member of group kids" },
      { stage: "control", note: "well-formed, one question" },
      { stage: "authoritative", note: "no local zone covers youtube.com" },
      { stage: "blocking", note: "policy kids enforces safe search: CNAME to restrict.youtube.com, TTL of the block TTL" },
      { stage: "cache", note: "restrict.youtube.com A is cached, 212 s left" },
      { stage: "log", note: "status safe search, policy kids, service youtube" },
    ],
    answer: [
      "www.youtube.com.       600  IN  CNAME  restrict.youtube.com.",
      "restrict.youtube.com.  212  IN  A      216.239.38.120",
    ],
    micros: 141,
  },
  {
    id: "truncated",
    label: "A flood from one prefix",
    client: "unknown",
    address: "203.0.113.9",
    proto: "udp",
    qtype: "ANY",
    qname: "home.example.com",
    status: "truncated",
    decidedBy: "ingress",
    steps: [
      { stage: "ingress", note: "203.0.113.0/24 is over its queries-per-minute limit: slip with TC=1, the next ones are dropped" },
      { stage: "log", note: "status rate limited, rcode NOERROR with TC, client must retry over TCP" },
    ],
    answer: [";; flags: qr tc; QUERY: 1, ANSWER: 0"],
    micros: 6,
  },
];

// Live trace rows in the hero: shorter than the scenarios, generated in a
// loop on the client.
export interface TraceRow {
  client: string;
  qtype: string;
  qname: string;
  status: Status;
  stage: StageId;
  micros: number;
}

export const TRACE: TraceRow[] = [
  { client: "robert-laptop", qtype: "A", qname: "nas.home.example.com", status: "authoritative", stage: "authoritative", micros: 38 },
  { client: "kids-tablet", qtype: "A", qname: "doubleclick.net", status: "blocked", stage: "blocking", micros: 109 },
  { client: "living-room-tv", qtype: "A", qname: "media.streaming.example", status: "cached", stage: "cache", micros: 52 },
  { client: "thermostat", qtype: "TXT", qname: "metrics.vendor.example", status: "blocked", stage: "blocking", micros: 273 },
  { client: "robert-phone", qtype: "AAAA", qname: "api.weather.example", status: "forwarded", stage: "upstream", micros: 12_610 },
  { client: "nas", qtype: "PTR", qname: "20.0.0.10.in-addr.arpa", status: "authoritative", stage: "authoritative", micros: 31 },
  { client: "doorbell", qtype: "A", qname: "events.camera.example", status: "cached", stage: "cache", micros: 47 },
  { client: "kids-laptop", qtype: "HTTPS", qname: "app-measurement.com", status: "blocked", stage: "blocking", micros: 167 },
  { client: "robert-laptop", qtype: "A", qname: "registry.k8s.example", status: "forwarded", stage: "upstream", micros: 9_420 },
  { client: "guest-phone", qtype: "AAAA", qname: "telemetry.tv-vendor.example", status: "blocked", stage: "blocking", micros: 170 },
  { client: "living-room-tv", qtype: "HTTPS", qname: "cdn.streaming.example", status: "cached", stage: "cache", micros: 44 },
  { client: "robert-phone", qtype: "SRV", qname: "_imaps._tcp.mail.example", status: "forwarded", stage: "upstream", micros: 18_090 },
  { client: "nas", qtype: "A", qname: "grafana.home.example.com", status: "authoritative", stage: "authoritative", micros: 29 },
  { client: "kids-tablet", qtype: "A", qname: "ads.game-studio.example", status: "blocked", stage: "blocking", micros: 121 },
];

export interface Register {
  id: string;
  label: string;
  title: string;
  lead: string;
  points: string[];
  figure: { value: string; unit?: string; caption: string };
  later?: string;
  link: { label: string; href: string };
}

export const REGISTERS: Register[] = [
  {
    id: "zones",
    label: "Zones",
    title: "Authoritative, with real serials.",
    lead: "Primary and forward zones come from DNSZone and DNSRecordSet objects and are answered from an in-memory tree that every replica holds.",
    points: [
      "Wildcards, CNAME and DNAME chasing, NODATA versus NXDOMAIN, per-zone query ACLs",
      "SOA serial stamping with coalescing, records that expire on their own, BIND zone file import and export",
      "AXFR and IXFR out with TSIG on every transport, NOTIFY with retries",
      "SERVFAIL, never NXDOMAIN, for a primary zone that has not loaded yet",
    ],
    figure: { value: "50–80", unit: "ms", caption: "AXFR of a 100,000-record zone" },
    later: "Secondary and stub zones, RFC 2136 updates: v0.2. DNSSEC signing: v1.0.",
    link: { label: "Zones and upstreams", href: docs("/guide/admin/07-zones-and-upstreams/") },
  },
  {
    id: "resolver",
    label: "Resolver",
    title: "Every transport. One listener set.",
    lead: "Do53 over UDP and TCP, DoT, DoH over HTTP/1.1 and HTTP/2, DoQ and DoH3, each with a certificate reloader, in front of one pipeline.",
    points: [
      "Upstream groups with parallel_best, strict, random and rtt_weighted strategies, conditional forwarding, ECS strip, forward or add",
      "Sharded TTL cache with prefetch, serve-stale (RFC 8767) and in-flight coalescing; it is restored after a restart",
      "SO_REUSEPORT listeners on every core: 53 % more UDP throughput",
      "Per-prefix rate limits that slip TC=1 before they drop",
    ],
    figure: { value: "5", caption: "transports: Do53, DoT, DoH, DoQ, DoH3" },
    link: { label: "Network setup", href: docs("/start/network/") },
  },
  {
    id: "blocking",
    label: "Blocking",
    title: "Lists, groups, schedules. No allocations.",
    lead: "A clean-room parser compiles hosts, domain, adblock and regex lists into one matcher chain. Allow wins, @@ exceptions apply, and answers are checked through their CNAME chain.",
    points: [
      "Client groups by IP, CIDR or DoT/DoH client ID, each with merged policies and weekday schedules",
      "150 blocked services in 14 categories, and safe search for the search engines and video sites that document it",
      "A block page that says which list blocked a site, with a request-access form that reaches an admin",
      "Answers with zeroIP, NXDOMAIN, a custom IP or REFUSED, tagged with EDE 15 or 17",
    ],
    figure: { value: "1.14", unit: "µs", caption: "p99 lookup against 1,000,000 suffix rules, 0 allocations" },
    link: { label: "Lists, groups and policies", href: docs("/guide/admin/05-groups-clients-policies/") },
  },
  {
    id: "identity",
    label: "Identity",
    title: "OIDC logins. Real roles.",
    lead: "Sign in with your identity provider, or with local accounts on a single box. Every write lands in the audit trail with who did it and which permission allowed it.",
    points: [
      "OIDC with a backend-for-frontend and PKCE; local users with Argon2id and lockouts",
      "Roles viewer, auditor, operator, zone-admin and admin, scoped per zone",
      "API tokens, service accounts, and Kubernetes ServiceAccount tokens checked with SubjectAccessReview",
      "The chart's aggregated ClusterRoles are generated from the same permission file",
      "Roles, OIDC and the audit trail need a license key; a free personal key covers up to 3 nodes",
    ],
    figure: { value: "0", caption: "OIDC logins that become admin without a role binding" },
    link: { label: "People, roles and tokens", href: docs("/guide/admin/09-people-and-tokens/") },
  },
  {
    id: "kubernetes",
    label: "Kubernetes",
    title: "The apiserver is the replicated store.",
    lead: "DNSZone, DNSRecordSet, BlockList, ClientGroup and Policy are CRDs. Every replica watches them and serves from memory, so an apiserver outage does not stop answers.",
    points: [
      "Helm charts kudurru and kudurru-crds, rendered and checked across 42 value combinations",
      "Each object records who manages it: the API, a file, an integration or Kubernetes. Two sources of truth cannot happen",
      "An external-dns webhook provider, and a read-only watch of DNSEndpoint",
      "The same YAML works in Docker: kudurru apply -f zone.yaml",
    ],
    figure: { value: "42", caption: "chart value combinations rendered and checked with kubeconform on every change" },
    link: { label: "Helm guide", href: docs("/deploy/helm/") },
  },
  {
    id: "edge",
    label: "Edge and VIP",
    title: "dnsdist in front. A VIP that follows health.",
    lead: "kudurru supervises the programs that do this best instead of rewriting them. dnsdist and keepalived stay unmodified, in their own containers, and kudurru renders, checks and runs them.",
    points: [
      "kudurru edge run renders dnsdist's configuration, runs --check-config before every swap and tracks backends by health",
      "PROXYv2 with an authenticated TLV keeps the real client IP for groups and the query log",
      "kudurru vip run runs keepalived with VRRPv3 unicast, auth_hmac and nopreempt",
      "Serve from memory; degrade, never stop",
    ],
    figure: { value: "3", caption: "nodes in the compose HA profile, one VIP" },
    link: { label: "Compose HA", href: docs("/deploy/compose-ha/") },
  },
];

export const SHOTS: { id: string; label: string; file: string; caption: string }[] = [
  { id: "dashboard", label: "Dashboard", file: "dashboard.png", caption: "Two nodes, 24 hours: queries by answer type and by client." },
  { id: "query-log", label: "Query log", file: "query-detail.png", caption: "Every row opens to the list, rule, cache state and node behind the answer." },
  { id: "long-term", label: "Long-term", file: "long-term.png", caption: "Rollups for weeks and months without keeping every query." },
  { id: "zones", label: "Zones", file: "zone-detail.png", caption: "A primary zone with its serial, records, BIND import and export, and NOTIFY." },
  { id: "upstreams", label: "Upstreams", file: "upstreams.png", caption: "Upstream groups with their strategy, servers, health and round-trip times." },
  { id: "nodes", label: "Nodes", file: "nodes.png", caption: "Every node with its readiness, version and role, and the state of dnsdist and the VIP." },
];

export interface Shape {
  name: string;
  truth: string;
  answers: string;
  writes: string;
  release: string;
}

export const SHAPES: Shape[] = [
  { name: "docker run, compose", truth: "state.db on a volume", answers: "one node", writes: "n/a", release: "v0.1" },
  { name: "Kubernetes, several replicas", truth: "CRDs in the apiserver", answers: "every pod, through apiserver outages", writes: "apiserver HA", release: "v0.1" },
  { name: "Kubernetes, one node (Pi-hole on k3s)", truth: "state.db on a PVC", answers: "one pod", writes: "n/a", release: "v0.1" },
  { name: "Compose HA, static twins", truth: "the same file-managed config on each node", answers: "all nodes, VIP", writes: "files only", release: "v0.1" },
  { name: "Compose HA, follower mode", truth: "the primary's state.db", answers: "all nodes", writes: "primary, fenced promote", release: "v0.2" },
  { name: "Compose HA, raft", truth: "replicated state.db", answers: "all nodes", writes: "any node", release: "v1.0" },
];

export interface Install {
  id: string;
  label: string;
  lines: string[];
  note: string;
  href: string;
}

export function installs(version: string): Install[] {
  return [
    {
      id: "docker",
      label: "Docker",
      lines: [
        "mkdir -p kudurru/secrets && cd kudurru",
        "openssl rand -base64 32 > secrets/encryption_key",
        "openssl rand -base64 18 > secrets/admin_password",
        "docker run -d --name kudurru --restart unless-stopped \\",
        "  -p 53:53/udp -p 53:53/tcp -p 8080:8080 \\",
        "  -v kudurru-data:/var/lib/kudurru \\",
        '  -v "$PWD/secrets:/run/secrets:ro" \\',
        "  -e KUDURRU_ENCRYPTION_KEY_FILE=/run/secrets/encryption_key \\",
        "  -e KUDURRU_ADMIN_PASSWORD_FILE=/run/secrets/admin_password \\",
        `  ${IMAGE}:${version}`,
        "dig @127.0.0.1 example.com",
      ],
      note: "The dashboard is on port 8080. Sign in as admin with the password in secrets/admin_password.",
      href: docs("/deploy/docker/"),
    },
    {
      id: "helm",
      label: "Helm",
      lines: [
        `helm install kudurru-crds ${CHARTS}/kudurru-crds \\`,
        `  --version ${version}`,
        `helm install kudurru ${CHARTS}/kudurru --version ${version} \\`,
        "  --set replicaCount=2",
        "kubectl apply -f zone.yaml",
        "kubectl get dnszones",
      ],
      note: "The CRD chart keeps the CRDs on uninstall, so removing kudurru never deletes your zones.",
      href: docs("/deploy/helm/"),
    },
    {
      id: "yaml",
      label: "zone.yaml",
      lines: [
        "apiVersion: kudurru.io/v1alpha1",
        "kind: DNSZone",
        "metadata:",
        "  name: home-example-com",
        "spec:",
        "  zoneName: home.example.com.",
        "  type: primary",
        "  nameservers: [ns1.home.example.com., ns2.home.example.com.]",
        "  policy:",
        "    zoneTransfer: useNetworkACL",
        '    transferACL: ["10.0.0.0/8"]',
        "    notify: zoneNameServers",
      ],
      note: "kubectl apply in a cluster, kudurru apply -f against a Docker instance: the same document.",
      href: docs("/reference/crds/"),
    },
  ];
}

export interface Release {
  version: string;
  name: string;
  meaning: string;
  items: string[];
}

export const ROADMAP: Release[] = [
  {
    version: "v0.1",
    name: "stone",
    meaning: "The stone itself: one record of authority.",
    items: [
      "Resolver with DoT, DoH, DoQ and DoH3",
      "Primary and forward zones, AXFR and IXFR out",
      "Blocking with client groups, services and safe search",
      "Dashboard with long-term views, OIDC and RBAC (a free personal key unlocks them)",
      "CRDs and Helm, compose HA with dnsdist and keepalived",
    ],
  },
  {
    version: "v0.2",
    name: "clay copies",
    meaning: "The landholder's copies: secondaries and followers.",
    items: [
      "Secondary and stub zones, RFC 2136",
      "DNSSEC validation, RPZ feed import",
      "Cloudflare split-horizon sink, UniFi inventory",
      "Follower-mode HA",
      "dnstap and tracing",
    ],
  },
  {
    version: "v1.0",
    name: "temple",
    meaning: "Where the original is kept.",
    items: [
      "DNSSEC signing, catalog zones",
      "RPZ publishing",
      "Raft store: writes on any node",
      "ACME, a Pi-hole API façade",
      "API freeze",
    ],
  },
];

export const FOOTER: { title: string; links: NavLink[] }[] = [
  {
    title: "Docs",
    links: [
      { label: "Get started", href: docs("/start/") },
      { label: "Guide", href: docs("/guide/") },
      { label: "Deploy", href: docs("/deploy/") },
      { label: "Reference", href: docs("/reference/") },
      { label: "Architecture", href: docs("/architecture/") },
    ],
  },
  {
    title: "Move over",
    links: [
      { label: "From Pi-hole", href: docs("/migrate/pihole/") },
      { label: "From AdGuard Home", href: docs("/migrate/adguard-home/") },
    ],
  },
  {
    title: "Project",
    links: [
      { label: "Source on GitHub", href: REPO_URL },
      { label: "License and keys", href: "/license/" },
      { label: "Releases", href: `${REPO_URL}/releases` },
      { label: "Changelog", href: docs("/project/changelog/") },
      { label: "Security", href: docs("/project/security/") },
      { label: "Contributing", href: docs("/project/contributing/") },
    ],
  },
];
