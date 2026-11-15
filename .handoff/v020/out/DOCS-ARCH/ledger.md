### DOCS-ARCH architecture pages in step with the code — November 10, 2026

Map entries G21-16 and the architecture part of DOC-1. No decision number: no
design decision was needed. Base commit `33ee04bc`; lane branch `wip/v020/docs`.
Seven source commits up to `0d565018`, then three source commits and one
`chore(gen)` commit that resolve the four findings of the independent review
(verdict "accept-with-fixes").

The architecture tour (`docs/architecture/`) described several v0.2 features
as unfinished after they had landed. Each such statement was checked against
the code at `33ee04bc` and replaced with what the code does. Statements about
work that other lanes own (integration `Delete`, Tier-B hooks, the Kubernetes
integration host, settings and sign-out forwarding, managed trust anchors,
query-log sinks) were left as written.

What the pages now say:

- **Index (`README.md`).** A v0.2.0 tour of ten pipeline stages. The tour table
  lists `integrations.md` and `central-stopped-source-rollups.md`. "Built and
  not built" no longer claims that no follower exists, that
  `/nodes/{other}/health` answers 501, that the blocking toggle is per node, or
  that dynamic UPDATE is in progress. A new table, "What depends on the store
  driver", records the complete-SQLite and Kubernetes behavior of RPZ feeds,
  the integration host, RFC 2136, stored TSIG keys, the blocking toggle,
  settings writes, TOTP, session inventory, RoleBinding writes, local-user and
  external-account administration, and enrollment/promotion, with the refusal
  key where Kubernetes answers 503. The loop diagram shows the transfer
  managers, the SQLite serving writer and the DNSEndpoint source, and the text
  explains that the SQLite election follows durable write authority. The
  objects loader appears in both groups of that diagram: every replica runs it
  on Kubernetes, and on SQLite it is a leader-only loop, so an enrolled
  follower never applies its local `objects:` and `objectsDir` files (review
  finding 3).
- **RPZ (`block-lists.md`, `policies-and-blocking.md`, `query-path.md`).** The
  adapter is mounted on the complete SQLite runtime (manager, complete-candidate
  composition, per-request selector, `blocked_rpz`), with a new diagram. The
  pages state that Kubernetes neither transfers nor evaluates RPZ sources and
  that per-feed transfer status has no API, CLI, metric or dashboard surface.
  The status table lists `local_record`, `blocked_upstream` and `blocked_rpz`
  as produced; only `retried` and `in_progress` remain reserved.
- **Client identity (`policies-and-blocking.md`).** The MAC, hostname and
  client-ID tiers are no longer described as unfed; policy `rewrites` are
  served by the `rewrite` stage. `queryLog` on a policy is still read by
  nothing, and the page still says so.
- **Enrolled SQLite membership (`configuration-and-store.md`,
  `deployment.md`).** Activation, invitation, join, follower acquisition,
  reviewed promotion, peer authority observation, offline conflict resolution
  and typed follower forwarding (blocking toggle, object writes, four zone
  commands, local login) are described as mounted. The sentence "Settings,
  other IAM administration and remaining typed root domains are unfinished" is
  kept. Static twins are described as read-only through the API, with a
  diagram comparing them with an enrolled cluster. The deployment table has an
  enrolled-cluster row. The driver comparison table no longer gives the SQLite
  leader as "always this node": it is the node with durable write authority (a
  standalone node, each static twin, or the enrolled primary), and "Used by"
  lists enrolled primaries and followers (review finding 1). The file loader
  paragraph says where the loader runs on each store.
- **Access (`access.md`).** Kubernetes binding revocation floors are durable
  (D364). New section "Local sign-in on an enrolled follower (D363)" with a
  sequence diagram. External sessions: re-evaluated on every request from
  stored login evidence without contacting the identity provider (D212).
- **Zones and TSIG (`zones.md`, `query-path.md`,
  `configuration-and-store.md`).** Kubernetes stored-key management and the
  ten-second reload replace "fixed until restart" and "unavailable". A native
  UPDATE is not forwarded between enrolled SQLite members; Kubernetes serves
  UPDATE through its single-object path.
