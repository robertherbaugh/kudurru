# Stopped-source rollups and the provenance transition

This is the concrete F71 design, not a description of implemented behavior.
F69's node predicates do not fix stopped-source rollups. F71 remains open until
both the source-owned implementation and the legacy transition below pass
actual PostgreSQL acceptance. D256 is reserved for this design; the integrating
parent owns the global completion records.

## Storage contract

The unit of ownership is the query-log file's registered `source.id`, not its
mutable node name. Two files with the same node name are independent owners.
The source row already serializes shipping; use that lock rather than a global
ingestion lock. A bucket's authoritative and synthesized values are alternative
versions of that source's contribution, never two additive contributions.

A versioned migration introduces source-owned storage for every existing
rollup family, including the minute families needed to retain source identity
before hourly and daily compaction:

| Relation | Key and contents |
|---|---|
| `source_rollup_<family>` | `(source, ts, node[, dimension])`; all existing metric columns; real source/node/dictionary foreign keys |
| `source_rollup_bucket` | `(source, resolution, ts)`; authoritative/synthesized origin, captured fact revision, input boundary, explicit completeness/error state |
| `source_fact_hour` | `(source, hour)`; monotonically increasing revision/head and retained input bounds, updated atomically with fact insertion |
| `source_rollup_progress` | `(source, resolution)`; keyset scan position and fixed pass boundary, distinct from authoritative shipping cursors |
| `legacy_rollup_<family>` | Original node-only rows retained unchanged until ownership is established by evidence |
| `legacy_rollup_resolution` | Explicit unresolved/verified ownership state, original cell identity/digest, and independently verified reconstruction receipt |

Existing node-shaped rollup relations can become read projections, summing only
contributions whose ownership and overlap are resolved. A projection must not
sum an opaque legacy cell with a possibly overlapping source contribution.
Read-side tier selection also needs source coverage: a single active-node
watermark cannot certify an inactive source's unfinished bucket. Either plan
coverage per source before combining results, or maintain a transactional
node/bucket coverage projection that proves equivalent coverage. A new schema
column plus the existing global watermark is insufficient.

Raise `compat_version` to the new schema. Already connected old writers must
also fail: replacing writable legacy relation names with read-only projections
makes their old `INSERT ... ON CONFLICT` operations fail atomically. An additive
schema marker alone cannot stop an already connected replace-upsert writer.
Do not expose new dictionary references without foreign keys and F70 reference
enumeration/pruning coverage, including the retained legacy relations.

## Authoritative shipments and late returns

1. Lock/register the source, recheck the current stream cursor and input
   generation, and obtain a consistent export. Stale exports must be retried
   from current input; taking `max(cursor)` after writing stale values does not
   prevent an older export from replacing a newer contribution.
2. Replace that source's complete contribution set for the covered bucket,
   including removal of keys no longer present. An empty authoritative export
   is a real replacement of that source's synthesized values. Another file's
   contribution, even under the same node name, remains untouched.
3. Replace the bucket origin/revision and update the cursor/projection in the
   same transaction. Enforce cursor monotonicity under the source lock. Rollback
   changes neither contributions nor revision, progress, cursor or projection.
4. A same-source return replaces its synthesized hour/day. A new file registers
   a new source and adds a separate contribution. Neither path reassigns legacy
   ownership based on the current node name.

The fact shipper increments affected source/hour revisions in its source-locked
transaction, including late facts for an already synthesized hour. A read never
sees a newly committed revision with an old summary advertised as complete.
Reconciliation rereads that source's retained input and replaces its prior
synthetic contribution. Official late arrival takes precedence atomically.

## Bounded stopped-source work

The leader runs this job before fact/rollup retention and dictionary pruning.
Select query-log sources with `last_seen <= now - 2h`; an exactly two-hour-old
source is eligible. Capture a fixed upper source/bucket boundary for each pass,
use durable ordered keyset progress, and cap sources, buckets, rows and elapsed
time. Lock each selected source with a short lock timeout, then recheck
inactivity, authoritative cursors and fact revision. A returning source can
invalidate eligibility before the lock is acquired. Other sources keep shipping.

Only closed UTC hours/days are eligible. Capture the source/hour revision and
fact head under that source lock. Use the existing accumulator definitions for
all seven hourly families (base, client, qtype, upstream, domain, list and zone),
including every status category, NULL latency handling and histogram boundary.
Do not substitute a count-only summary. An oversized or timed-out bucket stays
pending with an explicit error; it does not advance the completed boundary.

