### CORE-APPLY-OPEN DNS and reads during a long multi-item request — November 11, 2026 (D378)

On a complete SQLite node, DNS now keeps answering for the node's own zones
while one `kudurru apply`, `kudurru restore` or dashboard multi-delete
commits its documents. The cause was in the DNS output guard, not in the
request's chain. An ordinary local, authoritative or mixed answer required
full data currency, so every commit withheld it until the next publication.
The DNS dependency check also covered the provider host, the license cut and
the provider resource set, so every replacement of that set withheld answers
again until the node published once more. A trace of one 120-document apply
(diag07) showed seven such installs and retirements (resource worker
`owner.go:554`, provider authority admission `admission.go:130`, a reset),
and the DNS outages lined up with them exactly. An ordinary query answer is
now bound to the publication that is served (`Publisher.Serves`, by identity
of the root seal), or to full data currency when that still holds. Its
dependency check (`RuntimeOwner.DNSDependenciesCurrent`, from the new
`servingbuild.Dependencies.DNSCurrent`) covers only the root lifetime, the
DNS keyring, the transferred and catalog zone data and the anchor input.
When the node publishes a successor, older answers are retired at once.
Transfers, UPDATE acknowledgments, API dig, cache insertions and native
resolver mutations keep full data currency, and HTTP keeps the complete
dependency check.

Other callers' reads cannot use an older publication: their credentials and
data must be current. They wait up to two seconds in the HTTP listener for an
eligible publication. That listener now marks a waiting `GET` or `HEAD` on the
publisher (`Publisher.AwaitHTTP`). The next item of a multi-item request then
lets the node publish at once (`WaitTerm`, then 200 ms without commits), as
long as the request has not spent longer in its waits than in its own work.
Waiting writes are not marked, so a sign-in does not cut a restore. The
timed pace after two seconds of work stays, and its wait bound now adapts:
2 s at first, doubled after a miss, twice the last publication's time after
a hit, capped at 10 s, with work between two timed waits at least the next
bound. Both together keep a request at most about twice as slow. The chain
compare, the original-lease rule for the first item and every refusal answer
of D378 are unchanged.

Evidence under `.context/v020-final-20261110/evidence/CORE-APPLY-OPEN/`:

- Diagnosis, kept as found: `diag01`–`diag05` are the earlier implementer's
  traces (fixed pace, adaptive pace, ratio rule; `diag02` ran out of the
  five-minute lifetime with the adaptive wait but without the work-at-least-
  wait rule). `diag06` printed nothing (run without `-v`). `diag07` traced
  the provider resource retirements. `diag08` traced the pace: under the race
  detector one publication grew from 1.5 s to 10 s over the request.
  `diag09` profiled the same apply without the race detector: 6.1 s, gap
  57 ms, 8 of 8 reads, with publication cost dominated by
  `replication.Validate`/`Decode`/`Digest`. `diag10` (4 runs) and `diag11`
  (6 runs) traced the residual non-race gaps (see below).
- `race02`: `TestRunServeSQLiteLongApplyKeepsServingDNSAndReads`, 13
  `TestServingItems*`, `TestRootAnchorServedDNSFollowsThePublication` and
  `TestRootAnchorRetainedDNSDoesNotGrantDataOrCacheMutation`, 17 passed,
  162.6 s. The long apply had a longest DNS gap of 0.118 s (before: 80 s).
- `race03`: regression selection of 132 tests (`TestRunServeSQLite*`
  Apply/Restore/Multi/Batch/Update/Notify/TSIG, `TestServingItems*`,
  `TestRootAnchor*`, `TestRootUpdateOutput*`, `TestRootRetained*`,
  `TestApplyAndRestore*`, `TestServingZoneCommand*`, `TestServingZoneNotify*`,
  `TestServingRuntime*`, `TestServingHTTPOIDC*`, `TestPrimaryUpdate*`,
  `TestPrimaryRuntime*`, `TestProviderDependencyCapture*`,
  `TestDoctorNotify*`, `TestServingCommand*`, `TestAnchor*`): 131 passed and
  1 failed, 768 s. Retained failure:
  `TestRootAnchorPreparationPublishesFencedDNSOnly` timed out on one UDP
  exchange. In response, the served check was made never stricter than full
  data currency, so an answer whose data is still current is no longer
  refused after the node republishes the same data. `race05` then ran that
  test and the anchor/served/update-output tests 5 times each: 30 passed,
  107 s.
- `race07` (final tree): the same selection, 131 passed and 1 failed, 654 s.
  The long apply took 2m0.6s with 2242 DNS answers, a longest gap of 0.302 s
  (one query dropped while the node swapped publications), 11 reads ok and
  48 refused. Retained failure: `TestRunServeSQLiteStoredTSIGBackgroundTCP`
  failed with `bind: address already in use` on its ops listener, a port
  taken on the shared machine. `race08` reran it and its TLS sibling twice:
  4 passed, 208 s.
