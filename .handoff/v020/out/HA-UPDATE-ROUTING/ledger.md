### UPD-1 / G21-04 RFC 2136 UPDATE routing from enrolled SQLite followers — November 10, 2026 (D381)

Before this change, an enrolled SQLite follower answered every UPDATE with
SERVFAIL, because its local driver is not the ready writer. Now, when the
follower's own source is in follower mode and the member forwarder is mounted,
`servingRuntime.forwardUpdate` handles the UPDATE. It selects the zone in the
follower's own complete cut (NOTAUTH if the zone is not a primary zone there)
and uses the listener's verified TSIG proof. It admits the key against the
follower's current keyring (REFUSED otherwise). It then sends one version-1
`dns.update` member command. The command carries the exact request without its
TSIG record and a `TSIGKey`/`tsig` subject with the key name and its
secret-derived credential version. It also carries the algorithm, transport and
signature deadline. The exchange is bounded by four seconds, the DNS request
and the signature deadline.

On the primary, `executeMemberUpdate` decodes the payload strictly and requires
the driver to be the ready writer. It binds the elected writer and captures the
current serving lease. It then runs the same protocol adapter as a native
UPDATE. In `captureUpdate`, the forwarded branch admits only the exact key
identity in the primary's own current keyring (name, algorithm and credential
version), and `captureForwarded` issues a process-private proof. Zone selection,
grants, prerequisites, the atomic commit and publication are unchanged. The
member command ID is the update's retry identity in the zone's receipt ledger,
so a repeated delivery of the same command returns the retained rcode without a
second commit. The zone runtime commits in its own transaction, fenced by the
writer, the term and the admitted key identity. Unlike a generic member
command (D362), the sender's current enrollment is rechecked in a transaction
only when the acknowledgment is admitted, not before the effect: a member
revoked in the milliseconds between `CaptureMemberRequest` and `Runtime.Apply`
still commits, and only its answer is withheld. Such a member holds the TSIG
secrets and could send a native UPDATE, so the exposure is small, but it is a
deviation from the recheck-at-commit pattern.
The rcode is acknowledged inside the member request's command output scope: an
empty `MemberCommandWithOutput` runs after the zone runtime's own fenced
commit. The follower relays that rcode, signed with the request's key. Its
answer carries a forwarded acknowledgment (`completeForwardedUpdate`) bound to
the verified key, current dependencies and the unchanged empty response instead
of the follower's data generation, so its own pull of the commit cannot withhold
the answer. If the command was not sent, was refused by member admission, or
its exchange was lost, the follower answers SERVFAIL (RFC 2136 section 4.5:
try the next server) and nothing retries or replays it. Fenced, conflicted,
standby and static nodes keep their local SERVFAIL. The member command format
gained a version-1-only `TSIGKey` subject kind and `tsig` credential kind.

Evidence under `.context/v020-final-20261110/evidence/HA-UPDATE-ROUTING/`:

- `e2e04` (committed head dd2e3b53, `KUDURRU_E2E=1 go test -count=1 -v -run
  TestEnrolledClusterProcesses ./test/e2e/`): 2 passes, no skips, 290.9 s.
  - `TestEnrolledClusterProcessesPartitionCatchupAndRestart` (123.5 s): signed
    UPDATEs to the follower over UDP and TCP answered NOERROR, signed, and were
    served by both nodes. A failed prerequisite answered NXRRSET and an
    ungranted TXT type answered REFUSED, both signed and neither written. While
    partitioned, the follower answered SERVFAIL within the bound, and the update
    was absent on both nodes after reconnection and convergence.
  - `TestEnrolledClusterProcessesPromoteAndRejoin` (160.8 s): everything above.
    The fenced former primary answered SERVFAIL to a signed UPDATE that was
    never applied. The new primary applied its own UPDATE. After rejoining, the
    former primary forwarded UPDATEs (NOERROR, NXRRSET, REFUSED) to the new
    primary.
  - The new primary's own UPDATE got one SERVFAIL right after the forwarded
    settings patch. Its lease predated the republication and failed the writer
    check in `captureUpdate`. This is the window HA-OPEN recorded for queries.
    It is not occasional: it happened in every passing run of this sequence
    (`e2e04`, the reviewer's `review-e2e01` and `review-e2e02`, and `fix-e2e01`,
    `fix-e2e03-diag` and `fix-e2e05` below), and `e2e01` is the same event as a
    failure. The test repeats that idempotent insert until NOERROR and logs
    each SERVFAIL, so a later fix shows as zero such lines.
- `race04` (`go test -race -count=1 -v -run
  'TestRootMember|TestPrimaryRuntime|TestPrimaryUpdateIngress|TestRootUpdateOutput|TestRunServeSQLiteStoredTSIGNativeUpdateTransferNotify'
  ./cmd/kudurru`): 35 passes, no skips, 152.5 s. This includes the new
  `TestRootMemberUpdateCommandActualHTTP`: the mounted member listener answers a
  forwarded UPDATE for an unserved zone with NOTAUTH inside its output scope.
  Five malformed forwards (signed message, unknown transport, unknown field,
  non-canonical key name, other provider) are aborted without a store change.
  `race01` and `race02` are earlier passing runs of the same selection and of
  the new test alone.
