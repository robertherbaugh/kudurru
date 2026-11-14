### DOCS-VERSION guides and migration comparisons retargeted to v0.2.0 — November 10, 2026

Map entry G21-15 (and item 8 of G25-OPDOCS, which DOCS-OPS left for this
package). No decision number: no design decision was needed. Base commit
`5f10ed88` (lane branch `wip/v020/docs`, after DOCS-OPS). Seven source
commits: `66c45c13` (site and guide framing), `fe568c4e` (migration index,
cutover checklist, the two offline converter pages), `d9c62d1e` (Pi-hole,
AdGuard Home and Technitium guides), `71613d38` (administrator guide chapters
1, 2, 5 and 7), `56c99075` (deployment pages and the client identity page),
`2aa4868e` (architecture pages) and `319529fc` (Enterprise page, separate so
that it can be dropped). Three review-fix commits on top: `59c218fb`
(migration pages: the undownloadable list URL), `bf4d1af5` (restart effect
and the SQLite zone verdict in `zones.md` and chapter 7) and `a10afdf1`
(address matchers win over a client ID). No generated commit: the package
changes no generator input.

At the base, 40 lines outside the generated reference named `v0.1.0` and 3
more said "version 0.1". Now 12 lines name `v0.1.0`, and each is history that
is still true: DHCPv4 "included since v0.1.0" (8 lines), "v0.1.0 is the first
release" with its release page (2 lines), and the kustomize note about the
release candidates (2 lines). No line says "version 0.1". The framing sentences of `docs/guide/README.md` and
`docs-site/content/_index.md` name v0.2.0 ("clay copies"), the release name of
the README's roadmap.

Each rewritten statement was checked against the code, and the statements
about SQLite were also checked against real `kudurru serve` processes on
loopback (binary built from the lane head; the Go code of the head is that of
`5f10ed88`). What the pages now say:

- **Migration index.** The kudurru column of the comparison is v0.2.0: four
  served zone types, RFC 2136 updates, opt-in validation of forwarded answers,
  adblock modifiers, per-policy rewrites, authenticator codes on SQLite, the
  enrolled SQLite cluster. "Follower and Raft stores are deferred" is replaced
  by the three ways to run several nodes. The review date of the kudurru
  column is its own; the other three columns keep theirs, because they were
  not reviewed again. Store-dependent features are named with a link to the
  table of the architecture overview instead of a copy of it.
- **Technitium.** Secondary and stub zones and dynamic updates are no longer
  "Deferred"; the table says what kudurru serves and that the importer still
  skips them, and a new section 3 shows a secondary and a stub zone (the later
  sections are renumbered; nothing linked to their anchors).
- **AdGuard Home.** The seven DNS modifiers are applied instead of "counted as
  unsupported and skipped"; `$badfilter` and `@@` act inside their own
  block-list object; an unknown modifier or value counts as invalid.
  ClientIDs can move with DoT, DoQ and DoH clients, unless an IP, MAC, CIDR,
  hostname or name-glob matcher matches the arriving address (the first
  matching tier wins; `IdentifyAt` in `internal/policy/index.go`). The client
  identity page and the shared-address runbook say the same.
- **Pi-hole.** "No local validation" is corrected (it was already wrong for
  recursive groups), the blocking pause is node-local only on static twins,
  and MAC and hostname clients have a stated condition.
- **Cutover checklist.** Client matchers by source of identity; secondary
  zones and updates available; a new "Apply results" subsection (below).
- **Administrator guide.** Chapter 5: the client ID and MAC matchers are no
  longer "Reserved", with what each needs. Chapter 7: Default, Validate and Do
  not validate of a forward zone, temporary exceptions instead of "no list of
  exceptions", four zone types instead of "three", and a caution about
  schema-only zone types. Chapters 1 and 2 lose "In version 0.1" and "v0.1.0".
- **Deployment pages.** `client-identity.md` gains a table of where each
  matcher gets its identity (the guides link to it for the question of which
  installation has which source); `runbooks.md` no longer speaks of "the
  version 0.1 listeners"; `kustomize.md` is corrected (see below).
- **Enterprise page.** The integrations row names the background
  integrations on a SQLite node instead of calling them planned.

Four things the checks found that were wrong before this package and are not
version sentences:

