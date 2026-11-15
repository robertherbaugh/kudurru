### CORE-COMMIT-GAP zones keep answering across canonical commits — November 13, 2026 (D395)

**Answer to the integrator's question.** The QUAL-OPEN3 measurement
(`fix-measure03-release.log`) ran at `d2039002`, a commit that does not contain
D378 (`10d3ab5b`, "answer local data from the served publication"). That change
already stopped an unrelated commit (a BlockList create or delete) from leaving an
authoritative zone unanswered: the same QUAL-OPEN3 harness at the lane head
`cd0ee44f` showed no gap in either unrelated-BlockList window beyond one 8 ms
probe (`measure01-head-norace.log`). The draft's explanation (answers of a
superseded publication withheld by design) was half right. Two causes were
still there at the lane head, and both are now fixed:

1. **SERVFAIL after a zone's own write.** A write of a zone's own `DNSZone`
   (query ACL, name servers, selector) made every publication serve that zone as
   committed-unavailable coverage until the zone driver had refreshed the
   lifecycle receipt and, for a primary, committed a head of the new
   configuration. Primary and forward zones answered SERVFAIL for up to 320 ms
   and 302 ms (`measure01`). Now `PreparedImage.Serves` keeps the same zone (same
   name, UID, apex and type, a source that is not removed, an active lifecycle
   and head) in DNS routing through that gap. A primary answers its last
   committed head under the query access mode and ACL of the new configuration
   (`zone.RetainCommittedPrimary`), with zone transfers refused. A nonprimary
   answers its new configuration, which is what its model is always built from.
   `PreparedImage.Admitted` keeps the strict meaning, so UPDATE, NOTIFY, status
   projection, publication acknowledgment and integrations still wait for the
   driver. A deleted, replaced or retyped zone, a head that is not active, and
   expiry still get coverage. A child's own write no longer unbinds its parent.
2. **No reply for a query in flight across a publication swap.** The check that
   withheld the answer is `rootAnchorOutput.Emit` →
   `recursiveRootSeal.servedCurrent` → `Publisher.Serves`. A query captured on
   publication N whose output came after N+1 was swapped in failed that check
   with `serving: dependency changed`, and the UDP listener sent nothing. Shown by
   temporary instrumentation of every refusal point (`gapdbg-instrumentation.patch`,
   never committed): `emit-refused ... current=false dnsCurrent=true
   servesNow=false` followed by `write-failed` in `gap08` (grace removed) and in
   the before-variant of `gap10`. Now `Publisher.ServedWithin` also recognizes the
   last eight replaced publications for one second (`servedSuccessorGrace`) after
   the swap that replaced each, however many swaps followed. The record holds
   weak pointers only. It is written under the publication lock *before* the
   pointer swap, so a reader that loads the current pointer first always finds
   a leased publication either still current or already retired. (The first
   version wrote it after the swap; a reviewer's spinning-reader probe found
   that leftover window refusing a leased publication on about one swap in
   four, `review-08`/`review-09`.) A DNS-only (fenced) successor clears the
   record before it is swapped in, and `Close` clears it.

**Safety.** Every answer sent under these rules is committed data. `Capture`
only returns the served publication, so a query in flight was received while its
data was current. The one-second bound limits staleness; it is not a window of
silence. No query captured after a swap can use the old publication, and an
answer from a replaced publication is sent at most one second after that
publication was replaced. Nothing is answered across a fence. The DNS dependency
installations (keyring, transferred and catalog zone data, root lifetime) and
the anchor input are still checked at the handoff. Transfers, UPDATE
acknowledgments, API dig, cache insertions and native resolver mutations keep
full data currency. A tightened query ACL applies from the first publication
after the write, as the coverage did before.

**The 1.39 s gap of CORE-OPEN3 `run05`** (TestRunServeSQLiteLongApplyKeepsServingDNSAndReads,
no SERVFAIL): its cause is **not determined**, and it was not reproduced. The
long-apply probe uses a 200 ms timeout and sleeps 50 ms between queries
(`cmd/kudurru/serve_apply_multi_mount_test.go`), so one withheld query makes a
gap of about 0.3 s; 1.39 s needs five or six consecutive unanswered probes. The
withheld-answer mechanism was measured at about one query in a few thousand per
swap, and the only refusal caught in the instrumented control (`gap10`, the
before-variant) produced a 313 ms gap, one lost probe. Five or six independent
hits in a row is not a credible explanation; a server-side stall of more than
200 ms per query is at least as likely, and the one-second grace would not fix
that. After the change, 10 instrumented long-apply runs (`gap09`), 6
interleaved ones (`gap10` B) and 5 final ones (`gap11`) logged no refusal, and
the longest DNS gap was 238 ms, under load averages of 10 to 32. A follow-up
should capture it on an idle machine with the refusal instrumentation
(`gapdbg-instrumentation.patch`) plus per-query latency.

