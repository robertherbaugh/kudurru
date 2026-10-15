// Everything the pages say. Claims here follow CHANGELOG.md (0.1.0), the
// docs/architecture, docs/deploy and bench/baseline.txt; when a feature moves between releases,
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
// The license text: our own copy, rendered from the repository's LICENSE
// (/license/text/), the docs' copy, and the repository source.
export const LICENSE_TEXT_PATH = "/license/text/";
export const LICENSE_TEXT_FILE = "/license/text/Kudurru-Community-License.txt";
export const LICENSE_SOURCE_URL = `${REPO_URL}/blob/main/LICENSE`;
export const LICENSE_REQUIRED_NOTICE = "Required Notice: Copyright 2026 Robert Herbaugh (https://kudurru.io)";
// Without a key one Policy can exist: the Policy "default" that the bootstrap
// seeds (internal/license CommunityPolicies). A key lifts the limit.
export const LICENSE_FREE_POLICIES = 1;

// Enterprise overview: keep the feature and pricing summary in step with
// docs/enterprise.md. Annual list prices are in USD; the Order Form sets scope.
export const ENTERPRISE_CONTACT_URL = `mailto:${LICENSE_CONTACT}?subject=${encodeURIComponent("kudurru Enterprise")}`;
export const COMMERCIAL_PLANS = {
  core: { name: "Commercial Core", platform: 99, perNode: 99 },
  enterprise: { name: "Enterprise", platform: 499, perNode: 299 },
} as const;
export const VOLUME_PRICING = { discountFrom: 25, quoteFrom: 50, discount: 0.1 } as const;
// Public starting rate from the commercial pricing proposal. Support needs
// a separate agreement; keep docs/enterprise.md in step with this amount.
export const SUPPORT_STARTING_ANNUAL_USD = 1200;
export const COMMERCIAL_COMPARISON = [
  { what: "Eligible noncommercial use", community: "Yes", core: "Yes", enterprise: "Yes" },
  { what: "Business and government production rights", community: "No", core: "Yes", enterprise: "Yes" },
  { what: "Authoritative DNS and forwarding resolver", community: "Yes", core: "Yes", enterprise: "Yes" },
  { what: "Blocking and encrypted DNS", community: "Yes", core: "Yes", enterprise: "Yes" },
  { what: "Multiple nodes", community: `Up to ${LICENSE_FREE_NODES} with a free key`, core: "Licensed count", enterprise: "Licensed count" },
  { what: "OIDC and advanced RBAC", community: "With a free key", core: "No", enterprise: "Included" },
  { what: "Audit exports and long-term analytics", community: "With a free key", core: "No", enterprise: "Included" },
  { what: "Advanced policies and unblock requests", community: "With a free key", core: "No", enterprise: "Included" },
  { what: "dnsdist and keepalived integration", community: "With a free key", core: "No", enterprise: "Included" },
  { what: "Enterprise integrations", community: "With a free key", core: "No", enterprise: "Included" },
] as const;
export const ENTERPRISE_FEATURES = [
  {
    title: "Identity and access",
    body: "OIDC single sign-on, role bindings, zone-scoped roles and Kubernetes RBAC. Give each person and service the permissions they need.",
    href: "/guide/admin/09-people-and-tokens/",
  },
  {
    title: "An audit trail you can export",
    body: "Review stored events, see who changed what, and export the audit trail for your own review and retention workflows.",
    href: "/architecture/observability/#audit",
  },
  {
    title: "Statistics beyond 24 hours",
    body: "Use long-term rollups to review DNS activity over weeks and months without retaining every individual query.",
    href: "/guide/admin/03-watch-the-network/",
  },
  {
    title: "Policies for different teams",
    body: "Create policies beyond the default for client groups and schedules. Let users request access from the block page and review those requests in the dashboard.",
    href: "/guide/admin/05-groups-clients-policies/",
  },
  {
    title: "An edge with a floating IP",
    body: "Use the kudurru supervisors for dnsdist load balancing and keepalived virtual IP failover. See backend health and VIP ownership alongside your nodes.",
    href: "/deploy/compose-ha/",
  },
  {
    title: "DNS records from Kubernetes",
    body: "Connect external-dns through the webhook provider to manage DNSRecordSet objects. Cloudflare and UniFi integrations remain on the roadmap.",
    href: "/architecture/configuration-and-store/",
  },
] as const;

