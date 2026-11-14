### CMD-RECAPTURE Commands superseded before dispatch capture again — November 13, 2026 (D399)

A current HTTP command is held to the publication its request captured
(D199) and checked again in the command (D208). Until now a command whose
publication another commit superseded before its transaction body began
was refused `503 unavailable` ("current writable serving generation
unavailable"), with no effect. The node's own zone status, lifecycle,
admission and anchor commits, and signed UPDATEs, publish for seconds
after a write or a first start, so a ready node refused sign-ins,
activations and record-set writes this way (the hosted failures of
f6afe7d6, b9b66cba and ecd633b1).

What happens now (commits `6281a4ea`, `f456d5c6` and `eff1ac40`, on `wip/v020/ci`): such
a command captures the current publication again and is dispatched once
on it. This is done once, in the shared root path
(`recaptureCommand` around `currentAuthCommandThen` and
`currentAuthCommandOutput`, with `servinghttp.Recapture`), so sign-in,
sign-out, cluster activation and join-token, object, zone and settings
writes and every other route on that path get it. The wait is bounded by
5 s or the request deadline, whichever ends first; then the answer is the
same effect-free 503, now with `Retry-After: 1` (both the API and the
auth routes). `Retry-After` is set only on a refusal returned before any
transaction body of the command began (`notDispatchedError`, `eff1ac40`);
a refusal from inside a body, at commit or after a commit whose
acknowledgment failed carries none. The new capture is admitted only when no command of the
request has dispatched yet, its authority is the original one advanced
only in its installed revision, its auth view has the same dependency
identity (license, permissions, role bindings, keys, providers, auth
overrides), the same auth controls and the same cluster settings object,
and the original raw credential verifies on it as the same principal,
with the same session (ID, CSRF secret, issue and absolute expiry) and
the same effective roles. The transaction then repeats its own
authorization and source checks. Every lease the request held stays
retained until the request ends (`requestCell`). Never retried: a refusal
from inside the transaction body or at commit, an uncertain outcome, a
later item of a multi-item request (D378 chain), a second command after
the request's own commit, a fence or authority change, a changed
credential, session, role, policy or settings. `join-token` compares its
reviewed source digest only outside its transaction, so it captures again
only on the same source generation (`WithExactSource`); activation
compares the reviewed source in its transaction and answers `409
conflict` for a stale review.

Decided not to recapture (reasons in `docs/architecture/access.md`):
an integration operation's first no-op (`beginOperation`), which holds the
integration host's lease barrier (`Host.WithLease`, `h.mu.RLock`) and the
runtime's `r.mu.RLock`; it is marked `servinghttp.WithoutRecapture` and
keeps the immediate refusal (`eff1ac40`). The other roughly 25 callers
were audited for held locks: the node-config and cache-flush routes hold
a per-route request gate (a one-slot channel that publication never
takes), so a wait there delays only a second request of the same route,
bounded by its own deadline; migration steps run on their own cut
(`command_step.go`, not a request cell) and never recapture; no other
caller holds a lock across the command;
follower early admission and follower-forwarded commands (bound to the
follower's captured source; unchanged); promotion (bound to the reviewed
installed generation); the primary's forwarded-command entry needs no
change, because it is bound to the member request's authority (mode,
member, term, incarnation) and not to a publication, so the primary's own
commits never refused it; signed RFC 2136 UPDATEs (their capture is the
DNS transport's: the keyring that verified the signature and the output
that signs the answer come from it, so a recapture would admit the same
wire message on a second keyring). This is a decline, not a fix:
`Handler.resolve`'s unbound SERVFAIL and the withheld UPDATE
acknowledgment (D313) are unchanged, and whether UPDATE clients send a
SERVFAIL again was not measured (the earlier claim that they do was
removed from the docs, `2c075021`).