- **Other.** `integrations.md`: DNSEndpoint partition and lease loss is covered
  by `TestRunServeDirectDNSEndpointLeaseLossAndPartition`.
  `upstreams-and-cache.md`: unblock-request mutations are not "unconverted".
- **Hand-drawn diagrams (`diagrams/*.excalidraw.svg`, review finding 2).**
  Three diagrams carried labels that the rewritten pages contradict. Each label
  was changed twice, in the Excalidraw scene embedded in the SVG and in the
  drawn SVG text, so that a later re-export from the scene keeps the change.
  `overview`: the pipeline box says "seven of ten stages" and names
  postprocess, rewrite and dnssec as not drawn (the seven boxes were not
  redrawn as ten); the SQLite store box reads "Docker / systemd: state.db on
  each node (sqlite driver, leader = node with write authority)" instead of
  "the node is always leader"; the loader arrow reads "objects loader (kube:
  every replica, sqlite: the leader)" instead of "in every node".
  `config-flow`: "leader: the node with write authority" instead of "leader:
  always this node". `policy-decision`: the footnote of the MAC, hostname and
  client-ID tiers reads "only when a source supplies that identity" instead of
  "no transport fills these in yet". In `overview`, one label that an earlier
  hand edit had left 0.23 px off its scene position was moved back so that the
  scene and the drawn text agree exactly in all eight diagrams.

Rendering defects found and fixed while checking diagrams: at `33ee04bc`,
eleven sequence diagrams of the tour did not parse with the Mermaid 11.17.2
bundle the docs site ships. Ten had a semicolon in message or note text (a
semicolon ends a sequence-diagram statement); the eleventh had a closing fence
followed by text on the same line (`configuration-and-store.md`), so the code
block continued and swallowed the following paragraphs. Semicolons in
sequence-diagram text are now written `#59;`, the fence is closed, and the new
diagrams avoid semicolons.

Source strings and comments corrected with the pages:

- Dashboard `MatchersField.tsx` and `GroupDetailPage.tsx`: MAC, host name and
  name pattern matchers no longer say "inventory integrations arrive in v0.2"
  or "this version has none, so it never matches"; they name the sources each
  matcher needs. `policyFields.test.tsx` asserts the new text.
- `internal/config/schema.go`: the `cliClientID` help no longer says
  "(kudurru login, v0.2)"; the dashboard mock schema follows.
- Zone groups (review finding 4): the `zonegroup` scope help of
  `rbac.bindings` no longer says "reserved: zone groups arrive in v0.2". It
  says that zone groups are not implemented and that a binding with this scope
  grants nothing; the dashboard mock schema follows, and
  `TestReservedBindingScopeHelp` (`internal/config`) fails on the old text. The
  same promise had two more sites that the review's search for the exact
  phrase did not find: the `spec.scope.kind` field comment of RoleBinding
  ("zonegroup (v0.2)"), which is the CRD field description and a row of the
  generated CRD reference, and the scope constants of `internal/rbac`. Both now
  say reserved. The CRD copies and the reference page are regenerated in the
  `chore(gen)` commit. No behavior changes.
- Go comments (no behavior change): zone type constants and `zone.NewBuilder`,
  `internal/notifier` (inbound NOTIFY), `internal/store` (drivers and elector),
  the transfer counter, and the TOTP seed field of the internal kinds.

Code paths behind the main corrected statements:

