### G23-R5/G23-R6/G23-R7/ZONE-Q1 Knot as primary, digest-pinned dnsdist live suites and the mixed-pool benchmark rule — November 10, 2026 (D383)

Lane commits on `wip/v020/qual`, base `d657cbfb`: `fb9082a1`
(`test(edge)`, dnsdist image constant), `2e4e89fc` (`test(interop)`, Knot
primary), `9cc67e35` (`docs(bench)`, run table, closure rule and
`hack/test-dnsdist-live.sh`), `7ea440ed` (`docs(edge)`, lab README); review
fixes `da40ab0e` (`test(interop)`, when the AA-less Knot reply is reachable)
`b8f4e6c1` (`test(edge)`, F39b publication observer paced under the internal
rate limit) and `de8d3cdb` (`fix(hack)`, broken pipe in
`hack/test-dnsdist-live.sh`). No
product code, no generated file, no new dependency; `go.mod` unchanged. No
GitHub workflow was dispatched and no pull request label was changed.

What now happens:

- **Knot is qualified as the primary of kudurru's inbound secondary**
  (ZONE-Q1). `TestKnotInboundSecondaryTransfer` (`test/interop/knot_secondary_test.go`)
  starts actual Knot DNS 3.6.2 (`cznic/knot@sha256:174b0fcb...`, version checked
  as `knotd, Knot DNS 3.6.2`) on a fresh internal network of the private
  daemon and runs kudurru's receiver against it. It covers what the `TestBIND*`
  secondary tests cover against BIND: signed AXFR of a multi-message zone (206
  records; TTLs, SOA timers, TXT case and an unknown RR type preserved); the
  wrong secret (`TSIG BADSIG`), an unknown key and the wrong algorithm (Knot
  answers NOTAUTH without a signature, refused as `required TSIG missing`)
  for SOA and AXFR; an unsigned AXFR (NOTAUTH); a real unsigned Knot SOA reply
  refused with `dns.ErrNoSig` when a signature is required; a journal IXFR after
  two `knotc` transactions across 4294967294 -> 4294967295 -> 0, equal to a
  fresh AXFR (Knot steps onto serial 0, BIND skips it); the AXFR fallback of a
  zone with `journal-content: none` (Knot logs "incomplete history ... fallback
  to AXFR"); XoT on Knot's `listen-tls` port with the certificate verified for
  the container address against an explicit private CA, including a journal
  IXFR 0 -> 1 over TLS, while system trust, the plaintext port under TLS and a
  wrong key over TLS are refused without data; and the production receiver
  chain (Manager, `local.db`, `zoneview.Source`, real UDP/TCP listeners), which
  serves the Knot zone, keeps serving it after the Knot container is stopped and
  the receiver restarts offline without renewing confirmation or expiry, and
  answers SERVFAIL at SOA expire (120 s) with zero root forwarding.
  `hack/test-knot-catalog.sh` runs this test and `TestKnotCatalogConsumer`; the
  shared preconditions (opt-in, explicit private socket, both image digests,
  knotd version) moved into `newKnotDocker`.
- **One Knot difference was found and is pinned, not hidden.** Knot 3.6.2
  answers an IXFR at the current serial with one signed SOA without the AA bit
  (BIND sets it; checked with a raw query: AA clear for client serial equal to
  or newer than the zone, set for an older one and for AXFR). kudurru's
  receiver requires AA on every transfer message, so it refuses that reply
  without data (`secondary: non-authoritative response`). A refresh against a
  single consistent primary does not send that IXFR: at an unchanged SOA
  serial the Manager keeps its stored copy and transfers nothing
  (`internal/secondary/manager.go`, refresh). The SOA poll and the IXFR are
  separate requests, though, so the reply is reachable when they land on
  different instances behind one address or the primary moves back to the
  stored serial between them; that refresh then fails and is not confirmed,
  and no data is lost. The test asserts the refusal, so a later change of
  either side is visible; whether to accept a signed single-SOA reply at the
  client's serial without AA is a follow-up (`followups.md` in this folder).
- **The live dnsdist suites no longer depend on one machine** (G23-R5). The four
  hardcoded image IDs (`sha256:89c7f196...`, the locally built
  `kudurru-dnsdist:vip-fix-b8fd96f`, in `console_live_test.go` (also used by
  `managed_cache_live_test.go`), `serve_remote_cache_mount_test.go`,
  `serve_unifi_edge_test.go` and `serve_anchor_edge_live_test.go`) are replaced
  by one constant, `kudurrutest.DnsdistImage` =
  `powerdns/dnsdist-21:2.1.3@sha256:c70ae7363712188ab2878d22bc9f1801cae29190688474a224caee2089f74837`,
  the FROM line of `images/dnsdist/Dockerfile`. `TestSupervisorVersions` now fails
  when that FROM line moves and the constant stays behind (checked by changing
  the constant: the test failed, then passed after the revert). The suites only
  use `--entrypoint dnsdist`; the upstream image's `/usr/local/bin/dnsdist` and
  the lab image's have the same SHA-256 (`444c6799...`), and both print
  `dnsdist 2.1.3 (Lua 5.1.4 [LuaJIT 2.1.1737090214])`. The digest was pulled
  once into the private daemon (image ID `sha256:f2d41a5b...`).
  `hack/test-dnsdist-live.sh` checks the constant against the Dockerfile, pulls
  the digest only when it is missing and runs the six legs (F43 YAML and Lua,
  F28, F39b, anchor edge, F18c) with scratch directories it removes.
- **The benchmark closure rule for the mixed pool is recorded before the final
  run** (G23-R6). `bench/README.md` "Closure rule for the mixed runner pool
  (D383)": the final head gets exactly one paired run with the unchanged
  collector, workloads, threshold, noise floors and limits; it is the run of
  record whatever CPU it draws and its CPU model is recorded next to its
  outcome; a pass closes the benchmark element of G23 for that head and that
  CPU model only; a failure stays a failure (no rerun to draw another VM, no
  replaced or selected sample, no raised or recalibrated limit); on EPYC the
  diagnosis quotes `BenchmarkGet/hit-parallel`, `BenchmarkStrategies/fast/strict`,
  `BenchmarkLatency/stevenblack/miss` and `BenchmarkLookup100k/cname` p99 against
  run 42545184734 on `73d4b057`; a fix or method change is a new source or
  decision followed by a new run. No collector change (interleaving was
  rejected: it would change the control right before the deciding run).
- **bench/README.md lists all seven hosted recordings** (G23-R7) with mode,
  source, CPU and outcome (42514599389, 42517712186, 42518988512, 42522360297,
  42530394856, 42545184734, 42554034011), describes the three later paired runs,
  and records that the regex-1000 miss p99 (871 vs 8,376 ns on Ryzen, 1,389 vs
  11,390 ns on EPYC) supersedes the earlier adverse regex result and that the
  D348 cache fix has not been measured on EPYC.

Tracker row text for the integrator (G23-R7; this lane did not edit the
tracker):

- F20d (completion-plan.md:119), replace "regex1000 p99 18276ns remains
  adverse" with: "the historical regex1000 p99 18276 ns is superseded: hosted
  paired BenchmarkLatency/regex-1000/miss p99 is 871 vs 8,376 ns reference on
  Ryzen (42554034011, 9963dec2) and 1,389 vs 11,390 ns on EPYC (42545184734,
  73d4b057); final-head paired run under D383 open".
- F21c (completion-plan.md:122), append: "Final-head run counted under the D383
  closure rule (bench/README.md); D348 parallel hit not yet measured on EPYC".
- F63 (completion-plan.md:191), replace the run history after "Paired Ubicloud
  run42518988512" with: "Seven hosted recordings, all listed with CPU and
  outcome in bench/README.md: EPYC paired 42518988512, 42530394856 (reference
  ineligible) and 42545184734 failed; Ryzen 42522360297 failed cache hit/Set;
  Ryzen 42554034011 on 9963dec2 passed all three comparisons and all absolute
  gates. Final-head run counted under D383; current-source and final gates stay
  open".
- F68b (completion-plan.md:199), append: "Knot as primary of inbound secondary
  transfer (AXFR, journal IXFR across wrap, journal-less fallback, TSIG, XoT,
  expiry) passes TestKnotInboundSecondaryTransfer at 2e4e89fc (D383);
  local-only: private Docker daemon, not run in hosted CI".
- F43a/F43b (completion-plan.md:159-160) and F39b (:153), append: "live suites
  now use the upstream dnsdist digest
  `powerdns/dnsdist-21:2.1.3@sha256:c70ae7363712188ab2878d22bc9f1801cae29190688474a224caee2089f74837`
  (D383); all live dnsdist tests (F43 YAML/Lua, F28, F39b both tests,
  anchor edge, F18c) pass unskipped at d2039002 (local-only, private daemon);
  the earlier F39b and anchor failures were harness races with the backend's
  own reconcile commits, fixed in the tests (QUAL-OPEN3, D383)".
- G23 (completion-plan.md:222 and gaps.md:112), add per comparator: BIND and
  AdGuard hosted; Knot both directions local-only (private daemon,
  `cznic/knot@sha256:174b0fcb24c6c3bc5065c9d1ce46b588eec607a5f32bc40ff0cda60150f00bd9`
  knotd 3.6.2, last unskipped pass of `TestKnotCatalogConsumer` and
  `TestKnotInboundSecondaryTransfer` at 2e4e89fc, rerun by the reviewer at
  7ea440ed); dnsdist live local-only (upstream digest
  `sha256:c70ae7363712188ab2878d22bc9f1801cae29190688474a224caee2089f74837`;
  F43 YAML/Lua, F28, F39b (both tests), anchor edge and F18c last unskipped
  pass at d2039002, QUAL-OPEN3 `live04-all.log`); benchmark final-head run open under D383;
  Windows deferred (D353); two-VM keepalived deferred (D358).
- `dev-docs/followups.md`: add the three rows drafted in
  `out/QUAL-INTEROP/followups.md` (hosted Knot/dnsdist job with image
  digests; whether to accept Knot's AA-less up-to-date IXFR reply; the
  dnsdist diagnosis row is replaced by the QUAL-OPEN3 row).

Evidence under `.context/v020-final-20261110/evidence/QUAL-INTEROP/`:

- `knot01-baseline.log`: `hack/test-knot-catalog.sh -race -v -timeout=5m` before
  any change: `--- PASS: TestKnotCatalogConsumer (5.32s)`, 8 of 8 subtests,
  `ok test/interop 24.325s`. It shows that the private daemon's internal
  networks work despite `--bridge=none --iptables=false`.
- `knot02-both.log` (retained failure): first run of the new test. 9 subtests
  passed; `IXFR` failed at the same-serial confirmation with `secondary:
  non-authoritative response`, which skipped `VerifiedTLS` (it needed the IXFR
  snapshot). That is the Knot AA finding above; the test now asserts the
  refusal and stores the snapshot first.
- `knot03-both.log`: final run of both Knot tests at the code of `2e4e89fc`,
  `-race`: `--- PASS: TestKnotCatalogConsumer (7.66s)` (8 subtests) and
  `--- PASS: TestKnotInboundSecondaryTransfer (6.09s)` with `AXFR`,
  `TSIGRejection` (5 subtests), `IXFR`, `JournalFallback`, `VerifiedTLS` and
  `ExpiryAfterPrimaryStops (4.19s)` all passing, no skips, `ok test/interop
  19.990s`. Raw Knot logs (0600) in `knot-private/`.
- `interop04-bind-regression.log`: `KUDURRU_E2E=1 KUDURRU_REQUIRE_BIND=1 go test
  -race` of `TestBINDInboundSecondaryTransfer` (1.29s),
  `TestBINDSecondaryIXFR` (0.16s), `TestBINDSecondaryVerifiedTLS` (0.54s) and
  `TestBINDSecondaryThroughListeners` (0.20s) against local BIND 9.18: all pass
  after the shared certificate helper change; both Knot tests skip there
  without `KUDURRU_KNOT=1`, as they do in hosted CI.
- `dnsdist01-live.log`: `hack/test-dnsdist-live.sh` at `fb9082a1` on the
  upstream digest, `-race -v`, GOMAXPROCS=2: `TestConsoleLiveSupervisor` YAML
  PASS (48.30s) and Lua PASS (43.98s); `TestManagedCacheLivePinnedTransition`
  PASS (13.87s, yaml and lua); `TestRunServeUniFiHostnamePolicyLifecycle` with
  `KUDURRU_F18C_DNSDIST=1` PASS (99.58s). Retained failures:
  `TestRunServeRemoteEdgeRegexAndLateFill` FAIL (55.99s) and
  `TestRunServeRemoteEdgeAuthoritativeCacheACL` FAIL (34.38s), both "timed out:
  quiet original publication before one-shot mutation" (the first after its
  regex, token-refusal and lost-acknowledgment phases had passed);
  `TestRunServeManagedEdgePinnedDaemonRestart` FAIL (6.52s), "actual
  edge/backend answer missing udp anchor.test. ... i/o timeout".
