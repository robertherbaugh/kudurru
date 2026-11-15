### CORE-OPEN3 Lifecycle SERVFAIL window, repeated whole-image work and the follower cut — November 10, 2026 (D378)

**The lifecycle SERVFAIL window.** During a long apply the zone sometimes answered
its own committed records with SERVFAIL for up to 0.8 s. This happened when a
publication landed while the primary zone's lifecycle receipt still named the
DNSZone's previous resourceVersion. That is the case CORE-STATUS-WRITE-SERVFAIL
fixed at its cause in 56637cec (`imageServingSource`): every commit is followed by
the status write of a new serial, and a publication built in between now serves
the same head instead of committed-unavailable coverage. CORE-APPLY-OPEN measured
the 0.8 s on an older tree, before 56637cec. This package checked that this was
the same cause. Without the race detector, at HEAD, all 8 runs of the mounted
long-apply test stayed under 77 ms. With the pre-56637cec `image.go` put back
through `-overlay`, 2 of 8 runs went back to 0.30 s and 0.66 s gaps (`run02`,
which predates the SERVFAIL counter and so shows only gaps). The reviewer's
control with the counter (`review-01-overlay-control.log`) failed 4 of 8 runs
on the new SERVFAIL assertion, with 3 to 12 SERVFAIL answers. The mounted
test `TestRunServeSQLiteLongApplyKeepsServingDNSAndReads` now fails on any
SERVFAIL for the existing record, so a regression shows up even when the window
is shorter than the gap bound. It keeps the one-second gap bound. When that bound
fails, the test now lists what each probe in the gap got (timeout, other rcode or
answer) and when.

**Repeated whole-image work.** I took a CPU profile of the 120-document apply
under the race detector (`prof01`). It shows each reader of one generation
sealing and validating the same complete image again: the publisher's candidate,
the provider coordinator, the primary driver, the TSIG and integration runtimes
and each command's commit. Most of the time attributable to `replication` went
to canonical JSON rebuilt by `Digest` and `Decode` and to each object's identity
decoded by `Seal` and `validate`. Those are pure functions of their exact input
bytes, so `internal/store/replication/memo.go` now remembers them by SHA-256:
- the payload digest, keyed by the encoding it hashes;
- each object's key and header, keyed by its document (identities of at most
  4 KiB; at most 131,072 entries and 32 MiB, so an image of the default
  100,000 objects fits);
- the canonical form of the last two complete images of at most 8 MiB, keyed
  by a fixed-width depth bound followed by the image bytes.

Errors are never stored, and a hit returns what the uncached function returns
for the same bytes and depth. Each cache is bounded by entry count and by
bytes, and evicts random entries when full, so a store with more objects than
the bound still gets a share of hits on repeated passes. Identities are
remembered before their image is validated, so objects of a rejected image
can be remembered too, within the same bounds. Review found three defects in
the first version, fixed in a later commit: the identity memo was bounded by
entry count only (four 1 MiB names held 8 MiB), clearing it when full gave a
store above 32,768 objects no hits at all, and the canonical key hashed the
decimal depth before the bytes, so (6, `4{...}`) collided with (64, `{...}`)
and a hit could bypass a smaller depth bound. Owner callbacks (store object validation, servingbuild schema checks,
runtime owners) and the decoded image are not cached, so each owner still runs
its own checks. In the second profile (`prof02`), `Seal` fell from 18.6 s to
9.6 s, `objectKey` from 7.8 s to 0.2 s and `Digest` from 18.4 s to 12.3 s, and
total CPU samples fell by 18 % (511 s to 419 s). Reads under the race detector
are still mostly refused (43 of 58 in `race04`, 55 of 68 before), because one
publication there still takes several seconds.

**The follower cut.** The enrolled-cluster test that sends a many-document file
through a follower now checks every refused document, not two samples. After the
request, and again after one more follower pull, both nodes must list exactly the
committed prefix of the file (`GET /objects/dnsrecordsets`, read in pages of
50 following `continue`). The first version read the whole list in one
request through the e2e helper, which reads at most 128 KiB of a body, so a
late cut (134 or 138 of 160) truncated it and both tests failed with
"unexpected end of JSON input" (`review-05-e2e.log`). The
document at the cut is allowed only when its outcome was `completion_unknown`.
Both nodes must answer NXDOMAIN, over UDP and TCP, for each refused document.

**Admission refused after an authority was asked.** `TestFailureUpstreamContacted`
gained the row "native cache admission refused after asking". Its source admits
the first cache attempt and refuses the later ones. The failure carries
`UpstreamContacted`, which pins the D377 behavior that the verifier noted.
`TestRootAnchorRetainedDNSDoesNotGrantDataOrCacheMutation` binds such a contacted
failure to an output before the managed anchor acceptance and requires `Emit` to
refuse it afterwards, so the retained class never outlives the anchor.

