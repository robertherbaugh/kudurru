### OPS-1 CoreDNS Corefile offline importer — November 10, 2026 (D392)

`kudurru import coredns --file Corefile` (or `--file -` for stdin) now converts a
CoreDNS Corefile into the same reviewable `Plan` as the AdGuard Home and Blocky
importers, and `--apply [--wait]` imports it through the existing tracked
migration with normal validation, ownership and license checks. A hand-written
reader (`internal/importers/corefile.go`) implements the server-block grammar that
CoreDNS 1.12 documents: comments, double-quoted tokens, one or more keys
(`[scheme://]zone[:port]`, domains, `.` or octet/nibble reverse prefixes), braced
blocks or a single braceless block, directive sub-blocks, and snippets defined in
the file and expanded by `import name`. No CoreDNS or Caddy code or package is
used and no dependency was added. The reader bounds input (4 MiB), tokens
(200,000, 64 KiB each), nesting (8), server blocks (1,024) and snippet expansion
(200,000 directives), and refuses unbalanced braces, unterminated quotes and
duplicate snippets without echoing values.

The converter (`internal/importers/coredns.go`) maps `forward` in the root block
to `upstreams.groups.default` and other forwards to named groups with
`upstreams.conditional` routes (`policy` and `force_tcp` are honored); inline
`hosts` entries, with PTR answers unless `no_reverse`, and
`rewrite name [exact] FROM TO` become rewrites of `Policy/default` (at most 128);
the root block's `cache` becomes `cache.*` (TTLs 1 to 2^31-1 seconds, a minimum
TTL of 0, capacities 1 to 2^30; any other value omits the directive as a unit)
and an `acl` of the form type blocks,
network allows, final block becomes `control.blockedQTypes` and
`control.allowedNetworks`. Forwarders with `tls_servername`, a client `tls`
certificate, `except` or a resolv.conf file are omitted as a unit, following the
Blocky precedent. `file`, `auto` and `secondary` zones are named so they can be
imported with `kudurru import bind`. The converter first collects the zones of
every plain DNS server block and follows CoreDNS routing (longest matching
zone): a hosts name, a `rewrite name` source or a `forward FROM` zone that falls
inside a more specific block is omitted, and a hosts PTR answer is kept only
when its reverse name reaches the defining block, so a forward-zone block never
invents reverse answers and two such blocks sharing an address no longer
collide. Each omission is counted per block, naming the more specific block's
zone. A block mixing encrypted or gRPC keys with plain keys converts its plain
keys, and every encrypted key is named per block. Encrypted and gRPC listener blocks, ports,
imported files, environment placeholders and every other plugin are reported;
plugin names are echoed only when they are documented CoreDNS plugins, and other
directive names and all values and paths are redacted. The same zone in two
plain DNS blocks is refused as ambiguous. The preview's warnings state the syntax
version. `previewOffline` was split into shared read/plan/finish steps, and the
migration journal and API accept the `coredns` source type.

Evidence under `.context/v020-final-20261110/evidence/OPS-COREDNS/`:
- `internal/importers` (race, `race03-importers`, `race04-importers`): all pass,
  including `TestCoreDNSOfflineFixtureGolden` (fixture `testdata/coredns.Corefile`
  against `testdata/coredns.golden.json`, three repeated previews byte-identical,
  no `secret-` marker or `/etc/` path in the plan), `TestCoreDNSOfflineConversionShapes`,
  `TestCoreDNSOfflineGrammar` (braceless form, quoted tokens, recursive snippets,
  14 malformed or ambiguous inputs refused), `TestCoreDNSOfflineRefusalsAndRedaction`,
  `TestCoreDNSOfflineReadsNothingAndCancels` (real files at the referenced paths
  hold markers that never appear), `TestCoreDNSOfflineSnippetExpansionBounded`,
  `TestCoreDNSOfflineLargeInputsStayLinear` (2.2 s for five large inputs) and the
  existing offline tests, extended to `coredns`.
