# Open questions for the project owner

These were the decisions the architecture left to the project owner. Answers are recorded as decisions in [architecture.md §16](architecture.md#16-decision-log). Numbers match §15 of the architecture. New questions go in a fresh open table above the Decided table.

**The first 21 questions were answered by the project owner on 2026-10-10 and 2026-10-11.** The Decided table below is the record; the architecture decision log holds the rationale.

## Open

| # | Question | Raised | Context |
|---|---|---|---|
| 22 | **License portal go-live.** (a) The production OIDC issuer: customers and staff sign in at the same one, so it must allow customer sign-up (or invites) and verify email addresses. (b) A standby signing key: commit a standby public key such as `internal/license/keys/kudurru-2026-2.pub`, with its seed unused in Infisical, so the first rotation (even after a leak) needs no node upgrade. (c) Whether the issue wizard offers the Enterprise and Trial plans to admins (they are behind "More plans"; the public copy names only personal and business keys). | 2026-10-12 | D165, `license-server/README.md` |

## Decided

| # | Decision | Date | Log |
|---|---|---|---|
| 1 | **Project home.** Module `github.com/robertherbaugh/kudurru`, images under `ghcr.io/robertherbaugh/`, CRD group `kudurru.io`. | 2026-10-10 | D49 |
| 2 | **Compose HA ladder.** v0.1 static twins, v0.2 follower mode with fenced promote, v1.0 raft. | 2026-10-10 | D50 |
| 3 | **Sessions.** Sealed cookies in v0.1 for both modes; SQLite session store in v0.2 for Docker; optional external store for Kubernetes in v1.0. | 2026-10-10 | D54 |
| 4 | **Kubernetes as a DNS data source.** `external` zones from Ingress, Gateway API routes and LoadBalancer Services in v0.2; `cluster.local` stays with CoreDNS before 1.x. | 2026-10-10 | D64 |
| 5 | **Blocked-services and safe-search data.** A kudurru-owned, permissively licensed catalogue in a separate data repository, shipped in **v0.1** (M2). | 2026-10-10 | D60; revised by D120 (embedded catalogue in v0.1, the data repository waits for the owner) |
| 6 | **DHCP.** Opt-in DHCPv4 planned for v0.2.0 on one Linux/SQLite node; v0.1.0 uses an external DHCP server. DHCPv6 and multi-node failover remain deferred. | 2026-10-16 (revised) | [PR #66](https://github.com/robertherbaugh/kudurru/pull/66), revises D42/D52 |
| 7 | **Default block response.** `zeroIP` with a 10 minute TTL and EDE 15. | 2026-10-10 | D58 |
| 8 | **Query log defaults.** Privacy `full`, raw rows 30 days up to 10 GiB, answer summary only in `full`, rollups 35 d / 400 d / forever. | 2026-10-10 | D59 |
| 9 | **Cloudflare ownership marker.** Record comment `kudurru:owner=<clusterID>` plus a tag when the plan allows; external-dns heritage TXT records are read for adoption, never written. | 2026-10-10 | D62 |
| 10 | **UniFi legacy API.** Official Integration API by default; legacy controller endpoints behind a flag with version detection and per-endpoint 404 tolerance. | 2026-10-10 | D63 |
| 11 | **Edge default in Helm.** dnsdist off by default in v0.1; revisit once console diff-apply is proven in v0.2. | 2026-10-10 | D65 |
| 12 | **Pi-hole API façade.** Optional module in v1.0. | 2026-10-10 | D66 |
| 13 | **Transports.** DoQ and DoH3 are native in **v0.1**: all five transports (Do53, DoT, DoH, DoQ, DoH3) ship in milestone M1. | 2026-10-10 | D67 |
| 14 | **Hardware floor.** Raspberry Pi 4 class (arm64, 1 GB) supported; release targets are linux amd64 and arm64 only. | 2026-10-10 | D68 |
| 15 | **Authorization in Kubernetes.** SubjectAccessReview by default; apiserver impersonation stays a v0.2 opt-in. | 2026-10-10 | D55 |
| 16 | **Frontend stack.** Bun + React 19 + TypeScript + Astryx (Meta's design system, MIT); uPlot/ECharts for charts until `@astryxdesign/charts` leaves canary. | 2026-10-10 | D69 |
| 17 | **keepalived build.** Compile 2.4.3 from the upstream tarball with the GPL source offer. | 2026-10-10 | D53 |
| 18 | **VIP drain policy.** Lameduck keeps the VIP and only drains the node from dnsdist; the VIP moves on real failure or an explicit `kudurru vip drain`. | 2026-10-10 | D51 |
| 19 | **PROXYv2 authentication.** Exact peer allow-list plus the shared-secret TLV for every non-loopback peer; the loopback compose profile is exempt. | 2026-10-10 | D56 |
| 20 | **RPZ.** Import RPZ feeds as a BlockList source in v0.2; publish kudurru's own blocklists as transferable RPZ zones in v1.0. | 2026-10-10 | D61 |
| 21 | **Encryption key rotation.** Dual-key window via `KUDURRU_ENCRYPTION_KEY_PREVIOUS`; stored secrets are re-encrypted on start and old sessions stay valid until their lifetime ends. | 2026-10-10 | D57 |

## Week-1 spikes before the schedule is committed

These were the original pre-milestone measurement requirements. Items 3,
4, 5 and 7, plus transport spike (i), passed during development (reports in
[spikes/](spikes/)). Item 6 now runs on CI with kind + MetalLB, and the
[final feature review](m5/features-v0.1.md) records later cluster validation.
The cloud half and real-network dnsdist/VRRP checks require deployment
evidence; use [release-checklist.md](release-checklist.md#3-checks-before-ga-ci-and-the-owners-hardware).
This list preserves the original questions and does not certify later
hardware checks.

1. dnsdist 2.1 started from YAML plus console `newServer` and `reloadAllCertificates()`: does the console work under YAML config?
2. PROXYv2 with the shared-secret TLV plus `chashed` backend selection plus health-check validation function smoke test.
3. The `kube` store driver passing the conformance suite on envtest, including restoring from `local.db` with the apiserver stopped.
4. blocky matcher port with the benchmark baseline (p99 lookup under 2 µs, under 150 MB RSS for 3 M entries).
5. go-oidc backend-for-frontend login against dex with sealed sessions and `sessionsValidAfter`.
6. Mixed-protocol LoadBalancer Service on kind with MetalLB and on one cloud.
7. Binary-size and build-time cost of the apiextensions and CEL validator packages in the Docker binary.
8. keepalived 2.4.3 with `auth_hmac`, `track_file` and the notify FIFO on two VMs, recording failover timing for node loss, FAULT drain and the weight-60 no-op.