Roll up source-owned hourly contributions into the three retained daily
families (base, client and qtype). Track the input revision vector/digest and
replace that source's day when any hour changes or an official day arrives.
Reconcile a stopped source's last partial day as well as its last partial hour.
Otherwise daily-tier selection or hourly retention loses the recovered data.
Daily retention remains indefinite.

Raw retention must retain every partition containing input needed by pending
or failed eligible work. Hourly retention likewise waits for durable daily
coverage. Failed summary work cannot authorize deletion merely because the
Janitor continues to its next job. A source without shipped facts or suppressed
by privacy cannot supply missing raw dimensions; preserve its authoritative
rollups and explicitly report unavailable coverage rather than inventing data.

## Legacy ambiguity: the unresolved upgrade gate

Schema 4 stores `(ts, node[, key])`, not the source that last supplied that
cell. Replaced files can share a node name, raw rows can have expired, and
shipping cursors record covered ranges rather than the contents or ownership
of each retained cell. `source.node` is mutable. Neither a join by current node
name nor the most recently refreshed source proves who owns an old cell.

For example, a retained node/hour cell of five queries is compatible with
source A having supplied those five and source B advancing an empty range, or
B having supplied them and A advancing an empty range. The same retained
sources, hour cursors and node cell can survive both histories. A later
source-A replacement of seven requires seven in the first history and twelve
in the second. Assigning the legacy cell to A, assigning it to B, or summing
both a legacy default-source row and A's seven guesses in at least one history.

The final day has an additional trap: knowing that A has not shipped its last
hour does not attribute A's earlier node-only hours in that day. A synthesized
hour can safely cover a proved missing suffix, while a day rebuilt by joining
all earlier node hours to A can include B. A much later official day may also
include A's earlier hours already present in an opaque baseline. Replacing
only a synthesized tail cannot determine which baseline values to subtract.

A cursor proves a missing suffix only if its monotonicity and row coverage are
established. Schema 4's rollup range writer does not recheck the current cursor
under its source lock and can regress a stale range's cursor. Future monotonic
checks do not retroactively turn a legacy cursor into a provenance receipt.

There are only evidence-preserving automatic cases: no legacy overlap, or
complete independently verified source input/receipts that reconstruct all
owners of the affected cells. Do not infer completeness merely because some
raw facts remain: facts can be disabled, filtered by shipping retention,
purged, or discarded by the local size cap. Counts alone cannot reconstruct
client/domain dimensions or latency distributions.

For unresolved cells, retain the original values and expose an explicit
`legacy_provenance_required` state, affected source/node/bucket ranges and
unresolved hourly/daily coverage in status/Diagnosis and read partial metadata.
Refuse overlapping source replacements without advancing their cursors. Keep
needed input, and require an owner-approved reconstruction/import receipt or
an explicit data-loss disposition before activating those ranges. A fresh
schema can activate source ownership immediately; an upgrade cannot silently
pretend to be fresh. The public status/repair contract and proof format for
that owner-approved transition are unresolved. No such API, migration or
compatibility refusal is implemented in this change.

Implementing only the fresh-database case, freezing old sources indefinitely,
or silently skipping ambiguous daily replacements would leave the original
supported-upgrade/late-return requirement open. Therefore this work does not
ship a speculative schema migration or claim F71 closed. The blocker is absent
legacy provenance and its required explicit recovery contract, not PostgreSQL
availability, an unapproved F69 design, or a request for permission to proceed.

## Required implementation acceptance

Actual PostgreSQL must cover fresh and schema-4 upgrade databases, the exact
inactivity and UTC hour/day boundaries, every metric/dimension, repeat/restart/
rollback, bounded backlog progress and explicit failures. Concurrent tests must
show a blocked source does not block another source; late facts invalidate a
synthetic revision; official hour/day exports replace only their own source;
new-file rejoin adds independently; stale/empty exports cannot regress data.

Upgrade cases must retain IDs and original opaque values, reject connected old
writers, exercise both indistinguishable legacy histories, refuse unproved
overlap visibly, and accept only verified reconstruction receipts. Retention
must preserve pending input and every F70 dictionary reference. Read snapshots
must remain consistent during shipping, reconciliation, retention and pruning.
Mounted-router acceptance must verify node isolation, partial/error metadata,
permissions/privacy/license behavior and no peer/local fallback. These gates
remain open; F69's passing tests do not substitute for them.
