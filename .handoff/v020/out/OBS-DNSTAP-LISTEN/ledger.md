### OBS-DNSTAP-LISTEN dnstap listen with duplicate and loop protection — November 10, 2026 (D372)

Map entry OBS-3 (README contract: dnstap listen, prevent duplicate logging and
loops from dnstap input). Base commit `b69c545a` (lane branch `wip/v020/obs`,
after OBS-OPEN). Source commits: `da8461a0` (listener, dedup index, Origin,
config, mount and docs), `b79bebc5` (managed dnsdist edge renders dnstap of its
own answers; live test), `210b8019` (a pre-existing order-dependent assertion
in `TestQuerylogSinkCredentialsHonorOnlyTheNodeGrants` compares as a set).
Generated commit `cf8c1e80` (`make docs-gen`; `make openapi` produced no
change). Review fixes: `8f32ffe6` (accept retry, never-fatal runnable,
`unforwardedIdentities`, 6 s pair wait, bounded pairing FIFO, clock clamp)
and `44e26906` (the live test exercises the cache-hit rule), then the generated
commit `406d0091` (`make docs-gen` for the changed setting and flag help;
`make openapi` produced no change). `210b8019` is the
same change as `8d9e976e` already on the PR branch and can be dropped when
cherry-picking. No new dependency: the decoder is hand-written on `protowire`, which
was already linked.

What now happens. `listeners.dnstap` (host-only, restart required, at most 8
entries) binds Frame Streams listeners on `unix:/path` (mode 0660),
`tcp:ip:port` (`allowFrom` required) or `tls:ip:port` (`tls.certFile`/`keyFile`,
and `clientCAFile` or `allowFrom`). The bidirectional and the unidirectional
handshake are accepted, at most `maxConnections` senders at a time (default
16, maximum 256). `CLIENT_*` and `AUTH_*` messages become query events of the
node's one collector, so they go through `querylog.privacy`, the exclusions,
the local database, the live view and the additional sinks; a query waits 6 s
for its response (longer than the pipeline's 5 s bound, so a query kudurru
answers slowly is still recognized) and the pair is one row, a lone response
or a lone query is one row. `FORWARDER_*`, `RESOLVER_*` and other types are counted and not
recorded, so one client query stays one row. The rows have status `unknown`
and the node is this node; `Event.Origin` holds the sender identity in memory.

