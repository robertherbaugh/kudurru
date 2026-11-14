### HA-OPEN5 What verification left open in the three follower packages, and a large apply through a follower — November 13, 2026 (D381, D375)

**Forwarded UPDATE bursts (D381).** A burst of signed UPDATEs sent through an
enrolled follower, back to back as a DHCP server sends them, is now answered
`NOERROR` for every update the primary accepts. Before, once a commit was
being republished, most of the rest of the burst answered SERVFAIL (up to 6 of
8 in the HA-UPDATE-ROUTING verification; the verifier was right that the
comment in `test/e2e/enrolled_update_test.go` named the wrong cause, and the
docs and changelog draft understated it). Diagnostic runs with temporary
logging (`e2e02`, `e2e03`, `e2e05`, `e2e06`, `e2e08`; reverted before commit)
found four separate refusals, none of which had decided or committed
anything: the follower's keyring check after it installed the previous
update's commit, the primary's writer check on a lease being replaced, the
primary's zone admission while its lifecycle was being realigned after a
status write, and `Apply` running out of compare-and-swap attempts against
those writes (`primaryruntime: admission conflict`, committed=false). Now the
follower waits, inside the forward's four-second bound, for its own current
publication (`awaitFollowerPublication`), selects the zone and admits the key
again there and captures the forward on that source; a command is sent at
most once. The primary runs the command on its current writer publication
(`runMemberUpdate`, `Publisher.WaitTerm`) and runs it again after the next
publication only when `captureUpdate` rejected it for publication state alone
(`forwardedUpdate.stale`, set before admission) or when `Apply` lost every
compare-and-swap and committed nothing (`pr.ErrConflict`, observed through the
new `network.WithOutcome`). Every other answer is final, a committed but
unpublished one included; the command ID stays the retry identity in the zone's
receipt ledger. A native UPDATE to the primary is unchanged: right after a
commit it can still answer SERVFAIL without being applied (the e2e logs it
for `promoted.dyn.test.` on every run). What a client sees through a follower
now: NOERROR (or the primary's own refusal) for every update, after a wait of
up to the four-second bound; a burst of 8 took 1.2 s to 14.1 s on this loaded
machine (up to about 1.8 s per update on the rejoined former primary). An
update that is not sent within the bound, or whose exchange is lost, still
answers SERVFAIL and is never retried. CI-RED-2's
`TestRootMemberFollowerUpdateAcrossInstallAnswersServfail` pinned the old
follower SERVFAIL; it now requires the held UPDATE to be sent once on the
newer publication and answered NOERROR, and is renamed
`TestRootMemberFollowerUpdateAcrossInstallWaitsForItsPublication`.

**Refused write on neither node (HA-OPEN finding 2).** The check no longer
tolerates an unanswered query. `enrolledAbsent` requires every query over UDP
and TCP to be answered (an unanswered one fails at once) and allows an answer
that differs for at most two seconds; it is used for the refused-fenced write
and for the prerequisite and ungranted updates. The cause HA-OPEN found (a UDP
answer resolved from a replaced generation was refused by the guarded
emission and nothing was sent) is fixed on the release branch by binding an
ordinary answer to its publication and sending it across a swap (D378, D395,
commits `10d3ab5b`, `f0d2bca0`, `444ec28c`), which this lane does not carry.
The strict check passed in all three promote runs of `e2e09` on this lane too.

**Large apply through a follower (D375).** Chaining a follower's items on the
primary's receipts was examined and does not hold with the present protocol:
the primary's chain compares an in-process coordinator mark by identity, and
a follower would need a serializable receipt in every member outcome, a new
command field checked by the primary before admission, and a rule for
accepting a newer installed image and lease (its admission and every
forward's pins are bound to the request's publication) in the middle of a
request. That is recorded in the D375 row as rejected for v0.2. Making a
repeat finish the file was rejected too: it needs either an `unchanged`
answer decided from the follower's own data (wrong for a key repeated in the
file) or no deduplication record for an `unchanged` result (removes the
replay guard). Instead the refusal says exactly what to do: an item of a
multi-item request that a follower cut (marked by the new
`api.InObjectItems`) answers `unavailable` "the follower's source changed
while this request ran (...); this item was not sent and was not applied,
and neither was any later one; send the items that were not applied, or the
whole file, to the enrolled primary <name>, which applies them in one
request; running it again through this follower stops in the same way". A
forwarded write whose answer arrived but could not be relayed because the
follower's own source or membership changed during the exchange now answers
`completion_unknown` ("the write may have been applied") instead of
`unavailable` "cluster authority changed", which said that nothing was made;
in a multi-item request it adds that the later items were not applied and
names the primary. The cluster guide also no longer says that a follower does
not pass RFC 2136 updates on.

