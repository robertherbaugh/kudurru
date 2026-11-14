### CORE-RETAINED-SERVFAIL Recursive answers and upstream failures across a canonical commit — November 10, 2026 (D377)

Lane `wip/v020/core`, base `c781c5d6`. Source commits `f50cccb3`
(`fix(dns): resolve recursive lookups across a commit`), and after review
`08749fc7` (`fix(recursor): count an asked authority as contacted across
failures`) and `a09f876d` (`docs(dns): describe node refusals under the
served-publication policy`). No generated file, no
new dependency, `go.mod` unchanged. The unnumbered QUAL-CI decision row
(`out/QUAL-CI/decision.md`, commit `ae923e87` on this lane) is superseded by
D377: its first part is D377 part (1) with the narrower class.

What now happens:

- **A recursive node keeps answering while configuration is committed.** On a
  node whose default group is `mode: recursive`, a cache-miss lookup that
  arrived in the 60 to 90 ms between a canonical commit (an API token created
  through the admin socket, any object write) and the next publication was
  refused before recursion started: `captureRootNativeSource` required full
  data currency on the query's lease, although the recursive answer itself is
  retained across ordinary commits like a forwarded one (D332). Before
  `ae923e87` that refusal was dropped at output (no answer, the client's retry
  succeeded); after it, it was sent as an immediate SERVFAIL with extended
  error 23 although no authority had been asked. Native recursion now starts
  and takes each per-acquisition cache attempt under the retained recursive
  policy (`recursiveRootSeal.dnsCurrent`). The native cache still needs full
  data currency for its writes: after an ordinary commit an attempt is consumed
  without writing, exactly as a superseded raw-cache fill already was, and the
  resolution continues and answers. The answer is the same, but the part of
  the lookup that is not cached costs more authority queries, since
  delegations, DS and DNSKEY records learned during it are not remembered
  (measured below). Anchor retirement, a first managed
  acceptance and the loss of an installed DNS dependency still refuse
  recursion (`TestRootNativeActualCacheRetirement` unchanged and passing).
- **Only a failure of an upstream that was really asked is retained past a
  published successor.** `retainedDNSResponse` releases `servfail_upstream` and
  `timeout_upstream` only with origin unattributed and the new internal
  `pipeline.Response.UpstreamContacted` (besides the existing shape checks:
  SERVFAIL, AA clear, no answer or authority records, at most OPT in the
  additional section). Only the upstream stage sets that field, from
  `upstream.Error.Attempted`, which the group exchange sets when it collected
  the outcome of a server attempt that was not a native-resolver admission
  refusal (`recursor.IsAdmission`, a wrapper that keeps the original error and
  text). The resolver records in its per-client state when it hands a query to
  the exchanger, and marks an error as an admission refusal only when no
  authority was asked, so a deadline noticed at a cache commit after the
  authorities answered counts as contacted, like the same deadline noticed
  while asking (`08749fc7`). `server.exchange` keeps the mark when the parent
  context has ended, so a refusal that coincides with the end of the query is
  not counted as asked. Node-produced refusals are not in the retained class:
  a failed serving capture in `Stage.Bind`, a refused native recursion
  (`CaptureNative`) or cache admission, a query that ended before any server
  was tried, indeterminate DNSSEC validation (`dnssec` stage, origin failure),
  failed rewrite targets and stub-zone or other failure-origin answers. On this
  base every other ordinary query response follows the served-publication
  policy of D378 (`servedDNS`, `recursiveRootSeal.servedCurrent`), so such a
  refusal is still sent between a canonical commit and the next publication,
  as a local answer is, and is withheld only once a successor is published.
  A contacted upstream failure is still sent after that. The user-visible
  effect of the narrowing is therefore limited to queries held on a lease
  whose successor is already published, and it only corrects the unreleased
  QUAL-CI behavior; no separate changelog bullet is needed beyond QUAL-CI's
  ("a query whose upstream fails or times out"). `pipeline.Response` stays 248 bytes (the bool fills padding after
  `Origin`), and the field is written only on the upstream stage's failure path,
  so the query path's allocation profile is unchanged.
- **Docs.** `docs/architecture/query-path.md` describes the exact retained
  class, that authoritative, local and mixed answers and node-produced
  refusals follow the served publication (D378), and the retained native
  recursion with its extra authority queries; the matching sentences in
  `upstreams-and-cache.md` and `configuration-and-store.md` now say that
  superseded native writes are skipped, not that the resolution fails.
