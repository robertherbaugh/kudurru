### ID-1 Client IDs through the dnsdist edge — November 12, 2026 (D389)

A client that reaches kudurru through the dnsdist edge's DoT, DoQ, DoH or
DoH3 frontend now selects the same ClientGroup and policy with its client ID
as it would against a kudurru listener directly. The new host settings
`edge.clientIDDomain` (`--client-id-domain`) and `edge.clientIDs`
(`--client-ids`) make both renderers (YAML with its sibling functions file,
and Lua) emit one rule, `kudurru-client-id`. The rule sits after the
shared-secret rule and before pool routing. Its Lua function derives the ID
by the `internal/clientid` rules: one DNS label before the suffix in the
DoT/DoQ server name (case-folded, a trailing dot dropped), or one path
component after the DoH/DoH3 path. It sends the ID in PROXYv2 TLV `0xE1` and
marks the query `skipCache`, because the edge packet cache has no client key.
With path IDs on, the DoH frontend stops exact path matching, and the rule
answers HTTP 404 for empty, extra, escaped or invalid components, as kudurru
does. The trust rule is unchanged: kudurru honors `0xE1` only from a listed
peer that also sends the valid `0xE0` secret. Topology validation therefore
refuses client IDs without `proxyProtocolSecretFile`. It also refuses a
suffix with no room for an ID label and a DoH path that an ID cannot follow.
The pinned dnsdist 2.1.3 image provided the Lua API used here
(`getServerNameIndication`, `getHTTPPath`, `addProxyProtocolValue`,
`setHTTPResponse`), checked by a probe before the design was settled.

Evidence under `.context/v020-final-20261110/evidence/EDGE-CLIENT-ID/`:

- `probe01.log`: a probe of the pinned image (`kudurru-dnsdist:vip-fix-b8fd96f`,
  sha256:89c7f196a871...) on all five frontends. It records what each Lua
  accessor returns per protocol. The raw SNI keeps case and a trailing dot.
  The DoH path never includes the query string, and escaped paths reach Lua
  with `%`. DoH3 has no path filter. A `setHTTPResponse(404)` with
  `HeaderModify` gives HTTP 404 on DoH and DoH3.
- `check01.log`: `dnsdist --check-config` in the pinned image ran on every
  golden directory except `*-ebpf`, YAML and Lua, including the new
  `client-ids`: all "Configuration OK".
- `race03.log` (final code, `-race`): `TestRunServeEdgeClientIDLiveDnsdist`
  PASS (36.0 s), with subtests yaml and lua, against a real `runServe` on
  SQLite with native DoT/DoQ/DoH/DoH3 listeners. In the same run these
  passed: `TestEdgeRenderClientIDs`, `TestEdgeRender`, `TestEdgeRenderDnstap`,
  `TestCLIReferenceDrift`, `TestChartSupervisorArgs`, `TestEdgeFrontendFlags`,
  the ten `TestSupervisor*` settings-layer tests and the `TestEdge*`
  supervisor tests (`ok cmd/kudurru 39.3 s`). The live test covers four IDs
  (`phone`, `PHONE`, `laptop`, none) on four transports, native and through
  both rendered edges. It also covers a two-label server name, five refused
  paths (404 on both sides) and three direct PROXYv2 datagrams: the
  authenticated `0xE1 phone` blocks, and the ID without the secret and the ID
  with a wrong secret are dropped.
- `live08.log`: the same live test, `-count=6`, 6/6 PASS (48.3 s, code before
  the last fixture refactor to `proxyproto.Header.Append`). `live07.log`:
  5/5 PASS. These runs used a single DoH3 retry and no DoQ retry; the
  reviewer's runs below showed that this was not enough.
- Review fix, cause of the lost QUIC requests (`fix-probe01.log` to
  `fix-probe05-upstream-image.log`, probe source `fix-probe-source.go.txt`).
  A probe rendered the same edge (YAML, client IDs on) for a logging backend
  and sent queries with unique names, recording quic-go qlogs and dnsdist
  `--verbose` output. Every lost request was absent from the dnsdist verbose
  log and never reached the backend. In `fix-probe03.log`, 9 of 60 DoH3
  requests on fresh connections were lost (0 of 60 DoQ). The qlog
  classification (`fix-probe03-qlog-classes.log`) matched exactly: all 9
  lost connections, and none of the 111 answered ones, sent the request
  stream's FIN in its own empty STREAM frame after the data. With the FIN
  held back by 200 ms on purpose (`fix-probe04.log`), 10 of 10 DoQ and 10 of
  10 DoH3 requests were lost. The upstream `powerdns/dnsdist-21` 2.1.3 image
  behaves the same (`fix-probe05-upstream-image.log`: 6/6 and 6/6 lost with
  the FIN alone). A reused HTTP/3 or DoQ connection lost 0 of 30 in
  `fix-probe02.log`, because quic-go then usually sends the FIN with the
  data, not because reuse avoids the defect. This is a dnsdist defect in
  the pinned image: a request that ends with a separate FIN is never
  processed. It is not caused by the client-ID rule. (`fix-probe01.log` is a
  retained failed probe: its backend did not strip PROXYv2 on TCP. That run
  also showed that dnsdist forwards DoQ and DoH3 queries to the backend over
  TCP.)
