> **Research note, 2026-10-10.** Point-in-time study produced during the kudurru design phase and cited by [docs/architecture.md](../architecture.md). It describes other projects for analysis only; nothing in it is to be ported into kudurru (several of them are GPL/EUPL). Items marked UNVERIFIED were not confirmed against a primary source. Verify before relying on a version number or API detail.

# Technitium DNS Server — Research Report for kudurru

Research key: `technitium`
Date: 2026-10-10
Sources: cloned `TechnitiumSoftware/DnsServer` and `TechnitiumSoftware/TechnitiumLibrary` (HEAD at research time), APIDOCS.md, CHANGELOG.md, Dockerfile/compose, per-app README/`dnsApp.config`, GitHub API, Docker Hub API, Technitium blog posts (v14, clustering, catalog zones, v15).

---

## 0. Verified facts at a glance

| Item | Value | Source |
|---|---|---|
| Latest release | **v15.6** (tag `v15.6.0`), published 2026-10-03 | GitHub releases API; `DnsServerCore.csproj` `<Version>15.6</Version>` |
| License | **GPL-3.0** (both DnsServer and TechnitiumLibrary) | `LICENSE` files; GitHub `spdx_id: GPL-3.0` |
| Language / runtime | C# on **.NET 10** (ASP.NET Core / Kestrel for HTTP, `libmsquic` for QUIC) | csproj, Dockerfile `FROM mcr.microsoft.com/dotnet/aspnet:10.0` |
| Repo stats | 10,088 stars, 797 forks, 154 open issues, created 2017-10-29, pushed 2026-10-10 | GitHub API |
| Docker image | `technitium/dns-server` — tags `latest`, `15.6.0`, `15.5.1`, ...; arch `linux/amd64`, `linux/arm64`, `linux/arm`; 17.09M pulls | Docker Hub API |
| App Store | 27 official apps, manifest at `https://download.technitium.com/dns/apps/apps2.json` (redirect from `go.technitium.com/?id=44`) | `DnsApplicationManager.APP_STORE_URI`, fetched manifest |
| Key deps | `BouncyCastle.Cryptography 2.7.0` (Ed25519/Ed448), `Microsoft.AspNetCore.Authentication.OpenIdConnect 10.0.12`, `QRCoder 1.8.0` (TOTP), `System.DirectoryServices.Protocols 10.0.12` (LDAP) | `DnsServerCore.csproj` |

**Licensing caveat.** Technitium DNS Server and its library are GPL-3.0. kudurru is source-available (PolyForm Noncommercial; Apache-2.0 when this note was written). **No code, config-file formats-by-copy, UI markup, or app source may be ported.** Only ideas, protocol behaviors, data-model shapes and UX patterns described here are usable. Even the proprietary record type numbers (65280–65284, 65357) are "private use" ranges; kudurru can use the same private-use space but should pick its own numbers and names.

---

## 1. Executive summary

Technitium is a single-binary, single-process, "everything built in" DNS server: authoritative + recursive + forwarder + blocker + DHCP + web console + HTTP API, with a plugin ("DNS Apps") system for anything else. Its differentiators relative to BIND/Knot/PowerDNS/Unbound/Pi-hole/AdGuard:

1. **One daemon does authoritative + recursive + blocking** with a coherent zone model where *forwarding* and *app logic* are expressed as zone records (`FWD`, `APP`).
2. **Very complete zone-type matrix**: Primary, Secondary, Stub, Conditional Forwarder, Secondary Forwarder, Catalog, Secondary Catalog (RFC 9432), with per-zone ACLs for query/transfer/notify/update and TSIG-scoped RFC 2136 policies.
3. **Full DNSSEC signer** with automated key lifecycle (RFC 6781/7583 state machine, ZSK auto-rollover, KSK DS-polling for activation, RFC 5011 revoke step), NSEC/NSEC3 conversion, user-supplied PEM keys.
4. **All encrypted transports both ways**: server side DoT/DoH(1.1/2/3)/DoQ/DoH-plain/PROXY-protocol/Unix sockets; client side UDP/TCP/TLS/HTTPS/QUIC forwarders with HTTP/SOCKS5 proxy (Tor).
5. **Pluggable pipeline with 8 hook interfaces** (request controller, blocking handler, authoritative handler, APP-record handler, post-processor, query logger, query-log reader, preference ordering) and a ~27-app store.
6. **Clustering (v14, Nov 2025)**: primary/secondary *management-plane* replication over the HTTP API with DANE-EE node auth, heartbeat + periodic config pull + NOTIFY-style push, plus a cluster catalog zone for data-plane zone replication. It is **not** consensus/HA; the primary must be up to make changes; DHCP not clustered.
7. **RBAC**: users, groups, 11 permission sections × {View, Modify, Delete}, per-zone sub-item permissions, non-expiring API tokens, single-use tokens, TOTP 2FA, OIDC SSO with group mapping, LDAP.

Weak points for kudurru's purposes: custom **binary** config/zone file formats (`DS`/`DZ`/`AS` magic + version byte), no declarative config file, no Kubernetes story at all, clustering is a bespoke primary/secondary protocol rather than a replicated store, UI is jQuery/Bootstrap-3, and the ~8,000-line `DnsServer.cs` God-class is not a model to copy.

---

## 2. Repository / package layout

```
DnsServer/
├── DnsServerApp/                   # console host (entry: DnsServerApp.dll <configFolder>)
├── DnsServerCore/                  # the server (library)
│   ├── Dns/
│   │   ├── DnsServer.cs            # 8,032 lines: listeners, pipeline, resolver, settings
│   │   ├── StatsManager.cs         # 2,786 lines: dashboard stats
│   │   ├── Applications/           # DnsApplication(Manager), AssemblyLoadContext
│   │   ├── Dnssec/                 # DnssecPrivateKey (+Rsa/Ecdsa/Eddsa)
│   │   ├── ResourceRecords/        # per-record metadata (GenericRecordInfo etc.)
│   │   ├── Trees/                  # ZoneTree / AuthZoneTree / CacheZoneTree (byte-tree)
│   │   ├── ZoneManagers/           # Auth, Cache, Allowed, Blocked, BlockList, Special
│   │   └── Zones/                  # ApexZone, PrimaryZone, SecondaryZone, StubZone, ForwarderZone,
│   │                               #   SecondaryForwarderZone, CatalogZone, SecondaryCatalogZone, ...
│   ├── Auth/                       # AuthManager, User, Group, Permission, UserSession, LdapAuthProvider
│   ├── Cluster/                    # ClusterManager (100 KB), ClusterNode, InternalDnsClient
│   ├── Dhcp/                       # DhcpServer, Scope, Lease, Options/
│   ├── DnsWebService.cs            # Kestrel host for console+API (3,048 lines)
│   ├── WebService*Api.cs           # Auth, Zones, OtherZones, Settings, Dashboard, Apps, Logs, Dhcp, Cluster
│   ├── www/                        # admin SPA: jQuery + Bootstrap + Chart.js + moment (index.html + js/*.js)
│   └── dohwww/                     # DoH help page
├── DnsServerCore.ApplicationCommon/ # the plugin SDK: IDnsApplication etc. (10 small files)
├── DnsServerCore.HttpApi/          # typed C# client of the HTTP API (used by clustering)
├── Apps/                           # 27 official DNS Apps, one project each
├── APIDOCS.md                      # 278 KB hand-written HTTP API reference
├── DockerEnvironmentVariables.md, EnvironmentVariables.md, docker-compose.yml, Dockerfile
TechnitiumLibrary/
└── TechnitiumLibrary.Net/Dns/      # DnsClient, DnsDatagram, DnsCache, ZoneFile (BIND parser),
    ├── ClientConnection/           #   Udp/Tcp/Tls/Https/Quic client connections
    ├── Dnssec/                     #   public-key validation
    ├── EDnsOptions/                #   ECS, EXPIRE, EDE
    └── ResourceRecords/            #   37 RDATA classes incl. ANAME/ALIAS/FWD/APP
```

