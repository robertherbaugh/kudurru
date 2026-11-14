### DOCS-RECONCILE operator and migration docs true of the merged code — November 11, 2026

The operator, migration and guide pages now describe the code at the merged
head 68a709e2 instead of the defects that earlier lanes documented and later
lanes fixed. Every statement below was checked by running a binary built from
the lane head (`CGO_ENABLED=0 go build -trimpath ./cmd/kudurru`, plus a
`-tags=licensetest` build for the cluster and capability probes; `build01.log`)
or by reading the code path named with it. No Go code changed; no decision row
was needed.

Corrected statements and their proof:

- One `kudurru apply` (and one `kudurru restore`) applies every document (D378,
  `internal/api/object_command.go`, `internal/auth/providercoordinator/command_chain.go`).
  The migration pages, the cutover checklist and the `--select-object` advice no
  longer say "one document per run". Proof: `probe-single01.log` section
  "multi": a three-document file, `apply -f a -f b -f c` and a 60-document file
  each create everything in one run, the second run reports all `unchanged`, and
  one `kudurru restore` on a new node restores every record set. Commit 2c2f5627.
- A zone of type `external`, `catalog` or `secondaryCatalog` is refused at
  write on SQLite with `invalid` (`internal/zone/serving.go` `ServingErrors`)
  and the node keeps serving. The "stops the node across a restart" warnings
  are gone from `docs-site/content/migrate/*`, chapter 7 of the admin guide and
  `docs/architecture/zones.md`. Proof: `probe-single02.log` (11 of 11: each type
  refused, then API, readiness and the own zone sampled three times one second
  apart). Commits 2c2f5627, 3d063e8f.
- A BlockList URL source that never published a body is served empty and the
  list reports `Degraded`/`ListFetchFailed` (`notDownloaded` in
  `internal/listfetcher/prepare.go`, D373); it does not stop the node. The
  `curl -fsS` pre-check is removed, the failed-download cases are named, and the
  retry wording "minutes apart" is replaced by the real schedule: one minute,
  doubling up to one hour (`DefaultCooldown`/`DefaultMaxCooldown` and the
  `Cooldown << failures-1` backoff in `internal/listfetcher/fetcher.go`);
  `kudurru lists refresh` fetches at once. Because the node no longer goes into
  that state, the `prepare_failed` versus `preparing` sentence was removed
  rather than reworded. Proof: `probe-single05.log` (57 of 57): seven cases
  (refused connection, a server that never answers, a 200 HTML page, a 200
  comments-only body, a valid list on loopback with
  `blocking.allowLocalListURLs: false`, a redirect to a 404, a plain 404) each
  keep the zone, readiness and API for 12 s after the apply, show `0` entries
  in `kudurru lists list`, the download error under `Degraded`, and the list
  can be deleted; the recovery case blocks right after `kudurru lists refresh`.
  The converters put up to 256 URL sources into one BlockList
  (`internal/importers/importers.go`), so offline-config.md now says
  `--select-object` selects lists, not URLs. Commit 2c2f5627.
- `docs/deploy/dynamic-updates.md`: the incomplete "Check four things" CAUTION
  is replaced by the list of every update-policy shape the SQLite store refuses
  at write (`dynamicupdate.PreflightGrantSpecs` and `CompileGrantSpec` through
  `zone.ServingErrors`). Proof: `probe-single01.log` section "policy": twelve
  shapes refused (`allowOnlyPrivate`, a name without the final dot, a name
  outside the zone, `BOGUS`, `OPT`, `AXFR`, `*` inside a name, two `*` labels,
  empty and malformed `tsigKeyRef`, 4,097 grants, a bad grant under `deny`), the
  zone in service after each, and three controls accepted (a key the node does
  not have, lower-case type and `ANY`, 4,096 grants). The Kubernetes statement
  stays scoped to that store. Commit f300d190.
- The two cluster pages no longer contradict each other. `sqlite-cluster.md`
  now follows the merged follower table (`internal/api/follower.go`,
  `OperationFollowerClass`): sign-out and cluster settings are forwarded,
  refused changes and node settings answer `503 primary_required`, `kudurru
  login --username` against a follower answers `primary_required`, `cluster
  status` shows ROLE and LEADER, and repeating a cut apply through a follower
  does not reliably finish it; `sqlite-join.md` names the third per-item refusal
  "cluster authority changed" (`objectForwardError` in
  `cmd/kudurru/serving_member_objects.go`). Proof: `probe-cluster03.log` (24 of
  24): three documents through the follower in one run; 200 documents through
  the follower created 116 (84 refused with "the follower's source changed" or
  "cluster authority changed"), four repeats created 0 more, the same file on
  the primary applied the remaining 84 in one run, 200 on the primary in one run
  (38.7 s), and with the primary stopped an apply through the follower answers
  `completion_unknown` while the follower keeps answering DNS.
  `probe-cluster01.log` (12 of 12) covers the CLI sign-in refusal. Commit
  13bcfd67.
