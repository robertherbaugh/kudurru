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

- PostgreSQL 15 or newer (`UNIQUE NULLS NOT DISTINCT`). No extensions;
  TimescaleDB is not needed.
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

`kudurru doctor` checks that the database answers, runs PostgreSQL 15 or
newer and has a schema the binary can use.

## Audit log

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

## Upgrades and mixed versions

The database records `schema_version` and `compat_version` in
`kudurru_meta`. A node migrates an older schema to its own. During a rolling
upgrade a node that is older than the schema keeps using it while the
schema stays compatible. When a newer kudurru made a change that older
nodes cannot work with, those nodes stop using the database
(`CentralIncompatible` on the Diagnosis page) and keep their data locally
until they are upgraded. A node never refuses to start because of the
database.
