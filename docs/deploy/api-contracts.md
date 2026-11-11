# API contracts for scripts and CLI clients

Public routes use `/api/v1`. The served `/api/v1/openapi.json` describes JSON
fields and permissions. These examples explain contracts clients must
preserve. They omit optional fields and use illustrative timing values.

## Applying multiple objects

`POST /apply` accepts YAML documents or JSON objects/arrays. Each document is
a whole-object apply with manager `api`. The response is a list envelope:

```json
{
  "items": [
    {"kind":"ClientGroup","namespace":"default","name":"new","result":"created","resourceVersion":"42"},
    {"kind":"ClientGroup","namespace":"default","name":"owned","result":"error","error":"object is managed elsewhere","errorKey":"managed_by","hint":"file:objects.d/groups.yaml"}
  ],
  "took_ms": 1.2
}
```

Results remain in document order. `result` is `created`, `updated`, `unchanged`
or `error`. Successful results carry the stored `resourceVersion`. A failed
document carries human-readable `error`, machine-readable `errorKey`, and an
optional `hint`; a `managed_by` hint identifies the owner. These are fields
on the item, not the top-level `{error:{key,message,hint}}` error envelope.
HTTP 200 does not mean every document succeeded. The CLI prints all outcomes
and exits unsuccessfully if any document failed. Earlier successful documents
are not rolled back when another document fails. Do not automatically replay
the batch after a refusal or an uncertain transport outcome.