- Review fixes (after review `review-e2e01`, `review-e2e02`, `review-race01`,
  `review-replica01` at dd2e3b53):
  - `fix-race04` (`go test -race -count=1 -v -run
    'TestRootMember|TestPrimaryRuntime|TestPrimaryUpdateIngress|TestRootUpdateOutput|TestRunServeSQLiteStoredTSIGNativeUpdateTransferNotify'
    ./cmd/kudurru`): 36 passes, no skips, 153.0 s. It includes the new
    `TestRootMemberUpdateKeyIdentityAndRetryActualHTTP`. The test creates the
    primary zone `example.` through the forwarded object path and sends
    forwarded UPDATEs through the mounted member listener. Another credential
    version, an unknown key name and the other algorithm each answer REFUSED
    with no commit. A signature deadline in the past answers SERVFAIL with no
    commit. A "name not in use" prerequisite plus an insert answers NOERROR and
    advances the zone's commit sequence by exactly one. The same command ID sent
    again returns the retained NOERROR without a commit, and a new ID for the
    same request answers YXDOMAIN. `fix-race01` to `fix-race03` are the test's
    development runs: `fix-race01` failed in a setup hook that recreated the
    zone before start (the harness then saw a pending generation), and
    `fix-race02` failed because a command ID contained spaces.
  - Mutations, each reverted: `fix-mutation01-no-key-check` removed the key
    identity comparison in `captureUpdate`, and the new test failed (another
    credential version answered SERVFAIL, not REFUSED).
    `fix-mutation02-fresh-retry-id` gave each forwarded capture a fresh retry
    ID, and the new test failed (the repeated command ID answered YXDOMAIN).
    The expired-deadline case is enforced in three places (`captureForwarded`,
    the proof's `Verify` and `Apply`'s signature-deadline context). The test
    proves their combined effect, not each one.
  - `fix-e2e01` (the tree later committed unchanged as 3d159258) and `fix-e2e05` (head b2b20460):
    `KUDURRU_E2E=1 go test -count=1 -v -run TestEnrolledClusterProcesses
    ./test/e2e/`, 2 of 2 passes each, no skips, 302.6 s and 300.6 s. Each
    follower scenario now also sends eight committing UPDATEs to the follower
    back to back (`enrolledFollowerBurst`), three bursts per run. In
    `fix-e2e01` the counts were 8/0 (partition variant, NOERROR/SERVFAIL),
    then 3/5 and 4/4 (promote variant, via the follower and via the rejoined
    former primary). In `fix-e2e05` they were 8/0, then 3/5 and 8/0. The
    SERVFAILs were consecutive, 5 to 7 ms apart, at the end of a burst. Every
    NOERROR was served by both nodes. So the reviewer's prediction holds:
    back-to-back forwarded UPDATEs do get transient SERVFAILs, but the cause is
    different from the one predicted (see the diagnostic runs).
  - `fix-e2e04` is retained and incomplete. The agent's background time limit
    stopped it during the promote variant, after most of the limit was spent
    waiting for the heavy lock. The partition variant passed, with a burst of
    8/0.
  - `fix-e2e02-diag` and `fix-e2e03-diag` ran with temporary, uncommitted
    logging, and the burst failed on any SERVFAIL so the node logs were
    printed. Both failed at that intentional check (counts 6/2 and 6/2 in
    `fix-e2e02-diag`, and 6/2, 8/0 and 8/0 in `fix-e2e03-diag`). They show that
    the burst SERVFAILs come from the primary, not from the follower: the
    follower's forward capture never failed. The primary's protocol adapter
    answered SERVFAIL, and `fix-e2e03-diag` logged the reject site: the first
    `CheckWriter` in `captureUpdate`, which runs before `Apply`, so nothing was applied. Each
    failing update came 5 to 10 ms after the previous commit, while a
    successful one took about 90 ms. This is the same writer-lease window as
    the native SERVFAIL above.
  - `fix-lint01`: scoped golangci-lint v2.15.0 over `cmd/kudurru` and
    `test/e2e` reports 0 issues.
- `replica01` and `race03`: `TestCommandTSIGSubjectOnlyInVersionOne` passes,
  and the full `internal/cluster/replica` and `internal/servingdns` race runs
  pass.
- `lint02`: scoped golangci-lint v2.15.0 over `cmd/kudurru`,
  `internal/cluster/replica`, `internal/servingdns` and `test/e2e` reports 0
  issues. `lint01` is retained: it flagged an untyped constant in the new
  constant group (SA9004), which was fixed.
- Retained failed run `e2e01`: the promote variant failed on the new
  primary's own UPDATE (SERVFAIL). `e2e02-diag` and `e2e03-diag` ran with
  temporary, uncommitted logging. They traced the failure to the writer-lease
  check after republication, showed that every forwarded UPDATE had the
  expected outcome, and showed that the next attempt succeeds.

Still open: the SERVFAIL of an UPDATE on a primary while it republishes. A
lease captured before the republication fails `CheckWriter`. It is
reproducible on every run of the promote sequence (native UPDATE after a
forwarded settings patch). Bursts of forwarded UPDATEs also hit it, up to five
of eight back-to-back updates in some bursts (`fix-e2e01`, `fix-e2e05`). No
update is applied in that case, so a client retry is safe. This belongs to the
same DNS-path follow-up as the HA-OPEN query window (the lease window on the
DNS path) and was not changed here. `docs/deploy/dynamic-updates.md` and
`docs/architecture/zones.md` now describe it. Membership is rechecked when the
acknowledgment is admitted, not before the effect (see above). This is
recorded in D381 and was not narrowed, because the store has no read-only
recheck of a captured member request. A forwarded UPDATE expires at most four
seconds after the follower sends it, by the primary's clock. A primary clock
four or more seconds ahead fails every forwarded UPDATE with SERVFAIL, and the
operator guide now says so. The partition relay accepts and then closes the
connection, so the e2e partition exercises the lost-exchange answer. The
never-dialed answer is the same SERVFAIL, and only the shared forwarder's unit
tests prove it. A forwarded UPDATE does not write an audit row, just as a
native UPDATE does not. The full `./cmd/kudurru` package was not run, only the
selection above.