**Self-service low items (HA-SELF verification).** A token caller's forwarded
password change is answered for the original principal with
`mustChangePassword` cleared in every case, as the primary's route answers,
instead of from the token's own authentication with any failure treated as
`revokeTokens`. A cookie caller's `committed_output_unavailable` now names
the cause (bound ran out, cookie settings changed, other seal refusal). The
reviewer's "response-write error after the cookie was set" cannot occur:
`servingHTTPJSON` reports no write error, and every seal refusal comes before
the cookie and the answer are written. The mounted follower test
`TestRootMemberForwardPasswordAndTokensActualHTTP` now checks the primary's
audit rows: two attributed LocalUser rows of one member command for the
password change, one APIToken row each for creation and revocation, none for
the refused change or the refused revocation.

Evidence under `.context/v020-final-20261110/evidence/HA-OPEN5/`:

- `race07-update-fixed-x3.log`: `TestRootMemberUpdate*` under `-race
  -count=3`, 8 of 9 passed; the failure is the pre-existing
  `TestRootMemberUpdateKeyIdentityAndRetryActualHTTP` "a repeated command ID
  committed a second time 2 3".
- `race08-update-base-overlay-x4.log`: the same tests with the previous
  `primary_runtime.go` and `serving_member_update.go` (overlay):
  `TestRootMemberUpdateBurstActualHTTP` fails 4 of 4 (SERVFAIL in the burst),
  and the key-identity test fails 1 of 4 with the same "2 3", so that flake
  predates this package (an unrelated head commit of the zone lands between
  the test's two reads; the dedup itself is not at fault, as the retained
  NOERROR shows).
- `race01` to `race06`: development runs of the burst test, retained;
  `race02`-`race06` contain temporary diagnostics, and the concurrent variant
  in `race06` was dropped (concurrent member commands hit the harness's
  six-second client timeout and the member listener's admission, outside this
  package).
- `race09-self.log`, `race10`/`race11`, `race12`/`race13`: self-service tests.
  `TestRootMemberForwardTokenCallerPasswordActualHTTP` failed 2 of 9 runs of
  the changed code while two heavy runs shared the machine (the fallback
  `CaptureHTTP` after the five-second wait was refused) and passed 5 of 5
  alone (`race13`); the previous code passed 8 of 8 (`race11`, `race12`). The
  changed lines do not touch that fallback. Review fix `84f54bf9` makes the
  fallback wait for a publication being prepared (see below).
- `race14-follower-wait.log`, `race15-apply-refusal.log`: the stale-follower
  test, the item-refusal unit tests and `TestRootMemberForwardApply*`,
  `TestServingItems*`; race15 passed 14 of 14.
- `race16-root-selection.log`: `-race` selection `TestRootMember|
  TestMemberForward|TestMemberLogout|TestPrimaryRuntime|
  TestPrimaryUpdateIngress|TestRootUpdateOutput|
  TestRunServeSQLiteStoredTSIGNativeUpdateTransferNotify|TestServingItems|
  TestObjectForwardError|TestMemberItemRefusal|TestCLILogin` on the final
  code: 68 passed, 0 skipped, 605 s.
- `e2e09-final-x3.log`: `KUDURRU_E2E=1 go test -count=3 -run
  TestEnrolledClusterProcesses ./test/e2e` on the final code: 6 of 6 passed,
  0 skipped, 1268.6 s; every burst all NOERROR (8 of 8 each, 1.2 s to 14.1 s).
  `e2e01`, `e2e04` (failed bursts before the follower wait and the stale
  admission), `e2e07` (build error) and the diagnostic runs are retained.
