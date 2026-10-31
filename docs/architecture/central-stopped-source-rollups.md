# Stopped-source rollups and the provenance transition

Schema 5 implements source-owned central rollups and bounded recovery of stopped
sources. Ambiguous schema-4 history remains explicitly gated. D256 is reserved
for the integrating parent's design record; this page describes the implemented
contract and its remaining historical-input limitation.

## Ownership and shipping

Every minute, hour and day rollup family has a nullable source foreign key.
The unique key is `(ts, node[, dimension], source)`, with NULLs not distinct.
A source is the registered query-log file UUID, independent of its mutable node
name. Two files using the same node name contribute independently. Existing
schema-4 values retain NULL ownership, their dictionary IDs and their values;
no source is assigned to them.

Shipping locks the source row, rechecks its stream cursor, then captures all
families of that resolution under the local writer lock. Local compaction,
privacy changes, writes and retention cannot split that capture across versions.
The transaction deletes and replaces only that source's contribution in the
covered range. An empty authoritative export removes synthesized keys. Stale
cursor inputs fail before replacement; the cursor cannot regress. New streams
`source_rollup_1m`, `source_rollup_1h` and `source_rollup_1d` keep upgrade coverage
distinct from old node-only shipping cursors.

The authoritative bucket records the captured local fact head, including facts
that have not yet arrived in an earlier shipping batch. Subsequent fact inserts
increment the source/hour input revision. Facts beyond the captured head
invalidate affected hour/day buckets in the same transaction. Repeated minute
exports change revisions only when their base values change. Fact-free minute
changes also invalidate existing source summaries. A late official hour/day
replaces the synthesized contribution rather than adding a second copy.

## Bounded background work and retention

The leader runs `querylog-stopped-source-rollups` before retention and dictionary
pruning. A source is eligible at exactly two hours of inactivity. Work selects
closed UTC buckets only, rechecks under source row locks, skips locked sources
and has a five-second context/statement bound and 100 ms lock timeout.

A pass selects at most four sources and four hours plus four pending days per
source. Per-source hour/day cursors rotate through failed and successful work;
a durable source keyset cursor captures a fixed upper source boundary. A failed
early bucket cannot indefinitely starve later hours or another source. An hour
loads at most 10,001 facts to enforce a 10,000-row input limit. Oversized,
incomplete or failed work records a fixed operational error and keeps its input.
Transaction rollback commits neither summaries nor progress.

Hourly synthesis reuses the local accumulator's status, protocol, NULL latency
and histogram definitions for all seven families: base, client, qtype, upstream,
domain, list and zone. Daily synthesis sums source-owned hours into the existing
three daily families: base, client and domain. It includes the final partial day
when that UTC day closes, even if no new hour arrives. Input revisions invalidate
synthetic daily coverage when their underlying hours change.

With shipped facts disabled or privacy suppressing facts, only the four shipped
minute families can be synthesized. The job preserves any already known domain,
list and zone contribution and reports `facts_unavailable`; it does not fabricate
missing dimensions. Observed export gaps, skipped old facts, privacy transitions
and explicit purges conservatively mark source input incomplete. A late official
shipment can still supply authoritative coverage.

Raw retention retains partitions needed by pending, failed or legacy-gated
hours. Expiration records which source/hour raw and minute inputs are no longer
complete. When late facts arrive after raw expiration, an intact source-owned
hourly baseline can accept only IDs above its captured head, atomically updating
all retained dimensions and the daily projection. Replaying that tail does not
add it twice. A partition cached by a still-running shipper is checked again,
so it can recreate a partition removed by retention. If the hourly baseline
itself has expired, the remaining tail cannot reconstruct all seven dimensions;
`source_input_incomplete` keeps the old daily values and surviving input intact.
Fact-free recovery likewise refuses to replace a baseline from incomplete minute
input after minute retention. Minute/hour retention waits for corresponding durable daily coverage,
including its current input revision. Opaque legacy cells are retained. Daily
retention remains indefinite. All dictionary references remain in the original
relations with their real foreign keys, so F70's complete reference enumeration
continues to protect both opaque and source-owned contributions.

## Read and operational contracts

Central reads retain source coverage after inactivity, including retired files
and older files sharing a current node's name. A healthy new file cannot hide
an unresolved older one. The existing global tier boundary uses conservative
source coverage; a stopped source with no pending input does not pin that
boundary permanently to its last shipping time. Every read remains a read-only
repeatable-read snapshot. F69's exact node predicates and central ID namespace
remain in effect; central failure never falls back to a peer or local database.