| Statement | Code |
|---|---|
| RPZ mounted on SQLite only | `cmd/kudurru/serve.go` (`feeds` created only when `sql != nil`; `add("rpz", false, feeds.Run)` in the complete branch); `internal/servingbuild/dns.go` (`SetRPZCapture`); `internal/stages/blocking/blocking.go` (`s.opts.RPZ != nil`), `rpz.go` (`StatusBlockedRPZ`) |
| RPZ feed status has no surface | `internal/rpz/manager.go` `Statuses`/`StatusFor` have no caller outside the package |
| Shared edge cache and RPZ | `internal/servingbuild/edge_cache.go` (`cacheTimeline.at`), `internal/rpz/read.go` (`CacheUntil`) |
| `local_record`, `blocked_upstream` produced | `internal/responsepolicy/rewrites.go`, `internal/stages/blocking/rewrite.go`, `internal/dhcpdns/resolver.go`; `internal/responsepolicy/response.go` (`Guard`), `internal/stages/postprocess/postprocess.go` |
| Identity sources | `internal/stages/identify/identify.go`; `cmd/kudurru/serve.go` (`idStage.Hostnames = leaseDNS`, `mountReversePTR`); `internal/dnsserver/dns.go`, `doq.go`, `doh.go`, `handler.go` (client IDs); `internal/policy/index.go` `IdentifyAt` (tier order) |
| Edge does not render the client-ID TLV | `internal/dnsserver/proxyproto/header.go` (`TLVTypeClientID`) is referenced only by `proxyproto/policy.go` |
| SQLite election follows authority | `internal/store/sqlite/sqlite.go` (`Elector`), `authority.go` (`runDynamic`), `internal/store/replication/types.go` (`Authority.Writable`) |
| Peer health read over `:8082` | `internal/api/nodes.go` `nodeHealth` |
| Blocking toggle replicated | `cmd/kudurru/serving_blocking_commands.go`, `serving_member_forward.go`, `kube_blocking.go`, `internal/store/kube/blocking.go`; `internal/api/static.go` (`blockingScope`) |
| Static twins read-only | `internal/api/static.go` (`staticObjectMutation`, `staticWriteMW`) |
| Member commands a follower forwards | `cmd/kudurru/serving_member_command.go` (operation switch), `serving_member_objects.go`, `serving_member_zones.go`, `serving_member_login.go`, `serving_member_forward.go` |
| Production paths use `NewMemberClient` | `internal/cluster/replica/enrolled_source.go`, `enrolled_forward.go`, `join_client.go`, `member_authority.go` |
| Root assembly of the member server | `cmd/kudurru/serving_member_listener.go` `prepareMemberServices` |
| Partition, promotion, rejoin, conflict acceptance | `test/e2e/enrolled_cluster_test.go`, `test/e2e/enrolled_conflict_test.go` |
| Follower login | `cmd/kudurru/serving_member_login.go` (`forwardLogin`, `rootMemberLoginDomain`), `serving_http_sessions.go` (`sealEnrollmentWithin`) |
| OIDC start/callback/CLI prepare need the local writer | `cmd/kudurru/serving_http_request_limit.go` |
| External session re-evaluation | `internal/servingauth/external_cookie.go` `externalSessionPrincipal` |
| Kubernetes grant floors | `docs/architecture/access.md` "Authorization" (D364) |
| Expression policies: captured provider compiles, legacy adapter refuses | `internal/auth/oidccore/cel.go`, `internal/auth/providerregistry/plan.go`; `ee/oidc/fromconfig.go` |
| Kubernetes TSIG management and reload | `cmd/kudurru/tsig_kube_management.go` (`kubeTSIGLive`), `tsig_kube_startup.go`, `doctor_notify_kube.go` |
| UPDATE proof only on a writable node | `cmd/kudurru/primary_proof.go` `capture`; `primary_proof_test.go` |
| Kubernetes NOTIFY from the elected loop | `cmd/kudurru/serve.go` (`add("notifier", true, ...)` when there is no complete runtime) |
| Zone types served and unsupported | `internal/zoneview/zoneview.go`, `internal/zone/zone.go` `NewBuilder`, `internal/controller/zone.go` `zoneVerdict` |
| Per-driver refusals | `cmd/kudurru/serving_totp_legacy.go`, `serving_users_legacy.go`; `internal/auth/stack/routes.go`; `internal/api/rolebindings.go`, `integrations.go`, `cluster_enrollment.go`, `cluster_promotion.go`; `cmd/kudurru/reload.go` `settingsWriter` |
| Unblock decisions flush the name | `internal/api/unblock.go` (`flushName`); `cmd/kudurru/serving_routes.go` |
| DNSEndpoint lease loss | `cmd/kudurru/externaldns_crd_failover_test.go` |
| Objects loader: every replica on Kubernetes, leader-only on SQLite | `cmd/kudurru/serve.go` (`{"objects", sql != nil, ...}`: the second field is `leaderOnly`; the `objects` health check reports a follower ready without loading) |
| Ten stages, of which the overview draws seven | `cmd/kudurru/serve.go` (the `stages` slice passed to `pipeline.New`: ingress, identify, control, postprocess, authoritative, blocking, rewrite, cache, dnssec, upstream); `docs/architecture/query-path.md` (first diagram) |
| A zonegroup or clientgroup binding grants nothing | `internal/rbac/rbac.go` (`Scope.Covers` needs the same kind and name); every `rbac.Scope{Kind: ...}` a route builds is `ScopeCluster` or `ScopeZone` |