Evidence under `.context/v020-final-20261110/evidence/CORE-COMMIT-GAP/`:

- New mounted test `TestRunServeSQLiteZonesAnswerAcrossCommits`
  (`cmd/kudurru/serve_commit_gap_test.go`): a complete SQLite `runServe`, and
  direct UDP probes every 5 ms for a primary zone name and a forward zone name
  (upstream on loopback) while the test makes 32 writes: four rounds of the
  primary's query ACL on and off, a change of its name servers, its record set
  to a new address, the forward zone's query ACL on and off, and an unrelated
  BlockList created and deleted. Without the race detector every probe must get
  committed data (no SERVFAIL, no silence, no uncommitted address), and once a
  new address has been seen published and one second (`servedSuccessorGrace`)
  has passed, no probe sent after that may get an older address. Under the race
  detector SERVFAIL and wrong answers still fail and silent probes are bounded
  by one per commit. `hack/go-test.py` now runs it a second time without the
  race detector (`NORACE_TESTS`).
  - Before the fix (`-overlay overlay-before.json`, the lane-head versions of
    `image.go`, `membership.go`, `zones.go`, `serving_runtime.go`): `gap04`, 2
    of 2 runs fail with 254 and 378 probes answered SERVFAIL for the primary
    zone.
  - With the zone fix but without the grace (`overlay-nograce.json`): `gap06`
    2 of 2 fail (3 silent probes each), `gap08` 1 of 1 fails (1 silent probe,
    instrumented as above).
  - After the fix, no race detector: `gap05` 2 of 3 pass (1 silent probe, with
    the earlier immediate-predecessor-only grace; cause not captured), `gap07`
    3 of 3 pass, `gap09` 3 of 3 pass, and with the final code `gap11` 5 of 5,
    `gap12` 3 of 3 and `gap13` 3 of 3 pass: 0 of about 7,000 to 8,500 probes per
    name per run without a correct answer.
  - After the fix, race detector: `race04-zones.log` 3 of 3 pass, 0 of about
    12,300 to 13,500 probes per name without a correct answer (1 and 2 setup API
    requests asked again after 503 `generation_pending`). `race02` and `race03`
    are retained failures of earlier test versions: their setup GET or PUT got
    503 `generation_pending` under the race detector and the test did not ask
    again. Not a DNS failure; fixed in `69cc3aa1`.
- Review fixes (after the first acceptance review), with the final test:
  `fix-04` (with a child zone, see below) 3 of 3 and `fix-14` 3 of 3 and
  `fix-16` 2 of 2 pass without the race detector (0 of about 9,000 to 10,000
  probes per name without a correct answer); `fix-17` 3 of 3 pass under the
  race detector (0 of about 15,000 per name; 1 to 4 setup requests asked again
  after 503).
- `TestPublisherServedWithinHasNoGapAtTheSwap` (`internal/serving`): a lease
  holder spins on `ServedWithin` with a one-hour grace across 400 swaps.
  `fix-01` passes (386 swaps seen, 0 refusals). With the previous order (record
  written after the swap, `fix-overlay.json`), `fix-02` 2 of 2 fail with 145
  and 109 refused swaps out of 391 and 392. `fix-03`: `internal/serving` under
  the race detector passes.
