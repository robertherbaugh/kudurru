# IAM acceptance evidence, November 15, 2026

D414 adds executable acceptance evidence for D396 on top of D413's revocation
checkpoint redesign. The owner explicitly accepts service-account token issuance
in v0.2 with both `serviceaccounts.write` and `tokens.write` required. The old
permanent-history exhaustion behavior remains superseded by D413, not accepted
as a limitation.

## Exercised boundaries

| Requirement | Evidence | Scope |
| --- | --- | --- |
| Mounted HTTPS writers | `TestRunServeSQLiteIAMWritersRevokeAcrossRestart` | Real `runServe`, trusted certificate, all nine D396 mutations, local login, disabled/deleted user cookie refusal, service-account token refusal, retained unrelated token, database restart |
| Local-user and ServiceAccount revocation on a serving follower | `TestServingHTTPIAMAccountTLSRevocationAndServingFollower` | Original cookie/token authenticates on primary and follower; PATCH/PUT disable or DELETE refuses it on primary, after validated replica install, and after follower database reopen; unrelated administrator token survives |
| API-created provider browser credentials | `TestServingHTTPIAMAPIProviderTLSRevocationAndServingFollower` | Trusted TLS on router and discovery/JWKS/code endpoint; actual browser-code exchange through an API-created provider; original cookie and PAT work on primary/follower, then fail after provider replace/delete and replica install/restart |
| Real external issuer | `TestServingHTTPIAMLiveAuthentikRevocation` | Fresh RS256 ID token from `auth.herbaughmanor.net` over normal public TLS; API-created provider; accepted on primary/follower, refused after provider DELETE and replica install/restart; unrelated local session survives |
| Own cookie clearing | `TestServingHTTPIAMOwnCookieClearedAfterUpdateAndDelete` | Ordinary API-managed administrator completes password change, then self-disables or self-deletes through TLS; response clears cookie, replay fails, other administrator session works |
| Separate token permission | `TestServingHTTPIAMServiceAccountWriterWithoutTokensWrite` | Narrow test policy permits account creation but refuses token issuance with 403; entire image unchanged by refused issuance |
| Concurrent writers | `TestServingHTTPIAMWritersConcurrentCreateAndReplace`, `TestServingHTTPIAMConcurrentTokenIssuance` | Local PATCH/DELETE, ServiceAccount create/replace/delete, provider create/replace/delete each commit one reviewed effect; independent successful token issues produce distinct credentials and exactly the committed token count |
| Per-writer final time/cancellation | `TestServingHTTPIAMWritersTLSFinalClockAndCancellation` | All nine actual writer routes over trusted TLS, each with caller expiry, rollback and cancellation injected after successful staging; exact refusal and entire image unchanged |
| New-token and shared time/cancellation checks | `TestServingHTTPIAMWritersOriginalFinalClockAndCancellation`, `TestServingHTTPIAMTokenFinalClockAndCancellation` | Shared IAM transaction checks expiry/rollback/cancellation and released original proof; real token staging separately checks the new token's own deadline after the caller's final check; refused transaction leaves image unchanged |
| History exhaustion redesign | D413 evidence | 4,113 real account create/delete cycles, restart/sign-in and unaffected-session continuity, full-row/full-byte migrations, checkpoint security and negative controls; see the D413 ledger entry |

The mounted, account-follower, permission and lifecycle regression selection
passes under the race detector (70.389 s). The earlier expanded selection
passes its provider TLS (33.49 s), live Authentik (17.52 s), token final checks
(5.88 s), concurrency and self-cookie tests under race. That process exits
nonzero because its permission test incorrectly expected account creation to
require `tokens.write`; the corrected permission test passes in the regression
selection. Both logs remain retained. The hosted UniFi status-revision test's
whole-second timestamp prerequisite passes three repetitions under race with
required real envtest (87.776 s).

The final per-writer TLS fault selection and strengthened new-token proof pass
under race (77.884 s): all 27 route/fault combinations reach staging before the
fault, then return 401 for expiry/rollback or 503 for cancellation, with complete
object/security/audit/generation rollback. The existing command observer gains
an `afterStage` callback; it is unset in production. Token staging is extracted
without a policy change so the real issued-token checks can be exercised.
Negative overlays confirm the assertions fail when the caller's final check is
removed (local DELETE, ServiceAccount create, provider replace) or both redundant
issued-token time checks are removed (new-token expiry and rollback). Removing
only the explicit token check still passes because stored-token authentication
independently registers its final time check; that diagnostic is retained too.
Whole-repository vet, command lint (0 issues), workflow actionlint, 21 benchmark
control tests and the 168-document site link check pass.

