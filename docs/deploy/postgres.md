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

Sizing: a query takes about 250 to 300 bytes in the database with its
indexes, so about 2.5 GB a day at 100 queries per second and 25 GB a day at
1000. Statistics are small: per minute for 35 days, per hour for 400 days,
per day forever, like on the nodes. At high rates set
`database.querylog.facts: false` or a short `retention.raw`.

The query table is partitioned by day; the leader creates the partitions a
few days ahead and drops expired ones whole, so retention never deletes
rows one by one.

Statistics of a node that stops for good during an hour that it has not
summarised yet lose that part hour; the individual queries are kept.

## The dashboard's All nodes view

With the query log in the database, **All nodes** in the scope selector
reads it: every node's queries, charts, totals and top lists, the same on
whichever node serves the page (D161). The scope note reads "All nodes:
… (central database)" and names a node whose statistics are more than five
minutes behind. Without the database, **All nodes** asks every replica and
misses those that are down or replaced. **This node** and a chosen node
still read that node's own `querylog.db`, which is real time. The live
tail streams from the replicas, so it is off in the central view; choose a
node to follow it. Purging the query log deletes this node's rows and every
node's rows in the database; other nodes keep their own copies.

## Upgrades and mixed versions

The database records `schema_version` and `compat_version` in
`kudurru_meta`. A node migrates an older schema to its own. During a rolling
upgrade a node that is older than the schema keeps using it while the
schema stays compatible. When a newer kudurru made a change that older
nodes cannot work with, those nodes stop using the database
(`CentralIncompatible` on the Diagnosis page) and keep their data locally
until they are upgraded. A node never refuses to start because of the
database.