Evidence under `.context/v020-final-20261110/evidence/DOCS-ARCH/`:

- `mermaid01-baseline.log`: the Mermaid blocks of `docs/architecture/*.md` at
  `33ee04bc`, parsed with the vendored Mermaid 11.17.2 bundle
  (`docs-site/vendor/mermaid`) in jsdom by `mermaid-check.cjs` (retained next
  to the logs): 104 blocks, 11 failures (retained failed run; three in
  `access.md`, six in `configuration-and-store.md`, one each in
  `policies-and-blocking.md` and `zones.md`).
- `mermaid02.log`, `mermaid03.log`, `mermaid04-head.log`: the same check after
  the edits; the last one on lane head `0d565018`: 108 blocks, 0 failures.
- `links01.log` to `links03-head.log` (`anchors.py`): 118 relative links of the
  architecture pages, every target file and heading anchor present, 0 broken.
- `docs-site01.log` and `docs-site02.log` (head `0d565018`): `make docs-site`
  with the pinned Hugo 0.167.0 and `--panicOnWarning`, exit 0, 163 pages;
  `docs-links01.log` and `docs-site02.log`: `make docs-check-links` (htmltest
  0.17.0), 160 documents, passed. Neither target validates Mermaid syntax,
  which is why the parse check above was added.
- `build01.log`: `gofmt -l` on the touched Go files is empty, `go build ./...`
  and `go vet` on the touched packages exit 0.
- `race01.log`: `go test -race -count=1` on the touched Go packages:
  `internal/zone` 3.5 s, `internal/notifier` 2.2 s,
  `internal/stages/authoritative` 1.5 s, `internal/config` 11.1 s, all ok;
  `api/v1alpha1`, `api/internalkinds/v1alpha1` and `internal/store` have no
  test files. No skips.
- `dashboard01.log`: vitest on `policyFields.test.tsx` and
  `GroupClientPages.test.tsx`, 2 files, 24 tests passed, 20.5 s.
  `dashboard02-negative.log` (retained failed run, intended): the updated
  `policyFields.test.tsx` against `MatchersField.tsx` from `33ee04bc` fails 1
  test, so the assertion follows the help text.
- `gen01.log` on the seven source commits up to `0d565018`: `make docs-gen`
  and `make generate` exit 0 and leave the tree unchanged (the one
  `chore(gen)` commit of this package comes from the review fixes below);
  `TestReferenceDrift`, `TestCLIReferenceDrift`, `TestRoutePermissionLint`,
  `TestLintRoutesCatchesMissingDeclaration`, `TestOpenAPIDrift`,
  `TestOpenAPIServedAndDocs` and `TestAggregatedRolesDrift` pass;
  golangci-lint v2.15.0 on the touched packages reports 0 issues; dashboard
  `tsc --noEmit` and eslint on the changed files exit 0.
- `drift01-before-regen.log`: `TestReferenceDrift` already passed before
  regeneration, because the configuration reference lists the nested fields of
  `auth.identityProviders` without their help text.
- `review-*.log`: the independent reviewer's runs on `0d565018` (verdict
  "accept-with-fixes", four minor findings). `review-render01-head.log` is a
  render of the built site in headless Chrome 156: 108 Mermaid blocks drawn as
  SVG, 0 error graphics.

Evidence for the review fixes (lane head `51878320`, same directory; the
scripts `fix-stale-check.py`, `fix-exsvg.py` and `fix-heavy.sh` and the label
lists `fix-edits-*.json` are retained next to the logs):