- `e2e10-conflict.log`: `TestEnrolledProcessConflictResolution` on the final code: passed, 0 skipped, 121.4 s.
- `light01-api-network-race.log`: `internal/api` passed;
  `internal/primaryruntime/network` failed once in
  `TestActualNotifyAncestorExpiryInvalidatesBoundChildWithoutNewGeneration`
  under load, and passed 4 of 4 with and without the change (`light02`,
  `light03`).
- `lint02.log`: scoped golangci-lint v2.15.0 over `./cmd/kudurru/...`,
  `./internal/primaryruntime/network/...`, `./internal/api/...`,
  `./test/e2e/...` on the final code: 0 issues (`lint01` did not run: another
  lane's lint held its lock). `gofmt -l` is empty, `go vet` clean on the
  touched packages, and the bench guards pass (14 tests).


**Review fixes (accept-with-fixes review of November 13).** Six source
commits on top of `9d2c42f4`, no generated artifacts:

- `4a98737f` The stale-follower test
  (`TestRootMemberFollowerUpdateAcrossInstallWaitsForItsPublication`) failed
  4 of 7 race runs in review. Its held request's DNS deadline (five seconds
  from receipt, `dnsserver.DefaultQueryTimeout`), the client's five seconds
  and the follower's four-second forward bound all also covered the fixture's
  own hold (a commit on the primary and a pull). The mounted follower fixture
  now allows 30 seconds for pipeline and client, and the follower's forward
  bound is replaceable in tests (`servingRuntime.memberForwardUpdateBound`,
  zero keeps four seconds; 25 seconds here). The assertions are unchanged
  (NOERROR, signed, served once, YXDOMAIN on a repeat). The four-second bound
  itself is exercised only by the process test (`e2e09`, `review-e2e01`).
- `472a9f79` `TestRootMemberUpdateKeyIdentityAndRetryActualHTTP` compared
  the zone head's `CommitSequence`, which also advances on a head commit that
  changes no data (the root digest also binds expiry, configuration and
  receipts; a NOTIFY rearm is another). It now selects the head of the
  current DNSZone by UID and compares content hash and serial, which only a
  data commit changes (`zonestate.Prepare`). Which head commit moved the
  sequence in the retained failures was not identified.
- `56f06b45` `memberItemRefusal` recognized a cut by `strings.HasPrefix` on
  the message; it now compares memberCaptureError's exact answer through the
  shared constant `memberSourceChanged`. `TestMemberItemRefusalNamesThePrimary`
  adds a refusal that only starts the same way and must stay unchanged (the
  old prefix match rewrote it), and `TestMemberSessionUnservedNamesTheCause`
  covers the three causes of a cookie caller's `committed_output_unavailable`.
- `84f54bf9` A token caller's committed password change, after the
  five-second wait, captured once; while the follower was still preparing a
  publication (`ErrPending`) it answered 503 `committed_output_unavailable`
  (the review's flake of `TestRootMemberForwardTokenCallerPasswordActualHTTP`,
  `took_ms` about 6000). The fallback now waits for the publication being
  prepared, within another five seconds and the route's deadline; the
  architecture page says so.
- `c8c9e3b1` `captureUpdate` marked every non-active phase as stale; a
  retiring or retired zone is now refused at once, since a lifecycle never
  returns from retirement. The zones page now lists every stale cause and the
  rerun after a lost compare-and-swap (it said that nothing that reached
  `Apply` runs again, which was wrong for `pr.ErrConflict`).
- `7ac4f5a2` lets `TestRootMemberUpdateBurstActualHTTP` raise the member
  harness client's six-second timeout to 25 seconds, above the commands' own
  20-second bound: in `fix-race02` one burst command was reported as
  unanswered ("Client.Timeout exceeded while awaiting headers") while it was
  still inside its bound.

Review evidence under `.context/v020-final-20261110/evidence/HA-OPEN5/`:

