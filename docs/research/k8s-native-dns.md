> **Research note, 2026-10-10.** Point-in-time study produced during the kudurru design phase and cited by [docs/architecture.md](../architecture.md). It describes other projects for analysis only; nothing in it is to be ported into kudurru (several of them are GPL/EUPL). Items marked UNVERIFIED were not confirmed against a primary source. Verify before relying on a version number or API detail.

# Research: What "Kubernetes-native DNS server" should mean for kudurru

Key: `k8s-native-dns` — Date: 2026-10-10 — Author: research subagent

Scope: CoreDNS plugin architecture and its `kubernetes` plugin; external-dns (provider
interface, webhook provider, DNSEndpoint/DNSRecord CRDs, Cloudflare provider); k8s_gateway;
Kuadrant dns-operator; PowerDNS-Operator and other "DNS operator" projects; kubebuilder /
controller-runtime patterns (CRDs, conditions, finalizers, webhooks, leader election);
deployment mechanics (LoadBalancer mixed protocol, MetalLB/kube-vip, hostNetwork, PSS,
port 53 as non-root, probes, PDB, Helm/OCI, kustomize, OLM v1); how Gravity, Technitium,
AdGuard Home, blocky, Pi-hole and CoreDNS are shipped to Kubernetes today; and a
recommendation for kudurru's CRD set, source-of-truth model, and shipped manifests.

Method: `git clone --depth 1` of all primary repos into `/tmp/kudurru-research/k8s-native-dns/`
(read only, nothing executed), plus kubernetes.io / helm.sh / metallb.io / kube-vip.io docs.
Anything I could not confirm from a primary source is marked **UNVERIFIED**.

---

## 0. Sources (versions, licenses, commits as of 2026-10-10)

| Project | Latest tag | License | Clone commit (date) | URL |
|---|---|---|---|---|
| CoreDNS | v1.14.7 (`coremain/version.go`) | Apache-2.0 | 123829d (2026-10-09) | https://github.com/coredns/coredns |
| CoreDNS Helm chart | chart 1.48.2 / app 1.14.7 | Apache-2.0 | 807de5f (2026-10-01) | https://github.com/coredns/helm |
| external-dns | v0.23.0 (chart 1.23.0) | Apache-2.0 | 33ef223 (2026-10-05) | https://github.com/kubernetes-sigs/external-dns |
| k8s_gateway (ori-edge) | v0.4.0 — **unmaintained**, README says use fork | Apache-2.0 | 812f963 (2025-07-10) | https://github.com/ori-edge/k8s_gateway |
| k8s_gateway (fork) | v1.9.2 (tags only; not cloned) | Apache-2.0 (UNVERIFIED for fork) | — | https://github.com/k8s-gateway/k8s_gateway |
| Kuadrant dns-operator | v0.18.0 | Apache-2.0 | 4f18ae9 (2026-09-25) | https://github.com/Kuadrant/dns-operator |
| PowerDNS-Operator | v0.6.0 | Apache-2.0 | 9312b88 (2026-09-21) | https://github.com/powerdns-operator/PowerDNS-Operator |
| controller-runtime | v0.25.2 | Apache-2.0 | 117d60f (2026-10-05) | https://github.com/kubernetes-sigs/controller-runtime |
| kubebuilder | v4.16.0 | Apache-2.0 | 4c35c50 (2026-10-08) | https://github.com/kubernetes-sigs/kubebuilder |
| kube-vip | v1.2.4 | Apache-2.0 | 8d3fa79 (2026-10-09) | https://github.com/kube-vip/kube-vip |
| MetalLB | v0.16.1 (tags only) | Apache-2.0 | — | https://metallb.io |
| Gravity (BeryJu) | v0.33.0 | **GPL-3.0** (LICENSE in repo) | b873d1f (2026-10-09) | https://github.com/BeryJu/gravity |
| blocky | v0.35.0 | Apache-2.0 | ca450f8 (2026-10-09) | https://github.com/0xERR0R/blocky |
| Technitium DnsServer | v15.6.0 (tags only) | GPL-3.0 (UNVERIFIED here; see other report) | — | https://github.com/TechnitiumSoftware/DnsServer |
| AdGuard Home | v0.107.79 | GPL-3.0 (UNVERIFIED here) | — | https://github.com/AdguardTeam/AdGuardHome |
| Pi-hole | v6.4.3 | EUPL-1.2 (UNVERIFIED here) | — | https://github.com/pi-hole/pi-hole |
| gabe565/charts (adguard-home chart 0.3.25) | — | (chart repo; bjw-s common lib 1.5.1) | 7849a42 (2025-02-20) | https://github.com/gabe565/charts |
| truecharts/public (blocky 29.x common, pihole 22.15.0, adguard-home 13.10.0) | — | — | 14f2248 (2026-10-10) | https://github.com/truecharts/public |
| operator-controller (OLM v1) | v1.12.0 (tags only) | Apache-2.0 | — | https://github.com/operator-framework/operator-controller |

Kubernetes facts verified on kubernetes.io (docs currently at v1.37):
- `MixedProtocolLBService`: alpha 1.20, beta 1.24, **GA 1.26**, gate removed after 1.27 (feature-gates-removed table). The Service page: "This is a stable feature in Kubernetes, and has been since the 1.26 release… the associated feature gate has been removed." Cloud providers may still restrict protocol mixes.
- `spec.loadBalancerClass`: stable since v1.24; value is label-style (`example.com/internal-vip`); immutable once set; default cloud LB ignores Services that set it.
- `net.ipv4.ip_unprivileged_port_start` is a **safe sysctl since 1.22** and is in the Pod Security "Baseline" allowed list; **`net.*` sysctls are not allowed with `hostNetwork: true`**.
- Pod Security "Restricted": containers must `drop: [ALL]` and may add back **only `NET_BIND_SERVICE`**; `runAsNonRoot: true`; `allowPrivilegeEscalation: false`; `seccompProfile: RuntimeDefault|Localhost`. Baseline forbids `hostNetwork` and recommends no `hostPort`.
- gRPC container probes: GA 1.27 (`grpc.port`, optional `service`); TLS mode for gRPC probes is **alpha in 1.37** (`GRPCContainerProbeTLS`).
- `trafficDistribution`: `PreferSameZone`, `PreferSameNode` (1.37); `PreferClose` deprecated alias.
- ValidatingAdmissionPolicy (CEL, in-apiserver): **GA 1.30**; docs note "Type Checking does not apply to CRDs" (still works, just no static type-check).

---

## 1. Thesis: what "Kubernetes-native" should mean for kudurru

Three distinct things get called "Kubernetes-native DNS", and kudurru should do all three but keep them separable:

1. **Dataplane that is a good k8s citizen**: ships as an OCI image that runs non-root on port 53 under the Restricted PSS, exposes `/health` + `/ready` + `/metrics`, honours SIGTERM with lame-duck, scales horizontally behind a Service, tolerates API-server unavailability (serve from last cache), and is installable via Helm (OCI) and kustomize. CoreDNS and its Helm chart are the reference implementation.
2. **Kubernetes as a *source* of DNS data** (the CoreDNS `kubernetes`/`k8s_external`/k8s_gateway model): watch Services, EndpointSlices, Ingress, Gateway API routes and synthesize records for cluster-internal (`svc.cluster.local`) and external ("expose my Ingress hostnames on the LAN") zones. This is informer-driven and read-only against the API server; every replica does it independently, no leader election needed.
3. **Kubernetes API as the *configuration surface*** (the operator model: Kuadrant, PowerDNS-Operator, external-dns `DNSEndpoint`): zones, records, blocklists, policies, client groups and integrations are CRDs; `kubectl apply` / GitOps is a first-class way to configure the server; the server reconciles CRDs into its store, writes `status.conditions`, and uses leader election for the parts that have side effects (publishing to Cloudflare, writing status).

The hard requirement "also deployable as a single Docker container" forces one more design decision (section 10): the Go types that define a `DNSZone`, `BlockList`, etc. must be the *same* whether they are persisted by the API server (CRDs) or by kudurru's own embedded store, and the reconciler must not care which. That is exactly how Gravity avoids a k8s dependency (its own etcd + roles) and exactly where Technitium struggles (binary config files, no declarative surface — the community built `technitium-configurator` and a ConfigMap sidecar controller just to get one).

---

## 2. CoreDNS: plugin architecture and the `kubernetes` plugin

### 2.1 Plugin chain
- Plugins are compiled in at build time; `plugin.cfg` is an *ordered* list ("Ordering is VERY important. Every plugin will feel the effects of all other plugins below (after) them"). Current order (abridged): `root, metadata, geoip, cancel, proxyproto, quic, grpc_server, https, https3, timeouts, multisocket, reload, nsid, bufsize, bind, debug, trace, ready, health, pprof, shed, prometheus, errors, log, dnstap, local, dns64, siit, any, chaos, loadbalance, tsig, rewrite, autopath, acl, cache, header, dnssec, tls, minimal, template, transfer, dynupdate, hosts, route53, azure, clouddns, k8s_external, kubernetes, file, auto, secondary, etcd, loop, forward, grpc, erratic, whoami, on, sign, view, nomad`.
- The whole plugin contract is two methods (`plugin/plugin.go`):
  ```go
  type Handler interface {
      ServeDNS(context.Context, dns.ResponseWriter, *dns.Msg) (int, error)
      Name() string
  }
  type Plugin func(Handler) Handler   // middleware: wraps next
  ```
  Return-code convention: if `ServeDNS` returns SERVFAIL/REFUSED/FORMERR/NOTIMP CoreDNS assumes *no* reply was written; any other rcode means the plugin wrote the response. `plugin.NextOrFailure` wraps the writer in a `pluginWriter` so metrics can attribute which plugin answered.
