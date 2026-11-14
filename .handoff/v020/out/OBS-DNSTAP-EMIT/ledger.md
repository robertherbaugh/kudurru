### OBS-2 dnstap emit with the audited raw override — November 10, 2026 (D372)

A `querylog.sinks` entry of `type: dnstap` now sends every recorded query as
`dnstap.Dnstap` messages in Frame Streams to a collector on a Unix socket
(`unix:/path`), TCP (`tcp:host:port`) or TLS (`tls:host:port`, optional `caRef`,
`certRef`+`keyRef`, `serverName`). It runs on the D371 sink host like the HTTP
sinks: its own bounded queue, batches, timeout and attempts, so a collector that is
down or stops reading loses only its own messages, which are counted, and the sink
reconnects with the next batch. The sink writes the bidirectional handshake (READY,
ACCEPT, START, data frames, STOP, FINISH). Local zone and local record answers are
`AUTH_QUERY`/`AUTH_RESPONSE` (with `query_zone`), all others
`CLIENT_QUERY`/`CLIENT_RESPONSE`; `types` selects them, and the opt-in
`FORWARDER_QUERY`/`FORWARDER_RESPONSE` pair summarizes an upstream exchange from
what the event knows (question, rcode, the client's times, the upstream in `extra`).
Identity defaults to the node name, version is `kudurru/<version>`, `extra` carries
status, list, upstream, client name and trace ID. The protobuf encoding and the
framing are hand-written in `internal/querylog/sinks/dnstap`; no module was added to
the binary (`google.golang.org/protobuf`, already in the build, became a direct
requirement only for the schema test). The test collector (`dnstaptest`) reads
the framing with its own strict Frame Streams reader, written from the fstrm
description and importing nothing from the sink, and a golden test pins
hand-written handshake bytes to both that reader and the sink's encoder.

Default output follows `querylog.privacy`: each message is rebuilt from the
transformed event (question, header flags, rcode, never answer records, the logged
client address, no port), `hidden.` names under the hiding modes, and nothing under
`nothing`. `raw: true` sends the query as received and the answer as the pipeline
produced it, with the real client address and port, under every privacy mode.
Exclusions (`querylog.exclude.*`, the health name, the built-in dig) run first and
bind raw too. Raw is admin-only: a settings change of `querylog.sinks`, or of any
`querylog.exclude.*` key, that leaves a raw sink in place needs the new permission `querylog.raw` (only `admin`, through
`*.*`) on top of `settings.write`; the API audits a refusal as denied and the
accepted change as `querylog.raw`, and the complete SQLite runtime re-authorizes
`querylog.raw` against the original credential inside the commit transaction. Every
node records `querylog.raw.export` before a raw sink opens, so raw from the settings
file or a ConfigMap is audited too; a raw sink whose audit event is not recorded
does not connect, and the host retries the open. DNS wire bytes exist only while the
route holds a raw sink: the log observer asks the route (`WireRetainer`) and packs
only then, the collector copies the event only for such a route and clears
`Event.Wire` before the primary sink, the tail and every other sink.

Evidence under `.context/v020-final-20261110/evidence/OBS-DNSTAP-EMIT/`:

- `race08.log`: `go test -race -run 'TestQuerylog|TestRunServeQuerylog|TestOpenAPIDrift|TestReferenceDrift|TestCLIReferenceDrift|TestSettings' ./cmd/kudurru`, 22 tests pass in 109.6 s, among them `TestRunServeQuerylogDnstapSinkPrivacyAndRaw` (14.6 s: mounted serve on SQLite under `hide_domains_clients`, sink added through PATCH /settings, real UDP queries read from a Unix socket Frame Streams reader; hidden name, `0.0.0.0`, no port, no answer without raw; the exact exchange with ID, answer, `127.0.0.1` and a port with raw; nothing for `*.skip.example` either way; STOP/FINISH on every replaced stream), `TestQuerylogRuntimeRawDnstapWaitsForItsAuditEvent`, `TestQuerylogSinkTypesHaveFactories`, the existing sink mount tests and `TestCLIReferenceDrift`.
- `race06.log`, `race07.log`, `race09.log`: `-race` over `internal/querylog`, `internal/querylog/sinks/...` (including `dnstap`: `TestSinkFrameStreamsOverUnixTCPAndTLS`, `TestSinkRawSendsTheWireMessages`, `TestSinkRawRequiresItsAuditEvent`, `TestSinkTypesAndPrivacy`, `TestSinkReconnectsAndDropsBoundedOnTheHost`, `TestFrameMatchesTheDnstapSchema` with `dynamicpb` against a descriptor of `dnstap.proto`), `internal/stages/log` (`TestObserverRetainsWireOnlyForARawRoute`), `internal/config`, `internal/rbac`: all pass.
- `unit02-api.log`, `unit03-api.log`, `race04-api.log`: `TestSettingsRawDnstapSinkNeedsQuerylogRaw` (a `settings.write`-only principal is refused with 403 and audited denied twice, the administrator's change is audited as `querylog.raw`, turning raw off needs only `settings.write`); the full `internal/api` race run passed in 256 s before the lint-only edits.
- `fuzz01.log`, `fuzz02.log`: `FuzzDnstapEncode` and `FuzzFrameStreamRead` (test/fuzz, listed in `hack/fuzz.sh`) ran 60 s and 30 s each without a finding.
- `lint04.log`: scoped golangci-lint over `internal/querylog/...`, `internal/stages/log/...`, `internal/config/...`, `internal/api/...`, `test/fuzz/...`, `cmd/kudurru/...`: 0 issues. `race10.log`: dnstap and test/fuzz raced again after the lint edits, pass.
- `gen02.log` then `drift02.log`: after the generated commit, `TestCLIReferenceDrift`, `TestAggregatedRolesDrift`, `TestReferenceDrift` and `TestOpenAPIDrift` pass. `drift01.log` is a retained setup failure (package paths given without `./`), not a drift.
- Review fixes (`fix-*.log`):
  - `fix-unit01-api.log`, `fix-race04-api.log` (`-race -count=2 -run TestSettings ./internal/api`, pass in 9.3 s): `TestSettingsRawDnstapSinkNeedsQuerylogRaw` now also refuses a `settings.write`-only principal that changes `querylog.exclude.clients`, `.domains` or `.sudn` while a raw sink is in place (403, audited denied, nothing written; 5 denied and 2 granted `querylog.raw` events in all). `fix-unit02-api-without-fix.log` is the retained run without the gate change: the exclusion PATCH returns 200 with `changed: [querylog.exclude.clients]`. `fix-unit03-config.log`: `TestRawQuerylogChange` pins the shared rule.
  - `fix-cmd01.log`, `fix-race01.log` (`-race -count=2`, 16 tests pass in 52.1 s, with the neighboring `TestServingCurrentCommand*`, `TestServingCanonicalSettings*` and `TestServingBrandingOriginalCredentialRevocationIncludesNoop`): `TestServingSettingsRawDnstapReauthorizesQuerylogRaw` drives the complete SQLite runtime's `WriteSettings`, bypassing the API gate, with a token restricted to a custom role holding `settings.write` but not `querylog.raw`; a raw sink and an exclusion change under one are refused at commit with the generation unchanged, a plain sink by the same credential and both changes by the administrator persist. `fix-cmd02-without-commit-check.log` is the retained run with the loop over the extra permissions disabled: the first step persists and the test fails (its message prints the two values in swapped order; fixed afterward).
  - `fix-race02.log` (`-race -count=2` over the dnstap sink, `dnstaptest`, `internal/querylog`, `internal/stages/log`, `internal/config`, `test/fuzz`): pass, including `TestFrameReaderGolden` and `TestFrameReaderRefuses` and every sink test now read by the independent framing reader. `fix-fuzz01.log`: `FuzzFrameStreamRead` (now differential: the sink's reader must return every frame the strict reader accepts identically) and `FuzzDnstapEncode` (read with the strict reader), 20 s each, no finding.
  - `fix-mount03.log`, `fix-race03.log` (`-race -count=2`, pass; the mount takes 12.2 s and 12.4 s): `TestRunServeQuerylogDnstapSinkPrivacyAndRaw` now excludes the client 127.0.0.2 and queries from it before and after raw is on, and none of its queries reaches the collector; the node holds a test license with the audit feature, and `/audit` returns one `querylog.raw` success by `admin` and at least one `querylog.raw.export` by `System/query-log` for the sink. Retained: `fix-mount01.log` (the test's audit read hit the 32 KiB response cap of the shared request helper; now filtered by kind), `fix-mount02.log` (the test expected a node name on the stored event, which the node's own SQLite audit store does not keep; the check now uses the reason naming the target), `fix-mount04-without-client-exclusion.log` (the exclusion removed: the excluded client's raw query reaches the collector and the test fails).
  - `fix-lint01.log`: scoped golangci-lint over `internal/config`, `internal/api`, `internal/querylog`, `test/fuzz`, `cmd/kudurru`: 0 issues. `fix-gen01.log`, `fix-gen02.log` then `fix-drift01.log`: after the second generated commit (CRD help text), `TestOpenAPIDrift`, `TestReferenceDrift`, `TestAggregatedRolesDrift` and `TestCLIReferenceDrift` pass.
- Retained failures: `race01.log` and `race05.log` show `TestRunServeQuerylogDnstapSinkPrivacyAndRaw` failing with HTTP 400 on PATCH /settings. The cause was the store's CRD validator, whose `querylog.sinks[].type` enum had not been regenerated (`Unsupported value: "dnstap"`); after `make generate` (`gen02.log`) the test passes. The integrator must regenerate after cherry-picking, or the mounted test fails the same way. `race01.log` also shows `TestQuerylogSinkTypesHaveFactories` failing from a working state before the dnstap entry was added to the test. `lint02.log` and `lint03.log` list the lint findings fixed in the last source commit.

Open: no API route, CLI or dashboard surface shows sink state (`SinkStatus.Raw` exists
but `SinkHost.Status()` is still not mounted); no test reads the stream with a
third-party Frame Streams implementation (DNS-collector or fstrm): the independent
reader is a second hand-written implementation in this repository; the raw gate
treats every exclusion change under a raw sink alike, so adding an exclusion also
needs `querylog.raw`; renaming a client that a name in `querylog.exclude.clients`
refers to is governed by `clients.write`, not `querylog.raw`; the settings import of
a migration does not produce `querylog` keys today, so it is not gated;
`FORWARDER_*` messages are summaries, not the real upstream exchange; `dnstap
listen` (OBS-3) is not part of this package; the response wire is the pipeline's
answer before transport truncation or padding; the full `./cmd/kudurru` suite,
`test/e2e`, the paired performance gate (F20d/F63) and mixed-version behavior were
not run in this lane.
