### F28-EF-1 Live qualification of a joined follower behind managed dnsdist edges — November 11, 2026 (D<nnn>, number to be assigned by the integrator)

The supported order for adding a follower behind managed edges (runbook
"Add a follower behind managed edges") now runs against the pinned dnsdist
image (`kudurru-dnsdist:vip-fix-b8fd96f`, image sha256:89c7f196a871...), with
a primary and a newly joined follower as two backends of one real edge.
`TestRunServeManagedEdgePinnedDaemonFollower` (opt-in,
`KUDURRU_ANCHOR_EDGE_LIVE=1`) runs a mounted `runServe` primary with managed
anchors and edges topology. The primary bootstraps behind the edge, is
activated through the API (`/cluster/activate`) and restarts as the enrolled
primary. The test then issues a join token (`/cluster/join-token`) and adds
the follower's backend to the edge's static backends. The edge child restarts
with its retained deployment identity, a fresh run identity and two backends.
The receiver joins with the real `kudurru cluster join --offline` CLI, and its
first image carries the primary's managed owner. The follower then starts
with edges topology. Its start observes the one-edge fleet exactly once,
counted at the edge's authenticated observation listener, and its deny-only
supervisor makes its own root acquisition. With the primary stopped and
marked down by the edge, both pools (`anchor.test.` on the authoritative
pool, `resolver.test.` on the resolver pool) answer over UDP and TCP from
the follower alone. Both pools report zero cache size and entries
throughout. With both backends down, the edge returns no answer, so no
cached copy exists. A follower restart observes the fleet again exactly once,
serves again, makes at most one more acquisition and keeps the primary's
managed owner and its installed follower authority. The single-backend test
`TestRunServeManagedEdgePinnedDaemonRestart` now shares the same lab fixture.

The live run exposed one product defect, which is fixed. The elected
root-anchor updater stopped for its whole writer term when an ordinary commit
moved the canonical generation under it. At startup this is the zone status
write racing the first bootstrap of a fresh managed node: the bootstrap
rolled back with "authority changed", and the node had no managed trust
until a restart, which then acquired the root again. A capture refused by the
store's authority CAS is now retried one second later, since nothing was
acquired. An update transaction that rolls back with the authority CAS
(definite rollback, no denial committed) discards the reply without replaying
it, and a later step makes a new acquisition. These retries keep the existing
owner, policy and settings retry floor and are paced at 1, 2, 4 and so on up
to 64 seconds while the race repeats. Accepted-owner changes, uncertain
outcomes, committed denials, a lost term and every authentication or
admission refusal stay terminal. Each raced step logs a warning ("root anchor
update raced an ordinary commit; new acquisition scheduled") with the next
attempt time and the consecutive count, so repeated races under write churn
are visible. A revocation denial that an ordinary commit rolls back (denial
rolled back, update unknown) is deliberately not retried and stays terminal
for the writer term, as before; a unit test pins this.
`docs/architecture/upstreams-and-cache.md` describes this.

Review fixes (after the independent review). The decision no longer cites
D370, which belongs to F28-GUARDS; code, tests and the architecture page cite
F28-EF-1, and `decision.md` is a `D<nnn>` row for the integrator to number or
merge. The Restart test's old guard "exactly one root acquisition in the whole
run" was loosened by the retry to "at least one by the first stop, no more
after it". It is now bounded again: both live tests wait (bounded, two
minutes) until the primary's bootstrap acquisitions settle, using the
updater's own pace as the quiet window, and fail when a fresh primary makes
more than three bootstrap acquisitions. The settle wait also runs before
the first node stop and before the activation restart and join, which removes
the latent flake where a stop could precede a raced bootstrap's commit; the
Follower test then requires that the activation restart acquired nothing.
The Follower test also restarts the edge child a second time while the joined
follower serves alone (retained deployment, fresh run identity, both pools
answer), and asserts after that restart and again after the follower restart
that the follower has not observed the fleet more than once per start.

Evidence under `.context/v020-final-20261110/evidence/F28-EF1-LIVE/`:
- `fix08-live-race-x3.log` (`-race -count=3 -v`, final head after the review
  fixes): `TestRunServeManagedEdgePinnedDaemonRestart` (about 20 s),
  `TestRunServeManagedEdgePinnedDaemonFollower` (about 33 s),
  `TestAnchorEdgeActivationJoinedFollower` and all nine `TestAnchorUpdater*`
  tests, including the new `TestAnchorUpdaterRacedAdvancePollAcquiresAgain`
  (a zone write lands while a due managed advance poll's reply is in flight:
  definite rollback without denial, unchanged owner, a paced hold, then one
  new acquisition that commits or is unchanged, then the accepted schedule)
  and `TestAnchorUpdaterRacedRevokeDenialStaysTerminal`. 36 of 36 passed, no
  skips, no DATA RACE, exit 0, 209 s. Every live run settled after one
  bootstrap acquisition. No containers were left on the private daemon.