**Smaller items.**
- Stale comments: `pipeline.Response.UpstreamContacted` now states the
  "an authority was asked" criterion. The two `image.go` comments now say
  "outside the intent digest and the NS address dependency". The silent-probe
  bound comment in `serve_status_write_test.go` gives the observed 2 to 10 drops
  and says the bound is coarse.
- `.gitignore` now ignores `__pycache__/`.
- `docs/deploy/sqlite-cluster.md` gives the measured gaps: typically under
  0.1 s, an occasional single dropped query of about 0.3 s, and the one
  unexplained 1.4 s gap. Two comments in the long-apply test that still said
  one race-detector publication takes up to ten seconds, and did not say what
  a gap usually measures, were corrected after review.

Evidence under `.context/v020-final-20261110/evidence/CORE-OPEN3/`:

- `run01-norace-long-apply-head.log`: `go test -count=8 -run '^TestRunServeSQLiteLongApplyKeepsServingDNSAndReads$' ./cmd/kudurru` without race at e59af9be. 8 pass, 95.5 s. Gaps 54 to 76 ms, 0 reads refused.
- `run02-norace-long-apply-before-56637cec.log`: the same run with `-overlay overlay/before.json`, which puts back the `image.go` from before 56637cec. 8 pass, 96 s. Gaps 300 ms and 663 ms in 2 of 8 runs, about 55 ms in the rest. This run predates the SERVFAIL counter, so it shows only the gaps; the SERVFAIL control is `review-01-overlay-control.log` (the same overlay at the final test: 4 of 8 fail with 3 to 12 SERVFAIL, gaps 0.20 to 0.66 s).
- `prof01-race-long-apply.log`, `prof01-race-top-cum.txt`, `prof01-race-peek.txt`: race-detector CPU profile at HEAD. 1 pass. The apply took 2m18s, with 0 SERVFAIL, a 125 ms gap and 13 reads ok against 55 refused.
- `prof02-race-long-apply-memo.log`, `prof02-race-top-cum.txt`: the same profile with the memo. 1 pass. The apply took 2m10s, with 0 SERVFAIL, a 136 ms gap and 14 reads ok against 51 refused.
- `unit01-replication.log`, `unit03-replication.log`: `go test -race ./internal/store/replication`. ok, including the new `TestMemoizedDigestAndIdentityMatchUncached`, `TestMemoizedCanonicalImage`, `TestMemoBound` and `TestMemoizedDigestConcurrent`. `unit02-upstream-contacted.log`: `TestFailureUpstreamContacted`, 10 subtests pass.
- `race03-packages.log`: `go test -race ./internal/store/replication/... ./internal/store/sqlite/... ./internal/servingauth/...`. All ok (sqlite 321 s).
- `race04-cmd-selection.log`: `go test -race` over 27 tests of `./cmd/kudurru`: TestRootAnchorRetained*, TestRootAnchorServed*, TestRootNative*, TestServingItems* (15), the multi-document and multi-file apply tests, the long apply, TestRunServeSQLitePrimaryAnswersAcrossStatusWrites (45,342 answered, 0 SERVFAIL, 11 silent), TestRunServeSQLiteCompletePrimaryUpdateRestart, TestRootAnchorPreparationPublishesFencedDNSOnly and TestRunServeSQLiteStoredTSIGBackground{TCP,TLS}. 27 pass, 416 s.
- `e2e01-follower-cut.log`: `KUDURRU_E2E=1 go test -run '^TestEnrolledClusterProcesses(PartitionCatchupAndRestart|PromoteAndRejoin)$' ./test/e2e`. 2 pass, 279 s. 40 documents fit between two pulls. The 160-document file was cut after 44 and after 62 documents, and all 116 and 98 refused documents were checked on both nodes, twice. These early cuts kept the single-request list under 128 KiB.
- `review-05-e2e.log` (reviewer, at 98b8fc94): the same two tests. **2 of 2 failed** with "unexpected end of JSON input" after cuts at 134 and 138 of 160: the list exceeded the helper's 128 KiB read limit.
- `fix-01-e2e.log`: the same two tests with `-count=2` after the list was paged. 4 pass, 497 s. Cuts at 105, 125, 108 and 99 of 160; all 55, 35, 52 and 61 refused documents were checked on both nodes, twice.
- `run05-norace-long-apply-final.log`: 8 runs without race after the memo commit. **1 failure retained**: one run left the record unanswered for 1.39 s with 0 SERVFAIL and 0 reads refused (72 answers in 5.5 s, fewer than usual), on a shared, loaded machine. The test did not yet record what the probes got. The other 7 passed with gaps of 55 to 76 ms.
- `run06-norace-long-apply-diag.log`: 12 runs with the new diagnostics. 12 pass, gaps 54 to 81 ms.
- `run07-norace-long-apply-2cpu.log`: the compiled test binary pinned with `taskset -c 2,3`, 10 runs. 10 pass, gaps 109 to 145 ms, 0 reads refused.
- `race08-dependents.log`: `go test -race ./internal/primaryruntime/... ./internal/servingbuild/... ./internal/cluster/replica/... ./internal/pipeline/... ./internal/stages/upstream/...`: all 9 packages ok (servingbuild 147 s).
- `fix-02-memo-tests-before.log`: the new memo tests against the first memo version: `TestMemoizedCanonicalImageDepthIsSeparate` (depth collision returned `{"a":1}` with no error), `TestIdentityMemoSkipsLargeObjects` (a 1 MiB identity was remembered) and `TestMemoHitsBeyondBound` (0 hits of 1,300 on every pass with a bound of 1,000) all FAIL.
- `fix-03-memo-tests-after.log`: `go test -race -run 'Memo|Identity' ./internal/store/replication` after the fix: 8 pass; the 1,300-key loop gets 796 and 759 hits on passes 2 and 3. `fix-04-replication-race.log`: `go test -race -count=2 ./internal/store/replication`, ok twice.
- `fix-05-lint.log`: golangci-lint on `./internal/store/replication/...` and `./test/e2e/...`: 0 issues.
- `fix-06-norace-long-apply-loop.log`: `go test -count=30 -run '^TestRunServeSQLiteLongApplyKeepsServingDNSAndReads$' ./cmd/kudurru` without race at the final code. 30 pass, 322 s. 0 SERVFAIL and 0 reads refused in every run; gaps 52 to 64 ms in 28 runs and 301 ms (one dropped query) in 2.
- `fix-07-norace-long-apply-1cpu.log`: the compiled non-race test binary pinned to one CPU (`taskset -c 2`), `-test.count=15`. 0 SERVFAIL in every run; gaps 211 to 280 ms, so DNS stayed well under the bound even on one CPU. **1 failure retained**: one run had 1 read of the second caller refused (12 answered), and the message carried no stderr. The test now reports the CLI's error as well as its stderr, so a recurrence says why. Pinning to one CPU is harsher than CI; it is recorded, not explained.
- `fix-08-norace-long-apply-final.log`: the final test (with the read error reported), `-count=4` without race. 4 pass, 45 s. 0 SERVFAIL, 0 reads refused; gaps 54 to 55 ms and one 301 ms.
- `fix-09-lint-cmd.log`: golangci-lint on `./cmd/kudurru/...`. 0 issues.
- `lint01.log`: 1 issue (revive: `max` parameter in memo.go), fixed. `lint02.log`: 0 issues on replication. `lint03.log`: 0 issues over replication, pipeline, primaryruntime, stages/upstream, test/e2e and cmd/kudurru.