- `docs/deploy/kustomize.md` said that releases up to v0.1.0 keep the
  kustomize tree under `config/`. The v0.1.0 tag has `deploy/kustomize/`;
  only the release candidates up to `v0.1.0-rc.6` have `config/` (`git
  ls-tree` on all eight tags).
- All three product guides said that the query for an allowed name "must show
  an allow-list decision". `kudurru query` prints `status: forwarded` or
  `cached` and names no list. The guides now say what it prints.
- `docs/architecture/zones.md` said that `external`, `catalog` and
  `secondaryCatalog` zones "are accepted but get `Ready=False, reason
  Unsupported`". The controller writes that verdict on both stores, but on
  SQLite the API cannot show it because no generation is published (see the
  defect below). The first version of this package said the SQLite runtime
  had no such verdict; the review found the verdict in `state.db`
  (`review-probe01.log`), and the page now says so.
- `docs-site/content/migrate/pihole.md` said "DNSSEC validation: No local
  validation", which contradicted the index page.

Three product defects that a migration meets are described in the pages,
marked "in this version", because the pages cannot remove them:

1. **A zone of an unserved type stops a SQLite node.** `kudurru apply` of a
   `DNSZone` with `type: catalog`, `secondaryCatalog` or `external` is
   accepted ("created"). `servingzones.Prepare` then refuses every candidate
   while the object exists: readiness reports `serving(complete serving
   generation unavailable: prepare_failed)`, every API request answers
   `503 generation_pending`, also the read and the deletion of that object,
   the node's own zones get no answer (timeout; a forwarded public name still
   answered), and the node does not become ready after a restart. After the
   restart it answers SERVFAIL for every name, forwarded names included
   (`review-probe05.log`). Putting back a copy of the data directory made
   before the apply recovers it. New
   finding of this package; not in the tracker. The migration index, the
   Technitium table, chapter 7 and `zones.md` now say not to apply these
   types.
2. **One run of `kudurru apply` applies one new document on SQLite**
   (found by DOCS-OPS, confirmed here on the guides' own files): the
   three-document examples of the migration guides need three runs, the
   two-document examples two. The cutover checklist has "Apply results", and
   each guide points to it at its first example.
3. **A list URL that has never been downloaded stops a SQLite node** (found
   by the review of this package). An enabled `BlockList` URL source without
   a published digest and body makes list preparation return `ErrIncomplete`
   (`internal/listfetcher/prepare.go`), which refuses the whole serving
   candidate, even when no policy refers to the list. The effect is that of
   defect 1: API 503 `generation_pending` for every request including the
   deletion of the list, own zones unanswered, `prepare_failed`, SERVFAIL for
   every name after a restart. Unlike defect 1 it clears by itself at the
   first successful download attempt (the fetcher backs off: 1, 2, 4 minutes;
   `review-probe09.log`). A source with `enabled: false` is harmless. The
   Pi-hole and AdGuard first examples reach it on a host without outbound
   HTTPS (`review-probe07.log`), and so does `kudurru import adguard --apply
   --wait` with the repository's fixture (`review-probe02.log`,
   `review-probe03.log`). It has no owner under `out/` yet; the reviewer
   considers it release-blocking for the product. The fixing change should
   either compile the candidate without a never-downloaded source or refuse
   the write, and let the API delete an object while no generation is
   current. Until then the migration overview, "Apply results" in the cutover
   checklist, the offline import page, the three product guides, chapter 7
   and `zones.md` warn about it: download each URL from the kudurru host
   first, apply URL lists one at a time, keep a copy of the data directory,
   do not restart. The "refused once ... run it again" sentence of the
   checklist is replaced by one that says when repeating does not help.

Evidence under `.context/v020-final-20261110/evidence/DOCS-VERSION/`:

- Review fixes, binary built at `a10afdf1` (`fix-build02.log`, a light run
  with `go build -p 2`; `fix-build01.log` waited about 20 minutes on the
  heavy lock and was withdrawn; the header line "lock held" of both comes
  from the script text). `verify.py` gained a section `unfetchable` (a node of
  its own: a `BlockList` with the URL `https://lists.example.test/deny.txt`
  and no policy; API read, repeated apply and delete refused with
  `generation_pending`, own zone unanswered, forwarded name still answered,
  `prepare_failed`, after a restart not ready in 30 s and SERVFAIL for the own
  and a forwarded name, recovery by putting back the copy of the data
  directory, the list gone with it); a check of tier precedence in `identity`
  (a CIDR group on 127.0.0.0/8 makes `/dns-query/phone` select the CIDR
  group's policy and not the client-ID group's, and removing it restores the
  client ID); and the two zone-transfer checks now retry for up to 20 s,
  because a transfer that starts while the node publishes a change can be cut
  off (`review-verify02.log`: 133 of 135, both transfer checks with `SOA
  lines=0`; `review-probe08.log`: 11 to 17 of 27 to 41 AXFR attempts in the
  3 s after an unrelated apply ended with `communications error ... end of
  file`). That cut-off transfer is a SQLite node behavior worth a follow-up;
  the docs do not promise otherwise. `fix-verify01.log` (sections zones,
  identity, unfetchable): 53 pass, 0 fail. Four full runs of `verify.py` at `a10afdf1`
  (each about 3 to 3.5 minutes): `fix-verify03.log`, `fix-verify04.log` and
  `fix-verify05.log` 159 of 159 pass. `fix-verify02.log` is a retained
  failure, 157 of 159: `spec.dnssec does not sign` saw no answer for the new
  signed zone within the 15 s wait on the busy node (now 30 s; the check
  still requires NOERROR and no RRSIG), and on node-x the own zone answered
  SERVFAIL instead of timing out after the catalog zone (the pages say
  "stops answering", which both are; the check now accepts either and still
  requires that the record is not answered). Runs 04 and 05 are after those
  two adjustments; run 03 had only the first set of changes. The earlier
  single-run count "135 checks pass" of `run11-all.log` is superseded by
  these counts.
