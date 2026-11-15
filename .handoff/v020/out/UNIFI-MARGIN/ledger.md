### UNIFI-MARGIN UniFi hostname assignment margin under load — November 15, 2026 (D408)

The two mounted UniFi hostname tests failed under the race detector on a loaded machine because a connected client's assignment expired between two healthy polls. A UniFi assignment lives three polls (15 s at the tests' 5 s poll, truncated to the second) from its observation. The integration host started the next poll one interval after the previous sync returned, and a sync returns only after its source commit is acknowledged by a complete serving publication, which takes two or three preparations on a loaded race run. Observation to the next poll's last response was therefore Poll + one whole sync + one acquisition.

What now happens:

- A successful periodic or explicit sync schedules the next periodic sync one interval after it began, or at once when it ran longer than the interval (`nextSync` in `ee/integration/host/commands.go`). Syncs of one job never overlap, so a provider still sees at most one sync start per interval. Failed and fenced syncs keep their existing delay from the failure (D284), and reconcile still syncs at once.
- A sync ends with the job's own status write, which gives the stored Integration a resource version the job's frame does not have until the host captures it again; root fences a sync on the older frame. The next periodic sync now waits for a host refresh begun after that write, which the job requests with `Wake`, and at most one interval after the sync ended (the old schedule). Without this, the poll that followed at once was fenced in 70 ms and the fenced observer then waited a full interval, so the gap grew instead of shrinking.
- UniFi observation admission (`admitDesired`, `cmd/kudurru/integration_source.go`) accepts a target zone whose only change since the observation's cut is its status: same UID, generation, spec, labels, annotations, finalizers, owners and deletion (`zoneStatusOnly`). The zone driver writes that status after every change of the zone's records, the previous observation's included. Any other change still fences.
- On the complete SQLite runtime the integration host starts with the node's first complete serving publication (`afterInitialized`, `cmd/kudurru/serving_lifecycle.go`), the channel readiness waits for. Every provider request of an acquisition commits a budget reservation that makes the serving preparation in progress obsolete, so an acquisition at startup used to hold readiness back for its whole duration.
- No lifetime changed: assignments still expire 3 × Poll after their observation, truncated to the second; the operation, capture and request bounds and all retry and refusal backoffs are unchanged. Neither test was edited except one comment that described the old schedule.
- Identify (reviewer nit from INVENTORY-REGRESS): a request whose identity was captured by a call that returned without an identification (no index yet, or a failed index capture) now gets its lease MAC at the call that identifies it, once (`internal/stages/identify/identify.go`).
- `decision-D397.md` is a single merged D397 row that folds the lease-fill gate (`Index.MatchesMACs`) and the identify fix into the existing D397 text and replaces its "Open at 7663c957" note. Use it instead of the INVENTORY-REGRESS row that reused the number.

Evidence under `.context/v020-final-20261110/evidence/UNIFI-MARGIN/`. Every mounted run used a race test binary, `GOMAXPROCS=4 taskset -c 0-3`, one test per process, through the heavy lock, the two tests interleaved (`scripts/rounds.sh`):

- `base10/` at 329f26f4 (load 4.2 to 20.5):
  - `TestRunServeUniFiHostnamePolicyLifecycle` 6/10. Failures: round 2 held capture (`assignment_remaining=-2.824s`); rounds 3 and 7 "UniFi mount never ready" (20 s readiness bound); round 6 "UniFi identity did not converge source=127.0.0.2 ... phase=0" after the held step.
  - `TestRunServeUniFiHostnameHeldPublicationDeadline` 7/10. Failures: rounds 3, 6, 10 held capture (`assignment_remaining` −3.903, −3.09, −2.22 s).
  - Passing durations: lifecycle 112.6 to 134.8 s, held 38.0 to 62.5 s.
- `final10/` at 26cdf1b8, the final source head (built as a74226cb, the same tree before a message-only amend; load 5.9 to 21.1):
  - `TestRunServeUniFiHostnamePolicyLifecycle` 10/10, 73.3 to 129.1 s.
  - `TestRunServeUniFiHostnameHeldPublicationDeadline` 10/10, 26.4 to 51.0 s.
  - The held request completed 1.37 to 3.19 s after its capture in every run (5 s deadline).
- Retained intermediate runs:
  - `fix10-aborted/` at fdbd0bcc (schedule change only), stopped after four rounds at load 22 to 27: lifecycle 1/4, held 1/3. Failures: held capture −11.3 s and −3.4 s, and three "never ready".
  - `fix10b/` at 063a4d69 (schedule and frame wait): lifecycle 10/10, held 6/10. Failures: three "controller phase=3 was not durably accepted; attempts=28" and one held capture (−0.528 s).
