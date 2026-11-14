### CORE-STATUS-WRITE-SERVFAIL A primary zone answers across its own status write — November 12, 2026 (decision unnumbered)

The window existed before this package and still existed at 33ee04bc. Every head
commit on the complete SQLite primary runtime is followed by two more commits.
First the committed-status writer writes the new serial, which advances the
DNSZone resourceVersion. Then the driver realigns the zone lifecycle's source
receipt to that resourceVersion. A publication built between the two found the
lifecycle receipt different from the DNSZone (`imageCurrentSource` compared the
exact resourceVersion). It therefore served the zone as committed-unavailable
coverage, so the zone's own committed names answered SERVFAIL
(`servfail_unsynced`, upstream `secondary:unavailable:<apex>`) until the
realignment was published, for up to about half a second after each change. A
delegated child's status write broke its binding to the parent zone, so the
parent lost authority the same way. A real client sees this: a mounted node
queried without pause during record-set edits and dynamic UPDATEs answers
hundreds of SERVFAILs. A test-only wait would not have been an honest fix.

Serving readiness (`PreparedImage.Admitted` and the parent/child binding in
`internal/primaryruntime/image.go`) now also accepts a primary lifecycle whose
receipt differs from the DNSZone only in fields outside the intent digest and
the NS address dependency: status, and metadata such as the resourceVersion,
annotations other than managed-by, finalizers and owner references. None of
those other fields is read by the serving runtime for a DNSZone. The UID, apex,
type and intent digest must still be the same, the source must not be removed,
and the head's frozen spec, NS address dependency and source receipt must match
the DNSZone (`ConfigMatchesDesired`) (`imageServingSource`). The realignment
serves the same head with the same configuration, so answers do not change
across it. `SourceMatchesDesired` stays exact, so publication acknowledgment and
status projection still wait for the realignment, and every command's
transactional source check still compares the exact token. A change of intent
(for example the query ACL) or of the frozen NS address dependency in the same
window still fails closed. Four existing assertions encoded the old gap as
required behavior: image readiness, the status feedback fixture, the signed
transfer barrier and the stale-source publication test. In the status-only gap
they now require the committed head to be served, and a signed AXFR in that gap
gets exactly the committed head. They were changed deliberately; the commit
message says so. `docs/architecture/zones.md` describes the behavior.

Evidence under `.context/v020-final-20261110/evidence/CORE-STATUS-WRITE-SERVFAIL/`:

- Control on the read-only base checkout 33ee04bc (`control01-base.log`), race
  detector, pinned to two CPUs, built-in queries delayed by an overlay-only pause
  probe (never committed): 3 of 12 runs of
  `TestRunServeSQLiteCompletePrimaryUpdateRestart` failed at 450 ms with `mounted
  built-in query lost committed authority: ... Rcode:SERVFAIL
  Status:servfail_unsynced ... Upstream:secondary:unavailable:example.`; 12 of
  12 passed at 1000 ms.
- Deterministic reproduction `TestImageReadinessStatusWriteKeepsCommittedAuthority`
  (internal/primaryruntime): status writes of a child and then of its parent, the
  image built in each gap, the realignment after each, and then an intent change
  and an NS address change that must still fail closed. Without the fix (old
  image.go through an overlay) it fails with `status write of child made
  child.example. unavailable` (`before02.log`). With the old image.go, the
  updated `TestImageReadinessStatusOnlySourceAlignmentAdmitsEquivalentHead` also
  fails (`before01.log`).
- `TestRunServeSQLiteCompletePrimaryUpdateRestart` with the fix, race, two pinned
  CPUs, probe overlay: 100 of 100 at 0 ms and 100 of 100 at 450 ms
  (`accept01-restart-p0-p450.log`, 36 min), 100 of 100 at 1000 ms
  (`accept02-restart-p1000-p2000.log`; the 2000 ms half of that run was cut off
  by the machine restart and is retained), 100 of 100 at 2000 ms
  (`accept03-restart-p2000.log`, 22 min). Total 400 of 400, no failures.
- New mounted test `TestRunServeSQLitePrimaryAnswersAcrossStatusWrites`
  (cmd/kudurru/serve_status_write_test.go). It runs the mounted SQLite server and
  asks for a committed name back to back (1 ms gap) while 24 changes (18
  record-set edits through the API, 6 signed UPDATEs) each commit a new head and
  a status write of a new serial. It requires at least 20 status writes of new
  serials, no SERVFAIL and no wrong answer. With the fix: 3 of 3 passed
  (`mounted06-after.log`, 103 to 137 s each, 24 status writes of 24 new serials,
  20,808 / 30,204 / 37,874 answers, 0 SERVFAIL). Control with the old image.go
  (`mounted07-before.log`): 2 of 2 failed with 786 and 1,381 SERVFAILs.
- Root selection `-run 'Primary|Authoritative|Update|Transfer|Notify|Zone'`
  (skipping the two long tests above) of ./cmd/kudurru under race
  (`root-race01.log`): exit 0, 54 passes, 1 skip (the env-gated live dnsdist
  test `TestRunServeRemoteEdgeAuthoritativeCacheACL`), 360 s.
- Race over internal/primaryruntime/..., internal/servingbuild and
  internal/servingzones (`light-race03.log`) and internal/stages/authoritative,
  internal/zoneview and internal/primaryruntime (`light-race04.log`): all ok.
  `light-race01.log` keeps the run before the four assertions were updated (they
  failed as expected); `light-race02.log` shows them passing.
- Query-path allocation guards unchanged: `TestLookupCNAMEAnswerAllocation`
  and `TestMetadataInsertionKeepsOneAllocationAndDetachedValues` pass
  (`alloc02-query-guards.log`), and the internal/stages/authoritative,
  internal/zoneview and internal/pipeline packages pass without race
  (`alloc01-noraceguards.log`). The fix changes only image preparation, not the
  per-query path.
- Scoped golangci-lint (cmd/kudurru, internal/primaryruntime, internal/servingbuild):
  0 issues (`lint01.log`, `lint02.log`).
- Retained failed runs: `mounted01`–`mounted04` and `mounted05` (cut off by the
  restart) are earlier versions of the mounted test. `diag03` and `diag04` used
  overlay-only instrumentation (`diag2/`) to show why those runs failed. Their
  signed UPDATEs did commit and publish, roughly every 2.5 s, but the reply came
  after the 2 s client timeout. Each retry was a new request that committed
  again, and retries that arrived while the next status write was unpublished
  were refused at capture (`CheckWriterLease`: the lease's publication does not
  match the advanced authority). The test's UPDATE client now waits 15 s.
  `diag01` showed that UPDATEs after idle periods succeed.

Still open: under the race detector on two loaded CPUs, an UPDATE reply can take
more than 2 s, and while a status write is unpublished, a new UPDATE is answered
SERVFAIL at capture, which is a retryable refusal. Both are existing,
deliberate write-path guards (UPDATE acknowledgments keep full data currency,
D378), not query answers, and were not changed. The same relaxation is not
applied to nonprimary lifecycles or to resourceVersion changes that also
change the NS address dependency. Those still answer SERVFAIL until the head is
recompiled, as designed. The full ./cmd/kudurru package was not run (the
integrator runs hack/go-test.py).