- `TestRunServeSQLitePrimaryAnswersAcrossStatusWrites`: its status read now asks
  again for up to 30 s after 503 `generation_pending` (the transport gave up
  after 5 s once in the reviewer's `review-02`). Under the race detector
  `fix-06` and `fix-15` 6 of 6 pass with 0 probes unanswered within 200 ms and
  0 or 1 status reads asked again.
- QUAL-OPEN3 harness (`fix-measure-window.patch`, applied, never committed), no
  race detector: `measure01-head-norace.log` at the lane head (primary zone
  SERVFAIL up to 320 ms, forward zone up to 302 ms after their own writes) and
  `measure02-final-norace.log` after the fix: 0 SERVFAIL and 0 no-response in
  every one of the 16 windows (about 3,000 probes each).
- `TestPublisherServedWithinKeepsRecentlyReplacedPublications`
  (`internal/serving`): grace counted from each publication's own replacement,
  two swaps old still admitted, past the grace refused, older than the record
  refused, across a DNS-only fenced publication refused, after `Close` refused.
  Checked to fail with the DNS-only clearing removed and with a record depth of
  one.
- Unit and package tests updated to the new rule: `TestRootAnchorServedDNSFollowsThePublication`
  (inside the grace a query in flight is answered and a transfer is still
  refused; after it, refused as before), `TestRootAnchorRetainedDNSDoesNotGrantDataOrCacheMutation`,
  `TestImageStatusActualSourceAdvancedOldHeadAdmissionWindow`,
  `TestImageReadinessActualParentConfigGapClosesPrimaryAndNonprimaryChildren`,
  `TestImageReadinessStatusWriteKeepsCommittedAuthority`,
  `TestActualNonprimarySourceMutationMissingWitnessAndPreparing`,
  `TestPrimaryPublicationStopsWaitingForChangedDesiredSource`, and the primary
  status feedback fixture.
- Race runs: `light01`/`light03` (`internal/serving`, `internal/zone`,
  `internal/servingzones`) and `light02`/`light04` (`internal/primaryruntime`
  75 s, `internal/servingbuild` 229 s) all pass. `race02-cmd-selection.log`:
  every `TestRootAnchor*` test, `TestRunServeSQLitePrimaryAnswersAcrossStatusWrites`,
  `TestRunServeSQLiteCompletePrimaryUpdateRestart` and
  `TestRunServeSQLiteLongApplyKeepsServingDNSAndReads` pass under the race
  detector (the gap test's failure there is the setup 503 described above).
- Lint: `fix-07` and `fix-19` (`./internal/serving/...` and
  `./cmd/kudurru/...` after the review fixes), 0 issues. `lint02` (the five internal packages and `./cmd/kudurru/...`) and
  `lint03` (`./cmd/kudurru/...` after the last test change), 0 issues. `lint01`
  reported one errcheck issue in the uncommitted QUAL-OPEN3 measurement file
  only. Bench guards: 14 tests OK.

**Still open.** A new defect found while adding a parent/child pair to the
mounted test, not fixed here and not caused by D395: on a mounted node a
primary child zone under a primary parent can leave one of them SERVFAIL for
good, readiness reporting `zone awaiting committed publication: <apex>`. A
child created after its parent was answering stayed unanswered for six minutes
in 3 of 5 runs (`fix-13`, overlay `fix-overlay-latechild.json`; it either
answers within about 0.9 s or never), and in `fix-09`/`fix-11`/`fix-12` 3 of 6
runs at the 60 s setup bound. Created together, the parent never answered
within 60 s in 4 of 6 race runs (`fix-06` 1 of 3, `fix-15` 3 of 3) while 3 of
3 runs without the race detector passed (`fix-04`). The lane-head code without
D395 (`overlay-before.json`) does the same: `fix-08` 1 of 1 without the race
detector (child created later) and `fix-18` 2 of 2 under it (created together,
`fix-overlay-before-child.json`). The likely area is the driver's late-child
admission (`AdmitAncestor` and the parent fence in `primaryruntime/driver/work.go`,
`c.ready[ancestor] = false` in `primaryruntime/image.go`); it needs its own
package. The mounted commit-gap test therefore has no parent/child pair:
parent/child behavior across each other's writes, a forward zone changing its
upstream group, and a tightened ACL refusing from the first publication are
still shown only by `internal/primaryruntime`, `internal/servingbuild` and
`internal/servingzones` tests.

The silent-probe count of the status-write test under the race detector (0 to
10 per run, before and after D395, reviewer's `review-06`/`review-07` and
`fix-06`/`fix-15`) was not compared in a larger before/after control; the
samples are too small to attribute it either way.

`TestRunServeSQLiteLongApplyKeepsServingDNSAndReads` should be tracked as its
own package (CORE-APPLY-MULTI / D378 area); D395 does not resolve it. It fails
without the race detector on this shared machine on its *read* assertion
(`generation_pending` refusals of a second caller during the long apply; 0
SERVFAIL, DNS gaps at most 238 ms): `gap09` 10 of 10 at load 21 to 32, `gap11`
2 of 5 at load 5 to 15. The interleaved control `gap10` (before the fix: 3 of 6
fail; after: 3 of 6 fail) shows it does not come from this package. The apply
itself takes 9 to 69 s there against 4 to 5 s in the CORE-OPEN3 runs. This is
the CORE-APPLY-MULTI or D378 area and needs its own look on an idle machine. The
grace is one second, and a query whose processing outlives that (more than one
second between capture and output) is still withheld. The measurements are on
mounted `runServe` test binaries with `CGO_ENABLED=0` on 8 shared CPUs, not on
the packaged release binary. The live dnsdist suites (QUAL-OPEN3) were not run
again. Their harness waits stay as they are, but they should no longer be
needed for the reasons D383 gave.
