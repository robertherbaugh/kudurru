> **Research note, 2026-10-10.** Point-in-time study produced during the kudurru design phase and cited by [dev-docs/design-record.md](../design-record.md). It describes other projects for analysis only; nothing in it is to be ported into kudurru (several of them are GPL/EUPL). Items marked UNVERIFIED were not confirmed against a primary source. Verify before relying on a version number or API detail.

# kudurru research: the Go implementation stack

Key: `go-dns-stack` — Researched 2026-10-10. Sources: shallow clones of miekg/dns, coredns, blocky,
AdguardTeam/dnsproxy, BeryJu/gravity, quic-go (all at HEAD on 2026-10-09/10) under
`/tmp/kudurru-research/go-dns-stack/`, plus pkg.go.dev, GitHub/Codeberg release pages, official docs and
web search. Anything I could not confirm from a primary source is marked **UNVERIFIED**.

---

## 0. Executive summary (decision-relevant)

1. **Go is the right language.** Every project kudurru wants to borrow from (CoreDNS, blocky, AdGuard
   dnsproxy/AdGuard Home, Gravity, external-dns, kine, etcd, rqlite) is Go, and all of them share one wire
   library type system (`github.com/miekg/dns` v1). Rust's hickory-dns is good but its recursor is still
   "experimental" (and had a 2026 cross-zone cache-poisoning CVE), IXFR is unimplemented, and there is no
   Rust equivalent of the CoreDNS/blocky/dnsproxy code mass to reuse. See §1.
2. **miekg/dns has forked into two lines.** `github.com/miekg/dns` v1.1.73 (Sep 2026) is now
   "specific fixes only... will be archived". The successor is `codeberg.org/miekg/dns` (v2, 0.6.118,
   requires **Go 1.27**, ~2x faster, `context.Context` handlers, `rdata` split, `dnstest`, `dnshttp`,
   no DoQ). **CoreDNS, blocky, dnsproxy and Gravity are all still on v1.1.73** as of 2026-10-10.
   Recommendation: start on v1 for ecosystem interop (dnsproxy upstream clients, CoreDNS plugin code,
   dnstap), wrap it behind kudurru's own zone/record model so a v2 migration is a contained change. See §2.