export interface LicenseRow {
  what: string;
  community: string;
  personal: string;
  business: string;
}

export const LICENSE_ROWS: LicenseRow[] = [
  { what: "DNS answering, blocking, zones, the block page, the dashboard, short-term statistics", community: "Yes", personal: "Yes", business: "Yes" },
  { what: "Nodes in one cluster", community: "1", personal: `Up to ${LICENSE_FREE_NODES}`, business: "As agreed" },
  { what: "Policies", community: `${LICENSE_FREE_POLICIES}, the default policy`, personal: "Unlimited", business: "Unlimited" },
  { what: "Roles beyond the built-in administrator, zone-scoped roles, Kubernetes RBAC", community: "No", personal: "Yes", business: "Yes*" },
  { what: "OIDC single sign-on", community: "No", personal: "Yes", business: "Yes*" },
  { what: "The audit trail and its export", community: "No", personal: "Yes", business: "Yes*" },
  { what: "Long-term statistics (anything older than 24 hours)", community: "No", personal: "Yes", business: "Yes*" },
  { what: "Unblock requests from the block page", community: "No", personal: "Yes", business: "Yes*" },
  { what: "The dnsdist and keepalived edge", community: "No", personal: "Yes", business: "Yes*" },
  { what: "Integrations, such as the external-dns webhook", community: "No", personal: "Yes", business: "Yes*" },
  { what: "Business or government production use", community: "No", personal: "No", business: "As granted by the agreement" },
];

export const TAGLINE = "authority over every zone.";
export const DESCRIPTION =
  "kudurru (koo-DOO-roo) is a Kubernetes-native DNS server that also runs as one container: authoritative zones, a forwarding resolver over every encrypted transport, blocking with client groups, a dashboard that explains every answer. Core software under the Kudurru Community License: free for eligible personal and nonprofit use; business and government production needs an agreement.";

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
  { label: "Move over", href: "/move-over/" },
  // A page of its own: not prefixed with the home page path like the anchors.
  { label: "Enterprise", href: "/enterprise/" },
  { label: "License", href: "/license/" },
];

// The README's lineage table: the ideas that inspired the design.
export interface Lineage {
  idea: string;
  from: { name: string; href: string }[];
  meaning: string;
}

export const LINEAGE: Lineage[] = [
  {
    idea: "API-first design and one binary",
    from: [{ name: "Gravity", href: "https://github.com/BeryJu/gravity" }],
    meaning: "Every node runs the resolver, API and dashboard. Kubernetes replicas share configuration through the apiserver.",
  },
  {
    idea: "Authoritative DNS and encrypted transports",
    from: [{ name: "Technitium", href: "https://technitium.com/dns/" }],
    meaning: "Primary and forward zones, SOA serial management, AXFR and IXFR out, NOTIFY out, TSIG on UDP/TCP/DoT, DoT/DoH/DoQ/DoH3 and per-zone query ACLs.",
  },
  {
    idea: "A clear view of DNS activity",
    from: [{ name: "Pi-hole", href: "https://pi-hole.net/" }],
    meaning: "Stat tiles, 24-hour charts, a query log that says why, one-click allow/deny and views across nodes.",
  },
  {
    idea: "Powerful blocking",
    from: [
      { name: "blocky", href: "https://github.com/0xERR0R/blocky" },
      { name: "AdGuard Home", href: "https://github.com/AdguardTeam/AdGuardHome" },
    ],
    meaning: "hosts, domain, adblock and regex lists; client groups; per-group policies and schedules; zero-allocation matchers with a benchmark regression gate.",
  },
  {
    idea: "Load balancing at the edge",
    from: [{ name: "dnsdist", href: "https://dnsdist.org" }],
    meaning: "An optional kudurru supervisor renders, checks and runs dnsdist. Authenticated PROXYv2 preserves client IPs on the backend hop.",
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
    title: "OIDC sign-in and role-based access control",
    body: "viewer, auditor, operator, zone-admin and admin, scoped per zone. A login with no role binding is refused, never silently admin. Needs a license key (free for personal use).",
  },
  {
    title: "An integration framework",
    body: "The external-dns webhook provider writes records in v0.1.0. The Cloudflare split-horizon sink and UniFi inventory source are planned for v0.2. Integrations need a license key (free for eligible personal use).",
  },
];