On SQLite, a standalone node and the primary of a cluster apply every
document of one request, each in its own commit. A request holds its place
between two documents against its own commits and the writes that the node
makes without an API command (status, zone state, RFC 2136 updates, the local
admin socket and reconciliation), which it does not detect. While its
documents commit, the node answers no DNS query for its own zones and other
requests answer `503 generation_pending` until it has published again, so a
request that has run for two seconds waits, for up to two seconds, until the
node has published the documents so far and then commits nothing for 200
milliseconds before its next document. When the node takes longer to publish
(a large data set or a loaded machine), the request goes on without the
publication, so the pauses make it at most about twice as slow. When a command of anyone else changes the node's data while
the request runs (another request admitted in such a pause, a sign-in, a
write that a follower passes on, a migration step), every later document
carries `errorKey` `unavailable` with the error "another change was
committed on this node while the request ran; this item was not applied;
review current state before a new decision". Those documents were not
applied. Read the results, then send the request again as a new decision:
documents that are already applied report `unchanged`. A follower of a
cluster passes each document on to the primary and stops when its own data
changes; [sqlite-join.md](sqlite-join.md#commands-through-a-follower)
describes both cases. `POST /{kind}:batchDelete` follows the same rule for
its names and reports them under `processed.success` and `processed.errors`.

A request that begins while the node still publishes other work is refused
before its first document: with `503 generation_pending`, or with every item
reporting `unavailable` ("current writable serving generation unavailable").
Nothing was applied, and the same request may be sent again. Of two such
requests sent at the same time one usually ends this way, so send them one
after another. An item reached after the request's five-minute lifetime
reports `unavailable` ("the request's lifetime ended before this item was
reached; it was not applied"). Give the client a timeout of five minutes for
large requests; `kudurru apply` and `kudurru restore` do so unless
`--request-timeout` is set.

## Reviewing and replacing zone options

Review the complete specification before changing it:

```sh
kudurru zones options get home -o yaml > home-options.yaml
# Edit spec in home-options.yaml, preserving resourceVersion.
kudurru zones options set home -f home-options.yaml
```

The document contains only `resourceVersion` and `spec`. `set` replaces the
entire specification; omitted fields revert to their normal omitted/default
behavior. It accepts one YAML or JSON document of at most 1 MiB, including
stdin with `-f -`. Unknown or duplicate fields are rejected before a request.
Use `-o json` or `-o yaml` for a document; the default table is for reading.

The CLI reads the current object to preserve metadata and status, substitutes
the reviewed specification and resource version, and sends one
`PUT /objects/dnszones/<name>` with the reviewed `If-Match`. A newer revision
returns HTTP 412. The existing route enforces zone-write permission, ownership,
validation and, on complete SQLite serving, original credential and writer
authority through the final transaction check. Even unchanged options pass
through this authorization. The command cannot take over another manager's
zone and never retries a refused write. Review the current options explicitly
before submitting another change.

## Reading and changing settings

`GET /settings` and `GET /node/config` return a wrapper. `PATCH` accepts the
bare JSON merge patch and returns that wrapper plus change/persistence data:

```json
{
  "scope": "cluster",
  "node": "node-a",
  "settings": {"control":{"blockedQTypes":["A","ANY"]}},
  "changed": ["control.blockedQTypes"],
  "persisted": true,
  "took_ms": 1.2
}
```

The corresponding request body is `{"control":{"blockedQTypes":["A","ANY"]}}`.
`settings` contains all effective keys for that scope, with write-only values
redacted. `sections` and `conflicts`, when present, describe settings ownership.
`changed` and `restartRequired` are dotted key lists and are omitted when empty;
`persisted` is optional and must not be assumed true when absent. A false value
means the override will be lost on restart. Host-only keys cannot be written
through either route. Upstream ownership uses the reviewed revision and
`If-Match`; a refusal requires a new explicit review, not a replay.

On complete SQLite serving, a successful patch commits durable desired settings
before asynchronous publication. A new read can temporarily observe the prior
complete publication. Wait for the intended published settings before checking
their DNS effect. Each already-held request keeps its original publication.
`kudurru settings get` unwraps this response into key/value rows; its structured
output is a CLI representation rather than the HTTP wrapper.

## Answers that depend on the node's cluster role

On the SQLite driver the node that answers may be an enrolled follower, or a
member whose write authority is fenced or in conflict. Four `503` answers say
so. None of them means "try again": do not retry them in a loop.

| Key | Sent by | Meaning | `Retry-After` |
|---|---|---|---|
| `primary_required` | an enrolled follower, for a change it neither forwards nor runs itself | Nothing was changed. The message names the primary; send the request there. | no |
| `completion_unknown` | an enrolled follower, for a forwarded change whose exchange with the primary failed | The follower cannot tell whether the primary committed. Read the object before a new decision; do not replay. | 1 on the authentication routes, otherwise none |
| `cluster_fenced` | a former primary that observed a newer one, for every request | The API is closed until the member follows the newer primary, which it does at its next successful pull. | no |
| `cluster_conflict` | a member that recorded conflicting promotions, for every request | The API is closed until an operator resolves the conflict offline ([recover from conflicting promotions](sqlite-join.md#recover-from-conflicting-promotions)). | no |

`503 generation_pending` remains the answer of a node whose next publication
is still being prepared; it carries `Retry-After: 1` and a short wait is the
right reaction.

`GET /cluster` tells a client beforehand what a follower will answer:

```json
{
  "driver": "sqlite",
  "node": "node-b",
  "role": "follower",
  "primary": "node-a",
  "leader": "node-a",
  "nodes": 1,
  "readOnly": false,
  "primaryRequired": true,
  "forwardedOperations": ["apply", "createObject", "login", "setBlocking"],
  "localOperations": ["flushCache", "promoteCluster", "purgeQuerylog"],
  "took_ms": 0.3
}
```

The example shortens both lists. `primaryRequired` is `true` only on an
enrolled follower. There, a changing operation whose `operationId` is in
neither list answers `primary_required`; treat an operation that the lists do
not name as refused. On every other node the two lists are absent and no
operation is refused for the node's role. `role` and `primary` are absent on
the Kubernetes driver. A fenced or conflicted member never answers this
route. [What a follower forwards, runs itself and refuses](sqlite-join.md#what-a-follower-forwards-runs-itself-and-refuses)
lists the routes of each group.

## Live query events

`GET /queries/stream` responds with `Content-Type: text/event-stream` and named
`query` events. Each event's `data` is one `QueryRow`, the same row shape used
in `/queries`: `id`, `ts`, `node`, string `proto`/`qtype`/`rcode`, nested
`client`, `domain`, `status`, `status_class`, nested `reply`, `dnssec` and
`flags`, with optional provenance such as `upstream`, `list`, `rule` or `zone`.
It is not the internal collector's raw Go event. The stream has no enclosing
JSON list or per-event `took_ms`.

```text
id: 11
event: query
data: {"id":11,"ts":"2026-10-31T12:00:00Z","node":"node-a","proto":"udp","client":{"ip":"192.0.2.4"},"domain":"blocked.example","qtype":"A","status":"blocked_list","status_class":"blocked","rcode":"NOERROR","reply":{"type":"none"},"dnssec":"unknown","flags":{}}

```

Single-node event IDs are decimal query IDs. A cluster stream's event ID is
an opaque, filter-bound cursor containing per-node positions; do not parse it
as a single query ID or reuse it with different filters. Send the last SSE
`id` as `Last-Event-ID` to resume from the retained tail. Resume is bounded by
the ring's retention and is not durable lossless delivery. Cluster and remote
streams also send named `scope` events with node availability and `partial`
status; they are not query rows. Comments such as `: ping` are heartbeats,
and `retry` is the reconnection delay in milliseconds.

`kudurru logs --follow` prints history followed by live rows. Structured output
is one JSON row per line. Canceling the command closes its subscription. The
real-router CLI acceptance tests cover filtering, serialization and cleanup;
separate mounted tests cover production credential and writer authority.
