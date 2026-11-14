### CI-RED-3 The four hosted failures of ecd633b1 — November 13, 2026 (no decision number assigned)

Each failure has its own cause and its own commit. Three of the four are
done; the fourth (activation 503) is partial: its product cause is not
fixed, because that needs the decision CI-RED-2 proposed, and the tests
only avoid it. Commits on `wip/v020/ci` after 2943cd6b: `ec92628e`,
`16d9bb01`, `c51b7d06`, `5e7e8ccb`, and after review `6cd2663d`,
`e57d9a64`, `2ee92e93`.

1. `FuzzQueryLogFilter` (fuzz 5/7, corpus `4ed1bc8fcd981c34`: domain filter
   `"*\x00"` returned `www.example.com`). This is a product defect, and a
   real node hits it: SQLite's LIKE reads its pattern as a C string, so
   `domain=*%00` was evaluated as `*` and returned every row. PostgreSQL text
   (the central log) cannot hold NUL, so the same filter there answered a
   server error. `QueryFilter.Validate` now refuses a NUL in client, domain,
   upstream, list, zone and policy with `ErrInvalidFilter`. `Queries` and
   `Suggestions` (prefix) apply it, and `GET /queries` checks it before any
   node or central fan-out, so every scope answers 400. A stored query-log
   name never holds a raw NUL, because DNS names are kept in presentation
   form. The ClickHouse sink only writes, so it has no query path to fix. The
   minimized input is committed as corpus `2de5675dae4a0694`. (`ec92628e`)
2. `TestActualSQLiteSourceWaitsWithOriginalCancellationAndWriterToken/lease_cancel`
   (`canceled original writer entered command`). The test watches an
   interleaving that is allowed; this is not a fencing defect. The host links
   a lease to its operation with `context.AfterFunc`, so the cancellation
   reaches the waiting command one goroutine hop after `leaseCancel` returns.
   The test released the writer inside that hop. A command that enters there
   is still refused at commit: `final` reads the lease synchronously and
   returns `integration host: incarnation fenced`, and nothing is committed.
   A real node cannot commit after a lease cancel. 1c4f81ab ("end elected
   terms before an authority change is visible") is not the cause. It cancels
   terms only on a durable authority change, and lease_cancel changes no
   authority. It was already in 291c55a5, and nothing under `ee/integration`
   or `internal/store/sqlite` changed between 291c55a5 and ecd633b1, so the
   failure is intermittent. The test now waits, with a bound, until the
   operation context has seen the cancel before it releases the writer.
   (`16d9bb01`)
3. `TestRunServeSQLiteStoredTSIGTransportsAndEncryptionRestart`
   (`serve_tsig_transport_mount_test.go:114 read original native DNS frame
   length`). The `signed restart exchange` lines in the hosted log are
   progress logs, not failures: every signed exchange of phases 0-2 was
   answered. The only query that went unanswered was the phase-2 TCP query
   signed with the replaced key, which should get NOTAUTH BADSIG. This is a
   product defect, and a real node hits it. The transport answers a failed
   TSIG check itself, before the pipeline, and nothing bound that message to
   the captured output. An unbound root output keeps full data currency, so
   between any canonical commit and the next publication the refusal was
   withheld and the client timed out. Answers of the same query class follow
   the served publication (D378) and were sent. This is not the CORE-COMMIT-GAP
   authoritative window: unsigned authoritative answers were answered
   throughout. Native and encrypted transports now bind such a refusal to the
   captured output. It is then sent while its publication is served and
   withheld once a successor is published. A BADTIME that replaces an answer
   the pipeline already bound keeps that binding. TSIG refusals of UPDATE,
   NOTIFY and transfers keep full currency, so they are still withheld
   after an unpublished commit; `TestRootAnchorTSIGRefusalFollowsTheServedPublication`
   now asserts that for UPDATE, NOTIFY, AXFR and IXFR (`2ee92e93`). `docs/architecture/query-path.md`
   describes this. The hosted log cannot show which commit was in flight
   during phase 2; the mechanism is reproduced, but not that exact commit.
   (`c51b7d06`) Notes for the core lane: `out/CI-RED-3/for-core.md`.
4. `TestEnrolledProcessConflictResolution` (`POST /cluster/activate: status
   503 want 200, error unavailable` 4 s in). Partial. The node log was
   empty (level error). By reading, the refusal came from the activation's
   current command: `unavailable` there means a commit superseded the
   request's publication while it ran ("current writable serving
   generation unavailable", `servinghttp.CheckWriter` ->
   `Publisher.CheckWriterLease` returning `ErrPending`). This attribution
   is inferred; the activation 503 itself was never reproduced locally.
   The superseding commit was most likely the node's own: a zone write is
   followed by status, `PrimaryStatus.Reconcile`, three `LifecycleCAS`
   steps, `Admission`, and another `LifecycleCAS` after each zone status
   write. Under race these land over about 11 s, up to 1.75 s apart, and
   `enrolledZoneSettled` can pass before the last one. This is neither the
   F28 updater-first-step gate (the node uses static anchors) nor the
   CI-RED-2 409 fix; CI-RED-2's fix does not cover it. It is the CI-RED-2
   open item (a command is refused 503 when another commit publishes while
   it runs), here with no concurrent client: an in-process diagnostic had
   the record set POST right after the zone POST refused this way in 3 of
   6 runs, so an operator who creates a zone and then a record can meet
   it. The product is unchanged: a ready node still refuses a writer
   command 503 (no effect, Retry-After) during its own post-zone-write
   commits. Changing that needs the decision CI-RED-2 proposed (capture
   again before the first effect, or a defined client retry policy on
   that answer); it changes D199/D208 behavior for every command, so it
   was not made here. As a stopgap the three e2e tests that activate after
   a zone write take the enrollment review only after the reviewed source
   digest has stayed unchanged for 3 s (`enrolledQuietReview`):
   `TestEnrolledProcessConflictResolution` (`5e7e8ccb`),
   `TestEnrolledClusterProcessesPromoteAndRejoin` and
   `TestV01BinaryRefusesEnrolledDirectory` (`e57d9a64`). This only reads
   and the activation is still sent once, but it does not prove that a
   ready node accepts writer commands; it only makes the refusal unlikely,
   and the 3 s is sized from a 1.75 s gap measured locally under race, not
   on a hosted runner.

