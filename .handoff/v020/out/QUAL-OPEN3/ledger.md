### QUAL-OPEN3 The remaining live dnsdist failures, diagnosed and fixed in the harness — November 12, 2026 (D383)

Lane commit on `wip/v020/qual`, base `de8d3cdb`: `d2039002`
(`test(edge)`, settle the backend before one-shot live dnsdist queries). Test
code only (`cmd/kudurru/serve_remote_cache_mount_test.go`,
`cmd/kudurru/serve_anchor_edge_live_test.go`). No product code, no generated
file, no new dependency, no workflow change.

What now happens:

- **All six legs of `hack/test-dnsdist-live.sh` pass on the upstream dnsdist
  digest at `d2039002`.** That is every live dnsdist test:
  `TestConsoleLiveSupervisor` with YAML and with Lua,
  `TestManagedCacheLivePinnedTransition`,
  `TestRunServeRemoteEdgeRegexAndLateFill` and
  `TestRunServeRemoteEdgeAuthoritativeCacheACL` (F39b),
  `TestRunServeManagedEdgePinnedDaemonRestart` (anchor edge) and
  `TestRunServeUniFiHostnamePolicyLifecycle` with `KUDURRU_F18C_DNSDIST=1`.
  The image is
  `powerdns/dnsdist-21:2.1.3@sha256:c70ae7363712188ab2878d22bc9f1801cae29190688474a224caee2089f74837`
  (image ID `sha256:f2d41a5b...`), and the program reports `dnsdist 2.1.3`.
- **Root cause of each failure that remained at `de8d3cdb`.** In all three, a
  one-shot DNS query raced work that the backend node was still committing
  after the test's own write or start. It was not an edge fault.
  - `TestRunServeRemoteEdgeRegexAndLateFill`: the node itself answered
    SERVFAIL (a direct query to the node, without the edge, also got rcode 2)
    for the ACL-allowed client right after the forward zone's query ACL
    write. Every DNSZone write replaces its lifecycle source. Until the
    primary runtime admits the new source, the node serves fail-closed
    coverage for that zone (publication 22). The next publication (23)
    answers. The test waited only for the edge to report
    `bypass-zone-client-acl`, which the intermediate coverage publication
    already reports.
  - `TestRunServeRemoteEdgeAuthoritativeCacheACL` and
    `TestRunServeManagedEdgePinnedDaemonRestart`: the query got no response at
    all. A relay between dnsdist and the node showed that dnsdist forwarded
    it (backend `queries` +1, `downstream-timeouts` +1), the node received
    it, and the node never replied, not even late. A read-only commit watcher
    on the node's SQLite store showed that every loss came shortly after a
    zone status or lifecycle commit (214 ms after one in `diag06`, and in the
    middle of the startup reconcile in `diag07`). The answer came before the
    next publication was installed: during that time `/internal/zones`
    returns 503 for up to about 1.3 s. An authoritative answer captured from
    a publication that a later commit supersedes is withheld by design (D313,
    D332). One zone write is several commits (status, lifecycle source,
    primary head, status, lifecycle), spread over about 4.6 s under `-race`
    with GOMAXPROCS=2 on the shared machine, and the tests queried after the
    first of them.
  - The fix in the harness (`d2039002`): `writePublished` now waits after
    each mutation as well as before it. It waits for the settled source (the
    existing `controlMountedSourceQuery`: statuses at `observedGeneration`, no
    pending NOTIFY, every DNSZone Ready/Serving with an aligned active
    lifecycle), then for 500 ms of quiet installed publication. The anchor
    edge test waits for the same source settlement with its two primary
    heads after each backend start, then for a direct authoritative answer
    from both zones. A direct authoritative answer is only emitted from the
    current generation, so it proves the settled publication is installed.
    No DNS assertion is retried or relaxed. The internal rate limit, the
    25 s wait deadline, the 500 ms quiet window and the reset on a failed
    read are unchanged.
