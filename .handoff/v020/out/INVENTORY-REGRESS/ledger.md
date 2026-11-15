### INVENTORY-REGRESS UniFi hostname lifecycle failure and the lease-MAC query cost — November 13, 2026 (D397)

The inventory package (ID-INVENTORY) is not the cause of the
`TestRunServeUniFiHostnamePolicyLifecycle` failure that was bisected to it.
A race test binary of the real 2e15060e tree fails the held-capture step at
the same rate as the lane head when the two run interleaved under the same
pinned conditions. The earlier 3-of-3 pass at 2e15060e and 3-of-3 failure at
e0bddb72 came from runs at different times on a machine whose load moved
between 9 and 19. The 2e15060e log took 351 s for three passes, about 117 s
each. Passing runs here took 108 to 176 s.

What fails is the test's timing margin. The official UniFi API gives each
assignment a deadline 3 × Poll (15 s, truncated to the whole second) after
its observation. The next acquisition starts Poll after the source
acknowledgment. The held request is captured only when that acquisition
reaches its last response. With the race detector, the acknowledgment needs
two complete serving preparations of about 0.9 to 1.8 s each after the source
commit. Store generation changes cancel the preparations that run during an
acquisition. Each provider request writes a budget reservation (D364, D384),
so one acquisition cancels about 40 preparations. Observation to the next
held gate measured 12.7 to 19.4 s, so the 15 s deadline passes on about two
runs in three at a load of 12 or more.

Each candidate change was checked against that path:

- The lease-MAC fill. The test runs no DHCP service, so `LeaseMAC` loops over
  zero stores. The fill still ran for every query without an inventory MAC,
  and allocated when the address held a lease, whether or not any mac matcher
  could use the MAC. That is fixed below.
- The inventory snapshot rebuild. Instrumented copies of the serving
  publisher (diagnostic overlays, not committed) count 42 to 44 preparations
  per held run at head and 48 to 51 at 2e15060e. Five per run succeed at head
  (0.92 to 1.69 s) and five at 2e15060e (0.91 to 1.83 s). Both trees
  acknowledge after the second preparation that follows the commit.
- The same-poll collision rule. The phases before the failing step have no
  collision, and `Translate` produces the same clients when no address is
  contested.
- The expired-claim rule. It changes only `Snapshot.Devices` (the API).
  `Lookup` and the identify stage do not use it.

The identify stage now fills the lease MAC once, after it captures the policy
index, and only when `Index.MatchesMACs` reports a ClientGroup `mac` matcher.
A query that no mac matcher can select makes no lease snapshot read and no
allocation for the fill. Source-bound grants never match a lease MAC, so they
do not enable it.

The mounted SQLite inventory test now waits for the short lease's match with
`awaitInventoryDNSBefore`. It records the send time of every query. A blocked
answer counts only for a query sent before the lease's expiry; a query sent at
or after the expiry fails with "short lease expired before its match was
observed" whatever it is answered, and a query sent in time that is not yet
blocked fails the same way once the expiry has passed. Before, the test failed
whenever the match was observed at or after the expiry; the first version of
this helper (618c9ec0) accepted a blocked answer before it looked at the
expiry, so a query sent after the expiry could pass. The decision is the pure
`inventoryAttemptAfter`, covered by `TestInventoryAttemptAfterExpiry`.

Evidence under `.context/v020-final-20261110/evidence/INVENTORY-REGRESS/`:

- Baselines. `overlay-2e15060e/overlay.json` reverts only the package's 30 Go
  files at head ("package reverted"). `overlay-full-2e15060e/overlay.json`
  replaces every file under `api/`, `cmd/`, `ee/` and `internal/`, and every
  Go file, that differs from 2e15060e ("true 2e15060e"). go.mod is unchanged
  between the two commits. Every run used race binaries,
  `GOMAXPROCS=4 taskset -c 0-3`, one test per process, through the heavy lock.
- `final10/` holds ten interleaved rounds at 618c9ec0 (the final head 62a4b8ff adds only architecture docs) against
  true 2e15060e:
  - `TestRunServeUniFiHostnamePolicyLifecycle`: 3/10 pass at head, 3/10 at
    2e15060e. Six head failures and six base failures are the held-capture
    step (`assignment_remaining` −0.29 to −7.0 s). Base round 8 failed later:
    the identity after the conflict phase did not converge.
  - `TestRunServeUniFiHostnameHeldPublicationDeadline`: 3/10 pass at head,
    4/10 at 2e15060e. Every failure is the held-capture step.
  - Passing durations: lifecycle 123.6 to 175.9 s, held 41.7 to 60.1 s.
    Load averages ranged from 9.4 to 18.6.
- Earlier interleaved runs at 531bffd0, with diagnostic copies of the test
  that log the controller's request times:
  - `ab03/`: head 2/5, package reverted 3/5.
  - `ab04/`: head 2/5, true 2e15060e 2/5.
  - `diag01-head.log` / `diag02-base.log`: head 2/2, package reverted 0/2
    (load 10 vs 19).
  - `diag/cycles.py` reads these logs. Acquisition takes 1.6 to 3.8 s, and
    poll end to next poll start 9.8 to 13.8 s in both trees.
