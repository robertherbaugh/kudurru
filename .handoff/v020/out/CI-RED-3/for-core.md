# CI-RED-3 notes for the core lane (CORE-COMMIT-GAP)

Two findings from CI-RED-3 touch the "no answer / refusal after a canonical
commit" family. Neither changes D313/D332/D378; the first is fixed in the ci
lane, the second is measured only.

## 1. TSIG refusals were held to full data currency (fixed here: c51b7d06)

Hosted failure: `TestRunServeSQLiteStoredTSIGTransportsAndEncryptionRestart`
on ecd633b1, `serve_tsig_transport_mount_test.go:114 read original native DNS
frame length`. The `signed restart exchange` lines in that log are `t.Logf`
progress lines, not failures: every signed exchange of phases 0-2 was answered.
Only the phase-2 TCP query signed with the replaced key (expected NOTAUTH
BADSIG) got no answer.

Mechanism: a failed TSIG check is answered by the transport
(`verifyTSIG` for UDP/TCP/DoT, `encryptedSigner.failure` for DoH/DoH3/DoQ)
before any pipeline work, so `BindResponse` never ran for it. An unbound
`rootAnchorOutput` emits only under `seal.current` (full data currency), so
any canonical commit whose publication is not yet installed withheld it. The
ordinary answers of the same moment use `servedCurrent` (D378) and were sent.
This is not the authoritative-answer window of CORE-COMMIT-GAP: in the same
measurement unsigned authoritative answers were all answered.

Evidence (`evidence/CI-RED-3/`):
- `tsig02-badsig-after-commit.log` (non-race, mounted `runServe`, diagnostic
  test `scripts/zz_cired3_badsig_test.go`): quiet node 0/20 BADSIG unanswered;
  right after a signed UPDATE 8/30 unanswered (TCP, 1 s timeout), every loss
  after an UPDATE answered SERVFAIL; the unsigned authoritative query sent
  after it 30/30 NOERROR (sent after the BADSIG timed out, so not a same-moment
  comparison).
- `tsig07-dnsserver-refusal-binds-before.log`: the new
  `TestCapturedTSIGRefusalBindsOutput` without the fix: no answer on
  UDP, TCP, DoT, DoH, DoH3 and DoQ. `tsig06-...-after.log`: passes 3/3 with it.
- `tsig08-root-refusal-served.log`: `TestRootAnchorTSIGRefusalFollowsTheServedPublication`
  holds the publisher across a commit: the bound refusal is sent, an unbound
  output is withheld.

Fix: `dnsserver.bindRefusal` binds the refusal message (a plain
`pipeline.Response{Msg: m}`) before emission; it then takes the served
class for an ordinary QUERY. UPDATE/NOTIFY/transfer refusals keep full
currency (unchanged). If CORE-COMMIT-GAP changes the served policy or the
output classes, this path now follows it automatically.

Not covered by the fix, same shape: `Handler.resolve` returns
`servfail(r)` without binding when the pipeline returns an error or no
message, so that SERVFAIL also stays unbound and is withheld after an
unpublished commit. Not changed here (no hosted failure points at it).

## 2. Signed UPDATE acknowledgments go unanswered under race (measured only)

In the mounted refusal test under `-race` on this loaded machine (load
25-35), a TCP UPDATE got no answer within 5 s in 5 of 10 runs before the
test accepted it (`tsig04-refusal-test-after-fix.log`: "signed UPDATE N
failed: read tcp ...: i/o timeout"; `tsig03`: also one EOF). The updates are
new requests, one at a time, about 50 ms apart. With the final test, which
records such an UPDATE as "rcode -1" and goes on, 2 of 288 UPDATEs went
unanswered in 8 runs (`tsig09-mounted-after-fix.log`), and all 288 refusals
were answered. This is the withheld
acknowledgment of D313 across a later commit; the test now records it and
does not depend on it. A client behind it times out.

## 3. Commands refused 503 by the node's own post-zone-write cascade

`TestEnrolledProcessConflictResolution` (e2e, ecd633b1):
`POST /cluster/activate: status 503 want 200, error unavailable` 4 s in.
A zone write is followed by the node's own commits: recordset/zone status,
`PrimaryStatus.Reconcile`, three `LifecycleCAS` (Preparing, Fencing, Active),
`Admission`, and a `LifecycleCAS` that realigns the lifecycle source after
every zone status write. Under race they land for about 11 s after the zone
create, up to 1.75 s apart (`e2e02-diag-inprocess-zone-commits.log`, store
generation changes with callers). A command whose publication one of them
supersedes is refused 503 `unavailable` "current writable serving generation
unavailable" (or `generation_pending`), with no effect: in that in-process
diagnostic 3 of 6 runs had the recordset POST right after the zone POST
refused this way. In the e2e diagnostic (`e2e01-diag-source-timeline.log`)
1 of 8 runs saw the reviewed source change 230 ms after the review while the
zone and recordset RVs stayed the same (an internal lifecycle commit). This is
the CI-RED-2 open item (commands refused 503 when another commit publishes
while they run), with no concurrent client at all: an operator who creates a
zone and then a record in the UI can meet it. The three e2e tests that activate after
a zone write (`TestEnrolledProcessConflictResolution`,
`TestEnrolledClusterProcessesPromoteAndRejoin`,
`TestV01BinaryRefusesEnrolledDirectory`) now review only after the source
has been quiet for 3 s. That is a stopgap, not a readiness proof: 20 of 20
pinned local runs pass both with and without it (`fix-01`, `fix-02`). The
product is unchanged and needs the decision CI-RED-2 proposed.
