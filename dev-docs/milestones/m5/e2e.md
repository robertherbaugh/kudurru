# M5 work package `e2e`: interoperability, end-to-end tests, fuzzing

Branch `worktree-wf_b932d6b4-504-2`, based on `347f4c7`. Covers
dev-docs/design-record.md §13.1 M5 ("e2e (BIND secondary, dex, kind,
apiserver-outage, 2-VM VRRP), fuzz corpus") and §12 (`make e2e`) as far as
one Linux host allows: no Docker, no kind, no second VM. Production code is
unchanged. Every bug the tests exposed has a failing test, is skipped with
the bug id, and has a row in [followups.md](../../followups.md#end-to-end-and-fuzzing-docsm5e2emd).

Files: `internal/testutil/kudurrutest` (new), `test/e2e`, `test/interop`,
`test/fuzz` (new), `hack/fuzz.sh` (new), `Makefile` (`e2e`, `interop`,
`fuzz`), `.github/workflows/ci.yml` (the `e2e` job replaces `auth-e2e`, plus
`fuzz-targets` and the `fuzz` matrix), `dev-docs/followups.md`. No shared file
(serve.go, design-record.md, internal/config) was touched.

## How it runs

- **`kudurrutest`**, the harness:
  - `MainBuild` (TestMain) builds `./cmd/kudurru` once per package into a
    temp dir, with `CGO_ENABLED=0` as shipped. `KUDURRU_E2E_BINARY`
    substitutes a prebuilt binary.
  - `NewNodeSpec` allocates free loopback ports (DNS udp+tcp on one port,
    API, ops, metrics, internal) and a data dir, and writes the base
    config: sqlite, plain-HTTP API, one socket per listener, the admin
    socket inside the data dir, and the default upstream group on the
    discard port so nothing leaves the host.
  - `Start` / `StartReady` run the binary and poll `/healthz/ready` every
    10 ms. They strip the caller's `KUDURRU_*` variables and dump the node
    log when a test fails.
  - `StartUpstream` is a fake resolver with per-name delays.
  - `KnownBug(t, id, summary)` skips with the bug id unless
    `KUDURRU_E2E_KNOWN_BUGS=1`.
- **Opt-in.** The e2e and interop tests skip unless `KUDURRU_E2E=1`.
  `make e2e` and `make interop` set it, so `make test` stays as fast as
  before; it still replays every fuzz seed corpus.
- **`make e2e`** runs `go test -race -v ./test/e2e/ ./test/interop/`.
  **`make interop`** runs only the BIND tests. Both skip cleanly without
  `named`; `KUDURRU_NAMED` points at a non-standard binary.
- **`make fuzz`** runs `hack/fuzz.sh` (`FUZZTIME` default 60s,
  `FUZZ_JOBS`, `FUZZ_PARALLEL`). The script has four modes:
  - `--list`: the target list (one place, also the CI matrix).
  - `--one <pkg> <target>`: a single target, for CI.
  - `<regex>`: a subset of targets.
  - `--save-corpus N <pkg> <target>`: copies the N smallest inputs the
    fuzzer found into the package's `testdata/fuzz/`.
- **CI**:
  - The `e2e` job installs `bind9` and stops its service. It unloads the
    `usr.sbin.named` AppArmor profile, which would deny a named reading its
    config from `$TMPDIR`. It then runs `make e2e` and `make
    test-auth-e2e` (dex, cached under `bin/`).
  - `fuzz-targets` emits `hack/fuzz.sh --list` as JSON.
  - `fuzz` is a `fail-fast: false` matrix that runs each target for 60 s in
    parallel and uploads `testdata/fuzz/` and the logs on failure.

## Pass/fail matrix

Measured on this sandbox (8 shared CPUs, 3 other agents running), BIND
9.18.50, Go 1.27.2. `make e2e` builds the binary in 2.3-3.2 s.

| Test | Result | Time / measurement |
|---|---|---|
| `TestBINDSecondary/AXFR` | pass | named loaded the zone 0.11-0.76 s after start; zone identical to kudurru's AXFR; `dig @named` sees the records |
| `TestBINDSecondary/NOTIFY` | pass | file change → kudurru serial +1 in 0.81-0.84 s → named at the new serial 0.84-1.01 s after the change (named's refresh timer is ≥ 300 s) |
| `TestBINDSecondary/TSIGRefused` | pass | unsigned: REFUSED; wrong secret, unknown key: bad authentication |
| `TestBINDSecondary/ZoneTransferDeny` | pass | REFUSED with a valid key; named answers SERVFAIL for the zone |
| `TestBINDUpstream/ForwardZone` | pass | A (UDP), CNAME chain (TCP), NXDOMAIN through a forward DNSZone to named |
| `TestBINDUpstream/Conditional` | **skip, E2E-1** | SERVFAIL: `upstreams.conditional` not consulted |
| `TestBINDUpstream/DefaultGroupNotNamed` | pass | |
| `TestTwoNodes` (6 subtests) | pass | each node ready in 0.12-0.60 s; 108-byte list pulled and verified in 0.7-0.9 ms |
| `TestSIGTERMLameduck/InFlightQueriesComplete` | pass | 2 in-flight queries answered 1.50 s after SIGTERM; exit 0 after 1.51-1.53 s |
| `TestSIGTERMLameduck/ReadinessFlipsBeforeListenersClose` | **skip, E2E-3** | 503 seen, but the window before the ops listener closes is 1.7-13 ms |
| `TestRestartKeepsState` (5 subtests) | pass, `NoFirstBoot` **skip, E2E-4** | restart ready in 55-242 ms; serial, cache (1 entry) and query-log ids continuous |
| `TestConfigValidationFails` (12 cases) | pass | each exits 1 in 10-380 ms with `error:` naming the key |
| `TestPortConflictsFailFast` (7 cases) | pass | exit 1 in 66-550 ms naming the address |
| `TestReadyMeansServing` | **skip, E2E-5** | first query after readiness SERVFAIL on 4/4 restarts |
| `make test-auth-e2e` (dex v2.46.0 + mock OP) | pass | 17 dex subtests + mock OP, 12 s including the cached dex build |

Wall clock: `make e2e` 28 s (two binary builds of 12 s each under load,
2.3-3.2 s when idle); `KUDURRU_E2E_KNOWN_BUGS=1 make e2e` fails exactly
the five skipped tests above.

## Bugs found

| Id | Severity | What | Test (skip marker) | Owner |
|---|---|---|---|---|
| E2E-1 | high | `upstreams.conditional` never takes effect. On first boot, bootstrap seeds a root forward zone, and the authoritative stage sets `UpstreamGroup=default` for every forwarded name. `upstream.Set.Route` returns any non-empty hint before it consults the suffix map. Every conditional rule (including dnsmasq imports) is ignored and the names go to the default group. The forward-zone path (`DNSZone type: forward`, `forward.upstreamGroup`) works. | `TestBINDUpstream/Conditional` | upstream / authoritative |
| E2E-3 | medium | No lameduck window. serve.go wires `checker.Lameduck` but no `LameduckDuration`, and no config key exists, so readiness is 503 for only 1.7-13 ms before the ops listener closes. In-flight queries are drained correctly (both answered 1.5 s after SIGTERM). dnsdist health checks and non-Kubernetes load balancers never observe the drain. In Kubernetes the chart's preStop sleep hides it. | `TestSIGTERMLameduck/ReadinessFlipsBeforeListenersClose` | serve / config |
| E2E-5 | medium | `/healthz/ready` turns 200 when zonecache reaches `Live`, before the first live snapshot is published. For about 50 ms every query, primary zones and forwarding alike, answers SERVFAIL `servfail_unsynced` (`zonecache:unsynced`). Reproduced on 4 of 4 restarts with 10 ms readiness polling. A rolling restart therefore sends real traffic to a node that answers SERVFAIL. | `TestReadyMeansServing` | zonecache |
| E2E-6 | low | keepalived `Config.Validate`'s `safePath` admits `{` and `}` in every path-like field, because of the `{vrid}` placeholder. `--hmac-key-file /x}` renders `key 1 file:/x}` and closes the `auth_hmac` block early. These are operator flags, and `keepalived -t` runs before every swap, so this is not remotely reachable. | `FuzzKeepalivedRender` (inputs filtered unless `KUDURRU_E2E_KNOWN_BUGS=1`; crasher kept in `test/fuzz/testdata/fuzz/FuzzKeepalivedRender/`) | edge |
| E2E-4 | low | serve.go logs "generated the encryption key" on every start: `keys.Load` sets `Generated` for a key read back from the file. The key is reused correctly; only the log line is wrong. | `TestRestartKeepsState/NoFirstBoot` | auth/keys |

Gaps recorded as follow-ups rather than bugs:

- **E2E-2.** The sqlite driver's membership is hard-coded to
  `cluster.Self`, so two sqlite nodes cannot be configured to pull list
  bodies from each other, and each reports itself as leader.
- **Admin socket.** The default `/run/kudurru/kudurru.sock` is host-global:
  a second instance on the same host dies at startup. That is how the
  first two-node run failed.
- **keepalived validation.** It accepts `0.0.0.0` as the unicast source.
- **edge.Build.** It accepts listen hosts and backend hosts that are not
  addresses. They render quoted, and `dnsdist --check-config` refuses
  them.
- **Fuzz budget.** `FuzzImportBIND` spends its budget on `$GENERATE`
  inputs near the 1M-record cap.

## 1. BIND interop (`test/interop`)

`TestBINDSecondary` makes named a TSIG secondary of a kudurru primary. The
setup:

- **kudurru** serves `interop.test` and `denied.test` from an
  `objectsDir`:
  - `interop.test` has `zoneTransfer: useNetworkACL` with
    `transferACL: [127.0.0.0/8]`, `transferTSIGKeyRefs: [xfr-key]`, and
    `notify: specified` to named's port.
  - `denied.test` has `zoneTransfer: deny`.
  - It has `tsigKeys` with a random hmac-sha256 secret and
    `notify.source: 127.0.0.1`.
- **named** runs as the current user on a random port, with a test-written
  `named.conf` checked by `named-checkconf`: `listen-on port`, no rndc, no
  recursion, everything in the temp dir, `-g -4 -n 1`.

The subtests:

- **AXFR.** named loads the zone at kudurru's serial.
  - `dig @named +short` and a miekg query see both A records and the TXT.
  - A signed AXFR from kudurru and an AXFR from named yield the same RR set.
  - named logs `Transfer status: success`.
- **NOTIFY.** The test rewrites the record file, adding a record.
  - kudurru bumps the serial (about 0.8 s, file watch + controller).
  - named has the new serial and record about 1 s after the change. Its
    refresh timer is clamped to 300 s, so only NOTIFY can explain that.
  - named logs the notify from 127.0.0.1.
- **TSIGRefused.** An unsigned transfer gets REFUSED. A wrong secret and
  an unknown key name both fail with "bad authentication" (NOTAUTH
  BADSIG/BADKEY). No non-SOA record leaks.
- **ZoneTransferDeny.** A signed AXFR of `denied.test` gets REFUSED. named,
  configured as its secondary too, answers SERVFAIL for it and never
  serves its data.

`TestBINDUpstream` runs named as the primary of `upstream.test` and
`cond.test`. kudurru has an upstream group `bind` pointing at it:

- **ForwardZone** (forward `DNSZone` `upstream.test` → `bind`) passes:
  A over UDP, a CNAME chain over TCP, and NXDOMAIN passed through.
- **Conditional** (`upstreams.conditional: {cond.test.: bind}`) is E2E-1.
- **DefaultGroupNotNamed** passes: a name outside both suffixes is not
  answered by named.

## 2. Two nodes (`test/e2e/cluster_test.go`)

`TestTwoNodes` starts two `kudurru serve` processes. They have distinct
data dirs and ports, the sqlite driver, and one
`KUDURRU_ENCRYPTION_KEY`. Both carry a BlockList whose source is an
adblock list on a local `httptest` server, plus a Policy using it.

serve.go fixes the sqlite membership to `cluster.Self` (E2E-2). The test
therefore plays node B's resolver against node A's internal listener. It
uses `internalapi.NewCredentials` from the shared key and the same
`internalapi.Client` the resolver uses:

- **Status.** `/internal/status` reports `node-a` as node and leader (and
  `node-b` for B), with version and state.
- **DigestsMatch.** Both nodes report the same digest for the list,
  equal to sha256 of the served body.
- **PullBody.** `GET /internal/lists/{digest}` from A returns the body
  through the verifying reader, and it hashes to the digest. Size and
  source headers are correct, B serves the same body, and an unknown
  digest is a 404.
- **Unauthenticated.** These requests get 401 on both routes:
  - no token, a wrong bearer token, or the right token with the `Basic`
    scheme;
  - a token derived from another key, even with certificate checks off.

  A client holding another key cannot complete the TLS handshake (the
  derived CA does not match), and plain HTTP gets nothing.
- **BothNodesBlock.** Both nodes block the listed name.

## 3. Serve-level resilience (`test/e2e/serve_test.go`)

- **TestSIGTERMLameduck.** The fake upstream delays `slow.example` by
  1.5 s. Two in-flight queries (UDP and TCP) are pending when SIGTERM
  arrives, while a poller hits `/healthz/ready` every millisecond.
  - Both queries are answered.
  - The exit code is 0, and the exit comes 1.5 s after SIGTERM: the DNS
    listeners drain.
  - Readiness goes 200 → 503 → connection refused and never returns to
    200. The window is 1.7-13 ms (E2E-3).
- **TestRestartKeepsState.** Three runs over one data dir, with an
  `objectsDir` zone, the sqlite query log (`exclude.sudn: false`, since
  `.test` is special-use and would be excluded) and a forwarded name
  cached from the fake upstream.
  - Run 2 starts with the upstream stopped:
    - no first-boot seeding or new admin, and the same key file;
    - the same SOA serial;
    - the authoritative answer;
    - `cache restored entries=1`, and the cached name still resolves with
      the upstream gone (local.db cache dump);
    - querylog.db keeps the run 1 rows unchanged and new ids continue
      after them.
  - Run 3 starts after a record was added while the node was down: the
    serial advances and the new record resolves.
- **TestConfigValidationFails.** 12 cases each exit non-zero within
  100 ms. Each prints `error:` and names the key or cause:
  - unknown store driver, upstream strategy, conditional group, DNS
    protocol, log level, TSIG algorithm and querylog sink;
  - a missing TSIG secret file;
  - invalid YAML;
  - an unknown `KUDURRU_` key;
  - a malformed encryption key;
  - a missing `--config` file.
- **TestPortConflictsFailFast.** One test occupies each address in turn
  (DNS udp, DNS tcp, API, ops, metrics, internal). The node exits non-zero
  in 66-142 ms with `address already in use` and the address. A second
  process on the same data dir exits with "another kudurru process owns
  state.db".
- **TestReadyMeansServing.** Four restarts; the first query after
  readiness answers SERVFAIL every time (E2E-5).

## 4. Fuzzing

Without `-fuzz`, `go test` replays every seed corpus, so `make test`
covers them.

Existing in-package targets, now in `make fuzz` and CI:

- `internal/blocking/parser.FuzzParse` (the adblock/hosts/domains parser);
- `internal/dnsserver/proxyproto.FuzzParse`, `FuzzDatagram` and
  `FuzzStream` (the PROXYv2 stream);
- `internal/zone.FuzzImportBIND` (zone file / BIND import with an
  export/re-import fixed point) and `FuzzCanonicalKey`.

Their seeds stay `f.Add` plus the existing `testdata/fuzz` of the parser.
I did not write corpus files into other packages' `testdata`: corpus
files encode the target's argument types, and another M5 package editing
those targets would turn them into failures.

New targets in `test/fuzz` (external package, exported APIs; seeds in
`f.Add` and `test/fuzz/testdata/fuzz/`):

| Target | Input | Invariant |
|---|---|---|
| `FuzzConfigLoad` | arbitrary kudurru.yaml | Load never panics; accepted configs re-validate, have the default group and a DNS listener, and build an upstream set |
| `FuzzConfigEnv` | one `KUDURRU_<KEY>=value` | same, through the environment overrides |
| `FuzzConfigValues` | text | `Duration` and `ByteSize` round-trip through MarshalText |
| `FuzzDoHRequest` | method, path, content type, body | a real DoH listener (registry, TLS, ingress stage) always answers HTTP; never 5xx for client input; a 200 is `application/dns-message`, unpacks, echoes the id and question |
| `FuzzTSIGVerify` | wire message | the provider the server installs never panics; a verified message ends in a TSIG of a configured key with that key's algorithm (seeds: valid sha256/sha512, wrong algorithm, unknown key, unsigned) |
| `FuzzQueryLogFilter` | client, domain, upstream, zone, list, class, qtype, cursor, limit | against a real querylog.db: no SQL error (bound parameters), domain filters return only matching rows |
| `FuzzQueryLogNames` | text | qtype and status names round-trip; special-use classification never panics |
| `FuzzDnsmasqImport` | dnsmasq config | imported suffixes and groups build an upstream set and route |
| `FuzzKeepalivedRender` | `--vip`, router id, unicast src, peer, hmac file, qname, run dir | accepted configs render balanced braces and never copy an unsafe value (`\n \r " \ { } # ! ;`) verbatim (found E2E-6) |
| `FuzzKeepalivedFIFOLine` | FIFO line | parsed names carry no quote; state is one token |
| `FuzzDNSDistRender` | server id, listen, ACL, zone, backend name/address, transfer source, PROXYv2 secret | whatever `edge.Build` accepts renders YAML that parses with `server_id` intact and Lua with no unsafe value verbatim |

The query-log filters, BIND import, and keepalived/dnsdist input
validation from the brief are covered by `FuzzQueryLogFilter`,
`FuzzImportBIND` (already existed) and the three edge targets.

### Fuzz run

All 17 targets for 60 s each (`FUZZ_JOBS=3 FUZZ_PARALLEL=1`, shared
machine), no crasher apart from E2E-6 and two harness false positives
fixed in the targets (an invalid HTTP header value built by the client; a
Lua-quoted `!` flagged by the keepalived character set):

| Target | execs in 60 s |
|---|---|
| proxyproto `FuzzParse` / `FuzzDatagram` / `FuzzStream` | 1.88M / 1.95M / 1.91M |
| blocking parser `FuzzParse` | 302k |
| zone `FuzzCanonicalKey` / `FuzzImportBIND` | 539k / 190k (stalls on `$GENERATE` near the 1M cap, follow-up) |
| `FuzzConfigLoad` / `FuzzConfigEnv` / `FuzzConfigValues` | 12.5k / 19.8k / 54-180k |
| `FuzzDoHRequest` | 15k (HTTPS round trip per input) |
| `FuzzTSIGVerify` | 110-162k |
| `FuzzQueryLogFilter` / `FuzzQueryLogNames` | 14k / 18k |
| `FuzzDnsmasqImport` | 17-90k |
| `FuzzKeepalivedRender` / `FuzzKeepalivedFIFOLine` | 25k / 216k |
| `FuzzDNSDistRender` | 3-20k |

Go minimizes every new interesting input for `-fuzzminimizetime`
(default 60 s); with one worker that stalled whole runs (`FuzzDoHRequest`
did 1.4k execs). `hack/fuzz.sh` passes `-fuzzminimizetime 5s`
(`FUZZ_MINIMIZE`), which brought it to 15k. The 12 smallest inputs each
`test/fuzz` target found are committed as its seed corpus.

## 5. dex

`make test-auth-e2e` (dex v2.46.0 built from the pinned tarball, plus the
mock OP) now runs in the `e2e` CI job. The separate `auth-e2e` job is gone,
so it does not run twice. Locally, with the cached dex build, `TestDexEndToEnd` (17 subtests) and `TestMockOPEndToEnd` passed in 12 s.

## 6. AdGuard Home grammar conformance

`make test-adguard` downloads the official AdGuard Home v0.107.79 executable
for Linux amd64 or arm64, verifies its pinned SHA-256, and runs
`TestAdGuardBlackBox` under the Go race detector. It runs in the `e2e` CI job.
The executable and temporary configuration are removed afterward; neither its
copyleft implementation nor its rule data is vendored or included in artifacts.
The corpus is first-party data written from the public grammar. It checks
modifiers, exception precedence, rewrites and returned-answer inspection over
loopback DNS with a local stub upstream, without public resolver dependencies.

The test records two known v0.107.79 differences: bare `NOERROR` precedence
against address rewrites and combined client/type selectors on returned CNAMEs.
These explicit assertions remain reviewable; a version update must review them
and update the two archive hashes in `hack/test-adguard.sh`. This gate does not
claim complete list-corpus compatibility or performance qualification.

Other hosts can run the existing test directly with
`KUDURRU_ADGUARD_BINARY=/absolute/path/to/AdGuardHome go test -race -count=1 -v -run '^TestAdGuardBlackBox$' ./internal/blocking`.
An unset binary skips the ordinary unit-suite comparator; the Make target always
downloads the pinned executable and fails on download, checksum or test errors.

## Remaining external acceptance

All recorded in followups:

- kind + Helm with MetalLB;
- the apiserver-outage test (kill the control plane, restart a pod, answer
  from local.db);
- the 2-VM keepalived failover;
- the Knot catalog consumer.

They need Docker, kind or VMs. The `kudurrutest` harness is ready for a
Playwright dashboard run.