- **Cleanup.** `rootAnchorInsertion.retainedDNS` was true at both
  construction sites; the field is removed and `Apply` always checks under
  `dnsCurrent` (writes still need `seal.current()` inside the held read).
- **One existing assertion changed on purpose.**
  `TestRootAnchorRetainedDNSDoesNotGrantDataOrCacheMutation` asserted that a
  native mutation attempt captured before an ordinary commit fails when applied
  after it. It now asserts that the attempt is consumed without running its
  writes (the callback still fails the test if it runs) and cannot be reused,
  and that native recursion and a cache attempt captured on the retained lease
  after the commit are admitted with their writes skipped. Its `failed target`
  row (origin failure, `servfail_upstream`), added by `ae923e87`, is now
  refused.

Evidence under `.context/v020-final-20261110/evidence/CORE-RETAINED-SERVFAIL/`:

- `e2e01-before.log` (retained failure, the reproduction): the lane head
  `c781c5d6` with only the new test added. `TestRecursiveAnsweredAcrossCommits`
  FAIL in 2.52 s: `commit 1 of 5, query 21: SERVFAIL EDE 23 with 0 answers,
  want NOERROR with 192.0.2.77 (stand-in root asked: false)`.
  `TestUpstreamFailureAnsweredAcrossCommits` PASS (4.00 s, 972 queries
  answered SERVFAIL across 5 commits).
- `e2e02-after.log`: the uncommitted fix on `c781c5d6` (identical to `f50cccb3`): `TestRecursiveAnsweredAcrossCommits` PASS 3.88 s (917 cache-miss lookups answered NOERROR across 5 commits), `TestUpstreamFailureAnsweredAcrossCommits` PASS 4.87 s (1025 SERVFAIL); `TestCentralQuerylogTwoNodes` skipped there (no PostgreSQL DSN), run in `e2e03`.
- `e2e03-after-postgres.log`: `f50cccb3`, PostgreSQL 17 started natively on a private loopback port, `KUDURRU_REQUIRE_POSTGRES=1`, `-count=3`: 9 pass, no skip, 59.5 s. `TestRecursiveAnsweredAcrossCommits` 4.92/3.91/3.80 s (949, 911, 952 lookups NOERROR), `TestUpstreamFailureAnsweredAcrossCommits` 4.91/4.90/3.89 s (985, 987, 977 SERVFAIL), `TestCentralQuerylogTwoNodes` 9.81/10.06/10.04 s.
- `race01-cmd-anchor.log`: `go test -race` of ten anchor/native/retained/served
  tests in `./cmd/kudurru` (including
  `TestRootAnchorRetainedDNSDoesNotGrantDataOrCacheMutation` with the new
  actual-stage rows, `TestRootNativeActualCacheRetirement`,
  `TestRootNativeOriginalLifetimeAndForeignDomain`,
  `TestRunServeDirectAnchorsActualRecursiveDNSKEYAndRestart`): 10 pass, 25.0 s.
- `race03-cmd-anchor-wide.log`: `go test -race` of every test function in
  `cmd/kudurru/serving_anchor_*_test.go`, `serve_anchor_*_test.go`,
  `anchors_test.go`, `serving_update*_test.go` and
  `reload_recursive*_test.go`: 91 pass, 1 skip (the opt-in
  `TestRunServeManagedEdgePinnedDaemonRestart`, needs
  `KUDURRU_ANCHOR_EDGE_LIVE=1`), 232.0 s.
- `race02-light.log`: `go test -race ./internal/recursor ./internal/upstream
  ./internal/stages/upstream`: all ok (includes the new
  `TestFailureUpstreamContacted`: forwarder failed, query ended before any
  server, serving capture failed, native recursion refused, native cache
  admission refused, native authorities failed).
- `unit03-pipeline-stages.log`: `go test ./internal/pipeline
  ./internal/stages/... ./internal/dnsserver`: all ok, including the
  allocation guards of `internal/stages`.
- `lint01.log`: golangci-lint v2.15.0 on `./cmd/kudurru/... ./internal/upstream/... ./internal/recursor/... ./internal/stages/upstream/... ./internal/pipeline/... ./test/e2e/...`: 0 issues. `gofmt -l` on the touched packages is empty; `go vet` of the touched packages passes.

After review (`08749fc7`, `a09f876d`):

