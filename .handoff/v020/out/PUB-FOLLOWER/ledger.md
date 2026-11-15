### PUB-FOLLOWER Forwarding through a follower across a same-source publication — November 15, 2026 (D406)

With the two D405 changes (zone chunk sweep, Wake coalescing) every
enrolled-cluster e2e test failed: a 200-document apply through a follower
answered "the enrolled primary is unavailable; the write was not applied" at
document 2 to 6, and a batch deletion through a follower cut its fourth name
the same way (hosted CI on 329f26f4; locally 7 of the 10 applies in two
rounds at 329f26f4). The answer is not the primary's: an instrumented build
(PUBF_DIAG, overlay only, never committed; the patch is retained in the
evidence directory) showed that the follower refused the item itself before
sending it, with `serving: generation pending` from `CheckReadLease`, 3 to 5
ms after the follower had published the image it already served a second
time. Since D405 a `Wake` that arrives while a preparation runs is answered by
one more preparation once the first has published, instead of canceling it.
On a follower with managed root anchors the anchor source wakes the publisher
during every pull's own preparation, so every install was published twice,
the second time with the same generation 0.2 to 1.4 s later (25 such pairs on
one follower in one test; on followers with static anchors fewer, and with
the same dependency identity). A forwarded request captures one lease before
it reads its body and checked before every item that this lease is the very
publication served now; the second publication of the same image therefore
cut every later item of a request that began on the first. Before D405 the
Wake canceled the first preparation and the image was published once, which
hid the strictness of that check. The chunk sweep plays no part.

What happens now (commits `d21a9b72`, `1bc817cc` on `wip/v020/ci`):

- `withMemberForward`, which every forwarded object write and delete (alone
  or as an item of `POST /apply` or `:batchDelete`), zone convenience
  command, blocking toggle and settings patch passes before it sends and
  again before it relays the answer, checks the request's source instead of
  its exact publication: `Publisher.CheckSourceLease` requires that the
  Store's authority, including its installed revision, is still the lease's
  own and that the lease's dependency cursors are still current. A later
  publication of the same image no longer cuts the request; a pull, a fence,
  a term change or a dependency change still does, with the same answers as
  before (D375, D368). The host, keyring, provider-resource and runtime
  checks of the forward are unchanged, and reads and local commands keep
  `CheckReadLease`.
- Both D405 changes stay: neither is unsafe. The decision row says why the
  alternatives (bringing back the Wake cancellation, skipping a Wake's
  publication when its identity is unchanged) were not taken.
- `docs/architecture/configuration-and-store.md` and
  `docs/deploy/sqlite-join.md` describe the check and why a follower
  publishes its image again without an install.

Evidence under `.context/v020-final-20261110/evidence/PUB-FOLLOWER/`:

- Reproduction at 329f26f4 (`KUDURRU_E2E=1`, prebuilt binaries, `taskset -c
  0-3`, through the heavy wrapper, load 5 to 30 from the other lanes):
  `e2e01-partition-329f26f4.log`: TestEnrolledClusterProcessesPartitionCatchupAndRestart
  passed once (200 documents in 21.0 s, 219 s). `e2e02-enrolled-round1-329f26f4.log`
  and `e2e03-enrolled-round2-329f26f4.log` (all seven enrolled tests):
  5 of 7 failed in each round; of the 10 failed applies, 7 were cut at
  document 5 or 6 within 0.6 to 1.4 s with "the enrolled primary is
  unavailable; the write was not applied", 3 ran out of the request's
  30-second lifetime at document 110 to 168 (`completion_unknown`).
  TestEnrolledProcessConflictResolution and
  TestEnrolledManagedAnchorsConflictResolution passed in both rounds (they
  apply nothing through a follower). `e2e04-enrolled-round3-329f26f4.log` is
  a round the agent stopped during its first test, to free a slot for the
  diagnosis; it is not a result.
- Diagnosis (instrumented binaries): `diag02-e2e-instrumented.log` with
  its trace `diag02-trace.txt.gz` (publications and forward errors;
  `diag03-trace.txt.gz` adds Wake callers and dependency identities,
  `diag01-trace.txt.gz` is the UPDATE trace of the mounted run):
  TestEnrolledManagedAnchorsPartitionRestartAndRevoke passed,
  TestEnrolledClusterProcessesPromoteAndRejoin failed at document 5; the
  trace shows follower publication 42 (revision 188) at 00:40:37.307, the
  same-generation publication 43 at 00:40:38.119 and the refused item
  4 ms later with `serving: generation pending`. `diag03-e2e-instrumented.log`:
  the same sequence in TestEnrolledManagedAnchorsPromoteAndRejoin (publication
  53 then 54 of revision 185, refusal 3 ms later) and in
  TestEnrolledManagedAnchorsOfflineRecovery; every same-generation
  republication followed the publisher's own coalesced Wake
  (`settleWake`) after an anchor-source Wake during the install's
  preparation. The same run's TestEnrolledClusterProcessesPartitionCatchupAndRestart
  failed earlier and for another reason: `POST /iam/users` on the primary
  answered `503 unavailable` (the D399 family; IAM users are not recaptured).
  `diag-instrumentation.patch` is the overlay.