- `fix-gates-light02.log` at head `a10afdf1`: guide checker 67 files, 0
  errors; `make version-check` PASS; `TestReferenceDrift` ok; `make
  docs-site` 165 pages; `make docs-check-links` passed; the acceptance grep
  and the stale-marker search unchanged. `fix-gates-light01.log` is a
  retained failed run: chapter 7's caution had 8 sentences (limit 6) and was
  split into two paragraphs.

- `run11-all.log` (1 min 57 s): 135 checks pass, 0 fail, every section of
  `verify.py`: importers present (adguard, bind, blocky, pihole, technitium;
  no coredns); secondary zone transfers and answers on a second node, its
  records are read-only; stub zone answers through the discovered authority;
  `spec.dnssec.enabled` signs nothing; AXFR out, and IXFR for an older serial
  in AXFR form; RFC 2136 with a stored TSIG key (signed accepted, unsigned
  `REFUSED`); 18 checks of modifier rules (`$dnstype`, `$dnsrewrite`,
  `$important`, `$badfilter`, `$denyallow`, `$client`, unknown modifier,
  unknown `$dnstype`, hosts rewrite, `@@` and `$badfilter` across two list
  objects; list status `unsupported: 1, invalid: 3`); `Policy.spec.rewrites`
  for an exact name and `*.suffix`, and not for a name in an own zone; client
  ID on DoH path and DoT server name selects a group and its policy, other
  IDs and plain DNS do not; `dnssec.enabled` off by default, AD only after it
  is on, a negative trust anchor removes AD for its domain, an expiry nine
  days away is refused, a forward zone's `dnssecValidation` wins in both
  directions; an RPZ feed transferred from another node answers its local
  data; blocking state `scope: cluster` on a SQLite node and `scope: node` on
  a static twin; a catalog zone on a node of its own (accepted, then 503 for
  read and delete, no answer for the own zone, not ready); and the manifests
  and commands of the three product guides, read from the Markdown files and
  run as written on one node for each guide (35 checks).
- `run12-examples-head.log` (1 min 9 s): the guide examples and the catalog
  check again with the worktree at the committed head `319529fc`, 49 pass. On
  new nodes the three-document files of the guides needed 3 runs of `kudurru
  apply` each, the two-document files 2 runs and the one-document files 1.
- `probe-catalog01.log`, `probe-external01.log`,
  `probe-secondaryCatalog01.log`: one SQLite node after each unserved type
  (state at 1, 3, 6, 12 and 20 s, unrelated writes, readiness, delete,
  restart). `probe-restore01.log`: 13 checks pass, recovery by putting back
  the data directory.
