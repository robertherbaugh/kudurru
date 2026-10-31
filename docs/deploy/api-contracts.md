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
