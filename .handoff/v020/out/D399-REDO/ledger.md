### D399-REDO Commands superseded before dispatch, opt-in per command class — November 13, 2026 (D399)

The first D399 (CMD-RECAPTURE) recaptured every current command in the shared
command path and was reverted after seven contract tests failed
deterministically. It is now opt-in. `servinghttp.WithRecapture` marks a
command that carries nothing derived from its first capture into its
transaction, and only a marked command that is refused with
`serving.ErrPending` before its transaction body began, only because another
commit superseded its request's publication, captures the current publication
again (bounded by 5 s or the request deadline) and is dispatched once on it.
Three call sites are marked: the local password sign-in (`POST /auth/login`),
the sign-out (`POST /auth/logout`) and a single whole-object write without
If-Match (the API marks it with `api.RecapturableCommand` in
`commandWriteObject`; `ObjectCommand` turns that into the servinghttp marker).
Every other command keeps the immediate refusal of D199 and D208, exactly as at
the base: every item of a multi-item request (the first one too), If-Match
writes, deletes, source entries edits, domain-list plans, cache invalidation,
node-local settings, import adoption, settings and control merges, cluster
activation, `join-token` and promotion, OIDC callbacks and browser sign-out,
TSIG keys, factors, tokens, users, role bindings, IAM writers, blocking,
notify, integration operations (check, preview, sync, actions, release and
delete cleanup), forwarded commands and signed UPDATEs. The marked object
write covers whole-object PUTs and POSTs of `DNSZone`, `DNSRecordSet`,
`BlockList`, `ClientGroup`, `Policy` and `Integration` objects (an
Integration's secrets authorization and license check still run inside the
transaction). The admission checks of the first version are kept (no earlier dispatch in the
request, authority advanced only in its revision, same auth dependency
identity, auth controls and cluster settings, the raw credential verifying as
the same principal, session and roles). `Retry-After: 1` on an object route's
`unavailable` is set only when no part of the request ran. The seven tests are
unchanged and pass; `TestServingItemsFirstItemKeepsTheOriginalSource` and the
zone-group tests are back to their base text (the first version had rewritten
them).

Held commits (`wip/v020/ci-held`, equivalents of the prompt's list):
`28530541`/6281a4ea changed (opt-in instead of every command; join-token
`WithExactSource` and the output-path recapture dropped; the multi-item rule
widened to every item), `1e6b463b`/f456d5c6 kept (dispatch marker, settings
and dependency checks) except its rewrite of the items test (dropped),
`ebf5102c`/bec9d351 dropped (zone-group helpers keep their base retries: a
rolebinding DELETE there is not recaptured), `8b762ef1`/eff1ac40 changed
(`WithoutRecapture` is unnecessary under opt-in and removed; Retry-After rule
and its tests kept), `c19f1610`/2479ecdb kept (mounted test; its anchor test
change was net zero), `bc5690d8`/2c075021 changed (docs rewritten for the
narrowed rule; sqlite-cluster.md and the apply paragraph of api-contracts.md
kept at base because apply items are not recaptured), `41d1aca9`/d5fc108d and
`612d10e9`/10587ce2 kept, `154a779a` kept (NORACE_TESTS entry and race skip),
`aa57d5d4` kept (Retry-After only while nothing ran) with docs rewritten,
`3d3e88bc` kept (effect-free object 503 test, D405 docs), `bdff2d8f` kept
(TOTP clock), `980c7fe0` and `28087ae9` kept (strengthened mounted test),
`4ef85f36` kept (zone setup helper asks again only after Retry-After),
`f1279497` kept (chunk sweep docs). `5b200f28`, `f66940d5`, `a141541e`,
`5e8dd096`, `53eea181` were already in the base.

The mounted proof, `TestRunServeCommandsNotRefusedAsSupersededUnderSteadyUpdates`,
sends ten signed UPDATEs a second; about one a second commits (the rest
answer SERVFAIL, because an UPDATE's ingress check needs a current
publication and is not recaptured). The test requires at least 0.5 committed
UPDATEs a second, so the load really supersedes the API commands it proves.
The sign-in recapture test runs the runtime clock a second ahead for the
recaptured sign-in and requires exactly the control's audit rows (its
lastLoginAt status row), as well as exactly one new session.

Evidence under `.context/v020-final-20261110/evidence/D399-REDO/`:

