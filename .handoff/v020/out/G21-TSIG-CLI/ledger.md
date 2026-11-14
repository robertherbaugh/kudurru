### G21-07 CLI for stored TSIG keys — November 10, 2026 (D394)

`kudurru tsigkeys` now drives the five `/tsigkeys` routes on both drivers
(`cmd/kudurru/tsigkeys.go`, mounted in the root's DNS group). `list` prints
name, algorithm, source, UID, resourceVersion and creation time, or the full
document with `-o json|yaml`. `create NAME --algorithm --secret-file` posts to
the collection. `replace`, `reseal` and `revoke` take the exact `--uid` and
`--resource-version`; `replace` also takes `--secret-file` and an optional
`--algorithm`. The secret is read only from a file or stdin (`-`). It is never
a flag value and never appears in any output. Whitespace in the file is
ignored, so wrapped `openssl rand -base64` output works. The CLI checks
locally that the secret is canonical standard base64 of 1 to 512 bytes and
refuses a bad one without echoing it. Every subcommand checks `-o` before it
reads a secret or sends anything, so a format typo never follows a committed
mutation. NAME must be one canonical key name (`tsigkinds.CanonicalName`, so
`.` and `..` are refused) and `--algorithm` is bounded to the server's 64
characters. These refusals say that nothing was sent. The API body types redact themselves
when marshaled, so the CLI builds the body directly and clears it after the
call. `apiclient.TSIGKeyCommand` sends the command once through generated
dispatch. Redirects, replayable bodies and idempotency headers are disabled,
and the body is bounded at 4 KiB. A refusal before sending (invalid
operation or name, empty or oversized body) wraps `ErrTSIGKeyNotSent` and
reads as "not sent; nothing changed"; an error from the round trip itself
stays an unknown outcome. It accepts only 201 (create) or 200 (other
operations) for an acknowledged publication, or 202 for a commit without that
acknowledgment. The CLI then checks that the receipt matches the operation,
the requested canonical key name, the reviewed UID (and, for revoke, the
reviewed resourceVersion), the removed flag, the store source and the status;
a mismatch is an unknown outcome, never a success. A 202 exits 0 and prints a note on standard
error that publication was not confirmed and that this is not a rollback. A
server refusal keeps its HTTP status and error key. An enrolled follower's
`503 primary_required` reads as "did not run the command; nothing changed"
and gets the existing primary hint. Other 4xx answers read as "refused, not
retried". 5xx answers say the outcome may be uncertain. A missing or unusable
response reports an unknown outcome. Nothing is ever retried. The operator
guide (`docs/deploy/tsig-keys.md`), the dynamic-update guide and
`docs/architecture/zones.md` describe the commands. The CLI reference is
regenerated.

Evidence under `.context/v020-final-20261110/evidence/G21-TSIG-CLI/`:
`race02.log`: `go test -race -count=1 -v -run 'TestCLIRealRouterTSIGKeys|TestCLITSIGKeyUnknownOutcome|TestRunServeSQLiteTSIGKeysCLI' ./cmd/kudurru`
ran 4 tests, 4 passed, 0 skipped. `TestCLIRealRouterTSIGKeys` (0.89 s) covers
the production router with a stub command: the wire body and decoded secret
for a wrapped secret file and for stdin; receipts for 201, 200 and 202; local
refusals that send nothing; conflict and unavailable refusals sent once; a
server-refused unconfirmed receipt; and a viewer forbidden before the
command runs. `TestCLIRealRouterTSIGKeysFollowerRefusal` (0.49 s) covers the
router with `ClusterRole` returning a follower: list still works, and reseal
gets `503 primary_required`, reads as "nothing changed" with the primary hint,
and is sent once. `TestCLITSIGKeyUnknownOutcome` (0.01 s) covers a wrong
success status and a dropped connection: each reports an unknown outcome, and
2 commands produce 2 requests. `TestRunServeSQLiteTSIGKeysCLI` (10.25 s) runs
the mounted SQLite `runServe` with a CLI password sign-in. It drives create
from a file, replace from stdin, reseal, a stale revoke refused with 4xx
(stored resourceVersion unchanged), and revoke. After each receipt, signed
DNS succeeds with the current secret, and after revoke it gets BADKEY. Neither
secret appears in any output. `race01.log` is the same run without the
follower test (3/3 passed). `unit01.log` and `unit02.log` are the first
non-race runs (2/2 passed). `apiclient01.log` and `apiclient02.log`:
`TestTSIGKeyCommandShapeAndSingleAttempt` and the full race run of
`./internal/apiclient/...` passed. In that test, invalid operations, paths and
bodies send nothing, and a 307 is not followed. `lint01.log`: scoped
golangci-lint on `./cmd/kudurru/... ./internal/apiclient/...` reported 0
issues. `docsgen01.log`: `make docs-gen` changed only
`docs-site/content/reference/cli.md`. `drift01.log` and `drift02.log`:
`TestReferenceDrift` and `TestCLIReferenceDrift` passed after the generated
commit, with the four TSIG tests passing again (non-race). `bench01.log` and
`bench02.log` are retained failures: `test_bench_socket_calibration` fails
because `internal/dnsserver/quic_socket_test.go` no longer matches its pinned
SHA-256. That file was changed by the earlier lane commit c510158d, not by this
package. The PR branch already restores it in 2133f47e.

Review fixes (commit `fix(cli): check tsigkeys input before sending and bind
the receipt`). `fix-02-apiclient-prefix.log` and `fix-02-cli-prefix.log` /
`fix-03-cli-prefix.log` are retained failures that prove the new cases fail
on the previous source: the apiclient test does not build without
`ErrTSIGKeyNotSent`; `reseal ..` reported an unknown outcome; `create ... -o
bogus` reached the command; and 3 of 10 `TestCLITSIGKeyReceiptBinding`
subtests (create for another key, revoke for another key, revoke for another
resourceVersion) were rendered as success. `fix-01-*-withfix.log` are an
early mislabeled run of the same tests with the fix in place (passed).
`fix-04-cli-race.log`: `go test -race -count=2 -v -run
'TestCLIRealRouterTSIGKeys|TestCLITSIGKeyUnknownOutcome|TestCLITSIGKeyReceiptBinding|TestRunServeSQLiteTSIGKeysCLI|TestCLIReferenceDrift' ./cmd/kudurru`
ran 6 tests per iteration, 12/12 passed, 0 skipped (33.5 s total;
`TestRunServeSQLiteTSIGKeysCLI` 14.50 s and 12.71 s,
`TestCLITSIGKeyReceiptBinding` 0.10 s and 0.06 s with 10 subtests each).
`TestCLIRealRouterTSIGKeys` now also refuses `-o bogus` for all four
mutating subcommands and list, `.`, `..`, `bad..name` and a 5,000-character
algorithm, asserting after each that nothing reached the command and that the
error claims no uncertain outcome. `TestCLITSIGKeyReceiptBinding` sends ten
mismatched receipts from a stub server (201 unpublished, another key, a file
source, another UID, 202 with `published: true`, another operation, a
non-object body, revoke for another key or resourceVersion, revoke without
`removed`); each reports an unknown outcome after exactly one request.
`fix-04-apiclient-race.log`: `go test -race -count=2 ./internal/apiclient/...`
passed. `fix-04-vet.log`: `go vet ./cmd/kudurru` clean. `fix-06-lint.log`:
scoped golangci-lint 0 issues. `fix-05-docsdrift.log`: `TestReferenceDrift`
passed; help text did not change, so no new generated commit was needed.
`fix-05-bench-guards.log` is the same pre-existing `quic_socket_test.go`
SHA-256 failure as `bench01.log`, unchanged by this package.

Still open: the CLI never generates a secret, because the API takes a secret
that the operator shares with peers. There is no dashboard surface for stored
keys; that belongs to the dashboard lane. The Kubernetes envtest mount
(`serve_tsig_kube_management_test.go`) was not extended with a CLI pass. The
CLI calls the same routes through the same client on Kubernetes, but no test
runs the CLI against a Kubernetes node. Errors from the generated dispatch
step itself (for example its method-or-path guard) are still reported as an
unknown outcome, because the client cannot tell them apart from a transport
error after the request left; with dot-segment names refused locally, no
known input reaches that path.