`GET /querylog/status` includes bounded `backfill` metadata: capped legacy-cell,
pending-hour and pending-day counts, up to 25 source/range details, and explicit
capped/truncated flags. Node selection binds details to retained references in
that node's scope. Historical responses remain partial while coverage or
provenance is unresolved. An opaque cell remains visible as
`legacy_provenance_required` even if its source registration is absent.

`GET /querylog/rollup-evidence?source=<uuid>&hour=<unix-seconds>` captures one
closed UTC hour; the normal `scope`/`node` parameters select central scope.
`POST /querylog/rollup-evidence/verify` accepts that receipt and checks it against
a fresh read-only central snapshot. Both routes require `querylog.read`, use the
existing long-term license window, return `no-store`, and have no local/peer
fallback. Evidence contains operational IDs/digests, not raw DNS values. The
response carries central scope metadata and a distinct `source_id` for the file.
Verification does not claim a later apply will succeed.

The source-locked repair backend implements exact-once replacement of known
source-owned inputs. Its callback must check current authority before work and
immediately before commit, including duplicate receipts. A committed digest
returns `already_applied`; new input revisions refuse the old receipt. A failed
final check rolls back both the contribution and receipt. The API exposes capture
and verification only; automatic leader reconciliation performs known-provenance
repairs. There is no public mutation endpoint that assigns legacy ownership.

## Independently recomputable receipt

Version 1 binds the source UUID, exact selected node (empty for cluster), hour,
input revision/head, legacy boundary, fact availability/partial flag and expiry.
The receipt expires in five minutes. The digest is SHA-256 over Go's JSON encoding
of that ordered header (`Version`, `Source`, `Node`, `Hour`, `Revision`, `Head`,
`Legacy`, `Facts`, `Partial`, `ExpiresAt`, `Complete`, `MinuteComplete`), followed by PostgreSQL `to_jsonb(row)::text`
for retained facts ordered by `(source_id,id,ts)`, each terminated by a NUL byte.
For each family in `sqlitelog.RollupTables` order, append its table name and a NUL,
then the source's rows ordered by `(ts,node[, dimension])`, each NUL-terminated.
Minute/hour ranges cover the selected hour; daily ranges cover its UTC day.
The evidence read caps each relation at 10,000 rows and the combined row/header
bytes at 4 MiB. The source/hour/node and input are recomputed under the same
source lock before replacement. This digest is a checkable input fingerprint,
not an authorization token or a proof of opaque historical ownership.

## The exact remaining legacy gate

Schema 4 discarded source ownership in rollups. After raw expiration, the same
retained source/cursor/node cells can describe two different histories. A node
cell of five may belong to A or to B, even when both registered files use the
same node name and have the same cursors. An A replacement of seven requires
seven in one history and twelve in the other. Current names, cursor order and
retained counts cannot distinguish them. A final missing hour also does not
attribute earlier hours needed for a daily replacement.

Migration therefore records a conservative global legacy boundary covering all
retained legacy buckets and at least the end of the migration's UTC day. Every
existing or subsequently registered source inherits it. Contributions at or
after that boundary can use source ownership; earlier authoritative/synthetic
replacement is gated. Original opaque values, facts and contributions are
preserved, and status/read/evidence show `legacy_provenance_required`. New
shipping coverage must not be read as proof that a skipped legacy prefix was
attributed. Raw purge refuses a gated range; no data-loss disposition exists.

The missing input is independently reconstructable history establishing all
owners of overlapping legacy cells, including daily dimensions and latency
histograms. A digest of the surviving input cannot establish that absent
history. This implementation deliberately has no adoption/import operation that
can turn a guessed attribution into an accepted receipt. Reconstructing and
validating unavailable legacy ownership remains an explicit supported-upgrade
limitation; source-owned/new-schema behavior and safe post-boundary operation
are implemented independently.

Schema/compatibility version 5 refuses older binaries on reconnect. Removing
the old unique conflict targets also makes already-connected schema-4
replace-upsert writers fail atomically. Upgrade all central writers and plan
for migration locks, indexes, source/revision backfill and extra retained-input
storage. Do not lower compatibility markers or drop legacy rows to bypass a gate.