Evidence under `.context/v020-final-20261110/evidence/CI-RED-3/` (scripts and
overlays in `scripts/`):

- `nul03-corpus-before-fix.log`: the corpus input fails before the fix
  (`domain filter "*\x00" returned ...`). `nul01`, `nul02`: `./internal/api`
  and `./internal/querylog/sqlitelog` pass. `nul04-race-packages.log`: `-race`
  over `./internal/querylog/...`, `./internal/api`, `./test/fuzz`: ok.
  `fuzz01.log`: `FuzzQueryLogFilter` fuzzed for 2m31s, 192,970 execs, PASS.
- `lease01-before.log`: 16 of 200 runs fail under `taskset -c 0,1 -race`.
  `lease03-diag-entered-hop.log` (diagnostic overlay
  `scripts/lease-diag-overlay.diff`): the entry happened 11 of 200 times, and
  each one was refused as `incarnation fenced`. `lease02` (retained) is a
  failed overlay build. `lease04-after.log`: 200 of 200 pass with the same
  pinning. `lease05-package-race.log`: the package with `-race`: ok.
- `tsig01-diag/`: 20 of 20 local passes of the hosted test with markers. The
  hosted failure did not reproduce locally. `tsig02-badsig-after-commit.log`
  (non-race): on a quiet node, 0 of 20 BADSIG queries unanswered; right after
  a signed UPDATE, 8 of 30 unanswered. `tsig07-dnsserver-refusal-binds-before.log`:
  new `TestCapturedTSIGRefusalBindsOutput` without the fix: no answer on
  UDP, TCP, DoT, DoH, DoH3 and DoQ. `tsig06-...-after.log`: 3 of 3 pass.
  `tsig05-dnsserver-race.log`: `./internal/dnsserver` with `-race`: ok (65 s).
  `tsig08-root-refusal-served.log`:
  `TestRootAnchorTSIGRefusalFollowsTheServedPublication` and
  `TestRootAnchorServedDNSFollowsThePublication`, 3 runs each: ok.
  `tsig09-mounted-after-fix.log` (`-race -count=8`): the new mounted refusal
  test, the hosted test and the root output test pass 8 of 8 each. All 288
  refusals sent after a signed UPDATE were answered. `tsig03` (retained,
  prefix overlay) and `tsig04` (retained): an earlier form of the mounted
  test failed on UPDATEs that got no answer within 5 s, not on refusals.
  Under race on a loaded machine the mounted test does not tell before from
  after (it passed 32 of 32 refusals before the fix). The dnsserver test is
  the deterministic proof.