Test-side workarounds (commit `bec9d351`): the zone-group tests
(`serving_zonegroup_test.go` and the mounted client in
`serve_zonegroup_mount_test.go`) no longer ask a command again after
"current writable serving generation unavailable"; they still ask again
after a capture refusal (`generation_pending`, before the request is
read), which D399 does not cover. Kept, with reasons: `enrolledQuietReview`
(an activation is bound to the exact reviewed source digest, so a commit
after the review makes it stale; D399 turns the in-flight 503 into the
correct `409 conflict`, it cannot make the review current); the
updater-first-step wait of `71ac05f0` (that 503 is the writer not running
yet, `primary.Run` starts only after the anchor writer's first attempt,
not a superseded capture). The CI-RED-2 login reordering is not in this
tree; the anchor activation settle helper keeps its sign-in and, again
since `2479ecdb`, its zone status read single-shot (the
`generation_pending` retry that `bec9d351`'s neighbor had added hid the
capture refusal and was removed on review).
`controlMountedRequest` names the error key and message of a failed
answer (`5f295cff`). One existing test changed its expectation by
decision: `TestServingItemsFirstItemKeepsTheOriginalSource` (a commit by
another writer before the first item is now captured again and all three
items are applied once).

Evidence under `.context/v020-final-20261110/evidence/CMD-RECAPTURE/`
(scripts and overlays in `.context/v020-final-20261110/scripts-CMD-RECAPTURE/`):

- `race02-command-tests.log`: `TestServingCommandRecaptureSignInSupersededBeforeDispatch`,
  `TestServingCommandRecaptureObjectWriteSupersededBeforeDispatch` and
  `TestServingCommandRecaptureRefusesWhatItMustNotRetry` (race, 3 runs):
  pass. The hook commits as another writer after the writer binding and
  waits until `CheckWriter` reports the capture superseded; sign-in and
  write each succeed with 2 attempts, 1 recapture, 1 dispatch, one more
  session and the control's audit rows plus one (sign-in), exactly one
  audit row (write). `race14-failed-neighbors-rerun.log` (final rules):
  the same plus the subtests for changed settings, changed role
  bindings and a second command after the request's own commit, all
  pass; the bound subtest refuses after 5.04 s with 15 attempts, 14
  recaptures, 0 dispatches and `Retry-After: 1`.
- `race07-command-tests-without-recapture.log` (overlay without
  recapture): the sign-in and write tests fail with 503, the bound test
  fails (1 attempt, refused after 11 ms); the refusal subtests pass both
  ways, as they must.
- `race10-neighbors.log` (first commit): seven existing contract tests
  failed, which led to `f456d5c6`: a second command after the request's
  own commit was recaptured (blocking, object, zone, items "plain"), and
  a successor auth policy / role binding publication was adopted.
  `race12-neighbors-after-dispatch-rule.log`: those pass except the
  cookie-policy test, fixed by the auth-controls and settings rule, and
  three load-timing failures (`TestServingHTTPEnrollmentFinalClockRollback`
  with a DATA RACE between the test's unsynchronized clock variable and the
  integration host, `TestServingHTTPBrowserLogoutExpiredOrMismatchedHintStaysLocal`,
  `TestServingHTTPTOTPLoginPasswordConsumeExactlyOnce` 401 instead of 429)
  at load 15-20. `race14`: the first two pass; the TOTP test failed once
  more there and passes 3 of 3 alone with and without recapture
  (`race15-totp-*.log`). It assumes two sign-ins within the 1 s factor
  limiter; a sign-in that waits in a recapture makes that less likely.
- `diag22-login-recapture-all.log` (overlay that logs every recapture,
  `TestRunServeDirectAnchorsBootstrapTransportsRestartAndDisabled`, race,
  `taskset -c 0,1`, `GOMAXPROCS=2`): 20 of 20 pass; the sign-in captured
  again in 5 of them, each admitted; no recapture refused.
  `diag21-login-recapture.log`: 12 of 12 pass on CPUs 2,3.
- Pinned rate runs (race binaries, `taskset -c 0,1`, `GOMAXPROCS=2`, one
  process per run), `rate09` at `6281a4ea` and `rate19` at the final
  rules: `TestRunServeDirectAnchorsFencedAndConflictRestart` 10/10 and
  10/10; `TestRunServeDirectAnchorsBootstrapTransportsRestartAndDisabled`
  9/10 (a zone status GET answered 503 `generation_pending`; the helper briefly repeated that read, which the review removed)
  and 8/10 (two sign-ins answered 503 at load about 15, before the
  error key was logged, so whether they were capture refusals or a
  bound that ended is not known); `TestEnrolledProcessConflictResolution`
  5/5 and 5/5, `TestStaticClusterProcessesReadOnlyPeerLossRestart` 5/5 and
  5/5, `TestAnchorUpgradeStaticTwinV010` 5/5 and 5/5,
  `TestV01BinaryRefusesEnrolledDirectory` 5/5 and 5/5.