3. **DNSSEC signing must be written by kudurru** (chain building, key lifecycle, rollover). miekg gives
   the primitives (`RRSIG.Sign/Verify`, `DNSKEY.Generate/ToDS/KeyTag`, `HashName`, `NSEC3.Cover/Match`,
   BIND-format private-key I/O). CoreDNS has two reference designs (online "NSEC black lies" `dnssec`
   plugin; offline NSEC-only `sign` plugin, no NSEC3, no rollover). Technitium (C#) is the feature bar:
   NSEC+NSEC3, RSA/ECDSA/EdDSA, policy-driven. Sign on the leader, replicate the signed RRsets. See §3.
4. **Transports are solved in Go.** DoT = `dns.Server{Net:"tcp-tls"}`; DoH (h1/h2) = `net/http`;
   DoH3 = `quic-go/http3.Server` sharing the mux; DoQ = `quic.Listen` + 2-byte-length-prefixed
   messages per stream (CoreDNS `server_quic.go` and dnsproxy `serverquic.go` are copyable references).
   quic-go v0.63.0 (Sep 22 2026, MIT). For *upstream* clients use `github.com/AdguardTeam/dnsproxy/upstream`
   (Apache-2.0) which already does udp/tcp/tls/https/h3/quic/dnscrypt with bootstrap. See §4.
5. **Storage recommendation: one engine, two modes.** A domain-level `Store` interface backed by an
   embedded engine (bbolt v1.5.0 or cgo-free SQLite via modernc v1.54) on every node, replicated with
   **hashicorp/raft v1.8.0 (MPL-2.0, Sep 2026)** when `--cluster` is on. Single Docker = raft with a
   single voter (rqlite does exactly this). In Kubernetes, kudurru runs as a StatefulSet and **CRDs are a
   declarative *input* reconciled into the store by a leader-elected controller, not the store itself**.
   Embedded etcd (Gravity) is the credible alternative if you refuse to own consensus; dragonboat (v4
   never released, pseudo-versions only) and marmot are out. See §6 for the tradeoff table and interface.
6. **Query log: per-node SQLite (WAL, batched) + in-memory ring + rollup buckets** (Pi-hole FTL's
   model: 24 h in RAM, 10-minute `overTime` slots, flush every 60 s, `MAXDBDAYS` retention), with optional
   ClickHouse (clickhouse-go v2, pure Go) and dnstap sinks. Avoid DuckDB in the core binary (cgo). See §7.
7. **App stack:** Go 1.27 min; cobra 1.10.2; koanf v2.3.x (YAML+env+flags); net/http 1.22+ mux (+chi
   5.3.2 if you want middleware ergonomics); **huma v2.38** for code-first OpenAPI 3.1; go-oidc v3.17
   or zitadel/oidc v3.48 for OIDC RP; slog; prometheus client_golang 1.24.1; OTel 1.45; fsnotify 1.9;
   goreleaser 2.18 + ko 0.19.1 + cosign 3.1.3 for distroless multi-arch signed images. See §8–§13.
8. **Frontend:** React 19.2 + TypeScript + Vite 8 + Tailwind + shadcn/ui + TanStack Table v9 (Aug 2026)
   + ECharts 6.1 (uPlot 1.6.x for the 24 h live series) as a pure SPA served via `go:embed`. Gravity's
   Lit 3 + PatternFly stack is viable but has a thinner dashboard/data-table ecosystem. See §9.

---

## 1. Go vs Rust

| Criterion | Go | Rust (hickory-dns) |
|---|---|---|
| Wire library | miekg/dns v1 (1.1.73) + v2 (codeberg 0.6.118). All RR types, DNSSEC, TSIG, SIG(0), EDNS0, zone parser, AXFR/IXFR. | hickory-proto / hickory-net 0.26.x. Full RR coverage, DNSSEC via `dnssec-ring`/`dnssec-aws-lc-rs`. |
| Server building blocks | CoreDNS (1.14.7, Aug 19 2026): plugins for dnssec, sign, transfer, secondary, tsig, dynupdate (RFC 2136, bbolt-persisted), dnstap, cache, quic, https, https3, proxyproto, multisocket, acl, view. | hickory-server 0.26: authority (file, sqlite), RFC 2136 update, DNSSEC auto-resign on dynamic update, NSEC3, DoT/DoH/DoQ/DoH3 (feature-gated per crypto backend). **IXFR listed as "not yet implemented"**; offline signing "not presently supported". |
| Recursive resolver | CoreDNS is a forwarder, not a recursor; blocky/dnsproxy are forwarders. Go lacks a mature recursor too (parity). | hickory-recursor "experimental"; CVE-2026-42254 (cross-zone cache poisoning, 0.1–0.25.2), fix not clearly stated in advisories. |
| Blocking engine | blocky (0.35.0, Sep 2026) and AdGuard Home (v1.0.0-b.1, Sep 30 2026; AdGuard `urlfilter` adblock-syntax engine) in Go. | Nothing comparable. |
| Replication | embedded etcd (Gravity), hashicorp/raft (Consul/Nomad/rqlite), NATS JetStream (kine), kine. | openraft/raft-rs exist, but no DNS-specific reference. |
| Kubernetes | client-go v0.37.1, controller-runtime v0.25.1, external-dns (v0.23.0, Sep 18 2026) all Go; CoreDNS *is* the cluster DNS. | kube-rs is good, but k8s ecosystem is Go-first. |
| Upstream DoH3/DoQ clients | dnsproxy/upstream (Apache-2.0) production-grade (AdGuard scale). | hickory-resolver supports DoQ/DoH3 features. |
| Hiring/contributors | Large pool familiar with CoreDNS-style code. | Smaller. |

**Verdict:** Go, with no decisive reason for Rust. The only area Rust would win is memory-safety of a from-
scratch parser; miekg/dns has had 15 years of fuzzing and is what CoreDNS (k8s cluster DNS) runs on.
hickory-dns facts: dual MIT/Apache-2.0; 0.26.0 moved protocols into `hickory-net`; 0.26.2 "fixes many
security-relevant bugs" (Fedora update notes, 2026). Sources: github.com/hickory-dns/hickory-dns README,
docs.rs/crate/hickory-server/latest, feedly.com/cve/CVE-2026-42254.

---

## 2. miekg/dns in depth

### 2.1 Two lines, verified facts

| | `github.com/miekg/dns` (v1) | `codeberg.org/miekg/dns` (v2) |
|---|---|---|
| Version | **1.1.73** (`version.go`), last commit 2026-09-01 (dep bump) | **0.6.118** (latest "Release" commit on Codeberg; Debian has 0.6.117 as of 2026-10-05) |
| go.mod | `go 1.25.0`; deps x/net, x/sync, x/sys | `go 1.27.0`; deps certmagic 0.25.2, fsnotify 1.9.0, sqlx, geoip2, prometheus client, tidwall/btree, zap, modernc.org/sqlite 1.46.1 (these are for `cmd/atomdns`) |
| License | BSD-3-Clause ("The Go Authors... Extensions copyright 2011 Miek Gieben") | BSD-3-Clause (same text) |
| Status | README: "DNS version 2 is now available at https://codeberg.org/miekg/dns ... The version here will only see specific fixes and nothing else. At some point this repo will be archived." | README: "Everything from github.com/miekg/dns works"; "this version is young" so "backwards incompatible changes" are possible; v1.0.0 "2028?"; "rigorously follow upstream Go versions and use bleeding edge Go language features" |
| Perf claims (author) | baseline | ~2x qps serving, ~1.5x zone parsing, ~0.5x memory; `cmd/reflect` ~420K UDP / 340K TCP qps on good hardware; parses .SE (8M RRs) in ~11 s |
| Who uses it | CoreDNS 1.14.7, blocky, dnsproxy (v1.1.72), Gravity, external-dns, Consul, dnscontrol | catz (catalog zone generator), early adopters |

### 2.2 v1 API surface you will touch (from the clone)

**Server** (`server.go`):
```go
type Handler interface { ServeDNS(w ResponseWriter, r *Msg) }
type HandlerFunc func(ResponseWriter, *Msg)
type Server struct {
    Addr string; Net string            // "udp", "tcp", "tcp-tls" (DoT)
    Listener net.Listener; PacketConn net.PacketConn   // systemd socket activation / pre-bound sockets
    TLSConfig *tls.Config; Handler Handler
    UDPSize int                         // default MinMsgSize (512) — set 1232/4096
    ReadTimeout, WriteTimeout time.Duration; IdleTimeout func() time.Duration // RFC 5966 TCP idle (8s)
    TsigProvider TsigProvider; TsigSecret map[string]string
    NotifyStartedFunc func(); DecorateReader; DecorateWriter
    MaxTCPQueries int; ReusePort, ReuseAddr bool        // SO_REUSEPORT for multi-socket
    MsgAcceptFunc MsgAcceptFunc; MsgInvalidFunc MsgInvalidFunc
}
func (srv *Server) ListenAndServe() error
func (srv *Server) ActivateAndServe() error
func (srv *Server) Shutdown() error
func (srv *Server) ShutdownContext(ctx context.Context) error
```
`ResponseWriter` exposes `LocalAddr/RemoteAddr/WriteMsg/Write/Close/TsigStatus/TsigTimersOnly/Hijack`;
`ConnectionStater` gives TLS `ConnectionState` for DoT client-cert / SNI based client identification.

**Client** (`client.go`): `Client{Net, UDPSize, TLSConfig, Dialer, Timeout, Dial/Read/WriteTimeout,
TsigSecret, TsigProvider}`; `Exchange`, `ExchangeContext`, `ExchangeConn`, `Dial*WithTLS`.
`SingleInflight` is deprecated (no-op) — do your own singleflight.

**Messages** (`msg.go`, `defaults.go`): `SetReply/SetQuestion/SetNotify/SetUpdate/SetAxfr/SetIxfr/
SetTsig/SetEdns0/SetRcode`, `IsTsig/IsEdns0`, `Pack/PackBuffer/Unpack/Len/Truncate/Copy`.

**EDNS0** (`edns.go`): option codes LLQ, UL, NSID, ESU, DAU/DHU/N3U, SUBNET (RFC 7871), EXPIRE, COOKIE,
TCP_KEEPALIVE, PADDING, EDE (RFC 8914), REPORTING (RFC 9567), ZONEVERSION (RFC 9660), LOCAL range; each is a
struct implementing `EDNS0` interface. `OPT.SetDo()/Do()/UDPSize()`. `SVCB/HTTPS` in `svcb.go`.

**TSIG** (`tsig.go`): `TsigProvider{Generate, Verify}` interface (pluggable for KMS/HSM), `TsigGenerate/
TsigVerify(+WithProvider)`, algorithms hmac-sha1/224/256/384/512. Server verifies automatically when
`TsigSecret`/`TsigProvider` set; check `w.TsigStatus()` in handler. **SIG(0)** (`sig0.go`): `SIG.Sign/Verify`.

**Zone files** (`scan.go`): `NewZoneParser(r, origin, file)`, `SetDefaultTTL`, `SetIncludeAllowed`,
`SetIncludeFS(fs.FS)`, iterate `Next()` / `Err()` / `Comment()`; supports `$ORIGIN $TTL $INCLUDE $GENERATE`.
`ReadRR`, `NewRR(string)` for single records.

**Transfers** (`xfr.go`): `Transfer{DialTimeout, ReadTimeout, WriteTimeout, TsigProvider, TsigSecret,
TLS *tls.Config}`; client `t.In(msg, addr) (chan *Envelope, error)` handles both AXFR and IXFR envelope
framing; server `t.Out(w, q, ch chan *Envelope)` writes multi-message responses and TSIG-signs with
"timers only" continuation. `TLS` field = XFR-over-TLS (RFC 9103).

**Dynamic update builders** (`update.go`): `Msg.NameUsed/NameNotUsed/Used/RRsetUsed/RRsetNotUsed`
(prereqs) and `Insert/RemoveRRset/RemoveName/Remove` (updates). Server-side *processing* of UPDATE is
not in the library (see §5).

**DNSSEC** (`dnssec.go`, `dnssec_keygen.go`, `dnssec_keyscan.go`, `dnssec_privkey.go`, `nsecx.go`):
algorithms RSASHA1/256/512, RSASHA1NSEC3SHA1, ECDSAP256SHA256, ECDSAP384SHA384, ED25519, ED448 (constants;
`Generate` supports RSA, ECDSA P-256/384, Ed25519); `DNSKEY.Generate(bits)`, `KeyTag()`, `ToDS(hash)`,
`ToCDNSKEY()`, `DS.ToCDS()`, `RRSIG.Sign(crypto.Signer, rrset)`, `RRSIG.Verify(key, rrset)`,
`RRSIG.ValidityPeriod(t)`, `DNSKEY.ReadPrivateKey(r, file)` / `PrivateKeyString(priv)` (BIND
`K<zone>+<alg>+<id>.private` format), `HashName(label, alg, iters, salt)`, `NSEC3.Cover(name)`,
`NSEC3.Match(name)`. No chain builder, no key rollover logic, no NSEC3 "closest encloser" helper.

**Sockets**: `listen_socket_options.go` implements SO_REUSEPORT/SO_REUSEADDR; `udp.go` reads OOB data
(pktinfo) so replies use the right source IP on multi-homed hosts (matters for VIPs/keepalived).

### 2.3 v2 (Codeberg) differences that matter (from `_doc/README-v1-to-v2.md` and README)

- Packages: `dns`, `rdata` (RDATA split out of RR), `dnsutil` (Fqdn, Canonical, IsBelow, SetQuestion...),
  `dnstest` (httptest-like helpers), `dnsjson` (RFC 8427), `svcb`, `deleg` (draft-ietf-deleg), `dnshttp`
  (DoH), `dnsconf` (resolv.conf), `internal/dnsperf`.
- RR has no `Type`/`Rdlength` fields (type comes from Go type; `dns.RRToType(rr)`); `RR_Header` →
  `dns.Header`, `Ttl` → `TTL`; `NewRR` → `dns.New`; `IsDuplicate` → `Compare`/`sort.Interface`.
- `Msg` owns a `Data []byte` buffer (reusable/pooled, `msg.Hijack()`), is an `io.Writer`; OPT folded into
  `Msg.UDPSize`/`Msg.Security`; EDNS0 options are RRs in a `Pseudo` section; `dns.NewMsg(name, type)`.
- Server handlers take `context.Context`; `Zone(ctx)` returns the matched pattern; TSIG handled in the
  handler; TCP pipelining; thread-safe ResponseWriter; `Server` gains `ListenFunc`, `NotifyShutdownFunc`,
  `MsgPool`; `Shutdown(ctx)`; TLS requires `NextProtos` to include "dot".
- Client takes a `Transport` (like `http.Client`); `dns.Conn` removed.
- Dropped `\DDD` and `\x` escapes in domain names (perf); TXT rdata still supports escapes.
- Adds ML-DSA-44 DNSSEC (draft-westerbaan-dnssec-mldsa), DELEG, DSYNC (RFC 9859), ZONEVERSION, EDNS0 key
  tag (RFC 8145), DoT, DoH; **DoQ "not implemented, waiting until Go supports QUIC"** (so DoQ still
  needs quic-go either way).
- `cmd/atomdns`: a CoreDNS-like server ("production ready" per README) with handlers `dbfile`,
  `dbsqlite` (SQLite-backed zones), `sign`, `log`, `acl`, `metrics`, DoH/DoT, zone transfers and notifies;
  Conffile syntax resembles a Corefile. Worth reading as a second reference for zone storage in SQLite.

### 2.4 Recommendation for kudurru

- Use **v1.1.73** now. Reasons: (a) dnsproxy/upstream, CoreDNS plugin code (transfer, dynupdate, dnssec,
  cache), blocky resolvers, golang-dnstap and Gravity all use v1 types — borrowing code is copy-paste; (b)
  v2 explicitly reserves the right to break; (c) v2 forces Go 1.27 minimum today.
- Mitigate: keep `*dns.Msg`/`dns.RR` out of the store and API layers. kudurru's own `Record{Name, Type,
  TTL, Rdata string}` model (Gravity does this) converts at the edge via `dns.NewRR`. Then a v2 migration
  touches the server/transport package only.
- Revisit when CoreDNS migrates (watch github.com/coredns/coredns go.mod). Codeberg module path is
  fetchable with `go get codeberg.org/miekg/dns@latest` (Debian packages it as
  golang-codeberg-miekg-dns); proxy.golang.org availability **UNVERIFIED** but expected.

---

## 3. DNSSEC signing in Go

### 3.1 What exists

| Implementation | Mode | Denial | Keys | Rollover | Notes |
|---|---|---|---|---|---|
| CoreDNS `plugin/dnssec` | **online**, signs each response | NSEC "black lies" (draft-valsorda-dnsop-black-lies): synthesizes `qname NSEC \000.qname` with a fixed type bitmap; NXDOMAIN becomes NODATA | files (`Kzone+013+id.key/.private`) or AWS Secrets Manager JSON `{key, private}`; CSK by default, ZSK/KSK split if SEP bits differ | none | RRSIG cache (256-shard, 10k default) keyed by FNV64 of RRset text; sign window inception −3 h, expiration +8 d, re-sign at 75% (periodic 8 h sweep); singleflight per RRset; TTL fixed 3600 because it "can't see original TTL"; NSEC3 **not** supported |
| CoreDNS `plugin/sign` | **offline**, rewrites `db.<zone>.signed` | NSEC chain over authoritative names (AuthWalk) | CSK only, "does not support the ZSK/KSK split, nor will it do key or algorithm rollovers" | none | inception −3 h (−0..18 h jitter), expiration +32 d (+0..5 d jitter); resign every 6 d or when <14 d left (checked on SOA RRSIG); adds CDS (SHA1+SHA256) and CDNSKEY at apex; sets SOA serial = unix epoch; NSEC TTL = SOA minimum |
| CoreDNS `plugin/dynupdate` (2026) | — | — | — | — | **rejects** updates to DNSSEC/meta types "because the plugin cannot regenerate them after an update"; IXFR + auto re-sign "not supported" |
| Technitium (C#) | offline, in-server | NSEC **and NSEC3** | RSA, ECDSA, EdDSA; KSK/ZSK | per README "automatic key rollover" **UNVERIFIED** (not in fetched README; v8 blog 2022 says RSA+ECDSA; Hostinger listing adds EdDSA) | validation for recursion/forwarders; DNSSEC over all transports |
| hickory-dns (Rust) | auto-resign on dynamic update | NSEC3 (RFC 5155 listed) | `dnssec-ring` | — | offline signing not supported |
| Gravity | none | — | — | — | Gravity does not sign (grep found no DNSSEC signing code) |
| miekg v2 `cmd/atomdns` | `sign` handler | **UNVERIFIED** (README says "zone signing") | — | — | — |

### 3.2 Design kudurru should implement (synthesis)

1. **Zone-level, store-driven signing on the leader.** Every mutation (API, RFC 2136, AXFR-in for
   secondaries that re-sign is out of scope) produces a new zone version; the leader re-signs only the
   changed RRsets plus the affected NSEC/NSEC3 chain links, bumps SOA serial, and commits RRSIG/NSEC(3)/
   DNSKEY/CDS/CDNSKEY rows into the replicated store. Followers serve what the store says. Rationale:
   ECDSA signatures are randomized, so independent signing per replica yields different RRSIGs across
   nodes (breaks IXFR journals and confuses caches); Ed25519 is deterministic but a leader-only pipeline is
   simpler anyway.
2. **Defaults:** ECDSAP256SHA256 (alg 13) CSK; Ed25519 (15) opt-in; RSASHA256 (8) only for legacy
   parents. NSEC3 with RFC 9276 parameters (SHA-1, 0 extra iterations, empty salt, opt-out off); NSEC
   available for zones that don't care about enumeration (cheaper, CoreDNS-compatible).
3. **Chain builder** (~200 LOC): collect authoritative owner names incl. empty non-terminals (walk a
   sorted name tree — CoreDNS `plugin/file/tree` is a usable red-black tree with canonical ordering);
   NSEC: link in canonical order with type bitmap (sorted, include RRSIG+NSEC); NSEC3: `HashName` each
   owner, sort hashes, link, emit `NSEC3PARAM` at apex. Lookups for denial use `NSEC3.Cover/Match` and
   closest-encloser search (RFC 5155 §7.2.1 / RFC 7129).
4. **Timers:** copy CoreDNS `sign`: inception −3 h with jitter, expiration ~30 d with jitter, resign at
   ~6 d or when <14 d remain; store `next_resign_at` per zone so any leader can resume.
5. **Key lifecycle:** keys live in the store encrypted (age/AES-GCM with a node-local or KMS-wrapped
   master key), serialized with `PrivateKeyString` (BIND format) so `dnssec-keygen`/`ldns` interop works.
   State machine per key: generated → published → active → retired → removed, with RFC 7583 pre-publish
   ZSK rollover and double-DS/double-KSK for KSK, timed by DNSKEY TTL + propagation delay; CDS/CDNSKEY
   (RFC 7344/8078) published automatically so parents that support it (and Cloudflare via API) pick up DS.
6. **Validation in CI:** sign a fixture zone and run `ldns-verify-zone`/`dnssec-verify` and
   `delv +vtrace` in integration tests; miekg `RRSIG.Verify` for unit tests.
7. **Validating forwarder** (resolver side): blocky now ships `resolver/dnssec` (DNSSEC validation of
   upstream answers; `dnssec_resolver.go` + `resolver/dnssec/`) — a Go reference for chain-of-trust
   validation with DNSKEY/DS fetching. dnsproxy only sets the DO bit (`DNSSECEnabled`).

---

## 4. Transports: DoT, DoH, DoH3, DoQ (servers and clients)

**Versions:** quic-go **v0.63.0** (2026-09-22, MIT; `go 1.26.0`; FIPS 140-3 when built with Go ≥1.26
since v0.60; CVE-2026-40898 HTTP/3 trailer memory fixed in v0.59.1). CoreDNS and blocky pin v0.63.0,
dnsproxy v0.60.0.

### 4.1 Server patterns (from the clones)

- **DoT:** `dns.Server{Net: "tcp-tls", TLSConfig: cfg}`; set `NextProtos: ["dot"]`; idle timeout per
  RFC 7858/7766; client cert optional.
- **DoH (h1.1/h2):** `http.Server` with `TLSNextProto`/`NextProtos: ["h2","http/1.1"]`; route
  `/dns-query`; GET `?dns=<base64url>` and POST `application/dns-message`; respond with
  `Content-Type: application/dns-message`, `Cache-Control: max-age=<min TTL>`. CoreDNS `plugin/pkg/doh`
  has `RequestToMsg`/`NewRequest` helpers; `DoHWriter` carries real client addrs. Support
  `X-Forwarded-For`/PROXY protocol only from `TrustedProxies` (dnsproxy `Config.TrustedProxies netutil.SubnetSet`).
- **DoH3:** dnsproxy `proxy/serverhttps.go`:
  ```go
  tlsConfig.NextProtos = []string{"h3"}
  quicListen, _ := quic.ListenAddrEarly(addr, tlsConfig, &quic.Config{MaxIdleTimeout: ..., Allow0RTT: true})
  p.h3Server = &http3.Server{Handler: mux}       // same mux as the h2 server, same ip:port over UDP
  ```
  CoreDNS `server_https3.go` adds limits: `DefaultHTTPS3MaxStreams = 256`, `MaxHeaderBytes 16 KiB`, a
  semaphore-limited `QUICListener` wrapper for max connections. Advertise with `Alt-Svc` via
  `http3.Server.SetQUICHeaders(hdr)`.
- **DoQ (RFC 9250):** CoreDNS `server_quic.go`: `tlsConfig.NextProtos = ["doq"]`,
  `quic.Listen(pc, tls, &quic.Config{MaxIdleTimeout, MaxIncomingStreams: 256, MaxIncomingUniStreams: 256,
  Allow0RTT: true})`; accept conns (default max 200), accept streams (worker pool 1024), read 2-byte
  length + message, reject messages with non-zero ID per RFC 9250 §4.2.1 (DOQ_PROTOCOL_ERROR=2), write
  length-prefixed reply, close stream. Error codes: `DoQCodeNoError=0, InternalError=1, ProtocolError=2`.
  CVE-2026-47703 (AdGuard "DoQ-to-UDP state reduction / source-port oracle"): when proxying DoQ clients to
  UDP upstreams keep per-query random DNS IDs and source ports — do not reuse a fixed 0 ID.
- **Certificates:** `caddyserver/certmagic` v0.25.4 (CoreDNS, miekg v2) for ACME + auto-renew; in k8s,
  mount cert-manager Secrets and reload via `tls.Config.GetCertificate` + fsnotify.
- **PROXY protocol:** CoreDNS now has `plugin/pkg/proxyproto` (used by all server types). Needed so
  **dnsdist in front of kudurru** (`useProxyProtocol=true` on the dnsdist backend) preserves client IPs
  for client-group blocking. This is a hard requirement given the dnsdist mandate.

### 4.2 Client/upstream library: `github.com/AdguardTeam/dnsproxy/upstream` (Apache-2.0)

- `upstream.AddressToUpstream("quic://dns.adguard-dns.com", &upstream.Options{Bootstrap, Timeout,
  InsecureSkipVerify, HTTPVersions, PreferIPv6, ...})` returns an `Upstream{Exchange(*dns.Msg)
  (*dns.Msg, error); Address(); Close()}`; schemes: plain `1.1.1.1`, `udp://`, `tcp://`, `tls://`,
  `https://` (h2 and h3 via `http3.Transport` with `OnlyCachedConn` optimisation and 0-RTT GET),
  `quic://`, `sdns://` (DNSCrypt/DoH stamps). Bootstrap resolvers for hostnames.