- Review fixes (after the independent review of `f0a12b48`), source commit
  `5c2fee9f`: `fix-01-before.log` (retained failure) shows the two new tests
  failing on the reviewed code: `TestCoreDNSOfflineServerBlockRouting` refused a
  root `forward home.example` beside a `home.example` block, and
  `TestCoreDNSOfflineCacheBounds` got an overflowed `2157299h53m50s` TTL, a `0s`
  maxTTL, a negative `maxEntries` and no loss line for all 8 cases.
  After the fix, `fix-02-importers-race.log` (`-race -count=2 ./internal/importers/`)
  passes in 30.2 s and `fix-03-coredns-v.log` lists all 9 `TestCoreDNS*` tests
  passing. `TestCoreDNSOfflineServerBlockRouting` covers root hosts/rewrite names
  inside a `home.example` block (omitted with a counted loss line, the PTR the
  root block still answers kept), a shadowed `forward FROM`, root PTRs shadowed
  by a `10.0.0.0/8` block, hosts in `example.org`/`example.net` sharing one
  address (accepted, no invented PTR), and a mixed `tls://` plus plain key
  block. The golden fixture moved its converted hosts entries to
  `lan.example` and keeps `nas.home.example` to pin the shadowing line.
  `fix-04-fuzz.log`: 60 s, pass. `fix-05-lint.log`: golangci-lint on
  `internal/importers`, 0 issues. `fix-06-cli-race.log`: `./cmd/kudurru -race
  -run 'TestCoreDNSCLI|TestOfflineCLI|TestCLIReferenceDrift'`, pass (30 PASS lines including subtests, 0 skips, 52.9 s).
  `fix-07-bench-guards.log`: 14 tests OK. `fix-08-docs-drift.log`:
  TestReferenceDrift passes. No generated artifact changed, so there is no new
  `chore(gen)` commit.
- `FuzzCoreDNSPreview`: `fuzz01`–`fuzz04`, 60–90 s each, no failure. The
  zero-exec stretches in `fuzz01`–`fuzz03` are input minimization (the default
  60 s minimize time); `fuzz04` with `-fuzzminimizetime 1s` runs continuously.
  `fuzz01` led to bounding expanded snippet bodies and the acl/hosts duplicate
  checks before the source commit.
- `./cmd/kudurru` with `-race -run 'TestCoreDNSCLI|TestOfflineCLI|TestBlockyCLI|TestAdGuardCLI'`
  (`race01`, `race02`, `race05`): pass, 69.5 s in `race05`.
  `TestCoreDNSCLIPreviewAndSelection` goes through the real CLI router (no
  destination contact, determinism, selection and flag refusals, malformed input).
  `TestCoreDNSCLIActualApply` previews without a store write and applies
  `--apply --wait` on the mounted SQLite serving command fixture: the run
  completes with settings persisted, the journal has source `coredns` and no
  markers, `Policy/default` holds the hosts and PTR rewrites, and
  `kudurru settings get` shows the forwarders, the conditional route, the access
  network, the blocked type and the cache TTL. `TestOfflineCLIWaitCancellationKeepsTrackedJob`,
  `TestOfflineCLIContextNamespaceWithoutDestinationAccess` and
  `TestOfflineCLIRefusedStartIsNeverReplayed` gained `coredns` subtests (403/409/503).
- `internal/api -race -run 'Offline|Migration'` (`race06-api`): pass, 23.3 s.
- Scoped golangci-lint on `internal/importers`, `internal/api`, `api/internalkinds`
  and `cmd/kudurru` (`lint02`): 0 issues; `lint01` (retained) found a revive
  unused parameter and a gosec taint false positive on the golden write, both fixed.
- Bench guards (`bench-guards01`): 14 tests OK.
- Generated commit `f0a12b48` (`gen01`: make openapi, client-go, gen:api, docs-gen), then `drift01`: TestOpenAPIDrift, TestCLIReferenceDrift, TestForwardedShapes (cmd/kudurru, internal/api) and TestReferenceDrift (hack/docsgen) pass.

Still open: the DNS answers of an imported Corefile are not queried end to end in
this package (the review's routing finding came from that gap; routing is now
proven at the plan level only); the mounted test proves the objects and settings persist through
the tracked migration, and policy rewrites, conditional forwarding, cache and
control settings keep their existing serving tests. Hosts files, zone files and
imported files are reported but never converted (by design); `tls_servername`
forwarders need manual conversion. Listener ports are still merged: routing
compares zones across all plain DNS blocks regardless of port, so a root block
on :53 and a `home.example` block on :1053 still omit root entries under
`home.example` (reported, never silent). The dashboard has no offline-import page for
any source, so `coredns` is CLI-only like `adguard` and `blocky`. The full
`./cmd/kudurru` suite (hack/go-test.py) was not run.