- `ab05/`, `ab06/`: the publisher instrumentation above (preparation counts,
  cancellation causes, wake callers). These are diagnostic runs at a fixed
  load, not pass-rate evidence.
- Allocation guards and benchmarks (`bench01-base.log` package reverted,
  `bench02-head.log` 531bffd0, `bench03-fix.log` final; no race, five
  counts):
  - `BenchmarkServe` is 0 allocs in all three.
  - `TestSelectPolicyDoesNotAllocate` passes in all three.
  - `BenchmarkSelectPolicy/*` is 0 allocs in all three.
  - `BenchmarkTracedChain` is 12/14/14 allocs in all three.
  - The new `BenchmarkServeFreshIdentity` identifies a new request every
    iteration:
    - none: 3 allocs/560 B at both 531bffd0 and the fix.
    - unmatched (a lease for the address, no mac matcher): 4 allocs/576 B at
      531bffd0, 3 allocs/560 B with the fix.
    - matched: 6 allocs/608 B at both.
  - Time per op is too noisy on this shared machine to compare (1.7 to
    3.7 µs in every variant).
- `unit01-gate-before.log` (retained, failed on purpose): the new
  `TestLeaseMACsUnreadWithoutMACMatcher` fails against the 531bffd0 identify
  stage with one lease read. `race01-units.log`: `internal/stages/identify`,
  `internal/policy` and `internal/dhcpdns` pass under race, and `go vet`
  passes on them and `./cmd/kudurru`.
- `mount01-short-lease.log`: `TestRunServeSQLiteInventoryAPIAndLeaseMACs`
  (42.7 s) and `TestRunServeKubeLeaseMACSelectsGroupAndInventory` (13.1 s)
  pass without race.
- The short-lease diagnostic:
  - `mount04-match-after-expiry.log`: a diagnostic copy of the test sleeps
    until the 3 s lease has expired before its first query. The old test
    failed after 36.6 s with the generic "UDP query ... want blocked=true".
    The new test fails after 6.8 s with "short lease expired before its match
    was observed".
  - `mount02`/`mount03` (retained): sub-second leases still matched in time,
    and leases that had already expired were skipped by the import, so
    neither reached the expiry case.
- `lint01.log`: golangci-lint v2.15.0 reports 0 issues on
  `internal/stages/identify`, `internal/policy` and `cmd/kudurru`.
  `bench-guards01.log`: 14 bench-guard tests pass.
- `full01-cmd.log`: `python3 hack/go-test.py --timeout=30m
  github.com/robertherbaugh/kudurru/cmd/kudurru` at 618c9ec0 ran through the
  heavy lock with `taskset -c 0-3` and `GOMAXPROCS=4` (load 11 at the start;
  23:47 to 01:29). It discovered 891 targets in 9 race batches:
  - Batches 1, 2 and 4 to 8 pass (500 to 904 s each).
  - Batch 3 fails `TestServingHTTPRoleBindingsReviewedCRUD` (got 503, want
    409).
  - Batch 9 fails `TestServingHTTPFactorsShareFailureHistoryAcrossAdapters`
    (got 503, want 429).
  - The no-race timing run fails `TestRunServeSQLiteLongApplyKeepsServingDNSAndReads`
    (one of 19 reads failed during a 13 s apply).
  - Both UniFi hostname tests passed inside their batches.
- Each failing test alone (`alone01`, `alone02`, `alone03`), three times or
  more, at head ("fix"), at head with only the package reverted ("pkgrev"),
  and at true 2e15060e:
  - `TestServingHTTPRoleBindingsReviewedCRUD` (race): fix 10/11 pass, pkgrev
    3/5, 2e15060e 11/11.
  - `TestServingHTTPFactorsShareFailureHistoryAcrossAdapters` (race): fix 5/8,
    pkgrev 4/5, 2e15060e 8/8.
  - `TestRunServeSQLiteLongApplyKeepsServingDNSAndReads` (no race): fix 3/3,
    2e15060e 3/3.
  - The two serving-HTTP 503 failures also occur with the inventory package
    reverted, so they come from commits after 2e15060e other than this
    package's. They were not bisected further.

What remains open:

- The hostname tests still fail about two runs in three under the race
  detector at load 12 or more, at this head and at 2e15060e alike. The
  product change that would widen the margin without touching the test is in
  the integration host's schedule: start the next poll Poll after the
  previous observation rather than Poll after its acknowledgment, or stop
  per-request budget commits from canceling serving preparations. Both are
  cross-integration changes outside this package and were not made.
- The serving-HTTP 503 failures (`TestServingHTTPRoleBindingsReviewedCRUD`,
  `TestServingHTTPFactorsShareFailureHistoryAcrossAdapters`) are not seen at
  2e15060e in 19 runs and need a bisect over the commits after e0bddb72.
  Candidates, by code reading only, are `e76eb08c fix(serving): coalesce a
  Wake into the next preparation` and the anchor-writer commits.
- The short-lease diagnostic is proven only with a diagnostic overlay that
  delays the query. A natural expiry between import and match could not be
  forced in the mounted test: the import skips leases that have already
  expired.