- `fix-race01-follower-wait-x10.log`: the stale-follower test under `-race
  -count=10`, load average 13.7 to 15.4: 10 of 10 passed, 19.5 s to 24.0 s
  each.
- `fix-race02-touched-x3.log`: `TestRootMemberUpdate*`,
  `TestRootMemberForwardTokenCallerPasswordActualHTTP`,
  `TestRootMemberForwardCookiePassword*`, `TestRootMemberForwardApply*`,
  `TestRootMemberForwardPasswordAndTokensActualHTTP` and the three unit tests
  under `-race -count=3`: 41 passed, 1 failed (the burst harness timeout
  above, retained). The token-caller test passed 3 of 3 and the key-identity
  test 3 of 3.
- `fix-race03-burst-dedup-token-x5.log`: on the final code,
  `TestRootMemberUpdateBurstActualHTTP`,
  `TestRootMemberUpdateKeyIdentityAndRetryActualHTTP` and
  `TestRootMemberForwardTokenCallerPasswordActualHTTP` under `-race -count=5`,
  load average 14.7 to 16.3: 15 of 15 passed, 0 skipped (burst 21.5 s to
  48.3 s, key identity 5.1 s to 12.9 s, token caller 17.5 s to 21.7 s).
- `fix-lint01.log`: scoped golangci-lint v2.15.0 over `./cmd/kudurru/...`: 0
  issues (before `7ac4f5a2`, which changes two test files only). `gofmt -l` empty, `go vet` clean, bench
  guards 14 of 14.

Not done in review, for the integrator:

- Direction (1): the strict refused-write check (`enrolledAbsent`) still
  relies on D378/D395 (`10d3ab5b`, `f0d2bca0`, `444ec28c`), which this lane
  does not carry. After the cherry-pick, run `KUDURRU_E2E=1 go test -count=5
  -run 'TestEnrolledClusterProcessesPromoteAndRejoin' ./test/e2e` on the
  release branch and keep the log; an unanswered query there means the cause
  is not closed.
- Direction (4) and D375 against D381: the D381 follower re-capture is for
  one UPDATE that has sent and decided nothing yet, and it is sent at most
  once. A per-item re-capture in `POST /apply` would continue a request whose
  earlier items committed, and admit later items against a newer view and
  newer dependency pins than the earlier ones within one request
  (`CheckReadLease`). Whether the primary's recheck of the retained raw
  credential makes that equivalent was not examined, and no variant was
  built or measured. Accept D375 as written for v0.2 and track chaining (or a
  per-item re-capture) as a follow-up, or ask for that evaluation.
- Direction (3a) still has no differential test: no test fails with the old
  `Authenticate` fallback (a token that stops authenticating for a reason
  other than `revokeTokens` during the wait was not built).
- No mounted test changes the follower's source between the primary's answer
  and the relay (`completion_unknown`); it is proven by
  `TestObjectForwardErrorAfterTheAnswerIsUnknown` on the mapping alone.
- A forwarded UPDATE for a zone that stays unadmitted for any cause other
  than retirement (admission deadline passed, a desired-only placeholder,
  routing unavailable) still waits for publications until the command's
  bound, so the follower answers SERVFAIL after up to four seconds instead of
  at once, with nothing applied. No mounted test ends the retry loop at its
  bound: the fixture has no way to hold a zone unadmitted.

Open: FWD-E2E stays open in the tracker. Its static-twin re-run, one
`primary_required` representative per primary-only group, the node-local
route assertions and the `/cluster` role check on the follower in the process
test, the Semgrep scan, the full `internal/api` and `internal/auth`
default-race runs and the frozen-source hash evidence directory were not done
in this package. The follower's apply cut remains: a
file of many documents through a follower stops at the follower's next pull,
and a repeat through the follower does not finish it; chaining is a member
protocol change for a later release. The native UPDATE SERVFAIL right after a
commit on the primary is unchanged (D378/D395 lane). The strict absence check
relies on D395 on the release branch; it was not run on a binary built from
that branch. The full `./cmd/kudurru` package was not run.