- Focused proof: `TestRootMemberForwardAcrossSameSourceRepublication`
  (cmd/kudurru, the follower and primary runtimes of the member-forward
  fixture over real mTLS and HTTP): a single write and a three-document
  apply each capture their publication, stop at the body read, the follower
  publishes the same source again, and then they are forwarded. Without the
  fix it fails: "a single write after the follower published its source
  again: status=503 ... the enrolled primary is unavailable; the write was not
  applied" (`unit01-republication-test-without-fix.log`). With it every
  `TestRootMemberForward*` passes (18 tests, 115 s,
  `unit02-rootmemberforward-with-fix.log`), including
  `TestRootMemberForwardApplyOriginalSourceCutRefusesItems` and
  `TestRootMemberForwardOriginalBodyCutRefusesSuccessor`, which still require
  the cut after an install. `TestPublisherSourceLeaseSurvivesASameSourcePublicationOnly`
  (internal/serving): the lease survives a same-source publication, which
  `CheckReadLease` refuses; a dependency change, a new generation, a
  canceled context and a released lease are refused.
- At the fix (`d21a9b72`), all seven enrolled tests, three rounds:
  `e2e11-enrolled-round1-d21a9b72.log`, `e2e12-enrolled-round2-d21a9b72.log`
  (binaries built from `d21a9b72`) and `e2e13-enrolled-round3-d21a9b72.log`
  (built from `1bc817cc`, the docs commit on top; same code), `taskset -c
  0-3`, load 7 to 13: 14 of 21 test runs passed, and round 3 passed all
  seven (1,236 s, 1,310 s and 1,577 s per round). No apply and no batch
  deletion was cut early in any run. Every failure (7) was the 200-document
  apply running out of the request's 30-second lifetime at document 142 to
  189: TestEnrolledManagedAnchorsPartitionRestartAndRevoke failed in rounds
  1 and 2 (189, 164) and passed in round 3 (29.6 s);
  TestEnrolledManagedAnchorsPromoteAndRejoin failed in rounds 1 and 2 (177,
  178), passed in round 3 (26.1 s); TestEnrolledManagedAnchorsOfflineRecovery
  passed in rounds 1 and 3 (23.3 s, 28.4 s) and failed in round 2 (166);
  TestEnrolledClusterProcessesPartitionCatchupAndRestart failed in round 1
  (142) and passed in rounds 2 and 3 (23.6 s, 19.5 s);
  TestEnrolledClusterProcessesPromoteAndRejoin failed in round 1 (183),
  passed in rounds 2 and 3 (24.1 s, 25.3 s);
  TestEnrolledProcessConflictResolution and
  TestEnrolledManagedAnchorsConflictResolution passed in all three. The same
  apply took 10.3 to 11.9 s on the hosted runner at 329f26f4 where it was not
  cut, 21 s here at 329f26f4 under lighter load: on this shared machine the
  apply's duration, not the forwarding check, decides these runs.
- Race: `race01-internal-serving.log` (`./internal/serving/...` ok, 32 s);
  `race02-cmd-forwarding.log` (`-run '^(TestRootMember|TestRunServe.*Follower|TestServing.*Follower)'`,
  57 pass, 1 skip, ok); D405 proofs `race03-d405-proofs.log`
  (`./internal/zonestate/...` ok; every `TestZoneState*` of
  `./internal/store/sqlite` passes, 8 tests, including
  `TestZoneStateCommitCollectsUnreferencedChunks`,
  `TestZoneStateReadAcrossACommitReadsTheNewHead` and
  `TestZoneStateReadDuringSteadyCommits`);
  `TestPublisherWakeDuringPreparationIsCoalescedNotCanceling` passes in race01.
  The mounted D405 proof at `1bc817cc`, without the race detector
  (`norace04-mounted-steady-updates.log`):
  `TestRunServeCommandsNotRefusedAsSupersededUnderSteadyUpdates` passes in
  242 s, 200 sign-ins, 200 writes and 200 reads admitted, 1,430 UPDATEs sent,
  151 committed (0.62 a second), write to DNS answer median 434 ms, max
  2.635 s. Under race (`race05-d405-cmd-neighbors.log`): every
  `TestServingCommandRecapture*`, `TestServingItems*`,
  `TestServingObjectWriteUnavailableIsEffectFree` and
  `TestServingHTTPTOTPLoginPasswordConsumeExactlyOnce` pass (24 tests).
