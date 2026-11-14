### Added

- ClickHouse and Grafana Loki as query-log destinations. Two new types of
  `querylog.sinks` send every recorded query to a ClickHouse table
  (`type: clickhouse`, one `INSERT ... FORMAT JSONEachRow` per batch over the
  HTTP interface) or to Loki (`type: loki`, one push per batch, with client,
  domain, query type, status and trace ID as structured metadata and only the
  node and the status class as labels). The server sends the batches itself;
  no agent or collector runs beside it. As for every additional sink, the
  privacy mode and the exclusions are applied first, each sink has its own
  bounded queue, and a slow or failing destination loses only its own queries
  and never delays DNS answers, the local database or the live view.
  `docs/deploy/query-log-sinks.md` has the ClickHouse table definition, the
  Loki mapping and example settings.

- Credentials for these sinks are references, never values in the settings:
  `passwordRef`, `bearerTokenRef` and `caRef` name a file or an environment
  variable that the node's own configuration allows
  (`credentials.allowedFiles`, `credentials.allowedEnvironment`), or, on
  Kubernetes, a key of a Secret in kudurru's namespace that carries the label
  `kudurru.io/query-log-sink: "true"`. A sink reads its credential again when
  the destination answers 401 or 403, so a rotated password or token is
  picked up without a restart or a settings change. A referenced value
  holds at most 64 KiB; `caRef` names the private CA, not a whole system
  bundle, and a larger value is refused with a message that says so. A
  grant is not bound to a destination: whoever may change the settings can
  have a granted credential sent to any URL, so grant only credentials made
  for these destinations.

- Retry rules for the HTTP sinks. A batch is sent again, at most `attempts`
  times, only when nothing was sent or the destination answered that it did
  not store it (408, 429 or 5xx; a `Retry-After` header is honored up to 30
  seconds). Such a repeat can store a batch twice when a 5xx came after the
  destination had stored it; the documented ClickHouse table and Loki drop
  the duplicate, and `attempts: 1` turns repeats off (see the operator
  page). Every other answer drops the batch at once, and so does a request
  that was sent and got no answer, because its outcome is unknown. A
  refusal that ClickHouse sends while a large batch is still being written
  (a wrong password) is read and acted on, not mistaken for a lost
  answer. Drops and failed attempts are counted per sink in
  `kudurru_querylog_dropped_total{stage="sink",sink}` and
  `kudurru_sink_errors_total{sink}`. The server log names the HTTP status
  and, for ClickHouse, its error code; it never repeats the destination's
  error text, which can quote queries.

### Changed

- A credential file, environment variable or Secret key that holds more
  than 64 KiB is now refused with `credential is unavailable: it holds more
  than 64 KiB` instead of the bare `credential is unavailable`, for stored
  identity providers and integrations as well as for the query-log sinks.
