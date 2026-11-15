### F28-RFC RFC 5011 interoperability with real authorities, live root check and timer position — November 13, 2026 (D398)

Managed root anchors are now qualified against real authorities, not only the
in-process miekg stand-in roots. Separate `kudurru serve` processes (the
interop harness's CGO-free release build) run with `anchorMode: managed`,
`managedTopology: direct`, the authority as their only numeric root hint and
no cache. They bootstrap over the production TCP acquisition from a root zone
that BIND 9 signs (dnssec-keygen and dnssec-signzone; RSA/SHA-256 2048,
ECDSA P-256 and Ed25519 KSK plus ZSK), or from one that Knot DNS signs online
(the digest-pinned `cznic/knot` 3.6.2 image, keys from keymgr). They then answer
AD-validated recursion over UDP and TCP for `www.example.` through a signed
child delegation and for the root DNSKEY set.

Acquisitions are counted exactly. For BIND the count comes from named's query
log, where each acquisition is a line `. IN DNSKEY -E(0)TD`: TCP, EDNS version
0, DO, RD clear, no CD and no cookie. For Knot it comes from a byte-for-byte
UDP/TCP relay, because knotd keeps no query log. A bootstrap makes exactly one
acquisition, and a restart before the refresh is due makes none.

Both servers answer the apex DNSKEY query with NSCOUNT 0 and only the OPT
record. BIND 9.18.50 does so by default and also with `minimal-responses no`,
so the strict parser (NSCOUNT 0, ARCOUNT at most 1) needs neither a fixture
setting nor a relaxation.

Refusals against real BIND data:

- A DS that matches no published key, a seed published already revoked (by
  dnssec-revoke, self-signed) and an expired DNSKEY signature each make one
  acquisition. The node stays unready, answers SERVFAIL and logs
  `root anchor updater stopped for this writer term`. A restart makes exactly
  one new attempt, and it is refused the same way.
- An Ed448 (algorithm 16) seed is refused by configuration before any
  acquisition.
- An Ed448 KSK that signs beside the seeded key is ignored: it never enters
  the managed state.

A BIND-made revocation is observed at the committed refresh. The TTL is 3600,
so the refresh comes at the one-hour minimum, and it is reached through the
existing `-tags licensetest` updater-due seam (`KUDURRU_TEST_ANCHOR_UPDATER_AHEAD`,
F28-HA-4). That seam moves only the due comparison: the acquisition, its
receipt sample, the authentication and the commit are production code.

- With two seeds, the revoked key becomes a tombstone, the other key stays
  Valid and validated recursion continues. This holds across a restart without
  the seam: two acquisitions in total.
- With one seed and the successor still in hold-down, the successor is dropped
  and the node quarantines and answers SERVFAIL, also after a restart without
  the seam. This case takes three acquisitions, for the reason in the finding
  below.

The RFC 5011 timing itself is proven in the pure package on real bytes.
`internal/trustanchors` replays root stages that BIND (dnssec-signzone with
explicit signature windows) and Knot (online signing driven through a keymgr
timeline) signed and served. BIND yields five distinct stages. Knot yields
four: it signs the DNSKEY set with K2 as soon as K2 is published, so its s2
already carries signatures by K1 and K2 and its s3 repeats s2. Only the BIND
capture therefore has "K2 published, signed by K1 only" and "K1 still
published, signed by K2 only". Each stage is the exact TCP reply to the
production request. The replay places them on a 39-day synthetic timeline
through a test-binary-only `SyntheticSample` (export_test.go) and checks:

- A new key becomes Valid at the first authenticated observation strictly
  after 30 days of accrued same-session hold-down. No extra poll beyond that
  one is needed, but an observation exactly at the threshold does not count:
  the key is still AddPend at day 31 (first seen at day 1) and Valid one hour
  later.
- Missing stays trusted and returns without a new hold-down.
- Self-revocation is immediate, and permanent across republication.
- A restart in the middle of the hold-down loses only the credit that was not
  checkpointed: 7 days kept and Valid at day 39 plus one hour, not day 31.
- Revoking the only trusted key while the successor is pending quarantines.

The resulting states are framed through `accepted.ProposeManaged`, and
`resolverstate.Project` yields exactly the successor's DS with the revoked ID
(and the Quarantined disposition with no DS). The committed captures under
`internal/trustanchors/testdata/rfc5011/{bind,knot}` carry provenance (tool
versions, commands, signature windows) and SHA-256 pins, so CI without the
tools still replays real bytes. A fresh BIND capture runs whenever named is
installed; `make interop-internal` now includes the package, and CI's e2e job
sets KUDURRU_REQUIRE_BIND. A fresh Knot capture runs with KUDURRU_KNOT=1
(`hack/test-knot-managed-root.sh`).

The opt-in live check `TestLiveRootManagedBootstrap` (`KUDURRU_LIVE_DNS=1`,
`./cmd/kudurru`) runs the production wire plan, the first complete TCP reply,
the strict bootstrap unpack, and `trustanchors.Bootstrap` with each shipped seed
against all 26 built-in root addresses. On November 13, 2026, all 13 IPv4
letters answered 1139 to 1141 bytes with NSCOUNT 0 and ARCOUNT 1. KSK-2017
(20326) and KSK-2024 (38696) were both published, only 38696 signed, and only
38696 authenticated (13 of 13). The 13 IPv6 addresses were unreachable from
this sandbox: there was no complete reply, which the test reports and does
not count. The release checklist now runs this check.

D398 records the timer position: production constants and no release clock
seam, qualification in the three layers above, and the documented deviations
from RFC 5011. Operators get a new page, `docs/deploy/managed-root-anchors.md`
(requirements and seed choice; enable order for standalone, enrolled and edges;
monitoring; recovery; deviations), linked from the deploy index,
`docs/deploy/dnssec-validation.md` and `docs/architecture/upstreams-and-cache.md`.

F28-GATE-2 tracker reconciliation (for the integrator to apply):

- gaps.md "Managed-anchor hosted DoH investigation" (:6849) and its
  "unexplained and open" line (:7071), completion-plan.md (:1553, :1775) and
  followup-audit.md (:3698, :3920). Close these with a7e70b22 ("order transport
  checks after primary status settles": a startup DNSZone status write retires
  in-flight answers by design, D313/D332), and with hosted ci run 42736964083
  and kind run 42736964167 on 33ee04bc (from the map; this package did not
  re-run hosted CI).
- completion-plan.md:138 F28 row: replace "RFC interoperability ... remain
  open" with this section's result. "Full enrolled HA orchestration" stays
  open (F28-OPEN6 is partial: transition SERVFAIL at authority changes).
- followup-audit.md:147 F28 row still says "Missing ... No hold-down/add/revoke
  state machine". Mark it implemented: durable transitions D297, operator
  recovery D320/D321, revoked/unsupported keys and real-authority
  interoperability D398.
- docs/architecture/upstreams-and-cache.md: the review found that the stale
  "no reset or recovery path is mounted yet" and "backend activation admission
  is still pending" sentences were still there (an earlier draft of this
  ledger wrongly said they were gone). They are now rewritten: recovery is the
  offline `kudurru anchors recover --offline` (D320/D321), and edge-backed
  activation goes through the backend's startup admission of every declared
  edge (D319), covered by the pinned dnsdist test. The same stale claim in
  docs/architecture/deployment.md ("edge-backed managed activation is still
  disabled", "activation coordinator ... is still unmounted") is corrected
  too. The page also links the operator page and states the D398
  qualification. The flows and diagrams did not change.
- docs/deploy/dnssec-validation.md now limits "no RFC 5011 worker" to
  forwarded validation.
- CHANGELOG.md: see `changelog.md` beside this file.

Finding, not fixed (recorded in D398 and the operator page): an observation
that only the revoked key itself authenticates commits the tombstone, but
`trustanchors.Advance` reschedules only authenticated observations. The
committed refresh therefore stays due and the updater polls once more at once.
When that leaves no trusted key (the quarantine case), the second poll is
unauthenticated and stops the updater for the term, which is harmless because
recovery is offline. With a remaining trusted key that does not sign the set,
the same second poll would also stop the updater for the term, while the node
keeps serving. That case is not seen at the real root and was not run. Changing
it touches the audited pure core (D297), so it is left for a decision.

Evidence under `.context/v020-final-20261110/evidence/F28-RFC/`:

- `interop04.log`: `go test -race -p 2 -count=1 -v -run '^Test(BIND|Knot)ManagedRoot' ./test/interop`
  with KUDURRU_E2E=1, KUDURRU_REQUIRE_BIND=1, KUDURRU_KNOT=1 and the private
  daemon: PASS, 251.5 s, no skips.
  - TestKnotManagedRootBootstrap: 16.5 s.
  - TestBINDManagedRootRevocation: OnlyTrustedKey 73.6 s (3 acquisitions,
    quarantined); SecondSeedContinues 60.7 s (2 acquisitions).
  - TestBINDManagedRootBootstrap: RSASHA256, ECDSAP256SHA256, ED25519 and
    MinimalResponsesNo; 32.9 s.
  - TestBINDManagedRootRefusals: WrongDS, RevokedSeed, ExpiredSignature,
    UnsupportedSeedAlgorithm and UnsupportedKeyIgnored; 37.7 s.
- `rollover01-generate.log`: BIND generated PASS (capture written). Knot
  generated FAIL: the test parsed keymgr's DS output with the wrong field
  count.
- `rollover02-generate-knot.log`: Knot generated FAIL, same cause.
- `rollover03-generate-knot.log` and `rollover04-generate-knot.log`: Knot
  generated PASS (6.8 s). rollover04 wrote the committed capture with the
  tidied provenance steps.
- `rollover05-default.log`: default run with no Docker, named present. BIND
  committed and generated PASS, Knot committed PASS, Knot generated SKIP (no
  KUDURRU_KNOT).
- `race01-trustanchors.log`: `go test -race -count=1 ./internal/trustanchors/...`.
  All five packages pass.
- `live01.log`: TestLiveRootManagedBootstrap with KUDURRU_LIVE_DNS=1: PASS,
  0.55 s, with 13 IPv4 replies accepted and 38696 authenticating 13 of 13.
- `lint03.log`: scoped golangci-lint v2.15.0 over `./internal/trustanchors/...`,
  `./test/interop/...` and `./cmd/kudurru/...`: 0 issues. gofmt is clean,
  go vet passes, and the bench guards pass (14 tests).

Review fixes (fix-*.log, after the accept-with-fixes review):

- `fix-rollover03-bind-before.log`: the generated BIND capture as first
  committed (readiness = the word "running" in named's log, then one dial),
  30 runs on the loaded machine: 22 FAIL, 8 PASS, every failure
  `dial tcp 127.0.0.1:<port>: connect: connection refused`. named prints
  "running on Linux ..." before it listens. Kept as the failing-before proof.
- `fix-rollover01-bind.log` and `fix-rollover02-bind.log` (the second at load
  average 16 to 22, with the interop run on the machine): after the fix
  (readiness is the first authoritative NOERROR reply to the capture request,
  named started up to three times), 30 of 30 and 30 of 30 generated BIND
  captures PASS; no restart was needed.
- `fix-rollover04-all.log`: `go test -race -count=1 -run '^TestRFC5011'
  ./internal/trustanchors` with KUDURRU_REQUIRE_BIND=1, KUDURRU_KNOT=1 and the
  private daemon: BIND committed and generated, Knot committed and generated
  (14.7 s) PASS, no skips; no `kudurru-f28-rfc-*` container left afterwards.
- `fix-interop01.log`: `go test -race -p 2 -count=1 -v -run
  '^Test(BIND|Knot)ManagedRoot' ./test/interop` with KUDURRU_E2E=1,
  KUDURRU_REQUIRE_BIND=1, KUDURRU_KNOT=1: PASS, 383.0 s, no skips.
  TestKnotManagedRootBootstrap 21.6 s, and its new cleanup check found the
  container removed; `docker ps -a` showed no f28 container after the run.
  TestBINDManagedRootRevocation with the 120 s refresh window:
  OnlyTrustedKey 154.1 s (3 acquisitions, quarantined), SecondSeedContinues
  131.1 s (2 acquisitions). TestBINDManagedRootBootstrap 25.0 s,
  TestBINDManagedRootRefusals 32.8 s.
- `fix-containers01.log`: the two knotd containers the earlier interop03 and
  interop04 runs leaked (`kudurru-f28-rfc-knot-ij55olyb7v`,
  `kudurru-f28-rfc-knot-jrvhqqbo5c`) were removed; the reviewer removed the
  two from the review runs.
- `fix-gate01-knot-skip.log`: without KUDURRU_E2E and KUDURRU_KNOT,
  TestKnotManagedRootBootstrap skips before managedRootIP.
- `fix-race01-trustanchors.log`: `go test -race ./internal/trustanchors/...`,
  all five packages PASS.
- `fix-lint01.log`: one `unused` finding (the single-dial `captureTCP`
  helper the fix made obsolete), removed. `fix-lint02.log`: scoped
  golangci-lint v2.15.0 over `./internal/trustanchors/...` and
  `./test/interop/...`, 0 issues.
- `fix-checks01.log`: bench guards (14 tests), guide-lint (67 files, 0
  errors) and gofmt clean.

Retained failed runs and what they showed:

- `interop01.log`: dnssec-signzone needs a ZSK or `-z`. The first probe count
  raced named's query log.
- `interop02.log`: named 9.18.50 sometimes aborts at startup in
  dns_cache_create (cache.c:263 REQUIRE). The fixture now waits for an answer
  and restarts named up to three times. dnssec-signzone's own verification
  refuses expired signatures (`-P` added). A quarantined node never reports
  ready, so the fixture uses Start instead of StartReady.
- `interop03.log`: keymgr cannot open the KASP database while knotd runs
  ("temporarily out of resources"), so the root DS is now read before knotd
  starts. It also revealed the third acquisition after a quarantine
  (09:02:50.945 refresh, 09:02:52.069 unauthenticated re-poll), the finding
  above.
- Review runs `review-rollover01.log` and `review-rollover02.log` (1 of 8 and
  10 of 25 generated BIND captures failed with `connection refused`): the
  readiness defect fixed above. The review interop runs also showed each run
  leaking one knotd container (canceled `t.Context()` in the cleanup), fixed
  above.
- `lint01.log`: six gosec findings in the new tests, fixed in place.
  `lint02.log` did not run: another lane's lint was running in parallel.

Still open:

- No running node takes a key from AddPend to Valid in real time. That step is
  proven only in the pure package and through the structural owner and
  projection chain, by decision.
- The optional real-time soak (two scheduled refreshes, about 2 h 10 min) was
  not run.
- IPv6 root letters were not reachable from this sandbox, so the live check
  covered IPv4 only. Run it once from a CI runner with IPv6 egress.
- Knot qualification runs only with KUDURRU_KNOT=1 on a private daemon. Hosted
  CI does not run it.
- The Knot capture has four distinct stages (its s3 repeats s2), so the
  single-signer stages "K2 published, signed by K1 only" and "K1 still
  published, signed by K2 only" are proven on BIND bytes only.
- The revoke-only rescheduling finding above needs a decision.
- The tracker edits listed above are for the integrator.