- `lint01.log`: golangci-lint v2.15.0 over `./internal/serving/...` and
  `./cmd/kudurru/...`, 0 issues; `gofmt01.log` empty; `vet01.log` ok;
  `bench01.log`: bench guards OK (14 tests). No generated artifact changed.

D399-REDO's open item (the SERVFAIL share of signed UPDATEs) was not the
task once the enrolled tests failed, but the instrumented mounted run
measured it and is recorded here for the next package
(`diag01-mounted-norace-instrumented.log`, without the race detector, load
about 12, 7.5 minutes; the run failed its own committed-rate floor, 0.32 a
second against 0.5): of 2,216 signed UPDATEs 145 answered NOERROR and 2,071
SERVFAIL. (1) 1,744 SERVFAILs, 84 %, were refused at ingress before any
admission: the UPDATE's DNS request captured the publication served at its
arrival, a commit of the node (a record-set write, its zone commit, a
sign-in, a status write) had already moved the Store past it and its
successor was not published yet, so `captureUpdate`'s first `CheckWriter`
refused it. That is effect-free and is D399's "signed UPDATEs are not
recaptured". (2) 111 SERVFAILs were committed UPDATEs: `publishUpdate` waited
for its own commit's publication, a later commit of the same zone (the
test's record-set writes to that zone) was published first, and
`publishedMember` answered `ErrSuperseded` because the served member's
version was newer than the UPDATE's. That is the case the D399-REDO note
asked about: D313's acknowledgment refuses an UPDATE whose commit its own
node superseded, and 111 of 256 committed UPDATEs (43 %) answered SERVFAIL.
Accepting a served successor of the same zone incarnation would answer them
correctly (zone heads are a compare-and-swap chain, so a later head of the
same incarnation contains the commit), but it changes `publishedMember`,
`Runtime.install` and `Runtime.publish`, which require the exact member, and
needs its own decision. (3) After the 256th committed UPDATE every further
UPDATE answered SERVFAIL "retry ledger full" (204 times): the zone's receipt
ledger (`zonestate.Limits.MaxReceipts`, default 256) is append-only (the
SQLite adapter requires exactly one more receipt per update) and nothing
evicts receipts, so a zone accepts at most 256 committed signed UPDATEs in
its lifetime. That is a release-relevant defect for dynamic DNS and for this
mounted test on a slow runner, where it ends the committed stream; it needs a
decision on eviction (native UPDATE receipts carry a per-request retry ID and
are never matched again; a forwarded command's ID is reused only within its
30-second bound) and a format review, so it is not changed here.

Open: the three local passes of each enrolled test at the final head
were not reached: 14 of 21 runs passed, round 3 all seven, and every failure
was the 200-document apply needing more than its 30-second lifetime on this
loaded 4-CPU pin (19.5 to 29.6 s when it fit, about 7 documents a second; the
hosted runner applied the same file in 10 to 12 s). Nothing in this package
makes an item slower, but whether 200 documents in one 30-second lifetime is
the right bound for the process test (3f0869f0) is for the ha lane and the
integrator; the hosted e2e job on the integrated head decides. When the
lifetime ends before an item is sent the item answers "the enrolled primary
is unavailable; the write was not applied" (`objectForwardError`'s default),
not the documented "the request's 30-second forwarding lifetime ended before
this write was sent", which `memberCaptureError` gives only at capture; the
answer is true (nothing was applied) but names the wrong cause (ha lane,
follower forwarded apply handling). A same-generation publication after a
Wake still supersedes HTTP leases on any node (D399 recaptures only marked
commands): on followers with managed anchors it follows almost every pull
(25 of 54 publications on one follower), on primaries it is rare (4 of 103
in the same run); skipping a Wake's preparation that would publish an
identical identity would remove it on static-anchor nodes, not on
managed-anchor ones (their identity changes on every preparation), and was
not done. The UPDATE findings above (ingress refusals, committed UPDATEs
answered SERVFAIL when their own node superseded them, and the 256-receipt
lifetime cap of a zone) are open and need decisions; the receipt cap is the
most urgent. `TestEnrolledClusterProcessesPartitionCatchupAndRestart` also
failed once in an instrumented run at `POST /iam/users` on the primary (503,
an unmarked command superseded by the node's own commits, D399), not at the
apply.