Notable: the DNS wire/protocol library is a *separate* reusable library; the plugin SDK is a *separate* tiny assembly so apps only depend on it plus the Net library.

---

## 3. Request-processing pipeline (hook points)

Reconstructed from `DnsServer.ProcessRequestAsync` / `ProcessQueryAsync` / `ProcessAuthoritativeQueryAsync` / `ProcessRecursiveQueryAsync`:

```
listener (UDP/TCP/TLS/QUIC/HTTP(S)/PROXY)        -- QPM rate-limit check per client prefix
  └─ ProcessRequestAsync
       1. IDnsRequestController.GetRequestActionAsync  → Allow | DropSilently | DropWithRefused
       2. parse errors → FORMERR; invalid qname → FORMERR
       3. TSIG verify (if signed) → unsigned request continues, response re-signed
       4. EDNS version check → BADVERS
       5. ProcessQueryAsync (by OPCODE)
            QUERY:
              AXFR/IXFR → ProcessZoneTransferQueryAsync (ACL + TSIG)
              else      → ProcessAuthoritativeQueryAsync
                           a. AuthZoneManager.Query (local zones, incl. query-access ACL)
                           b. SpecialZoneManager (RFC 6303 locally-served zones) if recursion
                           c. IDnsAuthoritativeRequestHandler apps (ordered by Preference)
                           d. answer post-processing: CNAME chase, ANAME/ALIAS flatten,
                              FWD → conditional forwarding, APP → IDnsAppRecordRequestHandler,
                              NS referral + RD → forced recursion
              if no auth answer and RD+recursion allowed → ProcessRecursiveQueryAsync
                           a. IsAllowedAsync: bypass-list → allowed zone / block-list allow →
                              IDnsRequestBlockingHandler.IsAllowedAsync
                           b. if not allowed: BlockedZone → BlockListZone → IDnsRequestBlockingHandler.ProcessRequestAsync
                           c. RecursiveResolveAsync (cache → prefetch / serve-stale → upstream)
                           d. CNAME cloaking re-check against block lists
            NOTIFY → ProcessNotifyQueryAsync (ACL by zone NS / notifyAllowedNetworks)
            UPDATE → ProcessUpdateQueryAsync (RFC 2136 with ACL + TSIG policies)
       6. PostProcessQueryAsync: IDnsPostProcessor apps (may rewrite/drop), EDNS fixups
  └─ StatsManager + LogManager + IDnsQueryLogger apps (async channel)
```

Key design points worth noting:
- **Blocking is evaluated only on the recursive path** — authoritative local zones always win over block lists (so you cannot accidentally block your own zone).
- Every response carries a `Tag` of `DnsServerResponseType` (`Authoritative, Recursive, Cached, Blocked, UpstreamBlocked, UpstreamBlockedCached, Dropped`) which drives stats and logs.
- Apps are invoked in try/catch; an app exception is logged and skipped, never fails the query.

---

## 4. Zone model

### 4.1 Zone types (`AuthZoneType`)

| Type | Enum | Role | Notes |
|---|---|---|---|
| Primary | 1 | authoritative master | DNSSEC signing, dynamic updates, zone history for IXFR, SOA serial date scheme option, record expiry TTL |
| Secondary | 2 | AXFR/IXFR slave | XFR over TCP/TLS/QUIC, TSIG, ZONEMD validation (RFC 8976), can accept RFC 2136 (forwards semantics via ACL), "Secondary Root Zone" shortcut (RFC 8806) |
| Stub | 3 | caches NS/SOA/glue of a zone, resolves via them | optional explicit primary addresses |
| Forwarder ("Conditional Forwarder") | 4 | contains `FWD` records; everything under it is forwarded | since v13 has a dummy SOA so it can be transferred; supports NS "static stub" trick, `this-server` forwarder, priority, per-FWD DNSSEC flag, per-FWD proxy, Zone File Editor (v15.5), NTA via EDE (v15.5) |
| SecondaryForwarder | 5 | XFR copy of a Forwarder zone | introduced v13 |
| Catalog | 6 | RFC 9432 catalog producer | member zones may be Primary/Secondary(v14.3)/Stub/Forwarder |
| SecondaryCatalog | 7 | RFC 9432 consumer; auto-provisions member zones locally | must be created manually once per secondary |

Zones can be enabled/disabled, cloned, converted between types (`/api/zones/convert`), imported from BIND zone text (incl. "overwrite entire zone" mode), exported to BIND text, and resynced. Reverse zones are auto-named from IP/CIDR input (`zone=192.168.1.0/24` creates `1.168.192.in-addr.arpa`).

### 4.2 Per-zone options (`AuthZoneInfo`) — the "zone policy" model

```csharp
AuthZoneQueryAccess { Deny, Allow, AllowOnlyPrivateNetworks, AllowOnlyZoneNameServers,
                      UseSpecifiedNetworkACL, AllowZoneNameServersAndUseSpecifiedNetworkACL }
AuthZoneTransfer    { Deny, Allow, AllowOnlyZoneNameServers, UseSpecifiedNetworkACL,
                      AllowZoneNameServersAndUseSpecifiedNetworkACL }
AuthZoneNotify      { None, ZoneNameServers, SpecifiedNameServers, BothZoneAndSpecifiedNameServers,
                      SeparateNameServersForCatalogAndMemberZones }
AuthZoneUpdate      { Deny, Allow, AllowOnlyZoneNameServers, UseSpecifiedNetworkACL,
                      AllowZoneNameServersAndUseSpecifiedNetworkACL }
```

Fields persisted per zone: `name, type, lastModified, disabled, catalogZoneName, overrideCatalog{QueryAccess,ZoneTransfer,Notify,PrimaryNameServers}, queryAccess + ACL, zoneTransfer + ACL + tsigKeyNames, zoneHistory[] (IXFR), notify + notifyNameServers + notifySecondaryCatalogNameServers, update + updateNetworkACL + updateSecurityPolicies, dnssecPrivateKeys[] (primary), primaryNameServerAddresses[], primaryZoneTransferProtocol, primaryZoneTransferTsigKeyName, expiry, validateZone, validationFailed`.

Network ACL syntax (used everywhere): comma-separated list, `!` prefix denies, first match wins, default deny: `192.168.10.0/24, !192.168.10.2`.

**RFC 2136 security policies** (TSIG-scoped, per-name, per-type):

```json
"updateSecurityPolicies": [
  { "tsigKeyName": "key.example.com", "domain": "example.com",   "allowedTypes": ["A","AAAA"] },
  { "tsigKeyName": "key.example.com", "domain": "*.example.com", "allowedTypes": ["ANY"] }
]
```
(API wire form is a pipe-separated table: `key|domain|A,AAAA|key|*.domain|ANY`.)

### 4.3 Catalog zones (RFC 9432) — how member properties are encoded

Catalog member records are plain zone records; Technitium adds `.ext.` properties (TXT) under each member's unique label:

```
zone-type.ext.<id>.zones.<catalog>                 TXT "primary|secondary|stub|forwarder"
allow-query.ext.<id>.zones.<catalog>               (ACL)
allow-transfer.ext.<id>.zones.<catalog>            (ACL)
transfer-tsig-key-names.ext.<id>.zones.<catalog>
primary-addresses.ext.<id>.zones.<catalog>
primary-transfer-protocol.ext.<id>.zones.<catalog>
primary-transfer-tsig-key-name.ext.<id>.zones.<catalog>
zonemd-validation.ext.<id>.zones.<catalog>
```

