# Spike: dnsserver-quic (DoT, DoH, DoQ, DoH3 listeners and authenticated PROXYv2)

Spike (i) from [architecture §14](../architecture.md#14-risks--mitigations), plus the half of spike (b) that does not need dnsdist: the authenticated PROXYv2 parser. Relevant design: §3.3 (`internal/dnsserver`), §5.2 (ports), §6.2 (listener config), D45, D56, D67.

**Verdict: pass.** All four TLS transports answer correctly through the shared pipeline, checked with an independent client (dnsproxy `upstream`) and with stock tools (dig, curl, dnsproxy CLI). Every PROXYv2 forgery case is dropped and counted on UDP, TCP and DoT. The binary-size delta is +996 KiB (+8.6 %), of which quic-go accounts for 448 KiB.

| Gate | Result |
|---|---|
| DoT, DoH (h2 and http/1.1), DoQ and DoH3 answer correctly | **pass**: `TestTransportsEndToEnd` (dnsproxy client), `spikecmd -selfcheck`, and dig/curl/dnsproxy-CLI smoke tests below |
| PROXYv2 forgery cases all dropped | **pass**: wrong TLV, missing TLV, disallowed peer, unlisted loopback and malformed headers are dropped and counted on UDP, TCP and DoT (`TestProxyProtocolEndToEnd`: 15 of 15). The parser has a unit-level matrix and three fuzz targets |
| Binary-size delta reported | **pass**: +1,019,904 bytes in total; quic-go/http3/qpack are +458,752 of that (table below) |

Environment for every number here: Linux amd64, 8 vCPU (Go reports `GOMAXPROCS 6` because of the cgroup quota), Go 1.27.2, quic-go v0.63.0, miekg/dns v1.1.73, dnsproxy v0.86.0. Client and server ran on loopback in the same VM, so the figures compare transports with each other; they are not capacity claims.

## What was built

All new code sits under `internal/dnsserver`, plus the config keys (`internal/config`, which the brief allowed).

| Package / file | What it does |
|---|---|
| `dnsserver/listener.go` | `Protocol` (udp, tcp, dot, doh, doq, doh3), `Spec`, the `Listener` interface, `Limits`, and the **Registry**. The registry builds listeners from a protocol-keyed factory table, binds all of them or none, runs them, and reports readiness per listener (`Ready() (bool, []string)`) for `/healthz/ready` |
| `dnsserver/handler.go` | `Handler.Resolve`, the single entry into `pipeline.Chain` for every transport. It sets `Client.Proto`, `Client.Addr` and `ProxiedFrom`. `resolveFreshID` gives fixed-ID transports a random ID inside the pipeline |
| `dnsserver/dns.go` | Do53 UDP and TCP, and DoT (`tcp-tls`, ALPN `dot`), on miekg/dns. Wraps the sockets for PROXYv2 when a policy is enabled |
| `dnsserver/doh.go` | RFC 8484 handler (GET `?dns=` base64url, POST `application/dns-message`, `Cache-Control: max-age=<min TTL>`), served by the DoH listener on net/http over HTTP/1.1 and HTTP/2 |
| `dnsserver/doq.go` | RFC 9250 on quic-go: one stream per query, 2-byte prefix, message ID 0, STREAM FIN required, `DOQ_PROTOCOL_ERROR` / `DOQ_NO_ERROR` / `DOQ_EXCESSIVE_LOAD`, 256 streams per connection, 1024 stream workers shared per listener, idle timeout |
| `dnsserver/doh3.go` | DoH over HTTP/3 (quic-go/http3, ALPN `h3`). Shares the RFC 8484 handler and has an optional connection cap |
| `dnsserver/config.go` | `SpecsFromConfig(config.Listeners)` and `PolicyFromConfig`, which wire §6.2 into the registry |
| `dnsserver/server.go` | The existing Do53-only `Server` API, unchanged for `cmd/kudurru` but now running the same listeners. It gains a `Proxy` field |
| `dnsserver/tlsreload` | The **one certificate-reload path** (D67). A `Reloader` per cert/key pair keeps the certificate behind an atomic pointer, and `tls.Config.GetCertificate` reads it. A `Store` deduplicates pairs. Changes are picked up by fsnotify on the parent directories (this also catches the Kubernetes `..data` symlink swap), with an mtime/size poll as fallback. A half-rotated pair keeps serving the previous certificate |
| `dnsserver/proxyproto` | Clean-room PROXYv2 parser (`Parse`, `Append`, TLVs), the `Policy` (exact-host allow-list plus the TLV `0xE0` secret, the loopback exemption, drop counters and a hook), and the `PacketConn` / `Listener` wrappers |
| `dnsserver/spikecmd` (tag `spike`) | Runs all six listeners for operator smoke tests and size probes. Not part of `kudurru` |
| `config` | New keys: `listeners.dot`, `listeners.doh`, `listeners.doq`, `listeners.doh3` and `listeners.proxyProtocolSecretFile`, all `hostOnly` and `restartRequired`. `Validate` checks them and enforces D56: a non-loopback peer requires the secret file. Existing keys are unchanged and `TestSchemaMatchesConfig` stays green |

`cmd/kudurru` is untouched, as the brief required. Wiring the registry into `serve` takes four lines: `SpecsFromConfig`, `PolicyFromConfig`, `NewRegistry`, and `Run` as a runnable feeding `checker`. That is a follow-up for whoever owns `cmd/`.

### Decisions taken in the spike (please confirm or record)

1. **PROXYv2 headers are optional, never trusted by default.** A datagram or connection without the signature is an ordinary query attributed to its transport peer, even from an allowed peer. That keeps the loopback readiness self-query and `dig @127.0.0.1` working. A header is honoured only if the peer is listed **and** the header is authenticated. Any other header is dropped and counted. PowerDNS's `proxy-protocol-from` instead *requires* a header from listed peers. Requiring it adds no security, because a header-less query can only claim the peer's own address.
2. **"Loopback" means exactly `127.0.0.1` and `::1`**, as D56 lists them. `127.0.0.2` is treated as an ordinary peer: it needs the secret, which is also what makes the rule testable on one host. Loopback must still appear in `proxyProtocolFrom`. The exemption only waives the TLV, and only while no secret is set.
3. **Exact hosts only.** `NewPolicy` refuses `10.0.0.0/24`, `fd00::/64` and anything else wider than /32 or /128 (D45/D56). See the surprise about §5.2 below.
4. **Secret format.** The TLV value is compared in constant time with the secret file's bytes, after trailing CR/LF are trimmed. The secret must be at least 32 bytes. `openssl rand -hex 32` (64 printable bytes, 32 bytes of entropy) is the recommended form, because Helm `randAlphaNum` and dnsdist's string parameter then carry exactly the same bytes, with no binary-escaping question.
5. **Drop reasons** (the bounded label values for `kudurru_dns_dropped_total{reason=…}`):
   - `proxy_unauthenticated`: the peer is allowed but the TLV is missing or wrong.
   - `proxy_untrusted_peer`: the peer is not listed.
   - `proxy_malformed`: the header is truncated, out of spec, longer than 2 KiB, or uses AF_UNIX.

   `LOCAL` commands and `AF_UNSPEC` headers still need authentication, and then keep the peer address.
6. **PROXYv2 applies to the udp, tcp and dot listeners.** DoT costs nothing extra because the header precedes TLS on the same TCP wrapper. DoH, DoQ and DoH3 do not parse headers, because dnsdist talks Do53 to backends. With no `proxyProtocolFrom` configured nothing is inspected, so the miekg `*net.UDPConn` fast path is kept.
7. **Fresh DNS IDs.** DoQ queries (ID 0 by RFC 9250) and DoH/DoH3 queries (ID 0 recommended by RFC 8484) enter the pipeline with a crypto-random ID. The client's ID is restored on the response, so no stage can carry the client's fixed ID onto an upstream wire (the CVE-2026-47703 class). The upstream stage also randomises IDs. `TestDoQFreshIDsPerQuery` sends 32 queries and checks that the upstream saw at least 28 distinct IDs, none of them 0.
8. **DoH body cap is 65,535 bytes, not 2 MiB.** No DNS message can be larger than 65,535 bytes, so a 2 MiB cap would buffer 32 times more per in-flight request for no benefit. A larger body gets 413, an oversized `?dns=` gets 414, headers are capped at 16 KiB, and all of these are configurable through `Limits`. This deviates from the brief; please confirm.
9. **DoH3 reports `pipeline.ProtoDoH`**, because `pipeline.Proto` has no HTTP/3 value and `internal/pipeline` was out of scope. The registry key and readiness name are still `doh3`. See the recommendation below.
10. **QUIC limits** follow CoreDNS: 256 bidirectional streams per connection, 1024 DoQ stream workers per listener, 30 s idle timeout, 0-RTT allowed (queries are replay-safe, and the ingress stage refuses non-QUERY opcodes). DoQ grants **no** unidirectional streams. `Limits.QUICMaxConns` is implemented (excess connections get `DOQ_EXCESSIVE_LOAD` or `H3_EXCESSIVE_LOAD`) but defaults to **unlimited**, whereas CoreDNS defaults to 200. A config knob should expose it.
11. **DoQ error policy.** Every RFC 9250 §4.3.3 violation closes the connection with `DOQ_PROTOCOL_ERROR`, as CoreDNS and dnsproxy do: zero-length prefix (draft framing), FIN before the declared length, data after the query, no FIN before the 2 s read deadline, non-zero ID, `edns-tcp-keepalive`, or an unpackable message.

## Measurements

### 1. Binary size (`CGO_ENABLED=0 -trimpath -ldflags '-s -w'`, as in the Makefile)

```sh
git archive --format=tar -o /tmp/kudurru-base.tar 79a0e37   # the scaffold this spike started from
git archive --format=tar -o /tmp/kudurru-head.tar HEAD
# extract both; in a third copy of HEAD delete internal/dnsserver/doq.go and doh3.go and the
# ProtoDoQ/ProtoDoH3 lines of the factories table ("noquic")
CGO_ENABLED=0 go -C /tmp/kudurru-base   build -trimpath -ldflags '-s -w' -o /tmp/ksize/kudurru-base   ./cmd/kudurru
CGO_ENABLED=0 go -C /tmp/kudurru-noquic build -trimpath -ldflags '-s -w' -o /tmp/ksize/kudurru-noquic ./cmd/kudurru
CGO_ENABLED=0 go -C /tmp/kudurru-head   build -trimpath -ldflags '-s -w' -o /tmp/ksize/kudurru-head   ./cmd/kudurru
CGO_ENABLED=0 go -C /tmp/kudurru-head   build -trimpath -ldflags '-s -w' -tags spike          -o /tmp/ksize/spikecmd          ./internal/dnsserver/spikecmd
CGO_ENABLED=0 go -C /tmp/kudurru-head   build -trimpath -ldflags '-s -w' -tags spike,dnsproxy -o /tmp/ksize/spikecmd-dnsproxy ./internal/dnsserver/spikecmd
```

| Binary | Bytes | Delta |
|---|---:|---:|
| `kudurru` at 79a0e37 (scaffold: Do53 only) | 11,813,024 | |
| `kudurru`, this branch without DoQ/DoH3 (DoT, DoH, PROXYv2, tlsreload, fsnotify) | 12,374,176 | +561,152 (+548 KiB) |
| `kudurru`, this branch (all six transports) | 12,832,928 | **+1,019,904 (+996 KiB, +8.6 %)** |
| of which quic-go + quic-go/http3 + qpack + DoQ/DoH3 code | | **+458,752 (+448 KiB)** |
| `spikecmd` (all listeners, no prometheus/cobra) | 10,039,456 | |
| `spikecmd` + `dnsproxy/upstream` client linked | 11,161,760 | **+1,122,304 (+1.07 MiB)** for dnsproxy on top of quic-go |

`cmd/kudurru` is unchanged, yet it already carries everything. The factory table is initialised at package init, so the linker keeps every listener once `internal/dnsserver` is imported. For the non-QUIC +548 KiB, a `go tool nm -size` diff of unstripped builds points mostly at the stdlib TLS **server** handshake (crypto/tls +98 KB of symbols), ECDSA/RSA signing, x509 private-key parsing, golang.org/x/crypto and x/text, and the HTTP/2 server. The scaffold linked net/http but never served TLS, so the linker had dropped those parts.

`dnsproxy/upstream` is a **test-only** dependency today, so it adds 0 bytes to `kudurru`. It brings five modules into the build list: `AdguardTeam/dnscrypt`, `AdguardTeam/golibs` and `ameshkov/dnsstamps` (all Unlicense), `robfig/cron/v3` (MIT) and `golang.org/x/exp`. None is GPL or EUPL. Its go.mod pins quic-go v0.60.0; MVS selects v0.63.0 and dnsproxy builds and passes against it. The weight is reasonable, and §2 already plans to link it for `internal/upstream`, which will cost the +1.07 MiB measured above.

### 2. Handshake + first query on loopback

Each sample opens a new connection with a fresh `tls.Config` (full handshake, no resumption), sends one A query, then times a second query on the same connection. The pipeline is a local-answer stage, so transport cost dominates. n = 200 per protocol; the table shows the median of three runs, with the range in brackets.

```sh
KUDURRU_SPIKE_MEASURE=1 go test -run 'TestSpikeMeasure$' -count=1 -v ./internal/dnsserver/
```

| Protocol | First query p50 (ms) | First query p99 (ms) | Second query, same conn, p50 (ms) |
|---|---:|---:|---:|
| udp | 0.050 [0.042–0.054] | 0.093 [0.083–0.105] | 0.027 |
| tcp | 0.085 [0.079–0.087] | 0.151 [0.132–0.176] | 0.030 |
| dot (TLS 1.3) | 0.901 [0.876–0.915] | 1.463 [1.457–1.569] | 0.036 |
| doh (h2) | 1.037 [1.020–1.122] | 1.569 [1.490–1.803] | 0.129 |
| doq | 1.258 [1.208–1.284] | 1.952 [1.769–2.024] | 0.091 |
| doh3 | 1.467 [1.333–1.574] | 2.322 [1.932–10.649] | 0.177 |

The TLS transports cost about 1 ms of CPU-bound handshake on loopback (ECDSA P-256, X25519). After the handshake, DoT is as cheap as TCP, and DoQ is cheaper per query than either HTTP transport. One DoH3 run had a 10.6 ms p99 outlier. It is a scheduling artefact of sharing CPUs with the client; the other two runs measured 1.9 and 2.3 ms.

### 3. Queries per second, 4 connections, 5 s per run

Same harness and local-answer stage. "x 1" means each connection has one query in flight (closed loop). "x 16" means 16 concurrent queries per connection, which only the multiplexing transports support (miekg's client cannot pipeline). The table shows the median of three runs, with the range in brackets. There were zero errors in every run after the `MaxTCPQueries` fix (see Surprises).

| Protocol | 4 conns x 1 in flight (q/s) | 4 conns x 16 in flight (q/s) |
|---|---:|---:|
| udp | 111,143 [97,920–133,445] | n/a |
| tcp | 99,557 [97,232–100,527] | n/a |
| dot | 98,534 [96,842–109,076] | n/a |
| doh (h2) | 36,886 [36,213–38,210] | 50,595 [42,109–50,933] |
| doq | 46,064 [42,080–48,160] | **100,064** [82,633–112,710] |
| doh3 | 21,533 [21,219–24,528] | 40,511 [27,466–45,910] |

Even with client and server sharing six cores, every transport is far above what a home or office network generates. DoQ with multiplexing roughly matches Do53/TCP. DoH3 is the most expensive transport per query (HTTP/3 framing and QPACK on top of QUIC; not profiled further). That is acceptable.

### 4. Goroutines at 256 concurrent DoQ streams

The DoQ-only server runs in a child process, so client goroutines are not counted. A gate stage parks every query until the target count has arrived; the server then samples `runtime.NumGoroutine()` and its heap.

```sh
KUDURRU_SPIKE_MEASURE=1 go test -run 'TestSpikeDoQGoroutines$' -count=1 -v ./internal/dnsserver/
```

```
DoQ server, 1 conn(s) x 256 streams: goroutines before any connection 11, with 1 idle connection(s) 14, at 256 in-flight queries: goroutines 270, heap in use 3.1 MiB; failed exchanges 0
DoQ server, 4 conn(s) x 256 streams: goroutines before any connection 11, with 4 idle connection(s) 23, at 1024 in-flight queries: goroutines 1047, heap in use 6.5 MiB; failed exchanges 0
```

Each idle QUIC connection costs 3 goroutines: two inside quic-go plus our stream acceptor. Each in-flight stream costs exactly one goroutine (the worker), so 256 streams give 270 goroutines and about 3 MiB of heap. The 1024-worker cap bounds the total at about 1050 goroutines per listener whatever the connection count. Further streams wait in QUIC flow control: `TestDoQStreamAndWorkerLimits` holds 4 workers against 40 streams, and checks that stream 257 on one connection gets `StreamLimitReachedError`.

### 5. PROXYv2 parser cost and fuzzing

```sh
go test -run XXX -bench . -benchtime 2s ./internal/dnsserver/proxyproto/
BenchmarkParse-6      	138267898	        17.31 ns/op	       0 B/op	       0 allocs/op
BenchmarkDatagram-6   	26900391	        89.37 ns/op	       0 B/op	       0 allocs/op
```

`Datagram` covers the full per-packet work: signature check, peer lookup, parse and constant-time comparison of a 64-byte secret. It adds 89 ns and no allocations to each proxied UDP query.

Each fuzz target ran for 60 s, seeded with the golden header, IPv4/IPv6/LOCAL/UNIX headers and plain DNS bytes:

```sh
go test -run XXX -fuzz '^FuzzParse$'    -fuzztime 60s ./internal/dnsserver/proxyproto/
go test -run XXX -fuzz '^FuzzDatagram$' -fuzztime 60s ./internal/dnsserver/proxyproto/
go test -run XXX -fuzz '^FuzzStream$'   -fuzztime 60s ./internal/dnsserver/proxyproto/
```

| Target | Property checked | Executions | Result |
|---|---|---:|---|
| `FuzzParse` | no panic; consumed ≤ input and ≤ 2 KiB; every accepted header survives `Append` → `Parse` unchanged | 13,980,667 (228k/s) | pass, no crashers |
| `FuzzDatagram` | no panic; a header-less datagram passes through untouched; a header is accepted only from the listed peer and only with the exact secret | 11,546,205 (189k/s) | pass, no crashers |
| `FuzzStream` | no panic with 1-byte reads; header-less streams are replayed byte for byte | 12,290,155 (220k/s) | pass, no crashers |

### 6. Test suite

```sh
go test -race -count=1 ./internal/dnsserver/... ./internal/config/   # all green, about 4 s for dnsserver
golangci-lint run ./...                                              # 0 issues (also with --build-tags spike,dnsproxy)
CGO_ENABLED=0 go build ./... && go vet ./...                         # clean
```

Notable tests:
- `TestTransportsEndToEnd`: all six transports through ingress → recorder → upstream to a fake server, using dnsproxy `upstream` as the client.
- `TestDoQProtocolErrors`: seven RFC 9250 violations, wrong ALPN, and unidirectional streams.
- `TestDoHHTTPSemantics`: twelve status-code cases, h1.1 and h2, and the DoH3 path.
- `TestSharedCertificateReload`: rotates the certificate behind DoT, DoH, DoQ and DoH3, and checks that new handshakes see it without a restart.
- `TestKubernetesSecretSwap`: the `..data` symlink swap.
- `TestProxyProtocolEndToEnd`: the forgery matrix on real UDP, TCP and DoT sockets, using 127.0.0.2/127.0.0.3 as peers.
- `TestLongLivedStreamConnections`, `TestUDPLargeQueryNotTruncated`.

## Operator commands

Start every listener with a generated certificate. `spikecmd` prints the certificate path; pass `-cert/-key` for your own pair, and `-upstream 1.1.1.1:53` to forward instead of answering `A 192.0.2.1`:

```sh
go run -tags spike ./internal/dnsserver/spikecmd   # udp/tcp :5300, dot :8853, doh :8443, doq :8853/udp, doh3 :8443/udp
go run -tags spike,dnsproxy ./internal/dnsserver/spikecmd -selfcheck   # queries all six through dnsproxy/upstream and exits
```

Every command below was run against `spikecmd` (script `/tmp/kspike/smoke.sh` during the spike) and returned `192.0.2.1`, except the kdig lines. kdig is not installed in the spike VM; those lines use the documented Knot 3.3+ syntax and are untested here.

| Transport | dig 9.18 (run) | kdig (Knot ≥ 3.3, not run here) | dnsproxy / curl (run) |
|---|---|---|---|
| Do53 | `dig @127.0.0.1 -p 5300 example.com` (+`+tcp`) | `kdig @127.0.0.1 -p 5300 example.com` | `dnsproxy -l 127.0.0.1 -p 5400 -u udp://127.0.0.1:5300` |
| DoT | `dig +tls +tls-ca=cert.pem +tls-hostname=localhost @127.0.0.1 -p 8853 example.com` | `kdig +tls +tls-ca=cert.pem +tls-hostname=localhost @127.0.0.1 -p 8853 example.com` | `dnsproxy … -u tls://127.0.0.1:8853 --insecure` |
| DoH (h2) | `dig +https=/dns-query +tls-ca=cert.pem +tls-hostname=localhost @127.0.0.1 -p 8443 example.com` (and `+https-get`) | `kdig +https=/dns-query +tls-ca=cert.pem +tls-hostname=localhost @127.0.0.1 -p 8443 example.com` | `curl --cacert cert.pem --resolve localhost:8443:127.0.0.1 -D - 'https://localhost:8443/dns-query?dns=AAABAAABAAAAAAAAB2V4YW1wbGUDY29tAAABAAE'` → `HTTP/2 200`, `content-type: application/dns-message`, `cache-control: max-age=60` |
| DoQ | not supported by dig | `kdig +quic +tls-ca=cert.pem +tls-hostname=localhost @127.0.0.1 -p 8853 example.com` | `dnsproxy -l 127.0.0.1 -p 5401 -u quic://127.0.0.1:8853 --insecure` then `dig @127.0.0.1 -p 5401 example.com` |
| DoH3 | not supported by dig | not supported by kdig (HTTP/2 only) | `dnsproxy -l 127.0.0.1 -p 5402 -u h3://127.0.0.1:8443/dns-query --insecure` then `dig @127.0.0.1 -p 5402 example.com`; or `curl --http3-only` with an HTTP/3-enabled curl (the VM's curl 8.17 lacks HTTP/3) |

Running PROXYv2 against dnsdist (untested here, because the VM has no dnsdist binary):

```sh
openssl rand -hex 32 > /run/secrets/proxy
go run -tags spike ./internal/dnsserver/spikecmd -dns 127.0.0.1:5353 -proxy-from 127.0.0.1 -proxy-secret-file /run/secrets/proxy
# dnsdist.conf:
#   newServer({address="127.0.0.1:5353", useProxyProtocol=true})
#   addAction(AllRule(), SetAdditionalProxyProtocolValueAction(0xE0, "<contents of /run/secrets/proxy>"))
```

For the metrics owner: `Registry.Proxy()` returns the `*proxyproto.Policy`. Register `kudurru_dns_dropped_total{reason}` either as a `CounterFunc` per `proxyproto.Reasons` entry reading `policy.Dropped(reason)` (lock-free atomics), or by incrementing a `CounterVec` from `policy.SetDropHook`.

## Surprises

1. **The worktree branch started at the initial commit (5a893a4)**, not at the scaffold (79a0e37) that the other spike worktrees use. I fast-forwarded the branch to 79a0e37 before starting. No rebase was needed.
2. **miekg/dns closes TCP and DoT connections after 128 queries** (`MaxTCPQueries` default). The first throughput run showed DoT and TCP stalling at about 100 q/s with millions of errors. The fix (commit `c13bb60`) defaults to unlimited, as CoreDNS does (`tcpMaxQueries = -1`), bounded by the 10 s idle timeout. The scaffold's `Server` had the same latent bug.
3. **miekg/dns reads UDP into a 512-byte buffer by default** (`UDPSize`). An EDNS query larger than 512 bytes was silently truncated and dropped, and a PROXYv2 header (16 + 12 + 67 bytes with the TLV) eats into that budget. The UDP listeners now use 4096 bytes, plus `proxyproto.MaxHeaderLen` when PROXYv2 is on. `TestUDPLargeQueryNotTruncated` fails at the old setting.
4. **§5.2 contradicts D56.** It says `--proxy-protocol-from=<edge pod CIDR>`, while D56 and this implementation accept exact /32s only. In Kubernetes, edge pods have dynamic IPs, so a hostOnly, restart-required list of exact peers cannot track them.
5. **Linking is all-or-nothing.** Because the factory table is a package-level map, `kudurru` carries quic-go (448 KiB) even before `cmd/` wires DoQ. That is fine under D67, but nothing can be dropped from a "small" build without a build tag.
6. **dnsproxy's go.mod pins quic-go v0.60.0** while CoreDNS and blocky pin v0.63.0. MVS picks v0.63.0 and dnsproxy compiles and passes against it. This needs re-checking on every quic-go bump, because quic-go is pre-1.0 and changes its API between minor versions.
7. **Wrapping the UDP socket for PROXYv2 loses miekg's `*net.UDPConn` OOB path**, which picks the reply source address on multi-homed hosts bound to `0.0.0.0`. That is harmless for dnsdist on loopback or in-pod, but a listener on a multi-homed host with PROXYv2 enabled should bind a specific address. The wrapper is only installed when `proxyProtocolFrom` is non-empty.
8. **Go's TLS server refuses mismatched ALPN.** A DoT client offering only `h2` gets `no_application_protocol`. Clients offering no ALPN, or `dot`, work. This is the correct behaviour for DoT, and the operator docs should mention it.

## Recommended changes to docs/architecture.md (not edited here)

1. **§5.2 / D56:** resolve the pod-CIDR contradiction. Either (a) allow CIDR entries in `proxyProtocolFrom` **only when `proxyProtocolSecretFile` is set**, so the TLV carries the authentication and the CIDR only narrows exposure, or (b) keep exact /32s and say how Kubernetes supplies them. The spike recommends (a). It is a one-line change in `proxyproto.parseHost`, and D45's rationale (CIDR trust *alone* is forgeable) still holds.
2. **§3.3:** record decisions 1, 2, 4 and 5 above: optional headers, loopback meaning exactly 127.0.0.1 and ::1, secret bytes equal to the file contents, and the three bounded drop reasons.
3. **§3.3 / `pipeline.Proto`:** add `ProtoDoH3` (label `doh3`), so metrics and the query log can tell HTTP/3 from HTTP/2. Until then DoH3 reports `doh`.
4. **§3.3 limits:** record `MaxTCPQueries` unlimited with a 10 s idle timeout, the UDP read buffer of 4096 + PROXYv2 headroom, the DoH body cap of 65,535 bytes rather than 2 MiB, and the QUIC connection cap (implemented, default unlimited, needs a config key).
5. **§11.1:** add `kudurru_dns_dropped_total{reason}` with the reason values listed above.
6. **§6.2:** mention that the dot, doh, doq and doh3 listeners share one reloader per cert/key pair, with fsnotify plus a 30 s poll fallback, and that a half-rotated pair keeps the old certificate.