- `proxy` package (`proxy.New(&proxy.Config{...})`, `Start(ctx)`, `Shutdown(ctx)`, `Resolve(ctx, dctx)`)
  is a complete forwarding proxy with cache (`CacheSizeBytes`, min/max TTL, optimistic serve-stale), DNS64,
  bogus-NXDOMAIN, EDNS client subnet, rate limiting, `UpstreamMode` (load-balance / parallel / fastest),
  `PendingRequests` dedup, `TrustedProxies`, `PrivateRDNSUpstreamConfig`, `HTTPConfig{HTTP3Enabled...}`.
  Logger is `*slog.Logger`. Go directive `go 1.26.8`. Latest release v0.86.0 (2026-10-01).
- blocky's `resolver/` chain is the other reference (parallel_best, strict, upstream_tree, conditional,
  caching with prefetch, ede, ecs, dns64, rebinding protection, rate_limiting, sudn, client_names).

---

## 5. Authoritative protocol features: RFC 2136, NOTIFY, AXFR/IXFR, TSIG, catalog zones

- **RFC 2136 UPDATE (server side):** parse `Opcode == dns.OpcodeUpdate`; Question = zone (ZOCOUNT=1),
  Answer = prerequisites, Ns = update RRs, Extra may hold TSIG/OPT. Implement §3.2 prereq semantics
  (RRset exists value-independent/dependent, RRset does not exist, name in use, name not in use), §3.4
  updates (ADD, DELETE RRset, DELETE all RRsets at name, DELETE specific RR) with CNAME/SOA/NS apex
  invariants, atomic apply, serial bump, respond NOERROR/REFUSED/NOTAUTH/NOTZONE/NXRRSET/YXRRSET.
  CoreDNS `plugin/dynupdate` (experimental, 2026) is a current Go reference: TSIG via `tsig` plugin +
  `allow <key> <owner> <rrtype>` ACL, bbolt persistence committed *before* publishing the snapshot,
  coalesced NOTIFY, AXFR of current snapshot, SOA serial 0 rejected, `max_bytes` limit. Reuse its ACL
  model (key name × owner × type). Also honour `require_opcode UPDATE` style policies.