Member zones inherit the catalog's Zone Options unless `overrideCatalog*` is set. Secondary side: create a `SecondaryCatalog` zone with primary address + TSIG; member zones appear automatically ("within roughly 10 seconds"); deleting the secondary catalog deletes provisioned zones; the secondary catalog never modifies a pre-existing same-named zone. The *cluster* feature reuses this: it creates `cluster-catalog.<clusterDomain>`.

### 4.4 Record types

Standard RDATA implemented (library `ResourceRecords/`): A, AAAA, NS, CNAME, SOA, PTR, HINFO, MX, TXT (multi-string, base64 char-strings, Unicode), RP, NAPTR, DNAME, APL, DS, SSHFP, RRSIG, NSEC, DNSKEY, NSEC3, NSEC3PARAM, TLSA (with PEM→hash helper), ZONEMD, SVCB/HTTPS (with *automatic ipv4/ipv6 hints*), URI, CAA, TSIG, OPT, SRV; unknown types via RFC 3597 hex `rdata`.

Proprietary / private-use types:

| Type | Code | Semantics |
|---|---|---|
| `ANAME` | 65280 | CNAME-at-apex / flattening: server resolves target and returns A/AAAA. Multiple allowed. Chased in `ProcessANAMEAsync`. |
| `FWD` | 65281 | Conditional forwarder record: `{protocol, forwarder, dnssecValidation, proxyType(DefaultProxy/None/Http/Socks5/NoProxy), proxyAddress/Port/User/Pass, priority}`; special forwarder `this-server`; comment text doubles as EDE NTA reason. |
| `APP` | 65282 | `{appName, classPath, data(JSON, comments allowed)}` — delegates the name (and its subtree if wildcard) to an installed app class implementing `IDnsAppRecordRequestHandler`. |
| `CHILD_NS` / `PARENT_NS` | 65283/4 | cache-internal (draft-fujiwara resolver-update); not user-facing. |
| `ALIAS` | 65357 | "Simple DNS" compatible ALIAS = ANAME with explicit type. |

Per-record metadata (`GenericRecordInfo`): `disabled, comments, lastModified, expiryTtl (auto-delete), lastUsedOn (in-memory)`; NS records carry glue; SOA carries `useSoaSerialDateScheme`; SVCB carries auto-hint flags. Deleted records are kept in `HistoryRecordInfo` with `deletedOn` for IXFR.

---

## 5. DNSSEC signing (Primary zones)

- Algorithms: RSA (MD5/SHA1/SHA256/SHA512, key size), ECDSA P-256/P-384, Ed25519/Ed448 (RFC 8080 via BouncyCastle). Validation side additionally supports all of `DnssecAlgorithm` incl. unsupported-algo downgrade protection (v14).
- Proof of non-existence: NSEC or NSEC3 (iterations 0–50, salt 0–32 bytes), convertible either way live (`convertToNSEC`, `convertToNSEC3`, `updateNSEC3Params`), ENT handling, wildcard proofs.
- Keys: `DnssecPrivateKey {algorithm, keyType(KSK/ZSK), state, stateChangedOn, stateTransitionBy, isRetiring, rolloverDays}`; user-supplied PEM keys accepted at sign time and for added keys.
- **Key state machine** (RFC 7583 states, exact enum): `Generated → Published → Ready → Active → Retired → Dead → Removed`, plus `Revoked` (RFC 5011 consideration for KSK). A per-zone timer (initial 30 s, then every 15 min) advances states:
  - Published→Ready after `maxRecordTtl + propagationDelay` where `propagationDelay = SOA.refresh + SOA.retry` (worst-case secondary sync).
  - **KSK Ready→Active only after the server observes the DS at the parent** (`GetDSPublishedPrivateKeysAsync` polls parent); v15 added a manual "activate KSK" override to stop polling.
  - ZSK auto-rollover when `now > stateChangedOn + rolloverDays` (default 30 at sign time via API, 90 for added keys; range 0–365; KSK default 0 = manual).
  - Retire → Revoke (KSK) → Dead → Removed handled automatically; "Rollover DNSKEY" and "Retire DNSKEY" API calls exist; "Publish All Private Keys" to force.
- Secondary zones can validate transferred zones with ZONEMD. Signed zones in a cluster have their private keys synced (`zones/<name>.keys` in the config zip).
- DS helper: `GET /api/zones/dnssec/dsinfo` returns DS records for parent.

---

## 6. Zone transfer, NOTIFY, TSIG, dynamic update

- AXFR (RFC 5936) and IXFR (RFC 1995) in and out; zone history kept in-memory and persisted in the zone file for IXFR; `CleanupHistory` bounds it.
- Transport: XFR over TCP, **TLS (RFC 9103)** and **QUIC** (`zoneTransferProtocol = Tcp|Tls|Quic`).
- NOTIFY (RFC 1996) with per-zone target selection; failed notify targets exposed in zone options (`notifyFailed`, `notifyFailedFor[]`); inbound NOTIFY ACL (`notifyAllowedNetworks`).
- Secondary refresh: SOA check every tick (`REFRESH_TIMER_INTERVAL` 5 s scheduler), 10 s SOA timeout, 120 s XFR timeout, 2 retries, IXFR unless expired/resync, minimum SOA refresh/retry floors configurable globally.
- TSIG (RFC 8945): global key store (`name, sharedSecret(base64), algorithmName`), algorithms HMAC-MD5/SHA1/SHA224/SHA256/SHA256-128/SHA384/SHA384-192/SHA512/SHA512-256; used for XFR auth (per-zone key allow-list), update policies, and cluster zone transfers.
- RFC 2136 UPDATE: prerequisite handling (NXDOMAIN/YXDOMAIN/NXRRSET/YXRRSET/NOTZONE/NOTAUTH), ACL + TSIG policy per name/type, supported on Primary; on Secondary/Forwarder it is accepted and forwarded/applied per ACL.
- Zone-level expiry handling and `Resync` API for secondaries.

---

## 7. Transports and certificates

Server-side "Optional Protocols" (all off by default except plain 53):

| Protocol | Default port | Notes |
|---|---|---|
| DNS-over-UDP/TCP | 53 | UDP socket pooling, send/recv buffer sizes, out-of-order TCP (RFC 7766), UDP payload 1232 default |
| DNS-over-TLS | 853/tcp | Kestrel-free raw `SslStream` |
| DNS-over-HTTPS | 443/tcp (+443/udp for HTTP/3) | Kestrel; `GET`/`POST /dns-query`, `application/dns-message`; HTTP/1.1, 2, 3; help page redirect optional; `X-Real-IP` from allowed reverse proxies |
| DNS-over-HTTP (plain) | 80 or 8053 | for TLS-terminating reverse proxy; also **Unix domain socket** (v15.3/15.4) |
| DNS-over-QUIC | 853/udp | RFC 9250, needs `libmsquic`; idle timeout / max streams tunable |
| DNS-over-UDP-PROXY / TCP-PROXY | 538 | HAProxy PROXY protocol v1/v2, ACL for allowed proxies |

Certificates: **PKCS#12 only (`.pfx`/`.p12`) + password**, separate for web console and DNS services; a timer re-reads the file when its mtime changes (hot reload); self-signed cert auto-generated (5-year, SAN includes local addresses) for console / cluster bootstrap. **No ACME client** — the documented path is certbot + DNS-01 against Technitium's own API, then convert PEM→PFX and copy into the config dir (v15's non-root service cannot read root-owned certbot files). Client-side forwarders: `Udp|Tcp|Tls|Https|Quic` (plus legacy JSON DoH), address format `name (ip:port)` / `https://host/dns-query (ip)`, optional HTTP/SOCKS5 proxy, concurrency (`forwarderConcurrency`), retries/timeouts, `concurrentForwarding` toggle.