- `fix-light00-before.log`, `fix-light02-before.log` (retained failures):
  `TestFailureUpstreamContacted` with the three new rows on `f50cccb3`'s code.
  `native cache admission refused as the query ended` failed (`wrong contact
  evidence true`, the dropped admission mark); with the group timeout at
  300 ms, `native cache commit reached the deadline after asking` failed
  (`wrong contact evidence false`). (In `fix-light00` that row used the query
  deadline, which the dropped mark hid, so it passed there.)
- `fix-light03-after.log`: `go test -race -run TestFailureUpstreamContacted
  ./internal/stages/upstream` with `08749fc7`'s code: 9 subtests pass, 0.81 s.
- `fix-light06-skipped-writes-cold.log`: `TestNativeMutationSkippedWritesStillResolve`
  (`internal/recursor`, DNSSEC on, default QNAME minimization, the signed
  `newWorld` harness, a source whose attempts are consumed without writing):
  16 of 16 names (delegations, CNAME and DNAME across zones, wildcards,
  glueless name servers, insecure delegations, NSEC and NSEC3 denials) give
  the same response code, security state and answers as a cold normal
  resolver, and nothing reaches the native cache. Authority queries, cold
  normal vs skipped: www.example.com A 7 vs 18, MX 7 vs 32, alias.example.com
  7 vs 32, far.example.com 11 vs 39, nope.example.com 7 vs 46, x.wild.nsec3.org
  8 vs 47, nope.nsec3.org 7 vs 60, www.glueless.org 10 vs 26. The
  test fails if a skipped lookup needs more than 90 of the 120-query budget.
- `fix-race01-light.log`: `go test -race ./internal/recursor ./internal/upstream
  ./internal/stages/upstream`: all ok.
- `fix-race02-cmd.log`: `go vet ./cmd/kudurru`, then `go test -race -run
  'TestRootAnchorRetained|TestRootNative|TestRootAnchorServed|TestRunServeDirectAnchorsActualRecursiveDNSKEYAndRestart'
  ./cmd/kudurru` on the final code: 6 tests (59 including subtests) pass,
  14.7 s, among them `TestRootAnchorServedDNSFollowsThePublication` with the
  new rows (an uncontacted refusal, a failure-origin refusal with and without
  contact, and a contacted upstream failure: all sent after the unpublished
  commit; after the successor publication only the contacted upstream failure
  is sent) and `TestRootAnchorRetainedDNSDoesNotGrantDataOrCacheMutation`.
  These rows pin the documented behavior of the base; they would pass on
  `f50cccb3` too, since that part of the finding was wording.
- `fix-e2e01.log`: `KUDURRU_E2E=1 go test -run
  'TestRecursiveAnsweredAcrossCommits|TestUpstreamFailureAnsweredAcrossCommits'
  ./test/e2e` on `a09f876d`: both pass (4.81 s, 1005 lookups NOERROR; 4.82 s,
  1021 SERVFAIL across 5 commits).
- `fix-unit01-pipeline-stages.log`: `go test ./internal/pipeline
  ./internal/stages/... ./internal/dnsserver`: 13 packages ok, including the
  allocation guards.
- `fix-lint01.log`: golangci-lint v2.15.0 on `./cmd/kudurru/...
  ./internal/upstream/... ./internal/recursor/... ./internal/stages/upstream/...`:
  0 issues. `gofmt -l` on the touched packages is empty.

What remains open: the window is closed for ordinary commits, but not for a
commit whose successor publication replaces an installed DNS dependency of the
old lease (for example a settings change that rebuilds the upstream set) or
retires the anchor input. A recursive or forwarded query held on the old lease
then is refused at output as before (no answer; the client retries), and its
node-produced refusal is not sent. The extra authority queries of a lookup with skipped
writes are measured only in the recursor's simulated world; the mounted
`TestRecursiveAnsweredAcrossCommits` still runs with DNSSEC and QNAME
minimization off and a one-hop stand-in root. A native resolution that
returns a SERVFAIL message (not an error) without sending a query, for example
a cached failure, counts as contacted, as before. The fixed-class
benchmark gates (F20d, F63) were not rerun; the change adds no allocation and
keeps `pipeline.Response` at 248 bytes. The full `./cmd/kudurru` package was not
run here (the integrator runs `hack/go-test.py`). `TestCentralQuerylogTwoNodes` was not rerun after `08749fc7` and `a09f876d` (no PostgreSQL in the review-fix run); its last pass is `e2e03` on `f50cccb3`.