- `fix-stale01-before.log` (retained failed run, expected): `fix-stale-check.py`
  on `0d565018`, 25 checks, 19 failed. It reads the driver table, the loop
  diagram of the index, the labels of the three diagrams (embedded scene and
  drawn text), the stage list of `query-path.md` against the overview's stage
  boxes, and the zone-group strings in the source and generated files. All four
  findings reproduce. `fix-stale02-head.log`: the same check on `51878320`, 25
  checks, 0 failed.
- `fix-diagrams01-before.log` (retained failed run): `fix-exsvg.py check` on
  the eight hand-drawn diagrams at `0d565018`. It rebuilds, from every text
  element of the embedded scene, the SVG group that Excalidraw exports and
  requires exactly that group in the file. Seven diagrams matched byte for
  byte; `overview` had one label 0.23 px off its scene position (left by the
  hand edit of October 16).
- `fix-diagrams02-apply.log`: the relabeling. The first attempt was refused
  three times, by design: the widths stored in the scenes are those of a
  browser whose web fonts had not loaded, so the tool now proves its
  measurement against either width before it trusts it, and "H" and "T" are
  not in the font subset embedded in `policy-decision`, so the footnote was
  reworded without "DHCP" and "PTR". The second attempt applied six labels;
  each new label is measured in headless Chrome with the embedded font and
  fits its container.
- `fix-diagrams03-head.log`: `fix-exsvg.py check` on `51878320`: eight
  diagrams, 234 labels, all drawn exactly as their scenes say; `xmllint` exit
  0; Chrome renders the three edited diagrams. `fix-render-*-before.png` and
  `fix-render-*-after.png` are those renders. They were inspected by eye:
  every new label is in the hand-drawn font, inside its box, without overlap.
- `fix-diagrams04-negative.log` (retained failed runs, intended): a copy with
  one drawn label changed and its scene left alone fails the check, and a
  label that needs "H" and "T" is refused without changing the file.
- `fix-race01-before.log` (retained failed run, expected):
  `TestReservedBindingScopeHelp` against the unchanged `schema.go` fails on
  "help promises the scope for v0.2". `fix-race02.log`: after the fix the test
  passes, and `go test -p 2 -race -count=1` passes for `internal/config`
  (13.2 s) and `internal/rbac` (1.0 s); `api/v1alpha1` has no test files.
  `fix-race03-head.log` on `51878320`: `internal/store/validator` (7.0 s) and
  `internal/config/crdschema` (1.1 s), which read the regenerated CRD, pass
  under the race detector. No skips.
- `fix-build01.log`: `gofmt -l` on the touched Go files is empty,
  `go build ./...` and `go vet` on the touched packages exit 0.
- `fix-gen01.log` (one run under the machine-wide lock): `make generate` and
  `make docs-gen` change exactly four files (three copies of the RoleBinding
  CRD and `docs-site/content/reference/crds/rolebinding.md`), which form the
  `chore(gen)` commit. `TestReferenceDrift`, `TestCLIReferenceDrift`,
  `TestOpenAPIDrift`, `TestRoutePermissionLint`,
  `TestLintRoutesCatchesMissingDeclaration`, `TestOpenAPIServedAndDocs` and
  `TestAggregatedRolesDrift` pass; `go test ./deploy/` passes (15.8 s);
  golangci-lint v2.15.0 on `api/v1alpha1`, `internal/rbac` and
  `internal/config` reports 0 issues; `make docs-site` exits 0 with 163 pages;
  `make docs-check-links` passes 160 documents. `fix-race03-head.log` repeats
  the reference, OpenAPI and aggregated-role drift tests on the committed head
  with a clean tree.
- `fix-mermaid02-head.log`: 108 blocks, 0 failures with the vendored Mermaid
  11.17.2. `fix-links02-head.log`: 119 relative links, 0 broken (one more than
  before: the driver table links to "The leader").
- `fix-render01-head.log`: the reviewer's render script on the site built in
  `fix-gen01.log`, headless Chrome 156: 108 Mermaid blocks drawn as SVG, 0
  error graphics, and the built site serves the three relabeled diagrams
  unchanged.