- `fix-live02.log` (`-race`): `TestRunServeEdgeClientIDLiveDnsdist` PASS
  (27.4 s) with the review fixes. The test now attaches the default resolver
  packet cache to the rendered edge, as the supervisor does at run time once
  the cache is qualified, and uses an upstream that answers with TTL 300.
  After the no-ID round it repeats the no-ID query on all four edge
  transports and requires that the upstream query count stays unchanged
  (served from the edge cache). It then requires NXDOMAIN for ID `phone` on
  DoT, DoQ, DoH and DoH3. Each side also sends one DoQ and one DoH3 request
  with the FIN held back: kudurru must answer both, and the edge must still
  not answer them. This canary fails once a dnsdist bump fixes the defect,
  so the retry can be removed then. A DoQ or DoH3 edge request is tried
  again on a fresh connection only when the quic-go tracer saw its FIN go
  alone, after a 3 s cut-off, with at most six attempts. Any other failure,
  and any native failure, is final.
- `fix-live03-no-skipcache.log`: the counterfactual. With
  `dq.skipCache = true` removed from the rendered Lua, both subtests FAIL:
  "edge DoT/DoQ/DoH/DoH3 with ID phone after a cached no-ID answer: rcode
  NOERROR, want NXDOMAIN". The line was restored afterwards.
- `fix-live04.log` (`-race -count=8`, load average 9 to 15 from other
  lanes): 8/8 PASS (222.8 s), 16 subtests. It logged 18 FIN-alone retries
  (13 yaml, 5 lua, all DoH3) out of about 270 edge QUIC requests, about
  7 %. No other QUIC failure occurred.
- `fix-lint02.log`: golangci-lint v2.15.0 reports 0 issues on
  `cmd/kudurru/...` with the review fixes (`fix-lint01.log` is a retained
  run that refused to start because another lane's lint held the lock). The
  bench guards report OK.
- `dnsserver01.log` and `race02.log`: `TestTransportClientIDsMakeEquivalentPolicyDecisions`
  and `TestCapturedTransportClientIDs` PASS, extended with authenticated
  `0xE1` over the PROXYv2 UDP and TCP backend listeners and a dropped forged
  TLV. The `TestProxyProtocol*` tests PASS, and the full `internal/dnsserver`
  passed under `-race` (47.6 s).
- `race01.log`: `ee/edge` (new `TestBuildClientIDs`), `ee/edge/dnsdist`
  (`TestRenderGolden` with the `client-ids` case) and `internal/config` PASS
  under `-race`.
- `lint01.log`: golangci-lint v2.15.0 reports 0 issues on `ee/edge/...`,
  `internal/config/...`, `internal/dnsserver` and `cmd/kudurru/...`.
  `drift01.log`: `TestReferenceDrift` PASS after `chore(gen)`. The bench
  guards report OK.

Retained failed runs:
- `live01.log`: the fixture certificate lacked `*.phone.dot.example.com`.
- `live02.log`, `live03.log`, `live05.log` and `live06.log`: the pinned
  dnsdist now and then never answered a request on a fresh HTTP/3
  connection, with valid and refused paths, under both renderers, and once
  in the first probe with no client-ID function at all. The single DoH3
  retry added then did not cover it.
- `review-live01.log` (`-race -count=2`, reviewer): 0/2 PASS. One lua
  subtest lost a DoQ request (no DoQ retry existed), and one yaml subtest
  lost both the first DoH3 attempt and its retry. `review-live02.log`
  (`-race -count=5`, reviewer): 4/5 PASS; the failing run lost a DoH3 retry
  in both subtests. Every passing run logged 3 to 4 retries out of about 18
  DoH3 requests per subtest. Together that is 3 failures in 7 runs, and DoQ
  is affected as well as DoH3. The cause is the separate-FIN defect above;
  the earlier "every second attempt succeeded" was wrong.
- `live06.log` also failed twice because the fixture's Do53 port was free on
  UDP but not on TCP ("Address already in use" from dnsdist). The fixture now
  picks ports that are free on both and distinct.
- `unit03.log`: `TestSupervisorLifecycle` failed once ("4 starts, want 2") on
  the loaded machine. This is the known contention flake owned by
  FLAKE-PRIMARY-UNAVAILABLE. `unit01`, `unit02` and `race01` passed.

Still open: the pinned dnsdist 2.1.3 (and the upstream 2.1.3 image) never
answers a DoQ or DoH3 request whose FIN arrives in its own empty STREAM
frame. Real clients that send the FIN that way behind the edge wait until
their own timeout; a quic-go client did so for about 7 % (live test) to 15 %
(probe) of DoH3 requests on fresh connections here. This is not caused by
kudurru and is not fixed here: it needs a dnsdist fix and a pin bump. It
should be recorded as a known issue in `CHANGELOG.md` and
`dev-docs/followups.md`; `docs/deploy/client-identity.md` now describes it.
The test's retry is limited to exactly this case, and its canary fails when
an image bump fixes it. No other DoQ or DoH3 client was tried (curl here has
no HTTP/3). The Helm chart has no dedicated value for these settings. Chart users pass
`--client-id-domain=...` and `--client-ids` through the existing
`edge.extraArgs`, and Compose users through the edge command or its
configuration file. No chart value was added. The full `./cmd/kudurru` suite, `test/e2e`, `make
edge-check` with the upstream image digest, and hosted CI were not run.