- Cross-cutting plugin interfaces are small optional interfaces discovered by type assertion: `ready.Readiness` (`Ready() bool`), `transfer.Transferer` (`Transfer(zone string, serial uint32) (<-chan []dns.RR, error)`), `health` (process-wide), `metadata` providers. This is the cleanest "capability interface" pattern to copy for kudurru's internal plugin system.
- Server: one Caddy "server block" per zone+port; `core/dnsserver/server.go` listens with `reuseport.Listen("tcp")` and `reuseport.ListenPacket("udp")`; the `multisocket` plugin opens N SO_REUSEPORT sockets (default GOMAXPROCS) so the kernel load-balances across goroutine pools. `bind` selects addresses/interfaces. `reload` re-reads the Corefile on SHA512 change with jitter so many replicas don't reload at once.
- Health/readiness split (important for k8s probes): `health` is process-wide on `:8080/health` with `lameduck DURATION` (delays shutdown while still answering 200); `ready` on `:8181/ready` returns 503 with the list of not-ready plugins until every plugin implementing `Readiness` says ready (`monitor until-ready|continuously`). The kubernetes plugin's readiness is literally `return k.APIConn.HasSynced()`.
- Newer, relevant plugins in 1.14.x: `dynupdate` (RFC 2136 UPDATE with TSIG, persisted in an embedded **bbolt** DB — "no etcd server or container is required"; one writable zone per server block; NOTIFY on change; cache bypass for dynamic zones), `tls { acme DOMAIN... }` (DNS-01 ACME; "the DNS-01 challenge state is local to one CoreDNS process … validation queries must be routed to the replica performing the ACME operation" — a multi-replica caveat kudurru will also hit), `transfer` (AXFR/IXFR-with-AXFR-fallback for any `Transferer`, NOTIFY `to ADDRESS...`, pair with `acl` to restrict XFR), `view`, `multicluster` (MCS-API ServiceImport), and the new `zonal` option (`us-west-2a.pin._zone.db.prod.svc.cluster.local`).

### 2.2 How the `kubernetes` plugin watches the cluster (`plugin/kubernetes/controller.go`)
- One `dnsControl` holds **four informers** (plus two more for MCS): Services, Pods (only when `pods verified`), EndpointSlices (`discovery/v1`, transformed into a compact `object.Endpoints`), Namespaces. Each is `object.NewIndexerInformer(ListWatch, &api.Service{}, handlers, Indexers{...}, object.DefaultProcessor(object.ToService, ...))`.
- **Memory discipline**: the processor converts each API object to a *stripped* struct before it enters the cache and zeroes the original (`*svc = api.Service{}`). `object.Service` keeps only `Version, Name, Namespace, Index, ClusterIPs, Type, ExternalName, Ports, ExternalIPs` (ExternalIPs = `spec.externalIPs` + `status.loadBalancer.ingress[].ip|hostname`). The package doc says: "Adding new fields … should not be done lightly as this increases the memory use and will lead to OOMs in the k8s scale test."
- **Indexes** are the lookup keys that make `ServeDNS` O(1): `ServiceNameNamespace` (`name.namespace`), `ServiceIP`, `ServiceExternalIP`, `EndpointNameNamespace`, `EndpointsIP`, `PodIP`. Reverse (PTR) answers come from the IP indexes.
- `Modified(mode)` returns an atomic timestamp of last change; it is used as the SOA serial, so secondaries doing IXFR/AXFR see changes.
- Uses `cache.ToListWatcherWithWatchListSemantics` (streaming list) and supports `apiserver_qps/burst/max_inflight` and `labels` / `namespace_labels` selectors to shrink the watch.
- Startup: `InitKubeCache` starts informers in `OnStartup` and polls `HasSynced()` every 100ms up to `startup_timeout` (default 5s), then **starts serving anyway** with a warning ("starting server with unsynced Kubernetes API"); until synced, name errors become SERVFAIL rather than NXDOMAIN (`handler.go`: `if !k.APIConn.HasSynced() { return ... RcodeServerFailure }`). This is the right behaviour to copy: never answer an authoritative NXDOMAIN from an empty cache.
- `handler.go` dispatches by qtype to `plugin.A/AAAA/TXT/CNAME/PTR/MX/SRV/SOA/NS` helpers from the shared `plugin/backend` layer (the same abstraction `etcd`, `k8s_external` use), refuses AXFR/IXFR itself (the `transfer` plugin calls `Transfer()` instead), and does a "fake A lookup" for unknown qtypes to distinguish NODATA from NXDOMAIN.
- RBAC needed (from `coredns/helm` ClusterRole): `list,watch` on `endpoints, services, pods, namespaces` (core) and `endpointslices` (`discovery.k8s.io`); plus `serviceexports/serviceimports` (`multicluster.x-k8s.io`) if MCS is on. Nothing else — the dataplane is read-only.
- The record schema it implements is the **Kubernetes DNS-Based Service Discovery spec v1.1.0** (`kubernetes/dns/docs/specification.md`): `<svc>.<ns>.svc.<zone>` A/AAAA; `_<port>._<proto>.<svc>.<ns>.svc.<zone>` SRV; headless → one A per ready endpoint plus `<hostname>.<svc>.<ns>.svc.<zone>`; ExternalName → CNAME; PTR under in-addr.arpa/ip6.arpa; `dns-version.<zone>` TXT = "1.1.0"; pods `1-2-3-4.<ns>.pod.<zone>` (modes `disabled|insecure|verified`). Default TTL 5s, max 3600.
- `k8s_external` (same informer) adds an *external* zone: `name.namespace.example.org` → LoadBalancer ingress IPs / externalIPs; synthesizes SOA/NS at the apex with an `apex` label (default `dns`) and static serial; optional `headless` external IPs.

### 2.3 Takeaways for kudurru from CoreDNS
- The dataplane must be able to *embed a k8s informer cache* and answer directly from indexed, stripped structs. No "sync to store then serve from store" indirection for cluster-service records — that only adds latency and a second failure mode.
- Readiness = "all data sources synced"; liveness = "process alive"; add lameduck on shutdown. Port 8080/8181 convention is widely assumed by charts and ServiceMonitors.
- Implement `Transferer` on every authoritative zone source so AXFR/IXFR+NOTIFY to external secondaries (BIND, Technitium, PowerDNS) come for free.
- `dynupdate`'s design (TSIG validated by a separate plugin, explicit `allow KEY OWNER TYPE` rules, bbolt persistence, snapshot semantics) is a good template for kudurru's RFC 2136 support in single-node mode.

---

## 3. external-dns: provider interface, webhook protocol, CRDs, Cloudflare

### 3.1 Architecture in one paragraph
`Source` (Ingress/Service/Gateway-API/CRD/… → `[]*endpoint.Endpoint`) → `Registry` (ownership: TXT records, DynamoDB, or the new **CRD registry**) → `plan.Calculate()` (diff current vs desired, apply `Policy` `sync|upsert-only|create-only`, `--managed-record-types`, domain filters, owner-id conflict resolution) → `Provider.ApplyChanges(plan.Changes)`. `controller.RunOnce` (controller/controller.go:74) is literally: `Registry.Records` → `Source.Endpoints` → `Registry.AdjustEndpoints` → plan → `Registry.ApplyChanges` → emit k8s Events; run on `--interval` and on informer events (`Source.AddEventHandler`).

### 3.2 The Endpoint data model (the lingua franca)
`endpoint/endpoint.go`:
```go
type Endpoint struct {
    DNSName          string            `json:"dnsName,omitempty"`      // ≤253 chars, pattern-validated, wildcard `*.` allowed
    Targets          Targets           `json:"targets,omitempty"`      // []string, ≤1000 items, each 1..4096 chars
    RecordType       string            `json:"recordType,omitempty"`   // enum A;AAAA;CNAME;DNAME;MX;NAPTR;NS;PTR;SRV;TLSA;TXT
    SetIdentifier    string            `json:"setIdentifier,omitempty"`// Route53-style routing-policy discriminator
    RecordTTL        TTL               `json:"recordTTL,omitempty"`    // int64 seconds, 0 = provider default, max 2^31-1
    Labels           Labels            `json:"labels,omitempty"`       // registry metadata (owner, resource)
    ProviderSpecific ProviderSpecific  `json:"providerSpecific,omitempty"` // []{name,value}, ≤100
}
type Changes struct {   // plan/plan.go
    Create    []*endpoint.Endpoint `json:"create,omitempty"`
    UpdateOld []*endpoint.Endpoint `json:"updateOld,omitempty"`
    UpdateNew []*endpoint.Endpoint `json:"updateNew,omitempty"`
    Delete    []*endpoint.Endpoint `json:"delete,omitempty"`
}
```
CEL validations on the CRD: SRV/NAPTR targets must be absolute (end with `.`), PTR names must be under `.in-addr.arpa`/`.ip6.arpa`, MX normalized to `"<pref> <host>"`. The `refObjects` (which k8s objects produced the endpoint) are `json:"-"` and are re-attached after a webhook round trip. **Recommendation: kudurru's internal "record set" type should be isomorphic to this struct** so DNSEndpoint ingestion, webhook-provider serving, and the Cloudflare publisher all share one model.

### 3.3 Provider interface and the "no new in-tree providers" policy
```go
type Provider interface {                                      // provider/provider.go
    Records(ctx) ([]*endpoint.Endpoint, error)
    ApplyChanges(ctx, changes *plan.Changes) error
    AdjustEndpoints(endpoints []*endpoint.Endpoint) ([]*endpoint.Endpoint, error) // canonicalize so the plan has no spurious diffs
    GetDomainFilter() endpoint.DomainFilterInterface
}
var SoftError = errors.New("soft error") // transient: logged, not fatal
```
README (v0.23): "**No new provider will be added to ExternalDNS in-tree.** ExternalDNS has introduced a webhook system, which can be used to add a new provider." Existing in-tree providers include `cloudflare`, `coredns` (etcd-backed), `pdns`, `rfc2136`, `pihole`, `inmemory`, `webhook`. AdGuard Home has a third-party webhook provider (`muhlba91/external-dns-provider-adguard`). So **kudurru can only ever be reached by external-dns through (a) the webhook protocol, (b) RFC 2136, or (c) the CRD source** — and the webhook is the intended path.

