### Added

- `GET /api/v1/querylog/sinks` (permission `querylog.read`) shows the
  additional query-log sinks (`querylog.sinks`) the answering node runs: queue
  length and bound, queries sent and dropped, failed attempts, the time of the
  last successful write, and whether a dnstap sink sends raw queries. A sink
  that is still draining after a change is listed too. Each node reports only
  its own sinks.

### Fixed

- A ClickHouse or Loki sink no longer counts a batch as stored when the
  destination answered 2xx before the batch body was sent (a server that
  ignores "100 Continue" and the body). Such a batch is now an unknown
  outcome: it is counted as dropped and not sent again.
- A ClickHouse or Loki sink with a short `timeout` (under about 4 seconds) no
  longer loses every large batch to a server that ignores `Expect:
  100-continue`; it sends the body at once instead of waiting for the server,
  and still hears a destination that refuses the request at its headers.
- When the local query log itself does not flush in time at shutdown, the
  additional sinks are reported in one warning, `additional query-log sinks
  stopped because the query log did not flush`, instead of one
  `additional query-log sink did not drain` per sink that also named healthy
  idle sinks depending on timing.