- **NOTIFY (RFC 1996):** outbound `m.SetNotify(zone)` to each secondary (CoreDNS `transfer { to ... }`,
  optional `source` address) with retry/backoff and coalescing per zone; inbound NOTIFY (Opcode Notify)
  on a secondary zone → SOA query over TCP → AXFR/IXFR if remote serial is newer (CoreDNS
  `file/secondary.go: shouldTransfer`, `TransferIn` with candidate zone + validate callback, then
  `setData` swap and `t.Notify(origin)` fan-out).
- **AXFR/IXFR server:** CoreDNS `plugin/transfer` defines
  `type Transferer interface { Transfer(zone string, serial uint32) (<-chan []dns.RR, error) }` — plugins
  return a channel of RR batches; if `serial` > 0 and the plugin has a journal it returns IXFR deltas,
  else full AXFR (IXFR "with AXFR fallback"). kudurru: keep a per-zone **journal of N recent versions**
  (list of (serial, deletions, additions)) in the store to answer IXFR; respond via `dns.Transfer.Out`;
  TSIG-sign; restrict by ACL (CoreDNS `acl` plugin: `allow type AXFR net 10.0.0.0/8`) and XFR-over-TLS.
- **TSIG:** `Server.TsigSecret` map keyed by canonical key name; `w.TsigStatus()`; CoreDNS `tsig` plugin
  also loads BIND `key "name" { secret "..."; };` files and exposes `tsig.ValidatedKeyName(ctx)`. Store
  keys in the replicated store (encrypted), expose in UI with rotation.
- **Catalog zones (RFC 9432):** no Go library exists (catz is a CLI generator; dnsctl drives BIND).
  Format is trivial: `version TXT "2"`, members as `<unique-label>.zones.<catalog> PTR <member-zone>`,
  optional `group`/`coo` properties; consumer side: a "secondary catalog" zone that auto-provisions
  secondary zones on change. Technitium v13+ (2024), BIND 9.18.3+, Knot 3.1+, PowerDNS 4.7+ interoperate.
  kudurru should **produce** a catalog from its store (so BIND/Knot/Technitium secondaries auto-provision)
  and **consume** one (so kudurru can be a secondary farm). ~300 LOC.