**What remains open.** Under the race detector, on a loaded machine, one
publication of 120 record sets still takes several seconds, so most reads by a
second caller during such a request are refused with `generation_pending`. What
is left is per-generation work: each new generation is canonicalized and digested
once, and each owner decodes it and runs its own object and entry checks. The
rest is race-detector overhead, and 62 % of the samples could not be attributed
to a Go stack. Removing more would mean caching validations that run owner
callbacks, giving one generation's reads a shared `CreatedAt`, or sharing one
validated image across owners. I rejected all three (D378). No mounted test
fails without the memo; it is covered by unit tests only, and its measured
effect on refused reads under the race detector (55 of 68 before, then 51 of
65, 43 of 58 and 37 of 50, one run each) is within run-to-run noise, so it is
CPU saved, not a proven reduction in refusals.

The one-second DNS gap bound is open as a known rare failure. One run
(`run05`) left the existing record unanswered for 1.39 s with 0 SERVFAIL and
0 reads refused. The cause is not known: it may be several consecutive
answers dropped by the node or a starved probe goroutine. It did not recur in
22 later runs by the implementer, 12 by the reviewer and 45 here (30 on the shared machine, 15 pinned to one CPU), about 1 in 100 runs overall. Gaps of about 0.3 s (one probe timed
out) appear in roughly 1 run in 15. On one CPU, one run in 15 refused one read
of the second caller without the race detector (`fix-07`); its cause is not
known either. If the 1.39 s case recurs, the failure
message lists what each probe in the gap got, which should tell the two
causes apart. The full `./cmd/kudurru` package
was not run here; the integrator runs `hack/go-test.py`.