- `e2e01-diag-source-timeline.log` (overlay `scripts/enrolled_conflict_diag_test.go`):
  in 1 of 8 runs the reviewed source changed 230 ms after the review while
  the zone and record set RVs stayed the same.
  `e2e02-diag-inprocess-zone-commits.log` (overlay
  `scripts/authority_diag.go`, test `scripts/zz_cired3_zonecommits_test.go`):
  shows the commit cascade with its callers. In 3 of 6 runs the record set
  POST right after the zone POST was itself refused 503. `e2e03-conflict-after-fix.log`:
  `TestEnrolledProcessConflictResolution` 3 of 3 PASS (86 s, 65 s, 65 s).
- After review (`fix-*`, scripts and overlays in `fix-scripts/` and
  `fix-prefix/`):
  - `fix-01-e2e-rate-quiet-review.log`: `TestEnrolledProcessConflictResolution`
    with the quiet review, one race test binary and one process per run,
    `taskset -c 0,1`, `GOMAXPROCS=2`: 20 of 20 PASS (71-85 s).
    `fix-02-e2e-rate-immediate-review.log`: the same with the review taken
    immediately (the pre-`5e7e8ccb` test, overlay), `taskset -c 2,3`: also
    20 of 20 PASS (71-88 s). So the local rate does not discriminate: the
    stopgap has no local before/after proof, and the hosted 503 did not
    reproduce in 40 pinned runs.
  - `fix-03-lease-held-hop.log`: the new lease test with `sourceLifetime`'s
    `s.lease.Err()` check removed (overlay) fails 3 of 3 (the probe entry
    is committed); on HEAD, the new test and
    `TestActualSQLiteSourceWaitsWithOriginalCancellationAndWriterToken`
    pass 200 of 200 each under `-race`, pinned with `taskset -c 4,5`
    (the log's header line says 0,1; the command ran on 4,5), 234 s.
  - `fix-04-root-refusal-strict-classes.log`: with the served class
    widened to every opcode and to transfers (overlay) the extended
    `TestRootAnchorTSIGRefusalFollowsTheServedPublication` fails at UPDATE;
    on HEAD it and `TestRootAnchorServedDNSFollowsThePublication` pass 3
    of 3 (race). `fix-06-root-refusal-transfer-mutant.log`: with only the `o.query` check dropped (transfers in the served class) it fails at AXFR.
  - `fix-07-e2e-other-sites.log` (`2ee92e93`, race, `taskset -c 0-3`): `TestEnrolledClusterProcessesPromoteAndRejoin` PASS (222 s), `TestEnrolledProcessConflictResolution` PASS (71 s), `TestMixedVersionStaticPeerV01` PASS (127 s), `TestMixedVersionCentralDatabaseV01` SKIP (no PostgreSQL). `fix-08-e2e-v01-site.log`: `TestV01BinaryRefusesEnrolledDirectory` PASS (55 s). One run each, not a rate.
  - `fix-05-lint.log`: golangci-lint v2.15.0 over `./ee/integration/host/...`, `./test/e2e/...` and `./cmd/kudurru/...`: 0 issues; bench guards 14 OK. `gofmt -l` is empty and `go vet` is clean for the touched packages. No generated artifact changed.
- `lint01.log`, `lint02-dnsserver.log`, `lint03-touched.log`: golangci-lint
  v2.15.0 over `./internal/dnsserver/...`, `./cmd/kudurru/...`,
  `./test/e2e/...`, `./internal/querylog/...`, `./internal/api/...` and
  `./ee/integration/host/...`: 0 issues. `bench01.log`: bench guards OK.
  `gofmt -l` is empty. No generated artifact changed.

What remains open: the product behavior behind failure 4 (writer commands,
including activation, refused 503 during the node's own post-write
cascade) needs the decision CI-RED-2 proposed; until then the e2e quiet
wait is a stopgap at three sites, not a readiness proof, and no committed
test sends a command during that cascade and requires success (such a test
would fail today). The PostgreSQL "server error" for a NUL filter is a
reading, not a test, and the fuzz run was not repeated. The mounted TSIG
refusal test still only sometimes fails without the fix (1 of 4 non-race in
the review); the deterministic proofs are `TestCapturedTSIGRefusalBindsOutput`
and the root output test. The unbound `servfail(r)` that `Handler.resolve`
returns on a pipeline error has the same shape as failure 3 and was not
changed. Under race, signed UPDATE acknowledgments sometimes go unanswered
(2 of 288 in `tsig09`). Those items and the measurements for item 4 are in
`out/CI-RED-3/for-core.md`. The integrator's full `./cmd/kudurru` run was
not done here.