// The v0.1.0 query path: seven pipeline stages, then the log observer.
// pipeline.Order also reserves local, rewrite, dnssec and postprocess.
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
  { id: "ingress", does: "Checks query shape and per-prefix rate limits; answers CHAOS and health probes" },
  { id: "identify", does: "Matches the source IP or CIDR to client groups and their merged policies" },
  { id: "control", does: "Drops malformed queries" },
  { id: "authoritative", does: "Answers names in local zones from an in-memory tree" },
  { id: "blocking", does: "Evaluates the client's merged policies: lists, services, safe search" },
  { id: "cache", does: "Sharded TTL cache with prefetch, serve-stale and in-flight coalescing" },
  { id: "upstream", does: "Forwards to the client's upstream group over DoT, DoH, DoQ or Do53" },
  { id: "log", does: "After the chain returns, the observer records the answer and its reason" },
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
      "AXFR and IXFR out; TSIG on UDP, TCP and DoT; NOTIFY out with retries",
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
      "Client groups by IP and CIDR, with merged policies and weekday schedules (a policy beyond the default needs a license key, free for eligible personal use)",
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
    title: "OIDC sign-in. Scoped roles.",
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
      "Helm charts kudurru and kudurru-crds, with a render matrix checked against the binary's configuration schema and kubeconform",
      "Each object records its owner: the API, a file, an integration or Kubernetes. Writes from another source are rejected",
      "An external-dns webhook provider that writes DNSRecordSet objects (needs a license key, free for eligible personal use)",
      "The same YAML works in Docker: kudurru apply -f zone.yaml",
    ],
    figure: { value: "2", caption: "Helm charts, with configuration and Kubernetes schema checks" },
    link: { label: "Helm guide", href: docs("/deploy/helm/") },
  },
  {
    id: "edge",
    label: "Edge and VIP",
    title: "dnsdist in front. A VIP that follows health.",
    lead: "kudurru supervises the programs that do this best instead of rewriting them. dnsdist and keepalived stay unmodified, in their own containers, and kudurru renders, checks and runs them. The supervisors need a license key; a free personal key covers up to 3 nodes.",
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
  { id: "long-term", label: "Long-term", file: "long-term.png", caption: "Rollups for weeks and months without keeping every query (a license key unlocks periods over 24 hours)." },
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
  { name: "docker run, compose", truth: "state.db on a volume", answers: "one node", writes: "n/a", release: "v0.1.0" },
  { name: "Kubernetes, several replicas", truth: "CRDs in the apiserver", answers: "every pod, through apiserver outages", writes: "apiserver HA", release: "v0.1.0" },
  { name: "Kubernetes, one node (Pi-hole on k3s)", truth: "state.db on a PVC", answers: "one pod", writes: "n/a", release: "v0.1.0" },
  { name: "Compose HA, static twins", truth: "the same file-managed config on independent SQLite nodes", answers: "all nodes, VIP", writes: "distribute files to every node", release: "v0.1.0" },
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
      note: "Open https://<host>:8080/ and sign in as admin with the password in secrets/admin_password. The first start creates a self-signed certificate; keep a safe copy of the encryption key.",
      href: docs("/deploy/docker/"),
    },
    {
      id: "helm",
      label: "Helm",
      lines: [
        "kubectl create namespace kudurru-system",
        "kubectl label namespace kudurru-system pod-security.kubernetes.io/enforce=restricted",
        `helm install kudurru-crds ${CHARTS}/kudurru-crds \\`,
        `  --version ${version}`,
        `helm install kudurru ${CHARTS}/kudurru --version ${version} \\`,
        "  --namespace kudurru-system --set replicaCount=1",
        "kubectl apply -f zone.yaml",
        "kubectl get dnszones",
      ],
      note: "Start with one replica and no license key. For more replicas, add a key as described in the Helm guide. The separate CRD chart keeps your zones when the server chart is uninstalled.",
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
    version: "v0.1.0",
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
      { label: "Compare features", href: "/move-over/" },
      { label: "From Pi-hole", href: docs("/migrate/pihole/") },
      { label: "From AdGuard Home", href: docs("/migrate/adguard-home/") },
      { label: "From Technitium", href: docs("/migrate/technitium/") },
    ],
  },
  {
    title: "Project",
    links: [
      { label: "Source on GitHub", href: REPO_URL },
      { label: "Enterprise", href: "/enterprise/" },
      { label: "License and keys", href: "/license/" },
      { label: "Releases", href: `${REPO_URL}/releases` },
      { label: "Changelog", href: docs("/project/changelog/") },
      { label: "Security", href: docs("/project/security/") },
      { label: "Contributing", href: docs("/project/contributing/") },
    ],
  },
];