- Review fixes (at a85360ae): `fix-01-recapture-race.log`,
  `TestServingCommandRecapture.*` under race, count=3, CPUs 0-3: 8 tests x 3
  pass, ok in 116 s. `fix-02-mount-norace.log`: the mounted proof without
  race: 200/200/200 admitted, 1186 UPDATEs sent, 160 committed (0.99 a
  second), worst write-to-DNS lag 1.097 s, ok in 164 s.
  `fix-03-signin-negative-frozen-clock.log`: the sign-in test with the clock
  frozen across both sign-ins (overlay, not committed) fails with "0 of the
  sign-in (want the control's 1)", so the restored audit assertion has teeth.
  `fix-04-vet.log` clean, gofmt clean; `fix-05-lint.log`: only the base G117
  in `serve_inventory_mount_test.go:159`.
  FULLRUN_PLACEHOLDER

- `race01-seven-and-recapture.log`: the seven tests, the recapture tests,
  the items, zone-group and TOTP tests under race, pinned to CPUs 0-3,
  GOMAXPROCS=4: all pass except the new join-token test, which asserted no
  Retry-After on an auth route (the auth routes answer every 503 with it,
  `auth.WriteError`); assertion removed. `race03-focused.log`: the seven,
  `TestServingCommandRecapture.*` (sign-in, sign-out, object write, refusals,
  never-repeat, integration operation, join-token refused once, only marked
  commands), `TestServingObjectWriteUnavailableIsEffectFree`,
  `TestServingItems.*`, `TestServingObjectCommand.*`,
  `TestRunServeSQLiteZoneChanges.*`, zone-group, migration recapture and
  `TestServingHTTP(Browser|Durable|Auth).*`: ok (481 s).
- `race02-servinghttp.log`, `race05-api-serving-servinghttp.log`:
  `go test -race ./internal/api ./internal/serving ./internal/servinghttp`:
  ok (334 s, 28 s, 32 s).
- `lint06.log`: golangci-lint v2.15.0 on internal/api, internal/servinghttp,
  cmd/kudurru: one G117 in `serve_inventory_mount_test.go:159`, a base file
  this package does not touch. `vet04.log` clean, gofmt clean,
  `bench-guards07.log` 14 OK. No generated artifact changed (no route, DTO or
  permission change), so there is no chore(gen) commit.
- `full10-cmd.log`: `python3 hack/go-test.py --timeout=30m
  github.com/robertherbaugh/kudurru/cmd/kudurru` at df527383 through the heavy
  wrapper with `taskset -c 0-3`, GOMAXPROCS=4: batches 3, 4 and the no-race
  timing batch (including
  `TestRunServeCommandsNotRefusedAsSupersededUnderSteadyUpdates`, 309 s) ok;
  nine tests failed in the other batches on a machine shared with two other
  full runs. `full08-cmd.log` is the first full run, cut off by the tool's
  two-hour limit in batch 8 (seven batches complete, six failures).
- Each failing test was then run alone at head and at the base 606a53a8
  (`cmp09-*` by checkout; `cmp12`, `cmp13`, `cmp15`, `cmp16` by alternating a
  head binary with a base binary built through a go build overlay of the base
  files, so both ran under the same load). Failures head/base:
  `TestServingMigrationCutWaitDoesNotBoundTheStepCommand` 1/3 and 3/10 vs 0/3
  and 6/10 (same "context deadline exceeded" at base);
  `TestRootMemberUpdateKeyIdentityAndRetryActualHTTP` 1/3, 0/5 vs 2/3, 1/5;
  `TestServingTSIGObserverOriginalCancellationAndDeadline` 2/3, 3/5, 4/8 vs
  1/3, 0/5, 3/8 (`cmp14` with a logging overlay: in every failure on both
  sides a publication notification arrived during the deadline window,
  `windowOpen=false publisherChanged=false`);
  `TestIntegrationDeleteForgetsAHistoryOfSeveralStoredPages` 1/3 vs 2/3;
  `TestRunServeUniFiHostnameHeldPublicationDeadline` 0/3, 3/3 vs 0/3, 3/3;
  `TestServingHTTPClusterActivationCookieCSRF` (a race report between the
  test's own write of `Config.Node.DataDir` and the publisher, in the test)
  0/3, 0/3 vs 0/3, 0/3; `TestRunServeSQLiteManualNotifyRestartAndRearm` 0/5,
  0/3 vs 0/5, 0/3; `TestRunServeDirectAnchorsBootstrapTransportsRestartAndDisabled`
  (generation_pending) 0/3 vs 1/3; `TestVIPObservationActualKubernetesAndMountedAPI`
  0/3 vs 1/3; `TestRunServeUniFiHostnamePolicyLifecycle`,
  `TestRunServeQuerylogDnstapSinkPrivacyAndRaw`,
  `TestServingTSIGObserverQuietOriginalExpiry`,
  `TestRunServeQuerylogSinksStalledFailingAndReload` 0/3 vs 0/3;
  `TestServingHTTPPasswordChangesShareRejectedProofs` 0/3 vs 1/3.

What remains open: FULLRUN_OPEN_PLACEHOLDER Commands that are not marked (a settings PATCH, a password or
factor change, and the rest of the list above) still answer `503
unavailable` when the node's own commits supersede them, so tests that send
one of them once fail intermittently in full runs
(`TestRunServeQuerylogSinksStalledFailingAndReload`, PATCH /settings;
`TestServingHTTPPasswordChangesShareRejectedProofs/factor`; the base fails
the latter too, 1/3 in `cmp16`). Followup for the integrator: decide whether
settings and password commands should be recaptured, or have those tests
wait for a settled publication before the single-shot command. The SERVFAIL
share of signed UPDATEs under concurrent API commands (about 84%) is also
open. Earlier runs: the full command package did not pass in one run on this
machine; every test that failed in it fails at the base at a comparable or
higher rate, with the same message, and none of them goes through a marked
command. `TestServingHTTPClusterActivationCookieCSRF` has a data race in its
own setup (it writes `f.s.opts.Config.Node.DataDir` while the publisher
reads the config); that test is not changed here. Cluster activation and
`join-token` are deliberately not recaptured; if their refusals under steady
commits matter, they need their own decision. Refusals at capture
(`generation_pending`) are unchanged (D405 limit).