- `race04`: `go test -race ./internal/serving ./internal/servinghttp
  ./internal/servingbuild`, all ok (servingbuild 197 s).
- Without the race detector: `run06` plus `diag10` and `diag11`, 11 runs of
  the long apply, all passed with the tightened assertions. Every read during
  the apply succeeded (7 or 8 per run), applies took 4.4 to 5.3 s, and the
  longest DNS gaps were 0.056 to 0.815 s.
- New tests: `TestRootAnchorServedDNSOutlivesAProviderReplacement` (a retired
  provider resource set keeps DNS answers and retires the complete check; a
  replaced DNS keyring still withholds them), `TestServingItemsYieldToAWaitingCaller`,
  `TestServingItemsPaceArithmetic`, and
  `TestInitialCaptureWaitMarksOnlyReads`. The mark checks were added to
  `TestInitialCaptureWaitUsesPublishedAuthorityAndDispatchesOnce` and
  `TestInitialCaptureWaitIsBoundedAndCancelable`. The mounted long-apply
  test now requires a DNS gap of at most 1 s, every read to succeed without
  the race detector, and at least one read with it.
- `lint02`: golangci-lint v2.15.0 over `./internal/serving/...`,
  `./internal/servinghttp/...`, `./internal/servingbuild/...` and
  `./cmd/kudurru/...`: 0 issues. `lint01` found one unused parameter in the
  earlier implementer's test, since fixed.

- Review fixes (after the verifier's accept-with-fixes): `hack/go-test.py`
  (`make test`, which CI's go job runs) now runs
  `TestRunServeSQLiteLongApplyKeepsServingDNSAndReads` a second time without
  the race detector after the race batches (`NORACE_TESTS`), counts its
  failure, and refuses to start when the name is missing from the compiled
  inventory. `fix-pytest01`/`fix-pytest02`: `hack/test_go_test.py`, 6 passed,
  including the two new cases. `fix-norace01` (3 runs, before commit) and
  `fix-norace02` (3 runs at `c781c5d6`): all passed, every read ok (6 to 10
  per run, 0 refused), applies 4.3 to 6.6 s, longest DNS gaps 0.053 to
  0.811 s. The follower cut case is now in
  `TestEnrolledClusterProcessesPartitionCatchupAndRestart` and
  `TestEnrolledClusterProcessesPromoteAndRejoin` (`fix-e2e01`, `KUDURRU_E2E=1`,
  2 passed, 257 s): an apply of 40 documents through the follower fit between
  two pulls (40 created); one of 160 was cut after 103 or 104 documents, the
  document at the cut was an explicit failure, every later one answered
  `unavailable` ("the follower's source changed"), and none was applied on
  either node. `fix-lint02`: golangci-lint over `./test/e2e/...`, 0 issues.
  D378, `docs/deploy/sqlite-cluster.md` and `docs/architecture/zones.md` now
  say that any waiting `GET` or `HEAD`, signed in or not, triggers the pause
  (the credit bounds it at about twice the duration), and that right after an
  acknowledged API write DNS answers the previous data until the node
  publishes, indefinitely while it cannot (`prepare_failed` in readiness, log
  `complete serving generation not published`).

Open findings of CORE-APPLY-MULTI: the major residual is resolved for DNS
and resolved for reads on a node that publishes within two seconds; the
strict "every read succeeds" assertion now runs in every `make test`
without the race detector. The verifier's test-strength finding is resolved
by the tightened assertions. The informational finding that pauses invite
cuts no longer grows from reads, which do not commit. Finding 6 (a
process-level follower cut) is resolved by the e2e case above. What stays
open, for the integrator to record in `dev-docs/followups.md`: (a) under the
race detector on a loaded machine one publication of 120 record sets takes
up to 10 s, so most reads during the request are still refused with
`generation_pending` (11 of 59 succeeded in `race07`; the race run asserts
only at least one). No pace can serve a two-second capture wait then. The
cost is the whole-image `replication.Validate`/`Decode`/`Digest` per
candidate (`diag09` profile), a separate optimization. (b) Without the race
detector a publication sometimes lands while a primary zone's lifecycle
source still names the zone's previous resourceVersion; that publication
answers the zone with SERVFAIL until the lifecycle catches up, up to 0.815 s
(`diag11`, `ZONETRACE phase=active eligible=false`, `imageCurrentSource`;
0.811 s in `fix-norace02`). The lifecycle source guard is deliberate and was
left alone. It is the only margin under the test's one-second bound, so a
slower machine may flake the test; if it does, fix the lifecycle ordering
rather than widen the bound. Existing zones can see this sub-second
SERVFAIL window during an apply. The full `./cmd/kudurru` package was not
run here (the integrator's `hack/go-test.py` run is needed), and the
handed-off `TestServingZoneCommandActualHTTPCompleteSQLite` flake was not
examined.