- Client ID versus CIDR: `IdentifyAt` (`internal/policy/index.go`) checks the
  catch-all `0.0.0.0/0` and `::/0` groups after the client-ID tier. The
  client-identity page, the gateway runbook (which told operators to remove the
  matching address matchers) and the AdGuard Home guide say so. Proof:
  `probe-single04.log` (10 of 10): over DoH, with a `0.0.0.0/0` group
  `/dns-query/phone` selects the client-ID group; with `127.0.0.0/8` it does
  not. Commits 9ac25f46, 2c2f5627.
- `docs/enterprise.md` no longer lists the Cloudflare and UniFi integrations as
  shipped, which contradicted `website/src/content.ts` ("remain on the
  roadmap"). The row returns to the external-dns webhook provider; a paragraph
  says the two stay on the roadmap and that this version runs a first, partial
  form only on a licensed SQLite node (`cmd/kudurru/serve.go` mounts no
  integration host for the Kubernetes store), linking to
  `docs/deploy/integrations.md`, which states the limits. `website/` was not
  edited. Commit 0ee09aea.
- New page `docs/deploy/capabilities.md` (G21-13): what works on standalone,
  static, enrolled primary and follower, fenced or conflicted, and Kubernetes
  nodes, and a glossary of the refusal keys with status, meaning and next step,
  each with an anchor; linked from the deploy index and `api-contracts.md`. The
  standalone and static columns were run against the licensetest build
  (`probe-shapes04.log`, two sections with 46 route rows, 4 of 4 checks). The
  follower column was not run as a matrix: it rests on the table in
  `internal/api/follower.go` plus the five follower checks in
  `probe-cluster03.log` (tokens, a cluster setting, a node setting, apply and
  CLI login). The Kubernetes column rests on
  `cmd/kudurru/serving_auth_legacy.go` and `internal/api`, not on a run.
  Commits eed1ec8a and dc3ff846 (review fix: a failed
  `kudurru login --username` on a static node leaves the password sign-in's
  session behind, because `cmd/kudurru/login.go` returns before `Logout` when
  the token mint fails, as `review-static-session02.log` shows with 2 sessions
  for admin; and the static RFC 2136 cell now says "for this node only").

Evidence under `.context/v020-final-20261110/evidence/DOCS-RECONCILE/`:
`build.sh`/`build01.log` (both binaries built at 64bb954f, exit 0);
`probe_single.py`, `probe_cluster.py`, `probe_shapes.py` (the harnesses);
`probe-single02.log` 11 pass, 14 s; `probe-single04.log` 10 pass, 8 s;
`probe-single05.log` 57 pass, 2 min 35 s; `probe-cluster01.log` 12 pass, 12 s;
`probe-cluster03.log` 24 pass, 2 min 9 s; `probe-shapes03.log` and
`probe-shapes04.log` 4 pass each, about 10 s. Docs gates at eed1ec8a:
`make docs-site` with Hugo 0.167.0 and `--panicOnWarning`, 167 pages, exit 0
(`site02.log`); `make docs-check-links` (htmltest 0.17.0), 164 documents
passed (`links01.log`); `python3 hack/guide-lint.py`, 67 files, 0 errors
(`guide-lint02.log`, `guide-lint03.log`); relative links and fragments of the
11 changed files under `docs/`, 131 links, 0 broken (`links-md01.log`,
`links-md02.log`). `site01.log` is empty: that run was cut off by the restart.

Retained failed runs, all harness faults, each followed by a passing rerun:
`probe-single01.log` (55 pass, 1 fail): the first readiness sample after the
refused `external` zone said `preparing`, right after the record set apply
before it; the same check with three samples one second apart passed in
`probe-single02.log`. `probe-single03.log` (52 pass, 2 fail): the policy file
of the recovery case had a YAML error, so the list was never referenced;
fixed in `probe-single05.log`. `probe-cluster02.log` (23 pass, 1 fail): the
check of the forwarded setting read the wrong key from `settings get`; fixed in
`probe-cluster03.log`. `probe-shapes01.log` and `probe-shapes02.log`: the
static node's configuration and its sign-in were wrong in the harness (the CLI
password login cannot mint its token on a static node, although browser
password sign-in works there; the probe now uses the bootstrap token).

Review fixes at dc3ff846: `fix-check01.log` (at eed1ec8a) shows the two page
claims the review found ("nothing was changed" and the plain static "Yes" for
RFC 2136); `fix-check02.log` (at dc3ff846) shows both gone, and
`fix-guide-lint01.log` and `fix-links01.log` rerun the guide lint and the
relative-link check on the changed page. The body of commit eed1ec8a says the
follower column was run; it was not, as stated above.

Open: the Kubernetes column of `capabilities.md` was not run against a
cluster, the OIDC rows of the static column were not run, and most follower
cells rest on the `internal/api/follower.go` table rather than on a run. The follower behavior the cluster page now documents is itself a
product defect for the HA lane: through a follower 84 of 200 new documents were
refused with "the follower's source changed", repeats created nothing more,
and a repeat reported fewer documents `unchanged` (89, 70, 60, 70) than had
been created (116), so already-created documents are reported as errors
instead of `unchanged`. The tracker ledgers, decision log and changelog are
not edited (integrator-owned).
