# Additional query-log sinks

`querylog.sink` selects where a node records its queries: the local
database (`sqlite`, which the dashboard reads) or log lines (`slog`).
`querylog.sinks` adds further destinations that receive the same recorded
queries next to it. It is a cluster setting, available through the settings
API, the CLI, the settings file and, in Kubernetes, `config.settings`.
Changes apply without a restart.

```yaml
querylog:
  sink: sqlite            # the dashboard keeps its database
  sinks:
    - name: log-lines     # labels this sink in metrics and log messages
      type: slog          # one structured log line per query
```

Through the API, send a JSON merge patch to `PATCH /api/v1/settings`. The
list is replaced as a whole:

```json
{"querylog":{"sinks":[{"name":"log-lines","type":"slog"}]}}
```

## What a sink receives

Each sink receives every query the node records, after the privacy mode and
the exclusions have been applied. A sink never sees more than the local log
keeps:

- `querylog.privacy` hides clients, names or both before the query reaches
  any sink; under `nothing` no per-query event is sent to an additional sink.
- Names and clients in `querylog.exclude.*` are never sent.
- `querylog.enabled: false` stops every sink.

A new sink starts with the queries answered after it was added. Earlier
queries are not sent to it.

## Types

| Type | Destination | Notes |
|---|---|---|
| `slog` | the server's log output, one structured line per query with a `sink` attribute | at most one, and not together with `querylog.sink: slog`, which would write every query twice |

The server refuses a type it cannot serve. Query lines are written at the
`info` level, like those of `querylog.sink: slog`, so they need `log.level`
`info` or `debug`.

## Limits of one sink

Every entry has its own queue and its own limits. Omit a field for its
default.

| Field | Default | Range | Meaning |
|---|---|---|---|
| `name` | required | lowercase letters, digits and hyphens, at most 32 characters | unique in the list; `sqlite` and `slog` are taken by `querylog.sink` |
| `type` | required | see above | kind of destination |
| `queueSize` | 10000 | 100 to 100000 | queries that may wait for this destination |
| `batchSize` | 1000 | 1 to 10000, at most `queueSize` | queries per write |
| `flushInterval` | 1s | 100ms to 1m | longest wait before a partial batch is sent |
| `timeout` | 10s | 1s to 2m | opening the destination and each write attempt |
| `attempts` | 3 | 1 to 10 | writes of one batch when the destination reports that it was not stored and may be sent again; 1 never retries |
| `retryBackoff` | 1s | 100ms to 1m | wait before the second attempt; doubles up to 30s |

At most eight sinks can be configured.

## When a destination is slow or down

A sink cannot slow the server down. DNS answers, the local database and the
dashboard's live view never wait for it. When a destination is slower than
the query rate, stalled or failing:

- its queue fills to `queueSize`, and further queries are dropped for that
  sink only;
- a batch is sent again only when the destination reported that it was not
  stored; after `attempts` writes, or after any failure with an unknown
  outcome, the batch is dropped. A batch is never sent twice on a guess, and
  never moved to another sink;
- the server logs a warning at the first lost query and at each tenfold
  total, and keeps trying to reach the destination.

Dropped queries stay in the local database (`querylog.sink: sqlite`); only
the copy for that destination is lost.

## Monitoring

| Metric | Meaning |
|---|---|
| `kudurru_querylog_dropped_total{stage="sink",sink="<name>"}` | queries lost for this sink (queue overflow, an abandoned batch, or a query caught by a change of the sink's entry) |
| `kudurru_sink_sent_total{sink}` | queries the destination accepted |
| `kudurru_sink_errors_total{sink}` | failed attempts to open or write |
| `kudurru_sink_queue_length{sink}` | queries waiting now |
| `kudurru_sink_last_success_timestamp_seconds{sink}` | time of the last successful write |

The shipped `KudurruQuerylogDrops` alert fires on any increase of
`kudurru_querylog_dropped_total`, and the Grafana query-log panel shows the
drops by stage and sink. A queue length that stays near `queueSize` means
the destination cannot keep up: raise `batchSize`, or reduce what is logged
with `querylog.exclude.*`.

The series of a sink disappear when it is removed from the settings.

## Changing and removing sinks

- An entry that is not changed keeps running across unrelated settings
  changes.
- Changing any field of an entry replaces that sink: the old one finishes
  what it holds, the new one starts with new queries. On Kubernetes, a query
  that is being recorded at the very moment of the change can miss both. It
  is counted as dropped for that sink, is not sent to the new one, and stays
  in the local database. This is rare, but the drop alert fires on any
  drop, so changing a sink on a busy server can trigger it once.
- A removed sink gets no new queries. It writes what its queue holds for up
  to five seconds, counts what remains as dropped, and closes once requests
  that began before the change have finished (an open live view can delay
  the close and keep the sink's metrics visible until it ends).
- On shutdown the sinks drain inside the server's normal shutdown time; the
  local database does not wait for them.

How this is built: [the query log pipeline](../architecture/observability.md#additional-sinks).
