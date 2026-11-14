### PUBLISH-STARVATION Publication under steady commits — November 13, 2026 (D405)

A node that received signed UPDATEs fell further and further behind its own
commits, until sign-ins, reads and writes were refused at capture
(`503 generation_pending`) and sign-ins whose session committed answered
"authentication operation unavailable". Measurement, not the expected
cause, decided the fix. An instrumented publisher (`PUBDIAG`, overlay only)
showed preparation time rising linearly with the revision (0.1 s to 2.2 s
without the race detector over 215 revisions); a CPU profile put it in
whole-image `replication.Validate`/`Decode`; a per-kind dump of each
candidate image found 230 `ZoneChunk` objects (3.8 MB) after 220 commits of a
20-name zone. Every content commit of a primary zone staged the whole new
content as content-addressed chunks and moved the head, but nothing ever
deleted the previous content's chunks, so the replicated image, which every
publication reads, digests and validates five times, grew by a whole zone
with every UPDATE. Separately, 67 of 144 preparation cancellations in a
30 + 30 run were `Wake` calls about 15 µs into a preparation that the same
commit's generation change had just started.

What happens now (commits `5b200f28`, `f66940d5`, `aa57d5d4`, `3d3e88bc`,
`bdff2d8f`, `980c7fe0`, `a141541e`, `4ef85f36`, and after review `5e8dd096`,
`53eea181`, `28087ae9`, `f1279497` on `wip/v020/ci`):

- `CommandTx.CommitZoneState` (SQLite) sweeps in its own transaction when a
  head moves to new content: it marks the root manifest and parts of every
  `ZoneState` head, tombstoned ones included (`zonestate.References`), and
  deletes every other `ZoneChunk`, which also removes chunks an earlier
  version left behind. A head whose root cannot be read makes it delete
  nothing and logs a warning naming that head (`53eea181`). The image now
  stays at the referenced chunks (2 chunks, 50 KB in the same run).
- `zonestate.Repository.Read` reads the head and then its chunks in separate
  store reads, so with the sweep a content commit in between deleted the
  chunks of the head it held and the read failed with `store.ErrNotFound`
  (found in review: 204 of 19,656 reads during 300 commits). That reached
  `Runtime.Apply` (SERVFAIL for a signed UPDATE) and the lifecycle callers
  that take `ErrNotFound` from `Read` as "no head". A missing chunk is now
  checked against the head again: a moved head is read again (at most 16
  times, then `ErrConflict`); a head that did not move answers `ErrInvalid`.
  `ErrNotFound` from `Read` again means only that the zone has no head
  (`5e8dd096`).