- `fix-dashboard01.log` (a copy outside the worktree, two workers):
  `SettingsPage.test.tsx` and `AccessPage.test.tsx`, which read the settings
  schema mock, 2 files, 29 tests passed, 13.3 s; `tsc --noEmit` exits 0.

What remains open:

- This was not a sentence-by-sentence audit of all 9,200 lines of the tour. It
  covered the lines named in G21-16 and DOC-1 and every statement found by
  searching the pages for "in progress", "unfinished", "remain open/separate/
  pending", "not yet", "not wired", "planned" and version markers. `dhcp.md`,
  `licensing.md`, `observability.md`, `central-stopped-source-rollups.md` and
  the managed trust-anchor parts of `upstreams-and-cache.md`, `query-path.md`
  and `deployment.md` were searched but not rewritten.
- Statements owned by other lanes were left in place and need those lanes'
  updates when their work lands: `integrations.md` (Kubernetes execution,
  `Delete` cleanup, remaining providers, dashboard controls),
  `configuration-and-store.md` ("Settings, other IAM administration and
  remaining typed root domains are unfinished"), managed trust anchors, and
  query-log sinks. The new store-driver table in `README.md` states today's
  Kubernetes position for RPZ feeds, the integration host, TOTP, sessions,
  RoleBinding writes and user administration; each row must change with the
  package that changes it. `zones.md` now says that a native UPDATE is not
  forwarded from an enrolled follower (UPD-1/G21-04).
- Strings and comments that still promise v0.2 for features that have not
  landed were not changed, as directed: the `impersonate` mode help and errors
  (`internal/config/schema.go` "impersonate is planned for v0.2",
  `internal/config/auth.go`, `internal/auth/stack/stack.go`, IAM-1), the edge
  error "--backends-from cluster needs follower or raft membership (v0.2)"
  (`internal/cluster/source.go`), record views (`internal/zone/zone.go`,
  INT-6), `ExternalSpec` (INT-4) and the IXFR journal comments. They will be
  false in a v0.2.0 binary unless the owning package lands or rewords them.
  The zone-group strings are no longer in this list: they say "reserved" now.
  If IAM-2 lands zone groups, it must replace that wording in
  `internal/config/schema.go`, the dashboard mock schema,
  `api/v1alpha1/iam_types.go` and `internal/rbac/rbac.go`, and
  `TestReservedBindingScopeHelp` must then stop covering `zonegroup`.
- Hand-drawn diagrams. The overview still draws seven stage boxes; it now says
  so and names the three it leaves out, but it was not redrawn with ten.
  `diagrams/query-pipeline.excalidraw.svg` also shows seven stages; no page
  embeds it (`query-path.md` uses a Mermaid chain), so it was left alone. The
  four other diagrams (`deploy-docker`, `deploy-kubernetes`,
  `deploy-compose-ha`, `upstreams`) were read label by label for statements
  the pages contradict and none was found ("node always leader" on the
  single-container panel and "its own leader" on a static twin are true), but
  they were not checked against the code beyond that. No diagram shows an
  enrolled SQLite cluster, integrations or DHCP. The edited scenes were not
  opened in Excalidraw itself (it is not installed here and nothing was
  downloaded): they are verified by decoding, by the byte-exact scene-to-SVG
  check and by rendering the SVG in Chrome. The stored text boxes of most
  labels remain narrower than the drawn text, as before; the six edited labels
  store their drawn width.
- The loop diagram of the index lists "settings reload" under "every node".
  `serve.go` adds the `settings` loop only when there is no complete SQLite
  runtime; what reloads settings on the complete runtime was not traced in
  this package, so the entry was left as it is.
- Mermaid is rendered in a browser only by hand: the reviewer and the fix pass
  rendered the built site in headless Chrome 156 (`/usr/bin/google-chrome`;
  the earlier statement that no Chromium is installed was wrong). The parse
  check, the render script, the stale-statement check and the diagram check are
  scripts in the evidence directory, not repository tests or CI steps.
- The tracker table and root `README.md` parts of DOC-1 belong to the
  integrator and were not touched. `docs/enterprise.md` still says that
  "UniFi inventory integrations are planned"; that page is outside this
  package.
