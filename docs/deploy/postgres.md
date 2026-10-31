# Central database (PostgreSQL)

kudurru can keep the runtime data of every node in one PostgreSQL database
(D158): audit events, the query log and the statistics. Every node then
shows the same history, and the history outlives the nodes. It is
optional. Without it each node keeps its own data in SQLite, and in
Kubernetes the audit log is kept in `AuditLog` objects instead (D157).

Configuration (zones, lists, policies, users) never goes to this database.
It stays in Kubernetes custom resources or in each node's `state.db`.

## What happens when the database is away

DNS never depends on the database, and it is never part of readiness. Each
node writes its own SQLite files first and sends their new rows to the
database in the background. While the database is unreachable, nodes keep
their data and send it when it is back. The cluster-wide views of the
dashboard are out of date until then. A node that has not reached the
database for a minute reports `CentralUnreachable` on the Diagnosis page.

## Requirements

- PostgreSQL 16 or newer; 17 is the one to pick for a new database.
  No extensions; TimescaleDB is not needed.
- A database and a role that owns its schema: kudurru creates and migrates
  its tables at start, under an advisory lock, so replicas that start
  together migrate once.
- TLS for any connection that leaves the host: `sslmode=verify-full` and
  `sslrootcert=`. `kudurru doctor` warns about `sslmode=disable` to another
  host.

```sql
CREATE ROLE kudurru LOGIN PASSWORD '...';
CREATE DATABASE kudurru OWNER kudurru;
```

## Configure

Set `database.dsn` on every node. It may hold a password, so it is
`hostOnly` and `writeOnly`: set it in `kudurru.yaml` or through a file, never
through the API.

```sh
echo 'postgres://kudurru:...@db.example:5432/kudurru?sslmode=verify-full' > /run/secrets/kudurru-dsn
KUDURRU_DATABASE__DSN_FILE=/run/secrets/kudurru-dsn kudurru serve
```

`database.maxConns` (default 4) caps each node's connections.

`kudurru doctor` checks that the database answers, runs PostgreSQL 16 or
newer and has a schema the binary can use.

## Audit log

Reading the audit log (the Audit tab, `GET /api/v1/audit`, `kudurru audit`)
needs a license key that includes the audit trail. Recording and shipping the
events do not, so a key that arrives later shows the whole history.

With a database, `audit.store: auto` keeps the audit log there
(`audit.store: database`, D159). Each node still records every event in
its own store first (`state.db` with the sqlite driver, `audit-spool.db` in
Kubernetes) and sends it within a second or two. Events from all nodes form
one hash chain: every event's hash covers the one before it, whichever node
recorded it, so a changed, removed or truncated event is detected. Each
node's own chain is kept beside it (`origin_hash`).

While the database is away, the Audit tab answers that it is unavailable
and the events wait on the nodes. The leader deletes events older than
`audit.retention` (400 days) from the start of the chain.

## Query log and statistics

With a database, every node also sends its query log and statistics there
(`database.querylog.ship`, D160). Each node keeps its own `querylog.db`
and sends new queries every second and its statistics every 30 seconds.
A node keeps the queries it has not sent yet, even past
`querylog.retention.raw`, until it has sent them; only
`querylog.retention.maxDBBytes` removes them earlier.

| Setting | Default | |
|---|---|---|
| `database.querylog.ship` | on | send the query log and statistics |
| `database.querylog.facts` | on | send each query; off sends only the statistics (charts, totals, top lists) |
| `database.querylog.retention.raw` | 168h | keep queries in the database for 7 days; whole days are dropped |

Sizing depends on query content and index overhead. The former estimate of
250 to 300 bytes per query excludes schema 4's additional reference indexes;
allow more space and measure your workload. Statistics are small: per minute for 35 days, per hour for 400 days,
per day forever, like on the nodes. At high rates set
`database.querylog.facts: false` or a short `retention.raw`.

The query table is partitioned by day; the leader creates the partitions a
few days ahead and drops expired ones whole, so retention never deletes
rows one by one.

After raw and rollup retention, the leader also sweeps the seven query-log
dictionaries at startup and hourly. Each pass selects at most 256 entries per
dictionary, with a two-second overall deadline and a 100 ms lock timeout.
A durable ID cursor with a fixed sweep boundary resumes after restart and
revisits entries skipped because a shipper held them. Large backlogs take
multiple hourly passes. An entry is deleted only when **no retained fact or
rollup in this schema, from any source**, references it. Daily rollups remain
forever and can therefore protect dictionary entries forever. `last_seen`
alone never authorizes deletion. Retained IDs and string identities do not
change; an unreferenced string shipped again receives a new ID.

Shippers validate cached IDs and hold row locks in the same transaction as
their facts/rollups and cursor. Foreign keys guard every dictionary reference.
There is no global ingestion lock; a failed or timed-out sweep rolls back
both removals and cursor advancement and is not reported as successful.
Central readers use read-only repeatable-read transactions, so a reader that
captured data before retention keeps its matching dictionary rows through
that read. Reclamation does not cross database/schema isolation boundaries.

