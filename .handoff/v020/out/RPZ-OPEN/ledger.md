### RPZ-OPEN Close what verification left open in RPZ-KUBE, RPZ-DIAG and OPS-COREDNS — November 13, 2026 (D390, D391, D392)

**RPZ load window (D390).** A just-started node no longer forwards a listed
name unfiltered while the list resolver compiles an RPZ list. The policy index
now keeps every `blockListRefs` entry that exists and is enabled but is not
compiled yet in the decision (`Decision.PendingBlockLists`, scoped by the
list's client groups like compiled lists), and such a decision filters. The
Kubernetes selector (`kubeRPZ.capture`) adds those lists to the RPZ selection,
so the feed is enforced from a persisted copy or fails closed from the first
answer, for either `blocking.allowDegraded` value. The SQLite complete-bundle
selector (`internal/servingbuild/dns.go`) received the same line for
consistency, but the SQLite runtime never had the window:
`listfetcher.PrepareComplete` compiles every enabled, non-deleting BlockList
into the same candidate the policy index is built from (a source that was
never downloaded contributes nothing, and an RPZ source is external), so
`PendingBlockLists` is empty there. The only SQLite case that reaches it is a
BlockList held by a finalizer while it is deleted: the list leaves the compiled
snapshot but stays in the bundle's RPZ binding, so its feeds stay required
until the deletion completes. The downloaded sources of a mixed
list still apply only once the list is compiled. The readiness half had a
separate cause, found with a trace (`debug02.log`): on a start from
`local.db`, `zonecache.Cache.Ready` reported ready as soon as the cache switched
to `FromSnapshot`, before the debounced rebuild published the snapshot built
from `local.db`. The served snapshot was still the empty unsynced one, whose
policy index references no list, so the blocking readiness check passed; the
node was ready, then not ready ("list default/feed not loaded") once the
restored snapshot arrived, then ready again. Ready now waits for the published
`FromSnapshot` snapshot, as the `Live` branch already did. The architecture
page's promise that `allowDegraded: false` keeps the node not ready until every
referenced list is in the policy is restored, and the documented gap is
removed.

**Unsigned reuse on an offline restart.** `TestRunServeKubeRPZFeeds` now has an
unsigned feed (`open.test.`, its own primary) selected by a separate policy for
client 127.0.0.3. Before the offline restart its primary goes down too; after
the restart (apiserver gate closed) the unsigned feed answers NXDOMAIN over UDP
and TCP from the `local.db` copy while the signed feed of the other policy
fails closed, the unsigned feed's client still resolves unlisted names, and no
transfer of the unsigned zone happened. The user-facing claim is now tested.

**Discarded RPZ copies (D391).** When the manager discards a copy because the
configuration or credentials changed during persistence or acceptance
(`complete`, `persistAndAccept`), it now drops the status fields that describe
the copy too (`entry.dropCopy`: serial, stats, rules, ignored owners, primary,
confirmation, expiry). The API then reports the feed `unavailable` without copy
fields or `RPZRulesIgnored`. The clock-step case, where the node still holds the
copy, keeps its fields and now says why it is unusable. The no-copy message
says "This node holds no copy" instead of "No copy has been transferred". The
CLI fake API and `lists-rpz-all-wide.txt` carry the current pending message.

**CoreDNS importer (D392).** `cache.minTTL` is always emitted (the `success`
MINTTL or the CoreDNS default 5 s) and lowered to `cache.maxTTL` when larger,
with a loss line, as CoreDNS lets the TTL win; `prefetch` amounts above 2^30,
windows above 2^31-1 s and such `serve_stale` durations are omitted with a loss
line (`prefetch 9223372036854775807` had emitted a threshold the settings
loader rejects), and `serve_stale 0` says it turns stale answers off. Blocks
that are not converted take part in routing: a classless reverse prefix routes
the zones CoreDNS expands it to (`10.0.0.0/12` → `0.10` … `15.10.in-addr.arpa`),
and a key that cannot be read (placeholder, unsupported form) leaves routing
unknown, so no `hosts` or `rewrite` answer is imported and the report says so.
Routing is per port: `home.example:5353` no longer shadows the root block's
names on port 53; such names are imported with a loss line naming the port.
When blocks on different ports both answer one name, the block on the lowest
port wins and the other answers are counted in a loss line naming their port
(`addRewrite`); two answers on one port are still refused. An encrypted
classless key (`tls://10.0.0.0/12`) is an encrypted key like the others, with
the prefix redacted: alone it omits its block with the encrypted-listener line,
next to a plain key it is named in the encrypted-keys line (it had aborted the
import with "corefile server block has no keys", or vanished from the report).
While a block with unreadable keys exists, `forward` routes are still imported
and one loss line counts the conditional routes that block may take in
CoreDNS.
Every plan that a CoreDNS test converts goes through `validatePlan` (the CRD
validator) and `config.Load` over the defaults and over a destination with
`cache.minTTL: 1h`.

Evidence under `.context/v020-final-20261110/evidence/RPZ-OPEN/`:

- `debug01.log`, `debug02.log`: temporary trace of readiness and DNS answers
  for `blocked.rpzlab.net` during the offline restart (instrumentation removed).
  `debug02.log` shows `503 zonecache(waiting for the initial sync …)`, then
  `200 ready` with SERVFAIL, `200 ready` with NOERROR, `503 blocking: list
  default/feed not loaded` with NOERROR, then `200 ready` with SERVFAIL and EDE
  "RPZ policy.test. unavailable". Both runs passed (the debug test did not
  assert).
- `kube02-race.log`: `go test -race -v -run 'TestRunServeKubeRPZFeeds$|TestKubeRPZ'
  ./cmd/kudurru`: 2 PASS (TestRunServeKubeRPZFeeds 67.4 s). Watches: restart
  with the primary down 28 answers for the listed name, none NOERROR; offline
  restart 339 answers, none NOERROR, readiness never dropped after it was first
  reported; online restart 30 answers, none NOERROR.
- `kube03-negative-control-no-pending.log`: the same test with the pending loop
  of `kubeRPZ.capture` removed FAILS at the first restart: "the listed name
  resolved NOERROR 18 times of 32; first at 2.29s" (30.9 s).
- `kube04-negative-control-no-readiness-fix.log`: the same test with
  `internal/zonecache/cache.go` at HEAD FAILS at the offline restart: "the node
  reported ready, then not ready 14 times" (55.3 s).
- `kube01-zonecache-negative-control.log`: `TestColdStartReadyOnlyOncePublished`
  FAILS without the readiness fix (Ready true before publication).
- `diag01-negative-control.log`: `TestRuntimeDiscardedAcceptanceClearsCopyStatus`
  FAILS without `dropCopy` (serial 1 and its stats still reported).
- `coredns01-negative-control.log`: the new CoreDNS tests against the previous
  `coredns.go` (with a stub `classlessKey`): 4 FAIL — settings validation
  errors `cache.minTTL (5s) exceeds cache.maxTTL (3s)`, `(1h0m0s) exceeds (10s)`,
  `cache.prefetch.threshold: 9223372036854776000 is not an integer`, the
  classless block's shadowed PTR imported, and the port 5353 block shadowing
  port 53.
- `coredns02-race.log`: `go test -race ./internal/importers`: ok (30.7 s).
  `coredns03-lint.log`: golangci-lint v2.15.0 on `./internal/importers/...`:
  0 issues.
- `unit01-race.log`: `-race ./internal/policy ./internal/zonecache ./internal/rpz`:
  ok. `unit02-race.log`: `-race ./internal/stages/blocking ./internal/servingbuild
  ./internal/api`: ok (servingbuild 458 s, api 476 s on a shared machine).
- `kube05-family.log`: `go test -race -v -run 'TestRunServeKube|TestRunServeOffline|TestKubeRPZ|TestRunServeSQLite.*RPZ|TestConvenienceGolden|TestRunServeSQLiteComplete|TestRPZ' ./cmd/kudurru`:
  29 PASS, 0 FAIL, 0 skip, 625.6 s, including `TestRunServeKubeRPZFeeds`
  (58.3 s), `TestRunServeSQLiteRPZFeedDiagnostics`, `TestConvenienceGolden`
  (the updated wide golden file), `TestRunServeKubeOfflineSnapshot`,
  `TestRunServeKubeBlockingTwoReplicasRestartAndOutage` and
  `TestRunServeKubeStoredTSIGStartupTransportsRestartAndOffline`.
- `lint01.log`: golangci-lint v2.15.0 on `./cmd/kudurru/... ./internal/policy/...
  ./internal/zonecache/... ./internal/rpz/... ./internal/api/... ./internal/servingbuild/...`:
  0 issues.
- `drift01.log`: `TestOpenAPIDrift`, `TestReferenceDrift`,
  `TestAggregatedRolesDrift`, `TestCLIReferenceDrift`: ok. No generated
  artifact changed, so there is no chore(gen) commit.
- `bench-guards01.log`: 14 bench guard tests OK.
- Review fixes: `fix-01-coredns-negative-control.log`: the three new tests
  (`TestCoreDNSOfflineEncryptedClasslessKeys`,
  `TestCoreDNSOfflineSameNameOnTwoPorts`,
  `TestCoreDNSOfflineUnreadableBlockAndConditionalRoutes`) against the previous
  `coredns.go`: 3 FAIL ("corefile server block has no keys", "corefile gives
  the same name more than one local answer", missing conditional-routes line).
  `fix-02-coredns.log`: `go test -v -run TestCoreDNS ./internal/importers`:
  16 PASS (7.6 s), including the golden fixture. `fix-03-importers-race.log`:
  `go test -race ./internal/importers`: ok (63.5 s). `fix-04-lint.log`:
  golangci-lint v2.15.0 on `./internal/importers/...`.

What remains open: the SQLite pending-list line has no test that fails
without it (the review's mutant of `internal/servingbuild/dns.go` passes
`./internal/servingbuild`, `review-sqlite-mutant-servingbuild.log`), because
no ordinary SQLite state has pending lists (see above); whether a BlockList
being deleted under a finalizer should keep requiring its feeds on either
driver is not decided. A signed feed restarted without the apiserver
still fails closed instead of reusing its persisted copy (unchanged design
decision of D390). With `allowDegraded: true` a just-started node still answers
before downloaded lists compile, as documented; only RPZ feeds no longer wait.
In the CoreDNS importer, a classless prefix written with IPv6 brackets is
treated as an unreadable key (conservative: no local answers imported), and
names another block takes on a different port are imported with a loss line
rather than omitted, because kudurru has one listener set.