- **Zone types to support (Technitium's list):** Primary, Secondary, Stub, Conditional Forwarder,
  Catalog, Secondary Catalog (Technitium also lists Secondary Root / Secondary Forwarder **UNVERIFIED**).
  Technitium facts: v15 (Apr 2026), .NET 10, GPL-3.0 (so **never copy code**, only ideas), clustering
  since v14, DoH over HTTP/1.1+2+3, DoQ, XFR over TLS/QUIC, APP records (plugins), built-in HTTP API.

---

## 6. Storage and replication

### 6.1 What Gravity actually does (verified from clone, v0.33.0 May 2026)

- `pkg/roles/etcd/role.go`: `embed.NewConfig()`; `AutoCompactionMode="periodic"`, `Retention="60m"`,
  `MaxRequestBytes=10 MiB`, `PeerAutoTLS` with per-instance certs, `SelfSignedCertValidity=1`,
  `ListenClientUrls` on localhost:2379 + listen IP; join flow = `ETCD_JOIN_CLUSTER="<token>,<api url>"`
  → POST `/api/v1/etcd/join` → gets `InitialCluster` → `ClusterState=existing`; on join failure it
  **deletes `member/`** and retries. Cluster ops exposed as `/api/v1/etcd/{members,join,move_leader,remove}`
  plus `gravity cli etcdctl member remove <id>`.
- `pkg/storage`: thin `clientv3` wrapper with hooks; keys are path-like: `/gravity/dns/zones/<zone>` for
  zone JSON (`handlerConfigs []map[string]any`, `defaultTTL`, `authoritative`, `hook`), records under
  `/gravity/dns/zones/<zone>/<name>/<type>/<uid>` as JSON (`data, ttl, mxPreference, srvPort/Priority/Weight,
  soa*`). DNS role watches the prefix (`watcher.Watcher[*Zone]`) and the `memory` handler keeps records in
  a map so **resolution keeps working when etcd loses quorum** ("With 2 nodes... the other node cannot
  access etcd either. Gravity works around this issue with caching objects in memory").
- Docs: "Gravity requires at least 1 CPU core and 1 GB of memory"; odd member counts recommended.
- Auth: go-oidc v3.21.0 `provider.Verifier(&oidc.Config{ClientID})`, users with `Permissions []`,
  API tokens, local users; `tokenUsernameField` claim mapping.
- API: `swaggest/rest` (code-first OpenAPI), generated TS client `gravity-api`; UI Lit 3.3 + PatternFly 4 +
  Chart.js 4 + CodeMirror 6; build via rollup+esbuild; Node ≥24.

### 6.2 Option matrix

| Option | Consistency / watch | Single-node cost | k8s fit | Risk | Verdict |
|---|---|---|---|---|---|
| **Embedded etcd** (`go.etcd.io/etcd/server/v3/embed` v3.7.2, Apache-2.0) | Linearizable, MVCC, watch, leases, txn, compaction, snapshots, etcdutl backup | Heavy: pulls grpc/zap/bbolt/prometheus; baseline RSS hundreds of MB (Gravity needs 1 GB); fsyncs every write ("50 sequential IOPS required... 500 recommended", WAL fsync p99 <10 ms); 2 GiB default quota; k3s warns about SD cards | Needs StatefulSet + stable peer URLs; join orchestration is bespoke (Gravity token flow); 2-node cluster is *worse* than 1 | Version coupling with etcd server releases; "database space exceeded" ops; peers must agree on TLS | Viable fallback; what Gravity proves works |
| **hashicorp/raft** v1.8.0 (2026-09-16, MPL-2.0, Go ≥1.16) + raft-boltdb v2.3.1 (log/stable store) | Linearizable writes; reads from local FSM (fine for DNS); watch = FSM apply hook | Light: one voter bootstraps itself (`BootstrapCluster`); FSM is your own; rqlite (v10.2, Jun 2026) and Consul prove it | StatefulSet with headless Service; peers discovered via DNS/Lease; `raft.NewTCPTransport`; snapshot to PVC | You own FSM, snapshot/restore, membership API, backup; raft-wal still "experimental" (use raft-boltdb) | **Recommended** |
| **lni/dragonboat** | Multi-group raft, fast | — | — | v4.0 "working towards" since 2022-06; pkg.go.dev shows only pseudo-version `v4.0.0-20250723...`; last news 2025-07 (LLM wiki) | Avoid |
| **SQLite + Litestream** (0.5.x LTX, VFS read replicas) / **marmot** (NATS, LWW, HLC) | Litestream = backup + read-replica, single writer; marmot = leaderless eventual LWW | Minimal | Litestream to S3/NATS fine for backup | Not multi-writer consistent; marmot's conflict semantics unsuitable for authority data | Litestream for **backups**, not replication |
| **k8s apiserver (CRDs) as *the* store** | Watch, RBAC, audit, GitOps | n/a outside k8s → needs a 2nd store implementation | Best "feels native" | etcd request cap 1.5 MiB/object (ConfigMap/Secret 1 MiB), "etcd can be overloaded with only a few thousand large objects", watch-cache memory on LIST, large zones = thousands of CRs; query logs/stats cannot live there | Use CRDs as **input**, not as the store (see 6.3) |
| **NATS JetStream KV** (nats-server v2.14.3, Apache-2.0; embeddable `server.NewServer(opts)`, `nats.InProcessServer(ns)` — API names **UNVERIFIED** this session) | Raft-replicated streams; KV with watch; no multi-key txn | Medium (JetStream + raft) | Fine as StatefulSet | Second consensus system; KV semantics weaker than etcd; kine supports it as backend | Not for v1 |
| **kine** (v1.14.x, Apache-2.0; `pkg/endpoint`, `pkg/drivers/{sqlite,pgsql,mysql,nats}`) | etcd *subset* (Put/Get/Delete/Txn-as-k8s-uses/Watch/Lease Grant) over SQL | Light with SQLite | Designed for k3s | Only the etcd API subset the kube-apiserver needs | Interesting bridge if you chose the etcd client API; otherwise skip |

Engine under raft: **bbolt v1.5.0** (2026-06-03, MIT; CVE-2026-33817 index-out-of-range on branch pages
with zero elements, fixed version not listed — track) is what CoreDNS dynupdate and etcd use: pure Go,
single-writer, mmap, simple buckets. **modernc.org/sqlite v1.54.0** (SQLite 3.53.3, Jul 2026, cgo-free)
gives SQL for the UI's search/pagination and shares one engine with the query log; alternatives
mattn/go-sqlite3 v1.14.49 (cgo, SQLite 3.53.4) — rejected for static builds — and ncruces/go-sqlite3
v0.35.x (wasm→Go via wasm2go since 2026-03, cgo-free, supports VFS/Litestream fork). Pebble v2.1.3 is
overkill (LSM for CockroachDB).

### 6.3 Recommended architecture

```
                         ┌─────────────── kudurru node ───────────────┐
  API/UI/RFC2136/CRD ──▶ │ Command (proposal) ─▶ raft log ─▶ FSM.Apply │
  controller (leader)    │                                   │        │
                         │   local engine (bbolt | sqlite)  ◀┘        │
                         │        │ change events                      │
                         │        ▼                                    │
                         │   in-memory zone tree (serving), DNSSEC     │
                         │   signer (leader only), NOTIFY sender,      │
                         │   integrations (Cloudflare/UniFi, leader)   │
                         └──────────────────────────────────────────────┘
   docker run  → one voter, same binary, same code path (rqlite model)
   k8s         → StatefulSet (3 replicas), peers from headless Service, Lease for "controller leader",
                 CRDs (Zone/Record/Blocklist/ClientGroup/Forwarder) reconciled INTO the store by the
                 leader via controller-runtime; optional "read-only from CRDs" mode for GitOps shops.
```

Why CRDs-as-input: it keeps one store engine, gives `kubectl apply -f zone.yaml` + GitOps + k8s RBAC +
audit for free, keeps the apiserver out of the query/stat path, and avoids the per-object size limits for
big zones (status can summarize rather than mirror). external-dns can also target kudurru via a webhook
provider (`/`, `/records` GET/POST, `/adjustendpoints`, external-dns v0.23.0) — which is also exactly the
shape of the Cloudflare/UniFi "integration" interface (see §14).

### 6.4 Store interface sketch (engine-agnostic, raft-replicated)

```go
// package store — everything mutable in kudurru goes through Commands so raft can replicate them.
type Command struct {
    Kind    CommandKind      // ZonePut, ZoneDelete, RecordSetPut, RecordSetDelete, KeyPut, ListPut, ...
    Zone    string           // canonical, lowercased, FQDN
    Payload []byte           // protobuf/CBOR of the typed body
    Txn     []Command        // optional atomic batch (RFC 2136 update = one Txn)
    Expect  *Precondition    // optional CAS: zone serial / record set version must match
}

type Store interface {
    // Reads (local, possibly stale by one apply; DNS serving reads the in-memory tree, not this).
    Zone(ctx, name string) (*Zone, error)
    Zones(ctx, q ZoneQuery) (Page[Zone], error)
    RRsets(ctx, zone string, q RRsetQuery) (Page[RRset], error)
    Journal(ctx, zone string, fromSerial uint32) ([]Delta, bool /*have*/, error) // for IXFR
    Keys(ctx, zone string) ([]DNSSECKey, error)
    Get(ctx, key string, out any) error        // config/lists/clients/tokens/sessions as typed KV
    // Writes (go through consensus; return when applied on this node).
    Apply(ctx, cmd Command) (AppliedResult, error)
    // Change feed: FSM.Apply publishes; consumers (zone tree, signer, notifier, CRD status) subscribe.
    Watch(ctx, prefix string) (<-chan Event, error)
    // Lifecycle / ops.
    Snapshot(ctx, w io.Writer) error; Restore(ctx, r io.Reader) error
    Members(ctx) ([]Member, error); Join(ctx, m Member) error; Leave(ctx, id string) error
    IsLeader() bool; LeaderAddr() string
}

// Engine is the single-node persistence the FSM writes to; two implementations, same tests.
type Engine interface {
    Begin(writable bool) (Tx, error)   // bbolt: tx; sqlite: BEGIN IMMEDIATE
    Close() error
}
```

Serving path: `zone.Tree` (CoreDNS `plugin/file/tree` red-black tree keyed by canonical name) rebuilt
per zone on change events, with `atomic.Pointer` swap (Gravity's `setData` pattern). Forwarders/blocking
read config snapshots the same way. Query log and stats never touch raft.

---

## 7. Query log, stats, caching

### 7.1 Query log storage

| Design | Facts | Use |
|---|---|---|
| **In-memory ring (Pi-hole FTL)** | FTL keeps "the last 24 hours" of queries in shared memory; `overTime` buckets at 10-minute spacing (600-s apart timestamps; 144/day, array grows dynamically); flushes to `/etc/pihole/pihole-FTL.db` every `DBinterval` (60 s default) and on exit; `MAXDBDAYS` 365 default; `MAXLOGAGE` ≤24 h reloaded on start | Live tail + "last 24 h" dashboard from RAM; survive restarts by reloading from SQLite |
| **SQLite (blocky)** | gorm model `logEntry{RequestTS idx, ClientIP, ClientName idx, DurationMs, Reason, ResponseType idx, QuestionType, QuestionName, EffectiveTLDP, Answer, ResponseCode, Hostname}`; `pendingEntries` batched with `dbFlushPeriod`; retention `logRetentionDays`; driver `glebarez/sqlite` v1.11.0 (modernc under gorm); also MySQL/Postgres, CSV per client, console, **dnstap writer** (`dnstap/golang-dnstap` v0.4.0 frames to unix/tcp) | Per-node durable log; WAL mode; `synchronous=NORMAL`; partition by day (one table per day or `DELETE WHERE ts<`) to keep VACUUM cheap |
| **Rollups** | Pi-hole's dashboard reads counters (total/blocked/cached/forwarded, top domains/clients, overTime, query types, reply types) | Maintain `stats_minute(ts, client, qtype, rcode, status, n)` table + in-memory `stats.Collector` (blocky: hourly `bucket`s with pruned top-N maps and eviction) |
| **DuckDB** (duckdb-go v2.10505.x = DuckDB 1.5.6) | cgo, bundles native lib (~tens of MB), breaks `CGO_ENABLED=0` distroless build | Optional separate analytics tool/sidecar only |
| **ClickHouse** (clickhouse-go v2.4x, pure Go, native protocol) | Fine as optional sink; Grafana dashboards (blocky ships `blocky-query-grafana*.json`) | Optional `sink: clickhouse` exporter + dnstap/OTLP |
| **Multi-replica** | Gravity exposes `/api/v1/cluster/nodes/<id>/log` fan-out; logs stay per node | API fan-out (leader asks each node) for the UI; or central sink |

### 7.2 Caching

- Response cache: CoreDNS `plugin/cache` semantics are the bar — success/denial split capacities and
  TTL caps, min TTL (5 s), `prefetch N [duration] [pct]`, `serve_stale` (immediate/verify), `servfail`
  TTL, `keepttl`, DO-aware keys, ECS bypass; `plugin/pkg/cache` is 256 shards with random eviction.
  dnsproxy adds optimistic caching (`CacheOptimistic`, serve expired then refresh). blocky adds Redis
  (`go-redis v8`) as a **shared cache across replicas** — worth offering as optional.
- Generic caches: **otter v2** (Caffeine-style adaptive W-TinyLFU, Go ≥1.24; ~5 ns parallel reads in
  third-party microbenchmarks), ristretto v2 (generics), hashicorp/golang-lru/v2 v2.0.7 (CoreDNS pins; simple
  LRU + `expirable`). Recommendation: sharded TTL map for the DNS cache (deterministic, cheap), otter for
  LRU/LFU needs (client-name rDNS cache, list membership memo).
- Blocklists: blocky `lists/` streams list files with generic `SeriesParser[T]` + `parsers.Hosts`
  supporting hosts-file, plain host list, wildcard `*.example`, adblock `||example^` prefix (hosts.go
  trims `||`); `download_cache`, periodic refresh, `trie.Trie` with `SplitTLD` for suffix match. AdGuard's
  `urlfilter` engine (adblock syntax incl. `$dnstype`, `$client`, `$dnsrewrite`) is the richer option.

---

## 8. Config, CLI, web framework, OpenAPI, auth

| Concern | Choice (version) | Why / notes |
|---|---|---|
| Config | **koanf v2.3.x** (latest tags v2.3.1/2.3.2; activity Apr 2026; uses go-viper/mapstructure v2) | Providers: `file`+`yaml` parser, `env` with `KUDURRU_` prefix and `__`→`.` mapping, `posflag` for cobra flags; precedence flags > env > file > defaults; no key-lowercasing surprise (viper's known flaw); only referenced providers compile in. viper remains fine but heavier. blocky uses yaml.v2 + custom env; Gravity uses env vars only |
| CLI | **cobra v1.10.2** (Dec 2025; pflag 1.0.9) | Same UX as kubectl/helm; completions + man/md docs; blocky and Gravity use it. Alternatives: kong v1.16.1 (Aug 2026, MIT, struct-driven), urfave/cli v3.10.1 |
| HTTP routing | Go 1.22+ `net/http` method+pattern mux; **chi v5.3.2** optional | chi is what blocky uses with oapi-codegen; std mux is enough with huma |
| OpenAPI | **huma v2.38.0** (May 2026, code-first OpenAPI 3.1 + JSON Schema, router-agnostic incl. std mux/chi, built-in validation, Scalar/Stoplight/Swagger UIs) | Spec is derived from Go handlers → generate TS client (`openapi-typescript`/`orval`) and let the `kudurru` CLI reuse the same client. Alternatives: oapi-codegen v2.8.0 (Jul 2026, spec-first; std/chi/echo/gin/fiber/iris/gorilla) — blocky's choice; ogen v1.23.0 (Jul 2026, spec-first, no reflection); swaggest/rest — Gravity's |
| OIDC RP | **coreos/go-oidc v3.17.0** (`NewProvider`, `Verifier`, Go ≥1.24) or **zitadel/oidc v3.48.1** (Aug 2026; RP with PKCE, device flow, also an OP) | Authorization-code + PKCE, discovery, JWKS rotation; group/role claim mapping (Gravity: `tokenUsernameField`); also accept OIDC-issued JWTs as API bearer tokens (Gravity 0.19+) |
| Sessions/tokens | secure cookie session (HMAC+AES), API tokens hashed in store, short-lived JWT optional | |
| RBAC | roles `admin / operator / viewer` + zone-scoped grants; policy table in store; middleware checks `(subject, action, resource)` | casbin is optional later; keep it simple and auditable. In k8s mode, let k8s RBAC govern CRDs and map OIDC groups → kudurru roles |
| SPA embed | `//go:embed all:dist` + `fs.Sub` + SPA fallback to `index.html`, `Cache-Control` immutable for hashed assets | blocky `web/index.go` and Gravity `pkg/roles/api/ui.go` do this |

---

## 9. Frontend stack for a Pi-hole-quality dashboard

| Option | Versions (verified) | Pros | Cons |
|---|---|---|---|
| **React 19.2 + TS + Vite 8 + Tailwind + shadcn/ui + TanStack Query/Table v9** | React 19.2.6 (May 2026); Vite 8.0 (2026-03-12, Rolldown, `@vitejs/plugin-react` v6); TanStack Table v9 (2026-08-04, plugin system, "up to 90% memory savings" on large tables); shadcn CLI v4 (Mar 2026); Base UI now default base (Jul 2026, **UNVERIFIED**) | Largest component/data-table/chart ecosystem; virtualized query-log tables; many contributors | Bundle size (~42 KB runtime + libs) |
| Svelte 5 + Vite (SPA) | Svelte 5.56.x; SvelteKit 3 stable 2026-10-01 (requires Vite 8) — use plain Vite SPA, not SvelteKit, since Go serves it | Smallest/fastest runtime; good DX | Fewer dashboard-grade data tables; TanStack v9 has a Svelte adapter |
| Lit 3 (Gravity) | lit 3.3.3; Gravity pairs with PatternFly 4 CSS, Chart.js 4, rollup+esbuild | Web components, framework-free | Thin ecosystem for tables/forms; Gravity's UI is serviceable, not Pi-hole-polished |
| Preact | — | tiny | smaller ecosystem than React |

Charts: **ECharts 6.1.0** (May 2026; canvas, themes, many chart types) for the general dashboard;
**uPlot 1.6.32** for the 24 h live time series (ApexCharts' 2026 benchmark: uPlot draws 100k points in
9 ms and ~2,560 updates/s vs Chart.js ~232; uPlot ~50 KB; ≥1.6.31 fixes a prototype-pollution issue);
Recharts 3.9.x (SVG) is fine for moderate data but slowest in benchmarks. Pi-hole v6 web uses Chart.js.
Tables: TanStack Table v9 + TanStack Virtual for the query log. State/data: TanStack Query with the
generated OpenAPI client; live tail via SSE (`text/event-stream`) or WebSocket from the Go ring buffer.
Recommendation: React stack above; keep the UI a pure SPA (no SSR) so `go:embed` stays trivial.

---

## 10. Observability, logging, hot reload, graceful shutdown

- **Metrics:** prometheus/client_golang **v1.24.1** (2026-07-24; requires Go ≥1.25; UTF-8 name
  validation default). Model after CoreDNS `metrics` plugin: `kudurru_dns_requests_total{server,zone,
  proto,family,type}`, `dns_request_duration_seconds` histogram, `responses_total{rcode}`,
  `cache_{hits,misses,entries}`, `blocked_total{list,group}`, `upstream_rtt_seconds{upstream}`,
  `dnssec_signatures_total`, `raft_{term,commit_index,last_contact}`, `zone_serial{zone}`. Ship Grafana
  dashboards (blocky `docs/blocky-grafana.json`, Gravity `extras/grafana`).
- **Tracing:** OpenTelemetry Go **v1.45.0** (Aug 2026; v1.47.0-rc.1 brings Logs API v1). Span per
  query (`dns.query`) with child spans per resolver stage/upstream (CoreDNS `trace` plugin still uses
  OpenTracing — don't copy that part); `otelhttp` for API; OTLP gRPC/HTTP exporter; sample by default 0.1%.
- **Logging:** `log/slog` (JSON/text handlers, `slog.LevelVar` for runtime level, request-scoped attrs
  via context). dnsproxy already takes `*slog.Logger`; blocky/Gravity use logrus/zap (don't copy).
  Query log is **not** the app log; dnstap is the structured export (golang-dnstap v0.4.0; CoreDNS
  `dnstap` plugin: 10k-message buffer, drops above, `full` includes wire message, identity/version/extra).
- **Hot reload:** config file via fsnotify v1.9.0 + SIGHUP; zones/lists/clients via store watch (no
  restart); TLS certs via `GetCertificate`; listeners re-bound only when addresses change (CoreDNS
  `reload` plugin reloads the whole Corefile on checksum change every 30 s — fine for config, but prefer
  event-driven for data). blocky: `refreshPeriod` for lists with download cache + `strategy: fast|blocking`.
- **Graceful shutdown:** order = readiness→unready, stop accepting (close UDP/TCP/TLS/QUIC listeners),
  drain in-flight with `dns.Server.ShutdownContext`, `http.Server.Shutdown`, `http3.Server.Shutdown(ctx)`,
  flush query-log batch, raft `Shutdown()` (transfer leadership first if leader: `LeadershipTransfer()`),
  close engine. CoreDNS `graceTimeout`; k8s `terminationGracePeriodSeconds` ≥ 30 s + `preStop` sleep.
- **Multi-socket:** CoreDNS `multisocket` plugin starts N servers with SO_REUSEPORT (default GOMAXPROCS)
  for throughput on many-core hosts; miekg `Server.ReusePort` makes this one line.
- **Binding :53 in containers:** distroless `nonroot` user needs `CAP_NET_BIND_SERVICE`
  (`securityContext.capabilities.add: [NET_BIND_SERVICE]`) or listen on 1053 behind dnsdist/hostPort.

---

## 11. Testing DNS servers

- Unit: `dns.HandlerFunc` + `dns.Server` bound to `127.0.0.1:0` via `ActivateAndServe` with
  `net.ListenPacket`/`net.Listen`; CoreDNS `plugin/test` package: `test.Case{Qname, Qtype, Rcode, Answer,
  Ns, Extra, Do}`, record builders `test.A("example.org. 3600 IN A 1.2.3.4")`, `test.SortAndCheck`,
  `test.ResponseWriter`, `test.NextHandler`, `test.ErrorHandler`, temp zone files (`file.go`),
  Prometheus scraping helper (`scrape.go`). miekg v2 ships `dnstest` (httptest analogue).
- dnsproxy ships `dnsproxytest` (fake upstreams); blocky has mock UDP/DoT/DoQ upstream servers
  (`resolver/mock_*`), Ginkgo v2 suites, testcontainers-go v0.44.0 (Redis, MariaDB, Postgres).
- Interop/e2e: testcontainers with BIND9/Knot/NSD for AXFR/IXFR/NOTIFY/TSIG/catalog interop and
  Technitium for catalog consumer tests; `dig`/`kdig +tls/+https/+quic`; `delv` and `ldns-verify-zone`
  for DNSSEC; `dnsperf`/`resperf` and Gravity `hack/dnspef` for load; `go test -race`; Go native fuzzing on
  the UPDATE/AXFR parsers (miekg v2 removed its fuzz harness — write your own).
- Conformance: dnstap output + `dnstap` CLI; IETF DNS test suites (e.g., `dnsviz` offline) optional.

---

## 12. Release tooling

- **goreleaser v2.18** (2026-08-23; v2.14 added the `kos:` pipe) — one config builds `CGO_ENABLED=0`
  binaries for linux/darwin × amd64/arm64 (+ riscv64 opt), archives, checksums, SBOM (syft), nfpm
  deb/rpm/apk, Homebrew tap, GitHub release notes; the `kos:` block uses **ko v0.19.1** (2026-06-29; now
  under the `ko-build` org) to publish multi-arch images directly from Go without a Dockerfile. Default
  base: `cgr.dev/chainguard/static` (goreleaser) vs `gcr.io/distroless/static:nonroot` (ko) — pin one by
  digest; both are shell-less and run as uid 65532.
- **cosign v3.1.3** keyless (Sigstore bundle format default in v3.1; v4 planned) to sign images by
  digest and attest SBOM/provenance; GitHub `attest-build-provenance` for SLSA. Verify in Helm/compose docs.
- cgo-free is a release requirement (distroless static, cross-compilation, `ko`): hence modernc/ncruces
  SQLite, no DuckDB, no mattn.
- Helm chart (chart-releaser), `docker compose` profiles (single, cluster-3, with dnsdist + keepalived),
  k8s manifests with StatefulSet + headless Service + PodDisruptionBudget + Lease RBAC.

---

## 13. Minimum Go version

- Go 1.27.0 released **2026-08-19** (generic methods, ML-DSA in crypto, new JSON packages,
  `asynctimerchan` removed); Go 1.26 released February 2026 (`new(expr)`, goroutine leak profiles). Go
  supports the two most recent majors → 1.26 and 1.27 are supported today; 1.25 is EOL.
- Constraints: miekg v2 needs 1.27; prometheus client_golang 1.24 needs ≥1.25; quic-go v0.63 `go 1.26.0`;
  CoreDNS `go 1.26.0`; blocky `go 1.26.2`; dnsproxy `go 1.26.8`; Gravity `go 1.26.6`; otter v2 ≥1.24.
- **Recommendation:** `go 1.27` in `go.mod` from day one (toolchain line pinned to latest 1.27.x), CI on
  1.27 + tip; bump to 1.28 after it ships (Feb 2027) when 1.26 goes EOL. Use Go 1.22+ `ServeMux`
  patterns, `log/slog`, `iter`/range-over-func, `unique`, `crypto/mldsa` where useful.

---

## 14. Integration model (Cloudflare split-horizon, UniFi) — stack implications

- Model integrations as **sinks/sources over the store change feed** run only on the leader: an
  `Integration` interface `{Name(); Reconcile(ctx, ZoneSnapshot) error; Watch(ctx) <-chan ExternalEvent}`
  with checkpointing in the store. Cloudflare: `cloudflare/cloudflare-go` v4+ (**UNVERIFIED version**);
  UniFi: Network API (`/proxy/network/api/s/<site>/rest/user|stat/sta`, local API key since Network
  9.x — **UNVERIFIED**), producing client names/groups and static hosts.
- Expose the same shape as an **external-dns webhook provider** (`GET /` capabilities + domain filter,
  `GET/POST /records`, `POST /adjustendpoints`; external-dns v0.23.0, Sep 2026) so k8s Ingress/Service
  records flow into kudurru zones without a bespoke controller.

---

## 15. Version/license table (verified 2026-10-10 unless marked)

| Component | Version | Date | License | Source |
|---|---|---|---|---|
| github.com/miekg/dns | 1.1.73 | last commit 2026-09-01 | BSD-3 | clone `version.go`, LICENSE |
| codeberg.org/miekg/dns | 0.6.118 (Debian 0.6.117) | Oct 2026 | BSD-3 | codeberg.org/miekg/dns, tracker.debian.org |
| coredns/coredns | v1.14.7 | 2026-08-19 | Apache-2.0 | github releases; go.mod `go 1.26.0` |
| 0xERR0R/blocky | v0.35.0 | 2026-09-05 | Apache-2.0 | github releases; go.mod `go 1.26.2` |
| AdguardTeam/dnsproxy | v0.86.0 | 2026-10-01 | Apache-2.0 | github releases; LICENSE in clone |
| AdguardTeam/AdGuardHome | v1.0.0-b.1 (pre) / v0.107.x stable | 2026-09-30 | GPL-3.0 (**from memory, UNVERIFIED this session**) | github releases |
| BeryJu/gravity | 0.33.0 | 2026-05-09 | **UNVERIFIED** (likely MIT) | github releases |
| quic-go/quic-go | v0.63.0 | 2026-09-22 | MIT | pkg.go.dev, clone LICENSE |
| go.etcd.io/etcd server/client v3 | v3.7.2 | 2026 | Apache-2.0 | coredns/gravity go.mod |
| go.etcd.io/bbolt | v1.5.0 | 2026-06-03 | MIT | pkg.go.dev |
| hashicorp/raft | v1.8.0 | 2026-09-16 | MPL-2.0 | pkg.go.dev, LICENSE |
| hashicorp/raft-boltdb/v2 | v2.3.1 | 2025-01-13 | MPL-2.0 | pkg.go.dev |
| lni/dragonboat | v4 pseudo `v4.0.0-20250723...` | 2025-07-23 | Apache-2.0 | pkg.go.dev |
| modernc.org/sqlite | v1.54.0 (SQLite 3.53.3) | 2026-07-15 | BSD-3 | dependabot changelog |
| mattn/go-sqlite3 | v1.14.49 (SQLite 3.53.4) | mid-2026 | MIT | newreleases.io, Debian |
| ncruces/go-sqlite3 | v0.35.3 | 2026 | MIT | upd.dev |
| rqlite | v10.2.0 | 2026-06-01 | MIT | philipotoole.com |
| k3s-io/kine | v1.14.2+ | 2025-09-24 (newer exists) | Apache-2.0 | pkg.go.dev |
| nats-io/nats-server | v2.14.3 | 2026 | Apache-2.0 | releasealert, NVD CVE notes |
| Litestream | v0.5.15 | 2026 | Apache-2.0 | newreleases.io |
| cockroachdb/pebble/v2 | v2.1.3 | 2025-12-25 | BSD-3 | upd.dev |
| knadh/koanf/v2 | v2.3.2 | 2026 | MIT | newreleases.io |
| spf13/cobra | v1.10.2 | Dec 2025 | Apache-2.0 | dependabot |
| alecthomas/kong | v1.16.1 | 2026-08-09 | MIT | pkg.go.dev |
| urfave/cli/v3 | v3.10.1 | 2026 | MIT | newreleases.io |
| go-chi/chi/v5 | v5.3.2 | 2026 | MIT | blocky go.mod |
| danielgtaylor/huma/v2 | v2.38.0 | 2026-05-06 | MIT | pkg.go.dev |
| oapi-codegen/oapi-codegen/v2 | v2.8.0 | 2026-07-17 | Apache-2.0 | pkg.go.dev |
| ogen-go/ogen | v1.23.0 | 2026-07-10 | Apache-2.0 | pkg.go.dev |
| coreos/go-oidc/v3 | v3.17.0 | 2025/26 | Apache-2.0 | releases mirror |
| zitadel/oidc/v3 | v3.48.1 | 2026-08 | Apache-2.0 | tracker.debian.org |
| prometheus/client_golang | v1.24.1 | 2026-07-24 | Apache-2.0 | pkg.go.dev |
| go.opentelemetry.io/otel | v1.45.0 (v1.47.0-rc.1) | 2026-08-03 | Apache-2.0 | github releases |
| dnstap/golang-dnstap | v0.4.0 | — | Apache-2.0 | coredns/blocky go.mod |
| fsnotify/fsnotify | v1.9.0 | — | BSD-3 | miekg v2 go.mod |
| caddyserver/certmagic | v0.25.4 | 2026 | Apache-2.0 | coredns go.mod |
| k8s.io/client-go | v0.37.1 | 2026 | Apache-2.0 | coredns go.mod |
| sigs.k8s.io/controller-runtime | v0.25.1 | 2026 | Apache-2.0 | dependency PRs |
| kubernetes-sigs/external-dns | v0.23.0 | 2026-09-18 | Apache-2.0 | endoflife.date |
| maypok86/otter | v2.x | 2026 | Apache-2.0 | docs |
| hashicorp/golang-lru/v2 | v2.0.7 | — | MPL-2.0 | coredns go.mod |
| duckdb/duckdb-go/v2 | v2.10505.0 (DuckDB 1.5.6) | 2026 | MIT | duckdb.org docs |
| ClickHouse/clickhouse-go/v2 | v2.42–2.46 | 2026 | Apache-2.0 | mirrors |
| goreleaser | v2.18 | 2026-08-23 | MIT | goreleaser.com blog |
| ko | v0.19.1 | 2026-06-29 | Apache-2.0 | pkg.go.dev |
| sigstore/cosign | v3.1.3 | 2026 | Apache-2.0 | newreleases.io |
| Go | 1.27.0 (1.26 supported) | 2026-08-19 | BSD-3 | go.dev/doc/go1.27 |
| React / Vite / TanStack Table / ECharts / uPlot / Lit / Svelte | 19.2.6 / 8.0 / v9 / 6.1.0 / 1.6.32 / 3.3.3 / 5.56.x (SvelteKit 3) | 2026 | MIT/Apache | vendor blogs, package.json |
| Technitium DNS Server | v15 | 2026-04 | GPL-3.0 | github README |
| hickory-dns | 0.26.2 | 2026 | MIT/Apache-2.0 | Fedora updates, github |
| Pi-hole FTL | v6.x | 2026 | EUPL-1.2 (**from memory, UNVERIFIED**) | docs.pi-hole.net |

---

## 16. What kudurru should borrow

1. **miekg/dns v1 server/client/transfer/TSIG primitives** and the discipline of keeping `dns.RR` at the
   edges only (Gravity's `Record{data, ttl, ...}` JSON model).
2. **CoreDNS `transfer.Transferer` interface + `secondary.go` flow** (SOA check over TCP, candidate zone,
   validate, atomic swap, NOTIFY fan-out) and its **`dynupdate` ACL model** (`key × owner × rrtype`,
   commit-before-publish, coalesced NOTIFY, DNSSEC-type rejection until re-sign exists).
3. **CoreDNS `sign` timers/jitter and CDS/CDNSKEY publication; `dnssec` RRSIG cache + singleflight** —
   but extend to NSEC3 and leader-side signing with replicated RRSIGs.
4. **CoreDNS/dnsproxy DoQ and DoH3 server code** (limits: 256 streams, 1024 workers, 200 conns, 0-RTT,
   `h3`/`doq` ALPN, error codes) and **dnsproxy `upstream`** for all upstream protocols.
5. **PROXY protocol support** (CoreDNS `pkg/proxyproto`, dnsproxy `TrustedProxies`) so dnsdist can sit in
   front without losing client identity.
6. **Gravity's role model** (`roles.Role{Start,Stop}`, event bus, per-role API registration, memory
   handler that keeps serving when the store is unreachable, join-token cluster bootstrap, cluster-node
   log fan-out, OIDC + API tokens + permissions list).
7. **blocky's resolver chain decomposition** (blocking, caching w/ prefetch, conditional, custom DNS,
   hosts file, client names, ECS, EDE, DNS64, rebinding, rate limit, sudn, DNSSEC validation), its
   streaming list parsers with error tolerance, `stats.Collector` buckets, query-log writer plug-ins
   (SQLite/MySQL/Postgres/CSV/console/dnstap), Redis shared cache, oapi-codegen + chi API, cobra CLI with
   `blocking enable/disable`, `cache flush`, `lists refresh`, `query`, `validate`, `healthcheck`.
8. **Pi-hole FTL's 24 h in-RAM window + 10-minute overTime buckets + 60 s batched SQLite flush +
   `MAXDBDAYS` retention**, and its dashboard information architecture.
9. **rqlite's single-voter-bootstrap raft** so `docker run` and 3-node k8s share one code path.
10. **external-dns webhook provider shape** as the integration contract (Cloudflare/UniFi later).
11. **goreleaser `kos:` + cosign keyless + distroless static** release pipeline; cgo-free everything.

## 17. What kudurru should avoid

1. **Embedding etcd by default** (binary/dependency bloat, fsync-sensitive, 1 GB RAM floor, bespoke join,
   2-node anti-pattern) — keep it as an optional/legacy store at most.
2. **dragonboat** (no v4 release since 2022), **marmot** (LWW for authoritative data), **Litestream as
   replication** (it is backup/read-replica).
3. **Using CRDs as the primary datastore** (object-size limits, apiserver/etcd pressure, no home for
   query logs, forces a second store outside k8s). Use CRDs as declarative input.
4. **Independent per-replica DNSSEC signing** (ECDSA non-determinism → divergent RRSIGs, IXFR chaos).
5. **NSEC black lies as the only denial mode** (CoreDNS limitation; breaks zone walking expectations,
   NXDOMAIN becomes NODATA, no NSEC3) — fine as an online option, not the default for primaries.
6. **cgo dependencies in the core binary** (mattn sqlite, DuckDB) — they break distroless/ko/cross builds.
7. **logrus/zap/OpenTracing** (blocky/Gravity/CoreDNS legacies) — use slog + OTel.
8. **Copying Technitium code** (GPL-3.0 vs Kudurru Community License; Kudurru used Apache-2.0 when this note was written) — borrow features and UX only. AdGuard Home is
   GPL-3.0 too (**UNVERIFIED**); dnsproxy is Apache-2.0 and safe.
9. **Fixed DNS ID / port reuse when bridging DoQ→UDP** (CVE-2026-47703 class).
10. **Starting on miekg v2 for the whole codebase today** unless you accept API churn and Go 1.27 and
    give up copy-paste reuse from CoreDNS/blocky/dnsproxy; plan the migration instead.

## 18. Open questions for the architecture doc

- bbolt vs SQLite as the raft FSM engine (bbolt = simplest/fastest KV; SQLite = queryable, one engine for
  state + query log). Prototype both behind `Engine` and benchmark a 100k-record zone load + UI pagination.
- Should followers accept RFC 2136/API writes by forwarding to the leader (rqlite style `-join` + forward)
  or return a redirect? (Recommend transparent forward.)
- Read-your-writes for the UI after `Apply` on a follower: wait for apply index (raft `AppliedIndex`) or
  always route UI writes+reads to leader?
- miekg v2 timeline: when CoreDNS migrates, kudurru should too — decide the abstraction boundary now.
- Online (per-query) DNSSEC signing for dynamic/large zones vs precomputed chains: likely precomputed
  with incremental NSEC3 updates; confirm NSEC3 incremental-update complexity.
- Query log at scale in k8s: per-node SQLite with API fan-out vs mandatory central sink.
- Does proxy.golang.org serve codeberg.org/miekg/dns (UNVERIFIED) — matters for reproducible builds.