The leader recovers closed hours of sources inactive for at least two hours,
then reconciles their final closed UTC day. Schema 5 keeps each file's contribution
separate, so a late official shipment replaces its own summary without counting
it twice or overwriting another file. Each pass has source/bucket/row/time bounds;
large backlogs need multiple passes. Pending/failed summaries retain their input,
so retention can hold more data than its configured age. Late facts after raw
expiration add once to an intact source-owned hourly baseline. If that baseline
has also expired, `source_input_incomplete` preserves the prior daily values and
surviving tail rather than guessing missing hourly dimensions. Fact-free shipping can
recover only already shipped minute dimensions; missing dimensions remain partial.

`/querylog/status` exposes bounded `backfill` counts and source/hour error details.
`legacy_provenance_required` means earlier node-only rollups cannot be assigned
to historical files from retained evidence. Their values/input remain intact;
this is an explicit gate, not a healthy or completed migration. New and existing
files both respect the upgrade's conservative legacy boundary. Source-owned
post-boundary data continues to work. Review storage growth while a gate or
oversized-input failure is unresolved; do not delete retained evidence to make
status appear healthy.

The [source-provenance contract](../architecture/central-stopped-source-rollups.md)
describes exact bounds, stale-refusing evidence capture/verification, atomic
source-owned repair and the unavailable legacy attribution input. Evidence APIs
require query-log read permission and the existing history license window. They
do not authorize replacing opaque legacy cells.

## The dashboard's All nodes view

With the query log in the database, **All nodes** in the scope selector
reads it: every node's queries, charts, totals and top lists, the same on
whichever node serves the page (D161). The scope note reads "All nodes:
… (central database)" and names a node whose statistics are more than five
minutes behind. Without the database, **All nodes** asks every replica and
misses those that are down or replaced. **This node** reads its own `querylog.db`, which is real time. An explicit
other node (`scope=node&node=<name>`) reads only that name's retained central
history, including replaced files and retired nodes absent from membership.
These responses carry `scope: node`, `source: central`, and that node alone.
Unknown historical names return 404. Central failures return 503; historical
reads never fall back to a peer or this node's log. Without a central reader,
selecting another live node still uses its internal listener.

Node filtering applies before pagination, counts and aggregation, and to
retained dictionary references. Client last-seen values use that node's facts
or rollup buckets after raw expiration, rather than global dictionary touches.
Suggestions (`/queries/suggestions`) and status (`/querylog/status`) accept the
same scope/node parameters. A historical status counts only that node's rows;
its physical database size describes the shared schema, not allocated bytes
for that node. Historical upstream traffic has no local live-health rows.

Central query IDs remain central IDs when selecting a historical row for an
action: pass its `scope=node&node=<name>` selector, or the existing
`scope=cluster`. The immutable selected domain goes through the normal current
object command and its permission/ownership checks. A row is never authority
for a current mutation. The live tail continues to require live membership and
streams from active replicas; it does not stream retained central history. Purging the query log deletes this node's rows and every
node's rows in the database; other nodes keep their own copies.

## Grafana monitoring

The shipped overview dashboard includes a **Central database and audit** row:
database availability per node, query shipping lag, and local audit spool drops.
Nodes without `database.dsn` have no central availability or shipping series;
missing data does not mean a database outage. Availability zero means the
configured database is unreachable or incompatible. Query lag is sampled when
a batch ships, so it can retain the last value during an outage; use it together
with availability and the existing central-database alerts. Audit spool drops
count events discarded at the local cap and can occur with either shipping
destination.

## Upgrades and mixed versions

The database records `schema_version` and `compat_version` in
`kudurru_meta`. A node migrates an older schema to its own. During a rolling
upgrade a node that is older than the schema keeps using it while the
schema stays compatible. When a newer kudurru made a change that older
nodes cannot work with, those nodes stop using the database
(`CentralIncompatible` on the Diagnosis page) and keep their data locally
until they are upgraded. A node never refuses to start because of the
database.

Schema 4 makes dictionary reclamation an explicit compatibility boundary
(`compat_version=4`). Upgrade all central shippers. Migration adds and
validates dictionary foreign keys and builds reference indexes in the existing
atomic migration transaction; on a large database, plan for migration locks,
index storage and validation time. Startup connection/migration attempts have
a 30-second deadline and a default 15-second statement timeout. An upgrade
that exceeds these bounds rolls back and keeps central data unavailable;
retries do not retain partially built indexes. Such a database needs a
maintenance migration before rollout. Existing dangling references also fail
the migration atomically; migration never deletes or silently repairs them.
Existing IDs and sequences are preserved. A previously connected old node
might not discover incompatibility until reconnecting; foreign keys refuse
its stale cached-ID batch, rolling back its rows and watermark together.
Its local queue remains subject to the existing local size cap during an
extended upgrade/outage.


Schema 5 adds source ownership and is another explicit compatibility boundary
(`compat_version=5`). Existing node-only cells retain NULL ownership and every
original metric/dictionary ID. The migration builds source indexes and retained
fact revisions; plan for locks, time and storage, including input deliberately
retained by unresolved provenance gates. Already-connected schema-4 rollup
writers fail their old conflict targets atomically. Upgrade all shippers; never
lower markers or assign old cells to a default/current source. Missing historical
ownership requires independently reconstructable evidence and remains visible
as `legacy_provenance_required`; no destructive bypass is provided.