---

## 8. Recursive resolver features

- Root hints + root trust anchors shipped (`named.root`, `root-anchors.xml`); RFC 8109/9609 priming; optional local root zone (RFC 8806).
- Recursion ACL modes: `Deny | Allow | AllowOnlyForPrivateNetworks | UseSpecifiedNetworkACL`.
- QNAME minimisation (RFC 9156), 0x20 case randomisation, resolver concurrency (`resolverConcurrency`) with **latency-based name-server selection**, retries/timeouts, max stack depth (NXNSAttack/NRDelegation mitigations), max concurrent resolutions per core queue (v13.3), IPv6 mode (v15), parent/child NS separation (draft-fujiwara), spoof-detection→TCP fallback (RebirthDay CVE mitigation), bailiwick checks, KeyTrap limits (max 4 colliding DNSKEYs, 16 crypto failures, suspension budget), NSEC3 iteration cap 100.
- DNSSEC validation (RSA/ECDSA/EdDSA, NSEC/NSEC3), CD/DO/AD handling, EDE (RFC 8914) incl. NTA disclosure.
- **ECS (RFC 7871)**: outbound ECS with prefix lengths 24/56, override subnet, and v15 "ECS source address" (treat inbound ECS as client IP — useful behind dnsdist-like proxies).
- Locally-served zones (RFC 6303/6761) built-in and overridable by Stub/Forwarder zones.
- **QPM rate limiting**: per-prefix limits (`qpmPrefixLimitsIPv4: [{prefix:32, udp:600, tcp:600},{prefix:24, udp:6000, tcp:6000}]`, IPv6 /128,/64,/56), sampling window, UDP truncation percentage (force TCP), bypass list, rate-limited clients shown on dashboard.

---

## 9. Caching

`CacheZoneManager` constants: failure TTL 10 s, negative TTL 300 s, min record TTL 10 s, max 7 d, **serve-stale TTL 3 d, serve-stale answer TTL 30 s, reset TTL 30 s (10–900)**, cache maintenance every 60 s, `cacheMaximumEntries` (default 10,000; 0 = unlimited), persisted to `cache.bin` on shutdown (`saveCache`). Serve-stale (RFC 8767) waits min(`serveStaleMaxWaitTime`=1800 ms, clientTimeout−200 ms) before answering stale. **Prefetch**: entries with original TTL ≥ `cachePrefetchEligibility` (2 s) are refreshed when remaining TTL ≤ `cachePrefetchTrigger` (9 s) *on access* (v15.5 removed the "auto prefetch" sampling feature as too costly). Cache entries keep request metadata (which NS answered). Cache UI lists cached zones, allows per-zone delete and flush. Negative caching of resolution failures (RFC 9520).

---

## 10. Built-in blocking

- Settings: `enableBlocking`, `blockingType = AnyAddress(0.0.0.0/::) | NxDomain | CustomAddress`, `customBlockingAddresses[]`, `blockingAnswerTtl` (30), `allowTxtBlockingReport` (TXT query returns which list blocked it), `blockingBypassList` (client networks), `blockListUrls[]` with `!url` prefix meaning *allow list*, `blockListUpdateIntervalHours` (24, timer tick 15 min), "temporarily disable blocking for N minutes" API, "force update" API, dashboard toggle.
- Manual "Allowed zones" and "Blocked zones" (local lists with import/export; domain + subtree semantics).
- **List parsing** (`BlockListZoneManager.ReadListFile`): skip `#`/`!` comment lines; `||domain^` (AdBlock, only when no `$options` or options contain `doc`/`all`), `@@||domain^` exception → allow; otherwise hosts-file form (`ip hostname` or bare `hostname`), skipping `localhost`, `broadcasthost`, `ip6-*` etc. Downloaded files cached under `blocklists/<sha256(url)>` with conditional-GET style "no update" checks. Blocked response can carry EDE text. CNAME cloaking: after resolution, each CNAME target is re-checked.
- Advanced Blocking **App** replaces all of this with per-client-group policies (see §11.3) — the README explicitly warns to choose one or the other.

---

## 11. DNS Apps plugin system

### 11.1 Hook interfaces (`DnsServerCore.ApplicationCommon`, complete list)

| Interface | Method(s) | Where it runs / what it can do |
|---|---|---|
| `IDnsApplication : IDisposable` | `Task InitializeAsync(IDnsServer, string? config)`, `string? Description` | required base; `config` is the raw `dnsApp.config` text (JSON with comments) |
| `IDnsApplicationPreference` | `byte Preference` | ordering among apps for the same hook (lower first); shipped apps expose `appPreference` in config |
| `IDnsRequestController` | `Task<DnsRequestControllerAction> GetRequestActionAsync(request, remoteEP, protocol)` → `Allow / DropSilently / DropWithRefused` | first thing in the pipeline; firewall / ACL / malformed-drop |
| `IDnsRequestBlockingHandler` | `Task<bool> IsAllowedAsync(request, remoteEP)`; `Task<DnsDatagram?> ProcessRequestAsync(request, remoteEP)` | consulted on recursive path; return a blocked response (tagged `Blocked`) |
| `IDnsAuthoritativeRequestHandler` | `Task<DnsDatagram?> ProcessRequestAsync(request, remoteEP, protocol, isRecursionAllowed)` | after local zones, before recursion; can answer anything (bulk forwarding, zone alias, NXDOMAIN lists, DNS64 PTR) |
| `IDnsAppRecordRequestHandler` | `Task<DnsDatagram?> ProcessRequestAsync(request, remoteEP, protocol, isRecursionAllowed, zoneName, appRecordName, appRecordTtl, appRecordData)`; `string? ApplicationRecordDataTemplate` | invoked only when an `APP` record is hit; template JSON is shown in the UI when adding the record |
| `IDnsPostProcessor` | `Task<DnsDatagram?> PostProcessAsync(request, remoteEP, protocol, response)` | rewrite/filter/drop any response (DNS64 synth, AAAA filter, rebinding, default records, NXDOMAIN override, split-horizon NAT) |
| `IDnsQueryLogger` | `Task InsertLogAsync(timestamp, request, remoteEP, protocol, response)` | async log sink |
| `IDnsQueryLogs` | `Task<DnsLogPage> QueryLogsAsync(pageNumber, entriesPerPage, descendingOrder, start, end, clientIp, protocol, responseType, rcode, qname, qtype, qclass)` | makes the app a *queryable* log backend for the console's Query Logs tab and `/api/logs/query` |

The host-side `IDnsServer : IDnsClient` gives apps: `DirectQueryAsync(question|request[, remoteEP], timeout)` (re-enter the pipeline), `IsLocallyServedZone`, `WriteLog(...)`, `ApplicationName`, `ApplicationFolder`, `ServerDomain`, `ResponsiblePerson`, `DnsCache`, `Proxy`, `IPv6Mode`, `UdpPayloadSize`.

### 11.2 Loading, packaging, store

- Each app is a folder under `<config>/apps/<AppName>/` containing the DLLs, `dnsApp.config`, optional `README.md`, and a retained copy `<AppName>.zip`. Loaded in an isolated `AssemblyLoadContext` (`DnsApplicationAssemblyLoadContext`); every class implementing `IDnsApplication` is instantiated; interfaces are discovered by `is` checks; keyed by class path (`"SplitHorizon.SimpleAddress"`).
- Install/update/uninstall via API (`/api/apps/install|update|uninstall`, `downloadAndInstall`, `downloadAndUpdate`), config get/set via API (`/api/apps/config/get|set`); update preserves `dnsApp.config`; "automatic app update" setting; store = static JSON (`apps2.json`) listing name/description/versions[] with `serverVersion` minimum and zip URL.
- Apps run in-process with full trust (an app is arbitrary .NET code) — the v15.3 security note removed `Apps: Delete` from DNS Administrators precisely because installing an app is privilege escalation.