// Migration comparisons describe the 0.1.0 implementation, not schema-only
// features. Keep site/content/migrate in step; sources are linked below.
export const MIGRATIONS = [
  {
    slug: "pihole",
    name: "Pi-hole",
    summary: "Keep the lists and local names you trust. Add authoritative zones, encrypted DNS listeners and Kubernetes CRDs.",
    mapping: "Groups become client groups and policies. Adlists become block lists; local DNS records become zone records.",
    check: "Move DHCP to your router first if Pi-hole serves it. Translate regex extensions and reserve device addresses.",
    source: "https://docs.pi-hole.net/",
  },
  {
    slug: "adguard-home",
    name: "AdGuard Home",
    summary: "Bring your DNS filters, client rules and encrypted upstreams into a server with explicit object ownership and shared Kubernetes configuration.",
    mapping: "Filters become block and allow lists. Persistent clients become client groups; per-client settings become policies.",
    check: "Review rule modifiers, global exceptions and DNS rewrites. AdGuard's full filtering syntax is not supported in v0.1.0.",
    source: "https://github.com/AdguardTeam/AdGuardHome/wiki/Configuration",
  },
  {
    slug: "technitium",
    name: "Technitium",
    summary: "Move primary zones, conditional forwarding and blocking to a Go binary with a dashboard, CLI and Kubernetes API objects.",
    mapping: "Export primary zones as standard zone files. Recreate conditional forwarders, access controls and blocking policies explicitly.",
    check: "Inventory DNS Apps, recursive resolution, DNSSEC and secondary zones. These need a separate service or a later kudurru release.",
    source: "https://technitium.com/dns/",
  },
] as const;

export const MIGRATION_COMPARISON = [
  { feature: "DNS filtering", pihole: "Hosts, domains and regex; group assignments", adguard: "Adblock DNS rules, allowlists and client settings", technitium: "Allowed/blocked zones and lists; Advanced Blocking app", kudurru: "Hosts, domains, adblock subset and RE2; client-group policies" },
  { feature: "Local names and zones", pihole: "Local DNS and CNAME records", adguard: "DNS rewrites", technitium: "Primary, secondary, stub, forwarder and catalog zones", kudurru: "Primary and forward zones; BIND import/export; AXFR/IXFR out" },
  { feature: "Resolution", pihole: "Forwards to upstreams; recursion with a separate resolver", adguard: "Forwards to upstreams", technitium: "Full recursive resolver or forwarding", kudurru: "Forwards to named upstream groups; conditional forwarding" },
  { feature: "DNSSEC", pihole: "Optional validation", adguard: "Requests DNSSEC data from upstreams", technitium: "Validation and authoritative signing", kudurru: "No validation or signing in v0.1.0" },
  { feature: "DHCP", pihole: "Built in", adguard: "Built in", technitium: "Built in", kudurru: "Use your router or a separate DHCP server" },
  { feature: "Encrypted DNS listeners", pihole: "Use a separate proxy", adguard: "DoT, DoH and DoQ", technitium: "DoT, DoH and DoQ", kudurru: "DoT, DoH, DoQ and DoH3" },
  { feature: "Multiple servers", pihole: "Separate instances; external sync tools", adguard: "Separate instances; external sync tools", technitium: "Built-in clustering", kudurru: "Shared Kubernetes store; Compose HA uses shared object files" },
  { feature: "Kubernetes configuration", pihole: "Container configuration; no native CRDs", adguard: "Container configuration; no native CRDs", technitium: "Container configuration; no native CRDs", kudurru: "Native CRDs, Helm and external-dns webhook" },
  { feature: "Identity and access", pihole: "Local dashboard login", adguard: "Local users", technitium: "Roles, OIDC and LDAP", kudurru: "Local users; OIDC and scoped roles with a key" },
  { feature: "Migration input", pihole: "Teleporter backup", adguard: "Configuration and working directory", technitium: "Configuration backup and zone export", kudurru: "Manual configuration; standard BIND zone import only" },
  { feature: "License", pihole: "Open source; component licenses apply", adguard: "GPL-3.0", technitium: "GPL-3.0", kudurru: "Source available; eligible community use free, commercial production by agreement" },
] as const;
