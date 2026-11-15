### DASH-RPZ Dashboard support for RPZ feeds — November 13, 2026 (D391)

The dashboard now shows the RPZ feed state that D391 exposes. A block list
with an RPZ source has an "RPZ feeds on this node" section on its page, read
from `GET /blocklists/{namespace}/{name}/rpz`. The read repeats every 30
seconds while every feed serves, every 3 seconds while it answers 404 or 503
or a feed is not serving, and at once after any list or source write on the
page (every RPZ read, this list's and the Lists page's, is invalidated with the
list). A new list's feed therefore appears within seconds of the node's next
configuration snapshot, and a pending or expired feed is followed closely. For
each feed it shows the state the answering node reports (`pending`, `serving`,
`expired`, `unavailable`; the API word for a current copy is `serving`). Every
state but `serving`, `pending` included, gets an alert that says plainly that
this node answers SERVFAIL to every client whose policy uses the list. The
alert also carries the reason and message of the node's diagnosis and the next
transfer attempt. A serving copy whose refresh failed gets a warning with the
node's message. The section also shows the node, serial ("Last serial" once
the copy is not serving), primary, last confirmation, expiry, next attempt,
last error, configuration error, the compiler totals, the compiled rules by
trigger (QNAME, CLIENT-IP, RESPONSE-IP) and action (NXDOMAIN, NODATA,
PASSTHRU, DROP, TCP-only, local data), and a table of the ignored reasons with
their kind (unsupported or malformed), count and example owners. The dashboard
types treat every feed field as optional and render only what the node sends.
A copy the manager discarded (RPZ-OPEN) shows "no copy" instead of values the
node did not send. 501 (no feed manager), 404 (the node's configuration lacks
the list) and 503 (no configuration yet) are explained in an info banner. The
Lists page reads `GET /blocklists/_rpz` only while some block list has an
enabled feed. It shows the worst feed state of each such list under its
health: plain text while serving, a badge otherwise, and the node's message in
the tooltip.

RPZ sources used to be treated as inline sources: the sources table showed
"0 inline entries" and offered an entries browser that the API refuses with 422,
"Last refresh" said "inline, nothing to fetch", and an unnamed source was
called `inline-<i>` instead of the server's `rpz-<i>`. They now have their own
kind. The table shows the zone, primaries, TLS and TSIG key name, shows "per
node" for entries and fetch time, and offers no entries browser. A
`?source=` deep link to a feed explains that a feed has no entries. Feeds can
be created with **Add source → RPZ feed** (offered for block lists only) and
with **Add an RPZ feed** in the new-list wizard. Both take the policy zone,
ordered primaries (1 to 32), TCP or TLS and an optional TSIG key name, and
check them as the CRD does. The writes go through the existing `useCan` path,
so static mode and missing `blocklists.write` hide them, and on an enrolled
follower they are forwarded to the primary like every other list change. The
feed section has no actions and on a follower shows the follower's own copy.
The mock API now enforces the CRD source rules (exactly one kind, `rpz` exactly
with format `rpz`, block mode, no subdomains) and serves `db.rpzFeeds`.
`docs/deploy/rpz.md` and `docs/architecture/block-lists.md` (text and diagram)
describe the dashboard views.

The list header keeps the health badge of the shared BlockList status, but
when this node's feed answers SERVFAIL an "RPZ <state> on this node" badge
stands beside it and the Status line adds "RPZ feed <source> is <state> on node
<node>: clients whose policy uses this list get SERVFAIL from it." A list whose
fetched sources are all RPZ feeds shows "per node (see RPZ feeds)" for Last
refresh instead of a permanent "not yet", "not used: RPZ feeds follow their
zone's SOA timers" instead of the 24h default refresh interval, and "per node"
in the Lists page's Updated column.

Evidence under `.context/v020-final-20261110/evidence/DASH-RPZ/`:

- `vitest01.log` (retained failure): the first run of the new
  `src/components/lists/RPZFeeds.test.tsx` had 15 PASS and 3 FAIL. The mock API
  refused an RPZ source ("one of url, inline or configMapRef is required",
  fixed by mirroring the CRD rules). The allow-list test found no API-owned
  allow list in the fixture (the test now creates one). The wizard derived the
  source and list name `rpz.vendor.example`, which has dots (now
  `rpz-vendor-example`).