### 11.3 Official apps (27 in store; all GPL-3.0)

| App | Version | Interfaces | What it does / config shape |
|---|---|---|---|
| Advanced Blocking | 11.2.1 | RequestBlockingHandler | groups keyed by `networkGroupMap` (CIDR→group) and `localEndPointGroupMap` (listener ip:port or DoT/DoH SNI name→group); per group: `enableBlocking, blockAsNxDomain, blockingAddresses[], allowed[], blocked[], allowListUrls[], blockListUrls[] (string or {url, blockAsNxDomain, blockingAddresses})`, `allowedRegex[], blockedRegex[], regexAllowListUrls[], regexBlockListUrls[], adblockListUrls[]`, `blockingAnswerTtl`, update interval h+m |
| Advanced Forwarding | 5.2 | AuthoritativeRequestHandler, Preference(200) | named `proxyServers[]`, named `forwarders[] {forwarderProtocol, forwarderAddresses[], dnssecValidation, proxy}`, `networkGroupMap`, `groups[].forwardings[] {forwarders[], domains[] (wildcard '*')}`, `adguardUpstreams[] {configFile}` (AdGuard upstream syntax) |
| Auto PTR | 5.0 | AppRecordRequestHandler | APP record data `{prefix, suffix, ipSeparator}` generates PTR names from IP |
| Block Page | 8.1.1 | (none; runs Kestrel) | HTTP(S) block page server(s), self-signed or PFX; **online cert minting** for HTTPS MITM when its root CA is trusted; shows EDE blocking info |
| Default Records | 5.0 | PostProcessor | `zoneSetMap {"*":[...], "*.net":[...]}` + `sets[]` of zone-file-format records injected into authoritative NOERROR/NXDOMAIN answers |
| DNS64 | 6.0 | AuthoritativeRequestHandler, PostProcessor, Preference(30) | RFC 6147 synth per client group, prefix mappings, exclusions, PTR for synthesized space, skips when DO set |
| DNS Block List (DNSBL) | 5.1 | AppRecordRequestHandler | RFC 5782 host IP/domain lists served under an APP record; file format `network [A [TXT]]`, auto-reload |
| DNS Rebinding Protection | 5.0 | PostProcessor | strips RFC 1918/4193 addresses from non-authoritative answers unless domain in `privateDomains`; `bypassNetworks` |
| Drop Requests | 8.1 | RequestController | `allowedNetworks, blockedNetworks, blockedQuestions[{name, type, blockZone}], dropMalformedRequests, allowedLocalEndPoints` |
| Failover | 10.1 | AppRecordRequestHandler | APP data `{primary[], secondary[], serverDown[], healthCheck: "ping|tcp|http|https", healthCheckUrl, allowTxtStatus}`; app config defines health checks (interval/timeout/retries), email + webhook alerts, maintenance windows; TXT status query |
| Filter AAAA | 5.1 | PostProcessor | NODATA for AAAA when A exists; bypass lists |
| Geo Continent / Geo Country | 10.1.1 | AppRecordRequestHandler | APP data keyed by continent/country code (+`default`), ASN entries, custom groups in app config, MaxMind `.mmdb` bundled (GeoLite2) and updated by uploading a zip; ECS scope from ISP/ASN DB |
| Geo Distance | 10.0.2 | AppRecordRequestHandler | nearest server by lat/long (GeoLite2 City) |
| Log Exporter | 3.1 | QueryLogger | export to file / HTTP / Syslog sinks (community-contributed) |
| NO DATA | 6.0 | AppRecordRequestHandler | `{blockedTypes:[...]}` → NODATA |
| NX Domain | 8.0 | AuthoritativeRequestHandler, Preference(20) | flat `blocked[]` with subtree match; EDE text |
| NX Domain Override | 4.0 | PostProcessor | `domainSetMap` + `sets[]` → replace NXDOMAIN with A/AAAA |
| Query Logs (SQLite / MySQL / PostgreSQL / SQL Server) | 9.1.2 / 4.2 / 1.3 / 3.2.1 | QueryLogger + QueryLogs | bounded channel writer, retention by age/count, VACUUM, numeric protocol/responseType columns, RTT, server-domain column for shared DBs, pagination optimised |
| Split Horizon | 11.0 | AppRecordRequestHandler, AuthoritativeRequestHandler, PostProcessor, Preference(40) | APP classes `SimpleAddress` (`{"private":[...],"public":[...]}` or per named network) and `SimpleCNAME`; config `networks{}` named CIDR sets, `networkGroupMap`, `domainGroupMap`, `groups[] {externalToInternalTranslation {"1.2.3.0/24":"10.0.0.0/24"}, translateReverseLookups}` — i.e. **1:1 NAT rewriting of answers and PTRs** |
| Weighted Round Robin | 5.0 | AppRecordRequestHandler | weighted A/AAAA/CNAME selection from APP data |
| What Is My DNS | 9.0 | AppRecordRequestHandler | echoes client IP (A/AAAA/TXT) |
| Wild IP | 6.1 | AppRecordRequestHandler | sslip.io-style `192-168-1-10.ip.example.com`, hex forms, `allowedNetworks` guard |
| Zone Alias | 5.0 | AuthoritativeRequestHandler, Preference | serve zone B as alias of zone A by rewriting names |

Removed: MISP Connector (v15: "not feasible to support").

---

## 12. Logging, stats, dashboard

- **System log**: `LogManager` → file per day (`logs/yyyy-MM-dd.log`) or STDOUT (v13.5), UTC or local, `maxLogFileDays`, `noStackTrace`; query logging to the same file is optional (`logQueries`) in text form: `QNAME: x; QTYPE: A; QCLASS: IN; TSIG KeyName...; RCODE: NoError; ANSWER: [rdata, ...]; ADDITIONAL: [...]`.
- **Stats** (`StatsManager`): in-memory last-hour 60 minute-buckets; hourly files `stats/yyyyMMddHH.stat`, daily `yyyyMMdd.dstat` (binary), truncated to top-1000 per category, cleanup after `maxStatFileDays`; `enableInMemoryStats` for ephemeral containers. Counters: `totalQueries, NoError, ServerFailure, NxDomain, Refused, Authoritative, Recursive, Cached, Blocked, Dropped, Clients`, plus per-query-type, per-protocol, top clients (with rDNS and rate-limited flag), top domains, top blocked domains. Aggregates across the cluster (`node=cluster`). Ranges: LastHour/Day/Week/Month/Year/Custom (minute granularity up to 2 h).
- **Prometheus**: `GET /api/dashboard/metrics/text` (lifetime counters `uptime_seconds, start_time, total_queries, total_no_error, ... total_clients`) and `/metrics/json` (v15.0/15.1).
- **Health**: `GET /api/dnsClient/healthCheck` (loopback unauthenticated since v15.5; used by compose healthcheck); `GET /api/status` (version, SSO flags, cluster state) for pre-login UI.
- **Query Logs tab**: provided by whichever installed app implements `IDnsQueryLogs`; filters client/protocol/responseType/rcode/qname/qtype/qclass/time; CSV export; live update (v15); colour by response type.
- UI sections: Dashboard, Zones, Cached Zones, Allowed Zones, Blocked Zones, Apps, DNS Client (built-in dig with import-to-zone), Settings (General, Web Service, Optional Protocols, TSIG, Recursion, Cache, Blocking, Proxy & Forwarders, Logging), DHCP (Scopes, Leases), Administration (Sessions, Users, Groups, Permissions, SSO, LDAP, Cluster), Logs (Log Viewer, Query Logs), About. Dark/Light/Amber themes, custom `www` root via env var.