These are isolated SQLite runtimes and actual HTTP/TLS servers in test
processes. Follower changes use the production captured receiver,
`ReadGeneration`, `ValidateImage` and `InstallReplica` boundaries directly;
this is not a new network replication or independently deployed cluster proof.
Clock faults now cover both individual TLS writer requests and the real token
staging boundary. Existing review, permission, concurrency and D413 owner tests
remain complementary; this entry does not claim an independent release review.

A cold follower deliberately refuses its first historical provider entitlement.
The provider tests retain that rule: they run a new real writer episode,
install the newly admitted provider entitlement, wait past the original
whole-second floors, and only then issue the credentials. Authentication
succeeds on the live follower before revocation. Restart refusal also observes
the cold-start rule and is not by itself proof of retained-floor causality.

The live Authentik test uses a disposable machine principal and
`client_credentials` ID token. A human interactive login against live Authentik
was not completed; browser-code/cookie/PAT coverage uses the TLS provider double.
No TLS verification or JWT verification was disabled. No production Kudurru
configuration or deployment was changed.

## Reproduction and cleanup

Run the committed non-external tests normally with `go test -race ./cmd/kudurru`
and a focused `-run` selection of the names above. The live test skips unless
`KUDURRU_ACCEPTANCE_OIDC_FILE` names a private JSON file containing `issuer`,
`clientID` and `clientSecret`. Provision an isolated confidential Authentik
application supporting `client_credentials`, `openid profile email`, and signed
RS256 ID tokens; its discovery and token endpoints must have normally trusted
HTTPS certificates. The test reads those credentials privately and requests a
new ID token; it does not accept a pre-minted token or a copied signing key.
The token endpoint follows Authentik's `/application/o/token/` convention.

The November 15 application/provider name was
`kudurru-acceptance-20261115-cd41`. After the proof, cleanup deleted exactly that
application, provider, disposable user, automatically created
`ak-kudurru-acceptance-20261115-cd41-client_credentials` user and associated
session. Before counts were 1 application, 1 provider, 2 users and 1 session;
after counts were all zero, including authenticated sessions. Shared signing
certificates, flows and scope mappings were retained. The private credentials
and creation log were removed after the cleanup transaction succeeded.

Local evidence, commands, source overlays and diagnostic failures are retained
under `.context/iam-live-acceptance/`; prior D413 evidence remains under
`.context/iam-checkpoint/`. These ignored evidence directories are workspace
artifacts, not files available from a fresh Git checkout.

## Infrastructure and performance limits

KIND v0.33.0 could not boot in this cloud VM: node systemd reports
`Failed to create /init.scope control group: Structure needs cleaning.` Both
cluster attempts and the diagnostic bridge were removed. Docker Desktop on the
owner's Mac also failed to start within its bounded attempt. No KIND cluster
was deployed. The Helm deployment's Kubernetes store deliberately refuses the
D396 IAM commands, so complete SQLite runtimes are the applicable IAM target.
The homelab was reached through its existing Kubernetes API; Talos SSH was not
needed.

Performance remains release-blocking under D383/D412. The retained formal run
43690014307 completed all 60 commands on EPYC 9454P and failed seven upstream
absolute metrics in each source. Its other eight absolute gates passed; the
reference failure makes relative comparisons ineligible. That result is not
replaced by the live IAM checks.

One predeclared local diagnostic ran four unprofiled fast/strict processes
(reference/candidate with Go 1.27.3/1.27.4) and two instrumented scheduler traces,
all once with six processors. On this Intel Xeon sandbox the reference measured
97,659/91,444 ns/op and candidate 86,592/85,634 ns/op respectively. It did not
reproduce the hosted failure or establish its cause. These are single local
samples, with no qualification gates applied. Original sources, workloads,
calibration, thresholds, limits and failed recordings remain unchanged.

The separate `diagnostic` workflow mode (`release-benchmark-diagnostic` PR
label) now declares six native commands on the selected hosted runner, pinned
to Go 1.27.4 and six processors: reference/candidate, then candidate/reference,
then one trace per source, covering fast/slow-first with strict/RTT-weighted.
It retains two unprofiled processes and one instrumented trace per source,
source manifests, raw receipts and before/after CPU pressure/throttling counters.
The plan is written before timing. It reuses bounded process controls, holds
on source change/resource stop, never retries a native failure, and keeps
`release_qualified: false` with no gates or baseline read. This is diagnostic
collection, not a superseding formal measurement or changed acceptance rule. A
measured causal implementation fix or a justified, reviewed measurement-method
change must precede another formal recording; rerunning for a favorable VM,
selecting samples or raising limits does not close this gate. Final-source CI,
release artifacts and independent release review remain separate. AD/Windows
and independent two-VM VIP acceptance retain their owner-deferred status. The external Cloudflare docs build is owner-excluded.