- Mounted load (signed UPDATEs at 10 per second, sign-ins and record-set
  writes one after another, each sent once): `load13-compare60-nonrace.log`
  (60 + 60, writer's own first sign-in retried in both variants):
  with D399 0 refusals "current writable serving generation unavailable",
  30 capture refusals (`generation_pending`); without recapture 7 such
  refusals and 24 capture refusals. In `load05`/`load06` (200 + 200,
  non-race) the overlay without recapture failed the very first sign-in
  with that refusal in 2 s; with D399 none of 400 was refused that way,
  but 225-254 were refused at capture and 29-34 sign-ins answered
  "authentication operation unavailable" (the session committed, its
  publication did not arrive within the 5 s seal wait).
  `race16`/`race20` (race, 40 + 40): 53 of 80 refused at capture, 2
  refused after the 5 s bound, 12 seal timeouts, and in a paced variant
  the node published nothing for a whole minute.
- `lint23.log`: golangci-lint v2.15.0 over `./internal/servinghttp/...`
  and `./cmd/kudurru/...`: 0 issues. `gofmt -l` empty, `go vet` clean,
  `go test -race ./internal/servinghttp` ok, bench guards OK. No
  generated artifact changed.

- Review (`review-02-pinned-bootstrap.log`, HEAD `5f295cff`, race binary,
  `taskset -c 0,1`, `GOMAXPROCS=2`): `TestRunServeDirectAnchorsFencedAndConflictRestart`
  5 of 6; run 6 failed `POST /auth/login: status=503 error="generation_pending"`
  8.7 s into a fresh start, a capture refusal before the request was read.
  That is very likely the class of the two unexplained sign-in 503s of
  `rate19`. Bootstrap 12 of 12 there.
- Review fixes (`eff1ac40`..`10587ce2`):
  - `fix-02-servinghttp-recapture.log` (race):
    `TestRecaptureAdmitsOnlyAnUnchangedRequest`, 8 subtests, pass (43.5 s):
    an unrelated commit is admitted (new revision and publication, proof
    kept); a cookie session revoked by the superseding commit, a fence
    observed while waiting, an exact-source request after a commit that
    changed the source, a request that already dispatched, a proof of
    another request and a `WithoutRecapture` caller are each refused with
    the original lease kept; an exact-source request is admitted on a
    republication of the same generation (`Publisher.Wake`).
    `fix-03-servinghttp-mutation-without-exact-authority-marker.log`
    (overlay disabling the exact-source check, the `AdvancedTo` check and
    the marker): the exact-source and marker subtests fail; the fenced
    subtest still passes, because a fenced node publishes only DNS-only
    bundles that `CaptureHTTP` never returns, so the `AdvancedTo`
    comparison is a second line that no fixture reaches (a term change
    that stays writable would need a promotion fixture).
  - `fix-06-race-command-tests.log` (race, `2c075021`, 192.9 s, 19
    top-level tests pass): the D399 tests, the new
    `TestServingCommandRecaptureNeverRepeatsADispatchedCommand` (refused at
    commit, uncertain outcome after commit: body once, `1 0 1`, no
    `Retry-After`), `TestServingCommandRecaptureIntegrationOperationRefusesAtOnce`
    (`1 0 0`, refused without waiting) and
    `TestServingCommandRecaptureJoinTokenExactSource` (changed source:
    refused once, no audit row, enrollment unchanged; republished source:
    `2 1 1`, one invitation, one audit row via `checkJoinToken`), plus
    `TestServingItemsFirstItemKeepsTheOriginalSource`, every
    `TestIntegrationOperation*` and `TestClusterJoinToken*`; bound subtest
    refused after 5.011 s, 7 recaptures, `Retry-After: 1`.
  - `fix-10-before-fix-overlay.log` (overlay restoring the integration
    no-op without the marker and `Retry-After` on any `ErrPending`): the
    in-body, at-commit and uncertain subtests fail ("a refusal after
    dispatch answered Retry-After"); the integration test fails with
    `attempts=2 recaptures=1 dispatches=1` and the host then fenced the
    operation ("integration host: incarnation fenced").
  - Mounted, non-race, `TestRunServeCommandsNotRefusedAsSupersededUnderSteadyUpdates`
    (10 signed UPDATEs per second, 30 sign-ins and 30 record-set writes,
    each sent once after a read that sees a current publication):
    `fix-07-mount-with.log` at `2c075021`: run 1 failed on one sign-in
    answered "current writable serving generation unavailable" after
    5.274 s (the whole bound; 26 + 25 admitted, 7 capture refusals, 1
    unacknowledged sign-in), run 2 passed (27 + 29, 4 capture refusals).
    `d5fc108d` then classifies an answer of at least the 5 s bound as
    publication starvation. `fix-12-mount-with-x3.log`: 3 of 3 pass
    (30 + 30, 30 + 30, 28 + 28 admitted; 0 refused as superseded; 0, 0
    and 1 after the bound; 0, 0 and 3 at capture). Without recapture
    (`overlay-norecapture-fix.json`): `fix-08` run 1 failed at the
    writer's sign-in (62 ms), run 2 had 13 quick refusals (23 to 308 ms),
    `fix-13` 17 (22 to 160 ms): 3 of 3 fail. Across all runs 30 to 127 of
    the signed UPDATEs answered SERVFAIL.
  - `fix-14-anchor-pinned.log` (race binary at `10587ce2`, `taskset -c 0,1`,
    `GOMAXPROCS=2`, 6 runs each): `TestRunServeDirectAnchorsFencedAndConflictRestart`
    6 of 6; `TestRunServeDirectAnchorsBootstrapTransportsRestartAndDisabled`
    5 of 6, run 1 failed `control GET /objects/dnszones/anchor:
    status=503 error="generation_pending"` at the zone status read that is
    single-shot again (`serve_anchor_activation_test.go:129`), the capture
    refusal that remains open.
  - `fix-05-vet.log` and `fix-15-lint.log` (golangci-lint v2.15.0 over
    `./internal/servinghttp/...` and `./cmd/kudurru/...` at `10587ce2`): 0
    issues (`fix-11-lint.log` at `2c075021` had one revive finding in the
    new test helper, fixed by `10587ce2`). `fix-09-bench-guards.log`: OK.
    `gofmt -l` empty. No generated artifact changed.

Open. CI-RED-2 is not closed by this package. D399 removes the refusal
"current writable serving generation unavailable" for a command that was
read and then superseded before dispatch (the mounted test above fails 3 of
3 without it and passes 3 of 3 with it). It does not touch the capture
refusal: a request that arrives before the node has published its newest
commit is refused before it is read with `503 generation_pending`, also on
a ready node, and that is the residual hosted sign-in 503
(`review-02-pinned-bootstrap.log`, `fix-14-anchor-pinned.log`). Under a
steady stream of commits the publisher falls behind (`Publisher.Run`
cancels the preparation in progress on every generation change and every
`Wake`), so requests are refused at capture, sign-ins whose session
committed answer "authentication operation unavailable" when the 5 s seal
wait sees no publication, a command can use its whole 5 s bound, and many
signed UPDATEs answer SERVFAIL. That needs its own core package (for
example not canceling a preparation on `Wake` alone, pacing the node's own
follow-up commits, or letting a preparation finish before a newer one
starts); the original acceptance (3), 200 sign-ins and 200 record-set
writes all succeeding under 10 signed UPDATEs per second, stays open
against it, and the committed mounted test only asserts the D399 part.
The mounted test was run non-race only. `TestServingHTTPTOTPLoginPasswordConsumeExactlyOnce`
is timing-sensitive under load (401 instead of 429; the review measured 5
of 6 failures at load 15 to 24 with and without recapture,
`review-04-totp-compare.log`): it assumes the second TOTP sign-in arrives
within the 1 s factor limiter, a test-timing defect outside this package.
The signed UPDATE decline is for the integrator to confirm. The
integrator's full `./cmd/kudurru` run and the four e2e tests were not
re-run after the review fixes.