---

## 13. Authentication, RBAC, sessions

- **Users** (`User`): `displayName, username, type(Local|RemoteSSO|RemoteLDAP), password hash (PBKDF2-SHA256, salt, iterations; old HMAC scheme migrated), disabled, memberOfGroups, sessionTimeoutSeconds (default 1800), TOTP key (QR via QRCoder), previousSessionLoggedOn/From, recentSessionLoggedOn/From`.
- **Groups**: built-in `Administrators`, `DNS Administrators`, `DHCP Administrators`, `Everyone` (+ custom).
- **Permissions**: `PermissionSection {Dashboard, Zones, Cache, Allowed, Blocked, Apps, DnsClient, Settings, DhcpServer, Administration, Logs}` × `PermissionFlag {View=1, Modify=2, Delete=4}` for users and groups, **plus sub-item permissions** (per zone, per DHCP scope) — e.g. "Zones: Modify" lets you create zones, "Zone X: Modify" lets you add records only in X. Defaults: Administrators = all; DNS Administrators = Zones/Cache/Allowed/Blocked VMD, Apps VM, DnsClient V, Settings VM (Delete removed in v15.3 because backup/restore and app install are privilege escalation paths); DHCP Administrators = DhcpServer VMD; Everyone = View on most. Zone permission API: `/api/zones/permissions/get|set`.
- **Sessions** (`UserSessionType {Standard, ApiToken, ClusterApiToken, SingleUse}`): 64-hex-char tokens; `Authorization: Bearer <token>` (v15) or legacy `token` param; standard sessions expire by inactivity; API tokens never expire and cannot change password/profile; single-use tokens; static sessions from env `DNS_SERVER_AUTH_STATIC_SESSIONS=user:token,...`; sessions list shows user-agent and IP; brute-force protection per IP (IPv6-aware); audit of login/logout in system log. v15.5.1/15.6 fixed a token-prefix oracle in the delete-session API — a reminder to compare tokens constant-time and never match by prefix.
- **2FA**: TOTP (init/enable/disable APIs; `2fa-required` status).
- **OIDC SSO** (v15, `Microsoft.AspNetCore.Authentication.OpenIdConnect`): settings `authority, clientId, clientSecret(+_FILE), metadataAddress, scopes (openid, profile mandatory; email optional for username), allowSignup, allowSignupOnlyForMappedUsers, groupMap "RemoteGroup:LocalGroup,..."`; claims read from id_token and user-info endpoint (`sub|preferred_username|email|upn|name`, groups from `groups`/role claim); remote groups synced to local groups at each login; `createSsoUser`/`setSsoUser` APIs for pre-provisioning (also used by cluster sync); SSO users cannot change password locally. LDAP (v15.5) is the same shape with bind DN, search base/filter, group attribute, StartTLS/LDAPS.
- Web service hardening: CSP frame-ancestors header, reverse-proxy allow-list for `X-Real-IP`, HTTP→HTTPS redirect, HTTP/3 for console, Unix sockets.

---

## 14. HTTP API shape

- Single Kestrel host on 5380/53443; `GET` or `POST` (`application/x-www-form-urlencoded`), uploads `multipart/form-data`. Response envelope `{status: "ok"|"error"|"invalid-token"|"2fa-required", response: {...}, errorMessage, stackTrace, innerErrorMessage}`.
- Resource-ish paths with verbs: `/api/user/login|logout|createToken|createSingleUseToken|session/get|delete|changePassword|profile/get|set|2fa/init|enable|disable`, `/api/dashboard/stats/get|getTop|deleteAll|metrics/json|text`, `/api/zones/list|catalogs/list|create|import|export|clone|convert|enable|disable|delete|resync|options/get|set|permissions/get|set|dnssec/sign|unsign|dsinfo|properties/*|records/add|get|update|delete`, `/api/cache/list|delete|flush`, `/api/allowed/*`, `/api/blocked/*`, `/api/apps/list|listStoreApps|downloadAndInstall|downloadAndUpdate|install|update|uninstall|config/get|set`, `/api/dnsClient/resolve|healthCheck`, `/api/settings/get|set|getTsigKeyNames|forceUpdateBlockLists|temporaryDisableBlocking|backup|restore`, `/api/dhcp/*`, `/api/admin/sessions|users|groups|permissions|sso|cluster/*`, `/api/logs/list|download|delete|deleteAll|query|export`.
- "set" semantics overwrite; "add" append. Every call accepts `node=<nodeName>` when clustered: the receiving node **proxies the HTTP request** to the target node using a cluster API token (`ClusterNode.ProxyRequest`), acting as the same username.
- Record API is one giant `add`/`update` with per-type parameters (`ipAddress`, `ptr`, `createPtrZone`, `cname`, `exchange/preference`, `svcParams` pipe syntax `alpn|h2,h3|port|443`, `rdata` hex for unknown types, `appName/classPath/recordData` for APP, `protocol/forwarder/forwarderPriority/dnssecValidation/proxy*` for FWD). `ipAddress=request-ip-address` gives DDNS over HTTP.
- A typed C# client (`DnsServerCore.HttpApi`) exists and is what the cluster code uses — i.e., Technitium's own clustering is implemented *on top of its public API*.

---

## 15. Clustering (as of v15.6, Oct 2026) — what actually exists

Introduced v14.0 (2025-11-08), breaking changes in 14.1, 14.2, 15.0. It is a **management-plane replication** feature, not HA/anycast/consensus.