- `Publisher.Wake` no longer cancels the preparation in progress. The Wake
  count is noted before a preparation reads its candidate; Wakes after that
  are answered by exactly one more preparation after it has published. A new
  generation, authority or dependency cursor still cancels (the obsolete
  cut's publication would be refused).
- `Retry-After: 1` is sent only while no command of the request has
  dispatched: a refusal after an earlier command or item of the same request
  carries none (`servinghttp.Dispatched`, the anchored item chain).
  `capabilities.md` no longer claims that an answer without it was refused
  inside or after its transaction; it says such an answer does not tell
  whether anything ran.
- The zone change tests' question is settled: a 503 "current writable
  serving generation unavailable" of an object write is always effect-free
  (an object command passes no acknowledgment to `currentAuthCommandThen`, so
  nothing after its commit can refuse; `TestServingObjectWriteUnavailableIsEffectFree`).
  The helper no longer treats it as an unknown outcome nor counts a later
  409 as created. It asks again only after a 503 that carries
  `Retry-After` (the API's own statement that no part of the request ran):
  `generation_pending`, and a command refused after its whole D399 bound,
  both still produced under the race detector.
- `TestServingHTTPTOTPLoginPasswordConsumeExactlyOnce` (401 instead of 429
  under load): the factor limiter's second counts from the first attempt's
  admission, before its sign-in commits and waits for its session's
  publication; when that wait exceeds a second the repeat meets the durable
  replay check. The test now holds the runtime clock at one instant from the
  first attempt to the repeat.
- `classifyMounted` no longer tolerates a slow superseded answer: the
  mounted test now fails on every refusal.

Evidence under `.context/v020-final-20261110/evidence/PUBLISH-STARVATION/`
(scripts and overlays in `.context/v020-final-20261110/scripts-PUBLISH-STARVATION/`):

- Diagnosis: `diag01-nonrace.log` (base, 30 + 30: 102 publications, 75
  generation and 67 Wake cancellations, preparation 0.1 to 2.2 s),
  `diag02-pprof-top-cum.txt` (72 of 161 s in `replication.Validate`),
  `diag03-nonrace-image-sizes.log`, `diag04-nonrace-kinds.log` (230
  `ZoneChunk`, 3,799,265 bytes at revision 220). `diag05-nonrace-chunk-gc.log`:
  with the sweep, 2 chunks; test 107 s to 34.5 s. `diag06-nonrace-gc-wake.log`:
  with both, 17.3 s, 0 Wake cancellations, 30 + 30 admitted.
- Unit: `TestZoneStateCommitCollectsUnreferencedChunks` (20 commits, stored
  chunks equal to what the heads reference after each; a leftover staged
  chunk removed; another zone kept) and `TestZoneStateCommitKeepsChunksWhenAHeadIsUnreadable`;
  without the sweep the first fails ("commit 0: stored chunks 8, referenced
  by the heads 4", `unit01-chunk-gc-without.log`).
  Reads across the sweep: `TestZoneStateReadAcrossACommitReadsTheNewHead`
  (a commit injected between the head read and the first chunk read; the
  read answers the committed head), `TestZoneStateReadDuringSteadyCommits`
  (4 readers during 100 commits) and `TestZoneStateReadOfADamagedHeadIsNotNotFound`;
  against the reviewed `Read` all three fail (`fix-01-read-tests-without-fix.log`,
  race: `store: not found: ZoneChunk/...`, 140 of 9,560 reads failed during
  200 commits); with the fix every `TestZoneState*` passes under race
  (`fix-02`, 5,937 reads during 100 commits, 0 failed; `fix-07`, 5,834).
  `TestZoneStateCommitKeepsChunksWhenAHeadIsUnreadable` now also requires the
  warning and fails without it (`fix-06`). `./internal/zonestate/...`,
  `./internal/primaryruntime/...` and `./internal/store/kube` pass under
  race (`fix-03`, `fix-05`; `fix-03`'s three `kube` subtest failures are a
  missing `KUBEBUILDER_ASSETS`, rerun in `fix-05`).
  `TestPublisherWakeDuringPreparationIsCoalescedNotCanceling` (five Wakes
  during one preparation: no cancellation, exactly two preparations); fails
  at the base publisher (`unit02-wake-without.log`). Race runs:
  `unit03` (`./internal/serving`, `./internal/zonestate/...`,
  `./internal/servinghttp`: ok), `unit05`/`unit06` (`./internal/primaryruntime`
  ok; `./internal/store/sqlite` ok in 838.8 s; an earlier run with the
  default 10-minute timeout timed out at 600.5 s on the loaded machine and
  `unit04` failed for a missing `KUBEBUILDER_ASSETS`, both retained).
- Mounted, without the race detector, the strengthened
  `TestRunServeCommandsNotRefusedAsSupersededUnderSteadyUpdates` (10 signed
  UPDATEs a second, 200 sign-ins, 200 record-set writes and 200 reads, each
  sent once, nothing tolerated): `diag08` 600 of 600 admitted, write to DNS
  answer max 922 ms (274 s); `diag09` 600 of 600, max 2.259 s (394 s, load
  up to 38); `norace15-mounted.log` 600 of 600, max 828 ms (190 s). In
  review: `review-03` failed, 200 sign-ins, 200 writes and 188 reads
  admitted, 12 reads refused at capture, max 3.434 s (542 s, load 18 to 20
  with another probe running); `review-04` 600 of 600, max 1.9 s (438 s).
  Those lags were measured from after the follow-up read of the object,
  which itself waits for the write's publication; `28087ae9` starts the clock
  at the write's 200 and logs the committed UPDATE rate. With that,
  `fix-08-norace-mounted.log`: 600 of 600, write to DNS answer median
  624 ms, max 2.774 s (297 s); 1,723 UPDATEs sent, 157 committed (0.53 a
  second), 1,566 SERVFAIL. The proof is therefore at under one committed
  UPDATE a second (`norace15`: 158 of 1,115; the reviewer's runs 144 of 2,410
  and 140 of 1,873). The base before this package refused 225 to 254 of 400
  at capture (CMD-RECAPTURE `load05`/`load06`).
- Mounted under the race detector (retained failures): `race07-mounted-200-diag.log`
  (34 min, load 13 to 38): 92 of 200 sign-ins, 115 writes, 20 reads
  admitted; 178 refused at capture, 73 sign-ins unacknowledged, 12
  superseded after the 5 s bound, 25 writes 401 after the writer's session
  expired in the 34-minute run; publication gaps median 3.9 s, p90 5.4 s,
  max 11.1 s, preparation 1.5 to 2.4 s. `race10-mounted-40-cpuprofile.log`
  (40 + 40 + 40): 37, 40 and 13 admitted; the profile (`race10-pprof-*`)
  shows the process used 1.3 cores on average, with 33 % of samples in
  `__tsan_read` (the instrumented SQLite driver) and preparation 11.6 % of
  CPU. The race skip of the mounted test therefore stays, with that reason,
  and so does its `NORACE_TESTS` entry.
- D395/D378 neighbors, without the race detector (`norace15-mounted.log`):
  `TestRunServeSQLiteZonesAnswerAcrossCommits` pass (40 writes, 35,774
  probes, 0 without a correct answer), `TestRunServeSQLiteLongApplyKeepsServingDNSAndReads`
  pass in that one run (120 documents in 16.9 s, 0 SERVFAIL, longest gap
  96 ms, 17 reads ok, 0 reads failed); under load it is not reliable at base
  or head: the reviewer's interleaved runs (`review-13`) passed base 3 of 6,
  head 5 of 6 and head with the base publisher 4 of 6, each failure 1 to 5
  reads refused `generation_pending`, and `review-03`/`review-09` refused 6
  and 3 reads at head, `TestRunServeSQLitePrimaryAnswersAcrossStatusWrites` pass,
  both zone change tests pass. Under the race detector (`race14`, `race18`):
  `TestRunServeSQLitePrimaryAnswersAcrossStatusWrites`, both zone change tests
  (twice each with the final helper) pass; `race14` retains the failure of
  the first single-shot helper at `generation_pending`
  (`TestRunServeSQLiteParentEditedAsChildIsCreated`), which is why the helper
  keeps that retry. Every `TestServingCommandRecapture*`,
  `TestServingItems*`, `TestServingObjectWriteUnavailableIsEffectFree` and the
  TOTP test pass under race (`race14`).
- Anchors, race binaries pinned to `taskset -c 0,1`, `GOMAXPROCS=2`, single-shot
  sign-ins: `rate11-anchors-pinned-0-1.log` (head, 40 each, load 15 to 38):
  Bootstrap 28/40, Fenced 27/40, all failures `generation_pending` at the
  sign-in or the zone status read, or the test client's 5 s timeout.
  `rate13-anchors-interleaved-base-head.log` (base and head alternating,
  20 each): Bootstrap head 20/20, base 19/20; Fenced head 19/20, base 19/20
  (head 39/40 against base 38/40).
  `diag12/run-3.log` shows the mechanism of the remaining start-up refusal:
  the zone's status write and its lifecycle realignment commit 0.55 s apart,
  the second cancels the first's preparation, and the next current
  publication comes more than two seconds after the read arrived.
  `rate19-parent-edited-interleaved.log` (pinned to CPUs 2,3, race, base
  and head alternating, stopped after 7 runs): pairs answer 9 to 60 s after
  the child's records at both base and head (not a regression); base 2 of 3
  and head 3 of 4 passed, and both failures were a command refused after its
  whole five-second recapture bound (`took_ms` 6331 and 5412), which with the
  helper's first single-shot version failed the test; the helper now asks
  again after any 503 with `Retry-After`. With that helper
  (`race22-zone-change-helper-retry-after.log`, race, 2 runs each): both zone
  change tests pass, one setup request asked again.
- TOTP: `race16-totp-original-with-1100ms-delay.log` (original test with a
  1.1 s delay before the repeat): 3 of 3 fail 401 against 429;
  `race20-totp-fixed-with-1100ms-delay.log`: the fixed test with the same
  delay 3 of 3 pass; the factor sign-in itself took 0.7 to 1.1 s.
- `lint17.log`/`lint21.log`: golangci-lint v2.15.0 over the touched packages,
  one revive finding fixed by `a141541e`, then 0 issues; `gofmt17.log` empty;
  `bench17.log` OK. After review: `fix-04`, `fix-09` (`./internal/zonestate/...`,
  `./internal/store/sqlite/...`) and `fix-10` (`./cmd/kudurru`) 0 issues,
  `fix-11` bench guards OK. No generated artifact changed.

Open. Acceptance (1) holds without the race detector only, and there not
reliably on a loaded runner (5 of 6 runs; `review-03` refused 12 reads at
capture when one preparation outlasted the two-second wait). Under the race detector a
publication takes one to three seconds on a loaded machine, longer than the
two-second capture wait after a burst of two commits (a record-set write and
the driver's zone commit; a status write and its lifecycle realignment), so
reads and sign-ins are still refused at capture, and the mounted test keeps
its race skip. Acceptance (3), 40 of 40 for both anchor tests pinned to two
CPUs, is not met: 39 of 40 in the interleaved run against 38 of 40 at the base, and
55 of 80 in a run under heavier load; the remaining failure is that same
capture refusal at start-up. The largest remaining lever is the five
whole-image validations per preparation (`readServingCandidate`, the
provider coordinator, `PrepareImage`, `inbound.DecodeImage`,
`CaptureImage`), plus the coordinator's own validation of every generation;
memoizing them needs their owner callbacks proven pure (D378). Many signed
UPDATEs still answer SERVFAIL under this load (957 of 1,115 in
`norace15`), the D399 decline: an UPDATE's ingress check needs a current
publication and is not recaptured. Reads during a long apply, which D378
left open, remain load-dependent at base and head alike (interleaved:
base 3 of 6, head 5 of 6 passing; one unloaded run served 17 of 17): they
depend on one publication completing within the capture wait, which this
package makes more likely but does not guarantee. The CI-RED-2 series of 100
consecutive race runs pinned to 4 CPUs was not run, because the anchor tests
still fail intermittently. The zone change tests' helper still asks again
after a 503 with `Retry-After` (`generation_pending`, a bound-ended
refusal) and logs the count; remove that once capture refusals are gone
(with D405). The sweep adds work proportional to the number of zone heads
to every content commit inside the writer transaction (in review 1.2 ms of
26.7 ms with 1 other zone, 14.8 of 118.9 ms with 100, 83.0 of 438.1 ms with
300, `review-12`); sweeping only the replaced head's candidates, or every N
commits, is a follow-up. The Kubernetes zone state
backend still never deletes chunks (its staging is not transactional).
On SQLite, chunks that an earlier version left behind are removed by the
next content commit of any primary zone, not at start-up.