- `dnsdist02-ab-build.log` and `dnsdist03-ab-labimage.log`: A/B control. The
  `./cmd/kudurru` race test binary was built with the constant temporarily set
  back to the old lab image ID (never committed; reverted with `git checkout`
  right after the build) and the three failing tests were run with it: the same
  three failures with the same messages (anchor 8.04s; F39b 56.14s and 34.08s).
  The failures do not come from the image change: they reproduce with the
  byte-identical dnsdist of the old image, so the cause is in kudurru or the
  fixtures at this head. Both waits observe kudurru state (the F39b wait needs
  the resolver publication to stay unchanged for 500 ms; the anchor wait needs
  the backend's answer through the edge); they were last recorded passing in
  the November 2-8 runs, before this lane's base.
- Review fixes (logs `fix-*.log`):
  - `fix-diag01-f39acl.log` (retained failure, temporary diagnostic build,
    never committed): `TestRunServeRemoteEdgeAuthoritativeCacheACL` logging
    every `/internal/zones` read of the "quiet original publication" wait:
    the publication had settled at 7, but reads alternated between
    publication 7 and `429 Too Many Requests` (plus early `503`s), and every
    refusal restarted the 500 ms quiet window; FAIL (34.52s).
  - `fix-diag02-f39acl.log` and `fix-diag02-internal-requests.log` (retained
    failure, temporary request log in the internal listener, never
    committed): 1,062 requests from 127.0.0.1 in 28 s, all
    `GET /internal/zones`, 442 x 200, 476 x 429, 144 x 503, about 42 per
    second against `internalapi.DefaultRateLimit` of 20 per address. The
    test's 30 ms observer and the supervisor's 100 ms zone poll share that
    address; the refused supervisor polls also put its caches in
    `bypass-unavailable`. Root cause of the reviewed timeout: a fixture defect.
  - `fix-03-f39-paced.log` (retained failure): with the observer paced at
    150 ms (`b8f4e6c1`, deadline, quiet window and reset unchanged) the wait
    passes and both F39b tests get further:
    `TestRunServeRemoteEdgeRegexAndLateFill` passes five phases (publication
    and regex invalidation, regex allow/deny and token refusal, lost console
    acknowledgment, client-scope bypass, compiled `$client` rule) and fails at
    the forward-zone ACL phase, "original client response failed 127.0.0.2
    probe.forward.example." (50.07s);
    `TestRunServeRemoteEdgeAuthoritativeCacheACL` passes the measured
    authoritative cache hit and the primary ACL phase and fails after the
    forced auth failure, "original client response failed 127.0.0.2
    probe.private.example." (37.04s).
  - `fix-diag04-f39-anchor.log`: empty, the run was stopped while it waited
    for the machine lock; nothing ran.
  - `fix-diag05-f39-anchor.log` and `fix-diag06-f39-anchor.log` (retained
    failures, temporary DNS diagnostics, never committed): the forward-zone
    failure is SERVFAIL (rcode 2) for the ACL-allowed client 127.0.0.2 for
    about 1.4 s after the ACL publication (5 retries 250 ms apart SERVFAIL,
    then rcode 0), while the edge reports the backend `up` and both caches in
    `bypass-zone-client-acl`; the authoritative failure is one UDP query lost
    (5 s timeout) right after the auth failure detaches both caches, and every
    retry 250 ms apart answers at once. `TestRunServeManagedEdgePinnedDaemonRestart`
    failed in `fix-diag05` (first UDP query after the edge child restart lost,
    1 s timeout, ten immediate retries answered) and passed in `fix-diag06`
    (12.97s).
  - `fix-09-f39-anchor-final.log` (retained failure): the runner script
    stopped before any suite with "write /dev/stdout: broken pipe" from
    `dnsdist --version | head -n 1` under `pipefail`; fixed in `de8d3cdb`
    (`fix-10-version-line.log` shows the fixed extraction).
  - `fix-11-f39-anchor-final.log`: `hack/test-dnsdist-live.sh f39 anchor` at
    `de8d3cdb`: both F39b tests fail at the same later phases (48.82s and
    36.24s; the quiet-publication wait passed in all four paced runs);
    `--- PASS: TestRunServeManagedEdgePinnedDaemonRestart (12.83s)`.
  - `fix-08-vet.log`: `go vet ./cmd/kudurru ./test/interop` clean; `gofmt -l`
    empty for the touched files.
- `race05-unit-vet.log`: `go test -race ./deploy ./internal/testutil/kudurrutest
  ./ee/edge/dnsdist` pass (14.5 s, 1.5 s, 15.1 s); `go vet` of `./test/interop
  ./deploy ./internal/testutil/kudurrutest ./ee/edge/dnsdist ./cmd/kudurru`
  clean. `lint06.log`: golangci-lint v2.15.0 on the same five packages, `0
  issues.` `gofmt -l` empty.

What remains open: Knot and the dnsdist live suites are still local-only
evidence for v0.2.0, run on the private daemon of this machine, though now only
from registry-digest images that any daemon can pull; a hosted job that starts a
private daemon and runs `hack/test-knot-catalog.sh` and `hack/test-dnsdist-live.sh`
is a follow-up (row drafted in `out/QUAL-INTEROP/followups.md`; integrator: add
it to `dev-docs/followups.md`). The F39b timeout the review reported was a
fixture defect, fixed in `b8f4e6c1`; the later F39b failures and the
intermittent anchor edge test were harness races with the backend's own
reconcile commits, fixed in `d2039002` (QUAL-OPEN3, D383). All live dnsdist
tests pass unskipped at `d2039002` (QUAL-OPEN3 `live04-all.log`). The F43
privilege-drop variant (`KUDURRU_F43_CHILD_USER`, root test process) was not
run. Knot's AA-less up-to-date IXFR reply is refused by the receiver; a refresh
against one consistent primary does not ask for it, but a split or
rolled-back primary can reach it (one failed refresh, no data loss);
accepting it was not done because it would relax a transfer-validation rule,
and the decision is a drafted follow-up row. The final-head paired benchmark
has not run (the integrator triggers it); the D383 rule decides how its result
counts. Windows (D353) and two-VM keepalived (D358) stay deferred, not passed.