- **Topology**: exactly one Primary node, N Secondary nodes. `ClusterNodeType {Primary, Secondary}`, `ClusterNodeState {Self, Connected, Unreachable}`. Nodes identified by `id` (int), `name` (FQDN = `<server>.<clusterDomain>`), `url` (https console URL), `ipAddresses[]`, `version`, `lastSeen`.
- **Bootstrap**: `cluster/init` on the primary creates (a) a **Primary zone `<clusterDomain>`** holding each node's A/AAAA and **TLSA (DANE-EE)** records ("Cluster managed record. Do not update or delete.") and (b) a **Catalog zone `cluster-catalog.<clusterDomain>`**; HTTPS is force-enabled with a self-signed cert if needed; the server's own domain becomes `<host>.<clusterDomain>`. A secondary joins with primary URL + admin credentials (+TOTP) via `cluster/secondary/join`, which creates a `ClusterApiToken` session on both sides, pulls a full config zip, and creates Secondary copies of the cluster zone and cluster catalog (transferred with a TSIG key).
- **Node auth after join**: DANE-EE — each node validates the peer's TLS cert against the TLSA record in the cluster zone, resolved through an `InternalDnsClient` (the node's own resolver). Consequence: an HTTPS reverse proxy in front of the console breaks clustering (TCP passthrough only).
- **Sync mechanics** (`ClusterManager`): heartbeat every 30 s (retry 10 s; range 10–300) by calling the peer's `cluster/state`; **config refresh pull every 900 s** (retry 60 s; 30–3600) via `cluster/transferConfig?ifModifiedSince=...` which returns a **zip** containing `auth.config, cluster.config, webservice.config, dns.config, log.config, allowed.config, blocked.config, blocklist.config, blocklists/*, apps/*.zip + dnsApp.config, zones/<member>.keys` (DNSSEC keys only for cluster-catalog members); plus a **push NOTIFY** (`cluster/notify`, debounced 5 s) from primary after any change so secondaries pull immediately. Several API calls are fanned out explicitly to all nodes (`setClusterSettings`, `forceUpdateBlockLists`, `temporaryDisableBlocking`, `createSsoUser/setSsoUser`).
- **What is synced**: Settings (cluster-wide subset; node-specific settings like listen addresses/certs stay local), Allowed/Blocked, Apps + configs, Users/Groups/Permissions, API tokens, SSO group map, block lists. **Zones only if added to the cluster catalog** (then provisioned as Secondary zones on other nodes; DNSSEC-signed primaries get their keys replicated so a promoted node can keep signing).
- **Not synced**: cache, logs/stats (per node; dashboard offers an aggregate "cluster" view), login sessions, DHCP scopes, node-local settings.
- **Failover**: manual. `cluster/secondary/promoteToPrimary` (converts secondary zones of catalog members back to primary using synced keys), `cluster/primary/delete?forceDelete`, `updatePrimaryNode`, `leave`, `removeSecondaryNode`, `updateNodeIpAddresses`. The primary must be online for any config change (secondaries' settings are read-only in the UI). No leader election, no VIP, no quorum.
- **Known issues** reported in blog comments: 30 s HTTP timeout on slow links with many apps, "no active session found" after upgrades (all nodes must run the same version), DANE failures until the cluster zone has synced.

Relevance for kudurru: this is the "one stone in the temple, clay copies for landholders" model literally — primary = temple, secondaries = clay copies — but implemented by file-zip replication rather than a replicated store. It proves the UX (single console, `node=` selector, aggregate dashboard) is valuable; the transport is the part to replace.

---

## 16. Configuration & state storage

Everything lives under one config folder (`/etc/dns` in Docker; `DnsServerApp.dll <folder>`):

```
<config>/
├── dns.config          # binary, magic "DS", version 7: all DnsServer settings
├── webservice.config   # binary: console/web settings
├── auth.config         # binary, magic "AS", version 4: users, groups, permissions, sessions
├── cluster.config      # binary: nodes, intervals
├── log.config, blocklist.config, allowed.config, blocked.config, custom-blocked.config
├── zones/<zone>.zone   # binary, magic "DZ", version 4: AuthZoneInfo (v11) + records + history + DNSSEC keys
├── apps/<App>/         # app dlls + dnsApp.config (JSON w/ comments) + <App>.zip
├── blocklists/<sha256> # cached downloaded lists
├── stats/*.stat|*.dstat
├── cache.bin           # persisted cache
├── self-signed-cert.pfx
├── logs/               # (or platform log dir) yyyy-MM-dd.log
└── resetadmin.config   # presence resets admin password on start
```

- All writes are atomic (`*.tmp` then `File.Move(..., overwrite)`); saves are debounced via timers; zone files are saved lazily per zone.
- There is **no human-readable config file**. Docker env vars (`DNS_SERVER_*`, ~45 of them: domain, admin password(+_FILE), web ports/TLS, recursion ACL, blocking, forwarders, logging/stats retention, SSO/LDAP) are **read only on first start when `dns.config` does not exist**; two runtime env vars exist (`DNS_SERVER_WEB_SERVICE_WWW_FOLDER_PATH`, `DNS_SERVER_AUTH_STATIC_SESSIONS`). Backup/restore = zip of the folder via API; restore accepts older versions.
- Zone import/export uses BIND master-file text (`ZoneFile.cs` parser supports `$ORIGIN/$TTL`, extended format, proprietary types serialized as `APP "name" "class" base64`, `FWD proto addr ...`).

---

## 17. Deployment

- **Docker**: image built from a pre-published `DnsServerApp/bin/Release/publish`; `ENTRYPOINT dotnet DnsServerApp.dll`, `CMD /etc/dns`; includes `dnsutils`, `curl`, `libmsquic`; exposes 53/udp+tcp, 853/udp+tcp, 443/udp+tcp, 80, 8053, 5380, 53443, 67/udp. Compose: volumes `config:/etc/dns`, `logs:/var/log/technitium/dns`; `sysctls net.ipv4.ip_local_port_range=1024 65535` (resolver source ports); `healthcheck` curls `/api/dnsClient/healthCheck`; `network_mode: host` recommended for DHCP. Multi-arch amd64/arm64/arm.
- **Bare metal**: Windows installer (non-admin service since v15), Linux `install.sh` (systemd non-root since v15; Alpine OpenRC since v15.3), portable tar.gz for any OS with .NET 10 runtime; Raspberry Pi arm7 only.
- **Kubernetes**: nothing official — no Helm chart, no CRDs, no operator, no leader election. Community charts exist (UNVERIFIED). State is a single RW volume per instance; two instances = two independent servers unless you use the cluster feature (which needs stable node IPs/FQDNs and direct node-to-node HTTPS).
- Reverse proxy guidance: DoH behind nginx via DNS-over-HTTP 8053 + `X-Real-IP`; console behind proxy OK except when clustering (DANE).

---

## 18. DHCP (for completeness)

Built-in DHCPv4 server (RFC 2131/2132/3396/3397/3442/4702/5417/5859): scopes with ranges, exclusions, reserved leases, lease time, ping-check, options (router, DNS, WINS, NTP, domain search, TFTP, CAPWAP, classless static routes, generic options), `useThisDnsServer`, automatic A/PTR record management in local zones for leases (overwrite option, persistent records for reservations), lease conversion dynamic↔reserved, per-scope permissions. Not clustered. Not IPv6.

---

## 19. Table stakes vs. differentiators (for kudurru planning)

**Table stakes** (users of Technitium/Pi-hole/AdGuard/BIND will expect these on day one):
- Primary/Secondary zones with AXFR/IXFR/NOTIFY/TSIG; Stub; conditional forwarding per domain; reverse zones from CIDR; BIND zone import/export.
- Full common RR set incl. SVCB/HTTPS, TLSA, CAA, SSHFP, NAPTR, unknown-type hex; per-record disable/comment/TTL.
- Recursive resolver with DNSSEC validation, QNAME min, serve-stale, prefetch, negative caching, cache view/flush.
- DoT/DoH/DoQ as server and as forwarder; PROXY protocol for running behind dnsdist.
- Block lists (hosts + AdBlock syntax, allow lists, custom lists), NXDOMAIN/null/custom-IP response, per-client bypass, "disable for N minutes".
- Dashboard with totals, timeline, top clients/domains/blocked, query log with filters and CSV export, Prometheus endpoint, health endpoint.
- Users/groups/RBAC, API tokens, 2FA, OIDC; HTTP API covering 100% of UI; Docker image with env-var bootstrap.
- Access ACLs for recursion, transfers, updates, queries.

**Differentiators** Technitium has that most competitors lack (kudurru should decide which to match):
- DNSSEC *signing* with automated rollover and parent-DS polling (Pi-hole/AdGuard/blocky have none; PowerDNS/Knot have it but no UI).
- Catalog zones + Secondary Forwarder/Catalog types.
- `FWD`/`APP`/`ANAME` records as a uniform way to express policy inside zones.
- A real plugin SDK with 8 hook types and a store (Pi-hole/AdGuard have none; CoreDNS has compile-time plugins).
- Split-horizon with 1:1 address translation, Geo*/Failover/Weighted RR as apps.
- Clustering with single-console management, aggregate dashboard, per-node selector, synced users/tokens/apps/blocklists.
- Rate limiting per prefix with UDP→TCP truncation, spoof detection, extensive hardening track record (many CVE fixes credited to academic researchers — a sign the project is a research target; kudurru should expect the same scrutiny).
- Zone-level and per-record "expiry TTL", SVCB auto-hints, EDE/NTA disclosure, ECS-source-address for proxies.

---

## 20. What kudurru should borrow

1. **The zone-type matrix and zone-options model** (query/transfer/notify/update modes + ACL + TSIG lists + catalog-override flags). Model it as a declarative `ZonePolicy` struct (and a CRD) with the same six-way enum vocabulary; it is battle-tested and users already understand it.
2. **Policy-as-records**: a `FWD`-like forwarder RR with protocol/priority/DNSSEC flag, and an `APP`-like delegation RR with JSON payload + schema/template. Keep them in a private-use type range with distinct names; render them specially in UI; serialize in zone text as base64 so BIND tools can round-trip.
3. **The eight hook interfaces as the plugin contract** (RequestController → BlockingHandler → AuthoritativeHandler → AppRecordHandler → PostProcessor → QueryLogger/QueryLogs, with explicit Preference ordering, and a tagged `ResponseType` on every response). This maps cleanly onto kudurru's "simple integration model": a Cloudflare split-horizon writer is a *zone change observer* + *post-processor*; UniFi is a *request-controller/blocking group source*. Add the hook Technitium lacks: **zone/record change events** for outbound integrations.
4. **RFC 7583 key state machine with timers**, `propagationDelay = refresh+retry`, KSK activation gated on parent-DS observation, ZSK rolloverDays, PEM key import, NSEC↔NSEC3 live conversion. Implement as a reconciler loop in kudurru (fits the operator model).
5. **Catalog zones as the data-plane replication primitive**, including the `.ext.` property encoding for zone type/ACL/TSIG/primary addresses. In kudurru, the control plane (etcd/CRDs) can *generate* the catalog so standard BIND/Knot secondaries can also follow.
6. **RBAC granularity**: sections × View/Modify/Delete plus per-zone sub-item permissions; group-mapped OIDC with "signup only for mapped groups"; non-expiring named API tokens separate from sessions; single-use tokens; static tokens from env for bootstrapping automation (Kubernetes secrets fit perfectly).
7. **Advanced-Blocking's group model**: client → group via CIDR map *or* via local endpoint / DoT-DoH SNI name (`user1.dot.example.com` = per-device policy without client IPs), per group independent list sets, regex lists, per-list override of blocking response. Also the simple rule that `!url` in the block-list field means allow-list.
8. **Serve-stale/prefetch numbers** (3 d stale TTL, 30 s stale answer TTL, 1.8 s max wait, eligibility 2 s / trigger 9 s) as sane defaults; failure TTL 10 s; cache persisted across restarts.
9. **QPM per-prefix rate limiting with truncation-to-TCP** and dashboard flagging of rate-limited clients.
10. **Operational UX**: `node=` selector on every API call + aggregate dashboard; notify-failed indicators on zones; "disable blocking for N minutes"; TXT blocking report; `request-ip-address` DDNS helper; healthcheck endpoint that does not pollute query logs; conditional config transfer (`ifModifiedSince`); atomic file writes.
11. **Docker bootstrap via env vars + `*_FILE` secrets**, and the explicit documentation that they apply only on first start.
12. **Transport completeness list** as the target: UDP/TCP/TLS/HTTPS(1.1,2,3)/QUIC/PROXY-v1v2/Unix socket, hot-reloading certificates by mtime — but add ACME.

## 21. What kudurru should avoid

1. **Binary, versioned, hand-rolled config and zone files** (`DS`/`DZ`/`AS` magic). They make GitOps, diffing, backups and k8s ConfigMaps/CRDs impossible. Use text (YAML/JSON) and standard zone files; store state in etcd/CRDs.
2. **Env vars that only work on first boot.** Kubernetes users redeploy constantly; config must be fully declarative and re-applied.
3. **Clustering by zip-file replication over the admin API with a single mutable primary.** Keep the *experience* (single console, aggregate stats) but build on a replicated store with leader election; make every node able to accept writes or fail over automatically; never require node-to-node DANE bootstrapped from the DNS data it is replicating (chicken-and-egg, breaks behind proxies).
4. **In-process, full-trust plugin loading of arbitrary binaries from a store**, with app install being a privilege-escalation path (v15.3 had to strip permissions). If kudurru supports external plugins, sandbox them (WASM/gRPC sidecars) and make core apps built-in.
5. **The God-object**: `DnsServer.cs` at 8,000 lines mixes listeners, settings persistence, resolver, blocking, rate limiting. Keep kudurru's pipeline stages as separate packages with explicit interfaces.
6. **Two overlapping blocking engines** (built-in vs Advanced Blocking app) that the README warns not to combine. kudurru should have one blocking engine whose simplest configuration is one group.
7. **PKCS#12-only certificates and no ACME.** Accept PEM, cert-manager secrets, and run an ACME client with DNS-01 against kudurru itself.
8. **API design quirks**: GET for mutations, a single `records/add` with ~60 optional params, pipe-separated mini-languages (`updateSecurityPolicies`, `svcParams`), `status` strings instead of HTTP codes. Use REST/JSON bodies (or gRPC) with typed per-RR schemas.
9. **Stats in bespoke binary files truncated to top-1000**; use a real time-series/columnar store (or export to Prometheus and keep only recent raw query logs locally).
10. **Session/token footguns** that bit Technitium: prefix-matching tokens, user-controlled PBKDF2 iteration counts, regex injection in filters, log injection via username, XSS in log views — design the auth layer with constant-time comparisons, server-fixed KDF params, and escaped rendering from day one.
11. **No DHCPv6, no k8s, arm7-only Pi** — not mistakes to copy, just gaps kudurru can fill (or explicitly scope out DHCP).
12. **jQuery/Bootstrap-3 SPA with no API-first typing** — build the UI on the same typed API clients/CLI (`kudurru`) use.

---

## 22. Open questions / UNVERIFIED

- Exact wire-level details of cluster `transferConfig` conflict handling (what happens if a secondary has local edits) — inferred "primary overwrites" from blog; not read in full.
- Whether Technitium's Secondary Catalog interoperates with BIND 9.18+/Knot catalog producers (RFC 9432 compliant on paper; a v13.3 changelog entry fixed "Catalog zone SOA request that caused zone transfer to fail with BIND", suggesting it is tested with BIND). Not tested here.
- DHCP ↔ DNS record lifecycle specifics (which zone, TTLs) — only skimmed.
- Community Helm charts / k8s manifests for Technitium — not evaluated.
- Performance claims (100k qps on i7-8700) are from the README; not benchmarked.

## 23. Sources

- https://github.com/TechnitiumSoftware/DnsServer (HEAD 70495af5, 2026-10-03; README, CHANGELOG, APIDOCS, Dockerfile, docker-compose.yml, DockerEnvironmentVariables.md, EnvironmentVariables.md, SupportedRFCs.md, Apps/*/README.md, Apps/*/dnsApp.config, DnsServerCore/**)
- https://github.com/TechnitiumSoftware/TechnitiumLibrary (HEAD a3d0ad69, 2026-10-03; TechnitiumLibrary.Net/Dns/**)
- https://api.github.com/repos/TechnitiumSoftware/DnsServer, .../releases/latest, .../tags
- https://hub.docker.com/v2/repositories/technitium/dns-server/ and /tags
- https://download.technitium.com/dns/apps/apps2.json (app store manifest)
- https://technitium.com/dns/
- https://blog.technitium.com/2025/11/understanding-clustering-and-how-to.html
- https://blog.technitium.com/2025/11/technitium-dns-server-v14-released.html
- https://blog.technitium.com/2026/04/technitium-dns-server-v15-released.html
- https://blog.technitium.com/2024/10/how-to-configure-catalog-zones-for.html