- Diagnostic overlays (`diag/`, never committed; runs in `diag01` to `diag08`):
  - They log each sync's begin and end, the UniFi observation time and expiry, the held gate, the source commit and acknowledgment, frame refreshes, publisher preparations, and every root fence point.
  - `diag01` (329f26f4 against fdbd0bcc): syncs took 4.3 to 13.9 s. With the schedule change alone, the poll that followed a long sync at once was fenced in 69 to 70 ms ("incarnation fenced") and the next waited a full interval.
  - `diag02`/`diag03` (frame wait): the next sync starts 30 to 130 ms after the refresh that follows the status write. Acknowledgments took 1.7 to 5.7 s, two or three preparations after the commit.
  - `diag08` caught the "phase=3" failure: `target-zone-fenced zone=home rv=65 cut_rv=2 gen=1/1`, the target zone's status-only write landing while the held acquisition was in flight.
  - Readiness: before the startup gate, diagnostic runs became ready 6.4 to 13.8 s after start, and 2 of 15 were not ready within 20 s (body `serving(complete serving generation unavailable: preparing)`). With the gate, 19 of 19 were ready in 4.9 to 11.6 s.
- Focused tests that fail without each change:
  - `TestPeriodicPollIntervalCountsFromThePreviousPoll` (`ee/integration/host`): a 3.5 s gap after a 1.5 s acknowledgment at a 2 s interval (`unit01-before-fix.log`).
  - `TestPeriodicPollAfterStatusWriteUsesAFrameCapturedAfterIt`: three observations on one frame, `[0 0 0]` (`unit03-frame-wait-before.log`).
  - `TestIntegrationUniFiObservationAdmitsTargetZoneStatusWrite` (`cmd/kudurru`, real SQLite runtime and HTTPS controller fixture): it holds an acquisition at its last provider response and writes the target zone's status (the observation must commit and publish) or its spec (it must be fenced). The status case fails with the old `integration_source.go` ("incarnation fenced", `unit05-zone-status-before.log`).
  - `TestIntegrationsStartAfterServingInitialized` covers `afterInitialized`. It is a unit test of the helper; the wiring is one line, and the mounted readiness evidence is above.
  - `TestLeaseMACFillsAtTheIdentifyingCall` fails with lease reads=0 on the old identify stage (`unit02-identify-before-fix.log`).
- Race runs:
  - `race01-host.log` and `race04-host.log`: `ee/integration/host`.
  - `race02-identify-policy.log`: `internal/stages/identify`, `internal/policy`.
  - `race03-ee-integration.log` and `race06-ee-integration-final.log`: `go test -race ./ee/integration/...` plus identify and policy at the final head.
  - `race05-cmd-regression.log`: the final race binary with `-run 'TestRunServeUniFi|TestIntegration|TestServingRuntime|TestRunServeSQLiteInventory|TestRunServeKubeLease'` (100 tests).
- `bench01-identify.log`:
  - `BenchmarkServe` 0 allocs.
  - `BenchmarkServeFreshIdentity`: none 3 allocs/560 B, unmatched 3/560 B, matched 6/608 B, unchanged by the identify fix.
  - `TestSelectPolicyDoesNotAllocate` and `TestLeaseMACsUnreadWithoutMACMatcher` pass.
  - `bench-guards01.log`: 14 bench-guard tests pass.
- `lint02.log`/`lint03.log`: golangci-lint v2.15.0, 0 issues on `ee/integration/host`, `internal/stages/identify` and `cmd/kudurru`. `lint01.log` (retained) did not run: another lane's lint held the lock. gofmt is clean on every touched file.

What remains open:

- The margin is still finite. Observation to the next held gate is now max(Poll, one sync) plus one acquisition. The final runs reached load 21 without a failure, but at sustained load above about 25 (the aborted runs) a sync can still take longer than the assignment's remaining life.
- The acknowledgment still needs two or three complete preparations, because any write during a preparation leaves the publication behind the installed generation and `CaptureHTTP` refuses it. Making that cheaper belongs to the publisher (lane ci) and was not changed.
- Per-request budget reservation commits still cancel preparations during an acquisition. The startup gate stops this from delaying readiness, but not during later polls.
- Base round 6's "identity did not converge ... phase=0" after the held step (also seen once by INVENTORY-REGRESS at 2e15060e) did not recur in 20 final runs. It was not diagnosed.
- `TestIntegrationsStartAfterServingInitialized` tests the helper, not the mounted wiring.
- The Kubernetes integration runtime has no startup gate. UniFi record publication, and so the target zone check, is refused there anyway.