- **The rate limit is not a product fault.** The F39b supervisor polls
  `/internal/zones` every 100 ms (10 requests per second) and shares
  127.0.0.1 with the observer. `internalapi.DefaultRateLimit` is 20 per
  second per address, with a burst of 40. The production default
  `--zone-poll` is 5 s (`cmd/kudurru/edge.go`), 0.2 requests per second per
  edge, so one source address would need about 100 edge supervisors behind
  it to reach the limit. After `b8f4e6c1` the supervisor's debug log of
  failed zone polls has no `429 Too Many Requests` in any diagnostic run
  (`diag01` to `diag06`). The `bypass-unavailable` flips seen in
  `fix-diag06` were 503s ("complete serving publication unavailable") from
  publication lag, not 429s.
- **Why the same tests passed November 2-8 (likely, not demonstrated).**
  The tests did not change, and neither did the coverage and withholding
  rules (`dd416316`, October 14; D313/D332, November 7-8). Whether a
  reconcile commit lands between the write and the next one-shot query
  depends on timing, and the runs of this week were on a machine shared by
  three lanes, under `-race` with GOMAXPROCS=2, where one zone write's
  reconcile took several seconds. That is the likely explanation. No
  retained run compares reconcile timing then and now.
- **The product window, measured without `-race`.** A temporary mounted
  `runServe` test (never committed, `fix-measure-window.patch`), built with
  `CGO_ENABLED=0` and no `-race`, creates a primary zone with one record and a
  forward zone, then probes both directly over UDP every 10 ms (1.5 s client
  timeout) while it makes 8 rounds of 7 writes 4 s apart. This is a test
  binary that runs the production `runServe`, not the packaged release
  binary, on 8 CPUs shared with other lanes (load average 11 to 18). With no
  writes, none of 691 probes failed. Longest unanswered stretch per write
  kind (`fix-measure03-release.log`; the earlier `fix-measure01-release.log`,
  which lacks the combined metric, has single-kind maxima within about
  0.1 s of these):
  - Authoritative zone, after a write that does not touch it (BlockList
    create or delete, the forward zone's ACL): no response for up to 586 ms.
  - Authoritative zone, after a write to the zone's ACL or its record set:
    SERVFAIL, then no response, up to 860 ms in a row.
  - Forward zone, after a write to its own ACL: SERVFAIL for up to 558 ms.
    Writes to other objects did not affect it.

  Of the authoritative probes in the 4 s after each write, 5 to 6 percent
  failed after an unrelated BlockList write and about 21 to 23 percent after
  a write to the zone or its records. A build without `-race` does not
  remove the window; it is shorter than under `-race` (up to about 1.3 s
  per commit there). The
  settle waits in `d2039002` make the live suites wait it out, so no test
  bounds it now.

Evidence under `.context/v020-final-20261110/evidence/QUAL-OPEN3/`:

- `diag01-f39.log` and `diag01/` (retained failure, temporary diagnostics at
  `de8d3cdb`, never committed): both F39b tests FAIL (56.17s, 40.86s). At the
  forward-zone failure, each retry 100 ms apart logs the direct node rcode,
  the edge rcode and `/internal/zones`. Direct and edge are both rcode 2 at
  publication 22, then 503 for about 0.6 s, then rcode 0 at publication 23.
- `diag02-acl-1.log` and `diag02-acl-2.log` (retained failures): the ACL test
  lost a query at two different one-shot points, after the auth-failure
  detach and after the lease expiry. Both runs wrote the same supervisor log
  file, so only the second run's log is in `diag02/`.
- `diag03-acl.log` and `diag03/` (retained failures, 2 of 2): dnsdist
  statistics around the lost query show backend `queries` 16 -> 17 and
  `downstream-timeouts` 1 -> 2. The edge forwarded the query and got no reply.
- `diag04-acl.log` (retained failures, 2 of 2): the client listened on an
  unconnected socket. No reply from any source arrived, which rules out a
  reply sent to the PROXY client address.
- `diag05-acl.log` and `diag05/` (retained failures, 2 of 2): a UDP relay
  between dnsdist and the node logs the query (`id=4096`, from 127.0.0.2)
  going to the node with no reply ever, while the queries before and after
  it on the same dnsdist socket are answered in about 1 ms.
- `diag06-acl.log` and `diag06/` (retained failure): the commit watcher shows
  the restoring zone write as commits at +0.0, +0.2, +2.2, +2.9, +3.1, +4.4
  and +4.6 s. The lost query came 214 ms after the last commit, and 503
  stretches of about 1.3 s follow the commits.
- `diag07-anchor.log` (retained failures, 3 of 4 with `-count=4`; one PASS
  12.82s): the first UDP query after the edge restart at 28.317 was lost.
  The backend's startup reconcile committed at 28.035 (DNSZone status) and
  28.527 (lifecycle).
- `diag-f39-instrumentation.patch` and `diag-anchor-instrumentation.patch`:
  the temporary diagnostics (relay, commit watcher, snapshots), never
  committed.
- `live01-f39-anchor.log`, `live02-f39-anchor.log` and `live03-f39-anchor.log`:
  `hack/test-dnsdist-live.sh f39 anchor` with the fix, three consecutive runs,
  all PASS: RegexAndLateFill 59.92s, 61.01s and 64.09s;
  AuthoritativeCacheACL 41.24s, 39.94s and 67.82s; anchor edge 15.08s, 14.91s
  and 14.96s.
- `live04-all.log`: `hack/test-dnsdist-live.sh` (all six legs, `-race -v`,
  GOMAXPROCS=2) at `d2039002`, `rc=0`. The results:
  - `--- PASS: TestConsoleLiveSupervisor (48.35s)` (YAML)
  - `--- PASS: TestConsoleLiveSupervisor (45.20s)` (Lua)
  - `--- PASS: TestManagedCacheLivePinnedTransition (15.18s)`
  - `--- PASS: TestRunServeRemoteEdgeRegexAndLateFill (58.14s)`
  - `--- PASS: TestRunServeRemoteEdgeAuthoritativeCacheACL (38.64s)`
  - `--- PASS: TestRunServeManagedEdgePinnedDaemonRestart (13.94s)`
  - `--- PASS: TestRunServeUniFiHostnamePolicyLifecycle (95.43s)`

  There were no skips, and no containers were left on the private daemon.
- `lint01-cmd-kudurru.log`: golangci-lint v2.15.0 on `./cmd/kudurru/...`
  (this includes `b8f4e6c1` and `d2039002`), `0 issues.`. `gofmt -l` is
  empty for both touched files. The bench guards
  (`python3 -B -m unittest discover -s hack -p 'test_bench_*.py'`) report OK.

Tracker text for the integrator: in the F39b row (completion-plan.md:153)
and the G23 dnsdist entry, replace "F39b live fails at this head ... anchor
edge test is intermittent" with: "all live dnsdist tests pass unskipped at
`d2039002` on the upstream digest (local-only, private daemon): F43
YAML/Lua, F28, F39b (both tests), anchor edge and F18c; the earlier F39b and
anchor failures were harness races with the backend's own reconcile
commits, fixed in the tests (D383)".

- `fix-measure01-release.log` and `fix-measure03-release.log`: the window
  measurement above, `--- PASS: TestQualOpen3MeasureWindow` (239.99s and
  241.59s; it only measures, it asserts no bound). `fix-measure02-release-aborted.log`
  is a run I stopped after 15 s to add the combined unanswered metric; it has
  no results.

What remains open: the live suites are still local-only evidence, run on
this machine's private daemon; the hosted job is the follow-up row
QUAL-INTEROP drafted. The product behavior the tests ran into is not
changed for v0.2.0, is real on any node, and shipping it as is is a
release-acceptance call for the owner (D383 states it that way; it does not
say no product change is needed). After a canonical commit, an authoritative UDP
answer captured from the superseded publication gets no response until the
next publication is installed. A DNSZone write also gives that zone
fail-closed SERVFAIL coverage until its new lifecycle source is admitted. A
client that retries recovers, and dnsdist does not retry UDP for it. The
window lasts as long as a publication rebuild: up to about 1.3 s per commit
under `-race` on 2 shared CPUs, and up to 586 ms (no response) or 860 ms
(SERVFAIL then no response) without `-race` with `CGO_ENABLED=0` on 8 shared
CPUs. It was not measured on the packaged release binary or on an idle
machine. It is drafted as a follow-up (`out/QUAL-OPEN3/followups.md`):
repeat the measurement on the packaged binary, add a test that bounds the
window, avoid invalidating an authoritative zone on commits that do not
touch it, and decide whether a withheld authoritative answer should become
an empty SERVFAIL, as D332 already allows for unavailable local data.
The F43 privilege-drop variant (`KUDURRU_F43_CHILD_USER`, root test process)
was not run.