- `run-offline01.log`: `kudurru import blocky` and `import adguard` produce a
  plan with the plain binary and no server.
- `gates-light02.log` at head `319529fc`: `hack/guide-lint.py` 67 files, 0
  errors; `make version-check` PASS; `TestReferenceDrift` ok (0.045 s);
  `make docs-site` 165 pages with `--panicOnWarning`; `make
  docs-check-links` 162 documents passed; the acceptance grep and the search
  for stale markers in the nine files of the map entry (empty).
  `gates-light01.log` is the same before the commits.
- `build02.log`: the binaries (plain and `-tags=licensetest`), built as a
  light run with `go build -p 2`. `build01.log`: the same build waited 15
  minutes on the heavy lock behind three lanes and was withdrawn.
  `head-check01.log`: no path other than Markdown changed since that build.
- `gates-heavy01.log`: `TestCLIReferenceDrift` at head `319529fc` through
  the heavy lock, ok (0.027 s), worktree clean before and after. No file of
  `cmd/kudurru` changed in this package.
- Retained failed runs and what they showed. `run01-multi.log` (6 pass, 1
  fail): the check expected one run of `kudurru apply` for the Pi-hole
  guide's three documents; it took three. `run02-zones.log` (16 pass, 9
  fail): the catalog zone was applied on the shared node and every later
  check failed with `generation_pending`, which is how the first defect was
  found. `run03-zones.log` (22 pass, 1 fail): `kudurru query` on a new
  secondary answered `servfail_unsynced` once directly after its first
  transfer, while real DNS answered; the check now waits. `run06-rpz-blocking.log`
  (10 pass, 1 fail): the JSON output of `kudurru blocking disable` has no
  `scope`; the API's blocking state has, and the Pi-hole row says so.
  `run07-examples.log` (36 pass, 2 fail): `kudurru records list` and `zones
  export` directly after `kudurru import bind` answered `unavailable:
  committed zone read: serving: generation pending`; the checklist and the
  Technitium guide now say to run the command again. `run09-all.log` (132
  pass, 2 fail) and `run10-all.log` (134 pass, 1 fail): mistakes of the
  script (an object name used by two sections; a node without an upstream).

Not verified by a run: every Kubernetes statement (no cluster here; they rest
on the code, on the shared stage chain of `serve.go` and on the envtest tests
that DOCS-OPS cites); the enrolled cluster, static twins beyond the blocking
scope, and authenticator codes (DOCS-OPS ran them: `run-cluster*.log`,
`run-totp01.log`); client IDs on DoQ and DoH3 (code only; DoT and DoH were
run); `$ctag` and the name form of `$client` (code only); MAC matching with a
real inventory source; a provider run of an integration; recovery by putting
back the data directory on anything other than a standalone node (the
`unfetchable` section and `probe-restore01.log` are standalone nodes); a
list URL that cannot be downloaded on an enrolled cluster or on Kubernetes.

What remains open: the three defects above are product work, and the pages
carry "in this version" sentences that the fixing change has to lift
(`docs-site/content/migrate/_index.md`, `cutover.md` "Apply results" and the
three pointers to it, `offline-config.md`, the URL sentences of the Pi-hole
and AdGuard examples, `docs/guide/admin/07-zones-and-upstreams.md`,
`docs/architecture/zones.md`). If `CORE-APPLY-MULTI` lands, lift the
one-document paragraph of "Apply results", line 35 of the changelog draft
and the DOCS-OPS paragraph in `docs/deploy/sqlite-cluster.md`. The
Enterprise commit `319529fc` calls the integrations shipped while
`website/src/content.ts` still calls them planned; pick both together or
drop `319529fc`. The same one-document behavior affects the
`kudurru apply` examples outside the migration pages, which this package did
not change. Sentences that name v0.2 as unfinished work stay as they were
(`docs/deploy/response-processing.md:112`, `docs/deploy/iam-upgrade.md:106`),
as do the passages that other lanes own (managed trust anchors in
`dnssec-validation.md`, `integrations.md`, the follower section of
`sqlite-join.md`). The first paragraph of `docs/deploy/client-identity.md`
still says "its management UI and Kubernetes execution remain separate
work"; the new table below it defers to `integrations.md`. The root
`README.md` still describes v0.1.0 in lines 10 to 24 and 44 to 58; it is
integrator-owned.