Duplicates: the collector records every native query it admits, before the
privacy transform, in a bounded `DedupIndex` (client address, source port,
name, type, class; 30 s, 65,536 entries, 8 times per key). Each ingested row is
held 1 s and dropped when one native query lies inside its time window plus or
minus 2 s; one native query cancels one message. Messages of an identity in
`unforwardedIdentities` (a sender that reports only its own answers, such as
the managed edge) skip the check, so a packet cache hit from the same client
port as the miss forwarded before it is still a row. A message time more than
5 minutes from the receive time is moved to the receive time, keeping the
latency, and counted in `kudurru_querylog_dnstap_clock_skew_total{listener}`. The DNS ID is not part of the
key because dnsdist renumbers what it forwards; the real client address and
port reach kudurru through the edge's PROXY protocol. `Event.ClientPort` is set
by the log observer and cleared by the collector before any consumer. Loops: a
dnstap sink never sends an event with `Origin`, and messages with the identity
of `node.name` or of one of the node's dnstap sinks are dropped as `self`.
Bounds: data frames above 192 KiB are read past (a length above 16 MiB ends the
connection), undecodable messages are skipped, at most 10,000 messages wait
in each stage of a listener (decoded, queries waiting for a response with a
pairing FIFO compacted at twice that, rows held for the check), a failed
accept is counted as `accept_failed`, logged and retried after 5 ms to 1 s and
never ends the listener, the `querylog-dnstap` runnable always returns nil (so
the runner never shuts the node down over ingest) and logs a listener that
stops early, every connection is read at the sender's pace into non-blocking
queues and `Enqueue` never blocks; all of it is counted in
`kudurru_querylog_dnstap_frames_total{listener,result}` and
`kudurru_querylog_dnstap_connections_total{listener,result}`. The managed edge
(`edge.dnstapTarget` / `--dnstap-target`, `edge.dnstapIdentity` /
`--dnstap-identity`, default the server ID) renders a dnsdist Frame Streams
logger (tcp or unix; dnsdist's loggers have no TLS) with `DnstapLog` cache-hit
and self-answered response rules in YAML and in Lua, so dnsdist reports only
the answers no kudurru node saw; its identity belongs in the listener's
`unforwardedIdentities`.

Evidence under `.context/v020-final-20261110/evidence/OBS-DNSTAP-LISTEN/`:

- `race06.log`: `go test -race -count=1 ./internal/config/ ./internal/querylog/... ./internal/stages/log/ ./ee/edge/...`, 16 packages ok (internal/querylog/sinks/dnstap 6.3 s, internal/querylog/sqlitelog 235.3 s, ee/edge/dnsdist 20.1 s with the live test skipped there).
- `race07.log`: `go test -race -count=1 -v -run 'TestRunServeQuerylog|TestQuerylog|TestEdgeRender|TestEdgeFrontendFlags|TestSupervisor|TestChartSupervisorArgs|TestRunServeKubeQuerylog' ./cmd/kudurru`, 36 passed, 0 skipped, 112.5 s. It includes the mounted proof `TestRunServeQuerylogDnstapListen` (8.9 s): the real `runServe` on the complete SQLite runtime with a dnstap sink pointed at its own listener; a real UDP query is one row, the sink's two messages come back as `self`, a front end's message about the same query is a `duplicate`, a cache hit is one `recorded` row, and the ingested row is not sent again. Also `TestEdgeRenderDnstap` (0.12 s, new: both renderers, defaults and refusals) and `TestRunServeQuerylogDnstapSinkPrivacyAndRaw` (8.9 s, unchanged sink behavior).
- `race08.log`: `go test -race -count=3 ./internal/querylog/ ./internal/querylog/sinks/dnstap/ ./internal/config/`, ok.
- `race09.log`: `-v` of the listener tests: `TestListenRecordsEachClientQueryOnce` 0.61 s, `TestListenDedupClaimsOneMessagePerQuery` 0.93 s, `TestListenDropsSelfIdentityAndIgnoredClients` 0.46 s, `TestListenBoundsMalformedAndOversizedFrames` 0.47 s (malformed and oversized frames counted, the stream continues, a 1 GiB length ends the connection, a non-dnstap handshake is refused), `TestListenNeverBlocksTheSender` 0.20 s (20,000 frames written against a stalled collector without the sender waiting; overflow counted), `TestListenNeverLoops` 0.48 s, `TestListenPeersAndConnectionLimit`, `TestListenUnixAndMutualTLS` 0.90 s, `TestDedupIndexClaimsOncePerAnsweredQuery`, `TestAsyncRecordsNativeQueriesBeforeThePrivacyTransform`, `TestDnstapListenerValidation` 0.29 s; all passed.
- `live04-yaml.log` and `live05-lua.log`: `KUDURRU_F43_LIVE=1 go test -race -v -run TestDnstapLiveEdgeOneRowPerClientQuery ./ee/edge/dnsdist/` against the pinned image `kudurru-dnsdist:vip-fix-b8fd96f` on the private daemon, YAML 7.45 s and Lua (`KUDURRU_F43_RENDERER=lua`) 7.66 s, both passed: the supervisor runs dnsdist with the rendered dnstap output in front of a PROXYv2 stand-in that logs through the real observer and collector, an operator console rule makes dnsdist also log every relayed response, and five forwarded queries plus one NOTIFY dnsdist refuses itself give exactly six rows (`frames recorded=1 duplicate=5`). The earlier implementer's `live01.log` and `live02-lua.log` show the same result.
- `edge-check01.log` (failed: the new golden files had been written with mode 0600, so dnsdist in the container could not read them; they are 0644 now) and `edge-check02.log` (`dnsdist --check-config` on the new `dnstap` goldens, YAML and Lua: `Configuration OK`).
- `drift01.log`: `TestOpenAPIDrift`, `TestCLIReferenceDrift`, `TestAggregatedRolesDrift`, `TestForwardedShapes`, `TestReferenceDrift` pass after the generated commit. `lint03.log`: scoped golangci-lint v2.15.0 over the touched packages, 0 issues (`lint02.log` retained: two issues, an unchecked supervisor error in the live test and a duplicated `Claim` expression in a test, both fixed).

Review fixes (reviewer's probes in `review-probe01.log` showed the three
defects before the fix):

- `fix01-new-tests-without-fixes.log`: each new test run through `go test -overlay` with only its own fix reverted, all fail: `TestListenRetriesAcceptErrors` (Serve ends at the first EMFILE, the sender never gets ACCEPT), `TestListenUnforwardedIdentitySkipsTheDedupCheck` (the cache hit is claimed, nothing recorded), `TestListenSlowAnswerIsOneRow` (with the old 2 s wait: the constant check fails, and with that check off one row from dnstap besides the native row), `TestListenPairingFIFOIsBoundedByCount` (pairing FIFO 199 entries for a bound of 8), `TestListenShiftsASkewedClock` (row dated an hour ahead).
- `fix07-race-pkgs.log` and `fix16-race-pkgs.log`: `go test -race -count=2 ./internal/querylog/sinks/dnstap ./internal/config ./internal/querylog`, ok (23.1 s, 26.8 s, 2.2 s), including the five new listener tests above, `TestListenRetryWaitEndsAtClose` (Close ends a 1 s retry wait at once) and the new `TestDnstapListenerValidation` cases (`unforwardedIdentities` entries validated like `identities`, and refused when absent from a non-empty `identities`).
- `fix06-cmd-race.log`: `go test -race -count=1 -v -run '^(TestServeDnstapIsNeverFatal|TestRunServeQuerylogDnstapListen|TestEdgeRenderDnstap|TestQuerylogSinkCredentialsHonorOnlyTheNodeGrants|TestRunServeQuerylogDnstapSinkPrivacyAndRaw)$' ./cmd/kudurru`, 5 passed, 18.9 s (`TestServeDnstapIsNeverFatal` 0.22 s: a listener failing every Accept with EMFILE and one closed early are logged, serveDnstap keeps running and returns nil at shutdown; `TestRunServeQuerylogDnstapListen` 8.79 s unchanged).
- Live, pinned `kudurru-dnsdist:vip-fix-b8fd96f`, `KUDURRU_F43_LIVE=1 KUDURRU_F43_WORKDIR=<owned scratch dir> go test -v -run TestDnstapLiveEdgeOneRowPerClientQuery ./ee/edge/dnsdist`: `fix11-live-yaml.log` (YAML, 10.45 s), `fix12-live-lua.log` (Lua, 10.46 s) and `fix14-live-yaml-race.log` (YAML with `-race`, 10.47 s) pass: the resolver pool shows `hits=2 misses=1`, the repeated name has 3 rows (the backend's miss, the same-port hit and the fresh-port hit from the rendered cache-hit rule), and in total `frames recorded=3 duplicate=5`, 9 rows for 9 client queries. `fix13-live-yaml-without-unforwarded.log`: the same test without `unforwardedIdentities` fails (the same-port hit is dropped, recorded never reaches 2). `fix15-live-containers-left.log` is empty: no container left behind.
- `fix21-drift-light.log`: after `406d0091`, `TestAggregatedRolesDrift`, `TestReferenceDrift` and `TestOpenAPIDrift` pass. `fix24-race-pkgs.log`: `go test -race -count=2 ./internal/querylog/sinks/dnstap ./internal/config ./internal/querylog` on the final source, ok (12.0 s, 13.4 s, 1.5 s).
- `fix25-cmd-drift.log`: after `406d0091`, `go test -race -count=1 -v -run '^(TestCLIReferenceDrift|TestServeDnstapIsNeverFatal|TestRunServeQuerylogDnstapListen|TestEdgeRenderDnstap|TestQuerylogSinkCredentialsHonorOnlyTheNodeGrants|TestRunServeQuerylogDnstapSinkPrivacyAndRaw)$' ./cmd/kudurru`, 6 passed, 21.0 s (`TestRunServeQuerylogDnstapListen` 9.84 s). `fix19`, `fix20`, `fix22` and `fix23` are attempts of this run that never started (scope too broad, lock wait, machine restart).
- `fix17-lint.log`: scoped golangci-lint v2.15.0 over `internal/querylog/...`, `internal/config/...`, `ee/edge/dnsdist/...`, `cmd/kudurru/...`, 0 issues (`fix08-lint.log` retained: two revive issues, fixed).

Retained failed runs of the review fixes: `fix02-live-yaml.log`, `fix03-live-lua.log`, `fix04-live-yaml-without-unforwarded.log`, `fix09-live-yaml-diagnostics.log` and `fix10-live-yaml.log`. The first live runs timed out waiting for the cache hits because the test gave the supervisor no resolver settings, so the edge never attached the packet cache (`fix09` shows `hits=0 misses=0` and every repeat reaching the backend). This is the edge's designed cache qualification, not a defect. With `Zones` supplied and the backend's `InternalAddress` set, the cache qualifies (`fix10` timed out on qualification before `InternalAddress` was set).

Retained failed runs: `race01.log` (the mount test's first configuration was invalid: a 50 ms sink flush and a TLS listener without files), `race03-setup-failure-build.log` (a build before `edge.dnstapTarget` existed in the config struct), `unit01-without-origin-skip.log` (with the `Origin` check removed from the sink, `TestListenNeverLoops` records 8 events instead of 1: the test fails without the loop guard), `live03-without-dedup.log` (with the dedup claim disabled the live test never sees the five duplicates and fails: the live test fails without the duplicate guard), `race05.log` (`TestQuerylogSinkCredentialsHonorOnlyTheNodeGrants` failed because its cases run in map order and it compared the two Secret reads in a fixed order; a pre-existing test bug, fixed in `210b8019` by comparing as a set, with the same two reads required).

What remains open. `unforwardedIdentities` is explicit: an operator who runs the managed edge must list its identity, otherwise a same-port cache hit within about 2 s of the forwarded miss is still dropped (the docs and the `edge.dnstapIdentity` help say so). A sender that logs every response must not be listed. A lone query from a sender that logs queries only is now recorded after 6 s instead of 2 s. A message delayed more than 5 minutes (a long sender backlog) is dated at its receive time. The live test's kudurru is a stand-in (PROXYv2 backend plus the real observer, collector and listener), not a `kudurru serve` process; the mounted `runServe` proof feeds the listener with hand-written Frame Streams instead of dnsdist. Unbound was not run live; its messages are the same `CLIENT_*` types the tests cover. Rows from dnstap keep `Origin` in memory only: the query log API and the database do not show which front end sent a row, and there is no dashboard surface for the listener counters. The dedup match needs the real client address and port at kudurru (PROXY protocol); behind a front end without it the forwarded queries are recorded twice when that front end also logs them. Not run: the full `./cmd/kudurru` suite (`hack/go-test.py`), `test/e2e`, hosted CI.