### 3.4 Webhook provider protocol (verbatim from `provider/webhook/api/httpapi.go` and `docs/tutorials/webhook-provider.md`)
| Provider method | HTTP | Route | Success code |
|---|---|---|---|
| Negotiate | GET | `/` | 200, body = `DomainFilter` JSON (`{"filters":["example.com"]}`) |
| Records | GET | `/records` | 200, body = `[]Endpoint` |
| AdjustEndpoints | POST | `/adjustendpoints` | 200, body = `[]Endpoint` |
| ApplyChanges | POST | `/records` | **204 No Content** |
- Media type negotiated via `Accept`/`Content-Type`: **`application/external.dns.webhook+json;version=1`** (`MediaTypeFormatAndVersion`). Wrong content type on `/` fails startup.
- Only `5xx` is retried (backoff, max 5 tries, mapped to `SoftError`); `3xx/4xx` are permanent failures; bodies capped at 32 MiB (`--webhook-provider-max-body-size`); 413 for oversized requests; always write a body; drain-friendly.
- Recommended topology: the webhook server runs as a **sidecar in the external-dns pod on `localhost:8888`**, with `/healthz` + `/metrics` on `0.0.0.0:8080` (the Helm chart's `provider.webhook` adds a container named `webhook`, port `http-webhook` 8080, probes `/healthz`). Any in-tree provider can be run as a webhook with `--webhook-server --provider=aws`.
- Custom annotations: `external-dns.kubernetes.io/webhook-<name>` are passed through as provider-specific properties. Note the **annotation prefix changed in v0.22.0** from `external-dns.alpha.kubernetes.io/` (legacy, still accepted) to `external-dns.kubernetes.io/`.
- OpenAPI spec at `api/webhook.yaml` (info.version v0.15.0, server `http://localhost:8888`).

For kudurru this means a tiny `kudurru externaldns-webhook` subcommand (or a mode of the API server) that implements four handlers backed by the zone store makes kudurru a target for every external-dns source (Ingress, Gateway API, Services, DNSEndpoint) without writing any k8s watching code — and conversely kudurru's own Cloudflare integration can *reuse* `plan.Changes` semantics.

### 3.5 CRDs: `DNSEndpoint` (source) and the new `DNSRecord` (registry)
```yaml
apiVersion: externaldns.k8s.io/v1alpha1
kind: DNSEndpoint            # namespaced, status subresource, api-approved annotation PR #2007
metadata: {name: examplednsrecord, annotations: {external-dns: public}}
spec:
  endpoints:                 # ≤1000
  - dnsName: foo.bar.com
    recordTTL: 180
    recordType: A
    targets: [192.168.99.216]
status:
  observedGeneration: 1
```
Flags: `--source=crd --crd-source-apiversion externaldns.k8s.io/v1alpha1 --crd-source-kind DNSEndpoint`; the doc stresses any CRD with that Spec shape can be used ("a contract that a user-specified CRD must implement"). Two objects declaring the same `dnsName`+`recordType` conflict ("external-dns publishes one and drops the other").

New in 2026 (`apis/v1alpha1/dnsrecord.go`, `docs/registry/crd.md`, alpha): `DNSRecord` is a *registry* — one object per managed endpoint, `spec.endpoint` is a single `Endpoint`, label `externaldns.k8s.io/owner`, `status.conditions[type=Ready]` with reasons `Accepted` (planned, not yet programmed) / `Programmed` / `Failed`, print columns for DNS name/type/targets/status. "The CRD registry is a record of what ExternalDNS applied, not a mirror of the DNS provider." This is a good model for how kudurru should expose *publication status* of records pushed to Cloudflare.

### 3.6 Cloudflare provider specifics (`provider/cloudflare/cloudflare.go`, `docs/tutorials/cloudflare.md`)
- SDK `github.com/cloudflare/cloudflare-go/v7`; auth via env `CF_API_TOKEN` (preferred; supports `file:/path`) or `CF_API_KEY`+`CF_API_EMAIL`; token needs **Zone:Read + DNS:Edit** (optionally `--zone-id-filter` to restrict).
- Uses the **Batch DNS Records API** (transactional per chunk; `--batch-change-size` default 200, `--batch-change-interval` 1s; falls back to per-record on chunk failure); global rate limit 1,200 req / 5 min; `--cloudflare-dns-records-per-page` up to 5000.
- Per-record options via provider-specific properties/annotations: `external-dns.kubernetes.io/cloudflare-proxied`, `cloudflare-custom-hostname`, `cloudflare-region-key`, record comments (100 chars free / 500 paid), tags; proxy not supported for `LOC, MX, NS, SPF, TXT, SRV, TLSA`; TTL 1 = "automatic".
- Zone discovery: `ListZones` + public-suffix matching of the record name to the longest zone; `changesByZone` groups.
- Takeaway: for the split-horizon integration kudurru should **store a `publishedTo:` status per record** (zone id, record id, last applied content) and reconcile with the batch API, not one call per record, and surface Cloudflare API rate-limit errors as a `Degraded` condition on the `Integration` object.

### 3.7 Sources worth matching
`source/` has Ingress, Service (incl. headless, `endpoints-type` NodeExternalIP/HostIP, `internal-hostname`), Node, Pod, Gateway API (`Gateway` v1, `HTTPRoute` v1, `GRPCRoute` v1, `TLSRoute` v1alpha2, `TCPRoute`, `UDPRoute` v1alpha2, `ListenerSet`), Istio, Contour, Traefik, Kong, OpenShift Route, F5, Gloo, Skipper, CRD, "unstructured" (generic). Key annotations: `hostname`, `target`, `ttl`, `set-identifier`, `controller`, `access`, `endpoints-type`, `ingress-hostname-source`, `gateway-hostname-source`. If kudurru implements its own Ingress/Gateway source (k8s_gateway style), **honour the same `external-dns.kubernetes.io/hostname` and `target` annotations** so users don't learn two vocabularies.

---

## 4. k8s_gateway: exposing Ingress / Gateway API / LoadBalancer hostnames

- A single CoreDNS external plugin (`gateway.go`, `kubernetes.go`) that resolves: `HTTPRoute/TLSRoute/GRPCRoute` `spec.hostnames` → the parent `Gateway.status.addresses`; `Ingress` `spec.rules[].host` → `.status.loadBalancer.ingress`; `Service` type LoadBalancer → `name.namespace.<zone>` or the `coredns.io/hostname` / `external-dns.alpha.kubernetes.io/hostname` annotation; nginx `VirtualServer`. Only A/AAAA (others NODATA). Ordered resource precedence (first match wins).
- Each resource type has its own `SharedIndexInformer` with a hostname index (`ingressHostname`, `serviceHostname`, `httpRouteHostname`, `gatewayIndex`…); `ServeDNS` computes `indexKeys = [fqdn-without-trailing-dot, name.namespace]` and does an index lookup. CRD presence is probed at startup (`existGatewayCRDs`) so missing Gateway API CRDs just disable that source — copy this.
- **Apex/NS handling**: it synthesizes SOA + NS at the zone apex and a `dns.<zone>`-style sub-apex whose A record is the *plugin's own LoadBalancer IP* (`apex` defaults to `{release}-k8s-gateway.{namespace}`; `secondary` names a peer release for a two-NS glue setup, "some of the stricter resolvers expect a zone to be available on at least two servers (RFC1034 §4.1)"). The dual-NS recipe is: two Deployments, two LoadBalancer Services, delegate from the parent zone with two NS + two glue A records.
- Chart (ori-edge v2.0.3): `service.type: LoadBalancer`, **UDP only by default (`useTcp: false`)**, container listens on **1053** and runs `runAsUser: 1000` (`secure: true`), Service maps 53→1053; probes `/health:8080`, `/ready:8181`; `topologySpreadConstraints`, `priorityClassName`, `dnsChallenge` (forward `_acme-challenge` to a cloud zone), `extraZonePlugins`. Blocky's TrueCharts package bundles `ghcr.io/k8s-gateway/k8s_gateway:1.8.2` as a sidecar on UDP 5353 — i.e. people already compose "blocker + k8s_gateway" by hand; kudurru can be that combination natively.
- The ori-edge repo is archived in favour of `k8s-gateway/k8s_gateway` (v1.9.2).

---

## 5. Kuadrant dns-operator: CRD-as-zone-data served by a dataplane

The most relevant "operator + CoreDNS" split in the wild.
- CRDs (group `kuadrant.io/v1alpha1`): `DNSRecord` and `DNSHealthCheckProbe`.
  ```go
  type DNSRecordSpec struct {
      OwnerID     string              // immutable, 6..36 chars, defaults to UID
      RootHost    string              // immutable; all endpoints must be under it
      ProviderRef *ProviderRef        // -> Secret with type kuadrant.io/aws|gcp|azure|coredns (or label kuadrant.io/default-provider=true)
      Endpoints   []*externaldns.Endpoint   // REUSES external-dns's Endpoint type
      HealthCheck *HealthCheckSpec    // port/path/protocol/interval/failureThreshold/additionalHeadersRef
      Delegate    bool                // immutable; mutually exclusive with providerRef
  }
  type DNSRecordStatus struct {
      Conditions []metav1.Condition   // Ready, Healthy, ReadyForDelegation, Active
      ObservedGeneration int64; WriteCounter int64
      Endpoints []*externaldns.Endpoint   // last successfully published
      OwnerID, ZoneID, ZoneDomainName string; DomainOwners []string
      RemoteRecordStatuses map[string]apiextensionsv1.JSON  // per-cluster
  }
  ```
  CEL rules enforce immutability/transition constraints (`ownerID can't be unset if it was previously set`, `delegate=true and providerRef are mutually exclusive`). Condition reasons: `ProviderSuccess, ProviderError, AwaitingValidation, ProviderEndpointsRemoved, ProviderEndpointsDeletion, ValidationError, RegistryError`; `Healthy` reasons `AllChecksPassed/SomeChecksPassed/HealthChecksFailed`.
- **Provider credentials are typed Secrets** (`type: kuadrant.io/aws`, keys `AWS_ACCESS_KEY_ID…`) selected by `providerRef` or a `kuadrant.io/default-provider=true` label. Clean pattern for kudurru's `Integration` → `secretRef`.
- **CoreDNS integration** (`docs/coredns/coredns-integration.md`, `coredns/plugin/`): the operator does *not* push to CoreDNS. It labels processed `DNSRecord`s with `kuadrant.io/coredns-zone-name: <zone>`; a custom CoreDNS plugin runs one informer **per configured zone with a label selector** (`WATCH_NAMESPACES` env), rebuilds an in-memory zone tree (`file` plugin's `tree`, `rrutil`) from all matching `spec.endpoints` on every change (`refreshZone`), serves it with the file-plugin lookup algorithm (wildcards, CNAME chasing, delegation), implements `Transfer` for AXFR, and answers REFUSED to AXFR if `transfer` isn't loaded. GeoIP/weighted answers via provider-specific `geo-code`/`weight`. Multi-cluster uses a `--delegation-role=primary|secondary` and kubeconfig Secrets labelled `kuadrant.io/multicluster-kubeconfig=true`.
- RBAC for the dataplane: only `get,list,watch` on `dnsrecords` (kuadrant.io).
- Lesson: **the dataplane watches CRDs directly and needs no leader**; the operator (leader-elected) owns status, validation, health checks and cloud publishing. A single kudurru binary can host both halves as "roles".

---

## 6. PowerDNS-Operator and the other DNS operators (CRD shapes to learn from)

### 6.1 PowerDNS-Operator (Orange, v0.6.0, group `dns.cav.enablers.ob/v1alpha2`)
- Four kinds in **two scopes**: `ClusterZone` / `Zone` (namespaced) and `ClusterRRset` / `RRset`. Rationale: platform admins own zones cluster-wide; app teams get namespaced RRsets delegated via RBAC. Ships per-kind `viewer/editor/admin` ClusterRoles (`config/rbac/*_viewer_role.yaml` …).
- ```yaml
  kind: ClusterZone
  metadata: {name: example.org}          # object name IS the zone name
  spec: {kind: Native|Master|Slave|Producer|Consumer, nameservers: [ns1.example.org], catalog: ..., soa_edit_api: DEFAULT|INCREASE|EPOCH}
  status: {id, name, kind, serial, notified_serial, edited_serial, masters, dnssec, catalog, syncStatus: Pending|Succeeded|Failed, conditions: [Available|Progressing|Degraded], observedGeneration}
  ---
  kind: RRset
  metadata: {name: web.myapp.example.com, namespace: default}
  spec: {type: A, name: web, ttl: 300, records: [192.168.1.10, 192.168.1.11], comment: ..., zoneRef: {name: myapp.example.com, kind: Zone|ClusterZone}}
  status: {lastUpdateTime, dnsEntryName, syncStatus, conditions, observedGeneration}
  ```
- `spec.name` is immutable (CEL `self == oldSelf`), duplicates across namespaces are detected and marked `Duplicated`; a `GenericZone` interface abstracts the two scopes so one reconciler body serves both. Finalizers are used (also for metrics cleanup). PowerDNS itself runs *outside* (operator needs only the API URL + key via Secret). Supported: PowerDNS Auth 4.9/5.0, k8s 1.33–1.35, Go 1.26.

### 6.2 Others (secondary sources, UNVERIFIED details marked)
- **containeroo/cloudflare-operator**: `Account` (token Secret), `Zone`, `DNSRecord` (`dnsrecords.cloudflare-operator.io`, status `zoneID`, `accountName`); records can be generated from Ingress and (v1.8+) HTTPRoute annotations; shared generated records across owners. Not official Cloudflare. License UNVERIFIED.
- **95ulisse/dns-operator**: single `DNSRecord` kind, MIT, Cloudflare among providers.
- **linka-cloud/k8s-dns-manager** (`dns.linka.cloud/v1alpha1`): `DNSRecord` with typed sub-specs (`spec.a|cname|txt|srv|mx`) or `spec.raw` zone-file text; **embeds an in-process CoreDNS** (`--no-dns` to disable) with a `k8s_dns` plugin for apex NS/SOA; auto A records from LoadBalancer Services/Ingress; cert-manager for its validating webhook; alpha.
- **octodnscontroller**: `OctoDNSProvider`, `OctoDNSZone`, `OctoDNSRecordSet` wrapping octoDNS (RFC 2136 → BIND).
- **pfisterer/edsc-dns-domains**: BIND9 in-cluster reconfigured from CRs (`dnsseczone.farberg.de/v1`), stale (>4 years).
- **Technitium**: no official chart. `aaronsteed/dns-kube-job` is a sidecar controller that reads ConfigMaps and calls Technitium's HTTP API on `localhost:5380`; `ashtonian/technitium-configurator` exists because "config files are stored in binary … there isn't a declarative solution". Real-world k8s migration (virtualizationhowto, 2026-05): bind `0.0.0.0:53` inside the pod rather than the MetalLB VIP; zone transfers refused because requests arrived from pod CIDR (needed allow-list or SNAT); cluster-domain choice (`cluster.local` clashes with k8s) broke replication; Traefik `IngressRouteTCP` TLS passthrough for the cluster port.
- No "Cloudflare official dns-operator" and no maintained "bind9 operator" with DNSZone/DNSRecord kinds were found. The field is fragmented; nobody owns the obvious `DNSZone`/`DNSRecordSet` + blocklist/policy vocabulary. kudurru can.

---

## 7. Operator patterns (kubebuilder v4.16 / controller-runtime v0.25)

### 7.1 Project layout and generators
kubebuilder scaffolds `cmd/main.go` (Manager), `api/<version>/<kind>_types.go` with markers, `internal/controller/<kind>_controller.go`, `config/{crd,rbac,manager,webhook,default,samples}` as **kustomize** bases, `PROJECT` file, `Makefile` (`make manifests` runs controller-gen). controller-gen markers that matter:
- `+kubebuilder:object:root=true`, `+kubebuilder:subresource:status`, `+kubebuilder:resource:scope=Cluster|Namespaced,shortName=…`, `+kubebuilder:storageversion`, `+kubebuilder:printcolumn:name=…,type=…,JSONPath=…` (gives `kubectl get` columns — every kudurru kind should print Ready/Serial/Records).
- Validation: `+kubebuilder:validation:Enum=…;MinLength;MaxLength;Pattern;Minimum;Maximum;MaxItems;items:Pattern`, `+kubebuilder:default=…`, and **CEL `+kubebuilder:validation:XValidation:rule="…",message="…"`** (immutability via `self == oldSelf`, cross-field rules, transition rules with `oldSelf`). external-dns and Kuadrant both push nearly all validation into CRD schema + CEL and keep webhooks minimal; the kubebuilder book warns the apiserver estimates CEL admission cost as `maxItems × maxLength × regex size`, so bound every list/string (external-dns comments on exactly this).
- RBAC: `+kubebuilder:rbac:groups=…,resources=…,verbs=…` → ClusterRole/Role.
- Scope rules (book `crd-scope.md`): changing CRD scope later is a breaking change ("Only change scope during initial development"); cluster-scoped CRDs always need ClusterRole; manager scope (which namespaces it watches, `WATCH_NAMESPACE`) is independent of CRD scope.

### 7.2 Reconcile discipline (book `good-practices.md`)
- Reconciliation is level-based and **idempotent**; never branch on event type.
- "Avoid a design solution where the same controller reconciles more than one Kind" (one controller per kind; use `Owns()`/`Watches()` for cross-kind triggers).
- Adopt **status conditions** (api-conventions): `metav1.Condition{Type, Status True|False|Unknown, ObservedGeneration, LastTransitionTime, Reason (CamelCase, required), Message}`; types are PascalCase adjectives/past-tense verbs (`Ready`, `Programmed`, `Synced`, `Degraded`), absent condition = Unknown; `Ready` for long-running objects, `Succeeded` for bounded ones; **`phase` is deprecated** — don't add a `status.phase` enum. Use `+listType=map,+listMapKey=type` on `conditions` (PowerDNS-Operator, external-dns do). Record `status.observedGeneration` so stale status is detectable.
- Naming: JSON camelCase; references end in `Ref`/`Refs` (`zoneRef`, `secretRef`); cross-namespace references on namespaced kinds are discouraged (security/lifecycle); times end in `Time`, durations in `Seconds`.
- Finalizers (`using-finalizers.md`): add finalizer when first seen; on `DeletionTimestamp` run idempotent pre-delete logic (e.g. delete the Cloudflare records, NOTIFY secondaries, drop the zone from the store) then remove it. Kuadrant adds `ReadyForDelegation`/`ProviderEndpointsDeletion` reasons so status explains the teardown.
- Log in k8s style ("Capital, no period, past tense, object type named, structured kv").

### 7.3 Admission webhooks vs. CEL
- controller-runtime supports mutating/validating admission and conversion webhooks; `--webhook-port=-1` disables the server (needs v0.25+). Webhooks require TLS certs (cert-manager is the de-facto prerequisite — linka-cloud, OLM v1, kubebuilder scaffolds all assume it). The "webhook bootstrap deadlock" only applies to webhooks on core kinds (Pods…), not CRDs.
- Mutating webhooks cannot set `status`; controllers set initial status.
- **ValidatingAdmissionPolicy (GA 1.30)** evaluates CEL inside the apiserver with no server to run; `matchConstraints.resourceRules` take any `apiGroups`, though docs say CRD type-checking isn't supported yet. For kudurru: **ship CRD-schema + CEL validation first, an optional validating webhook only for things CEL can't express** (e.g. "zone file parses", "blocklist URL reachable", "record name inside referenced zone"). Keep the webhook optional in the Helm chart (`webhook.enabled=false` default) to avoid a cert-manager dependency in small installs.

### 7.4 Leader election (`pkg/leaderelection`, `pkg/manager`)
- Default lock is a **`coordination.k8s.io/Lease`** (`resourcelock.LeasesResourceLock`); `LeaderElectionID` is required; namespace defaults to the in-cluster SA namespace; identity = `hostname_uuid`; defaults `LeaseDuration 15s`, `RenewDeadline 10s`, `RetryPeriod 2s`; the lease client timeout is set to `max(1s, RenewDeadline/2)` so one hung apiserver call can't drop leadership; `LeaderElectionReleaseOnCancel: true` speeds hand-off but "requires the binary to immediately end when the Manager is stopped".
- A `Runnable` may implement `NeedLeaderElection() bool` — controllers and webhooks that must be singletons return true; **servers (the DNS listener, metrics, the UI API) return false and run on every replica**. This is precisely the mechanism for "one binary, both controller and dataplane".
- Needed RBAC: `leases` get/create/update in the manager namespace, plus `events` create/patch.

### 7.5 Authentication glue available in controller-runtime
`pkg/webhook/authentication` implements a **TokenReview webhook handler** (`authentication.Webhook{Handler}` returning `Authenticated(token, UserInfo)`/`Unauthenticated`). That is the inverse direction (kudurru acting as an authenticator for the apiserver) — not what we need, but the request/response types (`authenticationv1.TokenReview`) are the same ones kudurru's API would *send* to the apiserver (section 9).

---

## 8. One binary: controller AND dataplane

| Option | Who does it | Pros | Cons |
|---|---|---|---|
| A. Dataplane-only with informers (CoreDNS `kubernetes`, k8s_gateway, Kuadrant plugin) | every replica watches, no writes | no leader, trivially scalable, survives apiserver outage from cache | no status, no side effects, no CRD validation feedback |
| B. Operator-only (PowerDNS-Operator, cloudflare-operator, external-dns) | 1 leader reconciles into an external server | clean SRP; dataplane is any product | second product to run; status lag; dataplane config drift |
| C. **Single binary with roles** (Gravity `BOOTSTRAP_ROLES=dns;dhcp;api;…`; linka-cloud embeds CoreDNS in the operator) | same image, `--role` or CR-driven | one chart, one image, one API; status and serving share memory | must keep leader-only work strictly separated; bigger binary |

Recommendation for kudurru: **C**, structured like a controller-runtime Manager with two kinds of runnables:
1. **Dataplane runnables** (`NeedLeaderElection()=false`, every pod): DNS listeners (UDP/TCP/DoT/DoH/DoQ), the HTTP API/UI, metrics, and the *read-side* informers (or the embedded-store watchers in Docker mode) that materialize zones, blocklists, client groups and policies into lock-free in-memory indexes (CoreDNS stripped-object style). Readiness = all sources synced once. Do not SERVFAIL after sync just because the apiserver went away; keep serving the last snapshot (informers do this naturally).
2. **Controller runnables** (`NeedLeaderElection()=true`, one leader): write `status.conditions`/serial/`observedGeneration`, run blocklist downloads once and publish the compiled result (ConfigMap/object in store) rather than every replica downloading, push records to Cloudflare/UniFi, send NOTIFY, run health probes, own DNSSEC key generation/rollover (keys in a Secret), take backups.
   In Docker mode the "leader" is simply the only instance; in a multi-node non-k8s cluster (Gravity-style) the same interface is backed by an embedded-etcd/raft election. Keep `LeaderElector` behind an interface so both backends exist.

Serial/consistency: give each `DNSZone` a monotonically increasing serial derived from the *controller's* commit (Gravity-like) or, for CRD-derived zones, from `max(resourceVersion-derived timestamp)` as CoreDNS does with `Modified()`; replicas must agree on the serial for AXFR/IXFR to secondaries to be sane, so the leader should stamp `status.serial` and replicas should serve that value rather than a local clock.

Deployment shape for role C: a `Deployment` (replicas ≥2, PDB `maxUnavailable: 1`, topology spread across nodes/zones) when all state lives in CRDs; a `StatefulSet` with `volumeClaimTemplates` only for the embedded-store mode (single-node k8s "docker-like" install) — Gravity's k8s doc ships exactly that StatefulSet for single node and tells multi-node users not to use the embedded etcd in k8s ("the built-in cluster management is optimized for static nodes … use an etcd operator or Kubernetes' etcd").

---

## 9. Deployment mechanics (what the manifests must get right)

### 9.1 Exposing port 53: the Service problem
- **Mixed-protocol LoadBalancer (TCP+UDP on 53 in one Service) is GA since 1.26** and the gate is gone; the Kubernetes API accepts it. But: "The set of protocols that can be used for load balanced Services is defined by your cloud provider." GKE requires `1.34.1-gke.2190000+` and passthrough NLB; AWS LB controller needed a `TCP_UDP` listener (historically blocked) — **UNVERIFIED current AWS state**. MetalLB and kube-vip both handle one Service with both protocols (MetalLB's own docs example for IP sharing is "a TCP service and a UDP service, both on port 53").
- Portable fallback used by every DNS chart today: **two Services (one UDP, one TCP) that share one IP**. MetalLB: annotation `metallb.io/allow-shared-ip: <key>` (legacy `metallb.universe.tf/allow-shared-ip`), both services must either use `externalTrafficPolicy: Cluster` or "point to the exact same set of pods"; pin the address with `spec.loadBalancerIP` (deprecated in core but MetalLB honours it) or `metallb.io/loadBalancerIPs`; choose a pool with `metallb.io/address-pool`. gabe565's AdGuard chart ships exactly this (`dns-tcp` + `dns-udp` LoadBalancer Services, `externalTrafficPolicy: Local`, shared-IP annotation). Pi-hole's chart defaults to separate TCP/UDP Services with an opt-in `mixedService: true`. kudurru's chart should default to **one mixed-protocol Service** and offer `service.dns.splitProtocols=true` for legacy LBs, emitting the shared-IP annotation automatically.
- kube-vip: acts on `type: LoadBalancer` Services with `spec.loadBalancerClass: kube-vip.io/kube-vip-class` (or unset in legacy mode), IPs from the `kubevip` ConfigMap in `kube-system` (`cidr-global`, `range-global`, `cidr-<ns>`, `range-<ns>`) via `kube-vip-cloud-provider`, or a static `kube-vip.io/loadbalancerIPs` annotation; ARP (L2) mode uses leader election per Service (`svc_election=true`) and `externalTrafficPolicy: Local` "requires services election — only nodes running a pod for the service can take part", which preserves client IP. BGP mode uses multiple nodes.
- **Client source IP matters more for kudurru than for most apps** (Pi-hole-style per-client stats, client groups, UniFi client mapping): use `externalTrafficPolicy: Local` (kube-proxy then preserves the source IP and adds a `healthCheckNodePort`), accept uneven balancing, and document that `Cluster` policy hides clients behind node IPs. Technitium's k8s story shows what goes wrong when XFR peers see pod CIDR source addresses.
- `loadBalancerClass` should be a chart value; set `trafficDistribution: PreferSameZone` optionally; `ipFamilyPolicy: PreferDualStack` for v6.
- `hostNetwork: true` + `DaemonSet` variant: node IP == DNS IP, no LB needed, true client IPs, and the pod can also serve DHCP/mDNS later. Costs: conflicts with `systemd-resolved` stub on 127.0.0.53 only if binding 0.0.0.0 on the host (Ubuntu 24.04 note from Technitium guide), violates Baseline PSS, **`net.*` sysctls are disallowed with hostNetwork** (so use `NET_BIND_SERVICE` there), one pod per node, and Pi-hole's chart offers it as `hostNetwork` + `dnsHostPort`. Ship it as an opt-in overlay, not the default.
- `headless` Service (`clusterIP: None`) is needed for peer discovery (pods address each other for NOTIFY/XFR, cluster membership) and for `StatefulSet` DNS names; also useful to point an in-cluster CoreDNS `forward`/stub domain at kudurru pods directly.

### 9.2 Running on port 53 as non-root
Three options, all verified:
1. `securityContext.capabilities: {drop: [ALL], add: [NET_BIND_SERVICE]}` + `runAsNonRoot: true` + `runAsUser: 65532` + `readOnlyRootFilesystem: true` + `allowPrivilegeEscalation: false` + `seccompProfile: RuntimeDefault` — this is the CoreDNS chart default and "satisfies the restricted Pod Security Standard". Note the file capability must be effective for a non-root process: blocky ships its binary with `cap_net_bind_service=+p` so it starts even under `drop: [ALL]` and raises the cap only when a port < 1024 is configured; CoreDNS's distroless `nonroot` image relies on the container runtime's ambient/bounding set. Build kudurru's image with `setcap cap_net_bind_service=+ep` (or `+p` and raise at runtime like blocky) and document the k8s `add: [NET_BIND_SERVICE]`.
2. Pod sysctl `net.ipv4.ip_unprivileged_port_start=0` (safe since 1.22, PSS Baseline-allowed; not with hostNetwork).
3. Listen on 1053 and let the Service map 53→1053 (k8s_gateway chart, `runAsUser: 1000`). Simplest, but breaks hostNetwork/docker `--network host` parity. Prefer 1 with 3 as an escape hatch (`listen.port` value).
Avoid what Pi-hole/AdGuard TrueCharts do (root, `NET_ADMIN`, `allowPrivilegeEscalation: true`).

### 9.3 Probes
- CoreDNS convention: liveness `GET :8080/health` (initialDelay 60, period 10, failureThreshold 5) and readiness `GET :8181/ready` (initialDelay 30, period 5); `lameduck 5s` so the pod keeps answering while endpoints drain. Pi-hole and blocky use `exec` probes (`curl … api/info/login | jq`, `/app/blocky healthcheck`), Gravity uses `/healthz/live` and `/healthz/ready` on the metrics port. gRPC probes are GA (1.27) but TLS-less unless 1.37 alpha; `exec dig` works but forks a process every period. **Recommendation**: HTTP `/healthz/live` and `/healthz/ready` on the metrics/admin port, where ready additionally performs an *internal* DNS self-query (loopback UDP query for `version.kudurru.` CH TXT or the zone apex) so readiness reflects the real listener, plus a `startupProbe` (failureThreshold×period ≥ blocklist first-load time) so slow blocklist compilation doesn't trigger liveness kills.
- Terminate gracefully: on SIGTERM flip ready to 503, keep serving `terminationGracePeriodSeconds` ≈ lameduck + in-flight TCP timeouts.

### 9.4 Scheduling and availability
- `PodDisruptionBudget` (`policy/v1`, CoreDNS chart makes it opt-in via `podDisruptionBudget: {}`; recommend default `minAvailable: 1` when `replicas>1`), `topologySpreadConstraints` on `kubernetes.io/hostname` and `topology.kubernetes.io/zone` with `whenUnsatisfiable: ScheduleAnyway`, `priorityClassName: system-cluster-critical` when kudurru is the cluster resolver (CoreDNS sets `isClusterService` + `CriticalAddonsOnly` toleration), `dnsPolicy: Default` for the DNS server pod itself (never `ClusterFirst`, or you loop through yourself; CoreDNS chart enforces `dnsConfig` when `dnsPolicy: None`), HPA or `cluster-proportional-autoscaler` (CoreDNS chart supports both).
- `NetworkPolicy`: ingress UDP/TCP 53 (+853/443/8853 for DoT/DoH/DoQ) from anywhere, admin/API port only from the UI ingress namespace, metrics from monitoring; egress to upstream resolvers 53/853/443, to the apiserver, to Cloudflare/UniFi, and to secondaries for NOTIFY. Ship a template with `networkPolicy.enabled=false` default (most home clusters have no CNI policy engine).
- Config checksum annotation on the pod template (`checksum/config`) to roll pods on ConfigMap change (all three charts do it); but with CRD-driven config the goal is *no* restarts — only the bootstrap Corefile-equivalent should be a ConfigMap.

### 9.5 Packaging
- **Helm**: `crds/` directory installs CRDs on first install only — "Helm does not upgrade or delete CRDs" (deliberate, data-loss avoidance); `--skip-crds` exists; `--dry-run` can't validate CRs against uninstalled CRDs. Standard workaround: a separate `kudurru-crds` chart (or `crds.install`/`crds.keep` values with templated CRDs + `helm.sh/resource-policy: keep`, the pattern used by cert-manager/external-dns). Publish both to **OCI**: `helm push kudurru-<ver>.tgz oci://ghcr.io/<org>/charts` (tag = chart version; the chart name is omitted on push and included on install: `helm install kudurru oci://ghcr.io/<org>/charts/kudurru --version X`); GHCR packages are private until flipped; no `latest` tag is created by `helm push`. Add `artifacthub.io/*` annotations. Follow the chart conventions seen in CoreDNS/external-dns: `image.{repository,tag,pullPolicy}`, `service.{type,loadBalancerIP,loadBalancerClass,externalTrafficPolicy,annotations,ipFamilyPolicy}`, `podSecurityContext`/`securityContext`, `resources`, `nodeSelector/tolerations/affinity/topologySpreadConstraints`, `priorityClassName`, `serviceMonitor.enabled`, `rbac.create`, `serviceAccount.{create,name,annotations}`, `extraVolumes`, `customLabels/customAnnotations`.
- **kustomize**: kubebuilder's `config/` tree is the natural source; `kustomize build config/default | kubectl apply -f -` is what PowerDNS-Operator and Kuadrant document. Ship `deploy/kustomize/{base,overlays/{metallb,kube-vip,hostnetwork-daemonset,cloud-lb}}` and a flat `deploy/install.yaml` release artifact (PowerDNS-Operator publishes `bundle.yaml`/`dist/install.yaml`).
- **OLM v1** (operator-controller v1.12): `ClusterCatalog` (FBC image) + `ClusterExtension` (cluster-scoped, needs a pre-created ServiceAccount with sufficient RBAC); depends on cert-manager; console support still partial. Treat as a later "OpenShift" deliverable; don't let OLM's bundle format shape the core design.

---

## 10. Kubernetes RBAC → kudurru RBAC

Two parallel paths that must agree:
1. **kubectl / GitOps users** act on CRDs; the apiserver enforces RBAC. kudurru ships aggregated ClusterRoles (PowerDNS-Operator pattern): `kudurru-viewer` (get/list/watch all kinds), `kudurru-editor` (CRUD on namespaced kinds: `DNSRecordSet`, `BlockList`, `ClientGroup`, `Policy`), `kudurru-admin` (plus cluster-scoped `DNSZone`, `Integration`, `Cluster` config). Label them `rbac.authorization.k8s.io/aggregate-to-view|edit|admin: "true"` so the default `view/edit/admin` roles pick them up. Namespaced `DNSRecordSet` + cluster `DNSZone` is what lets app teams self-serve records in a zone the platform team owns.
2. **UI/API users**: OIDC is the primary login (requirement). To let *Kubernetes identities* use the API too (CI jobs with ServiceAccount tokens, `kubectl -n kudurru exec`-free scripting, `kudurru` CLI with a kubeconfig): accept a bearer token, call **`TokenReview`** (`authentication.k8s.io/v1`) to learn `user, groups`, then **`SubjectAccessReview`** (`authorization.k8s.io/v1`) with `resourceAttributes{group: kudurru.io, resource: dnsrecordsets, verb: update, namespace, name}` for each API action, mapping kudurru's REST verbs to k8s verbs 1:1. The kudurru ServiceAccount needs the built-in `system:auth-delegator` ClusterRole (create tokenreviews + subjectaccessreviews). Cache decisions briefly (apiserver already caches TokenReview). Client certs don't work with TokenReview — document "use a token". This makes the k8s RBAC the **single policy source** in k8s mode; in Docker mode the same role names (`viewer/editor/admin` + custom) are stored locally and mapped from OIDC groups.
Don't build an aggregated API server (`APIService`) — much heavier, and CRDs already give you `kubectl`.

---

## 11. How the comparables actually ship to Kubernetes

| Project | Workload | Service pattern | Security | Probes | Config/state | Notable |
|---|---|---|---|---|---|---|
| CoreDNS (helm 1.48.2) | Deployment (+HPA / cluster-proportional autoscaler), PDB opt-in | ClusterIP by default; `serviceType`, `loadBalancerClass`, `trafficDistribution`, UDP+TCP in one Service (`use_tcp`) | restricted PSS, uid 65532, `NET_BIND_SERVICE` | `/health:8080`, `/ready:8181` | Corefile ConfigMap + checksum annotation; zone files as ConfigMap keys | `isClusterService` toggles `k8s-app` label, critical tolerations, `dnsPolicy` |
| k8s_gateway (ori-edge 2.0.3) | Deployment, replicaCount 1 | LoadBalancer, **UDP only by default**, `useTcp` opt-in, port 53→1053 | `runAsUser: 1000` on port 1053 | CoreDNS probes | Corefile ConfigMap | `secondary` + `apex` for 2-NS delegation; `dnsChallenge` forwarder |
| Gravity (v0.33) | **StatefulSet** w/ PVC `/data` (single node); multi-node: external etcd operator | one LoadBalancer with UDP 53, TCP 53, UDP 67/69, TCP 8008 (mixed protocol) | not specified (runs as root? UNVERIFIED) | `/healthz/live`, `/healthz/ready` on `http-metrics` | `BOOTSTRAP_ROLES=dns;dhcp;api;…`, `INSTANCE_IP=<LB IP>` must be set manually, `INSTANCE_LISTEN=0.0.0.0` | no Helm chart in repo; "do not use built-in etcd in a multi-node k8s cluster" |
| Technitium (v15.6) | none official; community Deployment/StatefulSet | MetalLB VIP; Traefik `IngressRouteTCP` passthrough for cluster port 53443 | root container (UNVERIFIED) | — | binary config files; ConfigMap sidecar controller `dns-kube-job` | bind `0.0.0.0`, not the VIP; XFR ACL must include pod CIDR or use SNAT; cluster domain must not be `cluster.local` |
| AdGuard Home (gabe565 0.3.25) | Deployment (bjw-s common) | **two LoadBalancer Services sharing an IP** (`metallb.universe.tf/allow-shared-ip`), `externalTrafficPolicy: Local`; TCP 53/853, UDP 53/784 | default image (root) | HTTP `/` | full `AdGuardHome.yaml` as a chart value, copied if absent; PVCs `conf` + `work` | config file is the API; no CRDs |
| AdGuard Home (TrueCharts 13.10) | Deployment | ~8 Services (dns-tcp, dns-udp, dns-https, dns-quic-udp…) | `runAsUser: 0` | HTTP `/` | PVCs | — |
| blocky (TrueCharts) | Deployment **replicas 2**, RollingUpdate | one `dns` Service with udp+tcp 53, `dot` 853, `https` 4443, plus **k8s_gateway sidecar** service UDP 5353 | image is non-root uid 100 w/ `cap_net_bind_service=+p` | exec `/app/blocky healthcheck` (disabled by default) | config.yml from values; optional Valkey/redis for shared cache | stateless; good model for horizontal scale |
| Pi-hole (MoJo2600) | Deployment | separate TCP/UDP Services (NodePort default), `mixedService` opt-in, `hostNetwork`/`dnsHostPort` opt-in, dual-stack separate v6 services | root + `NET_ADMIN` (TrueCharts) | exec `curl …/api/info/login | jq` | PVC `/etc/pihole`; DoH sidecar | the `mixedService` toggle is the compatibility knob users expect |
| external-dns (chart 1.23) | Deployment, 1 replica (leader election optional) | none | restricted-ish | `/healthz` | flags/env; CRDs `DNSEndpoint`, `DNSRecord` | webhook sidecar container on localhost:8888 |
| Kuadrant dns-operator (v0.18) | operator Deployment + separate CoreDNS Deployment w/ plugin | CoreDNS LoadBalancer | — | — | `DNSRecord` CRs + provider Secrets | label-selected informer per zone in the dataplane |
| PowerDNS-Operator (v0.6) | operator Deployment; PowerDNS external | — | — | — | 4 CRDs (2 scopes); Helm + kustomize + `bundle.yaml` | per-kind viewer/editor/admin roles |

---

## 12. Recommendation: kudurru's CRD set

API group `kudurru.io`, start at `v1alpha1`, every kind has `/status` subresource, `conditions` (`+listType=map`), `observedGeneration`, print columns, and `shortNames`. Reuse external-dns's `Endpoint` JSON shape for record data wherever a "list of records" appears, so `DNSEndpoint` ingestion and webhook serving are free.

| Kind (scope) | Purpose | Spec sketch | Status / conditions |
|---|---|---|---|
| `Kudurru` (cluster, singleton `default`) | server-wide config that today lives in a config file: listeners, upstreams/forwarders, cache, DoH/DoT/DoQ TLS refs, query-log retention, OIDC, feature flags | `listeners[]{port,protocol,tls.secretRef}`, `upstreams[]`, `cache{}`, `queryLog{}`, `auth{oidc{issuer,clientIDSecretRef,groupClaim}}`, `kubernetes{clusterZone: cluster.local, exposeExternalZone: ...}` | `Ready`, `Listening`, `UpstreamsHealthy` |
| `DNSZone` (**cluster**) | authority boundary — the "stone in the temple". Types: `primary`, `secondary` (masters, TSIG), `forward`/`conditional`, `stub`, `kubernetes` (cluster.local synthesized), `external` (Ingress/Gateway/LB hostnames, k8s_gateway-style) | `zoneName` (immutable), `type`, `soa{mname,rname,refresh,retry,expire,minimum}`, `nameservers[]`, `ttl`, `dnssec{enabled,algorithm,keySecretRef,nsec3}`, `transfer{allowTo[],notify[],tsigKeyRef}`, `secondary{primaries[],tsigKeyRef}`, `forward{upstreams[]}`, `recordSetSelector` (which namespaces may add records: `namespaces[]`/`labelSelector`), `publish[]{integrationRef, filter}` (split-horizon: which records also go to Cloudflare) | `Ready`, `Signed`, `Transferred` (serial seen by secondaries), `Published`; `serial`, `recordCount`, `nameserverAddresses[]` (the LB IPs, auto-detected like k8s_gateway apex) |
| `DNSRecordSet` (**namespaced**) | records in a zone, delegated to app teams | `zoneRef{name}` (cluster `DNSZone`), `endpoints[]` = external-dns `Endpoint` (`dnsName, recordType, targets, recordTTL, setIdentifier, providerSpecific{geo, weight, cloudflare-proxied…}`), `views[]` (optional per-client-group answers) | `Ready` (reasons `Accepted/Programmed/Failed/ZoneMissing/Duplicated/OutsideZone`), `Published` (per integration) |
| `DNSEndpoint` compat | accept `externaldns.k8s.io/v1alpha1 DNSEndpoint` objects as if they were `DNSRecordSet` without `zoneRef` (zone inferred by suffix) | n/a (watch, don't own) | — |
| `BlockList` (namespaced, with cluster-wide via `kudurru-system` ns or `ClusterBlockList`) | a blocklist/allowlist source | `sources[]{url|inline|configMapRef, format: hosts|domains|adblock|regex|rpz}`, `refreshInterval`, `mode: block|allow`, `response: nxdomain|zeroIP|refused|customIP` | `Ready`, `Loaded` (`entries`, `lastFetched`, `etag`, `error`) |
| `ClientGroup` (namespaced or cluster) | Pi-hole/AdGuard-style client grouping | `clients[]{cidr|ip|mac|clientID|hostname}`, `sources{unifiIntegrationRef, kubernetesPods: true}` | `Ready`, `membersResolved` |
| `Policy` (namespaced or cluster) | which lists/upstreams/schedules apply to which clients (AdGuard "client settings", Pi-hole groups) | `clientGroupRefs[]`, `blockListRefs[]`, `allowListRefs[]`, `safeSearch`, `upstreamOverride`, `schedule`, `rewrites[]`, `priority` | `Ready` |
| `Integration` (cluster) | external systems | `type: cloudflare|unifi|rfc2136|webhook`, `secretRef`, `cloudflare{zoneIDFilter[], proxiedByDefault, batchSize}`, `unifi{controllerURL, site, syncClients: true, syncDNSRecords: true}`, `interval` | `Ready`, `Degraded` (rate-limit/auth), `lastSyncTime`, `publishedRecords` |
| `TSIGKey` (cluster) → or just use `Secret` type `kudurru.io/tsig` | XFR/UPDATE auth | prefer typed Secrets like Kuadrant (`type: kudurru.io/tsig`, keys `name`, `algorithm`, `secret`) | — |

Design notes:
- Mirror PowerDNS-Operator's **cluster zone / namespaced records** split; it maps onto k8s RBAC naturally and Kuadrant's `rootHost` idea (every endpoint must be under the zone) is the invariant to enforce with CEL (`self.endpoints.all(e, e.dnsName.endsWith(…))` can't reference another object, so enforce `dnsName`-under-`zoneRef` in the reconciler and set `Ready=False/OutsideZone`).
- Immutable fields via CEL: `zoneName`, `type` (changing primary→secondary is a rebuild), `zoneRef`.
- Duplicate detection across namespaces (same `dnsName`+`type` in two `DNSRecordSet`s) → `Duplicated` reason on the later one, like PowerDNS-Operator; or merge targets when `mergeStrategy: union` is set (a real gap in external-dns).
- Per-zone **publication** rather than per-record integration config: `DNSZone.spec.publish[]` says "records matching `*.lan.example.com` with label `public=true` also go to Cloudflare integration `cf-prod` as proxied CNAMEs" — that is the split-horizon story, and the `Published` condition + `status.integrations[]{name, recordCount, lastApplied, error}` tells the user whether Cloudflare is in sync.
- Status should also expose **what the server will actually answer** (`nameserverAddresses`, `serial`, `dnssec.ds`) so `kubectl get dnszones` is enough to finish delegation at the registrar.
- `kubectl` ergonomics: `shortNames: [zone, rrset, blocklist, cg, pol, integ]`, print columns `ZONE, TYPE, SERIAL, RECORDS, READY, AGE`.

## 13. Source-of-truth model and Docker-mode parity

Problem: in k8s the obvious source of truth is the API server (CRDs are versioned, RBAC'd, GitOps-able, watchable). In `docker run` there is no API server. Three options:

| Model | Description | Verdict |
|---|---|---|
| 1. CRDs **are** the store (k8s) / embedded store mirrors the same Go types (docker) | Desired state lives in exactly one place per mode. kudurru has an internal `Store` interface (`List/Get/Watch/Apply/UpdateStatus` keyed by GVK+namespace+name) with two drivers: `k8s` (controller-runtime client+cache, status via `/status`) and `embedded` (bbolt or SQLite, JSON-encoded objects, same `metadata.generation`/`resourceVersion`/conditions semantics, a local watch bus). Reconcilers and the dataplane only see the interface. | **Recommended.** Same objects, same validation (run the CRD OpenAPI+CEL schema locally with the apiextensions validator — it is a Go library), same `kudurru apply -f zone.yaml` CLI in both modes, same UI. Runtime state (query log, stats, cache, leases, DNSSEC private keys, blocklist compiled sets) is *not* in CRDs in either mode; it goes to the embedded store/Secrets/PVC. |
| 2. Internal store is the truth; a controller imports CRDs into it (CRD → store → serve) | Gravity-like: etcd is truth; CRDs would be an optional "import" surface. | Avoid in k8s: two sources of truth, status lag, and `kubectl` edits lose to the UI (or vice versa). Only acceptable if kudurru insisted on embedded etcd everywhere, which Gravity itself recommends against inside k8s. |
| 3. CRDs only, no Docker mode | — | Violates the requirement. |

Operational consequences of model 1:
- UI/API writes in k8s mode go **through the apiserver** (server-side apply with a `kudurru-ui` field manager); kudurru's API is a thin typed façade (and the TokenReview/SubjectAccessReview path makes k8s RBAC the policy). In docker mode the façade writes to the embedded store; OIDC roles enforce.
- Zone data derived from k8s (Services/Ingress/Gateway) is **never persisted** — it is computed from informers exactly as CoreDNS does; only the `DNSZone` of type `kubernetes`/`external` is stored.
- Multi-replica consistency in k8s comes from the apiserver (all replicas watch the same objects); in a non-k8s multi-node cluster the embedded driver needs replication — Gravity's embedded etcd is one answer, a raft-backed bbolt is another; defer, but keep the `Store` interface so it stays possible.
- Backups: in k8s mode a backup is `kubectl get kudurru.io -A -o yaml` plus the runtime PVC; in docker mode `kudurru backup` dumps the same YAML. One format.
- Migration path CoreDNS → kudurru: a `kudurru import corefile`/`zonefile`/`pihole`/`adguard` that emits `kudurru.io` YAML.

## 14. Deployment manifests to ship

1. `charts/kudurru` (OCI `oci://ghcr.io/<org>/charts/kudurru`) with `crds/` and a sibling `charts/kudurru-crds`; values modelled on the CoreDNS chart plus: `mode: kubernetes|standalone`, `service.dns.{type,port,splitProtocols,loadBalancerIP,loadBalancerClass,externalTrafficPolicy,sharedIPAnnotation}`, `service.doh/dot/doq`, `hostNetwork.enabled` (switches to DaemonSet), `replicaCount`, `podDisruptionBudget`, `topologySpreadConstraints`, `priorityClassName`, `securityContext` (restricted defaults with `NET_BIND_SERVICE`), `probes`, `persistence` (only when `mode=standalone` or query log on PVC), `webhook.enabled` (validating webhook, off by default), `serviceMonitor`, `networkPolicy`, `rbac.aggregateToDefaultRoles`, `externalDNS.webhookSidecar.enabled` (ships kudurru's webhook server as a sidecar *in the external-dns pod* via documented values for the external-dns chart), `integrations.cloudflare.existingSecret`.
2. `deploy/kustomize/base` + overlays `metallb`, `kube-vip`, `cloud-lb-mixed`, `hostnetwork-daemonset`, `single-node-statefulset`; and a generated `deploy/install.yaml` per release.
3. `deploy/docker-compose.yml` (single container, `--mode standalone`, volume `/data`, ports 53/udp+tcp, 853, 443, 8080) and an optional `keepalived` + `dnsdist` companion compose profile for bare-metal VIP/LB (in k8s, kube-vip/MetalLB *are* the VIP; don't ship keepalived inside pods).
4. `deploy/olm/` bundle (registry+v1 FBC) later, only after v1 CRDs stabilize.
5. Example CRs under `config/samples/` for every kind, including the split-horizon example (`DNSZone` with `publish[]` to a Cloudflare `Integration`) and the external-dns `DNSEndpoint` example.

---

## 15. What kudurru should borrow

1. **CoreDNS plugin contract** (`ServeDNS(ctx, w, r) (rcode, err)` + optional capability interfaces `Ready()`, `Transfer()`, metadata) and its ordered-chain semantics for kudurru's internal pipeline (acl → cache → policy/blocking → authoritative sources → forward).
2. **CoreDNS `kubernetes` plugin internals**: informers with stripped objects and name/IP indexes, `HasSynced` gating (SERVFAIL not NXDOMAIN before sync), `startup_timeout`, label/namespace selectors, `Modified()`-derived SOA serial, `transfer` integration, `multisocket` SO_REUSEPORT, `health`+`ready` split with `lameduck`.
3. **external-dns `Endpoint` + `plan.Changes` as the record interchange model**, the **webhook provider protocol** (so kudurru is an external-dns target: GET `/`, GET/POST `/records`, POST `/adjustendpoints`, media type `application/external.dns.webhook+json;version=1`, 204 on apply, 5xx-only retries, sidecar on localhost:8888 + `/healthz` on 8080), acceptance of `DNSEndpoint` CRs, the `external-dns.kubernetes.io/hostname|target|ttl` annotation vocabulary, and the `DNSRecord` registry's `Ready{Accepted|Programmed|Failed}` condition idiom for publication status.
4. **Cloudflare provider mechanics**: token via Secret (`CF_API_TOKEN`, Zone:Read + DNS:Edit), batch API with transactional chunks and per-record fallback, rate-limit awareness, `proxied`/comment/tags as provider-specific properties, zone resolution by public suffix.
5. **k8s_gateway**: Ingress/Gateway-API/LoadBalancer hostname indexes, CRD-presence probing, synthesized apex with the server's own LB IP as glue, the two-NS "secondary" recipe.
6. **Kuadrant**: `rootHost` invariant, immutable `ownerID`, typed provider Secrets with a `default-provider` label, health-check probes as a CRD, label-selected per-zone informers in the dataplane, explicit teardown reasons.
7. **PowerDNS-Operator**: `ClusterZone`/`Zone` + `ClusterRRset`/`RRset` dual scope with `zoneRef{name,kind}`, per-kind viewer/editor/admin ClusterRoles, duplicate detection, `syncStatus` print column, `bundle.yaml` + Helm + kustomize distribution.
8. **kubebuilder/controller-runtime**: conditions per api-conventions (no `phase`), CEL-first validation with bounded lists, finalizers for external cleanup, Lease-based leader election with `NeedLeaderElection()` to split leader-only runnables from always-on servers, aggregated RBAC labels, `kubectl` print columns.
9. **CoreDNS Helm chart** security/probe/service defaults (restricted PSS with `NET_BIND_SERVICE`, uid 65532, `loadBalancerClass`, `trafficDistribution`, PDB, autoscaler, `isClusterService`).
10. **gabe565/AdGuard + Pi-hole charts**: shared-IP two-Service fallback with `externalTrafficPolicy: Local`, `mixedService` toggle, `hostNetwork`/`dnsHostPort` opt-ins — as chart knobs, not defaults.
11. **blocky** image hygiene: non-root uid, file capability `cap_net_bind_service=+p` raised only when needed, works under `drop: [ALL]`; `healthcheck` subcommand.
12. **Gravity**: roles-in-one-binary (`BOOTSTRAP_ROLES`), `/healthz/live|ready`, single-node StatefulSet with PVC — and its honest advice not to run the embedded consensus store inside k8s multi-node.
13. **TokenReview + SubjectAccessReview** with `system:auth-delegator` so k8s identities can use kudurru's API with k8s RBAC as the policy.

## 16. What kudurru should avoid

1. **Two sources of truth** (CRDs *and* an internal store both editable): either CRDs are the truth (k8s mode) or the embedded store is (standalone), behind one `Store` interface. Don't "import" CRDs into etcd like a second-class input.
2. **Embedded etcd/raft as a hard dependency inside Kubernetes** (Gravity's own docs say don't); don't use StatefulSet+PVC for the k8s multi-replica mode when all config is in CRDs — use a Deployment.
3. **Running as root / `NET_ADMIN` / `allowPrivilegeEscalation`** (Pi-hole, AdGuard images in TrueCharts). Must pass the Restricted PSS out of the box.
4. **UDP-only default Services** (k8s_gateway chart) — TCP is mandatory for large answers, DNSSEC, AXFR; ship mixed-protocol by default and the split-services fallback explicitly.
5. **Binding the pod to the VIP** or assuming the LB IP is local (Technitium lesson; Gravity's `INSTANCE_IP` must be hand-set to the LB IP — auto-detect it from `Service.status.loadBalancer.ingress` instead, as k8s_gateway does for its apex).
6. **Answering NXDOMAIN from an unsynced cache**; **restarting pods on config change** (config is CRDs; only bootstrap settings may be a ConfigMap).
7. **Mandatory admission webhooks / cert-manager** for basic install; validate with CRD schema + CEL, webhook optional.
8. **`status.phase` enums**, free-form status strings instead of conditions, non-idempotent reconcilers, one mega-controller for all kinds.
9. **Inventing a new annotation vocabulary** for Ingress/Service hostnames; reuse `external-dns.kubernetes.io/*` (and accept the legacy `external-dns.alpha.kubernetes.io/*`).
10. **Expecting to be an in-tree external-dns provider** — the project will not accept any; only the webhook/CRD/RFC 2136 paths exist.
11. **Per-replica blocklist downloads and per-replica ACME DNS-01** (CoreDNS `tls acme` doc: challenge state is local to a process); do these in the leader and distribute results.
12. **Losing client source IPs** by defaulting to `externalTrafficPolicy: Cluster` — per-client stats and client groups are the product.
13. **Helm CRD upgrade surprises**: don't put CRDs only in `crds/` and forget upgrades; ship the CRD chart/`crds.keep` pattern and document `kubectl apply --server-side -f crds/`.
14. **Keepalived inside pods**: in k8s, VIPs belong to kube-vip/MetalLB/cloud LBs; keep keepalived (and dnsdist) for the bare-metal/compose profile.

## 17. Open questions for the architecture doc

- Embedded store engine for standalone mode (bbolt like CoreDNS `dynupdate` vs SQLite for query-log joins) and whether multi-node standalone clustering is v1 scope.
- Whether the `kubernetes` cluster zone (`cluster.local`) should be a first-class `DNSZone` type at launch (replacing CoreDNS as the in-cluster resolver is a big support surface: autopath, pods verified, MCS, `trafficDistribution`/zonal) or whether v1 forwards `cluster.local` to CoreDNS and only owns the external/LAN zones.
- Mixed-protocol LoadBalancer support on AWS LB controller today (UNVERIFIED); may need the split-services fallback as default on EKS.
- Exact RBAC mapping for the UI in k8s mode: pure delegation (SubjectAccessReview per request) vs. mapping OIDC groups to kudurru roles; both can coexist but the precedence rule must be written down.
- Whether to also *consume* external-dns (kudurru → Cloudflare via an external-dns `DNSEndpoint` + Cloudflare provider) instead of a native Cloudflare `Integration`. Native gives status/conditions and batch control; external-dns gives zero code but no UI status. Recommendation: native integration using the same `Endpoint`/`Changes` types, document the external-dns alternative.
- License: Gravity is GPL-3.0 — borrow ideas, not code. CoreDNS, external-dns, Kuadrant, PowerDNS-Operator, controller-runtime are Apache-2.0 and may be vendored.
