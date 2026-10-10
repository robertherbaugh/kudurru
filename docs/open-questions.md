# Open questions for the project owner

These are the decisions the architecture leaves to the project owner. Each has a proposed default that the v0.1 plan assumes; answer them before the v0.1 schedule is committed. Record each answer as a decision in [architecture.md §16](architecture.md#16-decision-log).

| # | Question | Proposed default | Why it matters |
|---|---|---|---|
| 1 | **Project home.** Module `github.com/robertherbaugh/kudurru`, images under `ghcr.io/robertherbaugh/`, CRD group `kudurru.io`. Is the `kudurru.io` domain owned or wanted? | Keep as is; the CRD group is the only thing that is painful to change later | A CRD group rename after v0.1 means a migration for every user |
| 2 | **Compose HA ladder.** v0.1 static twins (file-managed, read-only UI), v0.2 follower mode with fenced promote, v1.0 raft. | Accept the ladder | Pulling raft forward costs the Cloudflare and UniFi integrations in v0.2 |
| 3 | **Sessions.** Sealed cookies in v0.1 (no mid-session group refresh, no session listing); SQLite session store in v0.2 for Docker; external store option for Kubernetes in v1.0. | Accept | A Kubernetes Deployment has no shared database in v0.1 |
| 4 | **Kubernetes as a DNS data source.** `external` zones from Ingress/Gateway/LoadBalancer hostnames in v0.2 only, or also replace CoreDNS for `cluster.local`? | External zones only; `cluster.local` stays with CoreDNS before 1.x | CoreDNS-grade conformance is a large, separate burden |
| 5 | **Blocked-services and safe-search data.** Maintain a kudurru-owned permissively licensed catalogue, or drop those features? AdGuard's tables are GPL. | Own catalogue, v0.2 | Licensing of embedded data, not just code |
| 6 | **DHCP.** Out of scope for 1.0; UniFi or the router owns it and kudurru reads inventory from UniFi. | Confirm out of scope | DHCP was the largest bug cluster in both Gravity and Technitium |
| 7 | **Default block response.** `zeroIP` with a 10 minute TTL and EDE 15, or `nxdomain`? | `zeroIP` | One-click allow feels responsive with a short TTL |
| 8 | **Query log defaults.** Privacy `full`, raw rows kept 30 days up to 10 GiB, answer summary stored only in `full`. | Accept | Privacy posture of the project |
| 9 | **Cloudflare ownership marker.** Comment only (works on the Free plan) or comment plus tag; honour and produce external-dns TXT heritage records? | Comment, plus tag when the plan allows; read heritage TXT, never write it | Coexistence with a parallel external-dns |
| 10 | **UniFi legacy API.** Allow undocumented endpoints behind a flag for alias, fixed-IP and VLAN data, or official API only? | Behind a flag, with version detection | Official API lacks some inventory fields |
| 11 | **Edge default in Helm.** dnsdist off by default; flip on once console diff-apply is proven? | Off | Fewer moving parts for a first install |
| 12 | **Pi-hole API façade.** Optional module in v1.0, or pull into v0.2 for Home Assistant, Homarr and PADD users? | v1.0 | Protects v0.2 scope |
| 13 | **Transports.** Implement DoQ and DoH3 natively in v0.2, or rely on dnsdist for QUIC? | Native in v0.2 | Core size versus "extreme feature set" |
| 14 | **Hardware floor.** Raspberry Pi 4 class (arm64, 1 GB) is supported; is `armv7` a release target? | arm64 and amd64 only | Build matrix and testing cost |
| 15 | **Authorization in Kubernetes.** SubjectAccessReview by default; apiserver impersonation as a v0.2 opt-in. | Confirm | The impersonate grant makes a compromised DNS server cluster-admin-equivalent |
| 16 | **Frontend stack.** React + TypeScript + shadcn/ui, or Lit + PatternFly (Gravity's stack)? | React | Contributor pool and component ecosystem |
| 17 | **keepalived build.** Compile 2.4.3 from the upstream tarball (gets `auth_hmac`; kudurru carries the GPL source offer) or ship Alpine's 2.3.4 package (no authenticated VRRP)? | Compile 2.4.3 | Authenticated VRRP adverts versus image reproducibility |
| 18 | **VIP drain policy.** `kudurru lameduck` moves the VIP (FAULT) on every rolling restart of the MASTER and, with `nopreempt`, it does not come back. Acceptable, or should lameduck only drain dnsdist backends? | Move the VIP | Behaviour during routine upgrades |
| 19 | **PROXYv2 authentication.** Shared-secret TLV in v0.1 (proposed) versus NetworkPolicy and host firewalls only; should the loopback compose profile also require it? | TLV required for non-loopback peers; loopback exempt | Any neighbour in a trusted CIDR could otherwise forge client addresses |
| 20 | **RPZ.** Add response policy zones (BIND/Knot-compatible blocklist transport) in v0.2 or v1.0, or leave reserved? | Leave reserved | Lets kudurru publish its blocklists to other resolvers |
| 21 | **Encryption key rotation.** Dual-key window via `KUDURRU_ENCRYPTION_KEY_PREVIOUS`, or an explicit `kudurru admin rotate-key` that rewrites the store in place? | Dual-key window | Operator UX for a rare but critical operation |

## Week-1 spikes before the schedule is committed

Independent of the answers above, these must be measured before v0.1 milestones are dated:

1. dnsdist 2.1 started from YAML plus console `newServer` and `reloadAllCertificates()`: does the console work under YAML config?
2. PROXYv2 with the shared-secret TLV plus `chashed` backend selection plus health-check validation function smoke test.
3. The `kube` store driver passing the conformance suite on envtest, including restoring from `local.db` with the apiserver stopped.
4. blocky matcher port with the benchmark baseline (p99 lookup under 2 µs, under 150 MB RSS for 3 M entries).
5. go-oidc backend-for-frontend login against dex with sealed sessions and `sessionsValidAfter`.
6. Mixed-protocol LoadBalancer Service on kind with MetalLB and on one cloud.
7. Binary-size and build-time cost of the apiextensions and CEL validator packages in the Docker binary.
8. keepalived 2.4.3 with `auth_hmac`, `track_file` and the notify FIFO on two VMs, recording failover timing for node loss, FAULT drain and the weight-60 no-op.