- `vitest02.log`: `RPZFeeds.test.tsx` 18 PASS. These tests cover serving with
  every field, pending with the SERVFAIL alert and next attempt, expired with
  the last serial, counts and last error, a discarded copy with only
  identity, state and message, a configuration failure next to a serving copy
  with a failed refresh, 501 and 404, the `?source=` feed link, the source
  dialog (validation, then the exact stored `rpz` block with TLS and TSIG),
  no RPZ choice on an allow list, static mode (no Add source or Edit, a
  disabled switch, the section still shown), a follower (its own node and
  serial, Add source still offered), the Lists page (worst state, "RPZ
  unknown" on 501, no `_rpz` read without a feed list), the wizard creating
  an RPZ-only list, and the helpers (state words, worst feed, CRD checks).
- `vitest04-lists.log` (retained): the list, wizard and lib files run in
  parallel at a load average of 33 to 43 (other lanes) gave 6 FAIL. All were
  20 s test timeouts or `findBy` timeouts, two of them in tests that existed
  before this change. `vitest05-lists-serial.log`: the same four list files with
  `--no-file-parallelism`, 53 PASS (173 s).
- `vitest03-full.log`: full dashboard suite `bunx vitest run` through the
  heavy lock, 76 files, 1031 PASS (242 s).
- `lint01.log`: `bun run lint` (eslint `--max-warnings 0` and the theme check)
  and `bun run typecheck`, clean.
- `e2e01.log` (retained failure): `bun run build`, a `-tags=licensetest`
  binary and `e2e/rpz.spec.ts` against a real node. The flow worked up to the
  serving page; the test's `/Serial\s*7\b/` did not match the rendered
  "Serial7Primary" (no word boundary between "7" and "P"). The assertion was
  corrected to `(?!\d)`. The application was not changed.
- `e2e02.log`: `e2e/rpz.spec.ts`, `e2e/lists.spec.ts` and
  `e2e/secondary.spec.ts`, 5 PASS (38.3 s; the RPZ spec 18.2 s). The RPZ
  spec uses a stub TCP primary (`e2e/support/rpz.ts`: SOA, AXFR and IXFR as a
  full transfer, refresh and retry 1 s, expire 8 s). It creates the list in
  the wizard and applies it to its own client with its own policy. It sees
  the feed serving (serial 7, primary, QNAME 1, the NSDNAME owner in the
  ignored table) while `/dns/query` reports the name blocked. After the
  primary fails, the copy expires: the page shows "Expired: clients get
  SERVFAIL" with `RPZExpired` and "Last serial 7", `/dns/query` answers
  SERVFAIL, and the Lists page row shows "RPZ expired".
- `bench01.log`: bench guards, 14 OK.

Review fixes (after the review logs `review-*.log`):

- `fix-vitest01.log`: `RPZFeeds.test.tsx` 25 PASS (35 s). New: the 503 banner
  ("This node has no configuration yet"), a 404 first answer followed within
  the settling poll by a serving feed without a reload, a second feed read
  right after the source switch is toggled, the header badge "RPZ expired on
  this node" with the Status sentence, "per node (see RPZ feeds)" and the SOA
  wording for an RPZ-only list, no flag while serving, "per node" under
  Updated on the Lists page, and `rpzSettling` (fast on 404 and 503 and on a
  non-serving feed, not on 501 or all serving).
- `fix-vitest02-mutated.log` (retained, intended failure): the same file with
  the fix's behavior reverted in place (30 s poll, no RPZ invalidation, no
  header flag, old wording): 4 FAIL, 21 PASS. The failing tests are the
  settling reread, the reread after a source change, the header flag and
  wording, and the Lists page wording. The 503 test passes both ways: the
  branch existed and was only untested.
- `fix-lint01.log`: `bun run typecheck` and eslint `--max-warnings 0` on the
  touched source and e2e files, exit 0.
- `fix-e2e01.log`: rebuilt dashboard bundle and `-tags=licensetest` binary,
  then `e2e/rpz.spec.ts --repeat-each=2`, 2 PASS (42.0 s, 40.9 s). The stub
  primary now holds its answers until `serve()`, so the real node's first
  transfer stays in flight: the page shows the feed `pending` with "Pending:
  clients get SERVFAIL" and "Until its first complete transfer", the header
  badge "RPZ pending on this node" and "per node (see RPZ feeds)", and
  `/dns/query` answers SERVFAIL with no transfer served. After `serve()` the
  section turns `serving` and the name is blocked; after `fail()` it turns
  `expired` with the header badge and Status sentence. The page is never
  reloaded. The 404 console allowance stays, because the first read after
  the wizard can still come before the node's snapshot.
- `fix-vitest03-lists.log`: `src/components/lists`, `ListDetailPage.test.tsx`,
  `ListsPage.test.tsx` and `ListWizard.test.tsx` with `--no-file-parallelism`,
  4 files, 60 PASS (114 s); then `e2e/lists.spec.ts` and `e2e/rpz.spec.ts`,
  4 PASS (1.0 min).
- `fix-bench01.log`: bench guards, 14 OK.

What remains open: feed state is per node and the dashboard shows only the
answering node's. There is no cluster-wide view, because the API has no
fan-out read. The list header's health badge still comes from the shared
BlockList status; a failing feed on this node is flagged beside it, not
merged into it, and the Lists page's health column shows the feed state on
its own line as before. A feed that stays expired is reread every 3 seconds
for as long as the page is open. Editing `exclude` of an RPZ source
(switching single policy owners off) is not offered in the UI. An existing
exclusion is kept on edit. No Playwright run covers a follower, a
Kubernetes node, the `unavailable` state or static mode; those views use the
same routes and are covered by vitest against MSW. The test file names RPZ-OPEN's discarded-copy shape from
its ledger; if that package changes which fields it drops, the dashboard
still renders whatever arrives.