- `fix03-updater-race-x3.log`: `TestAnchorUpdater*` under `-race -count=3`
  (earlier fixer, same code), 27 of 27, exit 0.
- `fix07-lint.log`: golangci-lint v2.15.0 on `./cmd/kudurru/...`, 0 issues.
- `fix06-live-race-x3.log` is a retained failed run: it did not set
  `KUDURRU_ANCHOR_EDGE_WORKDIR`, so both live tests failed at fixture setup
  ("absolute owned edge scratch parent required") in 0.02 s; every other test
  passed. `fix01`, `fix02`, `fix04` and `fix05` are empty logs from the earlier
  fixer, whose runs were cut off by the machine restart.
- `live16-final-x3.log` (final head, no race detector):
  `TestRunServeManagedEdgePinnedDaemonRestart` and
  `TestRunServeManagedEdgePinnedDaemonFollower` 3 of 3 each, exit 0, no skips
  (durations in the log). `live14-both-x3.log` and `live15-both-x3-fixed.log`
  (earlier heads, before and after the updater fix): both tests 3 of 3 each,
  about 9 s for Restart and 13 s for Follower.
- `race07-final-live-race.log` (`-race -count=3`, final head): both live
  tests, `TestAnchorEdgeActivationJoinedFollower` and all seven
  `TestAnchorUpdater*` tests passed 3 of 3, 30 passes, exit 0. Restart took
  about 16 s and Follower about 28 s.
- `race01-updater.log`: `TestAnchorUpdater*` under `-race -count=3`, 21 of
  21, including the new `TestAnchorUpdaterRacedCommitAcquiresAgain`, which
  lands a zone write while the bootstrap reply is in flight and checks for a
  definite rollback, unchanged history, a paced hold and one new acquisition
  that commits. `probe01-updater-concurrent-write.log` is the same scenario
  on the code before the fix: the step returned "replication: authority
  changed" and the next step stayed stopped. (`race02-prechange.log` is a
  retained attempt to run the new test on the old code; it only shows that
  it does not compile there.)
- `race03-anchor-selection.log`:
  `-race -run '^(Test.*Anchor|TestManagedTopology|TestManagedEdges|TestClusterJoin|TestOfflineAnchor)'`
  on `./cmd/kudurru`, exit 0, 99 top-level and 128 subtest passes, no skips,
  231 s.
- `lint01.log` (2 findings in the new fixture: G117 test login, ineffassign),
  then `lint02.log` and `lint03.log` (final head): golangci-lint v2.15.0 on
  `./cmd/kudurru/...`, 0 issues.

Retained failed runs and what they showed: `live01`, `live03` and `live04-diag`
(earlier implementer, before the fixes) are single-shot asks that timed out
after the edge child restart, with the updater's "authority changed" stop in
the log. `live02-head-baseline` passed the old single-backend test once.
`live07`: a join token issued right after the enrolled primary restarted was
refused with 503 (the writer was not yet admitting commands). `live08`: the
readiness login hit the same 503. `live09`: a logged-in client must send CSRF
even to log in again. `live10`: the offline join needs a data directory that
does not exist yet. `live11` and `live12`: the follower's acquisition is
asynchronous, and the single-read root fixture could not answer it.
`race04-final-live-race` and `race05-restart-race-diag` (under `-race`):
the startup publication cascade outlasted the wait on one zone, and one ask
came right after the primary restart, before edge health saw it. The fix was
to settle both zones and wait for edge health. `live05-head-diag.log` is an
empty log from the interrupted earlier run.

What remains open: the live test is opt-in and needs the private Docker
daemon and the pinned image, so CI does not run it. Only one edge is
exercised (the in-process test covers two). The follower's own deny-only
supervisor was not checked for the same CAS race; its refusals keep their
existing handling. A revocation denial raced by an ordinary commit still
parks the updater for the writer term (fail closed); retrying it promptly
needs its own design and should be added to `dev-docs/followups.md` by the
integrator. No running-node surface reports the accepted managed owner, so
the live bootstrap settle wait infers it from the updater's pace and the
owner is checked offline after the stop. A login creates a session row and so publishes a new
generation, and each publication drops in-flight DNS answers (D313/D332).
That is existing behavior, not changed here; under load it can cost a
client one retry. The full `./cmd/kudurru` package was not run (the
integrator runs `hack/go-test.py`).
